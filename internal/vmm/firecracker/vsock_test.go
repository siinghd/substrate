package firecracker

import (
	"bufio"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
)

// fakeVsockUDS implements the host end of a Firecracker vsock device: accept a
// connection, read "CONNECT <port>\n", answer "OK <hostport>\n", then hand the
// stream to the guest agent. accepted receives the port each caller asked for.
func fakeVsockUDS(t *testing.T, path string, ok bool, accepted chan<- uint32) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				br := bufio.NewReader(c)
				line, err := br.ReadString('\n')
				if err != nil {
					c.Close()
					return
				}
				var port uint32
				if _, err := fmt.Sscanf(strings.TrimSpace(line), "CONNECT %d", &port); err != nil {
					c.Close()
					return
				}
				select {
				case accepted <- port:
				default:
				}
				if !ok {
					fmt.Fprintf(c, "ERROR Connection refused\n")
					c.Close()
					return
				}
				fmt.Fprintf(c, "OK 1234\n")
				// Behave like guestd: announce HELLO so the session settles.
				mux := proto.NewMux(c, false)
				st, err := mux.Open(proto.OpHello, proto.Encode(proto.Hello{
					Proto: 1, Agent: "fake-guestd", Version: "1.0.0",
				}))
				if err == nil {
					st.Close()
				}
			}(c)
		}
	}()
}

// TestVsockConnectHandshake: Firecracker does not put the guest on the host's
// AF_VSOCK address space — the host end of its vsock is a UDS, reached by
// dialling it and sending "CONNECT <port>". The driver used to configure the
// device with no uds_path at all and then open an AF_VSOCK socket to the guest
// CID, so there was nothing to connect to from either side.
func TestVsockConnectHandshake(t *testing.T) {
	dir := t.TempDir()
	uds := filepath.Join(dir, "vsock.sock")
	accepted := make(chan uint32, 1)
	fakeVsockUDS(t, uds, true, accepted)

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir})
	sess, err := d.connectGuest(uds, 5*time.Second)
	if err != nil {
		t.Fatalf("connectGuest: %v", err)
	}
	defer sess.Close()

	select {
	case port := <-accepted:
		if port != guestdVsockPort {
			t.Fatalf("host asked for vsock port %d, want %d", port, guestdVsockPort)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no CONNECT reached the vsock device")
	}

	// The handshake line must not be delivered into the protocol stream.
	if h, err := sess.Hello(3 * time.Second); err != nil {
		t.Fatalf("guest handshake after CONNECT: %v", err)
	} else if h.Agent != "fake-guestd" {
		t.Fatalf("unexpected guest hello: %+v", h)
	}
}

// TestVsockConnectRefusedIsAnError: a refused CONNECT must surface, not be
// treated as a live session that then hangs.
func TestVsockConnectRefusedIsAnError(t *testing.T) {
	dir := t.TempDir()
	uds := filepath.Join(dir, "vsock.sock")
	accepted := make(chan uint32, 1)
	fakeVsockUDS(t, uds, false, accepted)

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir})
	if _, err := d.connectGuest(uds, 1500*time.Millisecond); err == nil {
		t.Fatal("a refused vsock CONNECT was reported as a live session")
	}
}

// TestVsockDeviceIsConfiguredWithUDSPath: without uds_path Firecracker creates
// no host endpoint at all.
func TestVsockDeviceIsConfiguredWithUDSPath(t *testing.T) {
	d, fc, dir := fcEnv(t)
	spec := specWithTemplate(t, "sb_vsockcfg00001", dir)
	m, err := d.Create(contextTODO(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Kill()

	body := fc.bodyFor("/vsock")
	if body == "" {
		t.Fatal("the driver never configured a vsock device")
	}
	if !strings.Contains(body, "uds_path") {
		t.Fatalf("vsock configured without uds_path: %s", body)
	}
	if !strings.Contains(body, fcVsockUDSInChroot) {
		t.Fatalf("vsock uds_path is not the chroot path the VMM can create: %s", body)
	}
}
