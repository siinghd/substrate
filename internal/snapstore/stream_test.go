package snapstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

// TestWriteBlobToMatchesReadBlob: the streaming path must be byte-identical to
// the materialising one, including across zero-elided holes (which are the
// chunks most likely to be mis-sized, since they carry no data to check).
func TestWriteBlobToMatchesReadBlob(t *testing.T) {
	s, err := New(t.TempDir(), 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		make func() []byte
	}{
		{"random", func() []byte { b := make([]byte, 3<<20); rand.Read(b); return b }},
		{"all zeros", func() []byte { return make([]byte, 3<<20) }},
		{"zeros then data", func() []byte {
			b := make([]byte, 3<<20)
			rand.Read(b[2<<20:])
			return b
		}},
		{"data then zeros", func() []byte {
			b := make([]byte, 3<<20)
			rand.Read(b[:1<<20])
			return b
		}},
		{"empty", func() []byte { return nil }},
		{"single byte", func() []byte { return []byte{0x42} }},
		{"unaligned tail", func() []byte { b := make([]byte, (1<<20)+7); rand.Read(b); return b }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.make()
			ix, err := s.PutBlob(ctx, "blob-"+tc.name, bytes.NewReader(want))
			if err != nil {
				t.Fatal(err)
			}

			got, err := s.ReadBlob(ctx, ix)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("ReadBlob: %d bytes, want %d", len(got), len(want))
			}

			var buf bytes.Buffer
			n, err := s.WriteBlobTo(ctx, ix, &buf)
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(want)) {
				t.Fatalf("WriteBlobTo reported %d bytes, blob is %d", n, len(want))
			}
			if !bytes.Equal(buf.Bytes(), want) {
				t.Fatalf("WriteBlobTo produced %d bytes differing from the original", buf.Len())
			}
		})
	}
}

// TestWriteBlobToHonoursContext: a cancelled restore must stop streaming
// rather than run to completion into a writer nobody is reading.
func TestWriteBlobToHonoursContext(t *testing.T) {
	s, err := New(t.TempDir(), 1<<30, nil)
	if err != nil {
		t.Fatal(err)
	}
	blob := make([]byte, 4<<20)
	rand.Read(blob)
	ix, err := s.PutBlob(context.Background(), "cancel", bytes.NewReader(blob))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.WriteBlobTo(ctx, ix, io.Discard); err == nil {
		t.Fatal("cancelled context did not stop the stream")
	}
}
