package firecracker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// TestRestoreDoesNotClobberSandboxDisk: Restore used to call the same prepare()
// as Create, which copies the pristine template image over
// <jail>/<id>/fs/rootfs.ext4 — the very file the restored VM is wired to. Idle
// pause is on by default, so on the hv tier the DEFAULT lifecycle was: user
// writes a file, sandbox idle-pauses, user returns, disk silently rolled back
// to the template while the restored guest memory still describes the old disk.
func TestRestoreDoesNotClobberSandboxDisk(t *testing.T) {
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)

	// A "template" image, and the sandbox's own disk with the user's data.
	template := filepath.Join(dir, "template-rootfs.ext4")
	if err := os.WriteFile(template, []byte("PRISTINE-TEMPLATE"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(dialTestGuest(guestSock))

	const id = "sb_rootfskeep012"
	spec := vmm.Spec{ID: id, Template: vmm.TemplateSpec{Name: "t", RootfsPath: template}}

	// Cold boot stages the template — that part is correct.
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	m.Kill()
	fc.reset() // the VMM process died with it; a restore starts a new one

	j := d.JailDirsForTest(id)
	disk := filepath.Join(j.fs(), "rootfs.ext4")
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatalf("staged disk: %v", err)
	}
	if string(got) != "PRISTINE-TEMPLATE" {
		t.Fatalf("cold boot did not stage the template: %q", got)
	}

	// The user writes to their disk, then the sandbox idle-pauses.
	const userData = "USER-DATA-THAT-MUST-SURVIVE-A-RESUME"
	if err := os.WriteFile(disk, []byte(userData), 0o644); err != nil {
		t.Fatal(err)
	}

	m2, err := d.Restore(context.Background(), spec, "", "")
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	defer m2.Kill()

	got, err = os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userData {
		t.Fatalf("resume overwrote the sandbox disk: got %q, want %q — every byte the user wrote since creation is gone", got, userData)
	}
}

// TestRestoreRefusesMissingDisk: a missing disk means the sandbox's state is
// gone. Re-seeding it from the template would present a rolled-back sandbox as
// a successful resume; fail loudly instead.
func TestRestoreRefusesMissingDisk(t *testing.T) {
	dir := t.TempDir()
	fcSock := filepath.Join(dir, "fcapi.sock")
	newFakeVMM(t, fcSock)

	template := filepath.Join(dir, "template-rootfs.ext4")
	if err := os.WriteFile(template, []byte("PRISTINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})

	spec := vmm.Spec{ID: "sb_nodisk0000012", Template: vmm.TemplateSpec{Name: "t", RootfsPath: template}}
	_, err := d.Restore(context.Background(), spec, "", "")
	if err == nil {
		t.Fatal("restore with no existing disk succeeded; it must not silently re-seed from the template")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error does not name the cause: %v", err)
	}
}
