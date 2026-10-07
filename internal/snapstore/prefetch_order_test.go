package snapstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// recordingOrigin wraps a localOrigin and records Get call order.
type recordingOrigin struct {
	localOrigin
	mu    sync.Mutex
	calls []string
}

func (o *recordingOrigin) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	o.mu.Lock()
	o.calls = append(o.calls, key)
	o.mu.Unlock()
	return o.localOrigin.Get(ctx, key)
}

// TestPrefetchHotFollowsRecordedHotnessOrder pins the resume warm-up contract:
// PrefetchHot must fault chunks in the order RecordHotness recorded (hottest
// first). The previous implementation re-sorted by chunk index, discarding
// the ordering the hotness index exists to provide.
func TestPrefetchHotFollowsRecordedHotnessOrder(t *testing.T) {
	dir := t.TempDir()
	originRoot := t.TempDir()

	origin := &recordingOrigin{localOrigin: localOrigin{root: originRoot}}
	s, err := New(dir, 1<<30, origin)
	if err != nil {
		t.Fatal(err)
	}

	// 5 distinct chunks; chunk i content = byte(i).
	var blob []byte
	for i := 0; i < 5; i++ {
		blob = append(blob, byte('A'+i))
		blob = append(blob, make([]byte, DefaultChunkSize-1)...)
	}
	ix, err := s.PutBlob(context.Background(), "hotblob", newByteReader(blob))
	if err != nil {
		t.Fatal(err)
	}

	// Simulate demotion: chunks live in origin, cache is wiped.
	for _, c := range ix.Chunks {
		data, rerr := os.ReadFile(chunkPath(dir, c.Hash))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if err := origin.Put(context.Background(), "chunks/"+c.Hash, bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	os.RemoveAll(filepath.Join(dir, "chunks"))
	s2, err := New(dir, 1<<30, origin)
	if err != nil {
		t.Fatal(err)
	}

	// Recorded fault order: hottest = index 3, then 1, then 4, then 0, then 2.
	faultOrder := []int{3, 1, 4, 0, 2}
	if err := s2.RecordHotness(ix, faultOrder); err != nil {
		t.Fatal(err)
	}

	if err := s2.PrefetchHot(context.Background(), ix); err != nil {
		t.Fatalf("PrefetchHot: %v", err)
	}

	origin.mu.Lock()
	defer origin.mu.Unlock()
	wantOrder := []string{}
	for _, i := range faultOrder {
		wantOrder = append(wantOrder, "chunks/"+ix.Chunks[i].Hash)
	}
	if len(origin.calls) == 0 {
		t.Fatal("prefetch fetched nothing from origin")
	}
	got := origin.calls
	if len(got) < len(wantOrder) {
		t.Fatalf("only %d origin gets for %d hot chunks", len(got), len(wantOrder))
	}
	for i, want := range wantOrder {
		if got[i] != want {
			t.Fatalf("fetch #%d = %q, want %q (hotness order not honored)\ngot: %v", i, got[i], want, got)
		}
	}
}

func newByteReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
