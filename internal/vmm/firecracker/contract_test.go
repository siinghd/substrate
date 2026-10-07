package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/substrate-sh/substrate/internal/vmm"
)

func fcEnv(t *testing.T) (*Driver, *fakeVMM, string) {
	t.Helper()
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)
	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(dialTestGuest(guestSock))
	return d, fc, dir
}

func contextTODO() context.Context { return context.Background() }

func specWithTemplate(t *testing.T, id, dir string) vmm.Spec {
	t.Helper()
	return vmm.Spec{ID: id, Template: mustTemplate(t, dir)}
}

func mustTemplate(t *testing.T, dir string) vmm.TemplateSpec {
	t.Helper()
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if err := os.WriteFile(rootfs, []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	kernel := filepath.Join(dir, "vmlinux")
	if err := os.WriteFile(kernel, []byte("kernel"), 0o644); err != nil {
		t.Fatal(err)
	}
	return vmm.TemplateSpec{Name: "t", RootfsPath: rootfs, KernelPath: kernel, MemMiB: 512, VCPUs: 1}
}

// TestPauseMustPauseTheVMFirst: Firecracker refuses /snapshot/create on a
// RUNNING microVM. The driver went straight to snapshot/create, so pause could
// never have worked against a real VMM — and idle-pause and pressure-pause both
// route through it, so scale-to-zero rested on a call that always failed. The
// old permissive fake answered 204 to everything and hid it.
func TestPauseMustPauseTheVMFirst(t *testing.T) {
	d, fc, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_pauseorder001", Template: mustTemplate(t, dir)}

	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if fc.vmState() != "Running" {
		t.Fatalf("after create the microVM is %q, want Running", fc.vmState())
	}

	ref, err := m.Pause(context.Background(), "")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if ref == "" {
		t.Fatal("pause returned an empty ref")
	}

	// The VM must have been paused BEFORE the snapshot was taken.
	calls := fc.callsInOrder()
	pauseAt, snapAt := -1, -1
	for i, c := range calls {
		if c == "PATCH /vm" && pauseAt < 0 {
			pauseAt = i
		}
		if c == "PUT /snapshot/create" {
			snapAt = i
		}
	}
	if snapAt < 0 {
		t.Fatalf("no snapshot was created: %v", calls)
	}
	if pauseAt < 0 || pauseAt > snapAt {
		t.Fatalf("snapshot taken without pausing first: %v", calls)
	}
	if r := fc.rejections(); len(r) != 0 {
		t.Fatalf("the VMM rejected calls the driver made: %v", r)
	}
}

// TestPauseFailureLeavesGuestRunning: a snapshot that fails must not leave the
// tenant with a frozen sandbox.
func TestPauseFailureLeavesGuestRunning(t *testing.T) {
	d, fc, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_pausefail0001", Template: mustTemplate(t, dir)}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fc.failSnapshotCreate(true)

	if _, err := m.Pause(context.Background(), ""); err == nil {
		t.Fatal("pause reported success despite a failing snapshot")
	}
	if st := fc.vmState(); st != "Running" {
		t.Fatalf("guest left in state %q after a failed pause; want Running", st)
	}
}

// TestRestoreDoesNotReconfigureDevices: a loaded snapshot counts as a started
// microVM, so device configuration is rejected — and the vsock CID comes back
// from the snapshot anyway, so a freshly allocated one could never have reached
// the restored guest.
func TestRestoreDoesNotReconfigureDevices(t *testing.T) {
	d, fc, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_restoreorder1", Template: mustTemplate(t, dir)}

	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref, err := m.Pause(context.Background(), "")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	fc.reset() // pause killed the VMM; restore starts a new process
	mark := len(fc.callsInOrder())

	mem, snap, ok := strings.Cut(ref, "|")
	if !ok {
		t.Fatalf("malformed pause ref %q", ref)
	}
	m2, err := d.Restore(context.Background(), spec, mem, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer m2.Kill()

	// Only the calls the RESTORE made, not the create that set it up.
	for _, c := range fc.callsInOrder()[mark:] {
		if c == "PUT /vsock" || c == "PUT /boot-source" || c == "PUT /machine-config" ||
			strings.HasPrefix(c, "PUT /drives/") {
			t.Errorf("restore reconfigured %q; devices come from the snapshot", c)
		}
	}
	if r := fc.rejections(); len(r) != 0 {
		t.Fatalf("the VMM rejected calls the restore path made: %v", r)
	}
	if st := fc.vmState(); st != "Running" {
		t.Fatalf("restored microVM is %q, want Running", st)
	}
}

// TestSnapshotPathsAreInsideTheJail: the jailed VMM can only reach paths under
// its own chroot, so the paths handed to /snapshot/create must be chroot-
// relative and must resolve to real files on the host side.
func TestSnapshotPathsAreInsideTheJail(t *testing.T) {
	d, fc, dir := fcEnv(t)
	spec := vmm.Spec{ID: "sb_snappaths0001", Template: mustTemplate(t, dir)}
	fc.setChroot(d.JailDirsForTest(spec.ID).root())

	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ref, err := m.Pause(context.Background(), "")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	for _, p := range fc.snapshotPaths() {
		if !strings.HasPrefix(p, "/") || strings.Contains(p, "..") {
			t.Errorf("snapshot path %q is not a clean chroot-absolute path", p)
		}
	}
	// The ref must name HOST paths that actually exist, or the chunkifier
	// cannot read them.
	for _, hostPath := range strings.Split(ref, "|") {
		if _, err := os.Stat(hostPath); err != nil {
			t.Errorf("pause ref names %q, which does not exist on the host: %v", hostPath, err)
		}
	}
}

// TestJailLayoutIsSelfConsistent pins the one rule the layout has: the VMM sees
// a path P inside its chroot, and the host sees that same file at root()+P.
//
// The old layout broke it in three places at once — fs/, run/ and snapshots/
// were siblings of root() and therefore OUTSIDE the chroot the VMM was confined
// to, while the drive was configured as the chroot-absolute "/fs/rootfs.ext4";
// apiSock() named a different file from the one the client actually opened; and
// --chroot-base-dir was handed root() itself, so the jailer's real chroot was
// two levels below where every host path assumed it was.
func TestJailLayoutIsSelfConsistent(t *testing.T) {
	d := NewDriver(Config{FirecrackerBin: "/usr/local/bin/firecracker", JailerBin: "/bin/true", JailBaseDir: "/srv/jail"})
	j := d.JailDirsForTest("sb_layout0000001")

	// Everything must live under the sandbox's own tree.
	for name, p := range map[string]string{
		"root": j.root(), "run": j.run(), "fs": j.fs(),
		"snapshots": j.snapshots(), "apiSock": j.apiSock(),
	} {
		if !strings.HasPrefix(p, j.tree()+string(filepath.Separator)) {
			t.Errorf("%s = %q is outside the sandbox tree %q", name, p, j.tree())
		}
	}

	// Every host path must be root() + its chroot-absolute path.
	for chrootPath, host := range map[string]string{
		fcRunInChroot:       j.run(),
		fcFsInChroot:        j.fs(),
		fcSnapshotsInChroot: j.snapshots(),
		fcAPISockInChroot:   j.apiSock(),
	} {
		if want := filepath.Join(j.root(), chrootPath); host != want {
			t.Errorf("chroot path %q maps to host %q, want %q", chrootPath, host, want)
		}
		if !strings.HasPrefix(host, j.root()+string(filepath.Separator)) {
			t.Errorf("chroot path %q maps OUTSIDE the chroot: %q", chrootPath, host)
		}
	}

	// The jailer builds <chroot-base>/<exec-basename>/<id>/root; root() must
	// be what the jailer would actually produce from the base we pass it.
	wantRoot := filepath.Join(j.chrootBase(), "firecracker", "sb_layout0000001", "root")
	if j.root() != wantRoot {
		t.Fatalf("root() = %q but the jailer given --chroot-base-dir %q would create %q",
			j.root(), j.chrootBase(), wantRoot)
	}

	// The pidfile must not sit under a subdirectory the jailer never creates.
	if strings.Count(strings.Trim(fcPidFileInChroot, "/"), "/") != 0 {
		t.Fatalf("pidfile %q is nested; the jailer creates the chroot but no subdirectories", fcPidFileInChroot)
	}
}

// TestVsockCIDIsRealAndUnique: prepareJail wrote the allocated CID into its own
// BY-VALUE copy of the spec, so it never reached the caller and every microVM
// was configured with guest_cid 0 — VMADDR_CID_HYPERVISOR, a reserved address.
// Either Firecracker rejects it and nothing boots, or it accepts it and the
// per-boot CID uniqueness the §7.4 uniqueness protocol rests on is silently
// absent across the whole fleet.
func TestVsockCIDIsRealAndUnique(t *testing.T) {
	// ONE driver: CID uniqueness is a property of a node's allocator, so a
	// fresh driver per sandbox would trivially "pass" while proving nothing.
	d, fc, dir := fcEnv(t)
	seen := map[uint32]bool{}
	for i, id := range []string{"sb_cid0000000001", "sb_cid0000000002", "sb_cid0000000003"} {
		fc.reset() // each sandbox is its own Firecracker process
		m, err := d.Create(context.Background(), specWithTemplate(t, id, dir))
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		defer m.Kill()

		body := fc.bodyFor("/vsock")
		if body == "" {
			t.Fatal("no vsock device was configured")
		}
		var v struct {
			GuestCID uint32 `json:"guest_cid"`
			UDSPath  string `json:"uds_path"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("vsock body %q: %v", body, err)
		}
		// 0, 1 and 2 are reserved (hypervisor, local, host).
		if v.GuestCID < 3 {
			t.Fatalf("guest_cid = %d, which is reserved; the allocated CID never reached PutVsock", v.GuestCID)
		}
		if v.UDSPath == "" {
			t.Fatal("vsock configured without uds_path: Firecracker creates no host endpoint")
		}
		if seen[v.GuestCID] {
			t.Fatalf("two sandboxes on one node were given the same guest CID %d; §7.4 uniqueness is not in force", v.GuestCID)
		}
		seen[v.GuestCID] = true
	}
}

// TestRestoreDoesNotBurnACID: a restored microVM brings its vsock device back
// from the snapshot, so allocating a CID on every resume drained a never-freed
// allocator.
func TestRestoreDoesNotBurnACID(t *testing.T) {
	var allocated int
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)
	next := uint32(3)
	d := NewDriver(Config{
		FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock,
		CIDAlloc: func() (uint32, error) { allocated++; next += 2; return next, nil },
	})
	d.SetDialGuest(dialTestGuest(guestSock))

	spec := vmm.Spec{ID: "sb_cidrestore001", Template: mustTemplate(t, dir)}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	afterCreate := allocated
	ref, err := m.Pause(context.Background(), "")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	fc.reset()

	mem, snap, _ := strings.Cut(ref, "|")
	m2, err := d.Restore(context.Background(), spec, mem, snap)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer m2.Kill()
	if allocated != afterCreate {
		t.Fatalf("restore allocated %d extra CID(s); the vsock device comes back from the snapshot", allocated-afterCreate)
	}
}
