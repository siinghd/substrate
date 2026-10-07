package vmm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
)

// GuestSession is the host side of SGP/1 to a running guestd. A background
// serve loop consumes guest-initiated streams (HELLO, PORTS, METRICS); host
// operations open their own streams. Every message from the guest is
// length-checked by the mux and schema-validated here — the guest is hostile
// the moment user code runs (plan §6.2).
type GuestSession struct {
	mux  *proto.Mux
	next uint32
	idMu sync.Mutex

	mu      sync.RWMutex
	hello   *proto.Hello
	ports   []PortInfo
	metrics *GuestMetrics
	// helloCh and firstPort are LATCHES, closed once, not signals.
	//
	// They used to be a send on an unbuffered (resp. capacity-1) channel with a
	// `default:` fallthrough, so a HELLO that arrived before anyone was parked
	// in Hello() was silently discarded and the caller then waited out its full
	// timeout — a create that failed with "guest handshake timeout" purely
	// because the guest answered too fast. Closing is idempotent, order-
	// independent, and serves every waiter: the facts these announce (the guest
	// said hello; the guest announced ports) are monotonic, so a latch is the
	// right primitive and a signal never was.
	helloCh   chan struct{}
	helloOnce sync.Once
	firstPort chan struct{}
	portOnce  sync.Once

	closeOnce sync.Once
	stopHB    chan struct{}
}

// Heartbeat cadence. docs/protocol.md:73 states the contract — "Either side may
// PING; missing PONG within 10s marks the connection dead" — but nothing ever
// called Ping outside tests, and no read deadline was ever set on the vsock. A
// guest that stops answering (an end user is root in their own microVM;
// `kill -STOP $(pidof guestd)` is enough) left every host-side operation parked
// forever: one log fd, three goroutines and a permanently "running" exec row
// per request, reclaimed by nothing.
// Vars, not consts, so tests can exercise the state machine without waiting
// out production cadence (same idiom as maxExecLogBytes).
var (
	heartbeatInterval = 15 * time.Second
	heartbeatTimeout  = 10 * time.Second
	heartbeatFailures = 2 // tolerate one lost round trip before declaring death
)

func NewGuestSession(rwc io.ReadWriteCloser) *GuestSession {
	g := &GuestSession{
		mux:       proto.NewMux(rwc, true),
		next:      1,
		helloCh:   make(chan struct{}),
		firstPort: make(chan struct{}),
		stopHB:    make(chan struct{}),
	}
	go g.serve()
	go g.heartbeat()
	return g
}

// heartbeat enforces the documented liveness contract. On failure it closes the
// mux, which cascades EOF into every blocked Recv — that is what actually
// reclaims the fds, goroutines and exec rows a mute guest would otherwise pin.
func (g *GuestSession) heartbeat() {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	misses := 0
	for {
		select {
		case <-g.stopHB:
			return
		case <-g.mux.Done():
			return
		case <-t.C:
		}
		if err := g.Ping(heartbeatTimeout); err != nil {
			misses++
			if misses >= heartbeatFailures {
				_ = g.Close()
				return
			}
			continue
		}
		misses = 0
	}
}

func (g *GuestSession) allocID() string {
	g.idMu.Lock()
	defer g.idMu.Unlock()
	id := fmt.Sprintf("ex_%08x", g.next)
	g.next++
	return id
}

// serve consumes guest-initiated streams for the life of the session.
func (g *GuestSession) serve() {
	for {
		st, err := g.mux.Accept()
		if err != nil {
			return
		}
		f, err := st.Recv()
		if err != nil {
			continue
		}
		switch f.Op {
		case proto.OpHello:
			if h, err := proto.Decode[proto.Hello](f.Payload); err == nil {
				g.mu.Lock()
				g.hello = &h
				g.mu.Unlock()
				g.helloOnce.Do(func() { close(g.helloCh) })
			}
		case proto.OpPorts:
			if p, err := proto.Decode[proto.PortsAnnounce](f.Payload); err == nil {
				g.mu.Lock()
				g.ports = toPortInfos(p)
				g.mu.Unlock()
				g.portOnce.Do(func() { close(g.firstPort) })
			}
		case proto.OpMetrics:
			if m, err := proto.Decode[proto.GuestMetrics](f.Payload); err == nil {
				g.mu.Lock()
				g.metrics = &GuestMetrics{
					MemTotalKB: m.MemTotalKB, MemAvailKB: m.MemAvailKB,
					Load1: m.Load1, PIDs: m.PIDs, UptimeS: m.UptimeS,
				}
				g.mu.Unlock()
			}
		}
		st.Close()
	}
}

func toPortInfos(p proto.PortsAnnounce) []PortInfo {
	out := make([]PortInfo, 0, len(p.Ports))
	for _, e := range p.Ports {
		out = append(out, PortInfo{Port: int(e.Port), Proto: e.Proto})
	}
	return out
}

func (g *GuestSession) Ping(timeout time.Duration) error {
	st, err := g.mux.Open(proto.OpPing, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	f, err := frameWithTimeout(st, timeout)
	if err != nil {
		return err
	}
	if f.Op == proto.OpPong {
		return nil
	}
	return fmt.Errorf("unexpected op %d", f.Op)
}

// Hello waits for the guest handshake.
func (g *GuestSession) Hello(timeout time.Duration) (*proto.Hello, error) {
	select {
	case <-g.helloCh:
	case <-time.After(timeout):
		return nil, fmt.Errorf("guest handshake timeout")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.hello, nil
}

// WaitPorts waits for the first ports announcement after boot/restore.
func (g *GuestSession) WaitPorts(timeout time.Duration) ([]PortInfo, error) {
	select {
	case <-g.firstPort:
	case <-time.After(timeout):
		g.mu.RLock()
		defer g.mu.RUnlock()
		if g.ports != nil {
			return g.ports, nil
		}
		return nil, fmt.Errorf("no ports announcement")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	return append([]PortInfo(nil), g.ports...), nil
}

func frameWithTimeout(st *proto.Stream, timeout time.Duration) (proto.Frame, error) {
	type res struct {
		f   proto.Frame
		err error
	}
	ch := make(chan res, 1)
	go func() {
		f, err := st.Recv()
		ch <- res{f, err}
	}()
	select {
	case r := <-ch:
		return r.f, r.err
	case <-time.After(timeout):
		return proto.Frame{}, fmt.Errorf("frame timeout")
	}
}

// debugSSE is read once at init. It used to be an os.Getenv on EVERY exec
// output frame; os.Getenv takes a package-level RWMutex, so on a node serving
// thousands of streaming execs that was shared-lock traffic on the hottest path
// in the process, to answer a question whose answer cannot change.
var debugSSE = os.Getenv("SUBSTRATE_DEBUG_SSE") == "1"

func (g *GuestSession) Exec(ctx context.Context, spec ExecSpec) (*ExecHandle, error) {
	execID := g.allocID()
	req := proto.ExecRequest{
		ExecID: execID,
		Argv:   spec.Argv,
		Cwd:    spec.Cwd,
		Env:    spec.Env,
		TTY:    spec.TTY,
		Rows:   spec.Rows,
		Cols:   spec.Cols,
	}
	if debugSSE {
		fmt.Fprintf(os.Stderr, "HOST-EXEC opening stream for %s\n", execID)
	}
	payload, _ := json.Marshal(req)
	st, err := g.mux.Open(proto.OpExecReq, payload)
	if err != nil {
		return nil, err
	}

	bp := newBufPipe(1 << 20)
	exitCh := make(chan ExitInfo, 2)
	h := &ExecHandle{ID: execID, Stdout: bp, ExitCh: exitCh}
	h.cancel = bp.CloseWrite

	var exitOnce sync.Once
	fail := func(code int) {
		bp.CloseWithError(io.ErrClosedPipe)
		exitOnce.Do(func() { exitCh <- ExitInfo{Code: code, Killed: true} })
	}
	// Control frames ride the exec's OWN stream: that is where both agents
	// (shim + guestd) read them. The previous implementation opened separate
	// OpExecSignal/OpExecResize streams, which every agent discarded in its
	// accept loop — so ctx-cancel kills and DELETE /v1/execs/{id} were
	// silently ignored and the process kept running.
	signalFn := func(sig string) error {
		return st.Send(proto.OpExecSignal, proto.Encode(proto.ExecSignal{ExecID: execID, Signal: sig}))
	}
	resizeFn := func(rows, cols uint16) error {
		return st.Send(proto.OpExecResize, proto.Encode(proto.ExecResize{ExecID: execID, Rows: rows, Cols: cols}))
	}
	h.signalFn = signalFn
	h.resizeFn = resizeFn

	// execDone bounds the ctx watcher below to the exec's own lifetime.
	//
	// The watcher used to be a bare `<-ctx.Done()`, and the only production
	// caller (handlers_execs.go) passes context.Background() -- correctly, since
	// an exec outlives the HTTP request that started it. So the channel never
	// closed: one goroutine parked forever per exec ever run, each pinning the
	// stream and its closure. That is O(execs ever), not O(execs live), on a
	// path the plan sizes at 1,000-5,000 req/s. Passing r.Context() would have
	// been the wrong patch -- it would have killed every exec when its creating
	// request returned.
	execDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = signalFn("KILL")
		case <-execDone:
			// Normal completion: nothing to signal, and signalling here would
			// send KILL to an exec that has already exited.
		}
	}()
	h.stdinFn = func(b []byte) error {
		if b == nil {
			return st.CloseWrite()
		}
		frame := make([]byte, len(b)+1)
		frame[0] = 0
		copy(frame[1:], b)
		return st.SendBinary(proto.OpExecData, frame)
	}

	go func() {
		defer func() {
			close(execDone)
			// Every return path must produce an exit. The bp.Write branch
			// below used to return without one, leaving ExecHandle.Wait
			// blocked forever on a channel nobody would ever send to.
			exitOnce.Do(func() { exitCh <- ExitInfo{Code: -1, Killed: true} })
		}()
		for {
			f, err := st.Recv()
			if debugSSE {
				fmt.Fprintf(os.Stderr, "HOST-EXEC frame exec=%s op=%d fin=%v len=%d err=%v\n", execID, f.Op, f.Flags&proto.FlagFin != 0, len(f.Payload), err)
			}
			if err != nil {
				fail(-1)
				return
			}
			switch {
			case f.Op == proto.OpExecData && f.Flags&proto.FlagBinary != 0:
				if len(f.Payload) > 0 {
					if _, werr := bp.Write(f.Payload[1:]); werr != nil {
						return
					}
				}
			case f.Op == proto.OpExecExit:
				exit, xerr := proto.Decode[proto.ExecExit](f.Payload)
				bp.CloseWrite()
				code := -1
				killed := true
				if xerr == nil {
					code, killed = exit.Code, exit.Killed
				}
				exitOnce.Do(func() { exitCh <- ExitInfo{Code: code, Killed: killed} })
				st.Close()
				return
			}
		}
	}()
	return h, nil
}

// FsClient wraps filesystem ops; paths are absolute inside the guest.
type FsClient struct {
	g *GuestSession
}

func (g *GuestSession) Fs() *FsClient { return &FsClient{g: g} }

type fsResult struct {
	resp proto.FsResponse
	data []byte
	err  error
}

func (c *FsClient) do(ctx context.Context, req proto.FsRequest, payload []byte) fsResult {
	st, err := c.g.mux.Open(proto.OpFsReq, mustJSON(req))
	if err != nil {
		return fsResult{err: err}
	}
	defer st.Close()
	if payload != nil {
		// Payload-less requests and single-chunk payloads ride this path;
		// multi-frame writes go through Write's chunked sender.
		if len(payload) > 0 {
			if err := st.SendBinary(proto.OpFsData, payload); err != nil {
				return fsResult{err: err}
			}
		}
		_ = st.CloseWrite()
	}
	return c.awaitFsResp(st, ctx)
}

// awaitFsResp collects OpFsData binary chunks until the terminal OpFsResp.
// The per-Recv goroutine is abandoned on timeout by closing the stream —
// leaving it running would leak one goroutine per timed-out op.
// maxFsResponseBytes bounds one fs response. The guest agents cap a single read
// at 32MiB, so anything beyond this is a guest that is not answering the
// question it was asked.
var maxFsResponseBytes = 64 << 20

// fsOpTimeout bounds a whole fs operation, not one frame of it.
var fsOpTimeout = 60 * time.Second

// awaitFsResp collects OpFsData chunks until the terminal OpFsResp.
//
// REWRITTEN, not patched. The previous version had three defects that shared a
// single cause -- it treated each frame as an independent operation:
//
//   - No ceiling on out.data. A guest that answers any request, even a one-byte
//     stat, with an endless stream of 1MiB frames grew this without bound. One
//     GET /v1/sandboxes/{id}/fs/x reached tens of GiB and OOMed the node,
//     taking every co-tenant sandbox with it. End-user code owns the rootfs, so
//     replacing the agent is a supported thing for a tenant to do.
//   - The 30s timeout was created INSIDE the loop, so it restarted on every
//     frame and could never fire against a guest that kept sending. A deadline
//     that resets whenever the adversary acts is not a deadline.
//   - A goroutine and a timer were allocated per frame, both abandoned on the
//     timeout path.
//
// Patching the ceiling alone would have left the deadline unenforceable, and
// vice versa. proto.Stream.RecvContext (added in PASS-12 for exactly this) makes
// the correct shape available: one deadline for the whole operation, no
// per-frame goroutine at all.
//
// Complexity: O(frames) time, O(1) goroutines (was O(frames)), memory bounded by
// maxFsResponseBytes regardless of what the guest sends.
func (c *FsClient) awaitFsResp(st *proto.Stream, ctx context.Context) fsResult {
	opCtx, cancel := context.WithTimeout(ctx, fsOpTimeout)
	defer cancel()

	out := fsResult{}
	for {
		f, err := st.RecvContext(opCtx)
		if err != nil {
			if opCtx.Err() != nil && ctx.Err() == nil {
				st.Close()
				return fsResult{err: fmt.Errorf("fs op timeout after %s", fsOpTimeout)}
			}
			if ctx.Err() != nil {
				st.Close()
				return fsResult{err: ctx.Err()}
			}
			break
		}
		switch {
		case f.Op == proto.OpFsData && f.Flags&proto.FlagBinary != 0:
			if len(out.data)+len(f.Payload) > maxFsResponseBytes {
				st.Close()
				return fsResult{err: fmt.Errorf("guest fs response exceeds %d bytes", maxFsResponseBytes)}
			}
			out.data = append(out.data, f.Payload...)
		case f.Op == proto.OpFsResp:
			r, derr := proto.Decode[proto.FsResponse](f.Payload)
			if derr != nil {
				return fsResult{err: derr}
			}
			if !r.OK {
				return fsResult{err: fmt.Errorf("guest fs: %s", r.Error)}
			}
			out.resp = r
			return out
		}
		if f.Flags&proto.FlagFin != 0 {
			break
		}
	}
	return fsResult{err: fmt.Errorf("fs op incomplete")}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

var ErrNotFound = fmt.Errorf("not found in guest")

func (c *FsClient) Read(ctx context.Context, path string, offset, length int64) ([]byte, error) {
	r := c.do(ctx, proto.FsRequest{Op: "read", Path: path, Offset: offset, Length: length}, nil)
	return r.data, r.err
}

func (c *FsClient) Write(ctx context.Context, path string, data []byte, appendMode bool) error {
	// Chunk to the wire limit. The old single-frame send made any write over
	// 1MiB fail with "frame exceeds MaxFrameSize" after the API handler had
	// already accepted a 64MiB body. Both agents accumulate OpFsData frames
	// until FIN, so multi-frame writes are the intended encoding.
	st, err := c.g.mux.Open(proto.OpFsReq, mustJSON(proto.FsRequest{Op: "write", Path: path, Append: appendMode, Mode: 0o644}))
	if err != nil {
		return err
	}
	defer st.Close()
	for off := 0; off < len(data); {
		end := off + proto.MaxFrameSize
		if end > len(data) {
			end = len(data)
		}
		if err := st.SendBinary(proto.OpFsData, data[off:end]); err != nil {
			return err
		}
		off = end
	}
	_ = st.CloseWrite()
	out := c.awaitFsResp(st, ctx)
	return out.err
}

func (c *FsClient) List(ctx context.Context, path string) ([]proto.FsEntry, error) {
	r := c.do(ctx, proto.FsRequest{Op: "list", Path: path}, nil)
	if r.err != nil {
		return nil, r.err
	}
	return r.resp.Entries, nil
}

func (c *FsClient) Remove(ctx context.Context, path string) error {
	r := c.do(ctx, proto.FsRequest{Op: "remove", Path: path}, nil)
	return r.err
}

func (c *FsClient) Mkdir(ctx context.Context, path string) error {
	r := c.do(ctx, proto.FsRequest{Op: "mkdir", Path: path}, nil)
	return r.err
}

func (c *FsClient) Stat(ctx context.Context, path string) (*proto.FsEntry, error) {
	r := c.do(ctx, proto.FsRequest{Op: "stat", Path: path}, nil)
	if r.err != nil {
		return nil, r.err
	}
	if len(r.resp.Entries) == 0 {
		return nil, ErrNotFound
	}
	e := r.resp.Entries[0]
	return &e, nil
}

// Reseed drives the uniqueness protocol after every restore (plan §7.4).
func (g *GuestSession) Reseed(vmgenid string, timeout time.Duration) (ReseedAck, error) {
	st, err := g.mux.Open(proto.OpReseed, proto.Encode(proto.Reseed{VMGenID: vmgenid}))
	if err != nil {
		return ReseedAck{}, err
	}
	defer st.Close()
	f, err := frameWithTimeout(st, timeout)
	if err != nil {
		return ReseedAck{}, err
	}
	if f.Op != proto.OpReseedAck {
		return ReseedAck{}, fmt.Errorf("expected RESEED_ACK, got op %d", f.Op)
	}
	return proto.Decode[ReseedAck](f.Payload)
}

func (g *GuestSession) LatestMetrics() (*GuestMetrics, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.metrics == nil {
		return nil, false
	}
	return g.metrics, true
}

func (g *GuestSession) Close() error {
	var err error
	g.closeOnce.Do(func() {
		close(g.stopHB)
		err = g.mux.Close()
	})
	return err
}

func (g *GuestSession) Done() <-chan struct{} { return g.mux.Done() }
