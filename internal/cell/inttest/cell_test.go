//go:build cell

// Cell integration test against a real Postgres: registry, scheduler,
// enrollment, placement routing. Requires SUBSTRATE_TEST_DB_URL.
package inttest

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/substrate-sh/substrate/internal/cell"
	"github.com/substrate-sh/substrate/internal/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("SUBSTRATE_TEST_DB_URL")
	if dsn == "" {
		t.Skip("SUBSTRATE_TEST_DB_URL not set; skipping cell integration")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("postgres unavailable: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("postgres unreachable: %v", err)
	}
	for _, table := range []string{"nodes", "placements", "orgs", "projects", "api_keys", "sandboxes", "events"} {
		db.Exec("DROP TABLE IF EXISTS " + table)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestCellSchedulingAndRouting(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()

	st, err := store.OpenPostgres(os.Getenv("SUBSTRATE_TEST_DB_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", CreatedAt: now})

	reg, err := cell.NewRegistry(db)
	if err != nil {
		t.Fatal(err)
	}

	// two nodes; node B holds the template in its snapshot cache
	nodeA := &cell.Node{ID: "nd_a", Region: "eu", Tier: "hv", APIAddr: "127.0.0.1:1",
		MemTotalMiB: 1024, EnrolledAt: now, LastHeartbeat: now}
	nodeB := &cell.Node{ID: "nd_b", Region: "eu", Tier: "hv", APIAddr: "127.0.0.1:2",
		MemTotalMiB: 1024, SnapshotCache: []string{"tpl_hot"}, EnrolledAt: now, LastHeartbeat: now}
	reg.Register(ctx, nodeA)
	reg.Register(ctx, nodeB)

	chosen, err := reg.Place(ctx, cell.PlacementRequest{Region: "eu", Tier: "hv", TemplateID: "tpl_hot", MemMiB: 512})
	if err != nil {
		t.Fatal(err)
	}
	if chosen.ID != "nd_b" {
		t.Fatalf("scheduler ignored snapshot-cache affinity: chose %s", chosen.ID)
	}

	// stale heartbeat is filtered
	stale := &cell.Node{ID: "nd_stale", Region: "us", Tier: "hv", APIAddr: "x",
		MemTotalMiB: 4096, SnapshotCache: []string{"tpl_hot"},
		EnrolledAt: now, LastHeartbeat: now.Add(-time.Hour)}
	reg.Register(ctx, stale)
	chosen, err = reg.Place(ctx, cell.PlacementRequest{Region: "us", Tier: "hv", TemplateID: "tpl_hot", MemMiB: 512})
	if err == nil {
		t.Fatalf("stale node was schedulable: %s", chosen.ID)
	}

	// BYO isolation: in a region with ONLY a foreign-org BYO node, another
	// org's work must fail to schedule rather than land on it.
	reg.Register(ctx, &cell.Node{ID: "nd_byo", OrgID: "org_other", Region: "byo-region", Tier: "hv",
		MemTotalMiB: 8192, BYO: true, EnrolledAt: now, LastHeartbeat: now})
	_, err = reg.Place(ctx, cell.PlacementRequest{Region: "byo-region", Tier: "hv", OrgID: "org_1", TemplateID: "t", MemMiB: 512})
	if err == nil {
		t.Fatal("BYO node received another org's placement")
	}
	// ...but its owner CAN use it.
	own, err := reg.Place(ctx, cell.PlacementRequest{Region: "byo-region", Tier: "hv", OrgID: "org_other", TemplateID: "t", MemMiB: 512})
	if err != nil || own.ID != "nd_byo" {
		t.Fatalf("BYO owner could not place: %v %v", own, err)
	}

	// proxy routes by placement
	backendHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		w.WriteHeader(201)
		w.Write([]byte(`{"id":"sb_routed0000001"}`))
	}))
	defer backend.Close()
	addr := backend.Listener.Addr().String()
	reg.Register(ctx, &cell.Node{ID: "nd_real", Region: "eu", Tier: "hv",
		APIAddr: addr, MemTotalMiB: 1024,
		EnrolledAt: now, LastHeartbeat: now})

	proxy := cell.NewProxy(reg)
	// Placements now record the owning org, and Forward refuses a caller that
	// does not match: the cell speaks to nodes with one privileged credential,
	// so it is the only place tenancy can be enforced on this path.
	const sbID = "sb_routed0000001"
	reg.AssignPlacement(ctx, sbID, "nd_real", "org_owner")
	out, status, err := proxy.Forward(ctx, "org_owner", sbID, "GET", "/v1/sandboxes/"+sbID, nil, "sk_node_internal")
	if err != nil || status != 201 || !backendHit {
		t.Fatalf("forward failed: %d %s %v", status, out, err)
	}

	// And a different org must not reach it, even though the placement exists.
	if _, st, err := proxy.Forward(ctx, "org_other", sbID, "GET", "/v1/sandboxes/"+sbID, nil, "sk_node_internal"); err == nil {
		t.Fatalf("cross-tenant forward succeeded against Postgres (status %d)", st)
	} else if st != 404 {
		t.Fatalf("cross-tenant forward status = %d, want 404", st)
	}
}
