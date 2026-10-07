package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// These run the REAL store against a REAL Postgres. The cell requires Postgres
// and nothing here had ever been executed against one: SQLite's INTEGER is
// 64-bit, so a schema that types millisecond timestamps as INTEGER passed every
// test while being unusable on the database the control plane actually needs.
//
//	postgres=# INSERT INTO probe VALUES (1787519693166);
//	ERROR:  integer out of range
//
// Set SUBSTRATE_TEST_DB_URL to enable. See `make pg-test`.
func pgStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("SUBSTRATE_TEST_DB_URL")
	if dsn == "" {
		t.Skip("set SUBSTRATE_TEST_DB_URL to run the Postgres store tests (make pg-test)")
	}
	s, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestPostgresMillisecondTimestampsFit is the regression: every timestamp this
// system writes is milliseconds since the epoch, which does not fit in int4.
func TestPostgresMillisecondTimestampsFit(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	now := time.Now()

	org := &Org{ID: "org_pg_" + now.Format("150405.000000"), Name: "pg", CreatedAt: now}
	if err := s.CreateOrg(ctx, org); err != nil {
		t.Fatalf("CreateOrg on Postgres: %v — millisecond timestamps do not fit the column type", err)
	}
	got, err := s.GetOrg(ctx, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CreatedAt.UnixMilli() != now.UnixMilli() {
		t.Fatalf("created_at round-tripped as %d, wrote %d", got.CreatedAt.UnixMilli(), now.UnixMilli())
	}
}

// TestPostgresFullLifecycle exercises the paths the cell actually drives, with
// values large enough to expose any remaining int4 column.
func TestPostgresFullLifecycle(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	now := time.Now()
	suffix := now.Format("150405.000000")

	orgID := "org_life_" + suffix
	projID := "prj_life_" + suffix
	if err := s.CreateOrg(ctx, &Org{ID: orgID, Name: "o", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, &Project{ID: projID, OrgID: orgID, Name: "p", MaxSandboxes: 1000, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	tplID := "tpl_life" + suffix[:7]
	if err := s.PutTemplate(ctx, &Template{ID: tplID, ProjectID: projID, Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now}); err != nil {
		t.Fatalf("PutTemplate: %v", err)
	}

	sbID := "sb_life" + suffix[:9]
	sb := &Sandbox{
		ID: sbID, OrgID: orgID, ProjectID: projID, TemplateID: tplID,
		State: StateRunning, Desired: "running", Tier: "hv", MemMiB: 512, VCPUs: 1,
		TTLAbsoluteSecs: 3600, LastActivityMS: now.UnixMilli(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateSandbox(ctx, sb); err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	// ttl_deadline_ms is derived from a ms timestamp plus a TTL: also > int4.
	rows, err := s.SandboxesForReconcile(ctx)
	if err != nil {
		t.Fatalf("SandboxesForReconcile: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.ID == sbID {
			found = true
		}
	}
	if !found {
		t.Fatal("a running sandbox is missing from the reconcile set on Postgres")
	}

	// Usage: cpu_us and byte counters are the other columns that outgrow int4.
	if err := s.InsertUsageSample(ctx, UsageSample{
		SandboxID: sbID, TS: now,
		CPUUS: 9_000_000_000, MemPeakKB: 4_000_000,
		RxBytes: 8_000_000_000, TxBytes: 8_000_000_000,
		Running: true, ProjectID: projID, ExternalUserID: "u1", IntervalS: 15,
	}); err != nil {
		t.Fatalf("InsertUsageSample with realistic counters: %v", err)
	}
	usage, err := s.UsageRollup(ctx, UsageFilter{ProjectID: projID, GroupBy: "external_user_id",
		From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if err != nil {
		t.Fatalf("UsageRollup: %v", err)
	}
	if len(usage) != 1 || usage[0].RunningSeconds != 15 {
		t.Fatalf("usage rollup on Postgres: %+v", usage)
	}

	// Events, idempotency and the exec ledger all carry ms timestamps too.
	if err := s.AppendEvent(ctx, &Event{ID: "ev_" + suffix[:9], TS: now, OrgID: orgID, ProjectID: projID, SandboxID: sbID, Type: "created"}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if ok, err := s.ClaimIdempotency(ctx, "key_"+suffix, "/v1/sandboxes", "hash"); err != nil || !ok {
		t.Fatalf("ClaimIdempotency: ok=%v err=%v", ok, err)
	}
	if err := s.CompleteIdempotency(ctx, "key_"+suffix, "/v1/sandboxes", 201, `{"id":"x"}`, "hash"); err != nil {
		t.Fatalf("CompleteIdempotency: %v", err)
	}
	if _, err := s.PruneBefore(ctx, now.Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
}

// TestPostgresTenancyGuardsHold: the cross-tenant guards added in pass 12 and 15
// must behave the same on Postgres as on SQLite — ON CONFLICT ... WHERE and
// RowsAffected semantics differ enough between drivers to be worth pinning.
func TestPostgresTenancyGuardsHold(t *testing.T) {
	s := pgStore(t)
	ctx := context.Background()
	now := time.Now()
	suffix := now.Format("150405.000000")
	id := "tpl_tenpg" + suffix[:6]

	if err := s.PutTemplate(ctx, &Template{ID: id, ProjectID: "prj_victim", Name: "prod",
		RootfsPath: "/srv/victim.ext4", MemMiB: 512, VCPUs: 1, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	err := s.PutTemplate(ctx, &Template{ID: id, ProjectID: "prj_attacker", Name: "pwned",
		RootfsPath: "/tmp/attacker.ext4", MemMiB: 512, VCPUs: 1, CreatedAt: now})
	if err == nil {
		t.Fatal("cross-project template upsert succeeded on Postgres")
	}
	got, gerr := s.GetTemplate(ctx, id)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.ProjectID != "prj_victim" || got.RootfsPath != "/srv/victim.ext4" {
		t.Fatalf("victim template was modified on Postgres: %+v", got)
	}
}
