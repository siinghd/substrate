package process

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// TestAttackSymlinkEscapeOnFS: a planted symlink pointing outside the
// workdir must NOT let fs ops read or write outside the sandbox root.
func TestAttackSymlinkEscapeOnFS(t *testing.T) {
	shim := buildShim(t)
	d := NewDriver(shim)

	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("TOPSECRET"), 0o644)

	workdir := t.TempDir()
	os.MkdirAll(filepath.Join(workdir, "workspace"), 0o755)
	os.Symlink(outside, filepath.Join(workdir, "evil-link"))

	spec := vmm.Spec{
		ID:       "sb_symtest1",
		Template: vmm.TemplateSpec{Name: "t", RootfsPath: workdir, MemMiB: 512, VCPUs: 1},
	}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Kill()

	ctx := context.Background()
	fs := m.Fs()

	// read through the symlink must be refused (path resolves outside root)
	if data, err := fs.Read(ctx, "/evil-link/secret.txt", 0, 0); err == nil && string(data) == "TOPSECRET" {
		t.Fatal("symlink escape: read secret outside sandbox")
	}
	// write through the symlink must not land outside
	if err := fs.Write(ctx, "/evil-link/pwned.txt", []byte("pwn"), false); err == nil {
		if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
			t.Fatal("symlink escape: wrote file outside sandbox")
		}
	}
}
