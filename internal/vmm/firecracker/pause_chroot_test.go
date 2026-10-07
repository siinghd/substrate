package firecracker

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

func newPauseTestDriver(t *testing.T) (*Driver, *fakeVMM, string, vmm.Spec) {
	t.Helper()
	dir := t.TempDir()
	shimBin := buildShim(t, dir)
	fcSock := filepath.Join(dir, "fcapi.sock")
	fc := newFakeVMM(t, fcSock)
	guestSock := startShim(t, shimBin, dir)

	d := NewDriver(Config{FirecrackerBin: "/bin/false", JailerBin: "/bin/true", JailBaseDir: dir, APISockOverride: fcSock})
	d.SetDialGuest(dialTestGuest(guestSock))

	spec := vmm.Spec{
		ID:       "sb_pausecheck001",
		Template: vmm.TemplateSpec{Name: "t"},
	}
	return d, fc, dir, spec
}

// TestPauseUsesChrootPathsAndReturnsHostRef: the chrooted VMM must receive
// jail-relative snapshot paths (it cannot see host paths), while the ref
// handed back points at HOST-readable files for the chunkifier. The old code
// sent host paths verbatim — invisible to the VMM — so pause failed in
// jailed production mode.
func TestPauseUsesChrootPathsAndReturnsHostRef(t *testing.T) {
	d, fc, jailBase, spec := newPauseTestDriver(t)
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	callerDir := filepath.Join(t.TempDir(), "caller-pause-dir") // NOT inside any jail
	ref, err := m.Pause(context.Background(), callerDir)
	if err != nil {
		t.Fatalf("pause: %v", err)
	}

	var req struct {
		MemFilePath  string `json:"mem_file_path"`
		SnapshotPath string `json:"snapshot_path"`
	}
	if err := json.Unmarshal([]byte(fc.bodyFor("/snapshot/create")), &req); err != nil {
		t.Fatal(err)
	}
	if req.MemFilePath != "/snapshots/memfile" || req.SnapshotPath != "/snapshots/snapshot" {
		t.Fatalf("VMM saw non-chroot paths: %+v", req)
	}

	j := d.JailDirsForTest(spec.ID)
	root := j.root()
	wantRef := filepath.Join(j.snapshots(), "memfile") + "|" + filepath.Join(j.snapshots(), "snapshot")
	_ = jailBase
	if ref != wantRef {
		t.Fatalf("ref = %q, want %q", ref, wantRef)
	}
	for i, p := range strings.Split(ref, "|") {
		if _, err := os.Stat(p); err == nil {
			_ = i // file may or may not exist with the fake VMM; path shape is what matters
		}
		if !strings.HasPrefix(p, root) {
			t.Fatalf("ref component %q escapes the jail root", p)
		}
	}
}

// TestRestoreTranslatesRefToChroot: a ref produced by Pause (host paths under
// the jail root) must reach the VMM as chroot-relative load paths.
func TestRestoreTranslatesRefToChroot(t *testing.T) {
	d, fc, _, spec := newPauseTestDriver(t)
	j := d.JailDirsForTest(spec.ID)

	mem := filepath.Join(j.snapshots(), "memfile")
	snap := filepath.Join(j.snapshots(), "snapshot")
	os.MkdirAll(filepath.Dir(mem), 0o755)
	os.WriteFile(mem, []byte("mem"), 0o644)
	os.WriteFile(snap, []byte("snap"), 0o644)

	if _, err := d.Restore(context.Background(), spec, mem, snap); err != nil {
		t.Fatalf("restore: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(fc.bodyFor("/snapshot/load")), &body); err != nil {
		t.Fatal(err)
	}
	if body["snapshot_path"] != "/snapshots/snapshot" {
		t.Fatalf("snapshot_path = %v, want chroot-relative", body["snapshot_path"])
	}
	mb := body["mem_backend"].(map[string]any)
	if mb["path"] != "/snapshots/memfile" {
		t.Fatalf("mem_backend.path = %v, want chroot-relative", mb["path"])
	}
}

// TestRestoreRefusesOutsideJailPaths attempts the confused-deputy attack:
// restore input paths become authenticated reads by the jailed VMM; anything
// outside this jail's root must be refused, not translated.
func TestRestoreRefusesOutsideJailPaths(t *testing.T) {
	d, _, _, spec := newPauseTestDriver(t)
	_, err := d.Restore(context.Background(), spec, "/etc/shadow", "/etc/passwd")
	if err == nil {
		t.Fatal("out-of-jail snapshot paths accepted")
	}
	if !strings.Contains(err.Error(), "outside jail root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

var _ = time.Second
