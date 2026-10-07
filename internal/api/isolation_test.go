package api

import (
	"strings"
	"testing"

	"github.com/substrate-sh/substrate/internal/store"
)

// TestIsolationTierIsNeverSilentlyDowngraded: the node's create handler had no
// `isolation` field at all, so a request for "hv" was dropped and the sandbox
// came back stamped with whatever tier the node happened to run — on a KVM-less
// box, a bare host process with the agent's own uid, able to read /etc/passwd
// and see every host process. The response said 201.
//
// openapi.yaml documents the field as "Never silently downgraded (plan §2.4)"
// and the cell's scheduler already reads it, so the node was the single link in
// the chain that ignored it.
func TestIsolationTierIsNeverSilentlyDowngraded(t *testing.T) {
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_isolation01", ProjectID: "prj_1", Name: "iso", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})

	// This env runs the process driver, so "hv" cannot be honoured.
	code, body := e.do("POST", "/v1/sandboxes",
		`{"template_id":"tpl_isolation01","isolation":"hv"}`, e.apiKey, true)
	if code < 400 {
		t.Fatalf("a request for hv isolation returned %d on a process-tier node: %s\n"+
			"the caller believes it got hardware virtualisation and did not", code, body)
	}
	if !strings.Contains(body, "isolation_unavailable") {
		t.Errorf("refusal does not name the cause: %s", body)
	}
	// The error must say what the node actually offers, or the caller cannot act.
	if !strings.Contains(body, "process") {
		t.Errorf("refusal does not report the tier actually available: %s", body)
	}
}

// TestIsolationTierHonouredWhenItMatches: asking for the tier the node has must
// still work, and the response must report it.
func TestIsolationTierHonouredWhenItMatches(t *testing.T) {
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_isolation02", ProjectID: "prj_1", Name: "iso2", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	code, body := e.do("POST", "/v1/sandboxes",
		`{"template_id":"tpl_isolation02","isolation":"process"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("matching tier rejected: %d %s", code, body)
	}
	if !strings.Contains(body, `"isolation":"process"`) {
		t.Fatalf("response does not report the tier: %s", body)
	}
}

// TestIsolationTierRejectsUnknownValue keeps the enum honest.
func TestIsolationTierRejectsUnknownValue(t *testing.T) {
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_isolation03", ProjectID: "prj_1", Name: "iso3", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	for _, tier := range []string{"HV", "vm", "none", "hv "} {
		code, body := e.do("POST", "/v1/sandboxes",
			`{"template_id":"tpl_isolation03","isolation":"`+tier+`"}`, e.apiKey, false)
		if code < 400 {
			t.Errorf("unknown isolation %q accepted with %d: %s", tier, code, body)
		}
	}
}

// TestOmittedIsolationStillWorks: the field is optional; omitting it keeps the
// existing behaviour of taking the node's tier.
func TestOmittedIsolationStillWorks(t *testing.T) {
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_isolation04", ProjectID: "prj_1", Name: "iso4", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_isolation04"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("omitting isolation broke create: %d %s", code, body)
	}
}
