package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// doFresh issues a request with a UNIQUE idempotency key.
//
// attackEnv.do derives its key from method+path, so two identical creates
// replay the FIRST response from the idempotency cache. That made this test
// fail against correct code -- the second create returned a cached 201 that
// predated the suspension. A test that fails for the wrong reason is as
// useless as one that passes for the wrong reason.
var freshSeq atomic.Int64

func doFresh(e *attackEnv, method, path, body, token string) (int, string) {
	req, _ := newRequest(method, path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("fresh-%d", freshSeq.Add(1)))
	rec := httptest.NewRecorder()
	e.srv.Routes().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestSuspendedOrgCannotCreateSandboxes: the abuse lever must actually stop
// work starting.
func TestSuspendedOrgCannotCreateSandboxes(t *testing.T) {
	e, tpl := limitsEnv(t)
	const admin = "ak_test_admin_key_0123456789abcdef"
	e.cfg.API.AdminKey = admin

	// Works before suspension, so a failure after it means something.
	code, body := doFresh(e, "POST", "/v1/sandboxes", `{"template_id":"`+tpl+`"}`, e.apiKey)
	if code >= 400 && strings.Contains(body, "org_suspended") {
		t.Fatalf("org was already suspended: %s", body)
	}

	orgs := listOrgs(t, e, admin)
	if len(orgs) == 0 {
		t.Fatal("no orgs to suspend")
	}
	code, body = e.do("POST", "/v1/admin/orgs/"+orgs[0]+"/suspend",
		`{"suspended":true,"reason":"crypto mining"}`, admin, false)
	if code != 200 {
		t.Fatalf("suspend: %d %s", code, body)
	}

	code, body = doFresh(e, "POST", "/v1/sandboxes", `{"template_id":"`+tpl+`"}`, e.apiKey)
	if code != 403 || !strings.Contains(body, "org_suspended") {
		t.Fatalf("suspended org still created a sandbox: %d %s", code, body)
	}
	// The tenant is told WHY, so "I am blocked" does not need a support ticket.
	if !strings.Contains(body, "crypto mining") {
		t.Errorf("suspension response does not carry the reason: %s", body)
	}

	// Reversible: suspension is a state, not a deletion.
	if code, body = e.do("POST", "/v1/admin/orgs/"+orgs[0]+"/suspend", `{"suspended":false}`, admin, false); code != 200 {
		t.Fatalf("unsuspend: %d %s", code, body)
	}
	code, body = doFresh(e, "POST", "/v1/sandboxes", `{"template_id":"`+tpl+`"}`, e.apiKey)
	if code == 403 && strings.Contains(body, "org_suspended") {
		t.Fatalf("org stayed suspended after being restored: %s", body)
	}
}

// TestOnlyAdminCanSuspend: a tenant must not be able to suspend anyone --
// including a competitor whose org id they guessed.
func TestOnlyAdminCanSuspend(t *testing.T) {
	e, _ := limitsEnv(t)
	const admin = "ak_test_admin_key_0123456789abcdef"
	e.cfg.API.AdminKey = admin
	orgs := listOrgs(t, e, admin)

	for _, cred := range []string{e.apiKey, "", "ak_guessed_wrong_000000000000000000"} {
		code, body := e.do("POST", "/v1/admin/orgs/"+orgs[0]+"/suspend", `{"suspended":true}`, cred, false)
		if code < 400 {
			t.Fatalf("credential %q suspended an org: %d %s", cred, code, body)
		}
	}
	// Prove the org is still usable, i.e. none of those attempts landed.
	if code, body := e.do("GET", "/v1/sandboxes", "", e.apiKey, false); code != 200 {
		t.Fatalf("org was suspended by an unauthorised caller: %d %s", code, body)
	}
}

func listOrgs(t *testing.T, e *attackEnv, admin string) []string {
	t.Helper()
	code, body := e.do("GET", "/v1/admin/orgs", "", admin, false)
	if code != 200 {
		t.Fatalf("list orgs: %d %s", code, body)
	}
	var resp struct {
		Orgs []struct{ ID string } `json:"orgs"`
	}
	json.Unmarshal([]byte(body), &resp)
	var ids []string
	for _, o := range resp.Orgs {
		ids = append(ids, o.ID)
	}
	return ids
}
