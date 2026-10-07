// Package proto implements SGP/1, the framed multiplexed protocol spoken between
// noded and guestd over vsock (production) or unix/TCP sockets (dev). See
// docs/protocol.md for the wire contract. The Rust implementation in guestd/src/
// must stay byte-compatible; golden vectors live in testdata/.
package proto

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	MaxFrameSize = 1 << 20 // 1 MiB hard cap on payload length

	flagSyn    uint8 = 1
	flagFin    uint8 = 2
	flagBinary uint8 = 4
)

// Public aliases for cross-package use.
const (
	FlagSyn    = flagSyn
	FlagFin    = flagFin
	FlagBinary = flagBinary
)

const (
	OpPing uint8 = iota
	OpPong
	OpHello
	OpExecReq
	OpExecData
	OpExecExit
	OpExecSignal
	OpFsReq
	OpFsData
	OpFsResp
	OpPorts
	OpMetrics
	OpReseed
	OpReseedAck
	OpExecResize
)

type Frame struct {
	StreamID uint32
	Flags    uint8
	Op       uint8
	Payload  []byte
}

func (f *Frame) has(flag uint8) bool { return f.Flags&flag != 0 }

var ErrFrameTooLarge = errors.New("proto: frame exceeds MaxFrameSize")

func WriteFrame(w io.Writer, f Frame) error {
	if len(f.Payload) > MaxFrameSize {
		return ErrFrameTooLarge
	}
	var hdr [13]byte
	binary.LittleEndian.PutUint32(hdr[0:4], f.StreamID)
	hdr[4] = f.Flags
	hdr[5] = f.Op
	binary.LittleEndian.PutUint32(hdr[6:10], uint32(len(f.Payload)))
	hdr[10] = 0
	hdr[11] = 0
	hdr[12] = 0
	// Short writes are checked explicitly. io.Writer requires an error on a
	// short write, but ignoring n meant a writer that returned (k<len, nil)
	// silently truncated a frame and WriteFrame reported success — the same
	// stream desynchronisation as a mid-frame timeout, with no error anywhere.
	if n, err := w.Write(hdr[:]); err != nil {
		return err
	} else if n != len(hdr) {
		return io.ErrShortWrite
	}
	if len(f.Payload) == 0 {
		return nil
	}
	n, err := w.Write(f.Payload)
	if err != nil {
		return err
	}
	if n != len(f.Payload) {
		return io.ErrShortWrite
	}
	return nil
}

// DebugHook, when set, receives every received frame (test instrumentation).
var DebugHook func(Frame)
var DebugOutgoing bool

func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [13]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{
		StreamID: binary.LittleEndian.Uint32(hdr[0:4]),
		Flags:    hdr[4],
		Op:       hdr[5],
	}
	n := binary.LittleEndian.Uint32(hdr[6:10])
	if n > MaxFrameSize {
		return Frame{}, ErrFrameTooLarge
	}
	if n > 0 {
		f.Payload = make([]byte, n)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			return Frame{}, err
		}
	}
	if DebugHook != nil {
		DebugHook(f)
	}
	return f, nil
}

// stream is one logical operation channel.
type stream struct {
	id      uint32
	ch      chan Frame
	closed  chan struct{}
	closeMu sync.Once
}

// pushTimeout bounds how long the demux loop waits for ONE slow consumer.
// The previous unbounded `s.ch <- f` meant a single stream whose reader had
// wandered off wedged readLoop, and with it every other stream on the same
// mux — head-of-line blocking across the whole sandbox. Now a stalled stream
// fails alone.
const pushTimeout = 30 * time.Second

// push delivers f to the stream's buffer. ok reports delivery; stalled
// distinguishes "consumer too slow" (reset the peer) from "already closed".
func (s *stream) push(f Frame) (ok bool, stalled bool) {
	select {
	case <-s.closed:
		return false, false
	default:
	}
	// Fast path: room in the buffer, no timer allocated.
	select {
	case s.ch <- f:
		return true, false
	case <-s.closed:
		return false, false
	default:
	}
	t := time.NewTimer(pushTimeout)
	defer t.Stop()
	select {
	case s.ch <- f:
		return true, false
	case <-s.closed:
		return false, false
	case <-t.C:
		return false, true
	}
}

func (s *stream) close() { s.closeMu.Do(func() { close(s.closed) }) }

// Mux multiplexes Frame streams over a single byte stream. Hosts allocate odd
// stream ids, guests even, so both ends can initiate concurrently.
type Mux struct {
	rwc     io.ReadWriteCloser
	writeMu sync.Mutex

	mu      sync.Mutex
	streams map[uint32]*stream
	nextID  uint32

	incoming chan *stream
	done     chan struct{}
	dieOnce  sync.Once

	// resets carries stream ids the readLoop refused (shed at maxStreams, or
	// stalled). A dedicated writer drains it so the demux loop never blocks
	// on the socket to deliver a reset.
	resets chan uint32
}

// Observability counters. This package is linked into cmd/shim, which runs
// INSIDE the guest, so it must stay dependency-free — noded scrapes these
// instead of proto importing the metrics package.
var (
	StreamsShed    atomic.Uint64 // inbound SYNs refused at maxStreams
	StreamsStalled atomic.Uint64 // streams reset because the consumer stalled
	StreamResets   atomic.Uint64 // reset frames actually written
	WriteTimeouts  atomic.Uint64 // frame writes abandoned on deadline
)

// writeTimeout bounds a single frame write. Without it a peer that stops
// reading (a hostile guest is under no obligation to read) blocks the writer
// forever while holding writeMu — which stops every other stream from
// sending, and makes a deferred Close() hang.
const writeTimeout = 30 * time.Second

// acceptHandoffTimeout bounds how long readLoop waits to hand a newly accepted
// stream to Accept(). Parking here blocks demultiplexing for EVERY stream.
var acceptHandoffTimeout = 10 * time.Second

func NewMux(rwc io.ReadWriteCloser, initiator bool) *Mux {
	m := &Mux{
		rwc:      rwc,
		streams:  make(map[uint32]*stream),
		nextID:   1,
		incoming: make(chan *stream, 64),
		done:     make(chan struct{}),
		resets:   make(chan uint32, 64),
	}
	if !initiator {
		m.nextID = 2
	}
	go m.readLoop()
	go m.resetLoop()
	return m
}

// resetLoop writes refusal frames off the demux path. Bounded queue: when a
// peer is abusive enough to overflow it, dropping further resets is correct —
// it is already being told to stop.
func (m *Mux) resetLoop() {
	for {
		select {
		case <-m.done:
			return
		case id := <-m.resets:
			if err := m.writeFrame(Frame{StreamID: id, Flags: FlagFin, Op: OpPing}); err != nil {
				return
			}
			StreamResets.Add(1)
		}
	}
}

// reset asks the peer to abandon a stream, without ever blocking the caller.
func (m *Mux) reset(id uint32) {
	select {
	case m.resets <- id:
	default:
	}
}

func (m *Mux) nextStreamID() uint32 {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID += 2
	return id
}

// Open creates a new stream whose first frame carries op/payload with SYN set.
func (m *Mux) Open(op uint8, payload []byte) (*Stream, error) {
	s := &stream{
		id:     m.nextStreamID(),
		ch:     make(chan Frame, 64),
		closed: make(chan struct{}),
	}
	m.mu.Lock()
	if m.isDead() {
		m.mu.Unlock()
		return nil, errors.New("proto: mux closed")
	}
	m.streams[s.id] = s
	m.mu.Unlock()

	first := Frame{StreamID: s.id, Flags: FlagSyn, Op: op, Payload: payload}
	if err := m.writeFrame(first); err != nil {
		m.drop(s.id)
		return nil, err
	}
	return &Stream{s: s, m: m}, nil
}

// Accept returns the next inbound stream opened by the remote side.
func (m *Mux) Accept() (*Stream, error) {
	select {
	case s := <-m.incoming:
		return &Stream{s: s, m: m}, nil
	case <-m.done:
		return nil, errors.New("proto: mux closed")
	}
}

func (m *Mux) isDead() bool {
	select {
	case <-m.done:
		return true
	default:
		return false
	}
}

func (m *Mux) writeFrame(f Frame) error {
	if DebugOutgoing {
		fmt.Fprintf(os.Stderr, "WIRE-OUT sid=%d flags=%d op=%d len=%d\n", f.StreamID, f.Flags, f.Op, len(f.Payload))
	}
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	if c, ok := m.rwc.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = c.SetWriteDeadline(time.Now().Add(writeTimeout))
		defer c.SetWriteDeadline(time.Time{})
	}
	err := WriteFrame(m.rwc, f)
	if err != nil {
		if ne, ok := err.(interface{ Timeout() bool }); ok && ne.Timeout() {
			WriteTimeouts.Add(1)
		}
		// A failed write may have put PART of a frame on the wire — the
		// deadline added in pass 12 fires mid-write once the peer's socket
		// buffer fills. SGP/1 is a length-prefixed stream with no
		// resynchronisation, so the peer would read the NEXT frame's bytes as
		// the tail of this one and every frame after it is garbage, forever.
		// The connection cannot be recovered, so kill the mux and let the
		// caller re-establish it. Continuing on a desynchronised stream is
		// worse than failing.
		m.die()
	}
	return err
}

func (m *Mux) drop(id uint32) {
	m.mu.Lock()
	s := m.streams[id]
	delete(m.streams, id)
	m.mu.Unlock()
	if s != nil {
		s.close()
	}
}

// maxStreams bounds concurrently open streams per mux. Each buffered stream
// can hold 64 frames × up to 1MiB, so an unbounded hostile peer could pin
// gigabytes of host RAM by opening streams and never reading.
const maxStreams = 256

func (m *Mux) readLoop() {
	defer m.die()
	for {
		f, err := ReadFrame(m.rwc)
		if err != nil {
			return
		}
		m.mu.Lock()
		s, ok := m.streams[f.StreamID]
		if !ok && f.has(FlagSyn) {
			if len(m.streams) >= maxStreams {
				// Shed the new stream, but TELL the peer. Silently dropping
				// the SYN left the opener parked in Recv() forever: Open()
				// had already succeeded, so nothing ever reported an error.
				// A FIN on that id turns an unbounded hang into a prompt EOF.
				m.mu.Unlock()
				StreamsShed.Add(1)
				m.reset(f.StreamID)
				continue
			}
			s = &stream{id: f.StreamID, ch: make(chan Frame, 64), closed: make(chan struct{})}
			m.streams[f.StreamID] = s
			m.mu.Unlock()
			// Bounded handoff: if nothing is accepting, the demux loop must
			// not park here. A guest that opens streams faster than serve()
			// takes them filled this channel and wedged readLoop with no
			// timeout at all, which stops PONG, exec output and every fs
			// response for the whole sandbox.
			ht := time.NewTimer(acceptHandoffTimeout)
			select {
			case m.incoming <- s:
				ht.Stop()
			case <-m.done:
				ht.Stop()
				return
			case <-ht.C:
				StreamsShed.Add(1)
				m.drop(f.StreamID)
				m.reset(f.StreamID)
				continue
			}
		} else {
			m.mu.Unlock()
			if !ok {
				continue
			}
		}
		if ok, stalled := s.push(f); !ok {
			if stalled {
				// The consumer stopped reading. Fail this one stream — the
				// mux (and every other sandbox operation on it) survives.
				StreamsStalled.Add(1)
				m.drop(f.StreamID)
				m.reset(f.StreamID)
			}
			continue
		}
		if f.has(FlagFin) {
			s.close()
			// Reclaim the map entry: FIN'd streams were only removed when
			// the local side called Close, so a peer that FINs and moves on
			// grew this map without bound over a long session.
			m.mu.Lock()
			delete(m.streams, f.StreamID)
			m.mu.Unlock()
		}
	}
}

func (m *Mux) die() {
	m.dieOnce.Do(func() {
		close(m.done)
		m.mu.Lock()
		ss := make([]*stream, 0, len(m.streams))
		for _, s := range m.streams {
			ss = append(ss, s)
		}
		m.streams = map[uint32]*stream{}
		m.mu.Unlock()
		for _, s := range ss {
			s.close()
		}
		m.rwc.Close()
	})
}

func (m *Mux) Close() error { m.die(); return nil }

// Done is closed when the transport dies or Close is called.
func (m *Mux) Done() <-chan struct{} { return m.done }

// Stream is a bidirectional message channel within a Mux.
type Stream struct {
	s *stream
	m *Mux
	// finOnce guarantees exactly one FIN reaches the peer per stream,
	// whichever of SendFin/CloseWrite/Close gets there first.
	finOnce sync.Once
}

func (st *Stream) ID() uint32 { return st.s.id }

// Recv returns the next frame on the stream. Frames carrying FIN are delivered
// once; subsequent Recv calls return io.EOF after the buffer drains.
func (st *Stream) Recv() (Frame, error) {
	select {
	case f, ok := <-st.s.ch:
		if !ok {
			return Frame{}, io.EOF
		}
		return f, nil
	case <-st.s.closed:
		// drain remaining buffered frames before EOF
		select {
		case f, ok := <-st.s.ch:
			if !ok {
				return Frame{}, io.EOF
			}
			return f, nil
		default:
			return Frame{}, io.EOF
		}
	}
}

// RecvContext is Recv bounded by ctx. Call sites that must never park
// forever (every request-scoped guest operation) use this.
func (st *Stream) RecvContext(ctx context.Context) (Frame, error) {
	select {
	case f, ok := <-st.s.ch:
		if !ok {
			return Frame{}, io.EOF
		}
		return f, nil
	case <-st.s.closed:
		select {
		case f, ok := <-st.s.ch:
			if !ok {
				return Frame{}, io.EOF
			}
			return f, nil
		default:
			return Frame{}, io.EOF
		}
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

func (st *Stream) Send(op uint8, payload []byte) error {
	return st.m.writeFrame(Frame{StreamID: st.s.id, Op: op, Payload: payload})
}

func (st *Stream) SendBinary(op uint8, payload []byte) error {
	return st.m.writeFrame(Frame{StreamID: st.s.id, Flags: FlagBinary, Op: op, Payload: payload})
}

// sendFin puts exactly one FIN on the wire for this stream.
//
// The peer reclaims its stream-table entry ONLY on FIN (see readLoop), so a
// close that stays local leaks an entry on the far side of every stream. Once
// maxStreams such ghosts accumulate the peer sheds every new stream, and the
// mux wedges. Close therefore has to be a wire event, not a map delete.
func (st *Stream) sendFin(op uint8, payload []byte) error {
	var err error
	st.finOnce.Do(func() {
		err = st.m.writeFrame(Frame{StreamID: st.s.id, Flags: FlagFin, Op: op, Payload: payload})
	})
	return err
}

func (st *Stream) SendFin(op uint8, payload []byte) error { return st.sendFin(op, payload) }

func (st *Stream) CloseWrite() error { return st.sendFin(OpPing, nil) }

func (st *Stream) Close() error {
	_ = st.sendFin(OpPing, nil)
	st.m.drop(st.s.id)
	return nil
}

func (st *Stream) String() string { return fmt.Sprintf("stream/%d", st.s.id) }
