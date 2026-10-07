package snapstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

func benchStore(b *testing.B) *Store {
	s, err := New(b.TempDir(), 1<<30, nil)
	if err != nil {
		b.Fatal(err)
	}
	return s
}

func BenchmarkPutBlob1MB(b *testing.B) {
	s := benchStore(b)
	blob := make([]byte, 1<<20)
	rand.Read(blob)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.PutBlob(ctx, "b"+string(rune('a'+i%8)), bytes.NewReader(blob)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadBlob1MB(b *testing.B) {
	s := benchStore(b)
	blob := randomBytesBench(1 << 20)
	ix, err := s.PutBlob(context.Background(), "r", bytes.NewReader(blob))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		got, err := s.ReadBlob(ctx, ix)
		if err != nil || !bytes.Equal(got, blob) {
			b.Fatal("mismatch")
		}
	}
}

func randomBytesBench(n int) []byte {
	b := make([]byte, n)
	rand.Read(b)
	return b
}

// BenchmarkWriteBlobTo1MB is the streaming counterpart to BenchmarkReadBlob1MB.
// ReadBlob's cost is O(blob) memory plus a transient copy of every chunk;
// WriteBlobTo's is O(chunk), independent of blob size — which is what makes a
// 512MiB snapshot restorable without 512MiB of heap.
func BenchmarkWriteBlobTo1MB(b *testing.B) {
	s := benchStore(b)
	blob := randomBytesBench(1 << 20)
	ix, err := s.PutBlob(context.Background(), "w", bytes.NewReader(blob))
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := s.WriteBlobTo(ctx, ix, io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
