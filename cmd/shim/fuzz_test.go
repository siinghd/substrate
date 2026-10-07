package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzResolveContainment: adversarial paths must never resolve outside the
// sandbox root. The server's root is a fixed tempdir; any resolved path that
// escapes it is a containment failure.
func FuzzResolveContainment(f *testing.F) {
	seeds := []string{
		"/workspace/x.txt",
		"/../etc/passwd",
		"/../../root/.ssh/id_rsa",
		"/a/b/../../../../../../etc/shadow",
		"",
		".",
		"..",
		"/",
		"/tmp/../proc/self/mem",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if strings.ContainsRune(p, 0) {
			return // NUL bytes are invalid in unix paths; Go would error anyway
		}
		srv := &server{root: "/sandbox-root"}
		got := srv.resolve(p)
		if got == "" {
			return // refused = safe
		}
		if !strings.HasPrefix(got, "/sandbox-root") {
			t.Fatalf("escape: %q -> %q", p, got)
		}
	})
}

// TestResolveSymlinkExisting: EvalSymlinks resolves planted symlinks, so a
// link escaping the root must be refused by the prefix check.
func TestResolveSymlinkExisting(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}
	s := &server{root: dir}
	got := s.resolve("/link/secret")
	if got == "" {
		return // refused = safe
	}
	if !strings.HasPrefix(got, dir) {
		t.Fatalf("symlink escape allowed: %q", got)
	}
}
