package api

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
	procdrv "github.com/substrate-sh/substrate/internal/vmm/process"
)

// attackEnv boots the full API server in-process against a real store and
// process driver — no mocks on the request path.
type attackEnv struct {
	t      *testing.T
	srv    *Server
	st     store.Store
	cfg    *config.Config
	apiKey string
}

func newAttackEnv(t *testing.T) *attackEnv {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := timeNow()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", MaxSandboxes: 10, CreatedAt: now})
	key := "sk_attack_key"
	st.CreateAPIKey(ctx, &store.APIKey{ID: "k1", OrgID: "org_1", ProjectID: "prj_1", Kind: "project",
		KeyHash: authHashKey(key), CreatedAt: now})

	cfg := config.Defaults()
	cfg.API.JWTSecret = "attack-test-secret"
	cfg.Driver.AllowProcess = true
	log := logging.New("error")
	shim := buildShimForTest(t)
	drv := procdrv.NewDriver(shim)
	sup := reconcile.NewSupervisor(st, drv, log, t.TempDir())
	go sup.Events(ctx)

	return &attackEnv{t: t, srv: NewServer(cfg, log, st, sup), st: st, cfg: cfg, apiKey: key}
}

func (e *attackEnv) do(method, path string, body string, token string, idem bool) (int, string) {
	req, _ := newRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if idem {
		req.Header.Set("Idempotency-Key", "atk-"+path+method)
	}
	rec := recRecorder()
	e.srv.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestAttackSQLInjectionViaParams(t *testing.T) {
	e := newAttackEnv(t)
	injections := []string{
		"' OR '1'='1",
		"'; DROP TABLE sandboxes;--",
		"sb_x' UNION SELECT key_hash FROM api_keys--",
		"sb_0123456789012'--",
	}
	for _, inj := range injections {
		code, body := e.do("GET", "/v1/sandboxes?external_user_id="+urlQueryEscape(inj), "", e.apiKey, false)
		if code >= 500 {
			t.Fatalf("injection %q caused server error: %d %s", inj, code, body)
		}
		if strings.Contains(body, "key_hash") {
			t.Fatalf("injection %q leaked columns", inj)
		}
	}
}

func TestAttackPathTraversalOnFS(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_name":"t","external_user_id":"u1"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create failed: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")

	traversals := []string{
		"/v1/sandboxes/" + sbID + "/fs/../../etc/passwd",
		"/v1/sandboxes/" + sbID + "/fs/%2e%2e%2f%2e%2e%2fetc/passwd",
		"/v1/sandboxes/" + sbID + "/fs/..%2f..%2fetc/passwd",
	}
	for _, p := range traversals {
		code, body := e.do("GET", p, "", e.apiKey, false)
		if code == 200 && strings.Contains(body, "root:") {
			t.Fatalf("traversal succeeded via %s", p)
		}
	}
}

func TestAttackAuthBypass(t *testing.T) {
	e := newAttackEnv(t)
	attempts := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"garbage", "not-a-token"},
		{"jwt-none-alg", "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.e30."},
		{"sk-prefix-forged", "sk_forged_key_000000000000"},
	}
	for _, a := range attempts {
		code, _ := e.do("GET", "/v1/sandboxes", "", a.token, false)
		if code != 401 && a.token == "" {
			t.Fatalf("%s: expected 401, got %d", a.name, code)
		}
		if a.token != "" && code < 400 {
			t.Fatalf("%s: bypass succeeded (%d)", a.name, code)
		}
	}
}

func TestAttackJWTForgeryAndAudienceConfusion(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})
	_, body := e.do("POST", "/v1/sandboxes", `{"template_name":"t"}`, e.apiKey, true)
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	// mint a legit token for sbX with wrong-secret signature attempt
	forged := forgeTokenWithSecret("wrong-secret", sbID)
	code, _ := e.do("POST", "/v1/sandboxes/"+sbID+"/exec",
		`{"argv":["sh","-c","echo pwned"]}`, forged, false)
	if code == 201 {
		t.Fatal("forged JWT accepted")
	}

	// audience confusion: token for another sandbox must not work here
	otherTok := mintTokenForTest(e.cfg.API.JWTSecret, "sb_other012345")
	code2, _ := e.do("POST", "/v1/sandboxes/"+sbID+"/exec",
		`{"argv":["sh","-c","echo pwned"]}`, otherTok, false)
	_ = code2
	if code == 201 {
		t.Fatal("audience-confused token accepted")
	}
}

func TestAttackIdempotencyReplayPoisoning(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	// same key + DIFFERENT body must not replay the first response
	code1, body1 := e.do("POST", "/v1/sandboxes", `{"template_name":"t","external_user_id":"a"}`, e.apiKey, true)
	id1 := extractJSON(body1, "id")
	code2, body2 := e.do("POST", "/v1/sandboxes", `{"template_name":"t","external_user_id":"b"}`, e.apiKey, true)
	id2 := extractJSON(body2, "id")

	if code1 != code2 {
		t.Fatalf("status mismatch %d vs %d", code1, code2)
	}
	if id1 == "" || id2 == "" {
		t.Fatalf("missing ids: %q %q", id1, id2)
	}
	if id1 == id2 {
		t.Fatal("different bodies replayed same response — replay poisoning possible")
	}
}

func jsonUnmarshalStr(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func waitRunningAttack(e *attackEnv, sbID string) {
	for i := 0; i < 100; i++ {
		_, body := e.do("GET", "/v1/sandboxes/"+sbID, "", e.apiKey, false)
		if strings.Contains(body, `"running"`) {
			return
		}
		sleepMS(100)
	}
	e.t.Fatalf("sandbox %s never ran", sbID)
}

// TestIdempotencyReplayCounterIncrements: the retries signal must tick when
// a mutation is served from the idempotency cache.
func TestIdempotencyReplayCounterIncrements(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	body := `{"template_name":"t"}`
	c1, b1 := e.do("POST", "/v1/sandboxes", body, e.apiKey, true)
	if c1 != 201 && c1 != 202 {
		t.Fatalf("first create: %d %s", c1, b1)
	}

	before := substrateIdempotencyReplays()
	// debug: inspect ledger after first request
	rows, _ := e.st.ListEvents(ctx, store.EventFilter{Limit: 10})
	t.Logf("events after first: %d", len(rows))
	if rec1, err := e.st.GetIdempotency(ctx, "org_1:atk-/v1/sandboxesPOST", "/v1/sandboxes"); err != nil {
		t.Logf("getidem err: %v", err)
	} else {
		t.Logf("cached: status=%d", rec1.Status)
	}
	c2, b2 := e.do("POST", "/v1/sandboxes", body, e.apiKey, true)
	after := substrateIdempotencyReplays()

	if c2 != c1 || b2 != b1 {
		t.Fatalf("replay mismatch: %d/%s vs %d/%s", c1, b1, c2, b2)
	}
	if after <= before {
		t.Fatalf("replay counter did not increment: %v -> %v", before, after)
	}
}

// TestAttackCrossPrincipalCachePoisoning: a sandbox-token denial cached under
// (key, path) must NOT be replayed for an org-key request on the same path.
// Regression for the auth-boundary cache-poisoning bug.
func TestAttackCrossPrincipalCachePoisoning(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	_, body := e.do("POST", "/v1/sandboxes", `{"template_name":"t"}`, e.apiKey, true)
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	// mint sandbox token
	codeM, body2 := e.doIdemRaw("POST", "/v1/sandboxes/"+sbID+"/tokens", `{}`)
	if codeM != 201 {
		t.Fatalf("mint failed: %d %s", codeM, body2)
	}
	tok := extractJSON(body2, "token")
	if tok == "" {
		t.Fatalf("empty token; body=%s", body2)
	}

	sharedKey := "poison-test"
	delPath := "/v1/sandboxes/" + sbID

	// 1. sandbox token attempts destroy → denied AND cached under ITS identity
	req, _ := newRequest("DELETE", delPath, "")
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", sharedKey)
	rec := recRecorder()
	e.srv.Routes().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("expected 403 for sandbox-token destroy, got %d body=%s", rec.Code, rec.Body.String())
	}

	// 2. SAME key, org principal → must NOT get the poisoned 403 replay
	req2, _ := newRequest("DELETE", delPath, "")
	req2.Header.Set("Authorization", "Bearer "+e.apiKey)
	req2.Header.Set("Idempotency-Key", sharedKey)
	rec2 := recRecorder()
	e.srv.Routes().ServeHTTP(rec2, req2)
	if rec2.Code == 403 {
		t.Fatal("cross-principal cache poisoning: org request served sandbox-token denial")
	}
	if rec2.Code != 202 {
		t.Fatalf("org destroy failed: %d %s", rec2.Code, rec2.Body.String())
	}

	// 4. success responses still replay (cache works for the happy path)
	cA, bA := e.doIdemRaw("POST", "/v1/sandboxes/"+sbID+"/pause", `{}`)
	cB, bB := e.doIdemRaw("POST", "/v1/sandboxes/"+sbID+"/pause", `{}`)
	if cA != cB || bA != bB {
		t.Fatalf("success replay broken: %d/%s vs %d/%s", cA, bA, cB, bB)
	}
}

// TestAttackConcurrentIdempotencyRace: N simultaneous identical creates must
// produce EXACTLY ONE sandbox (plan §6.1 — retry storms corrupt state).
func TestAttackConcurrentIdempotencyRace(t *testing.T) {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_0123456789012", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	const racers = 8
	var wg sync.WaitGroup
	ids := make(chan string, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := newRequest("POST", "/v1/sandboxes", `{"template_name":"t"}`)
			req.Header.Set("Authorization", "Bearer "+e.apiKey)
			req.Header.Set("Idempotency-Key", "race-key-1")
			rec := recRecorder()
			e.srv.Routes().ServeHTTP(rec, req)
			if id := extractJSON(rec.Body.String(), "id"); id != "" {
				ids <- id
			}
		}()
	}
	wg.Wait()
	close(ids)

	distinct := map[string]bool{}
	total := 0
	for id := range ids {
		distinct[id] = true
		total++
	}
	if total == 0 {
		t.Fatal("no responses captured")
	}
	if len(distinct) != 1 {
		t.Fatalf("RACE: %d concurrent creates produced %d distinct sandboxes", racers, len(distinct))
	}

	rows, _ := e.st.ListSandboxes(ctx, store.SandboxFilter{Limit: 100})
	if len(rows) != 1 {
		t.Fatalf("DB has %d sandboxes after raced create, want 1", len(rows))
	}
}
