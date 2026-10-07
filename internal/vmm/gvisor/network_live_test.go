package gvisor

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/netns"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// liveNet programs a real namespace for one sandbox and tears it down after.
func liveNet(t *testing.T, id string) *vmm.NetInfo {
	t.Helper()
	mgr, err := netns.NewManagerWithOptions("100.64.192.0/24", netns.Options{WithTap: false, MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureNodeEgress(); err != nil {
		t.Fatalf("node egress: %v", err)
	}
	if err := mgr.EnsureBaseRuleset(); err != nil {
		t.Fatalf("base ruleset: %v", err)
	}
	n, err := mgr.Setup(id)
	if err != nil {
		t.Fatalf("netns setup: %v", err)
	}
	t.Cleanup(func() { mgr.Teardown(id) })
	return &vmm.NetInfo{
		NSName: n.NSName, NSPath: filepath.Join("/var/run/netns", n.NSName),
		GuestIP: n.GuestIP.String(), GatewayIP: n.GatewayIP.String(),
	}
}

// TestLiveGvisorSandboxReachesTheInternet is the test the whole networking
// pass exists for.
//
// Before it, `internal/netns` was written and kernel-tested and then wired to
// NOTHING: every call site passed a nil *vmm.NetInfo, and the gVisor driver ran
// `--network none`. Sandboxes had no network at all -- no package install, no
// API call -- and the egress policy (IMDS drop, peer isolation, rate limit) was
// programmed nowhere, so it protected nothing.
//
// It asserts DNS and TCP, not ping. ICMP crosses a host whose FORWARD policy is
// DROP while DNS and TCP silently time out, which is exactly the half-working
// state that looks healthy: measured on this host before the fix.
func TestLiveGvisorSandboxReachesTheInternet(t *testing.T) {
	requireGvisor(t)
	requireRoot(t)
	rootfs := testRootfs(t)
	writeResolvConf(t, rootfs)
	d := gvDriver(t)
	ctx := context.Background()

	id := "sb_gvnet00000001"
	spec := vmm.Spec{ID: id, Net: liveNet(t, id), Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
	sh := guestShell(t, ctx, m)

	if got := sh("ip addr show 2>&1 | grep -c 'inet '"); strings.TrimSpace(got) == "0" {
		t.Fatal("sandbox has no addresses: it did not join the namespace")
	}
	// DNS: a UDP round trip through NAT and the host's forward path.
	//
	// Assert that a NAME RESOLVED, not that it resolved to a particular
	// address: one.one.one.one has two, and pinning one made this fail against
	// a working sandbox that happened to be handed the other.
	got := sh("nslookup one.one.one.one 1.1.1.1 2>&1")
	if !strings.Contains(got, "Name:") || !strings.Contains(got, "Address:") {
		t.Errorf("DNS does not work inside the sandbox: %q", got)
	}
	if !strings.Contains(got, "1.1.1.1") && !strings.Contains(got, "1.0.0.1") {
		t.Errorf("DNS answered but not with a Cloudflare address: %q", got)
	}
	// TCP: a real connection to a real server.
	if got := sh("wget -q -T10 -O- http://1.1.1.1 2>&1 | head -c 40"); !strings.Contains(strings.ToLower(got), "html") {
		t.Errorf("TCP does not work inside the sandbox: %q", got)
	}
}

// TestLiveGvisorEgressPolicyIsEnforced: the policy that was inert for 28 passes
// must actually block things now that traffic flows.
func TestLiveGvisorEgressPolicyIsEnforced(t *testing.T) {
	requireGvisor(t)
	requireRoot(t)
	rootfs := testRootfs(t)
	writeResolvConf(t, rootfs)
	d := gvDriver(t)
	ctx := context.Background()

	id := "sb_gvpolicy00001"
	spec := vmm.Spec{ID: id, Net: liveNet(t, id), Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
	sh := guestShell(t, ctx, m)

	// The cloud metadata endpoint. Plan §4.2 makes this rule #1, and it has
	// never once been programmed on a live host until this pass.
	if got := sh("wget -q -T4 -O- http://169.254.169.254/latest/meta-data/ 2>&1 | head -c 60"); strings.Contains(strings.ToLower(got), "ami-") || strings.Contains(strings.ToLower(got), "instance") {
		t.Errorf("IMDS is REACHABLE from inside a sandbox: %q", got)
	}
	// The host's own private networks.
	for _, target := range []string{"10.0.0.1", "192.168.1.1", "172.16.0.1"} {
		if got := sh("wget -q -T3 -O- http://" + target + " 2>&1 | head -c 40"); strings.Contains(strings.ToLower(got), "html") {
			t.Errorf("RFC1918 address %s is reachable from a sandbox: %q", target, got)
		}
	}
}

func requireRoot(t *testing.T) {
	t.Helper()
	if out, err := exec.Command("id", "-u").Output(); err != nil || strings.TrimSpace(string(out)) != "0" {
		t.Skip("needs root to program namespaces")
	}
}

// writeResolvConf gives the template a resolver. Nothing in the product did:
// a sandbox with routing but no /etc/resolv.conf resolves nothing, which looks
// identical to a firewall problem.
func writeResolvConf(t *testing.T, rootfs string) {
	t.Helper()
	if err := exec.Command("sh", "-c",
		"mkdir -p "+filepath.Join(rootfs, "etc")+" && echo 'nameserver 1.1.1.1' > "+filepath.Join(rootfs, "etc/resolv.conf")).Run(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
}

// TestLiveGvisorCanInstallAPackage is the product requirement, not a protocol
// detail: a sandbox nobody can `apk add` in is not a sandbox anyone will rent.
//
// It depends on a third-party CDN, so a CDN outage SKIPS rather than fails --
// but an outage is the only thing allowed to skip it: a resolution or routing
// failure still fails, because that is our bug, not theirs.
func TestLiveGvisorCanInstallAPackage(t *testing.T) {
	requireGvisor(t)
	requireRoot(t)
	rootfs := testRootfs(t)
	writeResolvConf(t, rootfs)
	d := gvDriver(t)
	ctx := context.Background()
	id := "sb_gvapk00000001"
	spec := vmm.Spec{ID: id, Net: liveNet(t, id), Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
	sh := guestShell(t, ctx, m)

	out := sh("apk add --no-cache jq 2>&1 | tail -3")
	if strings.Contains(out, "temporary error") || strings.Contains(out, "server returned error") {
		t.Skipf("package mirror is unreachable, not our failure: %q", out)
	}
	if strings.Contains(out, "DNS") || strings.Contains(out, "Network unreachable") {
		t.Fatalf("package install failed on OUR networking, not the mirror: %q", out)
	}
	if !strings.Contains(out, "OK:") {
		t.Fatalf("could not install a package: %q", out)
	}
	if got := sh("echo '{\"a\":1}' | jq -r .a"); strings.TrimSpace(got) != "1" {
		t.Fatalf("installed package does not run: %q", got)
	}
}
