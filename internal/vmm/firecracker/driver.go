package firecracker

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// Config parameterizes the driver for one node.
type Config struct {
	FirecrackerBin string
	JailerBin      string
	JailBaseDir    string // /srv/jail
	NetnsDir       string // /var/run/netns
	Log            *logging.Logger

	// CIDAlloc returns a unique vsock guest CID per boot/restore. Uniqueness
	// across the fleet is part of the snapshot-uniqueness protocol (plan §7.4).
	CIDAlloc func() (uint32, error)

	// UFFDSocket, when set, is passed as the mem_backend on restore so cold
	// pages fault in from the chunk cache (§3.2). Empty = file-backed.
	UFFDSocket string

	// UIDAlloc returns the per-sandbox unprivileged uid/gid pair for jailer.
	// Plan §7.2: a VMM escape must land in an unprivileged context, never root.
	UIDAlloc func(sandboxID string) (uid, gid uint32)

	// APISockOverride redirects the client at a test double's socket.
	APISockOverride string
}

type Driver struct {
	cfg  Config
	log  *logging.Logger
	mu   sync.Mutex
	live map[string]*machine

	// dialGuest is overridable for tests (fake VMM + unix-socket guest). It
	// takes the HOST path of the vsock UDS, because that -- not a CID -- is
	// how a host reaches a Firecracker guest.
	dialGuest func(hostUDS string, timeout time.Duration) (*vmm.GuestSession, error)
}

func (d *Driver) SetDialGuest(f func(hostUDS string, timeout time.Duration) (*vmm.GuestSession, error)) {
	d.dialGuest = f
}

// JailDirsForTest exposes the jail layout so tests derive paths the same way
// production does instead of hardcoding a shape that can drift.
func (d *Driver) JailDirsForTest(sandboxID string) jailDirs {
	return jailDirs{base: d.cfg.JailBaseDir, id: sandboxID, execName: filepath.Base(resolveBin(d.cfg.FirecrackerBin))}
}

// UIDForTest exposes the per-sandbox uid allocator.
func (d *Driver) UIDForTest(sandboxID string) (uint32, uint32) {
	return d.cfg.UIDAlloc(sandboxID)
}

func NewDriver(cfg Config) *Driver {
	if cfg.Log == nil {
		cfg.Log = logging.New("info")
	}
	if cfg.CIDAlloc == nil {
		var mu sync.Mutex
		next := uint32(3)
		cfg.CIDAlloc = func() (uint32, error) {
			mu.Lock()
			defer mu.Unlock()
			cid := next
			next += 2
			if next > 0xFFFFFFFE {
				return 0, fmt.Errorf("vsock CID space exhausted")
			}
			return cid, nil
		}
	}
	if cfg.UIDAlloc == nil {
		var mu sync.Mutex
		next := uint32(100000)
		cfg.UIDAlloc = func(sandboxID string) (uid, gid uint32) {
			mu.Lock()
			defer mu.Unlock()
			u := next
			next++
			if next >= 4000000000 {
				next = 100000 // wrap; ids are unique per boot so reuse is safe per-host
			}
			return u, u
		}
	}
	return &Driver{cfg: cfg, log: cfg.Log, live: map[string]*machine{}}
}

func (d *Driver) Name() string { return "firecracker" }

func (d *Driver) Caps() vmm.Caps {
	return vmm.Caps{Tier: "hv", Pause: true, Snapshot: true, LiveFork: false}
}

// jailDirs lays out the jailer chroot per sandbox:
//
//	<jail>/<id>/root       (chroot target; firecracker sees only this)
//	<jail>/<id>/run        api socket lives here
//	<jail>/<id>/fs         drives
//	<jail>/<id>/snapshots  pause images
//
// jailDirs models the on-disk layout of one sandbox's jail.
//
// It previously agreed with neither the jailer nor ITSELF: `--chroot-base-dir`
// was given j.root() while the code treated j.root() as the chroot; fs/, run/
// and snapshots/ were siblings of root() and therefore OUTSIDE the chroot,
// even though the drive was configured as the chroot-absolute "/fs/rootfs.ext4";
// and apiSock() pointed at <id>/run/api.sock while the client was actually
// opened at <id>/root/run/api.sock. Host-side paths and guest-side paths
// referred to different files, so kills, snapshots and the API socket could all
// address the wrong thing.
//
// The model now has exactly one rule: the VMM sees a path P inside its chroot;
// the host sees that same file at root()+P. Everything derives from hostPath.
//
// ASSUMPTION (cannot be verified on a host without a real jailer, see
// docs/KVM-AVAILABILITY.md): the jailer creates and chroots into
//
//	<chroot-base-dir>/<exec-file-basename>/<id>/root
//
// That is the layout documented by the Firecracker jailer. It is encoded in
// exactly one place — root() — so if it is wrong, one line changes.
// TestJailLayoutIsSelfConsistent pins it.
type jailDirs struct{ base, id, execName string }

// tree is everything on disk for this sandbox; Cleanup removes it.
func (j jailDirs) tree() string { return filepath.Join(j.base, j.id) }

// chrootBase is the value passed to the jailer's --chroot-base-dir.
func (j jailDirs) chrootBase() string { return j.tree() }

// root is the jailer's actual chroot on the host.
func (j jailDirs) root() string {
	name := j.execName
	if name == "" {
		name = "firecracker"
	}
	return filepath.Join(j.chrootBase(), name, j.id, "root")
}

// hostPath maps a chroot-absolute path to its host location. This is the ONLY
// way host paths should be derived.
func (j jailDirs) hostPath(inChroot string) string {
	return filepath.Join(j.root(), filepath.FromSlash(inChroot))
}

func (j jailDirs) run() string       { return j.hostPath(fcRunInChroot) }
func (j jailDirs) fs() string        { return j.hostPath(fcFsInChroot) }
func (j jailDirs) snapshots() string { return j.hostPath(fcSnapshotsInChroot) }
func (j jailDirs) apiSock() string   { return j.hostPath(fcAPISockInChroot) }

const bootArgs = "console=ttyS0 reboot=k panic=1 pci=off random.trust_cpu=on vmgenid= quiet"

// fcPidFileInChroot makes the daemonized Firecracker write its pid inside the
// jail. This is the ONLY reliable process identity: the jailer execve's the
// VMM with just the post-`--` args, so no `--id` survives in the live cmdline
// and pattern-based kills can neither find it nor scope it.
// The pidfile sits at the chroot ROOT, not under /run: the jailer creates the
// chroot directory but no subdirectories, so a pidfile under /run would fail to
// open before Firecracker ever starts.
const fcPidFileInChroot = "/firecracker.pid"

// Chroot-absolute locations. Host views come from jailDirs.hostPath.
const (
	fcRunInChroot     = "/run"
	fcFsInChroot      = "/fs"
	fcAPISockInChroot = "/api.sock"
)

// fcSnapshotsInChroot is where pause/restore snapshot files live inside the
// jail; the host view is <jailBase>/<id>/root + fcSnapshotsInChroot.
const fcSnapshotsInChroot = "/snapshots"

// fcVsockUDSInChroot is the Unix socket Firecracker creates for its vsock
// device. Firecracker does NOT expose the guest on the host's AF_VSOCK address
// space: the host end of a Firecracker vsock is this UDS, and a host-initiated
// connection is made by dialling it and sending "CONNECT <port>\n". The device
// was previously configured with no uds_path at all, so Firecracker had no host
// endpoint to create, and the host then tried AF_VSOCK by CID — an address
// nothing was ever listening on.
const fcVsockUDSInChroot = fcRunInChroot + "/vsock.sock"

// guestdVsockPort is the port guestd listens on inside the guest.
const guestdVsockPort = 5000

// prepare builds the jail for a COLD BOOT: directories, VM config, client, and
// a freshly staged rootfs from the template.
//
// Restore must NOT use it. See prepareForRestore.
func (d *Driver) prepare(spec vmm.Spec) (jailDirs, *Client, uint32, error) {
	return d.prepareJail(spec, true, true)
}

// prepareForRestore builds the jail WITHOUT re-staging the rootfs.
//
// Restore used to call prepare, which copies the pristine template image over
// <jail>/<id>/fs/rootfs.ext4 unconditionally — the same file the restored VM is
// wired to. Idle-pause is on by default, so on the hv tier the default
// lifecycle was: user writes a file, sandbox idle-pauses, user returns, and the
// disk is rolled back to the template while the restored guest memory still
// describes the OLD disk. Every byte written since creation is lost, and the
// kernel's page cache, ext4 journal and inode state now refer to blocks that no
// longer exist — silent corruption on top of silent data loss.
//
// The disk must already exist: a missing one means the sandbox's state is gone,
// which is a failure to report, not a template to re-seed.
func (d *Driver) prepareForRestore(spec vmm.Spec) (jailDirs, *Client, error) {
	// No CID is allocated here: a restored microVM brings its vsock device --
	// and the guest CID it was booted with -- back from the snapshot, and the
	// host reaches it over the device's UDS, not by CID. Allocating one anyway
	// burned an address from a never-freed allocator on every resume.
	j, c, _, err := d.prepareJail(spec, false, false)
	if err != nil {
		return j, c, err
	}
	if spec.Template.RootfsPath != "" {
		rootfsInJail := filepath.Join(j.fs(), "rootfs.ext4")
		if _, statErr := os.Stat(rootfsInJail); statErr != nil {
			return j, nil, fmt.Errorf("restore %s: sandbox disk %s is missing (%v); refusing to re-seed it from the template, which would silently discard the sandbox's data", spec.ID, rootfsInJail, statErr)
		}
	}
	return j, c, nil
}

// prepareJail returns the allocated guest CID alongside the jail and client.
//
// It used to write the CID into its own BY-VALUE copy of spec, so the value
// never reached the caller: Create then read spec.VSockCID from the caller's
// struct and got 0 -- VMADDR_CID_HYPERVISOR, a reserved address -- for every
// microVM on the fleet. Either Firecracker rejects it and no sandbox ever boots,
// or it accepts it and the per-boot CID uniqueness the plan's §7.4 uniqueness
// protocol depends on is silently not in force. Returning it removes the
// possibility.
func (d *Driver) prepareJail(spec vmm.Spec, stageRootfs, allocCID bool) (jailDirs, *Client, uint32, error) {
	if !ids.ValidSandboxID(spec.ID) {
		return jailDirs{}, nil, 0, fmt.Errorf("refusing hostile sandbox id %q", spec.ID)
	}
	j := jailDirs{base: d.cfg.JailBaseDir, id: spec.ID, execName: filepath.Base(resolveBin(d.cfg.FirecrackerBin))}
	for _, p := range []string{j.root(), j.run(), j.fs(), j.snapshots()} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			return j, nil, 0, err
		}
	}
	if stageRootfs && spec.Template.RootfsPath != "" {
		rootfsInJail := filepath.Join(j.fs(), "rootfs.ext4")
		if err := copyOrReflink(spec.Template.RootfsPath, rootfsInJail); err != nil {
			return j, nil, 0, fmt.Errorf("stage rootfs: %w", err)
		}
	}

	vmcfg := map[string]any{}
	if spec.Template.KernelPath != "" {
		vmcfg["boot-source"] = BootSource{
			KernelImagePath: kernelInJail(j, spec.Template.KernelPath),
			BootArgs:        bootArgs,
		}
	}
	vmcfg["machine-config"] = MachineConfig{
		MemSizeMib: spec.Template.MemMiB,
		VcpuCount:  spec.Template.VCPUs,
		SMT:        false,
	}
	vmcfg["drives"] = []Drive{{
		DriveID:      "rootfs",
		PathOnHost:   fcFsInChroot + "/rootfs.ext4", // chroot-absolute
		IsReadOnly:   false,
		IsRootDevice: true,
	}}
	b := make([]byte, 6)
	rand.Read(b)
	mac := fmt.Sprintf("06:%02x:%02x:%02x:%02x:%02x", b[0], b[1], b[2], b[3], b[4])
	if spec.Net != nil {
		vmcfg["network-interfaces"] = []NetworkInterface{{
			IfaceID:     "eth0",
			GuestMac:    mac,
			HostDevName: spec.Net.TapName,
		}}
	} else {
		vmcfg["network-interfaces"] = []NetworkInterface{}
	}
	var cid uint32
	if allocCID {
		var err error
		cid, err = d.cfg.CIDAlloc()
		if err != nil {
			return j, nil, 0, err
		}
		vmcfg["vsock"] = Vsock{GuestCID: cid, UDSPath: fcVsockUDSInChroot}
	}

	blob, _ := json.Marshal(vmcfg)
	cfgPath := filepath.Join(j.run(), "vmconfig.json")
	if err := os.WriteFile(cfgPath, blob, 0o644); err != nil {
		return j, nil, 0, err
	}

	sock := j.apiSock()
	if d.cfg.APISockOverride != "" {
		sock = d.cfg.APISockOverride
	}
	cli, err := NewClient(sock)
	if err != nil {
		return j, nil, 0, err
	}
	return j, cli, cid, nil
}

// readPidFile reads a Firecracker pidfile written inside the jail.
func readPidFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 1 {
		return 0
	}
	return n
}

func kernelInJail(j jailDirs, hostKernel string) string {
	// The kernel image must also live inside the chroot; hardlink when possible.
	dst := filepath.Join(j.fs(), "vmlinux")
	_ = os.Remove(dst)
	if err := os.Link(hostKernel, dst); err == nil {
		return fcFsInChroot + "/vmlinux"
	}
	_ = copyOrReflink(hostKernel, dst)
	return fcFsInChroot + "/vmlinux"
}

func copyOrReflink(src, dst string) error {
	if src == "" {
		return fmt.Errorf("empty source path")
	}
	out, err := exec.Command("cp", "--reflink=auto", src, dst).CombinedOutput()
	if err == nil {
		return nil
	}
	_ = out
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	outF, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer outF.Close()
	_, err = ioCopy(outF, in)
	return err
}

// connectGuest dials guestd through Firecracker's vsock UDS, retrying while
// the VM boots.
//
// Protocol (Firecracker vsock, host-initiated): connect to the device's
// uds_path, send "CONNECT <port>\n", and read back "OK <hostport>\n". After
// that the stream is the vsock connection. The previous implementation opened
// an AF_VSOCK socket to the guest CID, which is how you reach a guest under a
// VMM that plumbs vsock into the host kernel — Firecracker does not.
func (d *Driver) connectGuest(hostUDS string, timeout time.Duration) (*vmm.GuestSession, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", hostUDS, 2*time.Second)
		if err != nil {
			lastErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err := vsockConnect(conn, guestdVsockPort, time.Until(deadline)); err != nil {
			conn.Close()
			lastErr = err
			time.Sleep(10 * time.Millisecond)
			continue
		}
		return vmm.NewGuestSession(conn), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out")
	}
	return nil, fmt.Errorf("guestd not reachable via vsock uds %s: %w", hostUDS, lastErr)
}

// vsockConnect performs the CONNECT handshake on an already-dialled UDS.
func vsockConnect(conn net.Conn, port uint32, budget time.Duration) error {
	if budget <= 0 {
		budget = 2 * time.Second
	}
	if err := conn.SetDeadline(time.Now().Add(budget)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return err
	}
	// The reply is a single short line; read it a byte at a time so no guest
	// payload is consumed along with it.
	var line []byte
	buf := make([]byte, 1)
	for len(line) < 64 {
		n, err := conn.Read(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if buf[0] == '\n' {
			break
		}
		line = append(line, buf[0])
	}
	if !strings.HasPrefix(string(line), "OK") {
		return fmt.Errorf("vsock CONNECT refused: %q", string(line))
	}
	// Clear the handshake deadline; the session manages its own timeouts.
	return conn.SetDeadline(time.Time{})
}

// track registers a live machine and arranges removal from the registry when
// it dies. Without the cleanup hook d.live grew one entry (+ Client + vsock
// fd) per sandbox for the life of the process.
func (d *Driver) track(id string, m *machine) {
	d.mu.Lock()
	d.live[id] = m
	d.mu.Unlock()
	go func() {
		<-m.Done()
		d.mu.Lock()
		if cur, ok := d.live[id]; ok && cur == m {
			delete(d.live, id)
		}
		d.mu.Unlock()
	}()
}

// abandonVMM kills a Firecracker that was spawned but never adopted into a
// machine, and returns the causing error unchanged.
//
// Every error path after the jailer spawn used to `return nil, err` and walk
// away. The jailer daemonizes, so the VMM kept running with its RAM, tap, vsock
// CID and fds — invisible to the supervisor, which never saw a Machine. A
// template with a bad kernel path could orphan one VMM per retry, every 10s.
func (d *Driver) abandonVMM(j jailDirs, cause error) error {
	pid := readPidFile(j.hostPath(fcPidFileInChroot))
	if pid <= 0 {
		pid = findVMMProc(resolveBin(d.cfg.FirecrackerBin), j.root())
	}
	switch {
	case pid > 0 && confirmProc(pid, resolveBin(d.cfg.FirecrackerBin), j.root()):
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			d.log.Error("could not kill orphaned vmm", "pid", pid, "err", err.Error())
		} else {
			d.log.Warn("killed vmm orphaned by a failed start", "pid", pid, "cause", cause.Error())
		}
	case pid > 0:
		// The pidfile is never invalidated, so after a kill -- or a Firecracker
		// that died before rewriting it -- the pid it names may have been
		// recycled by an unrelated process. We run as root: signalling an
		// unverified pid could take out another tenant's VMM or a host daemon,
		// which is far worse than leaving one orphan behind.
		d.log.Warn("stale vmm pidfile; refusing to kill an unverified pid",
			"pid", pid, "sandbox", j.id, "cause", cause.Error())
	}
	// Drop the pidfile either way, so the next attempt cannot inherit it.
	_ = os.Remove(j.hostPath(fcPidFileInChroot))
	return cause
}

// confirmProc is the process-identity check. It is a package var purely so
// tests can substitute a stand-in process they can actually describe --
// production always uses confirmVMMProc, and weakening it in production would
// reintroduce the root-kills-a-recycled-pid hazard it exists to close.
var confirmProc = confirmVMMProc

// confirmVMMProc verifies that pid really is THIS sandbox's Firecracker before
// anything signals it: the executable must match and the process's root must be
// this jail. A pidfile on its own is not proof of identity.
func confirmVMMProc(pid int, firecrackerBin, jailRoot string) bool {
	exe, err := os.Readlink(filepath.Join(procPseudoPrefix, strconv.Itoa(pid), "exe"))
	if err != nil {
		return false // cannot verify: do not kill
	}
	if exe != firecrackerBin && filepath.Base(exe) != filepath.Base(firecrackerBin) {
		return false
	}
	root, err := os.Readlink(filepath.Join(procPseudoPrefix, strconv.Itoa(pid), "root"))
	if err != nil {
		return false
	}
	return root == jailRoot
}

// Cleanup removes the entire per-sandbox jail tree. See vmm.Cleaner.
func (d *Driver) Cleanup(ctx context.Context, sandboxID string) error {
	if !ids.ValidSandboxID(sandboxID) {
		return fmt.Errorf("refusing hostile sandbox id %q", sandboxID)
	}
	return os.RemoveAll(filepath.Join(d.cfg.JailBaseDir, sandboxID))
}

func (d *Driver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	j, cli, cid, err := d.prepare(spec)
	if err != nil {
		return nil, err
	}

	nsPath := ""
	if spec.Net != nil {
		nsPath = filepath.Join(d.cfg.NetnsDir, spec.Net.NSName)
	}
	uid, gid := d.cfg.UIDAlloc(spec.ID)

	args := []string{
		"--id", spec.ID,
		"--node", "0",
		"--exec-file", resolveBin(d.cfg.FirecrackerBin),
		"--uid", strconvFormat(uid), "--gid", strconvFormat(gid),
		"--chroot-base-dir", j.chrootBase(),
		"--daemonize",
	}
	if nsPath != "" {
		args = append(args, "--netns", nsPath)
	}
	args = append(args, "--",
		"--api-sock", fcAPISockInChroot,
		"--log-path", "/fc.log",
		"--pid-file", fcPidFileInChroot,
	)

	cmd := exec.CommandContext(ctx, resolveBin(d.cfg.JailerBin), args...)
	cmd.Env = cleanEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("jailer start: %v: %s", err, truncateLog(out))
	}

	realCli := cli
	if d.cfg.APISockOverride == "" {
		var err error
		realCli, err = NewClient(j.apiSock())
		if err != nil {
			return nil, d.abandonVMM(j, err)
		}
		if err := realCli.WaitForSocket(10 * time.Second); err != nil {
			return nil, d.abandonVMM(j, err)
		}
	}

	ctx2, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	kernel := kernelInJail(j, spec.Template.KernelPath)
	if err := realCli.PutBootSource(ctx2, BootSource{
		KernelImagePath: kernel,
		BootArgs:        bootArgs,
	}); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("boot-source: %w", err))
	}
	if err := realCli.PutMachineConfig(ctx2, MachineConfig{
		MemSizeMib: spec.Template.MemMiB,
		VcpuCount:  spec.Template.VCPUs,
		SMT:        false,
	}); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("machine-config: %w", err))
	}
	if err := realCli.PutDrive(ctx2, Drive{
		DriveID:      "rootfs",
		PathOnHost:   fcFsInChroot + "/rootfs.ext4",
		IsReadOnly:   false,
		IsRootDevice: true,
	}); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("drive: %w", err))
	}
	if spec.Net != nil {
		if err := realCli.PutNetworkInterface(ctx2, NetworkInterface{
			IfaceID:     "eth0",
			GuestMac:    "02:00:00:00:00:01",
			HostDevName: spec.Net.TapName,
		}); err != nil {
			return nil, d.abandonVMM(j, fmt.Errorf("netif: %w", err))
		}
	}
	// The CID comes back from prepare, not from spec: spec is passed by value,
	// so prepare's assignment to it never reached this function and every VM
	// was configured with the reserved CID 0.
	if cid == 0 {
		return nil, d.abandonVMM(j, fmt.Errorf("vsock: no guest CID was allocated (0 is reserved)"))
	}
	if err := realCli.PutVsock(ctx2, Vsock{GuestCID: cid, UDSPath: fcVsockUDSInChroot}); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("vsock: %w", err))
	}
	if err := realCli.StartInstance(ctx2); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("InstanceStart: %w", err))
	}

	sess, err := d.dial(j.hostPath(fcVsockUDSInChroot), 15*time.Second)
	if err != nil {
		return nil, d.abandonVMM(j, err)
	}
	if _, err := sess.Hello(15 * time.Second); err != nil {
		return nil, d.abandonVMM(j, err)
	}

	m := newMachine(spec, j, realCli, sess, d.log)
	m.fcBin = resolveBin(d.cfg.FirecrackerBin)
	m.pidFile = j.hostPath(fcPidFileInChroot)
	go m.supervise()
	d.track(spec.ID, m)
	return m, nil
}

func (d *Driver) Restore(ctx context.Context, spec vmm.Spec, memFile, snapFile string) (vmm.Machine, error) {
	j, _, err := d.prepareForRestore(spec)
	if err != nil {
		return nil, err
	}
	memPath := memFile
	if memPath == "" {
		memPath = filepath.Join(j.snapshots(), "mem")
	}
	snapPath := snapFile
	if snapPath == "" {
		snapPath = filepath.Join(j.snapshots(), "snap")
	}

	nsPath := ""
	if spec.Net != nil {
		nsPath = filepath.Join(d.cfg.NetnsDir, spec.Net.NSName)
	}
	uid, gid := d.cfg.UIDAlloc(spec.ID)

	args := []string{
		"--id", spec.ID,
		"--node", "0",
		"--exec-file", resolveBin(d.cfg.FirecrackerBin),
		"--uid", strconvFormat(uid), "--gid", strconvFormat(gid),
		"--chroot-base-dir", j.chrootBase(),
		"--daemonize",
	}
	if nsPath != "" {
		args = append(args, "--netns", nsPath)
	}
	args = append(args, "--", "--api-sock", fcAPISockInChroot, "--pid-file", fcPidFileInChroot)

	if out, err := exec.Command(resolveBin(d.cfg.JailerBin), args...).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("jailer restore-start: %v: %s", err, truncateLog(out))
	}

	realCli, sockErr := NewClient(j.apiSock())
	if sockErr != nil {
		return nil, d.abandonVMM(j, sockErr)
	}
	if d.cfg.APISockOverride != "" {
		realCli, _ = NewClient(d.cfg.APISockOverride)
	} else if err := realCli.WaitForSocket(10 * time.Second); err != nil {
		return nil, d.abandonVMM(j, err)
	}

	// Snapshot paths arrive as HOST paths (the ref Pause returned); the
	// chrooted VMM needs the jail-relative form. Paths outside this jail are
	// refused rather than mangled.
	chrootRel := func(hostPath string) (string, error) {
		rel, err := filepath.Rel(j.root(), hostPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("snapshot path %q outside jail root", hostPath)
		}
		return "/" + filepath.ToSlash(rel), nil
	}
	memRel, err := chrootRel(memPath)
	if err != nil {
		return nil, d.abandonVMM(j, err)
	}
	snapRel, err := chrootRel(snapPath)
	if err != nil {
		return nil, d.abandonVMM(j, err)
	}
	load := SnapshotLoad{
		SnapshotPath: snapRel,
		MemBackend:   MemBackend{Type: "File", Path: memRel},
	}
	if d.cfg.UFFDSocket != "" {
		load.MemBackend = MemBackend{Type: "Uffd", Path: d.cfg.UFFDSocket}
	}
	if err := realCli.LoadSnapshot(ctx, load); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("snapshot/load: %w", err))
	}
	// The vsock device is RESTORED FROM THE SNAPSHOT, together with the guest
	// CID it had when the snapshot was taken. Re-PUTting it here was wrong
	// twice over: Firecracker rejects device configuration once the microVM has
	// started (and a loaded snapshot counts as started), and the freshly
	// allocated CID would not have matched the one the restored guest is
	// listening on, so the host could never have reached guestd afterwards.
	if err := realCli.PatchVM(ctx, VMState{State: "Resumed"}); err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("resume: %w", err))
	}

	// The restored VM brings its vsock device -- and its UDS path -- back from
	// the snapshot, so this is the same socket it had before pausing.
	sess, err := d.dial(j.hostPath(fcVsockUDSInChroot), 10*time.Second)
	if err != nil {
		return nil, d.abandonVMM(j, err)
	}
	vmgenid := newVMGenID()
	ack, err := sess.Reseed(vmgenid, 10*time.Second)
	if err != nil {
		return nil, d.abandonVMM(j, fmt.Errorf("uniqueness protocol failed: %w", err))
	}
	d.log.Info("restore reseeded", "sandbox", spec.ID, "machine_id", ack.MachineID, "vmgenid", vmgenid)

	m := newMachine(spec, j, realCli, sess, d.log)
	m.fcBin = resolveBin(d.cfg.FirecrackerBin)
	m.pidFile = j.hostPath(fcPidFileInChroot)
	go m.supervise()
	d.track(spec.ID, m)
	return m, nil
}

func (d *Driver) dial(hostUDS string, timeout time.Duration) (*vmm.GuestSession, error) {
	if d.dialGuest != nil {
		return d.dialGuest(hostUDS, timeout)
	}
	return d.connectGuest(hostUDS, timeout)
}

func newVMGenID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var (
	_ vmm.Driver = (*Driver)(nil)
	_ net.Conn   // keep imports honest in refactors
)

func resolveBin(p string) string {
	if lp, err := exec.LookPath(p); err == nil {
		return lp
	}
	return p
}

func cleanEnv() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin"}
}

func truncateLog(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}

func strconvFormat(u uint32) string {
	return fmt.Sprintf("%d", u)
}
