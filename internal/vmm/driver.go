// Package vmm holds the VMDriver abstraction (plan §2.2): noded talks to VMMs
// through this narrow interface; Firecracker is driver #1, the process driver
// (#0) exists so the whole stack runs on KVM-less dev boxes and CI.
package vmm

import (
	"context"
	"errors"
	"io"
	"time"
)

var (
	ErrUnsupported    = errors.New("vmm: capability not supported by driver")
	ErrMachineGone    = errors.New("vmm: machine exited")
	ErrSnapshotLocked = errors.New("vmm: lineage locked")
)

// Caps reports what a driver supports; the scheduler refuses placements a
// driver cannot honor rather than silently downgrading tiers (plan §2.4).
type Caps struct {
	Tier     string // hv | kernel | process
	Pause    bool
	Snapshot bool
	LiveFork bool
}

type TemplateSpec struct {
	Name       string `json:"name"`
	RootfsPath string `json:"rootfs_path"`
	KernelPath string `json:"kernel_path,omitempty"`
	MemMiB     int    `json:"mem_mib"`
	VCPUs      int    `json:"vcpus"`
	WarmCmd    string `json:"warm_cmd,omitempty"`
}

// NetInfo carries the sandbox network coordinates programmed by the netns
// manager. Kept as a plain struct so vmm stays dependency-free.
type NetInfo struct {
	NSName    string
	NSPath    string // /var/run/netns/<name>, for drivers that JOIN the namespace
	TapName   string
	GuestIP   string
	GatewayIP string
}

type Spec struct {
	ID       string
	Template TemplateSpec
	Net      *NetInfo
	VSockCID uint32 // unique per boot/restore; part of the uniqueness protocol
	WorkDir  string // per-sandbox scratch (jail dir / process workdir)
}

// ExecSpec describes one process launch inside the guest.
type ExecSpec struct {
	Argv []string
	Cwd  string
	Env  map[string]string
	TTY  bool
	Rows uint16
	Cols uint16
}

type ExitInfo struct {
	Code   int
	Killed bool
}

// ExecHandle streams output of a running exec.
type ExecHandle struct {
	ID       string
	Stdout   io.ReadCloser
	ExitCh   <-chan ExitInfo
	signalFn func(string) error
	resizeFn func(uint16, uint16) error
	stdinFn  func([]byte) error
	cancel   context.CancelFunc
}

func (h *ExecHandle) Wait(ctx context.Context) (ExitInfo, error) {
	select {
	case ei := <-h.ExitCh:
		return ei, nil
	case <-ctx.Done():
		h.Signal("KILL")
		ei := <-h.ExitCh
		return ei, ctx.Err()
	}
}

// Signal sends a POSIX signal name ("TERM", "KILL", ...) to the exec.
func (h *ExecHandle) Signal(sig string) error { return h.signalFn(sig) }

// WriteStdin feeds the exec's stdin; nil closes it.
func (h *ExecHandle) WriteStdin(b []byte) error {
	if h.stdinFn == nil {
		return ErrUnsupported
	}
	return h.stdinFn(b)
}

// Resize resizes the pty of a tty exec.
func (h *ExecHandle) Resize(rows, cols uint16) error { return h.resizeFn(rows, cols) }

// Machine is a live sandbox compute instance.
type Machine interface {
	ID() string
	Caps() Caps

	// Exec starts a process; returns immediately with a streaming handle.
	Exec(ctx context.Context, spec ExecSpec) (*ExecHandle, error)
	// Reattach is unsupported in v1 (execs die with connections).

	Fs() *FsClient
	Ports() []PortInfo

	// Reseed runs the post-restore uniqueness protocol (plan §7.4): the guest
	// regenerates machine-id/host-keys/random-seed and acks.
	Reseed(ctx context.Context, vmgenid string) (ReseedAck, error)

	Metrics(ctx context.Context) (*GuestMetrics, error)

	// Pause snapshots memory+disk state to destDir and stops the VMM.
	Pause(ctx context.Context, destDir string) (string, error)
	Shutdown(ctx context.Context) error
	Kill() error
	Done() <-chan struct{} // closed when the VMM process exits unexpectedly
}

type PortInfo struct {
	Port  int    `json:"port"`
	Proto string `json:"proto"`
}

type ReseedAck struct {
	MachineID      string `json:"machine_id"`
	SSHFingerprint string `json:"ssh_fingerprint"`
}

type GuestMetrics struct {
	MemTotalKB uint64 `json:"mem_total_kb"`
	MemAvailKB uint64 `json:"mem_avail_kb"`
	Load1      uint32 `json:"load1"`
	PIDs       uint32 `json:"pids"`
	UptimeS    uint64 `json:"uptime_s"`
}

// Driver constructs machines. One driver per isolation tier per node.
type Driver interface {
	Name() string
	Caps() Caps
	Create(ctx context.Context, spec Spec) (Machine, error)
	Restore(ctx context.Context, spec Spec, memFile, snapFile string) (Machine, error)
}

// Cleaner is an OPTIONAL driver capability: remove whatever the driver keeps
// on disk for a sandbox. Destroy removed the supervisor's own workdir but
// nothing driver-private, so the Firecracker jail tree — a per-sandbox rootfs
// copy plus a full-RAM memfile — survived every destroy and filled the disk.
//
// Implemented as an optional interface so drivers with nothing to clean (and
// test doubles) need no change.
type Cleaner interface {
	Cleanup(ctx context.Context, sandboxID string) error
}

// WaitFor polls until fn returns true or the deadline passes; the create/resume
// hot paths use it to bound guestd handshake latency.
func WaitFor(deadline time.Duration, interval time.Duration, fn func() bool) bool {
	t := time.NewTimer(deadline)
	defer t.Stop()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if fn() {
			return true
		}
		select {
		case <-t.C:
			return false
		case <-tick.C:
		}
	}
}
