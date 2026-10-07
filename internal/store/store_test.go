package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOrgProjectKeyCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	org := &Org{ID: "org_1", Name: "acme", CreatedAt: time.Now()}
	if err := s.CreateOrg(ctx, org); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetOrg(ctx, "org_1")
	if err != nil || got.Name != "acme" {
		t.Fatalf("get org: %v %v", got, err)
	}
	if _, err := s.GetOrg(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	p := &Project{ID: "prj_1", OrgID: "org_1", Name: "default", MaxSandboxes: 100, CreatedAt: time.Now()}
	if err := s.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	ps, _ := s.ListProjects(ctx, "org_1")
	if len(ps) != 1 || ps[0].MaxSandboxes != 100 {
		t.Fatalf("projects: %+v", ps)
	}

	key := &APIKey{ID: "k1", OrgID: "org_1", ProjectID: "prj_1", Kind: "project", KeyHash: "h1", CreatedAt: time.Now()}
	if err := s.CreateAPIKey(ctx, key); err != nil {
		t.Fatal(err)
	}
	k, err := s.GetAPIKeyByHash(ctx, "h1")
	if err != nil || k.Kind != "project" {
		t.Fatalf("key: %v %v", k, err)
	}
	if err := s.RevokeAPIKey(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	k, _ = s.GetAPIKeyByHash(ctx, "h1")
	if k.RevokedAt == nil {
		t.Fatal("revocation not persisted")
	}
}

func TestSandboxLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	started := now.Add(time.Second)

	sb := &Sandbox{
		ID: "sb_1", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		ExternalUserID: "user-42", Desired: "running", State: StateCreating, Tier: "process",
		MemMiB: 512, VCPUs: 1, Env: map[string]string{"FOO": "bar"},
		TTLIdleSecs: 300, LastActivityMS: now.UnixMilli(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSandbox(ctx, "sb_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Env["FOO"] != "bar" || got.State != StateCreating || got.TTLIdleSecs != 300 {
		t.Fatalf("roundtrip mismatch: %+v", got)
	}

	got.Desired = "running"
	got.State = StateRunning
	got.StartedAt = &started
	if err := s.UpdateSandbox(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetSandbox(ctx, "sb_1")
	if got2.State != StateRunning || got2.StartedAt == nil {
		t.Fatalf("update failed: %+v", got2)
	}

	// filters
	byUser, _ := s.ListSandboxes(ctx, SandboxFilter{ProjectID: "prj_1", ExternalUserID: "user-42"})
	if len(byUser) != 1 {
		t.Fatalf("filter by user: %d", len(byUser))
	}
	n, _ := s.CountSandboxesByUser(ctx, "prj_1", "user-42")
	if n != 1 {
		t.Fatalf("count by user: %d", n)
	}

	if err := s.DeleteSandbox(ctx, "sb_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSandbox(ctx, "sb_1"); err != ErrNotFound {
		t.Fatalf("want not found after delete, got %v", err)
	}
}

func TestPortsAndExecsAndSnapshots(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if err := s.UpsertPort(ctx, "sb_1", PortMapping{Port: 3000, Proto: "tcp", Public: false, Token: "t1"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPort(ctx, "sb_1", PortMapping{Port: 3000, Proto: "tcp", Public: true, Token: "t2"}); err != nil {
		t.Fatal(err)
	}
	ports, _ := s.ListPorts(ctx, "sb_1")
	if len(ports) != 1 || !ports[0].Public || ports[0].Token != "t2" {
		t.Fatalf("ports upsert broken: %+v", ports)
	}

	e := &Exec{ID: "ex_1", SandboxID: "sb_1", Argv: []string{"sh", "-c", "ls"}, State: "running", TTY: true, IdempotencyKey: "idem-1", CreatedAt: time.Now()}
	if err := s.CreateExec(ctx, e); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetExecByIdempotencyKey(ctx, "idem-1")
	if err != nil || len(got.Argv) != 3 || !got.TTY {
		t.Fatalf("exec idempotent lookup: %+v %v", got, err)
	}
	fin := nowPtr()
	got.State = "exited"
	got.ExitCode = 7
	got.FinishedAt = fin
	if err := s.UpdateExec(ctx, got); err != nil {
		t.Fatal(err)
	}
	got2, _ := s.GetExec(ctx, "ex_1")
	if got2.ExitCode != 7 || got2.FinishedAt == nil {
		t.Fatalf("exec update: %+v", got2)
	}

	sn := &Snapshot{ID: "snp_1", SandboxID: "sb_1", Kind: "pause", Ref: "/tmp/x", SizeBytes: 1234, LineageID: "lin_1", CreatedAt: time.Now()}
	if err := s.CreateSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}
	ok, err := s.AcquireLineageLock(ctx, "lin_1", "holder-a")
	if err != nil || !ok {
		t.Fatalf("lock acquire: %v %v", ok, err)
	}
	ok2, _ := s.AcquireLineageLock(ctx, "lin_1", "holder-b")
	if ok2 {
		t.Fatal("second holder acquired held lock")
	}
	if err := s.ReleaseLineageLock(ctx, "lin_1", "holder-a"); err != nil {
		t.Fatal(err)
	}
	ok3, _ := s.AcquireLineageLock(ctx, "lin_1", "holder-b")
	if !ok3 {
		t.Fatal("release did not free lock")
	}
}

func nowPtr() *time.Time { t := time.Now(); return &t }

func TestEventsAndUsage(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	for i, typ := range []string{"created", "running", "paused"} {
		e := &Event{ID: EventID(i), TS: time.Now().Add(time.Duration(i) * time.Second), ProjectID: "prj_1", SandboxID: "sb_1", Type: typ}
		if err := s.AppendEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	evs, err := s.ListEvents(ctx, EventFilter{SandboxID: "sb_1"})
	if err != nil || len(evs) != 3 {
		t.Fatalf("events: %v %v", evs, err)
	}
	if evs[0].Type != "paused" {
		t.Fatalf("expected DESC order, first=%s", evs[0].Type)
	}

	base := time.Now().Add(-time.Hour)
	for i := 0; i < 5; i++ {
		u := UsageSample{
			SandboxID: "sb_1", TS: base.Add(time.Duration(i) * time.Minute),
			CPUUS: int64(1000 * (i + 1)), MemPeakKB: 200000, RxBytes: 100, TxBytes: 200,
			Running: i < 3, ExternalUserID: "u1", ProjectID: "prj_1",
			IntervalS: 60, // these samples are a minute apart
		}
		if err := s.InsertUsageSample(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.UsageRollup(ctx, UsageFilter{GroupBy: "external_user_id", From: base.Add(-time.Minute), To: time.Now()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rollup: %v %v", rows, err)
	}
	r := rows[0]
	if r.Group != "u1" || r.CPUSeconds != 0.015 || r.RunningSeconds != 180 {
		t.Fatalf("rollup numbers wrong: %+v", r)
	}
}

// TestUsageRollupHonoursSampleInterval: running_seconds must come from each
// sample's recorded interval, not a hardcoded period. The rollup used to
// compute COUNT(running)*60 while the sampler emitted every 15s, billing every
// customer 4x their actual runtime.
func TestUsageRollupHonoursSampleInterval(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)

	const n = 8
	for i := 0; i < n; i++ {
		if err := s.InsertUsageSample(ctx, UsageSample{
			SandboxID: "sb_iv", TS: base.Add(time.Duration(i) * 15 * time.Second),
			CPUUS: 1000, MemPeakKB: 1, Running: true,
			ExternalUserID: "u_iv", ProjectID: "prj_iv",
			IntervalS: 15, // the sampler's real cadence
		}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.UsageRollup(ctx, UsageFilter{ProjectID: "prj_iv", GroupBy: "external_user_id",
		From: base.Add(-time.Minute), To: time.Now()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rollup: %v %v", rows, err)
	}
	if want := float64(n * 15); rows[0].RunningSeconds != want {
		t.Fatalf("running_seconds = %v, want %v (4x overbill would report %v)",
			rows[0].RunningSeconds, want, float64(n*60))
	}
}

// TestUsageSampleIntervalDefaults: a sample written without an explicit
// interval still bills at the documented default rather than zero.
func TestUsageSampleIntervalDefaults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	if err := s.InsertUsageSample(ctx, UsageSample{
		SandboxID: "sb_d", TS: base, CPUUS: 1, MemPeakKB: 1, Running: true,
		ExternalUserID: "u_d", ProjectID: "prj_d",
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.UsageRollup(ctx, UsageFilter{ProjectID: "prj_d", GroupBy: "external_user_id",
		From: base.Add(-time.Minute), To: time.Now()})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rollup: %v %v", rows, err)
	}
	if rows[0].RunningSeconds != DefaultSampleIntervalS {
		t.Fatalf("running_seconds = %v, want %v", rows[0].RunningSeconds, float64(DefaultSampleIntervalS))
	}
}

func EventID(i int) string { return "ev_" + string(rune('a'+i)) }
