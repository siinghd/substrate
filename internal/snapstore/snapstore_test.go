package snapstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func newStore(t *testing.T, maxBytes int64) *Store {
	t.Helper()
	s, err := New(t.TempDir(), maxBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

func TestPutReadRoundtripWithZeroElision(t *testing.T) {
	s := newStore(t, 1<<30)
	ctx := context.Background()

	blob := make([]byte, 0)
	blob = append(blob, randomBytes(DefaultChunkSize)...)
	blob = append(blob, make([]byte, DefaultChunkSize)...) // pure zero chunk
	blob = append(blob, randomBytes(100<<10)...)

	ix, err := s.PutBlob(ctx, "snap1", bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Chunks) != 3 {
		t.Fatalf("want 3 chunks (data + zero-elided + partial), got %d", len(ix.Chunks))
	}
	if ix.Chunks[1].Hash != "" || ix.Chunks[1].Len != DefaultChunkSize {
		t.Fatalf("zero chunk not elided: %+v", ix.Chunks[1])
	}

	got, err := s.ReadBlob(ctx, ix)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatalf("roundtrip mismatch: got %d want %d", len(got), len(blob))
	}

	// range read across the zero boundary
	zeroStart := int64(DefaultChunkSize)
	part, err := s.ReadRange(ctx, ix, zeroStart-10, 20+DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	want := blob[zeroStart-10 : zeroStart+10+DefaultChunkSize]
	if !bytes.Equal(part, want) {
		t.Fatalf("range read mismatch: %d vs %d", len(part), len(want))
	}
}

func TestDedupAcrossBlobs(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	shared := randomBytes(DefaultChunkSize)
	b1 := append(append([]byte{}, shared...), randomBytes(1000)...)
	b2 := append(append([]byte{}, shared...), randomBytes(1000)...)

	if _, err := s.PutBlob(context.Background(), "a", bytes.NewReader(b1)); err != nil {
		t.Fatal(err)
	}
	n1, sz1 := s.Stats()
	if _, err := s.PutBlob(context.Background(), "b", bytes.NewReader(b2)); err != nil {
		t.Fatal(err)
	}
	n2, sz2 := s.Stats()
	if n2 != n1+1 { // only the differing tail chunk is new
		t.Fatalf("dedup failed: %d -> %d chunks", n1, n2)
	}
	if sz2-sz1 <= 0 || sz2-sz1 > 2000 {
		t.Fatalf("unexpected growth: %d -> %d", sz1, sz2)
	}
}

func TestLRUEviction(t *testing.T) {
	s := newStore(t, int64(3*DefaultChunkSize))
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := s.PutBlob(ctx, "blob"+string(rune('a'+i)), bytes.NewReader(randomBytes(DefaultChunkSize))); err != nil {
			t.Fatal(err)
		}
	}
	n, sz := s.Stats()
	if n > 3 || sz > 3*DefaultChunkSize {
		t.Fatalf("eviction failed: %d chunks %d bytes", n, sz)
	}
}

func TestIndexPersistenceAndHotness(t *testing.T) {
	s := newStore(t, 1<<30)
	ctx := context.Background()
	ix, err := s.PutBlob(ctx, "hot1", bytes.NewReader(randomBytes(3*DefaultChunkSize)))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHotness(ix, []int{2, 0}); err != nil {
		t.Fatal(err)
	}
	if ix.Hot[0] != 2 || ix.Hot[1] != 0 || ix.Hot[2] != 1 {
		t.Fatalf("hotness order wrong: %v", ix.Hot)
	}
	ix2, err := s.LoadIndex("hot1")
	if err != nil {
		t.Fatal(err)
	}
	if ix2.HotBytes() != 3*DefaultChunkSize {
		t.Fatalf("hot bytes: %d", ix2.HotBytes())
	}
}

// localOrigin is a test double for object storage.
type localOrigin struct {
	root string
	mu   sync.Mutex
}

func (o *localOrigin) Put(_ context.Context, key string, r io.Reader) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	p := filepath.Join(o.root, key)
	os.MkdirAll(filepath.Dir(p), 0o755)
	b, _ := io.ReadAll(r)
	return os.WriteFile(p, b, 0o644)
}

func (o *localOrigin) Get(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(filepath.Join(o.root, key))
}

func (o *localOrigin) Delete(_ context.Context, key string) error {
	return os.Remove(filepath.Join(o.root, key))
}

func TestDemotionAndPrefetchFromOrigin(t *testing.T) {
	originRoot := t.TempDir()
	origin := &localOrigin{root: originRoot}

	cacheDir := t.TempDir()
	s, err := New(cacheDir, 1<<30, origin)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	blob := randomBytes(2 * DefaultChunkSize)
	ix, err := s.PutBlob(ctx, "demoted", bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	// simulate demotion: push chunks to origin, wipe cache dir
	for _, c := range ix.Chunks {
		data, _ := os.ReadFile(chunkPath(cacheDir, c.Hash))
		if err := origin.Put(ctx, "chunks/"+c.Hash, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	os.RemoveAll(cacheDir)

	s2, err := New(cacheDir, 1<<30, origin)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.ReadBlob(ctx, ix)
	if err != nil {
		t.Fatalf("read after demotion: %v", err)
	}
	if !bytes.Equal(got, blob) {
		t.Fatal("origin fetch mismatch")
	}
	if _, sz := s2.Stats(); sz == 0 {
		t.Fatal("fetched chunks not cached")
	}
}

func TestConcurrentPuts(t *testing.T) {
	s := newStore(t, 1<<30)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.PutBlob(context.Background(), "c"+string(rune('a'+i%4)), bytes.NewReader(randomBytes(DefaultChunkSize)))
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
