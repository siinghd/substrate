package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/substrate-sh/substrate/internal/store"
)

// TestBufferBodyNeverTruncates: the idempotency middleware has to read the
// whole body to hash it and then replay it to the handler. The previous
// io.LimitReader(4MiB) returned a SHORT body with no error, so a handler
// downstream would have written truncated data and answered 2xx. Latent today
// (only POST/DELETE reach the middleware, and those carry JSON), but the trap
// is one binary POST route away from being live.
func TestBufferBodyNeverTruncates(t *testing.T) {
	for _, size := range []int{0, 1, bodySpillThreshold - 1, bodySpillThreshold, bodySpillThreshold + 1, 5 << 20} {
		t.Run(fmt.Sprintf("%d", size), func(t *testing.T) {
			payload := bytes.Repeat([]byte("substrate-body-"), size/15+1)[:size]
			req := httptest.NewRequest("POST", "/x", bytes.NewReader(payload))

			mem, spill, hash, err := bufferBody(req)
			if err != nil {
				t.Fatalf("bufferBody: %v", err)
			}
			var got []byte
			if spill != nil {
				defer spill.Close()
				got, err = io.ReadAll(spill)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				got = mem
			}
			if len(got) != size {
				t.Fatalf("replayed %d bytes, body was %d — truncation", len(got), size)
			}
			if !bytes.Equal(got, payload) {
				t.Fatal("replayed body differs from what was sent")
			}
			sum := sha256.Sum256(payload)
			if hash != hex.EncodeToString(sum[:]) {
				t.Fatal("hash is not over the full body")
			}
			// Bodies past the spill threshold must not sit on the heap.
			if size > bodySpillThreshold && spill == nil {
				t.Fatalf("body of %d bytes was kept in memory; want spill to disk", size)
			}
		})
	}
}

// TestBufferBodyRefusesOversize: past the ceiling the answer is an error the
// caller can see, never a quietly shortened body.
func TestBufferBodyRefusesOversize(t *testing.T) {
	orig := maxRequestBody
	maxRequestBody = 2 * bodySpillThreshold
	defer func() { maxRequestBody = orig }()

	payload := bytes.Repeat([]byte("x"), int(maxRequestBody)+1)
	req := httptest.NewRequest("POST", "/x", bytes.NewReader(payload))
	if _, _, _, err := bufferBody(req); err == nil {
		t.Fatal("oversize body accepted; want errBodyTooLarge")
	}
}

// TestFsWriteRefusesOversizeInsteadOfTruncating: handleFsWrite used to cut the
// body at its limit and report 201 with the SHORT length — a corrupt file the
// caller had no signal about.
func TestFsWriteRefusesOversizeInsteadOfTruncating(t *testing.T) {
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_bodylimit01", ProjectID: "prj_1", Name: "b", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_bodylimit01"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	orig := maxRequestBody
	maxRequestBody = 4096
	defer func() { maxRequestBody = orig }()

	payload := strings.Repeat("A", int(maxRequestBody)+1)
	code, resp := e.do("PUT", fmt.Sprintf("/v1/sandboxes/%s/fs/tmp/over.bin", sbID), payload, e.apiKey, false)
	if code != 413 {
		t.Fatalf("oversize write returned %d (%s); want 413 — a 2xx here means the file was silently truncated", code, resp)
	}
}
