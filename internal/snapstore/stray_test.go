package snapstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestRebuildLRUIgnoresStrayFiles: leftover *.tmp files from a crashed write
// (or any non-chunk file, e.g. a one-byte name) must not enter the index.
// The old code indexed every basename and later panicked on names shorter
// than two bytes via hash[:2] in chunkPath during eviction.
func TestRebuildLRUIgnoresStrayFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutBlob(context.Background(), "blob1", bytes.NewReader([]byte("hello world, not zero"))); err != nil {
		t.Fatal(err)
	}

	// Scatter junk where chunks live: a crash leftover tmp file and a
	// pathologically short filename.
	junkDir := filepath.Join(dir, "chunks", "de")
	os.MkdirAll(junkDir, 0o755)
	os.WriteFile(filepath.Join(junkDir, "deadbeef.tmp"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(dir, "chunks", "x"), []byte("?"), 0o644)

	s2, err := New(dir, 1<<30, nil) // reopen runs rebuildLRU
	if err != nil {
		t.Fatalf("reopen with stray files present: %v", err)
	}

	s2.mu.Lock()
	n := len(s2.items)
	for k := range s2.items {
		if len(k) != 64 {
			s2.mu.Unlock()
			t.Fatalf("non-chunk %q entered the index", k)
		}
	}
	for hash := range s2.items { // eviction churn: the old panic site
		s2.touch(hash)
	}
	s2.mu.Unlock()
	if n == 0 {
		t.Fatal("valid chunks were dropped from the index")
	}

	// The blob must still read back through the junk.
	ix, err := s2.LoadIndex("blob1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.ReadBlob(context.Background(), ix)
	if err != nil || !bytes.Equal(got, []byte("hello world, not zero")) {
		t.Fatalf("readback after junk sweep: %q, %v", got, err)
	}
}
