package api

// Pass-4 security tests: every test here ATTEMPTS a real attack against the
// full in-process server and asserts it fails. Findings fixed this pass:
//
//   - cross-org listing via trusted ?project_id= (sandboxes/templates/usage)
//   - cross-org exec read/stream/stdin/kill (loadExec had no tenant check)
//   - sandbox tokens registering templates with arbitrary host paths
//   - template path traversal shapes

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/store"
)

// twoOrgEnv extends attackEnv with a victim org whose data must stay
// invisible to org_1 credentials.
type twoOrgEnv struct {
	*attackEnv
	org2Key string
	org1Key string // org-grade credential for org_1 (the attacker)
	prj2    string
}

func newTwoOrgEnv(t *testing.T) *twoOrgEnv {
	e := newAttackEnv(t)
	ctx := context.Background()
	now := timeNow()
	prj2 := "prj_2"
	org2Key := "sk_org2_key_0000000000"
	e.st.CreateOrg(ctx, &store.Org{ID: "org_2", Name: "victim", CreatedAt: now})
	e.st.CreateProject(ctx, &store.Project{ID: prj2, OrgID: "org_2", Name: "victim-p", MaxSandboxes: 10, CreatedAt: now})
	e.st.CreateAPIKey(ctx, &store.APIKey{ID: "k2", OrgID: "org_2", ProjectID: prj2, Kind: "project",
		KeyHash: authHashKey(org2Key), CreatedAt: now})
	org1Key := "sk_org1_key_0000000000"
	e.st.CreateAPIKey(ctx, &store.APIKey{ID: "k1org", OrgID: "org_1", Kind: "org",
		KeyHash: authHashKey(org1Key), CreatedAt: now})
	return &twoOrgEnv{attackEnv: e, org2Key: org2Key, org1Key: org1Key, prj2: prj2}
}

func TestAttackCrossOrgProjectListing(t *testing.T) {
	e := newTwoOrgEnv(t)
	ctx := context.Background()

	// Victim seeds a template and a sandbox inside prj_2.
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_victim00001", ProjectID: e.prj2, Name: "victim-tpl", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow()})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_victim00001","external_user_id":"victim-user"}`, e.org2Key, true)
	if code != 201 && code != 202 {
		t.Fatalf("victim create failed: %d %s", code, body)
	}

	// Attack 1: org_1 enumerates sandboxes by naming the victim project.
	code, body = e.do("GET", "/v1/sandboxes?project_id="+e.prj2, "", e.org1Key, false)
	if code < 400 {
		t.Fatalf("cross-org sandbox listing returned %d (expected rejection)", code)
	}
	if strings.Contains(body, "sb_") || strings.Contains(body, "victim") {
		t.Fatalf("cross-org sandbox listing leaked victim data: %s", body)
	}

	// Attack 1b: a project-grade key is pinned to its own project — it must
	// never receive victim rows either.
	code, body = e.do("GET", "/v1/sandboxes?project_id="+e.prj2, "", e.apiKey, false)
	if code >= 400 || strings.Contains(body, "victim") {
		t.Fatalf("project-key listing escaped scope: %d %s", code, body)
	}

	// Attack 2: same vector on templates.
	code, body = e.do("GET", "/v1/templates?project_id="+e.prj2, "", e.org1Key, false)
	if code < 400 && strings.Contains(body, "victim-tpl") {
		t.Fatalf("cross-org template listing leaked victim templates (%d): %s", code, body)
	}

	// Attack 3: same vector on usage rollups.
	code, body = e.do("GET", "/v1/usage?group_by=external_user_id&project_id="+e.prj2, "", e.org1Key, false)
	if code < 400 && strings.Contains(body, "victim-user") {
		t.Fatalf("cross-org usage leaked victim rows (%d): %s", code, body)
	}

	// Control: the victim can still see its own data.
	code, body = e.do("GET", "/v1/sandboxes?project_id="+e.prj2, "", e.org2Key, false)
	if code != 200 || !strings.Contains(body, "sb_") {
		t.Fatalf("legitimate owner lost access after fix: %d %s", code, body)
	}
}

func TestAttackCrossOrgExecAccess(t *testing.T) {
	e := newTwoOrgEnv(t)
	ctx := context.Background()

	// A sandbox owned by org_1 with an exec row in terminal state (so a
	// regressed stream authz would return instantly with the payload rather
	// than hang the test on a live SSE loop).
	logFile := filepath.Join(t.TempDir(), "ex.log")
	os.WriteFile(logFile, []byte("secret-output"), 0o644)
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_o1000000001", ProjectID: "prj_1", Name: "t1", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow()})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_o1000000001"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create failed: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	fin := timeNow()
	ex := &store.Exec{ID: "ex_victim000001", SandboxID: sbID, Argv: []string{"sh", "-c", "echo secret-output"},
		State: "exited", ExitCode: 0, FinishedAt: &fin, LogPath: logFile, CreatedAt: timeNow()}
	if err := e.st.CreateExec(ctx, ex); err != nil {
		t.Fatal(err)
	}

	// Attack: another org's key reads / streams / feeds / kills it.
	for _, tc := range []struct{ name, method, path, body string }{
		{"read", "GET", "/v1/execs/ex_victim000001", ""},
		{"stream", "GET", "/v1/execs/ex_victim000001/stream", ""},
		{"stdin", "POST", "/v1/execs/ex_victim000001/stdin", "evil-input"},
		{"kill", "DELETE", "/v1/execs/ex_victim000001", ""},
	} {
		code, body := e.do(tc.method, tc.path, tc.body, e.org2Key, false)
		if code == 200 || code == 202 {
			t.Fatalf("cross-org exec %s succeeded (%d)", tc.name, code)
		}
		if strings.Contains(body, "secret-output") {
			t.Fatalf("cross-org exec %s leaked output", tc.name)
		}
	}

	// Control: the owning org still reads its own exec.
	code, _ = e.do("GET", "/v1/execs/ex_victim000001", "", e.apiKey, false)
	if code != 200 {
		t.Fatalf("owner lost exec read access: %d", code)
	}
}

func TestAttackSandboxTokenRegistersTemplateWithHostPaths(t *testing.T) {
	e := newTwoOrgEnv(t)

	e.st.PutTemplate(context.Background(), &store.Template{ID: "tpl_o1000000009", ProjectID: "prj_1", Name: "t9", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow()})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_o1000000009"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create failed: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	tok, err := auth.MintSandboxToken(e.cfg.API.JWTSecret, "substrate", sbID,
		[]string{"exec", "fs", "ports"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	// End-user agent tries to plant a template pointing at host files.
	for _, paths := range [][2]string{
		{"/etc", "/boot/vmlinux"},
		{"", "../../etc/shadow"},
	} {
		payload := `{"name":"pwned","rootfs_path":"` + paths[0] + `","kernel_path":"` + paths[1] + `"}`
		code, body := e.do("POST", "/v1/templates", payload, tok, true)
		if code == 201 {
			t.Fatalf("sandbox token registered template with host paths %v: %s", paths, body)
		}
	}

	// Path traversal shapes are rejected even for legitimate keys.
	for _, p := range []string{"relative/rootfs", "/abs/../escape", "/with/../.."} {
		payload := `{"name":"t","rootfs_path":"` + p + `"}`
		code, _ := e.do("POST", "/v1/templates", payload, e.apiKey, true)
		if code == 201 {
			t.Fatalf("template path %q accepted", p)
		}
	}

	// Control: operator key registers a clean absolute path.
	code, body = e.do("POST", "/v1/templates", `{"name":"ok","rootfs_path":"/var/lib/substrate/artifacts/rootfs.ext4"}`, e.apiKey, true)
	if code != 201 {
		t.Fatalf("clean template rejected: %d %s", code, body)
	}
}

// TestAttackCrossOrgUsageRollupUnscoped: /v1/usage with NO project filter
// must still be org-scoped. UsageRollup used to discard f.OrgID entirely, so
// an org key grouping by external_user_id received every org's metering rows.
func TestAttackCrossOrgUsageRollupUnscoped(t *testing.T) {
	e := newTwoOrgEnv(t)
	ctx := context.Background()
	now := timeNow()

	// Victim org metering rows (no explicit project filter will be sent).
	e.st.InsertUsageSample(ctx, store.UsageSample{
		SandboxID: "sb_victim000001", TS: now, CPUUS: 5_000_000,
		MemPeakKB: 99999, Running: true, ProjectID: e.prj2, ExternalUserID: "victim-user",
	})
	// Attacker org row so the control case has something to find.
	e.st.InsertUsageSample(ctx, store.UsageSample{
		SandboxID: "sb_attacker0001", TS: now, CPUUS: 1_000_000,
		MemPeakKB: 11111, Running: true, ProjectID: "prj_1", ExternalUserID: "attacker-user",
	})

	code, body := e.do("GET", "/v1/usage?group_by=external_user_id", "", e.org1Key, false)
	if code != 200 {
		t.Fatalf("usage read failed: %d %s", code, body)
	}
	if strings.Contains(body, "victim-user") || strings.Contains(body, "sb_victim") {
		t.Fatalf("unscoped usage rollup leaked victim rows: %s", body)
	}
	if !strings.Contains(body, "attacker-user") {
		t.Fatalf("attacker's own rows missing from rollup: %s", body)
	}

	// Same check grouped by sandbox_id.
	code, body = e.do("GET", "/v1/usage?group_by=sandbox_id", "", e.org1Key, false)
	if code != 200 {
		t.Fatalf("usage read failed: %d %s", code, body)
	}
	if strings.Contains(body, "sb_victim000001") {
		t.Fatalf("sandbox-grouped usage leaked victim sandbox: %s", body)
	}

	// Control: the victim's own project key still sees its rows.
	code, body = e.do("GET", "/v1/usage?group_by=external_user_id", "", e.org2Key, false)
	if code != 200 || !strings.Contains(body, "victim-user") {
		t.Fatalf("victim lost access to own usage: %d %s", code, body)
	}
}
