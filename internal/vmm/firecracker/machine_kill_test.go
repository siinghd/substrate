package firecracker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/vmm"
)

func procAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// TestTerminateVMMKillsOnlyTrackedPID pins the kill contract: SIGKILL goes to
// the exact pid recorded in the jail's pidfile and to NOTHING else. The old
// implementation ran pkill -f (never matched) then syscall.Kill(-1, SIGKILL)
// — a host-wide massacre on any match.
func TestTerminateVMMKillsOnlyTrackedPID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "run"), 0o755)

	victim := exec.Command("sleep", "300")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	victimDone := make(chan struct{})
	go func() { victim.Wait(); close(victimDone) }() // reap so SIGKILL takes effect visibly
	bystander := exec.Command("sleep", "301")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		bystander.Process.Kill()
		bystander.Wait()
	})

	pidFile := filepath.Join(dir, fcPidFileInChroot) // host view of /run/firecracker.pid inside the chroot
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(victim.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	// The stand-in VMM is a plain `sleep`, which the production identity check
	// would rightly refuse to signal. Substitute the check so this test still
	// exercises what it is about: that ONLY the tracked pid dies.
	withPermissiveProcCheck(t)
	m := newMachine(specForTest("sb_killtest00001"), jailDirs{base: dir, id: "sb_killtest00001"}, nil, nil, logging.New("error"))
	m.pidFile = pidFile
	m.fcBin = "/nonexistent-firecracker"

	victimAlive := true
	if err := m.terminateVMM(); err != nil {
		t.Fatalf("terminateVMM: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-victimDone:
			victimAlive = false
		default:
		}
		if !victimAlive {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if victimAlive {
		t.Fatal("tracked VMM pid still alive after terminateVMM")
	}
	if !procAlive(bystander.Process.Pid) {
		t.Fatal("BYSTANDER PROCESS KILLED — kill escaped the tracked pid")
	}
	bystander.Process.Kill()
	bystander.Wait()
}

// TestTerminateVMMNoPidfileNoKill: without any identity source nothing may be
// signalled — certainly not a pattern or -1 broadcast.
func TestTerminateVMMNoPidfileNoKill(t *testing.T) {
	dir := t.TempDir()
	survivor := exec.Command("sleep", "302")
	if err := survivor.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		survivor.Process.Kill()
		survivor.Wait()
	}()

	m := newMachine(specForTest("sb_nopidfile001"), jailDirs{base: dir, id: "sb_nopidfile001"}, nil, nil, logging.New("error"))
	m.pidFile = filepath.Join(dir, "missing.pid") // absent
	m.fcBin = "/nonexistent-firecracker"

	if err := m.terminateVMM(); err != nil {
		t.Fatalf("terminateVMM should no-op gracefully: %v", err)
	}
	if !procAlive(survivor.Process.Pid) {
		t.Fatal("unrelated process killed despite missing pidfile")
	}
	survivor.Process.Kill()
	survivor.Wait()
}

// TestFindVMMProcRequiresExactRoot: the /proc fallback must match on BOTH exe
// and chroot root. Ordinary processes share exe names but never the jail root,
// so they must not be returned.
func TestFindVMMProcRequiresExactRoot(t *testing.T) {
	sleep := exec.Command("sleep", "303")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sleep.Process.Kill()
		sleep.Wait()
	}()

	exe, _ := os.Executable() // real exe path of this test binary
	got := findVMMProc(exe, "/definitely/not/a/jail/root")
	if got != 0 {
		t.Fatalf("findVMMProc matched pid %d outside the jail root", got)
	}
	_ = strings.TrimSpace // keep strings import if unused elsewhere
}

// TestVMMpidRejectsGarbage: a corrupt pidfile must disable the kill, not
// signal pid 0/-1/garbage.
func TestVMMpidRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]int{
		"":            0,
		"not-a-pid":   0,
		"-5":          0,
		"1":           0,
		"999999999\n": 999999999,
	}
	for content, want := range cases {
		p := filepath.Join(dir, "pidfile")
		os.WriteFile(p, []byte(content), 0o644)
		m := newMachine(specForTest("sb_pidgarbage01"), jailDirs{base: dir, id: "x"}, nil, nil, logging.New("error"))
		m.pidFile = p
		if got := m.vmmPID(); got != want {
			t.Errorf("vmmPID(%q) = %d, want %d", content, got, want)
		}
	}
}

func specForTest(id string) vmm.Spec { return vmm.Spec{ID: id} }

var _ = context.Background

// withPermissiveProcCheck substitutes the process-identity check for the
// duration of a test. Only for tests whose stand-in VMM is an ordinary process
// that cannot satisfy the real exe/chroot match.
func withPermissiveProcCheck(t *testing.T) {
	t.Helper()
	prev := confirmProc
	confirmProc = func(pid int, _, _ string) bool { return pid > 0 }
	t.Cleanup(func() { confirmProc = prev })
}

// TestTerminateVMMRefusesUnverifiedPID: the jail pidfile is never invalidated,
// so after a kill -- or a Firecracker that died before rewriting it -- the pid
// it names may have been recycled. noded runs as root, so signalling an
// unverified pid could take out another tenant's VMM or a host daemon.
func TestTerminateVMMRefusesUnverifiedPID(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "run"), 0o755)

	bystander := exec.Command("sleep", "303")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bystander.Process.Kill(); bystander.Wait() })

	pidFile := filepath.Join(dir, "firecracker.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(bystander.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newMachine(specForTest("sb_staleupid0001"), jailDirs{base: dir, id: "sb_staleupid0001"}, nil, nil, logging.New("error"))
	m.pidFile = pidFile
	m.fcBin = "/nonexistent-firecracker"

	if err := m.terminateVMM(); err == nil {
		t.Fatal("terminateVMM signalled a pid it could not verify")
	}
	if !procAlive(bystander.Process.Pid) {
		t.Fatal("an unrelated process named by a stale pidfile was killed as root")
	}
}
