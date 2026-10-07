//go:build linux

package netns

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These tests program REAL network namespaces and REAL nftables rules. Until
// now this package was only ever exercised through the Recorder, which asserts
// what commands we intended to run and nothing about whether the kernel accepts
// them or what they then do — which is how a policy that matched an interface
// invisible to the host namespace survived every test.
//
// Requires root. Skipped otherwise, and every test cleans up after itself.
func requireLiveNetns(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("live netns tests need root: run with sudo -E go test ./internal/netns -run Live")
	}
	for _, bin := range []string{"ip", "nft"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
}

const liveTable = "substrate"

func nftDump(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("nft", "list", "table", "inet", liveTable).CombinedOutput()
	if err != nil {
		return ""
	}
	return string(out)
}

func cleanupLive(t *testing.T, ids []string) {
	t.Helper()
	for _, id := range ids {
		exec.Command("ip", "netns", "del", "sb-"+id).Run()
		exec.Command("ip", "link", "del", "vb"+shortID(id)).Run()
	}
	exec.Command("nft", "delete", "table", "inet", liveTable).Run()
}

// TestLiveSetupProgramsTheKernel: the commands the manager emits must actually
// be accepted by the kernel, and produce the topology and policy we claim.
func TestLiveSetupProgramsTheKernel(t *testing.T) {
	requireLiveNetns(t)
	const id = "sb_live000000001"
	cleanupLive(t, []string{id})
	t.Cleanup(func() { cleanupLive(t, []string{id}) })

	// Base ruleset first, exactly as noded would.
	for _, rule := range BaseRuleset() {
		if out, err := exec.Command("nft", rule...).CombinedOutput(); err != nil {
			t.Fatalf("base rule %v rejected by the kernel: %v: %s", rule, err, out)
		}
	}

	mgr, err := NewManager("100.64.0.0/22", false)
	if err != nil {
		t.Fatal(err)
	}
	n, err := mgr.Setup(id)
	if err != nil {
		t.Fatalf("live Setup: %v", err)
	}

	// The namespace, the tap inside it, and the host-side veth must all exist.
	if out, err := exec.Command("ip", "netns", "list").CombinedOutput(); err != nil || !strings.Contains(string(out), n.NSName) {
		t.Fatalf("namespace %s not created: %v %s", n.NSName, err, out)
	}
	if out, err := exec.Command("ip", "-n", n.NSName, "link", "show", n.TapName).CombinedOutput(); err != nil {
		t.Fatalf("tap %s missing inside the namespace: %v %s", n.TapName, err, out)
	}
	if out, err := exec.Command("ip", "link", "show", n.VethHost).CombinedOutput(); err != nil {
		t.Fatalf("host veth %s missing: %v %s", n.VethHost, err, out)
	}

	// The policy must be live in the kernel, dispatched from the HOST-SIDE
	// interface, and the kernel must have accepted every rule.
	dump := nftDump(t)
	chain := SandboxChain(n.VethHost)
	if !strings.Contains(dump, "chain "+chain) {
		t.Fatalf("per-sandbox chain %s not in the kernel ruleset:\n%s", chain, dump)
	}
	if !strings.Contains(dump, n.VethHost) {
		t.Fatalf("dispatch for %s not present; the policy is unreachable:\n%s", n.VethHost, dump)
	}
	if strings.Contains(dump, n.TapName) {
		t.Fatalf("policy references the tap, which the host namespace cannot match on:\n%s", dump)
	}
	for _, want := range []string{"169.254.0.0/16", "100.64.0.0/22", n.GuestIP.String()} {
		if !strings.Contains(dump, want) {
			t.Errorf("kernel ruleset is missing %q:\n%s", want, dump)
		}
	}

	// Teardown must remove BOTH the namespace and the policy.
	if err := mgr.Teardown(id); err != nil {
		t.Fatalf("live Teardown: %v", err)
	}
	if out, _ := exec.Command("ip", "netns", "list").CombinedOutput(); strings.Contains(string(out), n.NSName) {
		t.Errorf("namespace survived teardown: %s", out)
	}
	after := nftDump(t)
	if strings.Contains(after, "chain "+chain) {
		t.Errorf("firewall chain survived teardown; policy outlives the sandbox:\n%s", after)
	}
	if strings.Contains(after, n.VethHost) {
		t.Errorf("dispatch element survived teardown:\n%s", after)
	}
}

// TestLiveRulesetSurvivesManySandboxes: teardown must be complete, or the
// shared chain grows for the life of the node.
func TestLiveRulesetSurvivesManySandboxes(t *testing.T) {
	requireLiveNetns(t)
	ids := []string{"sb_livemany00001", "sb_livemany00002", "sb_livemany00003"}
	cleanupLive(t, ids)
	t.Cleanup(func() { cleanupLive(t, ids) })

	for _, rule := range BaseRuleset() {
		if out, err := exec.Command("nft", rule...).CombinedOutput(); err != nil {
			t.Fatalf("base rule %v: %v %s", rule, err, out)
		}
	}
	mgr, err := NewManager("100.64.0.0/22", false)
	if err != nil {
		t.Fatal(err)
	}

	baseline := len(strings.Split(nftDump(t), "\n"))
	for cycle := 0; cycle < 3; cycle++ {
		for _, id := range ids {
			if _, err := mgr.Setup(id); err != nil {
				t.Fatalf("cycle %d setup %s: %v", cycle, id, err)
			}
		}
		for _, id := range ids {
			if err := mgr.Teardown(id); err != nil {
				t.Fatalf("cycle %d teardown %s: %v", cycle, id, err)
			}
		}
	}
	// After equal numbers of setups and teardowns the ruleset must be back to
	// its baseline size, not three cycles larger.
	final := len(strings.Split(nftDump(t), "\n"))
	if final > baseline {
		t.Fatalf("ruleset grew from %d to %d lines across 3 create/destroy cycles:\n%s",
			baseline, final, nftDump(t))
	}
}

// TestLiveIPAllocatorReleasesAddresses: addresses must be reusable across
// cycles, or a node exhausts its block.
func TestLiveIPAllocatorReleasesAddresses(t *testing.T) {
	requireLiveNetns(t)
	ids := []string{"sb_livealloc0001"}
	cleanupLive(t, ids)
	t.Cleanup(func() { cleanupLive(t, ids) })

	for _, rule := range BaseRuleset() {
		exec.Command("nft", rule...).Run()
	}
	mgr, err := NewManager("100.64.0.0/22", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if _, err := mgr.Setup(ids[0]); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if err := mgr.Teardown(ids[0]); err != nil {
			t.Fatalf("iteration %d teardown: %v", i, err)
		}
	}
	if used := mgr.alloc.Used(); used != 0 {
		t.Fatalf("%d addresses still marked in use after balanced setup/teardown", used)
	}
}

// pingFromNS sends one ICMP echo from inside a namespace and reports whether it
// got a reply. Absence of a reply is the interesting case, so the timeout is
// short and errors are treated as "no connectivity", not as test failures.
func pingFromNS(ns, srcIP, dstIP string) bool {
	cmd := exec.Command("ip", "netns", "exec", ns,
		"ping", "-c", "1", "-W", "1", "-I", srcIP, dstIP)
	return cmd.Run() == nil
}

// TestLivePacketLevelIsolation is the test this whole subsystem existed for and
// never had: it sends REAL PACKETS between two sandboxes and asserts the kernel
// drops them.
//
// Crucially it also establishes a CONTROL: with the per-sandbox policy removed,
// the same packet must get through. Without that, "the ping failed" proves
// nothing — it could be failing for any unrelated reason, which is exactly how
// a policy that matched a non-existent interface looked like it was working.
func TestLivePacketLevelIsolation(t *testing.T) {
	requireLiveNetns(t)
	a, b := "sb_livepkt000001", "sb_livepkt000002"
	cleanupLive(t, []string{a, b})
	t.Cleanup(func() { cleanupLive(t, []string{a, b}) })

	// Host forwarding must be on, or nothing traverses and every assertion
	// below would pass vacuously.
	prev, _ := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1"), 0o644)
	t.Cleanup(func() { os.WriteFile("/proc/sys/net/ipv4/ip_forward", prev, 0o644) })

	for _, rule := range BaseRuleset() {
		if out, err := exec.Command("nft", rule...).CombinedOutput(); err != nil {
			t.Fatalf("base rule %v: %v %s", rule, err, out)
		}
	}
	mgr, err := NewManager("100.64.0.0/22", false)
	if err != nil {
		t.Fatal(err)
	}
	na, err := mgr.Setup(a)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := mgr.Setup(b)
	if err != nil {
		t.Fatal(err)
	}

	// Stand in for the microVMs: give each namespace its own guest address on
	// the veth, which is where the host routes that /32.
	for _, n := range []*SandboxNet{na, nb} {
		for _, c := range [][]string{
			{"ip", "-n", n.NSName, "addr", "add", n.GuestIP.String() + "/32", "dev", n.VethPeer},
			{"ip", "-n", n.NSName, "route", "replace", "default", "dev", n.VethPeer},
		} {
			if out, err := exec.Command(c[0], c[1:]...).CombinedOutput(); err != nil {
				t.Fatalf("%v: %v %s", c, err, out)
			}
		}
	}

	chainA := SandboxChain(na.VethHost)

	// CONTROL: with A's policy chain emptied, A must be able to reach B.
	// This proves the topology works and the assertions below are meaningful.
	if out, err := exec.Command("nft", "flush", "chain", "inet", liveTable, chainA).CombinedOutput(); err != nil {
		t.Fatalf("flush %s: %v %s", chainA, err, out)
	}
	if !pingFromNS(na.NSName, na.GuestIP.String(), nb.GuestIP.String()) {
		t.Skipf("control failed: sandbox %s cannot reach %s even with the policy removed, "+
			"so this environment cannot demonstrate the drop (routing/ICMP restrictions)", a, b)
	}
	t.Logf("control: with the policy flushed, %s reaches %s", na.GuestIP, nb.GuestIP)

	// Now restore the real policy and require the same packet to be dropped.
	for _, rule := range EgressRules(na.VethHost, na.GuestIP.String(), "100.64.0.0/22") {
		// The chain and the map element already exist; re-adding rules is enough.
		if len(rule) > 2 && rule[1] == "rule" {
			if out, err := exec.Command("nft", rule...).CombinedOutput(); err != nil {
				t.Fatalf("re-add %v: %v %s", rule, err, out)
			}
		}
	}

	if pingFromNS(na.NSName, na.GuestIP.String(), nb.GuestIP.String()) {
		t.Errorf("PEER ISOLATION BROKEN: sandbox %s reached sandbox %s (%s -> %s)",
			a, b, na.GuestIP, nb.GuestIP)
	} else {
		t.Logf("peer isolation holds: %s cannot reach %s", na.GuestIP, nb.GuestIP)
	}

	// The metadata endpoint must be unreachable too.
	if pingFromNS(na.NSName, na.GuestIP.String(), "169.254.169.254") {
		t.Errorf("IMDS REACHABLE from sandbox %s", a)
	}

	// And a forged source address must be dropped rather than forwarded.
	//
	// Asserted on the rule COUNTER, not on reachability: a ping to an address
	// that happens to be local inside the namespace never reaches the forward
	// chain at all, so "the ping succeeded" would prove nothing about the
	// policy. The destination here is deliberately one the namespace does not
	// own, so the packet must be forwarded to be seen.
	exec.Command("ip", "-n", na.NSName, "addr", "add", nb.GuestIP.String()+"/32", "dev", na.VethPeer).Run()
	// The kernel's reverse-path filter would drop a forged source in the
	// ROUTING layer, before nftables ever sees it — which is good defence but
	// would make this assertion vacuous. Turn it off for the host-side
	// interface so the packet actually reaches our rule and we learn whether
	// OUR policy works, not whether rp_filter does.
	for _, k := range []string{"net.ipv4.conf.all.rp_filter", "net.ipv4.conf." + na.VethHost + ".rp_filter"} {
		key := k
		if prev, err := exec.Command("sysctl", "-n", key).Output(); err == nil {
			old := strings.TrimSpace(string(prev))
			// Restore it: this is a HOST-WIDE setting and a test must not
			// leave the machine less protected than it found it.
			t.Cleanup(func() { exec.Command("sysctl", "-qw", key+"="+old).Run() })
		}
		if out, err := exec.Command("sysctl", "-qw", key+"=0").CombinedOutput(); err != nil {
			t.Logf("could not relax %s: %v %s", key, err, out)
		}
	}
	before := antiSpoofCount(t, chainA, na.GuestIP.String())
	pingFromNS(na.NSName, nb.GuestIP.String(), "100.64.3.254")
	after := antiSpoofCount(t, chainA, na.GuestIP.String())
	if after <= before {
		t.Errorf("ANTI-SPOOF DID NOT FIRE: sandbox %s sent traffic as %s and the rule counter stayed at %d\n%s",
			a, nb.GuestIP, after, nftDump(t))
	} else {
		t.Logf("anti-spoof fired: counter %d -> %d", before, after)
	}
}

// antiSpoofCount reads the packet counter on a sandbox chain's anti-spoof rule.
func antiSpoofCount(t *testing.T, chain, guestIP string) int {
	t.Helper()
	out, err := exec.Command("nft", "list", "chain", "inet", liveTable, chain).CombinedOutput()
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "saddr != "+guestIP) || !strings.Contains(line, "counter") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "packets" && i+1 < len(fields) {
				n := 0
				fmt.Sscanf(fields[i+1], "%d", &n)
				return n
			}
		}
	}
	return -1
}
