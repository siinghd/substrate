package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Store is the full persistence surface. Implementations: SQLite (standalone,
// dev) and Postgres (cell). All methods are safe for concurrent use.
//
// NOTE (pass-11 audit): UpdateSandboxState, BeginTx/Tx and DeletePorts were
// removed — zero production callers, each superseded (UpdateSandbox +
// TouchSandbox; DeleteSandbox's inline cascade). Resurrect from history if a
// real consumer appears; do not re-add speculatively.
type Store interface {
	CreateOrg(ctx context.Context, o *Org) error
	GetOrg(ctx context.Context, id string) (*Org, error)
	ListOrgs(ctx context.Context) ([]*Org, error)
	SetOrgSuspended(ctx context.Context, id string, suspended bool, reason string) error

	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, id string) (*Project, error)
	ListProjects(ctx context.Context, orgID string) ([]*Project, error)
	DeleteProject(ctx context.Context, id string) error

	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)
	RevokeAPIKey(ctx context.Context, id string) error
	ListAPIKeys(ctx context.Context, orgID string) ([]*APIKey, error)

	PutTemplate(ctx context.Context, t *Template) error
	GetTemplate(ctx context.Context, id string) (*Template, error)
	GetTemplateByName(ctx context.Context, projectID, name string) (*Template, error)
	ListTemplates(ctx context.Context, projectID string) ([]*Template, error)
	DeleteTemplate(ctx context.Context, id string) error

	CreateSandbox(ctx context.Context, s *Sandbox) error
	GetSandbox(ctx context.Context, id string) (*Sandbox, error)
	UpdateSandbox(ctx context.Context, s *Sandbox) error
	DeleteSandbox(ctx context.Context, id string) error
	ListSandboxes(ctx context.Context, f SandboxFilter) ([]*Sandbox, error)
	CountSandboxesByUser(ctx context.Context, projectID, externalUserID string) (int, error)
	CountSandboxesByProject(ctx context.Context, projectID string) (int, error)
	// SandboxesForReconcile returns the narrow projection the dispatch loop
	// needs. See ReconcileRow.
	SandboxesForReconcile(ctx context.Context) ([]ReconcileRow, error)
	// SumMemMiB totals configured memory for the given sandboxes. Callers hold
	// the live set in memory, so this is O(live), not O(defined) — the node
	// heartbeat used to full-scan the table every 15s to compute one integer.
	SumMemMiB(ctx context.Context, ids []string) (int, error)
	// DistinctTemplateIDs returns the templates referenced by non-destroyed
	// sandboxes, for snapshot-cache affinity hints.
	DistinctTemplateIDs(ctx context.Context) ([]string, error)

	UpsertPort(ctx context.Context, sandboxID string, p PortMapping) error
	DeletePort(ctx context.Context, sandboxID string, port int, proto string) error
	ListPorts(ctx context.Context, sandboxID string) ([]PortMapping, error)

	CreateExec(ctx context.Context, e *Exec) error
	GetExec(ctx context.Context, id string) (*Exec, error)
	UpdateExec(ctx context.Context, e *Exec) error
	GetExecByIdempotencyKey(ctx context.Context, key string) (*Exec, error)
	ListExecs(ctx context.Context, sandboxID string) ([]*Exec, error)

	CreateSnapshot(ctx context.Context, s *Snapshot) error
	GetSnapshot(ctx context.Context, id string) (*Snapshot, error)
	ListSnapshots(ctx context.Context, sandboxID string) ([]*Snapshot, error)
	AcquireLineageLock(ctx context.Context, lineageID, holder string) (bool, error)
	ReleaseLineageLock(ctx context.Context, lineageID, holder string) error

	AppendEvent(ctx context.Context, e *Event) error
	// AppendEvents writes a batch in one statement per chunk. The supervisor
	// emits a burst of lifecycle events per sandbox transition; one INSERT per
	// event made the drain slower than the producer, which is what let the
	// in-process ring overflow and silently discard audit records.
	AppendEvents(ctx context.Context, evs []*Event) error
	ListEvents(ctx context.Context, f EventFilter) ([]*Event, error)

	GetIdempotency(ctx context.Context, key, path string) (*IdempotencyRecord, error)
	PutIdempotency(ctx context.Context, rec *IdempotencyRecord) error
	ClaimIdempotency(ctx context.Context, key, path, reqHash string) (bool, error)
	CompleteIdempotency(ctx context.Context, key, path string, status int, body, reqHash string) error
	DeleteIdempotency(ctx context.Context, key, path string) error
	PruneBefore(ctx context.Context, cutoff time.Time) (int64, error)
	InsertMetricSnapshots(ctx context.Context, snaps []MetricSnapshot) error
	ListMetricSnapshots(ctx context.Context, name string, limit int) ([]MetricSnapshot, error)
	InsertUsageSample(ctx context.Context, u UsageSample) error
	InsertUsageSamples(ctx context.Context, us []UsageSample) error
	TouchSandbox(ctx context.Context, id string, atMS int64) error
	UsageRollup(ctx context.Context, f UsageFilter) ([]UsageRow, error)

	Close() error
}

type Org struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	// SuspendedAt disables an org: no new sandboxes, and the reconciler
	// destroys its running ones. This is the lever for abuse response --
	// mining, outbound scanning, spam -- and for non-payment. Nil means active.
	SuspendedAt *time.Time `json:"suspended_at,omitempty"`
	// SuspendReason is shown to the tenant, so "why am I blocked" does not
	// require a support ticket.
	SuspendReason string `json:"suspend_reason,omitempty"`
}

type Project struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	Name         string    `json:"name"`
	MaxSandboxes int       `json:"max_sandboxes"`
	CreatedAt    time.Time `json:"created_at"`
}

type SandboxFilter struct {
	ProjectID      string
	OrgID          string
	ExternalUserID string
	State          string
	Limit          int
}

type EventFilter struct {
	OrgID     string
	ProjectID string
	SandboxID string
	Type      string
	After     time.Time
	Limit     int
}

type IdempotencyRecord struct {
	Key       string
	Path      string
	ReqHash   string
	Status    int
	Body      string
	CreatedAt time.Time
}

// MetricSnapshot is one timestamped percentile reading of a histogram,
// persisted so latency trends survive restarts and can be graphed from SQL.
type MetricSnapshot struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	Count  uint64            `json:"count"`
	Sum    float64           `json:"sum"`
	P50    float64           `json:"p50"`
	P95    float64           `json:"p95"`
	P99    float64           `json:"p99"`
	TS     time.Time         `json:"ts"`
}

// ReconcileRow is the NARROW projection the reconciler's dispatch loop needs.
// Loading full Sandbox rows every 2 seconds cost 130MB and 4.5M allocations per
// pass at 100k defined sandboxes — 65MB/s of garbage, forever, on every node.
// The worker re-reads the full row for the one sandbox it acts on, so nothing
// downstream needs the other 16 columns.
type ReconcileRow struct {
	ID              string
	Desired         string
	State           SandboxState
	TTLIdleSecs     int
	TTLAbsoluteSecs int
	CreatedAt       time.Time
	// OrgSuspended comes from a JOIN, not a per-sandbox lookup.
	//
	// needsConverge is a pure, I/O-free predicate so the dispatch loop stays
	// cheap at 100k defined sandboxes. Suspension therefore has to arrive with
	// the row: without it, a HEALTHY running sandbox is never enqueued at all
	// (the predicate returns false unless it has an idle TTL), so a suspended
	// org's workload would keep running forever.
	OrgSuspended bool
}

type UsageFilter struct {
	OrgID     string
	ProjectID string
	GroupBy   string // external_user_id | sandbox_id | project_id
	From, To  time.Time
	// Limit caps the number of groups returned; After is a keyset cursor on
	// the group column. Without them a rollup grouped by external_user_id
	// returned one row per end user — 100k rows in a single JSON response.
	Limit int
	After string
}

type UsageRow struct {
	Group          string  `json:"group"`
	RunningSeconds float64 `json:"running_seconds"`
	CPUSeconds     float64 `json:"cpu_seconds"`
	MemPeakKB      int64   `json:"mem_peak_kb"`
	RxBytes        int64   `json:"rx_bytes"`
	TxBytes        int64   `json:"tx_bytes"`
}

// ---- shared SQL engine -------------------------------------------------

// sqlStore implements Store over database/sql with a dialect flag. Postgres
// gets ?-placeholders rewritten to $n.
type sqlStore struct {
	db      *sql.DB
	dialect string // sqlite | postgres
}

func rewrite(dialect, q string) string {
	if dialect != "postgres" {
		return q
	}
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

func (s *sqlStore) exec(ctx context.Context, q string, args ...any) (sql.Result, error) {
	return s.db.ExecContext(ctx, rewrite(s.dialect, q), args...)
}

func (s *sqlStore) query(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	return s.db.QueryContext(ctx, rewrite(s.dialect, q), args...)
}

func (s *sqlStore) queryRow(ctx context.Context, q string, args ...any) *sql.Row {
	return s.db.QueryRowContext(ctx, rewrite(s.dialect, q), args...)
}

func nullTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return ms(*t)
}

func scanNullTime(src any) *time.Time {
	if src == nil {
		return nil
	}
	switch v := src.(type) {
	case int64:
		t := fromMS(v)
		return &t
	}
	return nil
}
