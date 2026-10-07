package vmm

import (
	"context"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
)

// mutePeer accepts the mux handshake and then answers nothing, ever — the
// state an end user reaches with `kill -STOP $(pidof guestd)` inside their own
// microVM. The connection stays open; no frame ever comes back.
// fastHeartbeat shrinks the cadence for tests and restores it afterwards.
func fastHeartbeat(t *testing.T) {
	t.Helper()
	oi, ot := heartbeatInterval, heartbeatTimeout
	heartbeatInterval, heartbeatTimeout = 100*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { heartbeatInterval, heartbeatTimeout = oi, ot })
}

func heartbeatBudget() time.Duration {
	return heartbeatInterval*time.Duration(heartbeatFailures+1) +
		heartbeatTimeout*time.Duration(heartbeatFailures) + 5*time.Second
}

func mutePeer(t *testing.T) (*GuestSession, func()) {
	t.Helper()
	hostConn, guestConn := net.Pipe()
	stop := make(chan struct{})
	go func() {
		// Drain and discard so the host's writes never block.
		buf := make([]byte, 4096)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := guestConn.Read(buf); err != nil {
				return
			}
		}
	}()
	g := NewGuestSession(hostConn)
	return g, func() { close(stop); guestConn.Close() }
}

// TestHeartbeatKillsMuteSession: docs/protocol.md promises "missing PONG within
// 10s marks the connection dead", but Ping had no caller outside tests and no
// read deadline was ever set. A mute guest therefore parked every host-side
// operation forever — one log fd, three goroutines and a permanently "running"
// exec row per request, reclaimed by nothing. The session must die on its own.
func TestHeartbeatKillsMuteSession(t *testing.T) {
	fastHeartbeat(t)
	g, cleanup := mutePeer(t)
	defer cleanup()
	defer g.Close()

	budget := heartbeatBudget()
	select {
	case <-g.Done():
	case <-time.After(budget):
		t.Fatalf("session with a mute peer was still alive after %v: nothing detects a dead guest, so every blocked operation leaks", budget)
	}
}

// TestHeartbeatUnblocksParkedReceivers: closing the mux is only useful if it
// actually cascades EOF into the goroutines a mute guest has parked. This is
// the mechanism that reclaims the leaked fds and exec rows.
func TestHeartbeatUnblocksParkedReceivers(t *testing.T) {
	fastHeartbeat(t)
	g, cleanup := mutePeer(t)
	defer cleanup()
	defer g.Close()

	st, err := g.mux.Open(proto.OpExecReq, []byte("{}"))
	if err != nil {
		t.Fatal(err)
	}
	var returned atomic.Bool
	go func() {
		_, _ = st.Recv() // would park forever against a mute peer
		returned.Store(true)
	}()

	deadline := time.Now().Add(heartbeatBudget())
	for time.Now().Before(deadline) {
		if returned.Load() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("a goroutine parked in Recv against a mute peer never returned: closing the session does not reclaim it")
}

// TestHeartbeatToleratesOneLostRoundTrip: the session must not be torn down by
// a single slow or dropped PONG.
func TestHeartbeatToleratesOneLostRoundTrip(t *testing.T) {
	if heartbeatFailures < 2 {
		t.Fatalf("heartbeatFailures = %d: a single lost round trip would kill a healthy session", heartbeatFailures)
	}
}

// TestHelloSurvivesArrivingBeforeTheWaiter: helloCh was an unbuffered channel
// written with a non-blocking send, so a HELLO that landed before anyone was
// parked in Hello() was thrown away — and the caller then waited out its whole
// timeout and failed the create with "guest handshake timeout" purely because
// the guest answered promptly. A latch cannot lose the event.
func TestHelloSurvivesArrivingBeforeTheWaiter(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	g := NewGuestSession(hostConn)
	defer g.Close()

	mux := proto.NewMux(guestConn, false)
	defer mux.Close()
	st, err := mux.Open(proto.OpHello, proto.Encode(proto.Hello{
		Proto: 1, Agent: "early-guestd", Version: "1.0.0",
	}))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	// Deliberately let the HELLO land well before anyone asks for it.
	time.Sleep(200 * time.Millisecond)

	h, err := g.Hello(2 * time.Second)
	if err != nil {
		t.Fatalf("Hello: %v — the handshake was dropped because it arrived before the waiter", err)
	}
	if h.Agent != "early-guestd" {
		t.Fatalf("unexpected hello: %+v", h)
	}
	// A latch must serve repeat callers too.
	if _, err := g.Hello(2 * time.Second); err != nil {
		t.Fatalf("second Hello: %v — the latch was consumed by the first caller", err)
	}
}

// TestPortsAnnounceSurvivesEarlyArrival: same defect, same fix, on the ports
// announcement that create waits for.
func TestPortsAnnounceSurvivesEarlyArrival(t *testing.T) {
	hostConn, guestConn := net.Pipe()
	g := NewGuestSession(hostConn)
	defer g.Close()

	mux := proto.NewMux(guestConn, false)
	defer mux.Close()
	st, err := mux.Open(proto.OpPorts, proto.Encode(proto.PortsAnnounce{
		Ports: []proto.PortAnnounce{{Port: 8080, Proto: "tcp"}},
	}))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	time.Sleep(200 * time.Millisecond)

	ports, err := g.WaitPorts(2 * time.Second)
	if err != nil {
		t.Fatalf("WaitPorts: %v", err)
	}
	if len(ports) != 1 || ports[0].Port != 8080 {
		t.Fatalf("ports = %+v", ports)
	}
	if _, err := g.WaitPorts(2 * time.Second); err != nil {
		t.Fatalf("second WaitPorts: %v — the latch was consumed", err)
	}
}

// floodPeer answers every fs request with an endless stream of binary frames and
// never sends the terminal OpFsResp — a tenant who replaced the guest agent
// with something hostile, which they can, because they own the rootfs.
func floodPeer(t *testing.T, chunk int, delay time.Duration) (*GuestSession, func()) {
	t.Helper()
	hostConn, guestConn := net.Pipe()
	g := NewGuestSession(hostConn)
	mux := proto.NewMux(guestConn, false)
	stop := make(chan struct{})
	go func() {
		for {
			st, err := mux.Accept()
			if err != nil {
				return
			}
			go func(s *proto.Stream) {
				payload := make([]byte, chunk)
				for {
					select {
					case <-stop:
						return
					default:
					}
					if err := s.SendBinary(proto.OpFsData, payload); err != nil {
						return
					}
					if delay > 0 {
						time.Sleep(delay)
					}
				}
			}(st)
		}
	}()
	return g, func() { close(stop); mux.Close(); guestConn.Close() }
}

// TestFsResponseIsBounded: awaitFsResp appended guest-controlled bytes with no
// ceiling, so one fs read against a hostile agent grew without bound until the
// node OOMed — taking every co-tenant sandbox on it down.
func TestFsResponseIsBounded(t *testing.T) {
	oldMax := maxFsResponseBytesForTest()
	setMaxFsResponseBytesForTest(256 << 10)
	defer setMaxFsResponseBytesForTest(oldMax)

	g, cleanup := floodPeer(t, 32<<10, 0)
	defer cleanup()
	defer g.Close()

	done := make(chan error, 1)
	go func() {
		_, err := g.Fs().Read(context.Background(), "/x", 0, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an endless guest response was accepted")
		}
		if !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("read failed with %v, want a size-limit error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("fs read never returned against a flooding guest: the response is unbounded")
	}
}

// TestFsOpTimeoutIsNotResetByTraffic: the old 30s timeout was created INSIDE the
// receive loop, so it restarted on every frame and could never fire against a
// guest that kept sending. A deadline that resets whenever the adversary acts is
// not a deadline.
func TestFsOpTimeoutIsNotResetByTraffic(t *testing.T) {
	oldTO := fsOpTimeout
	fsOpTimeout = 2 * time.Second
	defer func() { fsOpTimeout = oldTO }()
	// The guest sends SLOWLY and the ceiling is high, so neither the size cap
	// nor an idle stream can end this -- only a real overall deadline can.
	// Without the pacing the flood hits the ceiling first and the test passes
	// for the wrong reason, which it did on the first attempt.
	oldMax := maxFsResponseBytesForTest()
	setMaxFsResponseBytesForTest(1 << 30)
	defer setMaxFsResponseBytesForTest(oldMax)

	g, cleanup := floodPeer(t, 1<<10, 50*time.Millisecond)
	defer cleanup()
	defer g.Close()

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := g.Fs().Read(context.Background(), "/x", 0, 0)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("endless response accepted")
		}
		if elapsed := time.Since(start); elapsed > 20*time.Second {
			t.Fatalf("took %v to give up: the deadline is being reset by incoming frames", elapsed)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("fs op never timed out against a continuously-sending guest")
	}
}

func maxFsResponseBytesForTest() int     { return maxFsResponseBytes }
func setMaxFsResponseBytesForTest(n int) { maxFsResponseBytes = n }
