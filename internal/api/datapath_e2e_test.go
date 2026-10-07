package api

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// TestFSRoundTripBeyondFrameCapHTTP verifies the PASS-2 chunking fixes at the
// HTTP layer: a >1MiB write must land byte-exact and read back identically.
// Before the fix, writes over 1MiB failed outright and reads returned OK with
// EMPTY data (silent corruption).
func TestFSRoundTripBeyondFrameCapHTTP(t *testing.T) {
	e := newAttackEnv(t)
	ctx := e.srv == nil // placeholder to keep ctx used below if needed
	_ = ctx

	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_fshttp00001", ProjectID: "prj_1", Name: "f", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_fshttp00001"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	want := strings.Repeat("0123456789abcdef", 128*1024) // exactly 2 MiB
	code, resp := e.do("PUT", fmt.Sprintf("/v1/sandboxes/%s/fs/tmp/big.bin", sbID), want, e.apiKey, false)
	if code != 201 {
		t.Fatalf("PUT big file: %d %s", code, resp)
	}

	req, _ := newRequest("GET", fmt.Sprintf("/v1/sandboxes/%s/fs/tmp/big.bin", sbID), "")
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	rec := recRecorder()
	e.srv.Routes().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("GET big file: %d %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.String()
	if len(got) != len(want) {
		t.Fatalf("read back %d bytes, wrote %d — truncation/corruption at frame boundary", len(got), len(want))
	}
	if got != want {
		t.Fatal("read-back content mismatch")
	}
}

// TestExecStdinAndStreamResumeHTTP covers two previously untested endpoints:
// POST /v1/execs/{id}/stdin feeding real guest input, and GET
// /v1/execs/{id}/stream?offset=K resuming past the first K log bytes.
func TestExecStdinAndStreamResumeHTTP(t *testing.T) {
	e := newAttackEnv(t)
	e.cfg.Node.DataDir = t.TempDir() // exec logs land somewhere writable
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_stdin000001", ProjectID: "prj_1", Name: "s", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_stdin000001"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	// head(1) consumes exactly N stdin bytes then exits — no EOF dependency,
	// because the v1 API has no stdin-close verb.
	payload := strings.Repeat("S", 50000)
	code, body = e.do("POST", fmt.Sprintf("/v1/sandboxes/%s/exec", sbID),
		`{"argv":["head","-c","50000"]}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("exec create: %d %s", code, body)
	}
	execID := extractJSON(body, "exec_id")

	code, body = e.do("POST", fmt.Sprintf("/v1/execs/%s/stdin", execID), payload, e.apiKey, true)
	if code != 200 || !strings.Contains(body, `"written":50000`) {
		t.Fatalf("stdin write: %d %s", code, body)
	}

	// Wait for exit via status polling.
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, body = e.do("GET", fmt.Sprintf("/v1/execs/%s", execID), "", e.apiKey, false)
		if code != 200 {
			t.Fatalf("get exec: %d %s", code, body)
		}
		if strings.Contains(body, `"state":"exited"`) || strings.Contains(body, `"state":"killed"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exec never exited: %s", body)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Stream from offset 49990: must contain ONLY the last 10 bytes + exit.
	code, body = e.do("GET", fmt.Sprintf("/v1/execs/%s/stream?offset=49990", execID), "", e.apiKey, false)
	if code != 200 {
		t.Fatalf("stream: %d %s", code, body)
	}
	if !strings.Contains(body, "event: exit") {
		t.Fatalf("stream missing exit event: %q", body)
	}
	if strings.Count(body, "SSSSSSSSSS") != 1 || strings.Contains(body, strings.Repeat("S", 30)) {
		t.Fatalf("offset resume did not skip the first 49990 bytes: %q", truncateStr(body, 400))
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
