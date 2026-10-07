package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// adminEnv returns an environment with the admin API enabled.
func adminEnv(t *testing.T) (*attackEnv, string) {
	t.Helper()
	e, _ := limitsEnv(t)
	const adminKey = "ak_test_admin_key_0123456789abcdef"
	e.cfg.API.AdminKey = adminKey
	return e, adminKey
}

// TestAdminAPIIsClosedToTenantCredentials: the whole point of a separate admin
// grade is that a tenant key cannot provision tenants. Attempt it.
func TestAdminAPIIsClosedToTenantCredentials(t *testing.T) {
	e, admin := adminEnv(t)
	for _, tc := range []struct{ name, cred string }{
		{"tenant org key", e.apiKey},
		{"no credential", ""},
		{"guessed admin key", "ak_wrong_key_0000000000000000000000"},
		{"admin prefix on a tenant key", "ak_" + strings.TrimPrefix(e.apiKey, "sk_")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.do("POST", "/v1/admin/orgs", `{"name":"pwned"}`, tc.cred, false)
			if code < 400 {
				t.Fatalf("credential %q provisioned an org: %d %s", tc.name, code, body)
			}
		})
	}
	// And the real one works, so the test above is not passing because the
	// route is simply broken for everybody.
	code, body := e.do("POST", "/v1/admin/orgs", `{"name":"real"}`, admin, false)
	if code != 201 {
		t.Fatalf("admin key could not create an org: %d %s", code, body)
	}
}

// TestAdminAPIDisabledWhenNoKeyConfigured: a node that never set an admin key
// must not be provisionable at all. Fail closed, not open.
func TestAdminAPIDisabledWhenNoKeyConfigured(t *testing.T) {
	e, _ := limitsEnv(t)
	e.cfg.API.AdminKey = "" // the default
	for _, cred := range []string{"", "ak_", "ak_anything", e.apiKey} {
		code, body := e.do("POST", "/v1/admin/orgs", `{"name":"x"}`, cred, false)
		if code < 400 {
			t.Fatalf("admin API answered with no admin key configured: %d %s (cred %q)", code, body, cred)
		}
	}
}

// TestMintedKeyWorksAndIsShownExactlyOnce is the onboarding path end to end:
// create org -> create project -> mint key -> that key can create a sandbox.
func TestMintedKeyWorksAndIsShownExactlyOnce(t *testing.T) {
	e, admin := adminEnv(t)

	code, body := e.do("POST", "/v1/admin/orgs", `{"name":"acme"}`, admin, false)
	if code != 201 {
		t.Fatalf("create org: %d %s", code, body)
	}
	var org struct{ ID string }
	json.Unmarshal([]byte(body), &org)

	code, body = e.do("POST", "/v1/admin/projects", `{"org_id":"`+org.ID+`","name":"prod"}`, admin, false)
	if code != 201 {
		t.Fatalf("create project: %d %s", code, body)
	}

	code, body = e.do("POST", "/v1/admin/keys", `{"org_id":"`+org.ID+`"}`, admin, false)
	if code != 201 {
		t.Fatalf("mint key: %d %s", code, body)
	}
	var minted struct{ ID, Key string }
	json.Unmarshal([]byte(body), &minted)
	if !strings.HasPrefix(minted.Key, "sk_") || len(minted.Key) < 40 {
		t.Fatalf("minted key looks weak: %q", minted.Key)
	}

	// The secret must never appear again.
	code, body = e.do("GET", "/v1/admin/keys?org_id="+org.ID, "", admin, false)
	if code != 200 {
		t.Fatalf("list keys: %d %s", code, body)
	}
	if strings.Contains(body, minted.Key) {
		t.Fatal("the key listing returned the secret; it must be shown only at creation")
	}
	if !strings.Contains(body, minted.ID) {
		t.Fatalf("listing does not include the key we just made: %s", body)
	}

	// The minted credential must actually authenticate.
	code, body = e.do("GET", "/v1/sandboxes", "", minted.Key, false)
	if code != 200 {
		t.Fatalf("minted key does not authenticate: %d %s", code, body)
	}

	// And revocation must take effect immediately.
	if code, body = e.do("DELETE", "/v1/admin/keys/"+minted.ID, "", admin, false); code != 204 {
		t.Fatalf("revoke: %d %s", code, body)
	}
	if code, body = e.do("GET", "/v1/sandboxes", "", minted.Key, false); code != 401 {
		t.Fatalf("revoked key still works: %d %s", code, body)
	}
}

// TestProjectKeyCannotBeMintedAcrossOrgs: pairing an org with another org's
// project would produce a credential that reads across the tenant boundary.
func TestProjectKeyCannotBeMintedAcrossOrgs(t *testing.T) {
	e, admin := adminEnv(t)
	mk := func(name string) (orgID, projID string) {
		_, b := e.do("POST", "/v1/admin/orgs", `{"name":"`+name+`"}`, admin, false)
		var o struct{ ID string }
		json.Unmarshal([]byte(b), &o)
		_, b = e.do("POST", "/v1/admin/projects", `{"org_id":"`+o.ID+`","name":"p"}`, admin, false)
		var p struct{ ID string }
		json.Unmarshal([]byte(b), &p)
		return o.ID, p.ID
	}
	orgA, _ := mk("a")
	_, projB := mk("b")

	code, body := e.do("POST", "/v1/admin/keys",
		`{"org_id":"`+orgA+`","project_id":"`+projB+`"}`, admin, false)
	if code < 400 {
		t.Fatalf("minted a key for org A scoped to org B's project: %d %s", code, body)
	}
}
