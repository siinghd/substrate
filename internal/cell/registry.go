// Package cell implements the hosted control-plane unit (plan §5.4–5.6):
// one Postgres cluster, a node registry with enrollment, a bin-packing
// scheduler, and an API that places sandboxes onto nodes and routes to them.
// The data plane stays direct (gateway/node); the cell owns control only.
package cell

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

const nodeSchema = `
CREATE TABLE IF NOT EXISTS nodes (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL DEFAULT '', name TEXT NOT NULL,
  region TEXT NOT NULL DEFAULT 'local', tier TEXT NOT NULL DEFAULT 'hv',
  byo INTEGER NOT NULL DEFAULT 0,
  api_addr TEXT NOT NULL DEFAULT '',
  mem_total_mib INTEGER NOT NULL DEFAULT 0, mem_alloc_mib INTEGER NOT NULL DEFAULT 0,
  live_sandboxes INTEGER NOT NULL DEFAULT 0,
  snapshot_cache TEXT NOT NULL DEFAULT '[]',
  draining INTEGER NOT NULL DEFAULT 0,
  enrolled_at BIGINT NOT NULL, last_heartbeat_ms BIGINT NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS placements (
  sandbox_id TEXT PRIMARY KEY, node_id TEXT NOT NULL, placed_at BIGINT NOT NULL,
  -- The owning tenant. Routing by sandbox id ALONE let any authenticated key
  -- reach any other tenant's sandbox: the cell forwards with its own
  -- privileged node credential, so the node cannot tell the callers apart.
  org_id TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS idx_placements_org ON placements(org_id);
`

// placementMigrations bring pre-existing databases up to the current shape.
// Postgres supports IF NOT EXISTS here, so these are idempotent.
var placementMigrations = []string{
	`ALTER TABLE placements ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT ''`,
}

type Registry struct {
	db *sql.DB
}

func NewRegistry(db *sql.DB) (*Registry, error) {
	if _, err := db.Exec(pgRewrite(nodeSchema)); err != nil {
		return nil, err
	}
	for _, q := range placementMigrations {
		if _, err := db.Exec(q); err != nil {
			return nil, fmt.Errorf("registry migrate %q: %w", q, err)
		}
	}
	return &Registry{db: db}, nil
}

// pgRewrite converts ?-placeholders to $n for Postgres.
func pgRewrite(q string) string {
	n := 0
	var b strings.Builder
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			b.WriteByte('$')
			b.WriteString(fmt.Sprint(n))
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

type Node struct {
	ID            string    `json:"id"`
	OrgID         string    `json:"org_id,omitempty"`
	Name          string    `json:"name"`
	Region        string    `json:"region"`
	Tier          string    `json:"tier"`
	BYO           bool      `json:"byo"`
	APIAddr       string    `json:"-"`
	MemTotalMiB   int       `json:"mem_total_mib"`
	MemAllocMiB   int       `json:"mem_alloc_mib"`
	LiveSandboxes int       `json:"live_sandboxes"`
	SnapshotCache []string  `json:"snapshot_cache"`
	Draining      bool      `json:"draining"`
	EnrolledAt    time.Time `json:"enrolled_at"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

const nodeCols = `id, org_id, name, region, tier, byo, api_addr, mem_total_mib, mem_alloc_mib, live_sandboxes, snapshot_cache, draining, enrolled_at, last_heartbeat_ms`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var byo, draining int
	var cache string
	var enrolled, hb int64
	if err := row.Scan(&n.ID, &n.OrgID, &n.Name, &n.Region, &n.Tier, &byo, &n.APIAddr,
		&n.MemTotalMiB, &n.MemAllocMiB, &n.LiveSandboxes, &cache, &draining, &enrolled, &hb); err != nil {
		return nil, err
	}
	n.BYO = byo == 1
	n.Draining = draining == 1
	_ = json.Unmarshal([]byte(cache), &n.SnapshotCache)
	n.EnrolledAt = time.UnixMilli(enrolled)
	n.LastHeartbeat = time.UnixMilli(hb)
	return &n, nil
}

func (r *Registry) Register(ctx context.Context, n *Node) error {
	q := pgRewrite(`INSERT INTO nodes (` + nodeCols + `) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, region=excluded.region, api_addr=excluded.api_addr,
  mem_total_mib=excluded.mem_total_mib, mem_alloc_mib=excluded.mem_alloc_mib, live_sandboxes=excluded.live_sandboxes,
  snapshot_cache=excluded.snapshot_cache, last_heartbeat_ms=excluded.last_heartbeat_ms`)
	cache, _ := json.Marshal(n.SnapshotCache)
	draining := 0
	if n.Draining {
		draining = 1
	}
	_, err := r.db.ExecContext(ctx, q,
		n.ID, n.OrgID, n.Name, n.Region, n.Tier, boolInt(n.BYO), n.APIAddr,
		n.MemTotalMiB, n.MemAllocMiB, n.LiveSandboxes, string(cache), draining,
		n.EnrolledAt.UnixMilli(), n.LastHeartbeat.UnixMilli())
	return err
}

func (r *Registry) Get(ctx context.Context, id string) (*Node, error) {
	return scanNode(r.db.QueryRowContext(ctx, pgRewrite(`SELECT `+nodeCols+` FROM nodes WHERE id = ?`), id))
}

// Heartbeat updates ONLY the fields a heartbeat owns, in one statement.
// The old read-modify-write via Get+Register replayed stale name/region/
// mem_total values over concurrent enrollment changes (lost update).
// Returns false when the node is unknown (caller answers 404 → re-enroll).
func (r *Registry) Heartbeat(ctx context.Context, nodeID, apiAddr string, memAllocMiB, liveSandboxes int, snapshotCache []string, at time.Time) (bool, error) {
	cache, _ := json.Marshal(snapshotCache)
	q := pgRewrite(`UPDATE nodes SET
  api_addr = CASE WHEN ? = '' THEN api_addr ELSE ? END,
  mem_alloc_mib = ?, live_sandboxes = ?, snapshot_cache = ?, last_heartbeat_ms = ?
WHERE id = ?`)
	res, err := r.db.ExecContext(ctx, q,
		apiAddr, apiAddr, memAllocMiB, liveSandboxes, string(cache), at.UnixMilli(), nodeID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (r *Registry) List(ctx context.Context, region string) ([]*Node, error) {
	q := `SELECT ` + nodeCols + ` FROM nodes`
	var args []any
	if region != "" {
		q += ` WHERE region = ?`
		args = append(args, region)
	}
	rows, err := r.db.QueryContext(ctx, pgRewrite(q), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (r *Registry) SetDraining(ctx context.Context, id string, drain bool) error {
	d := 0
	if drain {
		d = 1
	}
	_, err := r.db.ExecContext(ctx, pgRewrite(`UPDATE nodes SET draining = ? WHERE id = ?`), d, id)
	return err
}

func (r *Registry) Delete(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, pgRewrite(`DELETE FROM nodes WHERE id = ?`), id)
	return err
}

// Place runs the scheduler (plan §5.2): filter (capacity, tier, region,
// draining) then score = w1·snapshot-cache-hit + w2·pack-tightness. O(nodes).
func (r *Registry) Place(ctx context.Context, req PlacementRequest) (*Node, error) {
	nodes, err := r.List(ctx, req.Region)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var best *Node
	bestScore := -1e9
	for _, n := range nodes {
		if n.Draining || n.Tier != req.Tier && req.Tier != "" {
			continue
		}
		if n.OrgID != "" && req.OrgID != "" && n.OrgID != req.OrgID {
			continue // BYO nodes only ever run their owner's org (§5.3)
		}
		if now.Sub(n.LastHeartbeat) > 45*time.Second {
			continue // stale heartbeat: not schedulable
		}
		free := n.MemTotalMiB - n.MemAllocMiB
		if free < req.MemMiB {
			continue
		}
		score := 0.0
		for _, t := range n.SnapshotCache {
			if t == req.TemplateID {
				score += 100 // snapshot-cache hit dominates (biggest cold-start lever)
				break
			}
		}
		packTightness := float64(n.MemAllocMiB) / float64(max(1, n.MemTotalMiB))
		score += packTightness * 20
		if score > bestScore {
			bestScore = score
			best = n
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no schedulable node for region=%q tier=%q mem=%dMiB", req.Region, req.Tier, req.MemMiB)
	}
	return best, nil
}

type PlacementRequest struct {
	Region     string
	Tier       string
	OrgID      string
	TemplateID string
	MemMiB     int
}

func (r *Registry) AssignPlacement(ctx context.Context, sandboxID, nodeID, orgID string) error {
	_, err := r.db.ExecContext(ctx, pgRewrite(`INSERT INTO placements (sandbox_id, node_id, placed_at, org_id) VALUES (?, ?, ?, ?)
ON CONFLICT(sandbox_id) DO UPDATE SET node_id=excluded.node_id, org_id=excluded.org_id`),
		sandboxID, nodeID, time.Now().UnixMilli(), orgID)
	return err
}

// Placement returns the node AND the owning org. Callers must check the org
// before forwarding: the cell speaks to nodes with a privileged credential, so
// it is the only place tenancy can be enforced on the proxy path.
func (r *Registry) PlacementOwner(ctx context.Context, sandboxID string) (nodeID, orgID string, ok bool) {
	err := r.db.QueryRowContext(ctx, pgRewrite(`SELECT node_id, org_id FROM placements WHERE sandbox_id = ?`), sandboxID).Scan(&nodeID, &orgID)
	if err != nil {
		return "", "", false
	}
	return nodeID, orgID, true
}

func (r *Registry) Placement(ctx context.Context, sandboxID string) (string, bool) {
	var nodeID string
	err := r.db.QueryRowContext(ctx, pgRewrite(`SELECT node_id FROM placements WHERE sandbox_id = ?`), sandboxID).Scan(&nodeID)
	if err != nil {
		return "", false
	}
	return nodeID, true
}

func (r *Registry) DropPlacement(ctx context.Context, sandboxID string) error {
	_, err := r.db.ExecContext(ctx, pgRewrite(`DELETE FROM placements WHERE sandbox_id = ?`), sandboxID)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = strings.TrimSpace
var _ = store.ErrNotFound
