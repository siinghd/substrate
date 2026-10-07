package api

import (
	"fmt"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/store"
)

// TestSandboxJWTReachesItsOwnExecRoutes: the auth middleware resolved the exec
// id with lastPathSegment, which is "stream"/"stdin" for the sub-routes — so
// GetExec failed and it answered 404 before the handler ran. Both SDKs poll
// /stream for exec output, so this broke the whole exec loop for the credential
// grade handed to end-user agents, with an error that read as "exec deleted".
func TestSandboxJWTReachesItsOwnExecRoutes(t *testing.T) {
	e := newAttackEnv(t)
	e.cfg.Node.DataDir = t.TempDir() // exec logs need a writable dir
	ctx := t.Context()
	now := timeNow()

	e.st.PutTemplate(ctx, &store.Template{
		ID: "tpl_sbjwt000001", ProjectID: "prj_1", Name: "j", MemMiB: 512, VCPUs: 1, CreatedAt: now,
	})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_sbjwt000001"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	tok, err := auth.MintSandboxToken(e.cfg.API.JWTSecret, "substrate", sbID,
		[]string{"exec", "fs", "ports"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	code, body = e.do("POST", fmt.Sprintf("/v1/sandboxes/%s/exec", sbID),
		`{"argv":["sh","-c","echo hi; sleep 5"]}`, tok, true)
	if code != 201 && code != 202 {
		t.Fatalf("exec with sandbox token: %d %s", code, body)
	}
	execID := extractJSON(body, "exec_id")
	if execID == "" {
		t.Fatalf("no exec id in %s", body)
	}

	// The exec's OWN sandbox token must reach the routes the SDKs use.
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/execs/" + execID},
		{"GET", "/v1/execs/" + execID + "/stream?offset=0"},
	} {
		code, body := e.do(tc.method, tc.path, "", tok, false)
		if code == 404 {
			t.Errorf("%s %s -> 404 with the exec's own sandbox token: the middleware resolved the wrong path segment as the exec id", tc.method, tc.path)
		} else if code >= 400 {
			t.Errorf("%s %s -> %d: %s", tc.method, tc.path, code, body)
		}
	}
	code, body = e.do("POST", "/v1/execs/"+execID+"/stdin", `{"data":"x"}`, tok, true)
	if code == 404 {
		t.Errorf("POST stdin -> 404 with the exec's own sandbox token: %s", body)
	}
}

// TestSandboxJWTCannotReachAnotherSandboxExec: fixing the id resolution must
// not open the audience check.
func TestSandboxJWTCannotReachAnotherSandboxExec(t *testing.T) {
	e := newAttackEnv(t)
	e.cfg.Node.DataDir = t.TempDir()
	ctx := t.Context()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{
		ID: "tpl_sbjwt000002", ProjectID: "prj_1", Name: "k", MemMiB: 512, VCPUs: 1, CreatedAt: now,
	})

	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_sbjwt000002"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create victim: %d %s", code, body)
	}
	victim := extractJSON(body, "id")
	waitRunningAttack(e, victim)

	code, body = e.do("POST", fmt.Sprintf("/v1/sandboxes/%s/exec", victim),
		`{"argv":["sh","-c","sleep 5"]}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("victim exec: %d %s", code, body)
	}
	victimExec := extractJSON(body, "exec_id")

	// A second sandbox the attacker legitimately holds a token for. It only
	// has to exist; it never needs to run.
	const attacker = "sb_attacker00001"
	if err := e.st.CreateSandbox(ctx, &store.Sandbox{
		ID: attacker, OrgID: "org_1", ProjectID: "prj_1", State: store.StateRunning,
		Desired: "running", Tier: "process", MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	attackerTok, err := auth.MintSandboxToken(e.cfg.API.JWTSecret, "substrate", attacker,
		[]string{"exec", "fs", "ports"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"/v1/execs/" + victimExec,
		"/v1/execs/" + victimExec + "/stream?offset=0",
	} {
		code, resp := e.do("GET", path, "", attackerTok, false)
		if code < 400 {
			t.Errorf("GET %s with ANOTHER sandbox's token -> %d (%s); want a denial", path, code, resp)
		}
	}
}
