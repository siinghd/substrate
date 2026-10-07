package main

import (
	"net/url"
	"testing"
)

// TestContainedSandboxPath: the cell forwards to nodes with a privileged node
// credential, so a caller that escapes /v1/sandboxes/{id} reaches any route on
// the node as the cell. r.URL.Path is DECODED, so %2e%2e%2f arrives as "../",
// and the ServeMux's cleanPath runs on the ESCAPED path and never sees it.
func TestContainedSandboxPath(t *testing.T) {
	const id = "sb_0123456789abc"
	base := "/v1/sandboxes/" + id

	ok := []struct{ in, want string }{
		{base, base},
		{base + "/exec", base + "/exec"},
		{base + "/fs/etc/hosts", base + "/fs/etc/hosts"},
		{base + "/fs/a/../b", base + "/fs/b"},
		{base + "/ports/8080", base + "/ports/8080"},
		{base + "/", base},
	}
	for _, tc := range ok {
		got, allowed := containedSandboxPath(id, tc.in)
		if !allowed {
			t.Errorf("legitimate path %q was rejected", tc.in)
			continue
		}
		if got != tc.want {
			t.Errorf("containedSandboxPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// Every one of these escapes the sandbox and must be refused.
	bad := []string{
		base + "/../../../v1/sandboxes",
		base + "/../" + "sb_ffffffffffff" + "/exec",
		base + "/..",
		base + "/../..",
		base + "/fs/../../../../etc/passwd",
		"/v1/sandboxes/sb_otherrrrrrrr/exec",
		"/v1/nodes",
		base + "/exec\x00/../../v1",
	}
	for _, in := range bad {
		if got, allowed := containedSandboxPath(id, in); allowed {
			t.Errorf("TRAVERSAL: %q was forwarded as %q with the privileged node credential", in, got)
		}
	}
}

// TestForwardTargetRejectsEncodedTraversal drives the exact computation the
// handler performs, on URLs parsed the way net/http parses them — so the
// %2e%2e%2f decoding that defeats the ServeMux's cleanPath is part of the test.
func TestForwardTargetRejectsEncodedTraversal(t *testing.T) {
	const id = "sb_0123456789abc"
	for _, raw := range []string{
		"/v1/sandboxes/" + id + "/..%2f..%2f..%2fv1%2fsandboxes",
		"/v1/sandboxes/" + id + "/%2e%2e%2f%2e%2e%2fv1%2fnodes",
		"/v1/sandboxes/" + id + "/../../../v1/sandboxes",
		"/v1/sandboxes/" + id + "/fs/%2e%2e%2f%2e%2e%2f%2e%2e%2fetc%2fpasswd",
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		if got, ok := forwardTarget(id, u); ok {
			t.Errorf("TRAVERSAL FORWARDED: %q -> %q, carrying the privileged node credential", raw, got)
		}
	}

	// And the ordinary paths must still work, query string intact.
	u, _ := url.Parse("/v1/sandboxes/" + id + "/fs/etc/hosts?offset=10&length=5")
	got, ok := forwardTarget(id, u)
	if !ok {
		t.Fatal("a legitimate fs read was rejected")
	}
	if got != "/v1/sandboxes/"+id+"/fs/etc/hosts?offset=10&length=5" {
		t.Fatalf("forwardTarget = %q", got)
	}
}
