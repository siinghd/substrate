// Package gvisor implements the tier-`kernel` isolation driver (plan §2.2,
// Phase 3): sandboxes run inside a gVisor sandbox (runsc), which interposes a
// userspace kernel between guest code and the host.
//
// It exists because a large class of cheap servers has no /dev/kvm — every
// nested VPS, most shared cloud instances — and on those the only other option
// was the `process` tier, which is not an isolation boundary at all: code there
// runs as the node agent's own uid, so on a box where that user has sudo it is
// one command from host root. gVisor's systrap platform needs no KVM, so this
// tier makes those machines usable for untrusted code.
//
// It is a WEAKER boundary than tier-`hv` and is labelled as such: gVisor is a
// userspace kernel, so the host kernel remains reachable through a filtered
// syscall surface. The tier is never substituted silently — a caller asking for
// `hv` on a node running this driver is refused (see handleCreateSandbox).
//
// The guest agent is the same `substrate-shim` speaking the same SGP/1 protocol
// as every other tier; only the containment around it differs. The shim is
// copied into the sandbox rootfs and reached over a unix socket bind-mounted
// out to the host, which keeps the protocol, exec, fs and ports paths identical
// across tiers.
package gvisor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/proto"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// Config wires the driver to its binaries and state.
type Config struct {
	// RunscBin is the gVisor runtime. Defaults to "runsc" on PATH.
	RunscBin string
	// ShimBin is the static substrate-shim, bind-mounted read-only into each
	// sandbox. It is NOT copied: see the mount in writeOCIConfig.
	ShimBin string
	// TemplateDir holds node-prepared, SHARED, read-only template rootfs
	// trees. One per distinct template, not one per sandbox.
	TemplateDir string
	// OverlaySizeMiB caps how much a single sandbox may write. Guest writes go
	// to a private overlay filestore, never to the template.
	OverlaySizeMiB int
	// StateDir holds runsc's own container state (its --root).
	StateDir string
	// BundleDir is where per-sandbox OCI bundles are built.
	BundleDir string
	// SockDir holds the per-sandbox agent sockets. It is SEPARATE from
	// BundleDir and deliberately short: a unix socket path cannot exceed
	// ~108 bytes, and a bundle path under a deep data_dir blows that limit
	// with a bare "bind: invalid argument" that names nothing useful.
	SockDir string
	// Platform is gVisor's platform; systrap needs no KVM and is the reason
	// this tier works on the machines it is for.
	Platform string
	// Network is passed to runsc. "none" until the netns layer is wired in;
	// see internal/netns, which is correct but not yet in the data path.
	Network string
	Log     func(string, ...any)
}

type Driver struct {
	cfg Config

	mu   sync.Mutex
	live map[string]*machine

	// tmplMu guards the prepared-template cache. Preparation happens once per
	// template per node, so a single mutex is the right trade: it can briefly
	// serialize the FIRST create of two different templates, and in exchange
	// there is no per-key lock table to get wrong.
	tmplMu sync.Mutex
	tmpl   map[string]string
}

func NewDriver(cfg Config) *Driver {
	if cfg.RunscBin == "" {
		cfg.RunscBin = "runsc"
	}
	if cfg.Platform == "" {
		cfg.Platform = "systrap"
	}
	if cfg.Network == "" {
		cfg.Network = "none"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/run/substrate-gvisor"
	}
	if cfg.BundleDir == "" {
		cfg.BundleDir = "/var/lib/substrate/gvisor"
	}
	if cfg.SockDir == "" {
		cfg.SockDir = "/run/substrate-gvisor/sock"
	}
	if cfg.TemplateDir == "" {
		cfg.TemplateDir = "/var/lib/substrate/gvisor/templates"
	}
	if cfg.OverlaySizeMiB <= 0 {
		// A default that is generous for real work and still bounded. Without
		// ANY ceiling one guest filled the node's data volume for every other
		// tenant on it, and `df` inside the sandbox reported the host's free
		// space, so the guest could see exactly how much there was to take.
		cfg.OverlaySizeMiB = 2048
	}
	if cfg.Log == nil {
		cfg.Log = func(string, ...any) {}
	}
	return &Driver{cfg: cfg, live: map[string]*machine{}, tmpl: map[string]string{}}
}

var _ vmm.Driver = (*Driver)(nil)

// tierName labels every metric this driver emits.
const tierName = "kernel"

func (d *Driver) Name() string { return "gvisor" }

// Caps reports Pause=false deliberately. runsc HAS checkpoint/restore, but it
// is not wired here yet, and claiming a pause capability the reconciler would
// then call is worse than reporting the truth: idle-pause would fail on every
// sandbox on the node.
func (d *Driver) Caps() vmm.Caps {
	return vmm.Caps{Tier: "kernel", Pause: false, Snapshot: false, LiveFork: false}
}

// containerID is the runsc container id for a sandbox. Validated, because it
// reaches an exec argument.
func containerID(sandboxID string) string { return "substrate-" + sandboxID }

func (d *Driver) bundlePath(sandboxID string) string {
	return filepath.Join(d.cfg.BundleDir, sandboxID)
}

func (d *Driver) runsc(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"--root", d.cfg.StateDir}, args...)
	cmd := exec.CommandContext(ctx, d.cfg.RunscBin, full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("runsc %s: %w: %s", strings.Join(args, " "), err, truncate(out))
	}
	return out, nil
}

// runscDetached starts a container that outlives this call.
//
// It must NOT use CombinedOutput: `runsc run --detach` daemonizes, and the
// sentry inherits the pipe, so CombinedOutput waits for an EOF that never
// comes and the create hangs forever. Output goes to a real file in the
// bundle, which also leaves something to read when a sandbox fails to start.
func (d *Driver) runscDetached(ctx context.Context, bundle string, args ...string) error {
	logPath := filepath.Join(bundle, "runsc.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()

	full := append([]string{"--root", d.cfg.StateDir}, args...)
	cmd := exec.CommandContext(ctx, d.cfg.RunscBin, full...)
	cmd.Stdout = lf
	cmd.Stderr = lf
	if err := cmd.Run(); err != nil {
		tail, _ := os.ReadFile(logPath)
		return fmt.Errorf("runsc %s: %w: %s", strings.Join(args, " "), err, truncate(tail))
	}
	return nil
}

// maxUnixPath is the portable ceiling on a unix socket path (sockaddr_un
// sun_path is 108 bytes on Linux, minus the NUL).
const maxUnixPath = 107

func truncate(b []byte) string {
	const max = 400
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return strings.TrimSpace(string(b))
}

func (d *Driver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	if !ids.ValidSandboxID(spec.ID) {
		return nil, fmt.Errorf("refusing hostile sandbox id %q", spec.ID)
	}
	createStart := time.Now()
	bundle := d.bundlePath(spec.ID)
	overlayDir := filepath.Join(bundle, "overlay")
	// The socket directory lives OUTSIDE the bundle, under a short base: unix
	// socket paths are capped near 108 bytes, and a bundle under a deep
	// data_dir silently blows that with "bind: invalid argument". It is
	// bind-mounted into the sandbox and is the only host path it can reach.
	sockDir := filepath.Join(d.cfg.SockDir, spec.ID)

	// Two separate obligations, because they are not equally optional:
	// stopContainer removes a RUNNING sandbox, cleanupFiles removes evidence.
	// They used to be one closure behind one debug flag, so
	// SUBSTRATE_KEEP_FAILED_BUNDLE=1 -- a flag whose whole purpose is to keep
	// files around to read -- also skipped the container teardown, orphaning a
	// live gVisor sandbox running tenant code with no supervisor, no registry
	// entry and nothing that would ever reap it.
	var stopContainer func()
	cleanupFiles := func() { _ = os.RemoveAll(bundle); _ = os.RemoveAll(sockDir) }
	ok := false
	defer func() {
		if ok {
			return
		}
		if stopContainer != nil {
			stopContainer()
		}
		// A failed start otherwise leaves nothing to look at: the bundle, its
		// config.json and runsc's log are the only evidence of why. Operators
		// (and this driver's own tests) can keep them.
		if os.Getenv("SUBSTRATE_KEEP_FAILED_BUNDLE") == "1" {
			d.cfg.Log("keeping failed sandbox bundle for inspection", "sandbox", spec.ID, "bundle", bundle)
			return
		}
		cleanupFiles()
	}()

	// The bundle is 0700: only the node agent can traverse into it.
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		return nil, err
	}
	// The overlay filestore lives in the bundle. runsc creates the file and
	// unlinks it immediately, holding it by fd, so it cleans itself up when
	// the sandbox dies -- there is no per-sandbox rootfs tree to reap.
	if err := os.MkdirAll(overlayDir, 0o700); err != nil {
		return nil, err
	}
	// Socket directories are 0700, owned by the node agent.
	//
	// These were 0777 while the SANDBOX created the socket -- it binds as
	// in-sandbox uid 0 with an empty capability set, so no CAP_DAC_OVERRIDE and
	// no writing to a directory it does not own. That reasoning stopped
	// applying the moment the direction was inverted and the HOST became the
	// listener, but the mode (and a comment claiming the parent was 0700, which
	// stopped being true when the sock dir moved out of the bundle) stayed.
	//
	// The result was a world-writable directory holding a socket that IS the
	// guest agent's identity: any local user could race the real agent to
	// connect, be accepted as the agent, and then answer every exec and fs call
	// the platform made for that sandbox. Both directories are locked down now.
	if err := os.MkdirAll(d.cfg.SockDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(d.cfg.SockDir, 0o700); err != nil { // MkdirAll respects umask
		return nil, err
	}
	if err := os.MkdirAll(sockDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(sockDir, 0o700); err != nil {
		return nil, err
	}
	// The agent must be STATIC. It runs inside a TENANT-supplied rootfs; a
	// dynamically linked agent needs that rootfs to contain a matching loader
	// and libc, which an Alpine (musl), distroless or scratch image does not
	// have. Checked here, not assumed, because the failure it produces is
	// deeply misleading: the container starts, the agent dies instantly in the
	// loader, and the host waits out its 30s accept and reports "guest agent
	// never connected" -- blaming the guest for a build flag on the host.
	if err := requireStatic(d.cfg.ShimBin); err != nil {
		metrics.IncDriverError(tierName, "shim_not_static")
		return nil, err
	}

	t0 := time.Now()
	rootfs, err := d.prepareTemplate(spec.Template.RootfsPath)
	if err != nil {
		metrics.IncDriverError(tierName, "prepare_template")
		return nil, fmt.Errorf("prepare template: %w", err)
	}
	metrics.ObserveStage("sandbox_create", "stage_rootfs", time.Since(t0))

	if err := writeOCIConfig(bundle, sockDir, d.cfg.ShimBin, rootfs, spec); err != nil {
		return nil, fmt.Errorf("write oci config: %w", err)
	}

	// THE HOST LISTENS; the agent inside the sandbox connects out.
	//
	// The reverse -- agent listens, host dials -- cannot work under gVisor: a
	// unix socket bound inside the sandbox lives in the sentry's own VFS and
	// the gofer does not propagate it, so the host sees an empty directory
	// while the sandbox sees its own socket. Verified directly:
	//
	//	host:    ls /…/sock/  -> (empty)
	//	sandbox: ls /sock/    -> srw------- agent.sock
	//
	// Listening on the host side also removes a race: the socket exists before
	// the sandbox starts, so the agent can never dial too early.
	hostSock := filepath.Join(sockDir, agentSock)
	if len(hostSock) > maxUnixPath {
		return nil, fmt.Errorf("agent socket path %q is %d bytes; the kernel limit is %d — configure a shorter gvisor sock_dir",
			hostSock, len(hostSock), maxUnixPath)
	}
	_ = os.Remove(hostSock)
	ln, err := net.Listen("unix", hostSock)
	if err != nil {
		return nil, fmt.Errorf("listen for guest agent: %w", err)
	}
	// THE LISTENER OUTLIVES Create.
	//
	// It used to be closed the moment Create returned, so the agent had
	// exactly one chance to connect and the sandbox died with that connection.
	// It is now owned by the machine and closed by Kill/teardown, so the agent
	// can come back after a transport error. See machine.supervise.
	lnOwned := false
	defer func() {
		if !lnOwned {
			ln.Close()
		}
	}()
	// 0600: the socket is the agent's identity, and anything that can connect
	// to it can run commands and read files inside the sandbox. The sandbox
	// reaches it as the same uid that created it, so owner permission is all it
	// needs.
	if err := os.Chmod(hostSock, 0o600); err != nil {
		return nil, fmt.Errorf("chmod agent socket: %w", err)
	}

	// The agent's credential. Written 0600 into the sandbox's own sock
	// directory; the agent reads it and unlinks it before any guest code can
	// exist (it is PID 1 and the spec starts nothing else), so it never
	// survives anywhere a tenant can read.
	token, err := newAgentToken()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sockDir, agentToken), []byte(token), 0o600); err != nil {
		return nil, fmt.Errorf("write agent token: %w", err)
	}

	cid := containerID(spec.ID)
	// Clear any stale record for this id before starting. A sandbox that
	// stopped on its own leaves its container in runsc's state dir, and
	// `runsc run` then fails "container with id … already exists" -- forever,
	// on every reconcile retry, making the sandbox permanently unstartable
	// while the failure path deletes its bundle and with it everything the
	// guest had written. Best-effort: a missing container is the normal case.
	_, _ = d.runsc(ctx, "delete", "-force", cid)

	// Detached: the container outlives this call and is supervised below.
	// --host-uds=open is the MINIMUM that lets the agent connect out: it grants
	// permission to open an existing host unix socket and nothing more (not
	// "create", not "all"). The reachable set is exactly one socket, because
	// the only host path bind-mounted into the sandbox is its own sock
	// directory. gVisor defaults this to "none", which is why the agent could
	// not connect at all.
	//
	// --pid-file makes runsc write the sentry pid we would otherwise have to
	// ask for. The usage sampler needs that pid on every tick; reading it from
	// a file once, here, is what lets VMMpid() be a field read instead of a
	// subprocess. See machine.VMMpid.
	spawnStart := time.Now()
	pidFile := filepath.Join(bundle, "sentry.pid")
	//
	// --overlay2 is what makes the shared template safe AND bounds the guest.
	// Writes land in a private filestore under the bundle, capped at
	// OverlaySizeMiB; the template underneath is never modified. Measured
	// before the change: a guest wrote 200MB straight onto the host volume and
	// `df /` inside the sandbox reported the node's real 149.9G/13.4G free.
	// After: the write stops dead at the cap and `df` reports the cap.
	overlayArg := fmt.Sprintf("root:dir=%s,size=%dM", overlayDir, d.cfg.OverlaySizeMiB)
	// "none" is a sandbox with NO network at all -- it cannot install a
	// package or call an API, which is most of what a sandbox is for. When the
	// node hands us a namespace, use netstack ("sandbox") so runsc attaches to
	// the addressed veth waiting in it.
	network := d.cfg.Network
	if spec.Net != nil && spec.Net.NSPath != "" {
		network = "sandbox"
	}
	if err := d.runscDetached(ctx, bundle, "--platform", d.cfg.Platform, "--network", network,
		"--host-uds", "open", "--overlay2", overlayArg,
		"run", "--detach", "--pid-file", pidFile, "--bundle", bundle, cid); err != nil {
		metrics.IncDriverError(tierName, "runsc_run")
		metrics.IncSandboxStart(tierName, "error")
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	metrics.ObserveStage("sandbox_create", "vmm_spawn", time.Since(spawnStart))
	// From here on a failure must tear the container down, not just the files.
	stopContainer = func() {
		killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = d.runsc(killCtx, "kill", "-all", cid, "KILL")
		_, _ = d.runsc(killCtx, "delete", "-force", cid)
	}

	connStart := time.Now()
	sess, err := acceptSession(ln, 30*time.Second)
	if err != nil {
		metrics.IncDriverError(tierName, "agent_connect")
		metrics.IncSandboxStart(tierName, "error")
		return nil, fmt.Errorf("guest agent never connected: %w", err)
	}
	metrics.ObserveStage("sandbox_create", "agent_connect", time.Since(connStart))

	helloStart := time.Now()
	hello, err := authenticate(sess, token, 10*time.Second)
	if err != nil {
		sess.Close()
		metrics.IncDriverError(tierName, "handshake")
		metrics.IncSandboxStart(tierName, "error")
		return nil, fmt.Errorf("guest handshake: %w", err)
	}
	metrics.ObserveStage("sandbox_create", "guest_handshake", time.Since(helloStart))
	d.cfg.Log("gvisor sandbox ready", "sandbox", spec.ID, "agent", hello.Agent, "platform", d.cfg.Platform)

	m := &machine{
		id: spec.ID, cid: cid, spec: spec, sess: sess, drv: d,
		bundle: bundle, done: make(chan struct{}),
		sentryPID: readPIDFile(pidFile),
		ln:        ln, token: token,
	}
	lnOwned = true
	if m.sentryPID <= 0 {
		// Not fatal -- the sandbox is up and usable -- but it means this
		// sandbox will report no usage for its whole life. Silent zero-billing
		// is the inverse of the "registered but never incremented" defect:
		// the series exists and looks healthy, it is just missing a tenant.
		metrics.IncDriverError(tierName, "pid_file")
		d.cfg.Log("no sentry pid: usage will not be sampled for this sandbox",
			"sandbox", spec.ID, "pid_file", pidFile)
	}
	go m.supervise()

	d.track(spec.ID, m)
	ok = true
	// The same histogram the other tiers report to, so create latency is
	// comparable across tiers on one dashboard.
	metrics.ObserveRestore(float64(time.Since(createStart).Milliseconds()), false)
	metrics.IncSandboxStart(tierName, "ok")
	return m, nil
}

// track registers a live machine and removes it when it dies, HOWEVER it dies.
//
// The entry used to be deleted only inside Kill, so a sandbox that stopped on
// its own -- an OOM inside the guest, the sentry crashing, an operator running
// `runsc delete` -- left a permanent entry holding a *machine and its
// GuestSession. At 2,000 sandboxes churning on a host that is an unbounded map
// leak. Mirrors the Firecracker driver's tracker, which already worked this way.
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

// LiveCountForTest reports tracked machines.
func (d *Driver) LiveCountForTest() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.live)
}

// Restore is not implemented. runsc supports checkpoint/restore, so this is a
// real future path rather than an impossibility -- but an unimplemented restore
// must report ErrUnsupported, not silently cold-boot a sandbox and lose its
// state.
func (d *Driver) Restore(ctx context.Context, spec vmm.Spec, memFile, snapFile string) (vmm.Machine, error) {
	return nil, vmm.ErrUnsupported
}

// Cleanup removes a sandbox's bundle. See vmm.Cleaner.
func (d *Driver) Cleanup(ctx context.Context, sandboxID string) error {
	if !ids.ValidSandboxID(sandboxID) {
		return fmt.Errorf("refusing hostile sandbox id %q", sandboxID)
	}
	cid := containerID(sandboxID)
	_, _ = d.runsc(ctx, "delete", "-force", cid)
	_ = os.RemoveAll(filepath.Join(d.cfg.SockDir, sandboxID))
	return os.RemoveAll(d.bundlePath(sandboxID))
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func seedRootfs(dst, src string) error {
	out, err := exec.Command("cp", "-a", filepath.Clean(src)+"/.", dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, truncate(out))
	}
	return nil
}

// readPIDFile reads the sentry pid runsc wrote at start. A container id is
// stable for one machine's whole life and so is its sentry, so this value never
// needs re-reading. Returns 0 if unreadable; VMMpid then reports not-ok and the
// sampler simply records no sample for that sandbox, which is the same thing it
// did when `runsc state` failed.
func readPIDFile(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// prepareTemplate returns a SHARED, node-owned rootfs tree for a template,
// copying it at most once per template per node.
//
// Create used to copy the whole template tree per sandbox (`seedRootfs`, a full
// per-file walk, measured at 24.8ms on a 9MB alpine and O(template size) on
// anything real) and then hand the copy to the guest to write. That was
// expensive AND it was the reason two separate defects existed: the guest could
// fill the node volume through it, and it could plant a symlink in it to
// redirect the next shim staging onto an arbitrary host path.
//
// With a size-capped overlay stacked on top (see Create) the template is never
// written by anyone, so there is nothing to give each sandbox its own copy OF.
// One tree, read by all of them.
//
// Preparation is: copy the operator's template once, then create the two bind
// MOUNT POINTS. Those have to exist in the lower layer -- verified: runsc
// creates a missing bind destination in the template directory itself, not in
// the overlay -- which is exactly why this is a node-owned copy and not the
// operator's original directory.
func (d *Driver) prepareTemplate(src string) (string, error) {
	if src == "" || !dirExists(src) {
		return "", fmt.Errorf("template rootfs %q does not exist", src)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	// Keyed on path AND mtime so replacing a template produces a new tree
	// rather than silently serving the old one. A template edited in place
	// WITHOUT touching its top directory keeps the old key; templates are
	// built as immutable trees by cmd/templates-build, so that is the
	// documented contract rather than an oversight.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", abs, fi.ModTime().UnixNano())))
	key := hex.EncodeToString(sum[:16])
	dst := filepath.Join(d.cfg.TemplateDir, key)

	d.tmplMu.Lock()
	defer d.tmplMu.Unlock()
	if p, ok := d.tmpl[key]; ok {
		return p, nil
	}
	// A previous run of this node may have prepared it already. The marker is
	// written LAST, so its presence means the copy completed.
	marker := filepath.Join(dst, ".substrate-ready")
	if _, err := os.Stat(marker); err == nil {
		d.tmpl[key] = dst
		return dst, nil
	}

	if err := os.MkdirAll(d.cfg.TemplateDir, 0o755); err != nil {
		return "", err
	}
	// Build under a temporary name and rename: a create interrupted halfway
	// must never leave a partial tree that a later create would trust.
	tmp, err := os.MkdirTemp(d.cfg.TemplateDir, ".prep-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	staging := filepath.Join(tmp, "rootfs")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", err
	}
	if err := seedRootfs(staging, abs); err != nil {
		return "", fmt.Errorf("copy template: %w", err)
	}
	// Directories ordinary guest code expects, plus the two bind mount points.
	for _, sub := range []string{"proc", "sys", "dev", "dev/pts", "dev/shm", "tmp", "etc", "root", "run", sockDirGuest} {
		_ = os.MkdirAll(filepath.Join(staging, sub), 0o755)
	}
	f, err := os.OpenFile(filepath.Join(staging, shimInGuest), os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		return "", fmt.Errorf("create shim mount point: %w", err)
	}
	f.Close()
	if err := os.WriteFile(filepath.Join(staging, ".substrate-ready"), []byte(abs+"\n"), 0o444); err != nil {
		return "", err
	}
	if err := os.Rename(staging, dst); err != nil {
		if os.IsExist(err) || errors.Is(err, unix.ENOTEMPTY) {
			// Another node process won the race; its tree is as good as ours.
			d.tmpl[key] = dst
			return dst, nil
		}
		return "", fmt.Errorf("publish template: %w", err)
	}
	d.cfg.Log("prepared shared template", "src", abs, "path", dst)
	d.tmpl[key] = dst
	return dst, nil
}

// requireStatic refuses to stage a guest agent that needs a dynamic loader.
//
// `go build ./...` with the default CGO_ENABLED=1 produces a binary with a
// PT_INTERP header pointing at the HOST's /lib/ld-linux-*.so and a libc.so.6
// dependency. Nothing in this tree needs cgo (the sqlite driver is
// modernc.org/sqlite, pure Go), so that dependency is pure accident -- but it
// makes the kernel tier work only on tenant images whose libc happens to match
// the node's. The Makefile now builds with CGO_ENABLED=0; this is the check
// that catches a binary built some other way, at stage time, with an error
// that names the actual cause.
func requireStatic(path string) error {
	f, err := elf.Open(path)
	if err != nil {
		// Not an ELF we can parse (a wrapper script, a foreign format). Let
		// the sandbox try it rather than inventing a policy for it.
		return nil
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return fmt.Errorf("guest agent %s is dynamically linked (needs a host loader) and cannot run in a tenant rootfs: rebuild with CGO_ENABLED=0", path)
		}
	}
	return nil
}

// acceptSession waits for the sandbox's agent to connect back.
func acceptSession(ln net.Listener, timeout time.Duration) (*vmm.GuestSession, error) {
	if dl, ok := ln.(interface{ SetDeadline(time.Time) error }); ok {
		if err := dl.SetDeadline(time.Now().Add(timeout)); err != nil {
			return nil, err
		}
	}
	conn, err := ln.Accept()
	if err != nil {
		return nil, err
	}
	return vmm.NewGuestSession(conn), nil
}

// newAgentToken mints the agent's credential: 32 bytes of crypto randomness,
// hex encoded so it survives a text file and a JSON field unchanged.
func newAgentToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate agent token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// authenticate completes the handshake and proves the peer is OUR agent.
//
// Once the host keeps its listener open so the agent can reconnect, the socket
// is reachable by guest code for the whole life of the sandbox -- it is
// bind-mounted into it. Without this check any process inside the sandbox could
// dial in, be accepted as the agent, and then answer on the sandbox's behalf:
// most importantly it would report the sandbox's own CPU and memory, which is
// what usage is billed from. So the availability fix must not be paid for in
// billing integrity.
//
// The comparison is constant time. Nothing here is timing-sensitive in
// practice, but a token comparison that leaks its prefix is the kind of thing
// that stops being theoretical the moment someone can retry cheaply, and they
// can: dialling a unix socket costs nothing.
func authenticate(sess *vmm.GuestSession, want string, timeout time.Duration) (*proto.Hello, error) {
	hello, err := sess.Hello(timeout)
	if err != nil {
		return nil, err
	}
	if want == "" {
		return hello, nil
	}
	if hello == nil || subtle.ConstantTimeCompare([]byte(hello.Token), []byte(want)) != 1 {
		return nil, fmt.Errorf("agent presented a bad token: refusing the connection")
	}
	return hello, nil
}

// runscState is the subset of `runsc state` output we read.
type runscState struct {
	Status string `json:"status"`
	Pid    int    `json:"pid"`
}

func (d *Driver) state(ctx context.Context, cid string) (runscState, error) {
	out, err := d.runsc(ctx, "state", cid)
	if err != nil {
		return runscState{}, err
	}
	var st runscState
	if jerr := json.Unmarshal(out, &st); jerr != nil {
		return runscState{}, fmt.Errorf("parse state: %w", jerr)
	}
	return st, nil
}
