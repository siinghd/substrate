package netns

import (
	"fmt"
	"os/exec"
	"strings"
)

const (
	Table = "substrate"
	// dispatchMap maps a host-side interface name to the per-sandbox chain to
	// jump to. A map element is O(1) to add and, crucially, O(1) to DELETE
	// without needing nftables rule handles — which is what makes teardown
	// reliable. Appending per-sandbox rules straight into the forward chain
	// left them there forever: nothing removed them on destroy, so the chain
	// grew without bound and a later sandbox that reused an interface name
	// inherited a dead tenant's policy.
	dispatchMap = "sbmap"
)

// SandboxChain is the per-sandbox chain name for a host-side interface.
func SandboxChain(hostIface string) string {
	return "sb_" + strings.TrimPrefix(hostIface, "vb")
}

// BaseRuleset is programmed once per node boot. Order is security-critical:
// the IMDS/link-local drop is rule #1 (plan §4.2), before anything else can
// accept or forward a packet.
//
// The security-relevant drops carry a `counter`. A silent drop is
// indistinguishable from a rule that never matches, which is precisely how a
// policy attached to an interface the host cannot see went unnoticed: nothing
// anywhere reported whether it had ever fired.
func BaseRuleset() [][]string {
	return [][]string{
		{"add", "table", "inet", Table},
		{"add", "map", "inet", Table, dispatchMap, "{ type ifname : verdict ; }"},
		{"add", "chain", "inet", Table, "forward", "{ type filter hook forward priority 0 ; policy accept ; }"},

		// Rule #1 and #2: the metadata endpoint, for BOTH families. The IPv6
		// link-local range was previously unfiltered entirely, and clouds do
		// expose a v6 metadata address.
		{"add", "rule", "inet", Table, "forward", "ip", "daddr", "169.254.0.0/16", "counter", "drop"},
		{"add", "rule", "inet", Table, "forward", "ip6", "daddr", "fe80::/10", "drop"},
		{"add", "rule", "inet", Table, "forward", "ip6", "daddr", "fd00:ec2::/64", "drop"},

		{"add", "rule", "inet", Table, "forward", "meta", "l4proto", "tcp", "tcp", "dport", "25", "drop"},
		{"add", "rule", "inet", Table, "forward", "ct", "state", "established,related", "accept"},

		// Dispatch to the per-sandbox chain for traffic arriving from a
		// sandbox's host-side interface.
		{"add", "rule", "inet", Table, "forward", "iifname", "vmap", "@" + dispatchMap},
	}
}

// EgressRules returns the per-sandbox chain and its rules.
//
// hostIface is the HOST-SIDE interface, i.e. the veth end that lives in the
// host namespace. It used to be the tap name — but the tap lives INSIDE the
// sandbox's netns, so `iifname "tap…"` could never match in the host's forward
// chain and every per-sandbox rule was dead. The entire egress policy was
// inert: no RFC1918 drop, no peer isolation, no rate limit.
//
// blockCIDR is the node's sandbox address block, used for peer isolation.
func EgressRules(hostIface, guestIP, blockCIDR string) [][]string {
	chain := SandboxChain(hostIface)
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	return [][]string{
		{"add", "chain", "inet", Table, chain},

		// ANTI-SPOOF FIRST. Everything below reasons about addresses, so a
		// guest that can forge its source address could otherwise sidestep the
		// checks. Nothing enforced this before.
		{"add", "rule", "inet", Table, chain, "ip", "saddr", "!=", guestIP, "counter", "drop"},

		// IPv6 has no v1 story: the table is `inet` (dual stack) but every
		// filter was an IPv4 `ip daddr` match, so v6 traffic fell through to
		// the rate-limit accept completely unfiltered.
		{"add", "rule", "inet", Table, chain, "meta", "nfproto", "ipv6", "counter", "drop"},

		// PEER ISOLATION. The previous rule was
		//   ip daddr <block> ip saddr != <guestIP> drop
		// which, under an iifname match that already implies the packet came
		// from this sandbox, is only ever true for spoofed packets — so
		// legitimate guest-to-guest traffic did NOT match and was accepted by
		// the rate-limit rule below. Sandboxes could talk to each other.
		{"add", "rule", "inet", Table, chain, "ip", "daddr", blockCIDR, "counter", "drop"},

		{"add", "rule", "inet", Table, chain, "ip", "daddr", "10.0.0.0/8", "drop"},
		{"add", "rule", "inet", Table, chain, "ip", "daddr", "172.16.0.0/12", "drop"},
		{"add", "rule", "inet", Table, chain, "ip", "daddr", "192.168.0.0/16", "drop"},

		{"add", "rule", "inet", Table, chain, "ct", "state", "established,related", "accept"},
		// `burst 500` alone is a SYNTAX ERROR: nft answers
		// "unexpected accept, expecting packets". The unit is required, and
		// without it this rule -- and therefore the whole sandbox chain --
		// was rejected by the kernel.
		{"add", "rule", "inet", Table, chain, "limit", "rate", "500/second", "burst", "500", "packets", "accept"},
		{"add", "rule", "inet", Table, chain, "drop"},

		// Wire it up last: until this element exists the chain is unreachable,
		// so a half-built policy never filters traffic with gaps in it.
		{"add", "element", "inet", Table, dispatchMap, "{ " + q(hostIface) + " : jump " + chain + " }"},
	}
}

// TeardownRules removes a sandbox's policy. Order matters: unhook the dispatch
// first so no packet can reach a chain that is being emptied.
func TeardownRules(hostIface string) [][]string {
	chain := SandboxChain(hostIface)
	q := func(s string) string { return fmt.Sprintf("%q", s) }
	return [][]string{
		{"delete", "element", "inet", Table, dispatchMap, "{ " + q(hostIface) + " }"},
		{"flush", "chain", "inet", Table, chain},
		{"delete", "chain", "inet", Table, chain},
	}
}

func runCmd(name string, args ...string) error {
	return exec.Command(name, args...).Run()
}

func outputCmd(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}
