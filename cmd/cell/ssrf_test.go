package main

import (
	"strings"
	"testing"
)

// TestValidateNodeAddrSSRFGuard asserts the DOCUMENTED deployment contract:
// cells and nodes share one trusted VPC, so RFC1918 and loopback node
// addresses are legitimate; what must always fail is metadata/link-local
// space, non-literal hosts, and unspecified addresses (the shapes a hostile
// registration would use to reach IMDS or dodge the literal-IP rule).
func TestValidateNodeAddrSSRFGuard(t *testing.T) {
	attacks := []struct {
		addr string
		why  string
	}{
		{"169.254.169.254:80", "cloud metadata (IMDS)"},
		{"169.254.1.1:8080", "link-local range"},
		{"0.0.0.0:8080", "unspecified"},
		{"fe80::1:8080", "ipv6 link-local"},
		{"::ffff:169.254.169.254:80", "v4-mapped metadata"},
		{"example.com:8080", "non-literal host (DNS rebinding)"},
		{"1.2.3.4", "missing port"},
		{"", "empty"},
	}
	for _, c := range attacks {
		t.Run(c.why, func(t *testing.T) {
			if err := validateNodeAddr(c.addr); err == nil {
				t.Fatalf("%s (%s) accepted — SSRF guard failed", c.addr, c.why)
			}
		})
	}

	legit := []string{
		"52.1.2.3:8080",        // public internet node
		"10.0.0.5:8080",        // VPC node (normal hosted topology)
		"192.168.1.10:9090",    // private lab node
		"127.0.0.1:8080",       // same-host dev cell
		"[2606:4700::1111]:80", // public IPv6
	}
	for _, addr := range legit {
		if err := validateNodeAddr(addr); err != nil {
			t.Fatalf("legitimate address %s rejected: %v", addr, err)
		}
	}
}

// TestResolveNodeKeyRefusesDefault pins the credential policy: the
// source-committed default must never activate without an explicit dev opt-in.
func TestResolveNodeKeyRefusesDefault(t *testing.T) {
	t.Setenv("SUBSTRATE_NODE_API_KEY", "")
	t.Setenv("SUBSTRATE_INSECURE_DEV", "")
	if key := resolveNodeKey(); key != "" {
		t.Fatalf("default node key leaked in production mode: %q", key)
	}

	t.Setenv("SUBSTRATE_INSECURE_DEV", "1")
	key := resolveNodeKey()
	if !strings.HasPrefix(key, "sk_") {
		t.Fatalf("dev fallback malformed: %q", key)
	}

	t.Setenv("SUBSTRATE_NODE_API_KEY", "sk_operator_supplied_value")
	if key := resolveNodeKey(); key != "sk_operator_supplied_value" {
		t.Fatalf("operator key overridden: %q", key)
	}
}
