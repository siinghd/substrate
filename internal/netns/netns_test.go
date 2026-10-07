package netns

import (
	"net"
	"strings"
	"testing"
)

func TestAllocatorExhaustionAndRelease(t *testing.T) {
	a, err := NewAllocator("100.64.0.0/28")
	if err != nil {
		t.Fatal(err)
	}
	var ips []net.IP
	// A /28 is 16 addresses = four /30s; slot 0 is reserved, so three
	// sandboxes. One sandbox costs four addresses now, not one.
	for i := 0; i < 3; i++ {
		sub, err := a.Allocate()
		if err != nil {
			t.Fatalf("alloc %d: %v", i, err)
		}
		if !a.InBlock(sub.GuestIP) {
			t.Fatalf("%s not in block", sub.GuestIP)
		}
		ips = append(ips, sub.GuestIP)
	}
	if _, err := a.Allocate(); err == nil {
		t.Fatal("expected exhaustion")
	}
	a.Release(ips[1])
	if _, err := a.Allocate(); err != nil {
		t.Fatalf("reuse after release: %v", err)
	}
}

func TestSetupTeardownDryRun(t *testing.T) {
	rec := NewRecorder()
	mgr := &Manager{
		run:       rec,
		alloc:     mustAlloc(t),
		blockCIDR: "100.64.0.0/22",
		withTap:   true, // hv topology: veth uplink plus a tap for the VM
		sandboxes: map[string]*SandboxNet{},
	}

	n, err := mgr.Setup("sb_test123456789")
	if err != nil {
		t.Fatal(err)
	}
	if n.GuestIP == nil || n.TapName == "" || n.NSName != "sb-sb_test123456789" {
		t.Fatalf("net struct wrong: %+v", n)
	}

	cmds := rec.Commands()
	if len(cmds) == 0 || !strings.HasPrefix(cmds[0], "ip netns add") {
		t.Fatalf("first command should create netns: %v", cmds[:2])
	}

	joined := strings.Join(cmds, "\n")
	// A /30 per sandbox, not a routed /32. gVisor's netstack cannot
	// source-select from a /32: the sandbox gets ICMP and answers "Network
	// unreachable" to every TCP connect and UDP send.
	if !strings.Contains(joined, "addr add "+n.Subnet.HostIP.String()+"/30 dev "+n.VethHost) {
		t.Fatalf("host end of the link has no address:\n%s", joined)
	}
	if !strings.Contains(joined, "addr add "+n.Subnet.GuestIP.String()+"/30 dev "+n.VethPeer) {
		t.Fatalf("guest end of the link has no address:\n%s", joined)
	}
	if !strings.Contains(joined, "route add default via "+n.Subnet.HostIP.String()) {
		t.Fatalf("sandbox has no default route:\n%s", joined)
	}
	if !strings.Contains(joined, "169.254.0.1/30 dev "+n.TapName) {
		t.Fatalf("tap gateway missing")
	}
	if !strings.Contains(joined, "netns add "+n.NSName) {
		t.Fatalf("netns name mismatch: %s", joined)
	}
	// The policy must attach to the HOST-SIDE interface. Matching the tap
	// (which lives inside the sandbox netns) meant no host-table rule could
	// ever fire, so the whole egress policy was inert.
	if strings.Contains(joined, `iifname "`+n.TapName+`"`) {
		t.Fatalf("policy matches the tap, which is not visible in the host namespace:\n%s", joined)
	}
	chain := SandboxChain(n.VethHost)
	if !strings.Contains(joined, `"`+n.VethHost+`" : jump `+chain) {
		t.Fatalf("policy is never dispatched from the host-side interface:\n%s", joined)
	}
	foundDrop := false
	for _, c := range cmds {
		if strings.HasPrefix(c, "nft add rule inet "+Table+" "+chain) && strings.HasSuffix(c, " drop") {
			foundDrop = true
		}
	}
	if !foundDrop {
		t.Fatalf("per-sandbox chain has no drop rule:\n%s", joined)
	}

	if _, ok := mgr.Get("sb_test123456789"); !ok {
		t.Fatal("setup did not register sandbox net")
	}

	rec.log = nil
	if err := mgr.Teardown("sb_test123456789"); err != nil {
		t.Fatal(err)
	}
	teardown := rec.Commands()
	// The namespace and the host end of the veth both have to go. The host
	// veth carries the /30, so deleting it releases the addressing too --
	// there is no separate host route to remove any more.
	if !contains(teardown, "ip netns del sb-sb_test123456789") || !contains(teardown, "ip link del "+n.VethHost) {
		t.Fatalf("teardown incomplete: %v", teardown)
	}
	// And the firewall policy must go with it. Per-sandbox rules used to be
	// appended to the shared forward chain and never removed, so they outlived
	// the sandbox and a later one reusing the interface name inherited them.
	joinedDown := strings.Join(teardown, "\n")
	if !strings.Contains(joinedDown, "delete chain inet "+Table+" "+SandboxChain(n.VethHost)) {
		t.Fatalf("teardown leaves the sandbox's firewall chain behind:\n%s", joinedDown)
	}
}

// TestSetupRollbackRemovesEverything: Setup's error path released only the IP,
// leaving the netns, tap, veth pair, host route and firewall chain behind --
// and there is no orphan sweeper anywhere, so a node that hit a transient
// failure accumulated dead namespaces until names or addresses ran out.
func TestSetupRollbackRemovesEverything(t *testing.T) {
	rec := NewRecorder()
	mgr := &Manager{run: rec, alloc: mustAlloc(t), blockCIDR: "100.64.0.0/22", withTap: true, sandboxes: map[string]*SandboxNet{}}
	// Fail at the very last step, so everything before it exists.
	rec.fail["ip -n sb-sb_test123456789 route add default via 100.64.0.5"] = true

	if _, err := mgr.Setup("sb_test123456789"); err == nil {
		t.Fatal("expected injected failure")
	}
	joined := strings.Join(rec.Commands(), "\n")
	for _, want := range []string{
		"ip netns del sb-sb_test123456789",
		"ip link del vb23456789",
		"nft delete chain inet " + Table + " sb_23456789",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("rollback did not run %q; the resource leaks:\n%s", want, joined)
		}
	}
	if mgr.alloc.Used() != 0 {
		t.Errorf("IP leaked on rollback: %d in use", mgr.alloc.Used())
	}
	if _, ok := mgr.Get("sb_test123456789"); ok {
		t.Error("a failed Setup registered the sandbox anyway")
	}
}

func TestSetupRollbackOnFailure(t *testing.T) {
	rec := NewRecorder()
	mgr := &Manager{run: rec, alloc: mustAlloc(t), sandboxes: map[string]*SandboxNet{}}
	rec.fail["ip link add vb23456789 type veth peer name vp23456789"] = true
	before := mgr.alloc.Used()
	if _, err := mgr.Setup("sb_test123456789"); err == nil {
		t.Fatal("expected injected failure")
	}
	if after := mgr.alloc.Used(); after != before {
		t.Fatalf("IP leaked on rollback: %d -> %d", before, after)
	}
}

func mustAlloc(t *testing.T) *Allocator {
	t.Helper()
	a, err := NewAllocator("100.64.0.0/22")
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// TestBaseRulesetOrdering: the IMDS drop must be the FIRST rule appended to the
// forward chain, whatever setup commands precede it. Indexing a fixed position
// made this test break the moment anything was added before it — and, worse, it
// would have kept passing if a rule were inserted between the chain creation
// and the drop.
func TestBaseRulesetOrdering(t *testing.T) {
	rs := BaseRuleset()
	var firstForwardRule []string
	for _, c := range rs {
		if len(c) >= 5 && c[0] == "add" && c[1] == "rule" && c[4] == "forward" {
			firstForwardRule = c
			break
		}
	}
	if firstForwardRule == nil {
		t.Fatal("base ruleset appends no rule to the forward chain at all")
	}
	joined := strings.Join(firstForwardRule, " ")
	if !strings.Contains(joined, "169.254.0.0/16") || !strings.Contains(joined, "drop") {
		t.Fatalf("IMDS drop must be the first forward rule; got %q", joined)
	}
}

// TestBaseRulesetCoversIPv6: the table is `inet`, so it sees both families, but
// every filter used to be an IPv4 `ip daddr` match and the accepts had no
// family qualifier — v6 traffic passed unfiltered, metadata addresses included.
func TestBaseRulesetCoversIPv6(t *testing.T) {
	joined := ""
	for _, c := range BaseRuleset() {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"ip6 daddr fe80::/10 drop", "ip6 daddr fd00:ec2::/64 drop"} {
		if !strings.Contains(joined, want) {
			t.Errorf("base ruleset is missing %q; IPv6 forwarding is unfiltered", want)
		}
	}
}

// TestEgressRulesTargetTheHostSideInterface: the tap lives inside the sandbox
// netns, so a host-table `iifname "tap…"` match can never fire. Matching the
// tap made the ENTIRE per-sandbox policy inert.
func TestEgressRulesTargetTheHostSideInterface(t *testing.T) {
	rules := EgressRules("vbabcdef12", "100.64.0.7", "100.64.0.0/22")
	joined := ""
	for _, c := range rules {
		joined += strings.Join(c, " ") + "\n"
	}
	if strings.Contains(joined, "tap") {
		t.Errorf("policy still references a tap interface, which is not visible in the host namespace:\n%s", joined)
	}
	if !strings.Contains(joined, `"vbabcdef12" : jump sb_abcdef12`) {
		t.Errorf("policy is never dispatched from the host-side interface:\n%s", joined)
	}
}

// TestEgressRulesEnforceIsolation pins the three properties the old rules did
// not actually provide.
func TestEgressRulesEnforceIsolation(t *testing.T) {
	const block = "100.64.0.0/22"
	rules := EgressRules("vbabcdef12", "100.64.0.7", block)

	var chainRules []string
	for _, c := range rules {
		if len(c) >= 5 && c[0] == "add" && c[1] == "rule" {
			chainRules = append(chainRules, strings.Join(c[5:], " "))
		}
	}
	idx := func(sub string) int {
		for i, r := range chainRules {
			if strings.Contains(r, sub) {
				return i
			}
		}
		return -1
	}

	spoof := idx("saddr != 100.64.0.7")
	if spoof < 0 {
		t.Error("no anti-spoof rule: a guest can forge its source address and sidestep every address-based check")
	}
	if spoof != 0 {
		t.Errorf("anti-spoof is rule %d, must be first (the rules below it all reason about addresses)", spoof)
	}

	peer := idx("daddr " + block)
	if peer < 0 {
		t.Error("no peer-isolation rule: sandboxes can reach each other")
	}
	if v6 := idx("nfproto ipv6"); v6 < 0 {
		t.Error("IPv6 is not dropped from the sandbox: it bypasses every IPv4 filter below")
	}

	// The drops must precede the rate-limited accept, or they never apply.
	accept := idx("limit rate")
	if accept < 0 {
		t.Fatal("no rate-limited accept")
	}
	if peer > accept {
		t.Errorf("peer isolation (rule %d) comes after the accept (rule %d): it never applies", peer, accept)
	}
	if last := chainRules[len(chainRules)-1]; !strings.HasSuffix(last, "drop") {
		t.Errorf("the sandbox chain does not end in a default drop; got %q", last)
	}
}

// TestTeardownRulesRemoveEverything: per-sandbox rules used to be appended to
// the shared forward chain and never removed.
func TestTeardownRulesRemoveEverything(t *testing.T) {
	joined := ""
	for _, c := range TeardownRules("vbabcdef12") {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{"delete element", "delete chain inet substrate sb_abcdef12"} {
		if !strings.Contains(joined, want) {
			t.Errorf("teardown is missing %q; the policy outlives the sandbox:\n%s", want, joined)
		}
	}
	// Unhook before emptying, or a packet can reach a half-flushed chain.
	if strings.Index(joined, "delete element") > strings.Index(joined, "flush chain") {
		t.Error("teardown flushes the chain before unhooking the dispatch")
	}
}

// TestSecurityDropsAreCounted: a silent drop is indistinguishable from a rule
// that never matches. That is exactly how a policy attached to an interface the
// host cannot see went unnoticed — nothing anywhere reported whether it had
// ever fired.
func TestSecurityDropsAreCounted(t *testing.T) {
	joined := ""
	for _, c := range BaseRuleset() {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, c := range EgressRules("vbabcdef12", "100.64.0.7", "100.64.0.0/22") {
		joined += strings.Join(c, " ") + "\n"
	}
	for _, want := range []string{
		"169.254.0.0/16 counter drop",
		"saddr != 100.64.0.7 counter drop",
		"nfproto ipv6 counter drop",
		"daddr 100.64.0.0/22 counter drop",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("security drop %q carries no counter, so nobody can tell whether it ever fires", want)
		}
	}
}
