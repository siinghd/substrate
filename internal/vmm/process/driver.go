// Package process implements the dev/test isolation tier: sandbox processes
// supervised by the substrate-shim agent over a unix socket speaking SGP/1.
// It exists so the entire platform (API, exec, fs, ports, reconciler, gateway)
// runs end-to-end on KVM-less machines and in CI. It provides NO security
// boundary and is refused unless explicitly enabled (config driver.allow_process).
package process

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

type Driver struct {
	shimBin string
	logf    func(string, ...any)

	mu   sync.Mutex
	live map[string]*machine
}

func NewDriver(shimBin string) *Driver {
	return &Driver{shimBin: shimBin, logf: func(string, ...any) {}, live: map[string]*machine{}}
}

func (d *Driver) SetLogger(f func(string, ...any)) { d.logf = f }

var _ vmm.Driver = (*Driver)(nil)

func (d *Driver) Name() string { return "process" }
func (d *Driver) Caps() vmm.Caps {
	return vmm.Caps{Tier: "process", Pause: false, Snapshot: false, LiveFork: false}
}

func (d *Driver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	if spec.WorkDir == "" {
		spec.WorkDir = filepath.Join(os.TempDir(), "substrate-"+spec.ID)
	}
	for _, sub := range []string{"", "etc", "root", "tmp", "run"} {
		if err := os.MkdirAll(filepath.Join(spec.WorkDir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	if src := spec.Template.RootfsPath; src != "" && dirExists(src) {
		if err := seedWorkdir(spec.WorkDir, src); err != nil {
			return nil, fmt.Errorf("seed workdir: %w", err)
		}
	}

	sock := filepath.Join(spec.WorkDir, "agent.sock")
	cmd := exec.Command(d.shimBin, "--unix", sock, "--root", spec.WorkDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if os.Getenv("SUBSTRATE_DEBUG_SSE") == "1" {
		cmd.Stderr = os.Stderr
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start shim: %w", err)
	}

	sess, err := dialSession(sock, 10*time.Second)
	if err != nil {
		cmd.Process.Kill()
		return nil, err
	}
	hello, err := sess.Hello(5 * time.Second)
	if err != nil {
		sess.Close()
		cmd.Process.Kill()
		return nil, err
	}
	d.logf("shim ready", "sandbox", spec.ID, "agent", hello.Agent)

	m := &machine{
		id: spec.ID, spec: spec, sess: sess, shim: cmd,
		done: make(chan struct{}),
	}
	go func() {
		cmd.Wait()
		close(m.done)
	}()
	d.mu.Lock()
	d.live[spec.ID] = m
	d.mu.Unlock()
	return m, nil
}

func (d *Driver) Restore(ctx context.Context, spec vmm.Spec, memFile, snapFile string) (vmm.Machine, error) {
	return nil, vmm.ErrUnsupported
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func seedWorkdir(workdir, templateRootfs string) error {
	out, err := exec.Command("cp", "-a", filepath.Clean(templateRootfs)+"/.", workdir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}

func dialSession(sock string, timeout time.Duration) (*vmm.GuestSession, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", sock)
		if err == nil {
			return vmm.NewGuestSession(conn), nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return nil, fmt.Errorf("shim socket %s never appeared", sock)
}

type machine struct {
	id   string
	spec vmm.Spec
	sess *vmm.GuestSession
	shim *exec.Cmd
	done chan struct{}

	killOnce sync.Once
}

var _ vmm.Machine = (*machine)(nil)

func (m *machine) ID() string { return m.id }
func (m *machine) Caps() vmm.Caps {
	return vmm.Caps{Tier: "process", Pause: false, Snapshot: false}
}

// VMMpid reports the shim host PID for usage sampling (procfs accounting).
func (m *machine) VMMpid() (int, bool) {
	if m.shim != nil && m.shim.Process != nil {
		return m.shim.Process.Pid, true
	}
	return 0, false
}

func (m *machine) Exec(ctx context.Context, spec vmm.ExecSpec) (*vmm.ExecHandle, error) {
	return m.sess.Exec(ctx, spec)
}

func (m *machine) Fs() *vmm.FsClient     { return m.sess.Fs() }
func (m *machine) Ports() []vmm.PortInfo { p, _ := m.sess.WaitPorts(0); return p }

func (m *machine) Reseed(ctx context.Context, vmgenid string) (vmm.ReseedAck, error) {
	return m.sess.Reseed(vmgenid, 5*time.Second)
}

func (m *machine) Metrics(ctx context.Context) (*vmm.GuestMetrics, error) {
	gm, ok := m.sess.LatestMetrics()
	if !ok {
		return nil, fmt.Errorf("no metrics yet")
	}
	return gm, nil
}

func (m *machine) Pause(ctx context.Context, destDir string) (string, error) {
	return "", vmm.ErrUnsupported
}

func (m *machine) Shutdown(ctx context.Context) error { return m.Kill() }

func (m *machine) Kill() error {
	m.killOnce.Do(func() {
		m.sess.Close()
		if m.shim.Process != nil {
			syscall.Kill(-m.shim.Process.Pid, syscall.SIGKILL)
		}
	})
	return nil
}

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) WorkDir() string { return m.spec.WorkDir }
