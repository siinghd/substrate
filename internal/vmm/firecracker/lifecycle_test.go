package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// TestDoneFiresWhenVMMDiesUnexpectedly: Done() closed only inside Kill, so a
// crashed microVM stayed "live" forever — the reconciler's watch goroutine
// never fired, the supervisor kept the map entry, and nothing ever restarted
// the sandbox. The platform did not know the sandbox was gone.
func TestDoneFiresWhenVMMDiesUnexpectedly(t *testing.T) {
	old := vmmPollInterval
	vmmPollInterval = 20 * time.Millisecond
	defer func() { vmmPollInterval = old }()

	d, _, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_vmmcrash00001", Template: mustTemplate(t, dir)}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer m.Kill()

	// Stand in for the daemonized Firecracker: a real process whose pid the
	// jail's pidfile names, which we then kill out from under the driver.
	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	fcm := m.(*machine)
	if err := os.MkdirAll(filepath.Dir(fcm.pidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fcm.pidFile, []byte(itoaTest(victim.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	go fcm.supervise()

	// It is alive: Done must stay open.
	select {
	case <-m.Done():
		t.Fatal("Done fired while the VMM was still running")
	case <-time.After(200 * time.Millisecond):
	}

	_ = victim.Process.Kill()
	_, _ = victim.Process.Wait()

	select {
	case <-m.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("VMM died and Done never fired: the machine is pinned live forever and the sandbox is never restarted")
	}
}

// TestCreateErrorDoesNotOrphanTheVMM: every error path after the jailer spawn
// used to walk away from a daemonized Firecracker, leaving it holding RAM, a
// tap, a vsock CID and fds with nothing tracking it.
func TestCreateErrorDoesNotOrphanTheVMM(t *testing.T) {
	dir := t.TempDir()
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	// Make the guest dial fail, so Create errors AFTER the spawn.
	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(func(string, time.Duration) (*vmm.GuestSession, error) {
		return nil, context.DeadlineExceeded
	})

	// The stand-in VMM is a plain process; substitute the identity check so
	// this test exercises the cleanup, not the check (which has its own test).
	withPermissiveProcCheck(t)
	spec := vmm.Spec{ID: "sb_orphan0000001", Template: mustTemplate(t, dir)}

	// A live "VMM" the failed create must clean up.
	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	defer victim.Process.Kill()
	pidPath := d.JailDirsForTest(spec.ID).hostPath(fcPidFileInChroot)
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pidPath, []byte(itoaTest(victim.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Create(context.Background(), spec); err == nil {
		t.Fatal("create succeeded despite a failing guest dial")
	}
	_ = fc

	// Reap and inspect: a killed child stays a zombie until waited, and
	// processAlive (signal 0) reports a zombie as alive. In production the VMM
	// is daemonized and reparented to init, so this only matters to the test.
	waited := make(chan *os.ProcessState, 1)
	go func() { st, _ := victim.Process.Wait(); waited <- st }()
	select {
	case st := <-waited:
		ws, ok := st.Sys().(syscall.WaitStatus)
		if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
			t.Fatalf("the spawned VMM exited as %v, not by our SIGKILL", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the spawned VMM is still running after a failed create: it is an orphan holding RAM, tap and vsock CID")
	}
}

// TestCleanupRemovesTheJailTree: destroy removed only the supervisor's own
// workdir, so the jail tree — the per-sandbox rootfs copy and the full-RAM
// memfile — survived every destroy.
func TestCleanupRemovesTheJailTree(t *testing.T) {
	d, _, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_jailtree00001", Template: mustTemplate(t, dir)}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m.Kill()

	j := d.JailDirsForTest(spec.ID)
	tree := j.tree()
	if _, err := os.Stat(tree); err != nil {
		t.Fatalf("jail tree was never created: %v", err)
	}
	// Prove it is not empty: this is the disk the leak was made of.
	if _, err := os.Stat(filepath.Join(j.fs(), "rootfs.ext4")); err != nil {
		t.Fatalf("staged rootfs missing: %v", err)
	}

	var cleaner vmm.Cleaner = d
	if err := cleaner.Cleanup(context.Background(), spec.ID); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatalf("jail tree still present after cleanup (err=%v)", err)
	}
}

// TestCleanupRefusesHostileID keeps the destroy path from becoming an
// arbitrary-path delete.
func TestCleanupRefusesHostileID(t *testing.T) {
	d, _, dir := fcEnv(t)
	victim := filepath.Join(dir, "important")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../important", "..", "/", "sb_../../etc", ""} {
		if err := d.Cleanup(context.Background(), id); err == nil {
			t.Errorf("cleanup accepted hostile id %q", id)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("hostile cleanup id removed an unrelated directory: %v", err)
	}
}

func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
