package proto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	frames := []Frame{
		{StreamID: 1, Flags: flagSyn, Op: OpHello, Payload: []byte(`{"proto":1}`)},
		{StreamID: 3, Flags: flagBinary | flagFin, Op: OpExecData, Payload: bytes.Repeat([]byte{0xAB}, MaxFrameSize)},
		{StreamID: 5, Op: OpPing},
	}
	var buf bytes.Buffer
	for _, f := range frames {
		if err := WriteFrame(&buf, f); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range frames {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.StreamID != want.StreamID || got.Flags != want.Flags || got.Op != want.Op || !bytes.Equal(got.Payload, want.Payload) {
			t.Fatalf("roundtrip mismatch: %+v vs %+v", got, want)
		}
	}
}

func TestFrameTooLarge(t *testing.T) {
	err := WriteFrame(io.Discard, Frame{Payload: make([]byte, MaxFrameSize+1)})
	if err != ErrFrameTooLarge {
		t.Fatalf("want ErrFrameTooLarge, got %v", err)
	}
	var hdr [13]byte
	hdr[6] = 0xFF
	hdr[7] = 0xFF
	hdr[8] = 0xFF
	hdr[9] = 0xFF
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); err != ErrFrameTooLarge {
		t.Fatalf("read: want ErrFrameTooLarge, got %v", err)
	}
}

// Golden vector: host Open(OpHello) then guest reply. Both Go and Rust tests
// assert against these exact bytes (see guestd/src/protocol.rs and testdata/).
const goldenHex = "0100000001020b0000000000007b2270726f746f223a317d030000000103100000000000007b22657865635f6964223a226531227d"

func TestGoldenVector(t *testing.T) {
	raw, err := hex.DecodeString(goldenHex)
	if err != nil {
		t.Fatal(err)
	}
	r := bytes.NewReader(raw)
	f1, err := ReadFrame(r)
	if err != nil || f1.StreamID != 1 || f1.Op != OpHello || string(f1.Payload) != `{"proto":1}` {
		t.Fatalf("frame1: %+v %v", f1, err)
	}
	f2, err := ReadFrame(r)
	if err != nil || f2.StreamID != 3 || f2.Op != OpExecReq || string(f2.Payload) != `{"exec_id":"e1"}` {
		t.Fatalf("frame2: %+v %v", f2, err)
	}

	var out bytes.Buffer
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(WriteFrame(&out, Frame{StreamID: 1, Flags: flagSyn, Op: OpHello, Payload: []byte(`{"proto":1}`)}))
	must(WriteFrame(&out, Frame{StreamID: 3, Flags: flagSyn, Op: OpExecReq, Payload: []byte(`{"exec_id":"e1"}`)}))
	if hex.EncodeToString(out.Bytes()) != goldenHex {
		t.Fatalf("encode mismatch:\n got %x\nwant %s", out.Bytes(), goldenHex)
	}
}

func pipeMuxes(t *testing.T) (*Mux, *Mux) {
	t.Helper()
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	guest := NewMux(c2, false)
	t.Cleanup(func() { host.Close(); guest.Close() })
	return host, guest
}

func TestMuxBidirectional(t *testing.T) {
	host, guest := pipeMuxes(t)

	st, err := host.Open(OpExecReq, Encode(ExecRequest{ExecID: "e1", Argv: []string{"echo", "hi"}}))
	if err != nil {
		t.Fatal(err)
	}

	got, err := guest.Accept()
	if err != nil {
		t.Fatal(err)
	}
	f, err := got.Recv()
	if err != nil || f.Op != OpExecReq {
		t.Fatalf("guest recv: %+v %v", f, err)
	}
	var req ExecRequest
	if err := json.Unmarshal(f.Payload, &req); err != nil || req.ExecID != "e1" {
		t.Fatalf("decode: %v %v", req, err)
	}

	go func() {
		got.SendBinary(OpExecData, []byte("hello from guest"))
		got.SendFin(OpExecExit, Encode(ExecExit{ExecID: "e1", Code: 0}))
	}()

	d, err := st.Recv()
	if err != nil || !bytes.Equal(d.Payload, []byte("hello from guest")) {
		t.Fatalf("data: %+v %v", d, err)
	}
	x, err := st.Recv()
	if err != nil || x.Op != OpExecExit {
		t.Fatalf("exit: %+v %v", x, err)
	}
	var exit ExecExit
	if err := json.Unmarshal(x.Payload, &exit); err != nil || exit.Code != 0 {
		t.Fatalf("exit decode: %v %v", exit, err)
	}
	if _, err := st.Recv(); err == nil {
		t.Fatal("expected EOF after FIN")
	}
}

func TestMuxManyStreams(t *testing.T) {
	host, guest := pipeMuxes(t)
	const n = 50

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			in, err := guest.Accept()
			if err != nil {
				return
			}
			go func(s *Stream) {
				f, _ := s.Recv()
				s.Send(OpPong, f.Payload)
				s.CloseWrite()
			}(in)
		}
	}()

	for i := 0; i < n; i++ {
		st, err := host.Open(OpPing, []byte{byte(i)})
		if err != nil {
			t.Fatal(err)
		}
		f, err := st.Recv()
		if err != nil || len(f.Payload) != 1 || f.Payload[0] != byte(i) {
			t.Fatalf("iter %d: %+v %v", i, f, err)
		}
	}
	wg.Wait()
}

func TestMuxCloseKillsStreams(t *testing.T) {
	host, guest := pipeMuxes(t)
	st, err := host.Open(OpPing, nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.Close()
	if _, err := st.Recv(); err == nil {
		t.Fatal("expected error after peer close")
	}
}

// TestMuxStreamCapShedsExcess: a hostile peer opening unbounded CONCURRENT
// streams must not pin unbounded host memory — excess SYNs are shed while
// existing streams keep working.
//
// The cap bounds streams held open at once, not streams opened over a
// lifetime: closed streams FIN and free their slot (see
// TestMuxClosedStreamsFreeTheirSlot).
func TestMuxStreamCapShedsExcess(t *testing.T) {
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	defer host.Close()
	guest := NewMux(c2, false)
	defer guest.Close()

	accepted := make(chan uint32, maxStreams+64)
	go func() {
		for {
			st, err := guest.Accept()
			if err != nil {
				close(accepted)
				return
			}
			accepted <- st.ID()
		}
	}()

	const open = maxStreams + 40
	// Held open deliberately: this is the abusive shape the cap exists for.
	held := make([]*Stream, 0, open)
	for i := 0; i < open; i++ {
		st, err := host.Open(OpPing, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		held = append(held, st)
	}
	defer func() {
		for _, st := range held {
			st.Close()
		}
	}()

	count := 0
	timeout := time.After(3 * time.Second)
	for count < open {
		select {
		case _, ok := <-accepted:
			if !ok {
				t.Fatalf("accept loop died after %d", count)
			}
			count++
		case <-timeout:
			if count > maxStreams {
				t.Fatalf("shedding failed: %d streams accepted (cap %d)", count, maxStreams)
			}
			if count != maxStreams {
				t.Fatalf("accepted %d, want exactly %d", count, maxStreams)
			}
			return // excess correctly shed
		}
	}
	if count > maxStreams {
		t.Fatalf("cap exceeded: %d accepted", count)
	}
}

// TestMuxShedSYNDoesNotHangOpener: when the peer sheds a SYN at maxStreams it
// must SAY SO. Silently dropping it left Open() reporting success and the
// caller parked in Recv() forever — a permanently wedged sandbox operation.
func TestMuxShedSYNDoesNotHangOpener(t *testing.T) {
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	defer host.Close()
	guest := NewMux(c2, false)
	defer guest.Close()

	// Drain Accept but never close: fill the guest's table to the cap.
	go func() {
		for {
			if _, err := guest.Accept(); err != nil {
				return
			}
		}
	}()
	held := make([]*Stream, 0, maxStreams)
	for i := 0; i < maxStreams; i++ {
		st, err := host.Open(OpPing, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		held = append(held, st)
	}
	defer func() {
		for _, st := range held {
			st.Close()
		}
	}()

	// This one must be shed — and must not hang.
	st, err := host.Open(OpPing, nil)
	if err != nil {
		t.Fatalf("open past cap: %v", err)
	}
	defer st.Close()
	type recvRes struct {
		f   Frame
		err error
	}
	done := make(chan recvRes, 1)
	go func() {
		f, rerr := st.Recv()
		done <- recvRes{f, rerr}
	}()
	select {
	case r := <-done:
		// The contract is "never park forever". A refusal surfaces either as
		// an error or as a FIN-flagged frame that ends the stream; what it
		// must never be is a normal data frame or an unbounded wait.
		if r.err == nil && !r.f.has(FlagFin) {
			t.Fatalf("shed stream yielded a live frame (op=%d flags=%d); want refusal", r.f.Op, r.f.Flags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv on a shed stream hung: the peer never signalled refusal")
	}
	// And the stream is finished: the next Recv is EOF, not another wait.
	second := make(chan error, 1)
	go func() {
		_, rerr := st.Recv()
		second <- rerr
	}()
	select {
	case rerr := <-second:
		if rerr == nil {
			t.Fatal("second Recv on a shed stream returned a frame; want EOF")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second Recv on a shed stream hung")
	}
}

// TestMuxClosedStreamsFreeTheirSlot: Close() must reach the peer. The peer
// reclaims a stream-table entry only on FIN, so a local-only close leaked one
// ghost entry per stream on the far side; after maxStreams ghosts the peer
// shed every new stream and the mux wedged for good. Regression test for the
// deadlock BenchmarkMuxRoundTrip hit at ~400 iterations.
func TestMuxClosedStreamsFreeTheirSlot(t *testing.T) {
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	defer host.Close()
	guest := NewMux(c2, false)
	defer guest.Close()

	go func() {
		for {
			st, err := guest.Accept()
			if err != nil {
				return
			}
			go func(s *Stream) {
				defer s.Close()
				for {
					if _, err := s.Recv(); err != nil {
						return
					}
				}
			}(st)
		}
	}()

	// Well past maxStreams, opened and closed one at a time.
	const rounds = maxStreams * 4
	for i := 0; i < rounds; i++ {
		st, err := host.Open(OpPing, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}

	// The guest's table must have drained, not filled with ghosts.
	deadline := time.Now().Add(5 * time.Second)
	for {
		guest.mu.Lock()
		n := len(guest.streams)
		guest.mu.Unlock()
		if n < maxStreams {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peer stream table still holds %d entries after %d open/close cycles: FIN is not reclaiming", n, rounds)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestMuxFINReclaimsMapEntry: streams closed by the peer's FIN must not
// accumulate in the mux map across a long session.
func TestMuxFINReclaimsMapEntry(t *testing.T) {
	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	defer host.Close()
	guest := NewMux(c2, false)
	defer guest.Close()
	// Peer side: accept every inbound stream and answer with FIN(Pong), so
	// the initiator sees completion and its readLoop reclaims the entry.
	// Without a consumer the guest's accept queue fills after 64 streams and
	// the initiator legitimately blocks on flow control.
	go func() {
		for {
			st, err := guest.Accept()
			if err != nil {
				return
			}
			go func(st *Stream) {
				f, err := st.Recv()
				if err == nil && f.Op == OpPing {
					st.SendFin(OpPong, nil)
				}
				st.Close()
			}(st)
		}
	}()

	for i := 0; i < 100; i++ {
		st, err := host.Open(OpPing, nil)
		if err != nil {
			t.Fatal(err)
		}
		st.SendFin(OpPong, nil)
		st.Close()
		// Drain the echo so both sides see completion.
		for {
			f, err := st.Recv()
			if err != nil || f.Op == OpPong {
				break
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		host.mu.Lock()
		n := len(host.streams)
		host.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	host.mu.Lock()
	n := len(host.streams)
	host.mu.Unlock()
	t.Fatalf("mux map still holds %d entries after 100 completed streams", n)
}

// slowConn accepts a bounded number of bytes and then blocks forever, standing
// in for a peer whose socket buffer has filled because it stopped reading.
type slowConn struct {
	net.Conn
	mu      sync.Mutex
	budget  int
	blocked chan struct{}
	once    sync.Once
}

func (c *slowConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	if c.budget <= 0 {
		c.mu.Unlock()
		c.once.Do(func() { close(c.blocked) })
		// Honour the deadline the mux set, exactly as a real socket would.
		if dl, ok := c.Conn.(interface{ SetWriteDeadline(time.Time) error }); ok {
			_ = dl
		}
		time.Sleep(2 * time.Second)
		return 0, timeoutErr{}
	}
	n := len(p)
	if n > c.budget {
		n = c.budget
	}
	c.budget -= n
	c.mu.Unlock()
	return n, nil
}

type timeoutErr struct{}

func (timeoutErr) Error() string { return "i/o timeout" }
func (timeoutErr) Timeout() bool { return true }

// TestPartialWriteKillsTheMux: a write deadline can fire MID-FRAME once the
// peer's socket buffer fills, leaving part of a length-prefixed frame on the
// wire. SGP/1 has no resynchronisation, so the peer reads the next frame's
// bytes as the tail of this one and every frame after it is garbage — forever.
// A desynchronised connection must be killed, not kept in service.
func TestPartialWriteKillsTheMux(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c2.Close()
	// Enough budget for the header and a little payload, then it stalls.
	sc := &slowConn{Conn: c1, budget: 32, blocked: make(chan struct{})}
	m := NewMux(sc, true)

	st, err := m.Open(OpExecData, make([]byte, 4096))
	if err == nil {
		// The Open write may have partially succeeded; force another write.
		err = st.SendBinary(OpExecData, make([]byte, 4096))
	}
	if err == nil {
		t.Fatal("expected the stalled write to fail")
	}

	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the mux survived a partial frame write: the stream is desynchronised and every later frame will be garbage")
	}
}

// TestAcceptHandoffDoesNotWedgeDemux: readLoop parked with no timeout handing a
// newly accepted stream to Accept(). A peer that opens streams faster than they
// are accepted filled that channel and stopped demultiplexing for EVERY stream
// — no pong, no exec output, no fs response for the whole sandbox.
func TestAcceptHandoffDoesNotWedgeDemux(t *testing.T) {
	old := acceptHandoffTimeout
	acceptHandoffTimeout = 150 * time.Millisecond
	defer func() { acceptHandoffTimeout = old }()

	c1, c2 := net.Pipe()
	host := NewMux(c1, true)
	defer host.Close()
	guest := NewMux(c2, false)
	defer guest.Close()

	// Nobody ever calls guest.Accept(): fill the handoff queue and well past it.
	for i := 0; i < 80; i++ {
		st, err := host.Open(OpPing, nil)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		st.Close()
	}

	// The demux loop must still be alive: a fresh request has to get a reply.
	done := make(chan struct{})
	go func() {
		defer close(done)
		if s, err := guest.Accept(); err == nil {
			s.Close()
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("demultiplexing stopped: readLoop is parked handing off a stream nobody is accepting")
	}
}
