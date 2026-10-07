package store

import (
	"context"
	"fmt"
	"strings"
)

// Column types are BIGINT, not INTEGER, deliberately.
//
// Every timestamp here is UNIX MILLISECONDS (~1.79e12 today) and several
// counters are byte or microsecond totals. SQLite's INTEGER is 64-bit so this
// was invisible in every test -- but Postgres's INTEGER is int4, max 2.1e9, and
// the cell REQUIRES Postgres. Verified against postgres:16:
//
//	INSERT INTO probe VALUES (1787519693166);
//	ERROR:  integer out of range
//
// so cell mode could not store a single row. BIGINT is int8 on Postgres and
// takes INTEGER affinity on SQLite, so one schema serves both.
const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);

CREATE TABLE IF NOT EXISTS orgs (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, created_at BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS projects (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, name TEXT NOT NULL,
  max_sandboxes BIGINT NOT NULL DEFAULT 1000, created_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_projects_org ON projects(org_id);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, project_id TEXT NOT NULL DEFAULT '',
  key_hash TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
  created_at BIGINT NOT NULL, revoked_at BIGINT);

CREATE TABLE IF NOT EXISTS templates (
  id TEXT PRIMARY KEY, project_id TEXT NOT NULL, name TEXT NOT NULL,
  rootfs_path TEXT NOT NULL DEFAULT '', rootfs_size BIGINT NOT NULL DEFAULT 0,
  kernel_path TEXT NOT NULL DEFAULT '',
  mem_mib BIGINT NOT NULL DEFAULT 512, vcpus BIGINT NOT NULL DEFAULT 1,
  net_policy TEXT NOT NULL DEFAULT '{}', warm_cmd TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS sandboxes (
  id TEXT PRIMARY KEY, org_id TEXT NOT NULL, project_id TEXT NOT NULL,
  template_id TEXT NOT NULL DEFAULT '', external_user_id TEXT NOT NULL DEFAULT '',
  desired TEXT NOT NULL DEFAULT 'running', state TEXT NOT NULL, tier TEXT NOT NULL,
  node_id TEXT NOT NULL DEFAULT '',
  mem_mib BIGINT NOT NULL, vcpus BIGINT NOT NULL,
  env TEXT NOT NULL DEFAULT '{}',
  lineage_id TEXT NOT NULL DEFAULT '', pause_ref TEXT NOT NULL DEFAULT '',
  error TEXT NOT NULL DEFAULT '',
  ttl_idle_secs BIGINT NOT NULL DEFAULT 0, ttl_absolute_secs BIGINT NOT NULL DEFAULT 0,
  last_activity_ms BIGINT NOT NULL DEFAULT 0,
  created_at BIGINT NOT NULL, updated_at BIGINT NOT NULL,
  started_at BIGINT, paused_at BIGINT,
  -- created_at + ttl_absolute_secs*1000, or 0 for "no absolute TTL".
  ttl_deadline_ms BIGINT NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS idx_sb_project ON sandboxes(project_id);
CREATE INDEX IF NOT EXISTS idx_sb_extuser ON sandboxes(project_id, external_user_id);
CREATE INDEX IF NOT EXISTS idx_sb_state ON sandboxes(state);
CREATE INDEX IF NOT EXISTS idx_sb_org ON sandboxes(org_id);
-- List endpoints filter by tenant and order by created_at. With only the
-- single-column tenant index, SQLite matched the index and then SORTED every
-- matching row: a LIMIT 50 over a 100k-sandbox project cost 249ms. These make
-- the ORDER BY index-served, so the cost follows the LIMIT, not the table.
CREATE INDEX IF NOT EXISTS idx_sb_project_created ON sandboxes(project_id, created_at);
CREATE INDEX IF NOT EXISTS idx_sb_org_created ON sandboxes(org_id, created_at);
-- Quota checks count live sandboxes per project on EVERY create. Covering the
-- state column keeps that count inside the index instead of touching 100k rows.
CREATE INDEX IF NOT EXISTS idx_sb_project_state ON sandboxes(project_id, state);
CREATE INDEX IF NOT EXISTS idx_sb_extuser_state ON sandboxes(project_id, external_user_id, state);
-- The reconciler needs desired-state changes without scanning the table.
CREATE INDEX IF NOT EXISTS idx_sb_desired ON sandboxes(desired);
-- Absolute-TTL expiry must reach PAUSED sandboxes too (otherwise a TTL is
-- evadable by pausing first), but paused rows are the bulk of the table at the
-- design centre. Materialising the deadline makes that check one indexed range
-- scan instead of a predicate the planner cannot use.
CREATE INDEX IF NOT EXISTS idx_sb_ttl_deadline ON sandboxes(ttl_deadline_ms);

CREATE TABLE IF NOT EXISTS ports (
  sandbox_id TEXT NOT NULL, port BIGINT NOT NULL, proto TEXT NOT NULL,
  public BIGINT NOT NULL, token TEXT NOT NULL,
  PRIMARY KEY (sandbox_id, port, proto));

CREATE TABLE IF NOT EXISTS execs (
  id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL, argv TEXT NOT NULL,
  state TEXT NOT NULL, exit_code BIGINT NOT NULL DEFAULT -1,
  tty BIGINT NOT NULL DEFAULT 0, log_path TEXT NOT NULL DEFAULT '',
  idempotency_key TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL, finished_at BIGINT);
CREATE INDEX IF NOT EXISTS idx_execs_sb ON execs(sandbox_id);
CREATE INDEX IF NOT EXISTS idx_execs_idem ON execs(idempotency_key);

CREATE TABLE IF NOT EXISTS snapshots (
  id TEXT PRIMARY KEY, sandbox_id TEXT NOT NULL, kind TEXT NOT NULL,
  ref TEXT NOT NULL DEFAULT '', size_bytes BIGINT NOT NULL DEFAULT 0,
  shared BIGINT NOT NULL DEFAULT 0, lineage_id TEXT NOT NULL DEFAULT '',
  created_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS idx_snap_sb ON snapshots(sandbox_id);

CREATE TABLE IF NOT EXISTS lineage_locks (
  lineage_id TEXT PRIMARY KEY, holder TEXT NOT NULL, acquired_ms BIGINT NOT NULL);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY, ts BIGINT NOT NULL,
  org_id TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '',
  sandbox_id TEXT NOT NULL DEFAULT '', type TEXT NOT NULL, data TEXT NOT NULL DEFAULT '');
CREATE INDEX IF NOT EXISTS idx_events_sb ON events(sandbox_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_proj ON events(project_id, ts);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts);

CREATE TABLE IF NOT EXISTS idempotency (
  key TEXT NOT NULL, path TEXT NOT NULL, req_hash TEXT NOT NULL,
  status BIGINT NOT NULL, body TEXT NOT NULL, created_at BIGINT NOT NULL,
  PRIMARY KEY (key, path));
CREATE INDEX IF NOT EXISTS idx_idem_created ON idempotency(created_at);

CREATE TABLE IF NOT EXISTS metric_snapshots (
  ts BIGINT NOT NULL, name TEXT NOT NULL, labels TEXT NOT NULL,
  count BIGINT NOT NULL, sum REAL NOT NULL,
  p50 REAL NOT NULL, p95 REAL NOT NULL, p99 REAL NOT NULL);
CREATE INDEX IF NOT EXISTS idx_msnap_name_ts ON metric_snapshots(name, ts);
CREATE INDEX IF NOT EXISTS idx_msnap_ts ON metric_snapshots(ts);

CREATE TABLE IF NOT EXISTS usage_samples (
  sandbox_id TEXT NOT NULL, ts BIGINT NOT NULL,
  cpu_us BIGINT NOT NULL, mem_peak_kb BIGINT NOT NULL,
  rx_bytes BIGINT NOT NULL, tx_bytes BIGINT NOT NULL,
  running BIGINT NOT NULL DEFAULT 0,
  external_user_id TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '',
  -- Seconds of wall clock this sample accounts for. Billing MUST read this
  -- rather than assume a period: the rollup used to multiply the sample count
  -- by a hardcoded 60 while the sampler emitted every 15s, overstating
  -- running_seconds by exactly 4x on every invoice.
  interval_s BIGINT NOT NULL DEFAULT 15);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_samples(ts);
CREATE INDEX IF NOT EXISTS idx_usage_grp ON usage_samples(project_id, external_user_id);
CREATE INDEX IF NOT EXISTS idx_usage_proj_ts ON usage_samples(project_id, ts);
`

// addColumns are applied after the base schema so databases created before a
// column existed pick it up. CREATE TABLE IF NOT EXISTS never alters an
// existing table, so new columns need an explicit ALTER. SQLite has no
// ADD COLUMN IF NOT EXISTS, so a duplicate-column error is the success case
// on an already-migrated database.
var addColumns = []string{
	`ALTER TABLE usage_samples ADD COLUMN interval_s BIGINT NOT NULL DEFAULT 15`,
	`ALTER TABLE sandboxes ADD COLUMN ttl_deadline_ms BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orgs ADD COLUMN suspended_at BIGINT NOT NULL DEFAULT 0`,
	`ALTER TABLE orgs ADD COLUMN suspend_reason TEXT NOT NULL DEFAULT ''`,
}

// backfills run after addColumns. Idempotent by construction: each only
// touches rows still holding the column default, so a second run is a no-op.
var backfills = []string{
	`UPDATE sandboxes SET ttl_deadline_ms = created_at + ttl_absolute_secs * 1000
	   WHERE ttl_absolute_secs > 0 AND ttl_deadline_ms = 0`,
}

func (s *sqlStore) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, rewrite(s.dialect, schema)); err != nil {
		return err
	}
	for _, q := range addColumns {
		if _, err := s.db.ExecContext(ctx, rewrite(s.dialect, q)); err != nil {
			if !isDuplicateColumn(err) {
				return fmt.Errorf("migrate %q: %w", q, err)
			}
		}
	}
	for _, q := range backfills {
		if _, err := s.db.ExecContext(ctx, rewrite(s.dialect, q)); err != nil {
			return fmt.Errorf("backfill %q: %w", q, err)
		}
	}
	return nil
}

func isDuplicateColumn(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "duplicate column") || // sqlite
		strings.Contains(msg, "already exists") // postgres
}
