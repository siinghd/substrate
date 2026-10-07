package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

func OpenSQLite(path string) (Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &sqlStore{db: db, dialect: "sqlite"}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenPostgres opens the cell's store.
//
// The pgx driver is registered by THIS package (see the blank import at the top
// of the file), not by callers. It used to be registered only in cmd/cell, so
// any other consumer of OpenPostgres -- including a test -- got
// `sql: unknown driver "pgx"` at runtime with nothing to point at the cause.
// The package that opens the database should be the package that registers its
// driver.
func OpenPostgres(dsn string) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	s := &sqlStore{db: db, dialect: "postgres"}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *sqlStore) Close() error { return s.db.Close() }

// ---- orgs / projects / keys ----

func (s *sqlStore) CreateOrg(ctx context.Context, o *Org) error {
	_, err := s.exec(ctx, `INSERT INTO orgs (id, name, created_at) VALUES (?, ?, ?)`, o.ID, o.Name, ms(o.CreatedAt))
	return err
}

func (s *sqlStore) GetOrg(ctx context.Context, id string) (*Org, error) {
	row := s.queryRow(ctx, `SELECT id, name, created_at, suspended_at, suspend_reason FROM orgs WHERE id = ?`, id)
	var o Org
	var created, susp int64
	if err := row.Scan(&o.ID, &o.Name, &created, &susp, &o.SuspendReason); err != nil {
		return nil, mapErr(err)
	}
	o.CreatedAt = fromMS(created)
	if susp > 0 {
		t := fromMS(susp)
		o.SuspendedAt = &t
	}
	return &o, nil
}

// SetOrgSuspended is the abuse and non-payment lever. Suspension is a state on
// the org, not a deletion: it is reversible, it keeps the audit trail, and it
// leaves usage records intact so a disputed bill can still be reconstructed.
func (s *sqlStore) SetOrgSuspended(ctx context.Context, id string, suspended bool, reason string) error {
	var at int64
	if suspended {
		at = ms(time.Now())
		if at == 0 {
			at = 1 // never let a real suspension read as "active"
		}
	} else {
		reason = ""
	}
	res, err := s.exec(ctx, `UPDATE orgs SET suspended_at = ?, suspend_reason = ? WHERE id = ?`, at, reason, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqlStore) ListOrgs(ctx context.Context) ([]*Org, error) {
	rows, err := s.query(ctx, `SELECT id, name, created_at, suspended_at, suspend_reason FROM orgs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Org
	for rows.Next() {
		var o Org
		var created, susp int64
		if err := rows.Scan(&o.ID, &o.Name, &created, &susp, &o.SuspendReason); err != nil {
			return nil, err
		}
		o.CreatedAt = fromMS(created)
		if susp > 0 {
			t := fromMS(susp)
			o.SuspendedAt = &t
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (s *sqlStore) CreateProject(ctx context.Context, p *Project) error {
	_, err := s.exec(ctx, `INSERT INTO projects (id, org_id, name, max_sandboxes, created_at) VALUES (?, ?, ?, ?, ?)`,
		p.ID, p.OrgID, p.Name, p.MaxSandboxes, ms(p.CreatedAt))
	return err
}

func (s *sqlStore) GetProject(ctx context.Context, id string) (*Project, error) {
	row := s.queryRow(ctx, `SELECT id, org_id, name, max_sandboxes, created_at FROM projects WHERE id = ?`, id)
	var p Project
	var created int64
	if err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.MaxSandboxes, &created); err != nil {
		return nil, mapErr(err)
	}
	p.CreatedAt = fromMS(created)
	return &p, nil
}

func (s *sqlStore) ListProjects(ctx context.Context, orgID string) ([]*Project, error) {
	q := `SELECT id, org_id, name, max_sandboxes, created_at FROM projects`
	var args []any
	if orgID != "" {
		q += ` WHERE org_id = ?`
		args = append(args, orgID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		var p Project
		var created int64
		if err := rows.Scan(&p.ID, &p.OrgID, &p.Name, &p.MaxSandboxes, &created); err != nil {
			return nil, err
		}
		p.CreatedAt = fromMS(created)
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *sqlStore) DeleteProject(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM projects WHERE id = ?`, id)
	return err
}

func (s *sqlStore) CreateAPIKey(ctx context.Context, k *APIKey) error {
	_, err := s.exec(ctx, `INSERT INTO api_keys (id, org_id, project_id, key_hash, kind, created_at, revoked_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.OrgID, k.ProjectID, k.KeyHash, k.Kind, ms(k.CreatedAt), nullTime(k.RevokedAt))
	return err
}

func (s *sqlStore) GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error) {
	row := s.queryRow(ctx, `SELECT id, org_id, project_id, key_hash, kind, created_at, revoked_at FROM api_keys WHERE key_hash = ?`, hash)
	return scanKey(row)
}

func (s *sqlStore) RevokeAPIKey(ctx context.Context, id string) error {
	now := time.Now()
	_, err := s.exec(ctx, `UPDATE api_keys SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`, ms(now), id)
	return err
}

func (s *sqlStore) ListAPIKeys(ctx context.Context, orgID string) ([]*APIKey, error) {
	rows, err := s.query(ctx, `SELECT id, org_id, project_id, key_hash, kind, created_at, revoked_at FROM api_keys WHERE org_id = ? ORDER BY created_at`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

type rowScanner interface{ Scan(dest ...any) error }

func scanKey(row rowScanner) (*APIKey, error) {
	var k APIKey
	var created int64
	var revoked any
	if err := row.Scan(&k.ID, &k.OrgID, &k.ProjectID, &k.KeyHash, &k.Kind, &created, &revoked); err != nil {
		return nil, mapErr(err)
	}
	k.CreatedAt = fromMS(created)
	switch v := revoked.(type) {
	case int64:
		t := fromMS(v)
		k.RevokedAt = &t
	}
	return &k, nil
}

// ---- templates ----

// PutTemplate inserts or updates a template. The upsert is SCOPED TO THE
// OWNING PROJECT: without the WHERE clause, ON CONFLICT(id) DO UPDATE let any
// caller who supplied an existing template id overwrite another tenant's row —
// including its rootfs_path, which every sandbox built from that template
// boots. That is arbitrary control of another tenant's VM image, so a
// cross-project write must fail loudly rather than silently succeed.
func (s *sqlStore) PutTemplate(ctx context.Context, t *Template) error {
	res, err := s.exec(ctx, `INSERT INTO templates (id, project_id, name, rootfs_path, rootfs_size, kernel_path, mem_mib, vcpus, net_policy, warm_cmd, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, rootfs_path=excluded.rootfs_path, rootfs_size=excluded.rootfs_size,
  kernel_path=excluded.kernel_path, mem_mib=excluded.mem_mib, vcpus=excluded.vcpus, net_policy=excluded.net_policy, warm_cmd=excluded.warm_cmd
WHERE templates.project_id = excluded.project_id`,
		t.ID, t.ProjectID, t.Name, t.RootfsPath, t.RootfsSize, t.KernelPath, t.MemMiB, t.VCPUs, t.NetPolicy, t.WarmCmd, ms(t.CreatedAt))
	if err != nil {
		return mapErr(err)
	}
	// The guarded DO UPDATE touches no row when the id belongs to another
	// project. Reporting success there would be the same silent cross-tenant
	// write, just without the corruption.
	if n, aerr := res.RowsAffected(); aerr == nil && n == 0 {
		return ErrConflict
	}
	return nil
}

const templateCols = `id, project_id, name, rootfs_path, rootfs_size, kernel_path, mem_mib, vcpus, net_policy, warm_cmd, created_at`

func scanTemplate(row rowScanner) (*Template, error) {
	var t Template
	var created int64
	if err := row.Scan(&t.ID, &t.ProjectID, &t.Name, &t.RootfsPath, &t.RootfsSize, &t.KernelPath, &t.MemMiB, &t.VCPUs, &t.NetPolicy, &t.WarmCmd, &created); err != nil {
		return nil, mapErr(err)
	}
	t.CreatedAt = fromMS(created)
	return &t, nil
}

func (s *sqlStore) GetTemplate(ctx context.Context, id string) (*Template, error) {
	return scanTemplate(s.queryRow(ctx, `SELECT `+templateCols+` FROM templates WHERE id = ?`, id))
}

func (s *sqlStore) GetTemplateByName(ctx context.Context, projectID, name string) (*Template, error) {
	// Deterministic pick: there is no UNIQUE(project_id, name), so re-registering
	// a name leaves several rows and an unordered query returned an arbitrary
	// one. Newest wins, which is what "register this template again" means.
	return scanTemplate(s.queryRow(ctx, `SELECT `+templateCols+` FROM templates WHERE project_id = ? AND name = ? ORDER BY created_at DESC LIMIT 1`, projectID, name))
}

func (s *sqlStore) ListTemplates(ctx context.Context, projectID string) ([]*Template, error) {
	rows, err := s.query(ctx, `SELECT `+templateCols+` FROM templates WHERE project_id = ? ORDER BY created_at`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Template
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *sqlStore) DeleteTemplate(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `DELETE FROM templates WHERE id = ?`, id)
	return err
}

// ---- sandboxes ----

func envJSON(env map[string]string) string {
	b, _ := json.Marshal(env)
	return string(b)
}

func parseEnv(s string) map[string]string {
	m := map[string]string{}
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

const sandboxCols = `id, org_id, project_id, template_id, external_user_id, desired, state, tier, node_id,
mem_mib, vcpus, env, lineage_id, pause_ref, error, ttl_idle_secs, ttl_absolute_secs, last_activity_ms,
created_at, updated_at, started_at, paused_at`

func scanSandbox(row rowScanner) (*Sandbox, error) {
	var sb Sandbox
	var createdAt, updatedAt int64
	var startedAt, pausedAt any
	var envStr string
	if err := row.Scan(&sb.ID, &sb.OrgID, &sb.ProjectID, &sb.TemplateID, &sb.ExternalUserID, &sb.Desired, &sb.State, &sb.Tier, &sb.NodeID,
		&sb.MemMiB, &sb.VCPUs, &envStr, &sb.LineageID, &sb.PauseRef, &sb.Error, &sb.TTLIdleSecs, &sb.TTLAbsoluteSecs, &sb.LastActivityMS,
		&createdAt, &updatedAt, &startedAt, &pausedAt); err != nil {
		return nil, mapErr(err)
	}
	sb.Env = parseEnv(envStr)
	sb.CreatedAt = fromMS(createdAt)
	sb.UpdatedAt = fromMS(updatedAt)
	sb.StartedAt = scanNullTime(startedAt)
	sb.PausedAt = scanNullTime(pausedAt)
	return &sb, nil
}

// ttlDeadlineMS materialises the absolute-TTL deadline so expiry is an indexed
// range scan rather than an unindexable arithmetic predicate. 0 means none.
func ttlDeadlineMS(sb *Sandbox) int64 {
	if sb.TTLAbsoluteSecs <= 0 {
		return 0
	}
	return ms(sb.CreatedAt) + int64(sb.TTLAbsoluteSecs)*1000
}

func (s *sqlStore) CreateSandbox(ctx context.Context, sb *Sandbox) error {
	_, err := s.exec(ctx, `INSERT INTO sandboxes (`+sandboxCols+`, ttl_deadline_ms) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sb.ID, sb.OrgID, sb.ProjectID, sb.TemplateID, sb.ExternalUserID, sb.Desired, string(sb.State), sb.Tier, sb.NodeID,
		sb.MemMiB, sb.VCPUs, envJSON(sb.Env), sb.LineageID, sb.PauseRef, sb.Error, sb.TTLIdleSecs, sb.TTLAbsoluteSecs, sb.LastActivityMS,
		ms(sb.CreatedAt), ms(sb.UpdatedAt), nullTime(sb.StartedAt), nullTime(sb.PausedAt), ttlDeadlineMS(sb))
	return err
}

func (s *sqlStore) GetSandbox(ctx context.Context, id string) (*Sandbox, error) {
	return scanSandbox(s.queryRow(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id = ?`, id))
}

func (s *sqlStore) UpdateSandbox(ctx context.Context, sb *Sandbox) error {
	_, err := s.exec(ctx, `UPDATE sandboxes SET desired=?, state=?, tier=?, node_id=?, mem_mib=?, vcpus=?, env=?,
lineage_id=?, pause_ref=?, error=?, ttl_idle_secs=?, ttl_absolute_secs=?, last_activity_ms=?, updated_at=?, started_at=?, paused_at=?,
ttl_deadline_ms=? WHERE id=?`,
		sb.Desired, string(sb.State), sb.Tier, sb.NodeID, sb.MemMiB, sb.VCPUs, envJSON(sb.Env),
		sb.LineageID, sb.PauseRef, sb.Error, sb.TTLIdleSecs, sb.TTLAbsoluteSecs, sb.LastActivityMS, ms(time.Now()),
		nullTime(sb.StartedAt), nullTime(sb.PausedAt), ttlDeadlineMS(sb), sb.ID)
	return err
}

func (s *sqlStore) DeleteSandbox(ctx context.Context, id string) error {
	for _, q := range []string{
		`DELETE FROM sandboxes WHERE id = ?`,
		`DELETE FROM ports WHERE sandbox_id = ?`,
		`DELETE FROM execs WHERE sandbox_id = ?`,
	} {
		if _, err := s.exec(ctx, q, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlStore) ListSandboxes(ctx context.Context, f SandboxFilter) ([]*Sandbox, error) {
	q := `SELECT ` + sandboxCols + ` FROM sandboxes`
	var where []string
	var args []any
	if f.ProjectID != "" {
		where = append(where, `project_id = ?`)
		args = append(args, f.ProjectID)
	}
	if f.OrgID != "" {
		where = append(where, `org_id = ?`)
		args = append(args, f.OrgID)
	}
	if f.ExternalUserID != "" {
		where = append(where, `external_user_id = ?`)
		args = append(args, f.ExternalUserID)
	}
	if f.State != "" {
		where = append(where, `state = ?`)
		args = append(args, f.State)
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY created_at DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}
	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Sandbox
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// liveStateList enumerates the states a quota counts, as a positive IN list.
// `state NOT IN ('destroyed','destroying')` forced the planner to walk every
// index entry for the project; an IN list over the enumerated states lets it
// seek each state range. Measured at 100k rows in one project: 30.6ms -> 13.3ms.
// It is exhaustive by construction — see the SandboxState constants.
const liveStateList = `('creating','running','paused','archived','error')`

func (s *sqlStore) CountSandboxesByUser(ctx context.Context, projectID, externalUserID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM sandboxes WHERE project_id = ? AND external_user_id = ? AND state IN `+liveStateList,
		projectID, externalUserID).Scan(&n)
	return n, err
}

func (s *sqlStore) CountSandboxesByProject(ctx context.Context, projectID string) (int, error) {
	var n int
	err := s.queryRow(ctx, `SELECT COUNT(*) FROM sandboxes WHERE project_id = ? AND state IN `+liveStateList, projectID).Scan(&n)
	return n, err
}

// SandboxesForReconcile returns only rows that could possibly need work.
//
// It used to return EVERY non-destroyed sandbox — at 100k defined that was
// 1.75s and 130MB of allocation every 2 seconds, per node, forever. Most of
// those rows were paused, and a paused sandbox needs nothing from the
// reconciler: convergence starts it only on an API touch. The two exceptions
// are handled by their own indexed queries.
//
// Cost is now O(non-paused rows + rows desired-destroyed + rows past their
// absolute TTL) instead of O(defined sandboxes); every predicate is indexed.
func (s *sqlStore) SandboxesForReconcile(ctx context.Context) ([]ReconcileRow, error) {
	// The org join costs one predicate on an indexed primary key and saves a
	// per-sandbox query in the reconciler's hot loop.
	const cols = `s.id, s.desired, s.state, s.ttl_idle_secs, s.ttl_absolute_secs, s.created_at,
	  CASE WHEN o.suspended_at > 0 THEN 1 ELSE 0 END`

	// Everything not paused and not destroyed: the resident and transitional
	// set, which is what convergence actually acts on.
	active := []SandboxState{StateCreating, StateRunning, StateArchived, StateDestroying, StateError}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(active)), ",")
	args := make([]any, 0, len(active))
	for _, st := range active {
		args = append(args, string(st))
	}

	out := make([]ReconcileRow, 0, 256)
	seen := make(map[string]struct{})
	collect := func(q string, a ...any) error {
		rows, err := s.query(ctx, q, a...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ReconcileRow
			var state string
			var created int64
			var susp int
			if err := rows.Scan(&r.ID, &r.Desired, &state, &r.TTLIdleSecs, &r.TTLAbsoluteSecs, &created, &susp); err != nil {
				return err
			}
			if _, dup := seen[r.ID]; dup {
				continue
			}
			seen[r.ID] = struct{}{}
			r.State = SandboxState(state)
			r.CreatedAt = fromMS(created)
			r.OrgSuspended = susp == 1
			out = append(out, r)
		}
		return rows.Err()
	}

	const from = ` FROM sandboxes s LEFT JOIN orgs o ON o.id = s.org_id `
	if err := collect(`SELECT `+cols+from+`WHERE s.state IN (`+ph+`)`, args...); err != nil {
		return nil, err
	}
	// A paused sandbox still has to be torn down when its desired state says so.
	if err := collect(`SELECT `+cols+from+`WHERE s.desired = ? AND s.state = ?`,
		"destroyed", string(StatePaused)); err != nil {
		return nil, err
	}
	// ...and when its absolute TTL expires, or pausing would evade every TTL.
	if err := collect(`SELECT `+cols+from+`WHERE s.state = ? AND s.ttl_deadline_ms > 0 AND s.ttl_deadline_ms <= ?`,
		string(StatePaused), ms(time.Now())); err != nil {
		return nil, err
	}
	return out, nil
}

// sqlChunk bounds placeholders per statement; SQLite's default parameter limit
// is conservative and Postgres has its own ceiling.
const sqlChunk = 100

func (s *sqlStore) SumMemMiB(ctx context.Context, ids []string) (int, error) {
	total := 0
	for start := 0; start < len(ids); start += sqlChunk {
		end := start + sqlChunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		var sum sql.NullInt64
		if err := s.queryRow(ctx, `SELECT COALESCE(SUM(mem_mib), 0) FROM sandboxes WHERE id IN (`+ph+`)`, args...).Scan(&sum); err != nil {
			return 0, err
		}
		total += int(sum.Int64)
	}
	return total, nil
}

func (s *sqlStore) DistinctTemplateIDs(ctx context.Context) ([]string, error) {
	states := []SandboxState{StateCreating, StateRunning, StatePaused, StateArchived, StateDestroying, StateError}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(states)), ",")
	args := make([]any, 0, len(states))
	for _, st := range states {
		args = append(args, string(st))
	}
	rows, err := s.query(ctx, `SELECT DISTINCT template_id FROM sandboxes WHERE state IN (`+ph+`) AND template_id <> ''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ---- ports ----

func (s *sqlStore) UpsertPort(ctx context.Context, sandboxID string, p PortMapping) error {
	_, err := s.exec(ctx, `INSERT INTO ports (sandbox_id, port, proto, public, token) VALUES (?, ?, ?, ?, ?)
ON CONFLICT(sandbox_id, port, proto) DO UPDATE SET public=excluded.public, token=excluded.token`,
		sandboxID, p.Port, p.Proto, boolInt(p.Public), p.Token)
	return err
}

func (s *sqlStore) DeletePort(ctx context.Context, sandboxID string, port int, proto string) error {
	_, err := s.exec(ctx, `DELETE FROM ports WHERE sandbox_id = ? AND port = ? AND proto = ?`, sandboxID, port, proto)
	return err
}

func (s *sqlStore) ListPorts(ctx context.Context, sandboxID string) ([]PortMapping, error) {
	rows, err := s.query(ctx, `SELECT sandbox_id, port, proto, public, token FROM ports WHERE sandbox_id = ? ORDER BY port`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PortMapping
	for rows.Next() {
		var p PortMapping
		var sid string
		var pub int
		if err := rows.Scan(&sid, &p.Port, &p.Proto, &pub, &p.Token); err != nil {
			return nil, err
		}
		p.Public = pub == 1
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- execs ----

func (s *sqlStore) CreateExec(ctx context.Context, e *Exec) error {
	argv, _ := json.Marshal(e.Argv)
	_, err := s.exec(ctx, `INSERT INTO execs (id, sandbox_id, argv, state, exit_code, tty, log_path, idempotency_key, created_at, finished_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.SandboxID, string(argv), e.State, e.ExitCode, boolInt(e.TTY), e.LogPath, e.IdempotencyKey, ms(e.CreatedAt), nullTime(e.FinishedAt))
	return err
}

const execCols = `id, sandbox_id, argv, state, exit_code, tty, log_path, idempotency_key, created_at, finished_at`

func scanExec(row rowScanner) (*Exec, error) {
	var e Exec
	var argv string
	var created int64
	var finished any
	if err := row.Scan(&e.ID, &e.SandboxID, &argv, &e.State, &e.ExitCode, &e.TTY, &e.LogPath, &e.IdempotencyKey, &created, &finished); err != nil {
		return nil, mapErr(err)
	}
	_ = json.Unmarshal([]byte(argv), &e.Argv)
	e.CreatedAt = fromMS(created)
	e.FinishedAt = scanNullTime(finished)
	return &e, nil
}

func (s *sqlStore) GetExec(ctx context.Context, id string) (*Exec, error) {
	return scanExec(s.queryRow(ctx, `SELECT `+execCols+` FROM execs WHERE id = ?`, id))
}

func (s *sqlStore) UpdateExec(ctx context.Context, e *Exec) error {
	argv, _ := json.Marshal(e.Argv)
	_, err := s.exec(ctx, `UPDATE execs SET argv=?, state=?, exit_code=?, tty=?, log_path=?, finished_at=? WHERE id=?`,
		string(argv), e.State, e.ExitCode, boolInt(e.TTY), e.LogPath, nullTime(e.FinishedAt), e.ID)
	return err
}

func (s *sqlStore) GetExecByIdempotencyKey(ctx context.Context, key string) (*Exec, error) {
	return scanExec(s.queryRow(ctx, `SELECT `+execCols+` FROM execs WHERE idempotency_key = ?`, key))
}

func (s *sqlStore) ListExecs(ctx context.Context, sandboxID string) ([]*Exec, error) {
	rows, err := s.query(ctx, `SELECT `+execCols+` FROM execs WHERE sandbox_id = ? ORDER BY created_at DESC LIMIT 200`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Exec
	for rows.Next() {
		e, err := scanExec(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---- snapshots / lineage ----

func (s *sqlStore) CreateSnapshot(ctx context.Context, sn *Snapshot) error {
	_, err := s.exec(ctx, `INSERT INTO snapshots (id, sandbox_id, kind, ref, size_bytes, shared, lineage_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sn.ID, sn.SandboxID, sn.Kind, sn.Ref, sn.SizeBytes, boolInt(sn.Shared), sn.LineageID, ms(sn.CreatedAt))
	return err
}

const snapshotCols = `id, sandbox_id, kind, ref, size_bytes, shared, lineage_id, created_at`

func scanSnapshot(row rowScanner) (*Snapshot, error) {
	var sn Snapshot
	var created int64
	var shared int
	if err := row.Scan(&sn.ID, &sn.SandboxID, &sn.Kind, &sn.Ref, &sn.SizeBytes, &shared, &sn.LineageID, &created); err != nil {
		return nil, mapErr(err)
	}
	sn.Shared = shared == 1
	sn.CreatedAt = fromMS(created)
	return &sn, nil
}

func (s *sqlStore) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	return scanSnapshot(s.queryRow(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE id = ?`, id))
}

func (s *sqlStore) ListSnapshots(ctx context.Context, sandboxID string) ([]*Snapshot, error) {
	rows, err := s.query(ctx, `SELECT `+snapshotCols+` FROM snapshots WHERE sandbox_id = ? ORDER BY created_at DESC`, sandboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Snapshot
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

func (s *sqlStore) AcquireLineageLock(ctx context.Context, lineageID, holder string) (bool, error) {
	res, err := s.exec(ctx, `INSERT INTO lineage_locks (lineage_id, holder, acquired_ms) VALUES (?, ?, ?)
ON CONFLICT(lineage_id) DO NOTHING`, lineageID, holder, ms(time.Now()))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *sqlStore) ReleaseLineageLock(ctx context.Context, lineageID, holder string) error {
	_, err := s.exec(ctx, `DELETE FROM lineage_locks WHERE lineage_id = ? AND holder = ?`, lineageID, holder)
	return err
}

// ---- events ----

func (s *sqlStore) AppendEvent(ctx context.Context, e *Event) error {
	return s.AppendEvents(ctx, []*Event{e})
}

// AppendEvents writes events in chunks of sqlChunk rows per statement:
// O(ceil(n/chunk)) round trips instead of n.
func (s *sqlStore) AppendEvents(ctx context.Context, evs []*Event) error {
	for start := 0; start < len(evs); start += sqlChunk {
		end := start + sqlChunk
		if end > len(evs) {
			end = len(evs)
		}
		batch := evs[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO events (id, ts, org_id, project_id, sandbox_id, type, data) VALUES `)
		args := make([]any, 0, len(batch)*7)
		for i, e := range batch {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?, ?, ?, ?, ?, ?, ?)`)
			data := "{}"
			if len(e.Data) > 0 {
				data = string(e.Data)
			}
			args = append(args, e.ID, ms(e.TS), e.OrgID, e.ProjectID, e.SandboxID, e.Type, data)
		}
		if _, err := s.exec(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlStore) ListEvents(ctx context.Context, f EventFilter) ([]*Event, error) {
	q := `SELECT id, ts, org_id, project_id, sandbox_id, type, data FROM events`
	var where []string
	var args []any
	add := func(col string, v any) {
		where = append(where, col+` = ?`)
		args = append(args, v)
	}
	if f.OrgID != "" {
		add(`org_id`, f.OrgID)
	}
	if f.ProjectID != "" {
		add(`project_id`, f.ProjectID)
	}
	if f.SandboxID != "" {
		add(`sandbox_id`, f.SandboxID)
	}
	if f.Type != "" {
		add(`type`, f.Type)
	}
	if !f.After.IsZero() {
		where = append(where, `ts > ?`)
		args = append(args, ms(f.After))
	}
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, ` AND `)
	}
	q += ` ORDER BY ts DESC`
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q += ` LIMIT ?`
	args = append(args, limit)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		var e Event
		var ts int64
		var data string
		if err := rows.Scan(&e.ID, &ts, &e.OrgID, &e.ProjectID, &e.SandboxID, &e.Type, &data); err != nil {
			return nil, err
		}
		e.TS = fromMS(ts)
		e.Data = json.RawMessage(data)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ---- idempotency ----

func (s *sqlStore) GetIdempotency(ctx context.Context, key, path string) (*IdempotencyRecord, error) {
	row := s.queryRow(ctx, `SELECT key, path, req_hash, status, body, created_at FROM idempotency WHERE key = ? AND path = ?`, key, path)
	var r IdempotencyRecord
	var created int64
	if err := row.Scan(&r.Key, &r.Path, &r.ReqHash, &r.Status, &r.Body, &created); err != nil {
		return nil, mapErr(err)
	}
	r.CreatedAt = fromMS(created)
	return &r, nil
}

func (s *sqlStore) PutIdempotency(ctx context.Context, rec *IdempotencyRecord) error {
	_, err := s.exec(ctx, `INSERT INTO idempotency (key, path, req_hash, status, body, created_at) VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(key, path) DO NOTHING`, rec.Key, rec.Path, rec.ReqHash, rec.Status, rec.Body, ms(rec.CreatedAt))
	return err
}

// ---- metric snapshots ----

// InsertMetricSnapshots writes percentile snapshots in chunks of 50 rows per
// statement (400 placeholders, under conservative SQLite limits) instead of
// one INSERT per row: O(ceil(n/50)) round trips per flush.
func (s *sqlStore) InsertMetricSnapshots(ctx context.Context, snaps []MetricSnapshot) error {
	const chunk = 50
	for start := 0; start < len(snaps); start += chunk {
		end := start + chunk
		if end > len(snaps) {
			end = len(snaps)
		}
		batch := snaps[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO metric_snapshots (ts, name, labels, count, sum, p50, p95, p99) VALUES `)
		args := make([]any, 0, len(batch)*8)
		for i, m := range batch {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?, ?, ?, ?, ?, ?, ?, ?)`)
			labels, err := json.Marshal(m.Labels)
			if err != nil {
				return err
			}
			args = append(args, ms(m.TS), m.Name, string(labels), int64(m.Count), m.Sum, m.P50, m.P95, m.P99)
		}
		if _, err := s.exec(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *sqlStore) ListMetricSnapshots(ctx context.Context, name string, limit int) ([]MetricSnapshot, error) {
	q := `SELECT ts, name, labels, count, sum, p50, p95, p99 FROM metric_snapshots`
	var args []any
	if name != "" {
		q += ` WHERE name = ?`
		args = append(args, name)
	}
	q += ` ORDER BY ts DESC`
	l := limit
	if l <= 0 || l > 10000 {
		l = 1000
	}
	q += ` LIMIT ?`
	args = append(args, l)

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricSnapshot
	for rows.Next() {
		var m MetricSnapshot
		var labels string
		var ts int64
		if err := rows.Scan(&ts, &m.Name, &labels, &m.Count, &m.Sum, &m.P50, &m.P95, &m.P99); err != nil {
			return nil, err
		}
		m.TS = fromMS(ts)
		_ = json.Unmarshal([]byte(labels), &m.Labels)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- usage ----

// PruneBefore deletes expired idempotency records, ledger rows older than
// the retention window, and terminal-state sandbox/exec/snapshot rows past
// retention — the latter previously grew forever (nothing ever deleted them).
// O(stale rows) per call, amortized to ~0 in steady state; every predicate
// column is indexed.
func (s *sqlStore) PruneBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	c := ms(cutoff)
	var total int64
	for _, q := range []string{
		`DELETE FROM idempotency WHERE created_at < ?`,
		`DELETE FROM events WHERE ts < ?`,
		`DELETE FROM usage_samples WHERE ts < ?`,
		`DELETE FROM metric_snapshots WHERE ts < ?`,
		// Execs: finished ones past retention, plus stuck "running" rows
		// older than retention (their handles died with old processes).
		`DELETE FROM execs WHERE created_at < ? AND (finished_at IS NOT NULL OR state = 'running')`,
		// Snapshots of destroyed lineages past retention.
		`DELETE FROM snapshots WHERE sandbox_id IN (
		   SELECT id FROM sandboxes WHERE state = 'destroyed' AND updated_at < ?)`,
		`DELETE FROM ports WHERE sandbox_id IN (
		   SELECT id FROM sandboxes WHERE state = 'destroyed' AND updated_at < ?)`,
		`DELETE FROM sandboxes WHERE state = 'destroyed' AND updated_at < ?`,
	} {
		res, err := s.exec(ctx, q, c)
		if err != nil {
			return total, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += n
		}
	}
	return total, nil
}

// ClaimIdempotency atomically reserves (key,path) for exactly one executor.
// Returns true when THIS caller won the claim; false when someone else holds
// it (in-flight or completed). A crashed executor's stale claim (>60s old,
// still status=0) can be stolen by a later caller so availability survives.
func (s *sqlStore) ClaimIdempotency(ctx context.Context, key, path, reqHash string) (bool, error) {
	now := ms(time.Now())
	res, err := s.exec(ctx, `INSERT INTO idempotency (key, path, req_hash, status, body, created_at)
VALUES (?, ?, ?, 0, '', ?)
ON CONFLICT(key, path) DO NOTHING`, key, path, reqHash, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	// Row exists — steal only a STALE in-flight claim (crashed handler).
	res, err = s.exec(ctx, `UPDATE idempotency SET req_hash = ?, created_at = ?
WHERE key = ? AND path = ? AND status = 0 AND created_at < ?`,
		reqHash, now, key, path, now-60000)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		return true, nil
	}
	return false, nil
}

// CompleteIdempotency fills in the winning execution's response. Non-2xx
// callers should use DeleteIdempotency instead so retries re-execute.
func (s *sqlStore) CompleteIdempotency(ctx context.Context, key, path string, status int, body, reqHash string) error {
	_, err := s.exec(ctx, `UPDATE idempotency SET status = ?, body = ?, req_hash = ?
WHERE key = ? AND path = ? AND status = 0`,
		status, body, reqHash, key, path)
	return err
}

func (s *sqlStore) DeleteIdempotency(ctx context.Context, key, path string) error {
	_, err := s.exec(ctx, `DELETE FROM idempotency WHERE key = ? AND path = ?`, key, path)
	return err
}

func (s *sqlStore) InsertUsageSample(ctx context.Context, u UsageSample) error {
	return s.InsertUsageSamples(ctx, []UsageSample{u})
}

// InsertUsageSamples writes a batch of samples in chunks of 100 rows per
// statement: O(ceil(n/100)) round trips instead of n. Chunk size keeps the
// placeholder count under SQLite's conservative parameter limits.
func (s *sqlStore) InsertUsageSamples(ctx context.Context, us []UsageSample) error {
	const chunk = 100
	for start := 0; start < len(us); start += chunk {
		end := start + chunk
		if end > len(us) {
			end = len(us)
		}
		batch := us[start:end]
		var b strings.Builder
		b.WriteString(`INSERT INTO usage_samples (sandbox_id, ts, cpu_us, mem_peak_kb, rx_bytes, tx_bytes, running, external_user_id, project_id, interval_s) VALUES `)
		args := make([]any, 0, len(batch)*10)
		for i, u := range batch {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
			iv := u.IntervalS
			if iv <= 0 {
				iv = DefaultSampleIntervalS
			}
			args = append(args, u.SandboxID, ms(u.TS), u.CPUUS, u.MemPeakKB, u.RxBytes, u.TxBytes, boolInt(u.Running), u.ExternalUserID, u.ProjectID, iv)
		}
		if _, err := s.exec(ctx, b.String(), args...); err != nil {
			return err
		}
	}
	return nil
}

// TouchSandbox updates ONLY the activity clock. The previous read-modify-write
// of the whole row could resurrect stale state over a concurrent reconciler
// transition (lost update); a single-column UPDATE cannot.
func (s *sqlStore) TouchSandbox(ctx context.Context, id string, atMS int64) error {
	_, err := s.exec(ctx, `UPDATE sandboxes SET last_activity_ms = ?, updated_at = ? WHERE id = ?`,
		atMS, ms(time.Now()), id)
	return err
}

func (s *sqlStore) UsageRollup(ctx context.Context, f UsageFilter) ([]UsageRow, error) {
	col := map[string]string{
		"external_user_id": "external_user_id",
		"sandbox_id":       "sandbox_id",
		"project_id":       "project_id",
	}[f.GroupBy]
	if col == "" {
		col = "external_user_id"
	}
	// running_seconds sums each sample's OWN interval. The previous
	// COUNT(...)*60.0 assumed a 60s sample period against a sampler that
	// emits every 15s — a flat 4x overbill on every invoice, and silently
	// wrong again after any change to the sampling cadence.
	q := `SELECT ` + col + `, SUM(cpu_us)/1000000.0, MAX(mem_peak_kb), SUM(rx_bytes), SUM(tx_bytes),
COALESCE(SUM(CASE WHEN running = 1 THEN interval_s ELSE 0 END), 0)*1.0
FROM usage_samples WHERE 1=1`
	var args []any
	if f.From.UnixMilli() != 0 && !f.From.IsZero() {
		q += ` AND ts >= ?`
		args = append(args, ms(f.From))
	}
	if !f.To.IsZero() {
		q += ` AND ts <= ?`
		args = append(args, ms(f.To))
	}
	if f.ProjectID != "" {
		q += ` AND project_id = ?`
		args = append(args, f.ProjectID)
	}
	if f.OrgID != "" && f.ProjectID == "" {
		// Org scoping is mandatory, not a hosted-mode nicety: without this
		// clause an org key grouping by external_user_id or sandbox_id
		// receives EVERY org's rows (usage_samples carries no org column,
		// so scope via the projects mapping).
		q += ` AND project_id IN (SELECT id FROM projects WHERE org_id = ?)`
		args = append(args, f.OrgID)
	}
	if f.After != "" {
		// Keyset pagination on the group column: cheaper than OFFSET, which
		// re-scans every skipped group.
		q += ` AND ` + col + ` > ?`
		args = append(args, f.After)
	}
	q += ` GROUP BY ` + col + ` ORDER BY 1`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
	}

	rows, err := s.query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		var r UsageRow
		var group any
		if err := rows.Scan(&group, &r.CPUSeconds, &r.MemPeakKB, &r.RxBytes, &r.TxBytes, &r.RunningSeconds); err != nil {
			return nil, err
		}
		if g, ok := group.(string); ok {
			r.Group = g
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- helpers ----

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func mapErr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "duplicate key") {
		return ErrConflict
	}
	return err
}
