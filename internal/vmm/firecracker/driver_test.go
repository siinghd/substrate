package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

func buildShim(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "shim")
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/shim")
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	return bin
}

// fakeVMM is a CONTRACT-ACCURATE stand-in for the Firecracker API.
//
// The previous version answered 204 to every PUT/PATCH regardless of state,
// which is why an entire class of driver bugs was invisible to these tests: a
// driver that drives the API in an illegal order passed just as happily as one
// that does it right. This host cannot run real Firecracker (see
// docs/KVM-AVAILABILITY.md), so the next best thing is a double that enforces
// the same state machine the real VMM does.
//
// Rules encoded, from the Firecracker API spec and the snapshotting docs:
//   - instance states: "Not started" -> "Running" <-> "Paused"
//   - InstanceStart requires boot-source + a root drive, and only from
//     "Not started"
//   - PATCH /vm Paused requires Running; Resumed requires Paused
//   - PUT /snapshot/create requires the microVM to be PAUSED -- this is the
//     documented precondition, and violating it is a 400
//   - PUT /snapshot/load only from "Not started"; the loaded VM comes back
//     Paused unless resume_vm is set
//   - configuration endpoints are rejected after boot
type fakeVMM struct {
	mu     sync.Mutex
	calls  []string
	bodies map[string]string

	state       string // "Not started" | "Running" | "Paused"
	bootSource  bool
	rootDrive   bool
	transitions []string
	// rejected records (method path -> reason) for assertions about misuse.
	rejected []string
	// snapshots written, by path.
	snapshots []string
	chrootDir string
	failSnap  bool
}

func newFakeVMM(t *testing.T, sockPath string) *fakeVMM {
	t.Helper()
	os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeVMM{bodies: map[string]string{}, state: "Not started"}
	go http.Serve(ln, f)
	t.Cleanup(func() { ln.Close() })
	return f
}

// reset models the VMM PROCESS ending and a new one starting on the same API
// socket. A test that creates then restores is driving two different
// Firecracker processes; the fake is one instance, so it has to be told.
// failSnapshotCreate makes /snapshot/create fail even from a legal state, to
// exercise the driver's recovery path.
func (f *fakeVMM) failSnapshotCreate(v bool) {
	f.mu.Lock()
	f.failSnap = v
	f.mu.Unlock()
}

// setChroot tells the fake where the jail root is on the host, so it can
// materialise snapshot files at the paths the driver asked for.
func (f *fakeVMM) setChroot(dir string) {
	f.mu.Lock()
	f.chrootDir = dir
	f.mu.Unlock()
}

func (f *fakeVMM) snapshotPaths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.snapshots...)
}

func (f *fakeVMM) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state = "Not started"
	f.bootSource = false
	f.rootDrive = false
}

func (f *fakeVMM) callsInOrder() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeVMM) bodyFor(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[path]
}

func (f *fakeVMM) vmState() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *fakeVMM) rejections() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rejected...)
}

func (f *fakeVMM) stateTransitions() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.transitions...)
}

// fcError mirrors Firecracker's error envelope.
func (f *fakeVMM) reject(w http.ResponseWriter, r *http.Request, reason string) {
	f.mu.Lock()
	f.rejected = append(f.rejected, r.Method+" "+r.URL.Path+": "+reason)
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(map[string]string{"fault_message": reason})
}

func (f *fakeVMM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	f.bodies[r.URL.Path] = string(body)
	started := f.state != "Not started"
	f.mu.Unlock()

	key := r.Method + " " + r.URL.Path
	switch {
	case r.Method == "GET" && r.URL.Path == "/":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"state": f.vmState(), "id": "fake"})

	case key == "PUT /boot-source":
		if started {
			f.reject(w, r, "The requested operation is not supported after starting the microVM.")
			return
		}
		f.mu.Lock()
		f.bootSource = true
		f.mu.Unlock()
		w.WriteHeader(204)

	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/drives/"):
		if started {
			f.reject(w, r, "The requested operation is not supported after starting the microVM.")
			return
		}
		var d map[string]any
		json.Unmarshal(body, &d)
		f.mu.Lock()
		if v, _ := d["is_root_device"].(bool); v {
			f.rootDrive = true
		}
		f.mu.Unlock()
		w.WriteHeader(204)

	case key == "PUT /machine-config", r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/network-interfaces/"),
		key == "PUT /vsock", key == "PUT /logger", key == "PUT /metrics":
		if started {
			f.reject(w, r, "The requested operation is not supported after starting the microVM.")
			return
		}
		w.WriteHeader(204)

	case key == "PUT /actions":
		var a map[string]any
		json.Unmarshal(body, &a)
		if a["action_type"] != "InstanceStart" {
			w.WriteHeader(204)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.state != "Not started" {
			f.rejected = append(f.rejected, key+": microVM already started")
			w.WriteHeader(400)
			return
		}
		if !f.bootSource || !f.rootDrive {
			f.rejected = append(f.rejected, key+": incomplete configuration")
			w.WriteHeader(400)
			return
		}
		f.state = "Running"
		f.transitions = append(f.transitions, "Not started->Running")
		w.WriteHeader(204)

	case key == "PATCH /vm":
		var v map[string]any
		json.Unmarshal(body, &v)
		want, _ := v["state"].(string)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch want {
		case "Paused":
			if f.state != "Running" {
				f.rejected = append(f.rejected, key+": cannot pause from state "+f.state)
				w.WriteHeader(400)
				return
			}
			f.state = "Paused"
			f.transitions = append(f.transitions, "Running->Paused")
		case "Resumed":
			if f.state != "Paused" {
				f.rejected = append(f.rejected, key+": cannot resume from state "+f.state)
				w.WriteHeader(400)
				return
			}
			f.state = "Running"
			f.transitions = append(f.transitions, "Paused->Running")
		default:
			f.rejected = append(f.rejected, key+": unknown state "+want)
			w.WriteHeader(400)
			return
		}
		w.WriteHeader(204)

	case key == "PUT /snapshot/create":
		f.mu.Lock()
		st := f.state
		f.mu.Unlock()
		// THE contract: Firecracker refuses to snapshot a running microVM.
		if st != "Paused" {
			f.reject(w, r, "Cannot create snapshot: microVM is not paused (state "+st+")")
			return
		}
		f.mu.Lock()
		fail := f.failSnap
		f.mu.Unlock()
		if fail {
			f.reject(w, r, "injected snapshot failure")
			return
		}
		var b map[string]any
		json.Unmarshal(body, &b)
		snapPath, _ := b["snapshot_path"].(string)
		memPath, _ := b["mem_file_path"].(string)
		f.mu.Lock()
		f.snapshots = append(f.snapshots, snapPath, memPath)
		chroot := f.chrootDir
		f.mu.Unlock()
		// Materialise the files where the driver told us to, so tests can
		// assert the paths are ones the VMM could actually reach.
		if chroot != "" {
			for _, p := range []string{snapPath, memPath} {
				if p == "" {
					continue
				}
				full := filepath.Join(chroot, p)
				os.MkdirAll(filepath.Dir(full), 0o755)
				os.WriteFile(full, []byte("snapshot"), 0o644)
			}
		}
		w.WriteHeader(204)

	case key == "PUT /snapshot/load":
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.state != "Not started" {
			f.rejected = append(f.rejected, key+": cannot load a snapshot into a started microVM")
			w.WriteHeader(400)
			return
		}
		var b map[string]any
		json.Unmarshal(body, &b)
		mb, _ := b["mem_backend"].(map[string]any)
		if mb["type"] != "File" && mb["type"] != "Uffd" {
			f.rejected = append(f.rejected, key+": bad mem_backend")
			w.WriteHeader(400)
			return
		}
		if resume, _ := b["resume_vm"].(bool); resume {
			f.state = "Running"
			f.transitions = append(f.transitions, "Not started->Running (load+resume)")
		} else {
			f.state = "Paused"
			f.transitions = append(f.transitions, "Not started->Paused (load)")
		}
		w.WriteHeader(204)

	default:
		w.WriteHeader(404)
	}
}

func startShim(t *testing.T, shimBin, root string) string {
	t.Helper()
	sock := filepath.Join(root, "guest.sock")
	cmd := exec.Command(shimBin, "--unix", sock, "--root", root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	return sock
}

// dialTestGuest stands in for the vsock dialer: it ignores the jail's UDS path
// and connects straight to the shim's socket, skipping the CONNECT handshake.
// The handshake itself is covered by TestVsockConnectHandshake.
func dialTestGuest(sock string) func(string, time.Duration) (*vmm.GuestSession, error) {
	return func(_ string, timeout time.Duration) (*vmm.GuestSession, error) {
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			conn, err := net.Dial("unix", sock)
			if err == nil {
				return vmm.NewGuestSession(conn), nil
			}
			time.Sleep(5 * time.Millisecond)
		}
		return nil, errDialTimeout
	}
}

var errDialTimeout = context.DeadlineExceeded

func TestDriverCreateCallSequence(t *testing.T) {
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(dialTestGuest(guestSock))

	rootfs := filepath.Join(dir, "rootfs.ext4")
	kernel := filepath.Join(dir, "vmlinux")
	os.WriteFile(rootfs, []byte("x"), 0o644)
	os.WriteFile(kernel, []byte("k"), 0o644)

	spec := vmm.Spec{
		ID:       "sb_fctest1234567",
		Template: vmm.TemplateSpec{Name: "t", RootfsPath: rootfs, KernelPath: kernel, MemMiB: 512, VCPUs: 1},
	}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Kill()

	want := []string{
		"PUT /boot-source",
		"PUT /machine-config",
		"PUT /drives/rootfs",
		"PUT /vsock",
		"PUT /actions",
	}
	got := fc.callsInOrder()
	for i, w := range want {
		if i >= len(got) || got[i] != w {
			t.Fatalf("call %d: want %q got %q\nfull: %v", i, w, got[i], got)
		}
	}

	h, err := m.Exec(context.Background(), vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", "echo hello-fc-driver"}})
	if err != nil {
		t.Fatal(err)
	}
	type outResult struct {
		data []byte
	}
	outCh := make(chan outResult, 1)
	go func() {
		var buf bytes.Buffer
		chunk := make([]byte, 4096)
		for {
			n, err := h.Stdout.Read(chunk)
			buf.Write(chunk[:n])
			if err != nil {
				outCh <- outResult{buf.Bytes()}
				return
			}
		}
	}()
	ei := <-h.ExitCh
	select {
	case res := <-outCh:
		if ei.Code != 0 || !bytes.Contains(res.data, []byte("hello-fc-driver")) {
			t.Fatalf("exec via fake VMM: code=%d out=%q", ei.Code, res.data)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("output pipe never closed after exit (code=%d)", ei.Code)
	}
}

func TestDriverRestoreSequenceAndReseed(t *testing.T) {
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(dialTestGuest(guestSock))

	spec := vmm.Spec{ID: "sb_fcrestore1234", Template: vmm.TemplateSpec{Name: "t"}}
	m, err := d.Restore(context.Background(), spec, "", "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer m.Kill()

	foundLoad, foundResume := false, false
	for _, c := range fc.callsInOrder() {
		if c == "PUT /snapshot/load" {
			foundLoad = true
		}
		if c == "PATCH /vm" {
			foundResume = true
		}
	}
	if !foundLoad || !foundResume {
		t.Fatalf("restore sequence incomplete: %v", fc.callsInOrder())
	}
	_ = fc
}
