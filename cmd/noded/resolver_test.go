package main

import (
	"context"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/store"
)

func resolverEnv(t *testing.T, tier string) (*storeResolver, store.Store, string) {
	t.Helper()
	st, err := store.OpenSQLite(t.TempDir() + "/r.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", CreatedAt: now})
	id := "sb_resolver00001"
	st.CreateSandbox(ctx, &store.Sandbox{
		ID: id, OrgID: "org_1", ProjectID: "prj_1", State: store.StateRunning,
		Desired: "running", Tier: tier, MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now,
	})
	cfg := config.Defaults()
	return newStoreResolver(st, cfg, logging.New("error"), nil), st, id
}

// TestResolverRefusesNodeReservedPorts: a tenant could expose the node's own
// API port and reach it — including its unauthenticated /metrics — from the
// open internet, because the target was always 127.0.0.1:<tenant's port>.
func TestResolverRefusesNodeReservedPorts(t *testing.T) {
	r, st, id := resolverEnv(t, "process")
	r.cfg.Gateway.AllowProcessTierPreviews = true // even fully opted in
	ctx := context.Background()

	for _, port := range []int{8080, 8081} {
		if err := st.UpsertPort(ctx, id, store.PortMapping{Port: port, Proto: "tcp", Public: true, Token: "t"}); err != nil {
			t.Fatal(err)
		}
		if route, ok := r.Resolve(ctx, id, port); ok {
			t.Fatalf("port %d resolved to %q — the gateway would proxy the internet into the node's own listener", port, route.Target)
		}
	}
}

// TestResolverFailsClosedForIsolatedTiers: with no per-sandbox network
// resolver wired, an hv sandbox's preview used to fall back to the node's
// loopback. No address must mean no route.
func TestResolverFailsClosedForIsolatedTiers(t *testing.T) {
	r, st, id := resolverEnv(t, "hv")
	ctx := context.Background()
	if err := st.UpsertPort(ctx, id, store.PortMapping{Port: 3000, Proto: "tcp", Public: true, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if route, ok := r.Resolve(ctx, id, 3000); ok {
		t.Fatalf("hv sandbox resolved to %q with no network resolver wired; want no route", route.Target)
	}

	// With the hook wired, the target is the sandbox's OWN address.
	r.net = func(string) (string, bool) { return "100.64.0.7", true }
	route, ok := r.Resolve(ctx, id, 3000)
	if !ok {
		t.Fatal("hv sandbox with a resolved guest IP got no route")
	}
	if route.Target != "100.64.0.7:3000" {
		t.Fatalf("target = %q, want the sandbox's own address", route.Target)
	}

	// A hook that cannot answer must not fall back to loopback.
	r.net = func(string) (string, bool) { return "", false }
	if route, ok := r.Resolve(ctx, id, 3000); ok {
		t.Fatalf("unresolvable sandbox fell back to %q", route.Target)
	}
}

// TestResolverProcessTierPreviewsAreOptIn: process-tier ports live on the
// node's shared loopback with no per-sandbox address, so two sandboxes on the
// same port would share one target. Off unless explicitly enabled.
func TestResolverProcessTierPreviewsAreOptIn(t *testing.T) {
	r, st, id := resolverEnv(t, "process")
	ctx := context.Background()
	if err := st.UpsertPort(ctx, id, store.PortMapping{Port: 3000, Proto: "tcp", Public: true, Token: "t"}); err != nil {
		t.Fatal(err)
	}
	if config.Defaults().Gateway.AllowProcessTierPreviews {
		t.Fatal("process-tier previews default to ON; they share the node's loopback and cannot be isolated")
	}
	if route, ok := r.Resolve(ctx, id, 3000); ok {
		t.Fatalf("process-tier preview resolved to %q without opt-in", route.Target)
	}
	r.cfg.Gateway.AllowProcessTierPreviews = true
	route, ok := r.Resolve(ctx, id, 3000)
	if !ok || route.Target != "127.0.0.1:3000" {
		t.Fatalf("opted-in process preview: ok=%v target=%q", ok, route.Target)
	}
}

// TestResolverUnexposedPortHasNoRoute keeps the original contract intact.
func TestResolverUnexposedPortHasNoRoute(t *testing.T) {
	r, _, id := resolverEnv(t, "process")
	r.cfg.Gateway.AllowProcessTierPreviews = true
	if _, ok := r.Resolve(context.Background(), id, 9999); ok {
		t.Fatal("a port that was never exposed produced a route")
	}
}
