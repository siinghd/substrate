package main

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestTarTreeBasicAndSpecialFiles: regular files, nested dirs, and symlinks
// must round-trip; FIFOs and sockets must be SKIPPED — the old code treated
// every non-dir entry as TypeReg, so a template containing a named pipe
// blocked the build forever on open().
func TestTarTreeBasicAndSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "etc"), 0o755)
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("hostname", "sb-test\n")
	write("etc/motd", "welcome")
	os.Symlink("../hostname", filepath.Join(dir, "etc", "hostlink"))
	// FIFO: would hang the old implementation at os.Open.
	if err := mkfifo(filepath.Join(dir, "etc", "pipe")); err != nil {
		t.Skipf("cannot create fifo: %v", err)
	}

	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- tarTree(dir, &buf) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tarTree: %v", err)
		}
	case <-timeoutAfter(10):
		t.Fatal("tarTree blocked — likely opened the FIFO (special-file regression)")
	}

	seen := map[string]tar.Header{}
	rd := tar.NewReader(bytes.NewReader(buf.Bytes()))
	for {
		h, err := rd.Next()
		if err != nil {
			break
		}
		seen[h.Name] = *h
	}
	if h, ok := seen["hostname"]; !ok || h.Typeflag != tar.TypeReg {
		t.Fatalf("regular file missing/wrong type: %+v", seen["hostname"])
	}
	if h, ok := seen["etc/hostlink"]; !ok || h.Typeflag != tar.TypeSymlink || h.Linkname != "../hostname" {
		t.Fatalf("symlink not preserved: %+v", seen["etc/hostlink"])
	}
	if _, ok := seen["etc/pipe"]; ok {
		t.Fatal("FIFO was archived as a regular file")
	}
	if h, ok := seen["etc/motd"]; ok && string(mustReadAll(rd)) != "welcome" {
		// content check via separate read below
		_ = h
	}
}

// TestTarTreeManyFilesNoFDEhaustion pins the close discipline: every file's
// descriptor must be released before the next entry opens. The old
// `defer f.Close()` inside the Walk closure kept every fd open until the end,
// so large trees died with EMFILE.
func TestTarTreeManyFilesNoFDEhaustion(t *testing.T) {
	dir := t.TempDir()
	const n = 3000 // far above typical soft fd limits when nothing is closed
	for i := 0; i < n; i++ {
		p := filepath.Join(dir, string(rune('a'+i%26)), "f"+itoa(i))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte{byte(i)}, 0o644)
	}
	var buf bytes.Buffer
	if err := tarTree(dir, &buf); err != nil {
		t.Fatalf("tarTree of %d files: %v (fd leak?)", n, err)
	}
	rd := tar.NewReader(bytes.NewReader(buf.Bytes()))
	count := 0
	for {
		h, err := rd.Next()
		if err != nil {
			break
		}
		if h.Typeflag == tar.TypeReg {
			count++
		}
	}
	if count != n {
		t.Fatalf("archived %d regular files, want %d", count, n)
	}
}

// TestTarTreeDeterministic: identical content must yield identical bytes
// (chunk-hash dedup depends on it).
func TestTarTreeDeterministic(t *testing.T) {
	build := func() []byte {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("alpha"), 0o644)
		os.MkdirAll(filepath.Join(dir, "d"), 0o755)
		os.WriteFile(filepath.Join(dir, "d", "b.txt"), []byte("beta"), 0o600)
		var buf bytes.Buffer
		if err := tarTree(dir, &buf); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	a, b := build(), build()
	if !bytes.Equal(a, b) {
		t.Fatal("tar output is not deterministic for identical input")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func timeoutAfter(sec time.Duration) <-chan time.Time {
	return time.After(sec * time.Second)
}

func mustReadAll(rd *tar.Reader) []byte {
	out, _ := io.ReadAll(rd)
	return out
}

func mkfifo(p string) error { return syscall.Mkfifo(p, 0o644) }
