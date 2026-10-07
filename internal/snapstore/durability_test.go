package snapstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPutBlobNeverReturnsAnUnreadableIndex is the invariant that matters:
// PutBlob either fails, or returns an index whose blob can be read back. It
// must never report success for a template whose data it has already deleted.
//
// The confirmed defect: the final touch loop evicted while over budget, and
// once the blob's own chunks were all that remained at the LRU tail it unlinked
// them — then returned the index and a success manifest. Reproduced directly:
// with the budget guard removed, budget=8 gives
// "PutBlob reported success, ReadBlob fails: chunk not in cache or origin".
func TestPutBlobNeverReturnsAnUnreadableIndex(t *testing.T) {
	const chunkSize = 4096
	const blobChunks = 40

	// Budgets from far too small, through just-fits, to comfortable.
	for _, budgetChunks := range []int64{4, 8, 20, 39, 40, 45, 80, 200} {
		s, err := New(t.TempDir(), budgetChunks*chunkSize, nil)
		if err != nil {
			t.Fatal(err)
		}
		s.chunkSize = chunkSize

		blob := make([]byte, blobChunks*chunkSize)
		rand.Read(blob)

		ix, err := s.PutBlob(context.Background(), "b", bytes.NewReader(blob))
		if err != nil {
			if !errors.Is(err, ErrCacheTooSmall) {
				t.Errorf("budget=%d: unexpected error %v", budgetChunks, err)
			}
			continue // refused honestly
		}
		got, rerr := s.ReadBlob(context.Background(), ix)
		if rerr != nil {
			t.Errorf("budget=%d: PutBlob reported success but the blob is unreadable: %v", budgetChunks, rerr)
			continue
		}
		if !bytes.Equal(got, blob) {
			t.Errorf("budget=%d: blob read back differs from what was written", budgetChunks)
		}
	}
}

// TestPutBlobRefusesBlobLargerThanTheBudget: a blob that cannot be held
// resident must fail loudly, not return an index whose chunks vanish the
// moment the pins drop.
func TestPutBlobRefusesBlobLargerThanTheBudget(t *testing.T) {
	const chunkSize = 4096
	s, err := New(t.TempDir(), 8*chunkSize, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.chunkSize = chunkSize

	blob := make([]byte, 40*chunkSize)
	rand.Read(blob)
	if _, err := s.PutBlob(context.Background(), "toobig", bytes.NewReader(blob)); !errors.Is(err, ErrCacheTooSmall) {
		t.Fatalf("PutBlob error = %v, want ErrCacheTooSmall — a blob that cannot fit must not report success", err)
	}
}

// TestRebuildSweepsOrphanedTmpFiles: a writer killed mid-write leaves
// <hash>.<rand>.tmp behind. Nothing ever removed them, so they were invisible
// to Stats() and to the budget while occupying real disk — the cache
// overshot maxBytes by an unbounded margin and never evicted.
func TestRebuildSweepsOrphanedTmpFiles(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 8192)
	rand.Read(blob)
	if _, err := s.PutBlob(context.Background(), "b", bytes.NewReader(blob)); err != nil {
		t.Fatal(err)
	}

	// Simulate an interrupted write.
	sub := filepath.Join(dir, "chunks", "ab")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(sub, "abcdef.123456.tmp")
	if err := os.WriteFile(orphan, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := New(dir, 1<<20, nil); err != nil { // re-open runs rebuildLRU
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned tmp file survived a restart (err=%v): it occupies disk the budget cannot see", err)
	}
}

// failingOrigin returns bytes that do not hash to the requested chunk.
type failingOrigin struct{ payload []byte }

func (f failingOrigin) Get(_ context.Context, _ string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(f.payload)), nil
}
func (f failingOrigin) Put(_ context.Context, _ string, _ io.Reader) error { return nil }
func (f failingOrigin) Delete(context.Context, string) error               { return nil }

// TestOriginContentIsVerified: the store is content-addressed and deduped, so
// adopting bytes that do not hash to the requested name poisons every template
// that shares the chunk.
func TestOriginContentIsVerified(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<20, failingOrigin{payload: []byte("not the chunk you asked for")})
	if err != nil {
		t.Fatal(err)
	}
	const bogus = "0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := s.getChunk(context.Background(), bogus, 0); err == nil {
		t.Fatal("origin content that does not match its content address was adopted")
	}
	if _, err := os.Stat(chunkPath(dir, bogus)); err == nil {
		t.Fatal("mismatching origin content was written under the requested hash")
	}
}

// TestChunkFilesAreDurable is a best-effort check that the write path publishes
// a fully-sized file under the content-addressed name.
func TestChunkFilesAreDurable(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4096)
	rand.Read(blob)
	ix, err := s.PutBlob(context.Background(), "d", bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ix.Chunks {
		if c.Hash == "" {
			continue
		}
		st, err := os.Stat(chunkPath(dir, c.Hash))
		if err != nil {
			t.Fatalf("chunk %s not on disk: %v", c.Hash, err)
		}
		if st.Size() != int64(c.Len) {
			t.Fatalf("chunk %s is %d bytes on disk, index says %d", c.Hash, st.Size(), c.Len)
		}
	}
	// No tmp files left behind by a successful write.
	_ = filepath.Walk(filepath.Join(dir, "chunks"), func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && strings.HasSuffix(p, ".tmp") {
			t.Errorf("successful write left a tmp file: %s", p)
		}
		return nil
	})
}
