package main

import (
	"net/http"
	"testing"
)

// TestValidateNodeAddr: the SSRF guard must reject metadata/link-local space
// and non-literal hosts — nodes register where traffic will be SENT.
func TestValidateNodeAddr(t *testing.T) {
	cases := []struct {
		addr string
		ok   bool
	}{
		{"10.0.0.5:8080", true},
		{"192.168.1.10:9090", true},
		{"169.254.169.254:80", false}, // cloud metadata
		{"127.0.0.1:8080", true},      // loopback allowed: same-host dev cells
		{"[::1]:8080", true},
		{"fe80::1%eth0:8080", false},
		{"node.internal:8080", false}, // hostnames refused in v1 (DNS rebinding)
		{"no-port-here", false},
		{"", false},
	}
	for _, c := range cases {
		err := validateNodeAddr(c.addr)
		if c.ok && err != nil {
			t.Fatalf("addr %q rejected: %v", c.addr, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("addr %q accepted", c.addr)
		}
	}
}

// TestRemoteIPIdentity: buckets keyed by peer IP with stable prefix.
func TestRemoteIPIdentity(t *testing.T) {
	req, _ := http.NewRequest("POST", "/v1/node/enroll", nil)
	req.RemoteAddr = "203.0.113.7:55555"
	got := remoteIPIdentity(req)
	if got != "ip_203.0.113.7" {
		t.Fatalf("identity = %q", got)
	}
	req.RemoteAddr = "garbage"
	if got := remoteIPIdentity(req); got != "ip_garbage" {
		t.Fatalf("fallback identity = %q", got)
	}
}
