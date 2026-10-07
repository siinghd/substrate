//go:build linux

package gvisor

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// These run REAL gVisor sandboxes. Unlike the Firecracker tier, this one can be
// tested on ordinary hardware -- gVisor's systrap platform needs no /dev/kvm,
// which is the entire reason the tier exists.
//
// Needs root (runsc sets up cgroups) and a rootfs to run. Skipped otherwise.
func requireGvisor(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("gVisor live tests need root: make gvisor-test")
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skip("runsc not installed")
	}
}

// testRootfs materialises a minimal rootfs. Uses the alpine image if docker is
// present; otherwise skips rather than pretending.
func testRootfs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "substrate-gvtest-" + filepath.Base(dir)
	if err := exec.Command("docker", "create", "--name", name, "alpine:3").Run(); err != nil {
		t.Skipf("cannot build a test rootfs (docker unavailable): %v", err)
	}
	defer exec.Command("docker", "rm", name).Run()
	tar := exec.Command("docker", "export", name)
	untar := exec.Command("tar", "-C", rootfs, "-xf", "-")
	untar.Stdin, _ = tar.StdoutPipe()
	if err := untar.Start(); err != nil {
		t.Fatal(err)
	}
	if err := tar.Run(); err != nil {
		t.Fatalf("docker export: %v", err)
	}
	if err := untar.Wait(); err != nil {
		t.Fatalf("untar rootfs: %v", err)
	}
	return rootfs
}

func gvDriver(t *testing.T) *Driver {
	t.Helper()
	base := t.TempDir()
	shim := filepath.Join(base, "substrate-shim")
	build := exec.Command("go", "build", "-o", shim, "./cmd/shim")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0") // must run inside the sandbox rootfs
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	// SockDir must be SHORT: a unix socket path is capped near 108 bytes and
	// t.TempDir() paths are long enough to blow it on their own.
	sockDir, err := os.MkdirTemp("/tmp", "gvs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sockDir) })
	return NewDriver(Config{
		ShimBin:     shim,
		StateDir:    filepath.Join(base, "state"),
		BundleDir:   filepath.Join(base, "bundles"),
		TemplateDir: filepath.Join(base, "templates"),
		SockDir:     sockDir,
		Log:         func(msg string, kv ...any) { t.Logf("%s %v", msg, kv) },
	})
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			return wd
		}
		wd = filepath.Dir(wd)
	}
	t.Fatal("repo root not found")
	return ""
}

// TestLiveGvisorSandboxRunsAndIsIsolated is the test that justifies the tier:
// a sandbox on a KVM-less host that actually contains the code inside it.
//
// The process tier fails every one of these probes — its "sandbox" sees 2,700+
// host processes, reads the host's /etc/passwd, and reaches sudo.
func TestLiveGvisorSandboxRunsAndIsIsolated(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)

	spec := vmm.Spec{ID: "sb_gvlive0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	ctx := context.Background()
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	if got := m.Caps().Tier; got != "kernel" {
		t.Fatalf("tier = %q, want kernel", got)
	}

	run := func(sh string) string {
		h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec %q: %v", sh, err)
		}
		var out []byte
		buf := make([]byte, 4096)
		for {
			n, rerr := h.Stdout.Read(buf)
			out = append(out, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		h.Wait(wctx)
		res := strings.TrimSpace(string(out))
		// Log every probe: a silent pass is how a "sandbox" that isolated
		// nothing looked healthy for twenty-two passes.
		t.Logf("  %-46s -> %q", sh, res)
		return res
	}

	// It runs at all.
	if got := run("echo GVISOR-RUNS"); !strings.Contains(got, "GVISOR-RUNS") {
		t.Fatalf("exec produced %q", got)
	}

	// It is a DIFFERENT kernel: gVisor's sentry, not the host's.
	if got := run("uname -r"); !strings.Contains(got, "gvisor") {
		t.Errorf("uname -r = %q; expected gVisor's own kernel, so this is not a real sandbox", got)
	}

	// The probes that expose the process tier.
	if got := run("ps -e 2>/dev/null | wc -l"); got != "" {
		if n := strings.Fields(got); len(n) > 0 && n[0] != "0" {
			// A handful of its own processes is expected; hundreds means the
			// host pid namespace is visible.
			if len(n[0]) > 2 {
				t.Errorf("sandbox sees %s processes — the host pid namespace is visible", n[0])
			}
		}
	}
	if got := run("head -1 /etc/passwd"); strings.Contains(got, "/bin/bash") {
		t.Errorf("sandbox read the HOST's /etc/passwd: %q", got)
	}
	if got := run("command -v sudo || echo NO-SUDO"); !strings.Contains(got, "NO-SUDO") {
		t.Errorf("sudo reachable inside the sandbox: %q", got)
	}
	if got := run("test -S /var/run/docker.sock && echo REACHABLE || echo NO-DOCKER"); !strings.Contains(got, "NO-DOCKER") {
		t.Errorf("docker socket reachable inside the sandbox: %q", got)
	}
	// The only host path it should be able to touch is its own socket dir.
	if got := run("ls /home 2>/dev/null | head -2"); got != "" {
		t.Errorf("sandbox can see host /home contents: %q", got)
	}
}

// TestLiveGvisorKillTearsDownTheSandbox: a killed sandbox must leave no runsc
// container and no bundle behind.
func TestLiveGvisorKillTearsDownTheSandbox(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	spec := vmm.Spec{ID: "sb_gvkill0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cid := containerID(spec.ID)
	if _, err := d.state(ctx, cid); err != nil {
		t.Fatalf("sandbox not running after Create: %v", err)
	}

	if err := m.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	select {
	case <-m.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("Done never fired after Kill")
	}
	if _, err := d.state(ctx, cid); err == nil {
		t.Error("the runsc container still exists after Kill")
	}
	if err := d.Cleanup(ctx, spec.ID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Stat(d.bundlePath(spec.ID)); !os.IsNotExist(err) {
		t.Errorf("bundle survived Cleanup (err=%v)", err)
	}
}

// TestLiveGvisorDoneFiresWhenSandboxDiesOutsideOurControl: a sandbox that stops
// on its own must not stay "live" forever, or the reconciler never restarts it.
func TestLiveGvisorDoneFiresWhenSandboxDiesOutsideOurControl(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvdied0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Cleanup(context.Background(), spec.ID)

	// Kill it behind the driver's back, as an OOM or an operator would.
	if _, err := d.runsc(ctx, "kill", "-all", containerID(spec.ID), "KILL"); err != nil {
		t.Fatalf("external kill: %v", err)
	}
	select {
	case <-m.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("sandbox died and Done never fired: it stays live forever and is never restarted")
	}
}

// TestLiveGvisorAgentSocketIsPrivate: the agent socket IS the guest agent's
// identity — anything that can connect to it runs commands and reads files
// inside that sandbox.
//
// These were 0777 while the sandbox created the socket (it binds as in-sandbox
// uid 0 with an empty capability set, so it cannot write to a directory it does
// not own). That stopped applying the moment the direction was inverted and the
// host became the listener, but the mode stayed — leaving a world-writable
// directory holding a socket any local user could race the real agent to
// connect to, and then answer every exec and fs call the platform made.
func TestLiveGvisorAgentSocketIsPrivate(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	spec := vmm.Spec{ID: "sb_gvperm0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	// Both the shared base and the per-sandbox directory must be private.
	for _, p := range []string{d.cfg.SockDir, filepath.Join(d.cfg.SockDir, spec.ID)} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			t.Errorf("%s is mode %#o: group/other can reach the agent socket and impersonate the guest agent", p, mode)
		}
	}
}

// TestLiveGvisorRegistryDoesNotLeak: the driver's live map used to be cleaned
// only by Kill, so a sandbox that stopped on its own left a permanent entry
// holding a machine and its guest session.
func TestLiveGvisorRegistryDoesNotLeak(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvleak0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Cleanup(context.Background(), spec.ID)
	if n := d.LiveCountForTest(); n != 1 {
		t.Fatalf("tracked %d machines after Create, want 1", n)
	}

	// Kill it behind the driver's back, so Kill() is never called.
	if _, err := d.runsc(ctx, "kill", "-all", containerID(spec.ID), "KILL"); err != nil {
		t.Fatalf("external kill: %v", err)
	}
	select {
	case <-m.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done never fired")
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if d.LiveCountForTest() == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("registry still holds %d machines after the sandbox died on its own", d.LiveCountForTest())
}

// TestLiveGvisorProcessLimitIsEnforced: without a process ceiling `ulimit -u`
// inside the sandbox reported "unlimited" and a fork bomb was unbounded. Each
// task is a sentry task backed by HOST memory, which the guest's own memory
// limit does not cover — so the blast radius was the node, not the sandbox.
func TestLiveGvisorProcessLimitIsEnforced(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	spec := vmm.Spec{ID: "sb_gvproc0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", "ulimit -u"}})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 1024)
	for {
		n, rerr := h.Stdout.Read(buf)
		out = append(out, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	h.Wait(wctx)

	got := strings.TrimSpace(string(out))
	if got == "unlimited" || got == "" {
		t.Fatalf("process limit inside the sandbox is %q: a fork bomb is unbounded and consumes host memory via sentry tasks", got)
	}
	t.Logf("process limit inside the sandbox: %s", got)

	// And the guest's memory ceiling must be the one we configured, not the
	// host's — otherwise the limit is decorative.
	h2, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", "head -1 /proc/meminfo"}})
	if err != nil {
		t.Fatal(err)
	}
	out = nil
	for {
		n, rerr := h2.Stdout.Read(buf)
		out = append(out, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	w2, cancel2 := context.WithTimeout(ctx, 20*time.Second)
	defer cancel2()
	h2.Wait(w2)
	mem := strings.TrimSpace(string(out))
	if !strings.Contains(mem, "262144 kB") {
		t.Errorf("guest sees %q; the 256MiB limit is not being applied", mem)
	}
}

// TestLiveGvisorCreateLatencyMeetsTarget guards the plan's headline number
// (p50 <= 300ms, p99 <= 1s) on the one tier this hardware can actually run.
// bench/phase0 refuses to produce Firecracker numbers without KVM, so until
// there is KVM hardware this is the only measured latency in the repo.
//
// Sequential, so it is the unloaded floor rather than a throughput figure.
func TestLiveGvisorCreateLatencyMeetsTarget(t *testing.T) {
	requireGvisor(t)
	if testing.Short() {
		t.Skip("latency measurement is slow")
	}
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	const n = 15
	stagesBefore := stageBreakdown(t)
	ds := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sb_lat%010d", i)
		start := time.Now()
		m, err := d.Create(ctx, vmm.Spec{ID: id, Template: vmm.TemplateSpec{
			Name: "l", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
		}})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		ds = append(ds, time.Since(start))
		m.Kill()
		d.Cleanup(ctx, id)
	}
	sort.Slice(ds, func(a, b int) bool { return ds[a] < ds[b] })
	q := func(p float64) time.Duration { return ds[int(float64(len(ds)-1)*p)] }
	p50, p95, max := q(0.5), q(0.95), ds[len(ds)-1]
	stagesAfter := stageBreakdown(t)
	for _, st := range []string{"stage_rootfs", "vmm_spawn", "agent_connect", "guest_handshake"} {
		mean, n := stageDelta(stagesBefore, stagesAfter, st)
		t.Logf("  stage %-16s mean=%6.1fms over %d creates", st, mean*1000, n)
	}
	t.Logf("gvisor create latency over %d runs: p50=%v p95=%v max=%v", n, p50, p95, max)

	if p50 > 300*time.Millisecond {
		t.Errorf("p50 create latency %v exceeds the plan's 300ms target", p50)
	}
	if max > time.Second {
		t.Errorf("slowest create %v exceeds the plan's 1s p99 target", max)
	}
}

// TestLiveGvisorSelfStoppedSandboxRestarts: a sandbox that stopped on its own
// left its container record in runsc's state dir, so every later `runsc run`
// failed "container with id … already exists" — permanently unstartable, on
// every reconcile retry — and each failed attempt's cleanup deleted the bundle,
// taking everything the guest had written with it.
func TestLiveGvisorSelfStoppedSandboxRestarts(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvrestart0001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}

	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Stop it the way a guest OOM or a crashing sentry would — never via Kill.
	if _, err := d.runsc(ctx, "kill", "-all", containerID(spec.ID), "KILL"); err != nil {
		t.Fatalf("external kill: %v", err)
	}
	select {
	case <-m.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done never fired after the sandbox stopped")
	}

	// The reconciler's next pass. This must succeed.
	m2, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("restart after a self-stop failed: %v\n"+
			"the stale container record was never reclaimed, so this sandbox is permanently unstartable", err)
	}
	defer func() { m2.Kill(); d.Cleanup(context.Background(), spec.ID) }()
}

// TestLiveGvisorGuestCannotRedirectShimStaging: the sandbox owns its rootfs, so
// hostile code can replace the staged agent with a symlink to a host path and
// kill its init. Create then re-stages the agent — and a plain write would
// follow that symlink and overwrite the target AS ROOT, then chmod it 0755.
func TestLiveGvisorGuestCannotTouchTheAgentOrTheTemplate(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvsymlink0001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}

	// Snapshot the operator's template and the host agent binary.
	tmplBefore := treeFingerprint(t, rootfs)
	shimBefore := fileFingerprint(t, d.cfg.ShimBin)

	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()
	sh := guestShell(t, ctx, m)

	// The attack used to be: replace /substrate-shim with a symlink to a host
	// path, die, and let the next Create write the agent THROUGH it as root.
	// The agent is now bind-mounted read-only, so the first step already fails
	// -- the attack has no surface rather than a guarded one.
	for _, probe := range []string{
		"echo PWN > /substrate-shim 2>&1 || echo DENIED",
		"rm -f /substrate-shim 2>&1 || echo DENIED",
		"ln -sf /etc/shadow /substrate-shim 2>&1 || echo DENIED",
		"chmod 777 /substrate-shim 2>&1 || echo DENIED",
	} {
		if got := sh(probe); !strings.Contains(got, "DENIED") && !strings.Contains(got, "Read-only") {
			t.Errorf("guest modified its own agent binary: %q -> %q", probe, got)
		}
	}

	// The guest CAN write to its own rootfs -- that is the point of a sandbox.
	// What must not happen is those writes reaching the shared template.
	if got := sh("echo tenant > /planted.txt && cat /planted.txt"); !strings.Contains(got, "tenant") {
		t.Errorf("guest cannot write to its own rootfs at all: %q", got)
	}
	sh("mkdir -p /etc/evil && echo x > /etc/evil/f && rm -rf /usr/bin 2>&1 | head -1")

	m.Kill()
	<-m.Done()

	if after := treeFingerprint(t, rootfs); after != tmplBefore {
		t.Errorf("the shared template was modified by a guest:\n before=%s\n after =%s", tmplBefore, after)
	}
	if after := fileFingerprint(t, d.cfg.ShimBin); after != shimBefore {
		t.Error("the host agent binary was modified from inside a sandbox")
	}
}

// TestLiveGvisorGuestWritesAreCapped: before this pass a guest wrote 200MB
// straight onto the host volume and `df /` inside the sandbox reported the
// node's real free space -- so one tenant could fill the data volume for every
// other tenant on the node, and could see exactly how much there was to take.
func TestLiveGvisorGuestWritesAreCapped(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	d.cfg.OverlaySizeMiB = 64
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvquota000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()
	sh := guestShell(t, ctx, m)

	// The guest must not be able to see the node's volume size.
	if got := sh("df -m / | tail -1"); strings.Contains(got, "149") || !strings.Contains(got, "64") {
		t.Errorf("df inside the sandbox does not report the overlay cap: %q", got)
	}

	// Try to write far past the cap.
	sh("dd if=/dev/zero of=/fill bs=1M count=512 2>&1 | tail -1")
	out := sh("wc -c < /fill 2>/dev/null || echo 0")
	var written int64
	fmt.Sscan(strings.TrimSpace(out), &written)
	const capBytes = 64 << 20
	if written > capBytes {
		t.Fatalf("guest wrote %d bytes against a %d byte cap: the quota is not enforced", written, capBytes)
	}
	if written == 0 {
		t.Fatalf("guest could not write at all; the sandbox is unusable, not bounded (%q)", out)
	}
	t.Logf("guest asked for 512MiB, got %d bytes against a %d byte cap", written, capBytes)
}

// TestLiveGvisorSandboxesSharingATemplateAreIsolated: the template is now
// SHARED between every sandbox that uses it. That is only safe if one
// sandbox's writes are invisible to another's.
func TestLiveGvisorSandboxesSharingATemplateAreIsolated(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	ids := []string{"sb_gvsharea00001", "sb_gvshareb00002"}
	var machines []vmm.Machine
	for _, id := range ids {
		m, err := d.Create(ctx, vmm.Spec{ID: id, Template: vmm.TemplateSpec{
			Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
		}})
		if err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		id := id
		defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
		machines = append(machines, m)
	}

	a, b := guestShell(t, ctx, machines[0]), guestShell(t, ctx, machines[1])
	a("echo secret-from-a > /tenant-a.txt")
	b("echo secret-from-b > /tenant-b.txt")

	if got := b("cat /tenant-a.txt 2>&1"); strings.Contains(got, "secret-from-a") {
		t.Fatalf("tenant B read tenant A's file through the shared template: %q", got)
	}
	if got := a("cat /tenant-b.txt 2>&1"); strings.Contains(got, "secret-from-b") {
		t.Fatalf("tenant A read tenant B's file through the shared template: %q", got)
	}
	if got := a("cat /tenant-a.txt"); !strings.Contains(got, "secret-from-a") {
		t.Fatalf("tenant A lost its own write: %q", got)
	}
}

// TestLiveGvisorTemplateIsPreparedOnce: the per-sandbox tree copy is gone. The
// first create pays for it, every later one must not.
func TestLiveGvisorTemplateIsPreparedOnce(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	var first, rest time.Duration
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("sb_gvprep000000%d", i)
		t0 := time.Now()
		m, err := d.Create(ctx, vmm.Spec{ID: id, Template: vmm.TemplateSpec{
			Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
		}})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		el := time.Since(t0)
		m.Kill()
		d.Cleanup(context.Background(), id)
		if i == 0 {
			first = el
		} else {
			rest += el
		}
	}
	avgRest := rest / 3
	t.Logf("first create (prepares template) = %v, later creates avg = %v", first, avgRest)

	// One prepared tree on disk, not four.
	entries, err := os.ReadDir(d.cfg.TemplateDir)
	if err != nil {
		t.Fatalf("read template dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 prepared template, found %d: the per-sandbox copy is still happening", len(entries))
	}
}

// guestShell returns a helper that runs a shell command inside the sandbox and
// returns its trimmed output, logging every probe -- a silent pass is how a
// "sandbox" that isolated nothing looked healthy for twenty-two passes.
func guestShell(t *testing.T, ctx context.Context, m vmm.Machine) func(string) string {
	t.Helper()
	return func(cmd string) string {
		h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", cmd}})
		if err != nil {
			t.Fatalf("exec %q: %v", cmd, err)
		}
		var out []byte
		buf := make([]byte, 4096)
		for {
			n, rerr := h.Stdout.Read(buf)
			out = append(out, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		h.Wait(wctx)
		res := strings.TrimSpace(string(out))
		t.Logf("  %-50s -> %q", cmd, res)
		return res
	}
}

// treeFingerprint summarises a directory tree: every path, size and mode.
func treeFingerprint(t *testing.T, root string) string {
	t.Helper()
	h := sha256.New()
	var n int
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		fmt.Fprintf(h, "%s|%d|%o\n", rel, fi.Size(), fi.Mode().Perm())
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return fmt.Sprintf("%d entries sha=%x", n, h.Sum(nil)[:8])
}

func fileFingerprint(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%d bytes sha=%x", len(b), sum[:8])
}
func TestLiveGvisorOrphansAreReaped(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvzombie00001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	run := func(sh string) string {
		h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"/bin/sh", "-c", sh}})
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
		var out []byte
		buf := make([]byte, 4096)
		for {
			n, rerr := h.Stdout.Read(buf)
			out = append(out, buf[:n]...)
			if rerr != nil {
				break
			}
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		h.Wait(wctx)
		return strings.TrimSpace(string(out))
	}

	for i := 0; i < 60; i++ {
		run("(sleep 0 &) ; exit 0")
	}
	time.Sleep(2 * time.Second)

	got := run("ps -e -o stat 2>/dev/null | grep -c Z || echo 0")
	first := strings.SplitN(got, "\n", 2)[0]
	if first != "0" {
		t.Fatalf("%s zombies remain after 60 orphaned children: they count against the sandbox's process ceiling and eventually brick it", first)
	}
}

// TestLiveGvisorEmitsStageMetrics: the tier shipped in PASS-24 emitted ZERO
// telemetry — no create latency, no per-stage timings, no error counters — so
// an operator could not answer "is this tier slow, and which stage?" at all,
// while every other tier reported through the same histograms.
func TestLiveGvisorEmitsStageMetrics(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	before := stageObservations(t)
	spec := vmm.Spec{ID: "sb_gvmetrics0001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	after := stageObservations(t)
	for _, stage := range []string{"stage_rootfs", "vmm_spawn", "agent_connect", "guest_handshake"} {
		if after[stage] <= before[stage] {
			t.Errorf("stage %q recorded no timing: an operator cannot see where create time goes", stage)
		}
	}
}

// stageBreakdown returns {sum, count} seconds per stage of sandbox_create.
//
// Callers must DIFF two snapshots. These are process-lifetime cumulative
// histograms on the default registry, so a bare mean mixes in every other
// test's creates -- which is exactly how an earlier version of this helper
// reported a "mean" that belonged to no measurement anyone had made.
func stageDelta(before, after map[string][2]float64, stage string) (float64, int) {
	ds := after[stage][0] - before[stage][0]
	dn := int(after[stage][1] - before[stage][1])
	if dn <= 0 {
		return 0, 0
	}
	return ds / float64(dn), dn
}

// stageBreakdown is the raw snapshot; see stageDelta.
func stageBreakdown(t *testing.T) map[string][2]float64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][2]float64{}
	for _, f := range fams {
		if f.GetName() != "substrate_stage_duration_seconds" {
			continue
		}
		for _, mt := range f.GetMetric() {
			var op, stage string
			for _, l := range mt.GetLabel() {
				switch l.GetName() {
				case "operation":
					op = l.GetValue()
				case "stage":
					stage = l.GetValue()
				}
			}
			if op == "sandbox_create" {
				out[stage] = [2]float64{mt.GetHistogram().GetSampleSum(), float64(mt.GetHistogram().GetSampleCount())}
			}
		}
	}
	return out
}

// stageObservations returns the observation count per stage of sandbox_create.
func stageObservations(t *testing.T) map[string]uint64 {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]uint64{}
	for _, f := range fams {
		if f.GetName() != "substrate_stage_duration_seconds" {
			continue
		}
		for _, mt := range f.GetMetric() {
			var op, stage string
			for _, l := range mt.GetLabel() {
				switch l.GetName() {
				case "operation":
					op = l.GetValue()
				case "stage":
					stage = l.GetValue()
				}
			}
			if op == "sandbox_create" {
				out[stage] = mt.GetHistogram().GetSampleCount()
			}
		}
	}
	return out
}

// TestLiveGvisorRejectsDynamicShim: the test helper above builds the agent with
// CGO_ENABLED=0 and a comment saying it "must run inside the sandbox rootfs" --
// but `make build` was a bare `go build ./...`, which defaults to CGO_ENABLED=1
// and links against the HOST's glibc. The test rootfs is alpine (musl), i.e.
// exactly the case that breaks, so 11 green tests proved nothing about what a
// node actually deploys.
//
// This test builds the agent the way production did and asserts Create fails
// FAST and NAMES THE CAUSE, instead of starting a container, losing the agent
// in ld.so, and blaming the guest 30 seconds later.
func TestLiveGvisorRejectsDynamicShim(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)

	// Rebuild the agent with cgo, i.e. what `go build ./...` used to produce.
	dyn := filepath.Join(t.TempDir(), "substrate-shim")
	build := exec.Command("go", "build", "-o", dyn, "./cmd/shim")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("no cgo toolchain here, nothing to prove: %v\n%s", err, out)
	}
	if err := requireStatic(dyn); err == nil {
		t.Skip("this toolchain links statically even with cgo enabled")
	}
	d.cfg.ShimBin = dyn

	spec := vmm.Spec{ID: "sb_dynshim000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	start := time.Now()
	m, err := d.Create(context.Background(), spec)
	elapsed := time.Since(start)
	if err == nil {
		m.Kill()
		d.Cleanup(context.Background(), spec.ID)
		t.Fatal("Create accepted a dynamically linked agent: it will die in the loader inside any musl/scratch rootfs")
	}
	if !strings.Contains(err.Error(), "dynamically linked") {
		t.Errorf("error does not name the cause, operator will chase the guest instead of the build: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %v to reject: that is the 30s agent-accept timeout, not a linkage check", elapsed)
	}
	if d.LiveCountForTest() != 0 {
		t.Errorf("rejected create left %d machines in the registry", d.LiveCountForTest())
	}
	t.Logf("rejected in %v: %v", elapsed.Round(time.Millisecond), err)
}

// TestLiveGvisorFailedCreateNeverOrphansAContainer.
//
// SUBSTRATE_KEEP_FAILED_BUNDLE exists so an operator can read a failed
// sandbox's config.json and runsc log. It used to return from the cleanup defer
// BEFORE the container teardown, so with the flag set a create that failed
// after `runsc run` left a LIVE gVisor sandbox on the host: running tenant
// code, no supervisor, no registry entry, nothing that would ever reap it.
// Keeping files and keeping a running container are not the same request.
func TestLiveGvisorFailedCreateNeverOrphansAContainer(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)

	// An "agent" that starts fine and simply never connects, so Create gets
	// past runsc run and then fails at the accept.
	stub := filepath.Join(t.TempDir(), "substrate-shim")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 3600\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d.cfg.ShimBin = stub

	t.Setenv("SUBSTRATE_KEEP_FAILED_BUNDLE", "1")
	spec := vmm.Spec{ID: "sb_gvorphan00001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	cid := containerID(spec.ID)
	defer func() { _, _ = d.runsc(context.Background(), "delete", "-force", cid) }()

	if m, err := d.Create(context.Background(), spec); err == nil {
		m.Kill()
		t.Fatal("Create succeeded with an agent that never connects")
	}

	// The container record must be gone; anything else is an orphan.
	out, err := d.runsc(context.Background(), "list")
	if err != nil {
		t.Fatalf("runsc list: %v", err)
	}
	if strings.Contains(string(out), cid) {
		t.Fatalf("failed create left container %s on the host:\n%s", cid, out)
	}
	if d.LiveCountForTest() != 0 {
		t.Errorf("failed create left %d machines in the registry", d.LiveCountForTest())
	}
}

// TestLiveGvisorReportsSentryPidWithoutForking.
//
// VMMpid used to fork+exec `runsc state` on every call, and the caller is the
// usage sampler: once per live sandbox per tick, serialized under the mutex
// that also guards the billing flush. It now reads a pid captured from
// runsc's --pid-file at start.
//
// The risk in that swap is silence: if the pid file is never written, VMMpid
// reports not-ok, the sampler records nothing, and usage accounting on this
// tier quietly goes to zero. So assert on a REAL pid that is a REAL live
// process, and that it stops being reported once the sandbox is gone.
func TestLiveGvisorReportsSentryPidWithoutForking(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	spec := vmm.Spec{ID: "sb_gvsentrypid01", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer d.Cleanup(context.Background(), spec.ID)

	pr, okIface := m.(interface{ VMMpid() (int, bool) })
	if !okIface {
		t.Fatal("machine no longer exposes VMMpid: the usage sampler silently records nothing for this tier")
	}
	pid, ok := pr.VMMpid()
	if !ok || pid <= 0 {
		t.Fatal("no sentry pid: usage accounting on the kernel tier would be silently zero")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("reported pid %d is not a live process: %v", pid, err)
	}
	// Cross-check against the authoritative source it used to shell out to.
	st, err := d.state(ctx, containerID(spec.ID))
	if err != nil {
		t.Fatalf("runsc state: %v", err)
	}
	if st.Pid != pid {
		t.Fatalf("cached pid %d disagrees with runsc state %d: usage is attributed to the wrong process", pid, st.Pid)
	}

	// It must go quiet once the sandbox is dead: the host recycles pids, and a
	// sample attributed to whatever now owns that number is worse than none.
	m.Kill()
	<-m.Done()
	if _, ok := pr.VMMpid(); ok {
		t.Error("still reporting a pid after the sandbox died: usage can be attributed to an unrelated host process")
	}
	t.Logf("sentry pid %d reported with zero subprocesses", pid)
}

// TestLiveGvisorSentryPidCostAtTargetScale measures the A/B directly on one
// live container: the OLD path (fork+exec `runsc state`, what VMMpid did on
// every call) against the NEW path (a field read), then extrapolates to the
// plan's 2,000-resident target, which is where the difference stops being an
// optimisation and becomes whether billing runs at all.
func TestLiveGvisorSentryPidCostAtTargetScale(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()

	spec := vmm.Spec{ID: "sb_gvpidbench001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	const iters = 20
	cid := containerID(spec.ID)
	t0 := time.Now()
	for i := 0; i < iters; i++ {
		if _, err := d.state(ctx, cid); err != nil {
			t.Fatalf("runsc state: %v", err)
		}
	}
	oldPer := time.Since(t0) / iters

	pr := m.(interface{ VMMpid() (int, bool) })
	t1 := time.Now()
	for i := 0; i < iters*1000; i++ {
		if _, ok := pr.VMMpid(); !ok {
			t.Fatal("VMMpid went not-ok on a live sandbox")
		}
	}
	newPer := time.Since(t1) / (iters * 1000)

	const target = 2000
	t.Logf("per call:  old (fork runsc state) = %v   new (field read) = %v", oldPer, newPer)
	t.Logf("one sampler sweep at %d resident sandboxes, serialized under the billing mutex:", target)
	t.Logf("  old = %v", time.Duration(target)*oldPer)
	t.Logf("  new = %v", time.Duration(target)*newPer)
	t.Logf("sampler interval is 15s: the old sweep %s within it",
		map[bool]string{true: "does NOT fit", false: "fits"}[time.Duration(target)*oldPer > 15*time.Second])

	if newPer > oldPer/100 {
		t.Errorf("new path is not decisively cheaper: old=%v new=%v", oldPer, newPer)
	}
}

// TestLiveGvisorSurvivesATransportDrop.
//
// The agent is the sandbox's PID 1 and used to serve exactly ONE connection:
// when it returned, the sandbox stopped. So any transport hiccut at all -- a
// mux desync, a slow reader, an unclean noded exit -- destroyed the sandbox and
// everything the tenant had written into it, with no way to reattach. Measured
// before the fix: closing the host side of a healthy sandbox's connection left
// `runsc state` reporting "file does not exist".
//
// The sandbox must now outlive its connection, keep its filesystem, and be
// usable again once the agent reconnects.
func TestLiveGvisorSurvivesATransportDrop(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	spec := vmm.Spec{ID: "sb_gvdrop0000001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()

	// Write something only this sandbox has, so we can prove it survived
	// rather than being silently recreated from the template.
	guestShell(t, ctx, m)("echo tenant-data > /survivor.txt")

	mm := m.(*machine)
	before := mm.session()
	before.Close() // the transport drop

	// The sandbox must NOT die.
	select {
	case <-m.Done():
		t.Fatal("sandbox died when its connection dropped: a transport error is unrecoverable data loss")
	case <-time.After(3 * time.Second):
	}

	// And the agent must be back and usable.
	deadline := time.Now().Add(20 * time.Second)
	for mm.session() == before && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if mm.session() == before {
		t.Fatal("agent never reconnected; the sandbox is alive but unreachable")
	}
	if got := guestShell(t, ctx, m)("cat /survivor.txt"); !strings.Contains(got, "tenant-data") {
		t.Fatalf("the sandbox came back WITHOUT its filesystem: %q", got)
	}
	st, serr := d.state(ctx, containerID(spec.ID))
	if serr != nil || st.Pid <= 0 {
		t.Fatalf("container is not running after the reconnect: %+v (err=%v)", st, serr)
	}
}

// TestLiveGvisorGuestCannotImpersonateTheAgent.
//
// Keeping the listener open so the agent can reconnect makes the socket
// reachable by guest code for the sandbox's whole life -- it is bind-mounted
// in. An accepted impostor answers on the sandbox's behalf, and among the
// things an agent reports is the sandbox's own CPU and memory, which is what
// usage is billed from. The availability fix must not be paid for in billing
// integrity, so the agent authenticates with a token it reads and UNLINKS
// before any guest code can exist.
func TestLiveGvisorGuestCannotImpersonateTheAgent(t *testing.T) {
	requireGvisor(t)
	rootfs := testRootfs(t)
	d := gvDriver(t)
	ctx := context.Background()
	stageImpostor(t, rootfs)
	spec := vmm.Spec{ID: "sb_gvimperson001", Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 256, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), spec.ID) }()
	sh := guestShell(t, ctx, m)

	// 1. The token must be unreadable from inside: the agent unlinked it.
	if got := sh("cat /sock/agent.token 2>&1"); strings.Contains(got, "  ") || !strings.Contains(got, "No such file") {
		t.Errorf("the agent token is still readable by tenant code: %q", got)
	}
	if got := sh("ls /sock/ 2>&1"); strings.Contains(got, "token") {
		t.Errorf("token file still present in the sandbox: %q", got)
	}
	// It must not be recoverable from the agent's argv or environment either.
	if got := sh("cat /proc/1/cmdline 2>/dev/null | tr '\\0' ' '"); strings.Contains(got, "--token ") {
		t.Errorf("token passed in argv, readable via /proc: %q", got)
	}

	// 2. A real impostor, hammering the socket THROUGH THE RECONNECT WINDOW.
	//
	// While the real agent is connected the host is not accepting at all, so
	// the only window that matters is after a drop -- which is precisely the
	// window this pass opened. Race it deliberately.
	mm := m.(*machine)
	live := mm.session()

	// Detached, writing to a file: the attacker must outlive the connection we
	// are about to drop, and anything streaming over that connection dies with
	// it (execs die with connections -- the v1 contract).
	// setsid: a determined attacker detaches, so it survives the process-group
	// cleanup the agent runs on reconnect. That cleanup must NOT be what this
	// test is really relying on -- the token check has to stand on its own.
	sh("setsid /impostor /sock/agent.sock > /impostor.out 2>&1 &")
	time.Sleep(300 * time.Millisecond) // let it get going
	live.Close()                       // the drop: the host starts re-accepting

	// The REAL agent must still have won the race and be serving.
	deadline := time.Now().Add(30 * time.Second)
	for mm.session() == live && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if mm.session() == live {
		t.Fatal("the real agent never reconnected: the impostor starved it out")
	}
	sh2 := guestShell(t, ctx, m)
	if got := sh2("echo still-mine"); !strings.Contains(got, "still-mine") {
		t.Errorf("the sandbox is not usable after the impersonation attempt: %q", got)
	}
	select {
	case <-m.Done():
		t.Error("the sandbox died during the impersonation attempt: a tenant can kill its own sandbox on demand")
	default:
	}

	// Now read the attacker's own scorecard.
	deadline2 := time.Now().Add(30 * time.Second)
	var out string
	for time.Now().Before(deadline2) {
		out = sh2("cat /impostor.out 2>/dev/null")
		if strings.Contains(out, "IMPOSTOR") {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !strings.Contains(out, "IMPOSTOR") {
		t.Fatalf("the impostor never ran, so nothing was attempted: %q", out)
	}
	if !strings.Contains(out, "accepted=0") {
		t.Errorf("the host talked to guest code as if it were the agent: %q", out)
	}
	if strings.Contains(out, "attempts=0") {
		t.Fatalf("the impostor never reached the socket, so this test proves nothing: %q", out)
	}
	t.Logf("impostor scorecard: %s", strings.TrimSpace(out))
}

// stageImpostor builds the guest-side attacker and puts it in the test rootfs.
func stageImpostor(t *testing.T, rootfs string) {
	t.Helper()
	bin := filepath.Join(rootfs, "impostor")
	build := exec.Command("go", "build", "-o", bin, "./internal/vmm/gvisor/testdata/impostor")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build impostor: %v\n%s", err, out)
	}
}
