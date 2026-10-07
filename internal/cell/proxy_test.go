package cell

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestReadBoundedRejectsTruncation: a node response that dies mid-body or
// blows the size cap must surface as an error — never as a complete-looking
// body forwarded to the client with the upstream status code.
func TestReadBoundedRejectsTruncation(t *testing.T) {
	full := strings.Repeat("a", 100)

	out, err := readBounded(strings.NewReader(full), 200)
	if err != nil || len(out) != 100 {
		t.Fatalf("normal read: %d bytes, err %v", len(out), err)
	}

	if _, err := readBounded(errReader{}, 200); err == nil {
		t.Fatal("mid-stream read error swallowed")
	}

	big := strings.Repeat("b", 4096)
	if _, err := readBounded(strings.NewReader(big), 1024); err == nil {
		t.Fatal("over-limit response accepted (silent truncation)")
	}
}

// TestForwardRejectsHostileSandboxID attempts path injection through the id
// segment that gets concatenated onto the node URL while carrying nodeKey.
func TestForwardRejectsHostileSandboxID(t *testing.T) {
	p := NewProxy(nil)
	for _, id := range []string{
		"sb_ok../../admin",
		"sb_x%2F..%2Fv1",
		"",
		"sb_short",
		"totally/not/a/sandbox",
	} {
		_, status, err := p.Forward(context.Background(), "org_1", id, "GET", "/v1/sandboxes/"+id, nil, "sk_test")
		if err == nil {
			t.Fatalf("hostile id %q forwarded", id)
		}
		if status != 404 && status != 0 {
			t.Fatalf("hostile id %q: want 404/no-placement, got %d", id, status)
		}
	}
}

// TestSchedulerCreateRejectsGarbageBody: malformed JSON must not silently
// provision default-sized sandboxes.
func TestSchedulerCreateRejectsGarbageBody(t *testing.T) {
	p := NewProxy(nil)
	_, status, err := p.SchedulerCreate(context.Background(), p.Registry, []byte("{not json"), "org_local", "sk_test")
	if err == nil {
		t.Fatal("garbage body accepted")
	}
	if status != 400 {
		t.Fatalf("want 400, got %d", status)
	}
}

type errReader struct{}

func (errReader) Read(p []byte) (int, error) { return 0, errors.New("connection reset mid-body") }

var _ io.Reader = errReader{}
