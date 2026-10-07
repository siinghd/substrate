package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/substrate-sh/substrate/internal/store"
)

func limitsEnv(t *testing.T) (*attackEnv, string) {
	t.Helper()
	e := newAttackEnv(t)
	e.st.PutTemplate(t.Context(), &store.Template{
		ID: "tpl_limits00001", ProjectID: "prj_1", Name: "lim", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow(),
	})
	return e, "tpl_limits00001"
}

// TestResourceRequestsAreBounded: mem_mib becomes a cgroup memory ceiling and
// vcpus a CPU share, and neither had ANY bound anywhere in the stack. One
// request could ask for terabytes of memory or thousands of vCPUs and the node
// would try to honour it — a denial of service from a single API call, on a
// host packing 2,000 sandboxes.
func TestResourceRequestsAreBounded(t *testing.T) {
	e, tpl := limitsEnv(t)
	for _, tc := range []struct{ name, body, want string }{
		{"absurd memory", `{"template_id":"` + tpl + `","mem_mib":1073741824}`, "mem_mib"},
		{"absurd vcpus", `{"template_id":"` + tpl + `","vcpus":100000}`, "vcpus"},
		{"absurd absolute ttl", `{"template_id":"` + tpl + `","ttl_absolute_secs":999999999}`, "ttl_absolute_secs"},
		// time.Duration is int64 NANOseconds: above ~9.2e9 seconds the
		// multiply wraps NEGATIVE, which flips the idle-ttl comparison to
		// always-true and pauses the sandbox immediately. A value that means
		// the opposite of what was asked for is a bug, not a limit.
		{"idle ttl that wraps time.Duration", `{"template_id":"` + tpl + `","ttl_idle_secs":9223372036}`, "ttl_idle_secs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// idem=true: an org key MUST send an Idempotency-Key, and without
			// one the request is refused with 400 before the limit check ever
			// runs -- which made the first version of this test pass on the
			// wrong 4xx. Non-2xx releases the claim, so the shared key is fine.
			code, body := e.do("POST", "/v1/sandboxes", tc.body, e.apiKey, true)
			if code < 400 {
				t.Fatalf("accepted with %d: %s\na single request just claimed an unbounded resource", code, body)
			}
			if strings.Contains(body, "idempotency_key_required") {
				t.Fatalf("rejected for the wrong reason, not the limit: %s", body)
			}
			if !strings.Contains(body, tc.want) {
				t.Errorf("refusal does not name the offending field %q: %s", tc.want, body)
			}
		})
	}
}

// TestNegativeResourceRequestsRejected: a negative value reaches the same
// limits and is not a meaningful request.
func TestNegativeResourceRequestsRejected(t *testing.T) {
	e, tpl := limitsEnv(t)
	for _, body := range []string{
		`{"template_id":"` + tpl + `","mem_mib":-1}`,
		`{"template_id":"` + tpl + `","vcpus":-4}`,
		`{"template_id":"` + tpl + `","ttl_absolute_secs":-10}`,
		`{"template_id":"` + tpl + `","ttl_idle_secs":-10}`,
	} {
		code, resp := e.do("POST", "/v1/sandboxes", body, e.apiKey, true)
		if code < 400 {
			t.Errorf("negative value accepted with %d: %s -> %s", code, body, resp)
		}
		if strings.Contains(resp, "idempotency_key_required") {
			t.Fatalf("rejected for the wrong reason (missing idempotency key), not the negative value: %s", resp)
		}
	}
}

// TestResourceRequestsWithinLimitsStillWork keeps the ceiling from becoming a
// wall for legitimate callers.
func TestResourceRequestsWithinLimitsStillWork(t *testing.T) {
	e, tpl := limitsEnv(t)
	code, body := e.do("POST", "/v1/sandboxes",
		fmt.Sprintf(`{"template_id":"%s","mem_mib":1024,"vcpus":2,"ttl_absolute_secs":3600}`, tpl),
		e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("a reasonable request was rejected: %d %s", code, body)
	}
}
