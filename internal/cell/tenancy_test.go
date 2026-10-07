package cell

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// newTestRegistry builds a registry over SQLite. The production cell runs on
// Postgres, but the placement/ownership logic under test is dialect-neutral
// and this keeps the tenancy check covered without a database dependency.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/reg.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(nodeSchema); err != nil {
		t.Fatal(err)
	}
	return &Registry{db: db}
}

// TestForwardRefusesCrossTenantSandbox: the cell forwards to nodes with ONE
// privileged node credential, so every tenant's traffic reaches the node as
// the same principal and every node-side org check passes. Routing on sandbox
// id alone therefore gave any authenticated key full read/write/destroy over
// every other tenant's sandboxes. Ownership must be enforced here.
func TestForwardRefusesCrossTenantSandbox(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()
	p := NewProxy(reg)

	const victimSandbox = "sb_victim0000001"
	if err := reg.AssignPlacement(ctx, victimSandbox, "node_1", "org_victim"); err != nil {
		t.Fatal(err)
	}

	// Attacker holds a legitimate, unrevoked key — for a different org.
	_, status, err := p.Forward(ctx, "org_attacker", victimSandbox, "POST",
		"/v1/sandboxes/"+victimSandbox+"/exec", []byte(`{"cmd":"cat /workspace/.env"}`), "node_key")
	if err == nil {
		t.Fatal("cross-tenant forward succeeded: another org's sandbox is reachable")
	}
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("error = %v, want ErrNotOwner", err)
	}
	if status != 404 {
		t.Fatalf("status = %d, want 404 — a 403 confirms the sandbox exists, which is itself a cross-tenant disclosure", status)
	}
}

// TestForwardRefusesUnownedPlacement: a placement written before ownership was
// recorded has an empty org. It must fail closed, not match everyone.
func TestForwardRefusesUnownedPlacement(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()
	p := NewProxy(reg)

	const sbID = "sb_legacy0000001"
	if err := reg.AssignPlacement(ctx, sbID, "node_1", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Forward(ctx, "org_any", sbID, "GET", "/v1/sandboxes/"+sbID, nil, "node_key"); err == nil {
		t.Fatal("placement with no recorded owner was forwarded")
	}
	// An empty caller org must never match an empty owner org either.
	if _, _, err := p.Forward(ctx, "", sbID, "GET", "/v1/sandboxes/"+sbID, nil, "node_key"); err == nil {
		t.Fatal("empty caller org matched an empty owner org")
	}
}

// TestPlacementOwnerRoundTrips: ownership must survive a re-placement, or a
// migrated sandbox would silently lose its tenancy binding.
func TestPlacementOwnerRoundTrips(t *testing.T) {
	reg := newTestRegistry(t)
	ctx := context.Background()
	const sbID = "sb_move000000001"

	if err := reg.AssignPlacement(ctx, sbID, "node_1", "org_a"); err != nil {
		t.Fatal(err)
	}
	node, org, ok := reg.PlacementOwner(ctx, sbID)
	if !ok || node != "node_1" || org != "org_a" {
		t.Fatalf("placement = (%q,%q,%v)", node, org, ok)
	}
	// Re-place onto another node; the owner must persist.
	if err := reg.AssignPlacement(ctx, sbID, "node_2", "org_a"); err != nil {
		t.Fatal(err)
	}
	node, org, ok = reg.PlacementOwner(ctx, sbID)
	if !ok || node != "node_2" || org != "org_a" {
		t.Fatalf("after re-place = (%q,%q,%v)", node, org, ok)
	}
}

// TestForwardDoesNotFollowRedirects: the cell forwards with a privileged node
// credential, and Go re-sends Authorization on a same-host redirect. A node-side
// ServeMux 301 — which any path the mux cleans produces — would therefore carry
// that credential to a path the cell never authorized. Defence in depth behind
// the path-containment check in cmd/cell.
func TestForwardDoesNotFollowRedirects(t *testing.T) {
	var hits []string
	var sawAuthOnSecondHop bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		if len(hits) > 1 && r.Header.Get("Authorization") != "" {
			sawAuthOnSecondHop = true
		}
		if r.URL.Path == "/v1/sandboxes/sb_redirect00001" {
			http.Redirect(w, r, "/metrics", http.StatusMovedPermanently)
			return
		}
		w.WriteHeader(200)
		w.Write([]byte("SECRET METRICS"))
	}))
	defer srv.Close()

	reg := newTestRegistry(t)
	ctx := context.Background()
	const sbID = "sb_redirect00001"
	if err := reg.AssignPlacement(ctx, sbID, "node_r", "org_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.db.ExecContext(ctx,
		`INSERT INTO nodes (id, org_id, name, region, tier, byo, api_addr, enrolled_at, last_heartbeat_ms)
		 VALUES (?, ?, ?, ?, ?, 0, ?, 0, 0)`,
		"node_r", "org_a", "n", "local", "hv", strings.TrimPrefix(srv.URL, "http://")); err != nil {
		t.Fatal(err)
	}

	p := NewProxy(reg)
	out, status, err := p.Forward(ctx, "org_a", sbID, "GET", "/v1/sandboxes/"+sbID, nil, "sk_node_privileged")
	if err != nil {
		t.Fatalf("Forward: %v", err)
	}
	if status != http.StatusMovedPermanently {
		t.Fatalf("status = %d, want the redirect surfaced (301) rather than followed", status)
	}
	if strings.Contains(string(out), "SECRET METRICS") {
		t.Fatal("the cell followed a node redirect and returned a path it never authorized")
	}
	if sawAuthOnSecondHop {
		t.Fatal("the privileged node credential was replayed on a redirect hop")
	}
}
