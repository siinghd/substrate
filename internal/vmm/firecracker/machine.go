package firecracker

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/vmm"
)

func ioCopy(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

type machine struct {
	spec vmm.Spec
	j    jailDirs
	cli  *Client
	sess *vmm.GuestSession
	log  *logging.Logger

	// fcBin is the resolved Firecracker exec-file (proc fallback matching);
	// pidFile is the host path of the pidfile Firecracker writes inside its
	// jail. Both are set at spawn and immutable afterwards.
	fcBin   string
	pidFile string

	mu       sync.Mutex
	done     chan struct{}
	doneOnce sync.Once
	killed   bool
}

func newMachine(spec vmm.Spec, j jailDirs, cli *Client, sess *vmm.GuestSession, log *logging.Logger) *machine {
	return &machine{
		spec: spec, j: j, cli: cli, sess: sess, log: log,
		done: make(chan struct{}),
	}
}

// vmmPollInterval is how often the supervisor checks that the VMM is still
// alive. A var so tests need not wait it out.
var vmmPollInterval = 2 * time.Second

// supervise closes Done() when the VMM process disappears without us killing
// it.
//
// Done() previously closed ONLY inside Kill, so an unexpectedly dead microVM —
// a Firecracker crash, an OOM kill, an operator's stray SIGKILL — was pinned
// "live" forever: the reconciler's watch goroutine never fired, the supervisor's
// machine map kept the entry, and because Get() still reported it live, nothing
// ever restarted the sandbox. The sandbox was gone and the platform did not
// know.
//
// Started only once a pid has actually been observed; with no pidfile we cannot
// distinguish "dead" from "never seen", and guessing would kill healthy
// machines.
func (m *machine) supervise() {
	pid := m.vmmPID()
	if pid <= 0 {
		// Give the daemonized VMM a moment to write its pidfile.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) && pid <= 0 {
			time.Sleep(50 * time.Millisecond)
			select {
			case <-m.done:
				return
			default:
			}
			pid = m.vmmPID()
		}
	}
	if pid <= 0 {
		m.log.Warn("no vmm pidfile; cannot supervise liveness", "sandbox", m.spec.ID)
		return
	}
	t := time.NewTicker(vmmPollInterval)
	defer t.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-t.C:
		}
		if processAlive(pid) {
			continue
		}
		m.log.Warn("vmm exited unexpectedly", "sandbox", m.spec.ID, "pid", pid)
		m.markDead()
		return
	}
}

// markDead records an exit we did not cause: tear down the guest session and
// close Done so the reconciler can converge, but do not try to kill a process
// that is already gone.
func (m *machine) markDead() {
	m.mu.Lock()
	m.killed = true
	m.mu.Unlock()
	m.sess.Close()
	m.doneOnce.Do(func() { close(m.done) })
}

// processAlive reports whether pid still exists. signal 0 performs the
// permission and existence checks without delivering anything.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Cleanup removes this sandbox's entire jail tree.
func (m *machine) Cleanup() error {
	return os.RemoveAll(m.j.tree())
}

var _ vmm.Machine = (*machine)(nil)

func (m *machine) ID() string { return m.spec.ID }
func (m *machine) Caps() vmm.Caps {
	return vmm.Caps{Tier: "hv", Pause: true, Snapshot: true}
}

func (m *machine) Exec(ctx context.Context, spec vmm.ExecSpec) (*vmm.ExecHandle, error) {
	return m.sess.Exec(ctx, spec)
}

func (m *machine) Fs() *vmm.FsClient     { return m.sess.Fs() }
func (m *machine) Ports() []vmm.PortInfo { p, _ := m.sess.WaitPorts(0); return p }

func (m *machine) Reseed(ctx context.Context, vmgenid string) (vmm.ReseedAck, error) {
	return m.sess.Reseed(vmgenid, 10*time.Second)
}

func (m *machine) Metrics(ctx context.Context) (*vmm.GuestMetrics, error) {
	gm, ok := m.sess.LatestMetrics()
	if !ok {
		return nil, fmt.Errorf("no guest metrics yet")
	}
	return gm, nil
}

// Pause takes a full snapshot into the jail's snapshots dir and stops the VMM.
// The caller chunkifies the files into the snapstore afterwards.
//
// Snapshots physically live INSIDE the jail chroot (the jailed VMM can only
// touch paths under its root); the returned ref carries the HOST view of
// those files so the chunkifier can read them and Restore can hand them
// back. destDir is accepted for interface compatibility but the jail layout
// is the driver's private knowledge. The previous version passed host paths
// verbatim to the chrooted VMM — files it literally could not see — so
// pause silently failed in jailed production mode.
func (m *machine) Pause(ctx context.Context, destDir string) (string, error) {
	_ = destDir
	if err := os.MkdirAll(m.j.snapshots(), 0o755); err != nil {
		return "", err
	}
	// Firecracker REFUSES to snapshot a running microVM: the guest has to be
	// paused first, or /snapshot/create returns 400. This step was missing, so
	// pause could never have worked against a real VMM — and because the fake
	// answered 204 to everything, no test noticed. Idle-pause and
	// pressure-pause both route through here, so the entire scale-to-zero
	// story depended on a call that always failed.
	if err := m.cli.PatchVM(ctx, VMState{State: "Paused"}); err != nil {
		return "", fmt.Errorf("pause vm: %w", err)
	}
	if err := m.cli.CreateSnapshot(ctx, SnapshotCreate{
		MemFilePath:  fcSnapshotsInChroot + "/memfile",
		SnapshotPath: fcSnapshotsInChroot + "/snapshot",
		Type:         "Full",
	}); err != nil {
		// Leave the guest running rather than frozen: a failed pause must not
		// cost the tenant a working sandbox.
		if rerr := m.cli.PatchVM(ctx, VMState{State: "Resumed"}); rerr != nil {
			m.log.Warn("could not resume after a failed snapshot; guest left paused",
				"sandbox", m.spec.ID, "err", rerr.Error())
		}
		return "", fmt.Errorf("snapshot create: %w", err)
	}
	if err := m.Kill(); err != nil {
		return "", err
	}
	mem := filepath.Join(m.j.snapshots(), "memfile")
	snap := filepath.Join(m.j.snapshots(), "snapshot")
	return mem + "|" + snap, nil
}

func (m *machine) Shutdown(ctx context.Context) error {
	h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"poweroff", "-f"}})
	if err == nil {
		select {
		case <-h.ExitCh:
		case <-time.After(5 * time.Second):
		}
	}
	return m.Kill()
}

func (m *machine) Kill() error {
	m.mu.Lock()
	if m.killed {
		m.mu.Unlock()
		return nil
	}
	m.killed = true
	m.mu.Unlock()

	m.sess.Close()
	_ = m.terminateVMM()
	m.doneOnce.Do(func() { close(m.done) })
	return nil
}

// terminateVMM stops the exact VMM process for THIS sandbox.
//
// The previous implementation ran `pkill -f --id <id>` — which can never
// match (the jailer execve's Firecracker with only the post-`--` args, so no
// `--id` remains in the live cmdline) — and then, on any match, executed
// syscall.Kill(-1, SIGKILL): a signal to EVERY process the agent may reach.
// On a root node that massacre kills all other sandboxes' VMMs plus host
// daemons. Both halves are gone: the VMM pid is captured at spawn from the
// Firecracker pidfile inside the jail and killed by exact pid.
func (m *machine) terminateVMM() error {
	pid := m.vmmPID()
	if pid <= 0 {
		// No pidfile (crashed VMM, ancient Firecracker): fall back to a scan
		// keyed on exe path AND jail root — still an exact-match kill, never
		// a pattern kill.
		m.log.Warn("vmm pidfile missing; scanning proc", "sandbox", m.spec.ID)
		pid = findVMMProc(resolveBin(m.specFirecrackerBin()), m.j.root())
	}
	if pid <= 0 {
		// Neither the pidfile nor the /proc scan (exe + chroot match) found a
		// process for this jail, which means it is genuinely gone. Returning an
		// error here would fail every legitimate pause of an already-exited
		// VMM, so this stays a no-op -- but it is logged, because "we could not
		// find it" and "it is not running" are only the same thing if the scan
		// is reliable.
		m.log.Warn("no VMM process found for this jail; assuming it already exited",
			"sandbox", m.spec.ID, "jail", m.j.root())
		return nil
	}
	if !confirmProc(pid, resolveBin(m.specFirecrackerBin()), m.j.root()) {
		// Never signal an unverified pid as root: the pidfile is not proof of
		// identity, and a recycled pid could be another tenant's VMM.
		m.log.Error("stale vmm pidfile; refusing to kill an unverified pid",
			"sandbox", m.spec.ID, "pid", pid)
		return fmt.Errorf("vmm pid %d for %s could not be verified", pid, m.spec.ID)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("kill vmm pid %d: %w", pid, err)
	}
	return nil
}

// specFirecrackerBin returns the configured exec-file for proc matching.
func (m *machine) specFirecrackerBin() string { return m.fcBin }

// vmmPID reads the daemonized Firecracker pid written into the jail at boot.
func (m *machine) vmmPID() int {
	b, err := os.ReadFile(m.pidFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 1 {
		return 0
	}
	return n
}

const procPseudoPrefix = "/proc/"

// findVMMProc locates the one process whose resolved exe is firecrackerBin and
// whose chroot root is jailRoot. O(running processes) per call, called only
// when the pidfile is missing. Returns 0 when nothing matches.
func findVMMProc(firecrackerBin, jailRoot string) int {
	entries, err := os.ReadDir(procPseudoPrefix[:len(procPseudoPrefix)-1])
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		exe, err := os.Readlink(filepath.Join(procPseudoPrefix, e.Name(), "exe"))
		if err != nil || exe != firecrackerBin {
			continue
		}
		root, err := os.Readlink(filepath.Join(procPseudoPrefix, e.Name(), "root"))
		if err == nil && root == jailRoot {
			return pid
		}
	}
	return 0
}

func (m *machine) Done() <-chan struct{} { return m.done }
