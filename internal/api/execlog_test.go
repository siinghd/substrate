package api

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestCopyExecOutputCapsSize: recorded exec output must be bounded — a
// runaway process used to grow its log until the node's disk filled.
func TestCopyExecOutputCapsSize(t *testing.T) {
	old := maxExecLogBytes
	maxExecLogBytes = 1 << 20 // 1MiB for the test
	defer func() { maxExecLogBytes = old }()

	var out bytes.Buffer
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	// 32 chunks = 2MiB of input against a 1MiB cap, then EOF.
	src := io.MultiReader(bytes.NewReader(chunk), &countingEOFReader{chunk: chunk, max: 31})
	n, truncated := copyExecOutput(&out, src)

	if !truncated {
		t.Fatal("copyExecOutput did not report truncation past the cap")
	}
	if n > maxExecLogBytes+int64(len(truncationMarker)) {
		t.Fatalf("wrote %d bytes, cap is %d (+marker)", n, maxExecLogBytes)
	}
	if n != maxExecLogBytes+int64(len(truncationMarker)) {
		t.Fatalf("wrote %d bytes, want exactly cap+marker %d", n, maxExecLogBytes+int64(len(truncationMarker)))
	}
	if !strings.HasSuffix(out.String(), truncationMarker) {
		t.Fatal("truncation marker missing from tail of log")
	}
}

// TestCopyExecOutputNoTruncationUnderCap guards against off-by-one: exactly
// cap-sized output must not be marked truncated.
func TestCopyExecOutputNoTruncationUnderCap(t *testing.T) {
	old := maxExecLogBytes
	maxExecLogBytes = 1 << 20
	defer func() { maxExecLogBytes = old }()

	var out bytes.Buffer
	n, truncated := copyExecOutput(&out, bytes.NewReader(bytes.Repeat([]byte("y"), 1<<20)))
	if truncated || n != 1<<20 {
		t.Fatalf("n=%d truncated=%v; exact-cap output must pass unmarked", n, truncated)
	}
}

// countingEOFReader yields its chunk max times then reports EOF.
type countingEOFReader struct {
	chunk []byte
	max   int
	seen  int
}

func (r *countingEOFReader) Read(p []byte) (int, error) {
	if r.seen >= r.max {
		return 0, io.EOF
	}
	r.seen++
	n := copy(p, r.chunk)
	return n, nil
}
