package gvisor

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/netns"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// TestLiveGvisorServesASiteFromInsideTheSandbox.
//
// The product question behind preview URLs: can a tenant run a web server in a
// sandbox and have something outside reach it?
//
// Until this pass the answer was no on every isolated tier. The gateway's
// resolver needs a per-sandbox address and REFUSES to route without one --
// correctly, since the alternative is pointing every tenant's preview at the
// node's own loopback -- and nothing ever supplied that address. So a sandbox
// could serve a site that nothing could reach.
//
// This connects from the HOST to the sandbox's own address, which is exactly
// what the gateway does once the resolver is wired.
func TestLiveGvisorServesASiteFromInsideTheSandbox(t *testing.T) {
	requireGvisor(t)
	requireRoot(t)
	rootfs := testRootfs(t)
	writeResolvConf(t, rootfs)
	// Stage the tenant's server BEFORE Create: the template tree is copied
	// once at create time, so anything added afterwards is not in the sandbox.
	stageMinisite(t, rootfs)
	d := gvDriver(t)
	ctx := context.Background()

	mgr, err := netns.NewManagerWithOptions("100.64.210.0/24", netns.Options{MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureNodeEgress(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureBaseRuleset(); err != nil {
		t.Fatal(err)
	}
	const id = "sb_gvsite0000001"
	n, err := mgr.Setup(id)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	defer mgr.Teardown(id)

	spec := vmm.Spec{ID: id, Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}, Net: &vmm.NetInfo{
		NSName: n.NSName, NSPath: filepath.Join("/var/run/netns", n.NSName),
		GuestIP: n.GuestIP.String(), GatewayIP: n.GatewayIP.String(),
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
	sh := guestShell(t, ctx, m)

	sh("setsid /minisite :8080 >/tmp/site.log 2>&1 & echo started")

	// Diagnostics before the assertion, so a failure says WHY.
	sh("sleep 1; netstat -ltn 2>&1 | tail -3; cat /tmp/site.log 2>&1")
	sh("wget -q -T3 -O- http://127.0.0.1:8080/ 2>&1 | head -1")

	target := net.JoinHostPort(n.GuestIP.String(), "8080")
	client := &http.Client{Timeout: 5 * time.Second}
	var body string
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://" + target + "/")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body = string(b)
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !strings.Contains(body, "hello from the sandbox") {
		t.Fatalf("could not reach the sandbox's web server at %s: %q", target, body)
	}
	t.Logf("served from %s: %s", target, strings.TrimSpace(body))

	// The sandbox must also ANNOUNCE the port, or the gateway has nothing to
	// route: the resolver takes a port from the URL, but the platform needs
	// the announcement to know the sandbox is listening at all.
	// Announcement is a POLL of the guest's own listening sockets, so allow a
	// couple of intervals for a server that started after the handshake.
	var ports []vmm.PortInfo
	deadline2 := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline2) {
		ports = m.Ports()
		for _, p := range ports {
			if p.Port == 8080 {
				t.Logf("announced ports: %+v", ports)
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Errorf("sandbox is serving on 8080 but announced %v; the gateway cannot discover it", ports)
	_ = fmt.Sprint()
}

// stageMinisite builds the tenant's web server into the rootfs.
func stageMinisite(t *testing.T, rootfs string) {
	t.Helper()
	bin := filepath.Join(rootfs, "minisite")
	build := exec.Command("go", "build", "-o", bin, "./internal/vmm/gvisor/testdata/minisite")
	build.Dir = repoRoot(t)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build minisite: %v\n%s", err, out)
	}
}

// TestLiveGvisorLoopbackOnlyServersAreNotPublished.
//
// A tenant binding 127.0.0.1 has chosen to keep a service private -- a
// database, an admin endpoint, a debug handler. Announcing it would publish a
// preview URL for something deliberately internal, and the URL could not work
// anyway since loopback is not reachable from outside the sandbox.
func TestLiveGvisorLoopbackOnlyServersAreNotPublished(t *testing.T) {
	requireGvisor(t)
	requireRoot(t)
	rootfs := testRootfs(t)
	writeResolvConf(t, rootfs)
	stageMinisite(t, rootfs)
	d := gvDriver(t)
	ctx := context.Background()

	mgr, err := netns.NewManagerWithOptions("100.64.211.0/24", netns.Options{MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureNodeEgress(); err != nil {
		t.Fatal(err)
	}
	if err := mgr.EnsureBaseRuleset(); err != nil {
		t.Fatal(err)
	}
	const id = "sb_gvloop0000001"
	n, err := mgr.Setup(id)
	if err != nil {
		t.Fatalf("netns: %v", err)
	}
	defer mgr.Teardown(id)

	spec := vmm.Spec{ID: id, Template: vmm.TemplateSpec{
		Name: "gv", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1,
	}, Net: &vmm.NetInfo{
		NSName: n.NSName, NSPath: filepath.Join("/var/run/netns", n.NSName),
		GuestIP: n.GuestIP.String(), GatewayIP: n.GatewayIP.String(),
	}}
	m, err := d.Create(ctx, spec)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { m.Kill(); d.Cleanup(context.Background(), id) }()
	sh := guestShell(t, ctx, m)

	// One private (loopback) and one public listener, so the test proves a
	// DISTINCTION rather than "nothing was announced".
	sh("setsid /minisite 127.0.0.1:9001 >/dev/null 2>&1 & echo private")
	sh("setsid /minisite :9002 >/dev/null 2>&1 & echo public")

	deadline := time.Now().Add(15 * time.Second)
	var ports []vmm.PortInfo
	for time.Now().Before(deadline) {
		ports = m.Ports()
		hasPublic := false
		for _, p := range ports {
			if p.Port == 9002 {
				hasPublic = true
			}
		}
		if hasPublic {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("announced: %+v", ports)
	for _, p := range ports {
		if p.Port == 9001 {
			t.Errorf("a loopback-only server was published: %+v", ports)
		}
	}
	found := false
	for _, p := range ports {
		if p.Port == 9002 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the public listener was not announced, so this proves nothing: %+v", ports)
	}
}
