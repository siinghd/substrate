package store

import (
	"context"
	"testing"
	"time"
)

func hasID(rows []ReconcileRow, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestReconcileScopeSkipsIdlePausedButKeepsTheExceptions: narrowing the 2s
// reconcile query away from "every non-destroyed row" is only safe if the two
// things a PAUSED sandbox still needs from the reconciler survive — being torn
// down when desired says so, and being destroyed at its absolute TTL (which
// must not be evadable by pausing first).
func TestReconcileScopeSkipsIdlePausedButKeepsTheExceptions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	mk := func(id string, state SandboxState, desired string, ttlAbs int, created time.Time) {
		if err := s.CreateSandbox(ctx, &Sandbox{
			ID: id, OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
			State: state, Desired: desired, Tier: "process", MemMiB: 512, VCPUs: 1,
			TTLAbsoluteSecs: ttlAbs, CreatedAt: created, UpdatedAt: created,
		}); err != nil {
			t.Fatal(err)
		}
	}

	mk("sb_running000001", StateRunning, "running", 0, now)
	mk("sb_pausedidle001", StatePaused, "running", 0, now)                    // skipped: nothing to do
	mk("sb_pauseddestr01", StatePaused, "destroyed", 0, now)                  // must appear
	mk("sb_pausedttlexp1", StatePaused, "running", 60, now.Add(-2*time.Hour)) // must appear
	mk("sb_pausedttlok01", StatePaused, "running", 86400, now)                // TTL far away: skipped
	mk("sb_destroyed0001", StateDestroyed, "destroyed", 0, now)               // terminal: skipped

	rows, err := s.SandboxesForReconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	mustHave := []string{"sb_running000001", "sb_pauseddestr01", "sb_pausedttlexp1"}
	for _, id := range mustHave {
		if !hasID(rows, id) {
			t.Errorf("%s missing from the reconcile set — it needs convergence and would never get it", id)
		}
	}
	mustSkip := []string{"sb_pausedidle001", "sb_pausedttlok01", "sb_destroyed0001"}
	for _, id := range mustSkip {
		if hasID(rows, id) {
			t.Errorf("%s is in the reconcile set but needs no work — this is the scan cost we removed", id)
		}
	}
	// No duplicates: a paused row can match both exception queries.
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ID] {
			t.Fatalf("%s returned twice", r.ID)
		}
		seen[r.ID] = true
	}
}

// TestTTLDeadlineTracksUpdates: the materialised deadline has to follow the
// row, or a TTL set (or cleared) after creation would never take effect.
func TestTTLDeadlineTracksUpdates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	created := time.Now().Add(-2 * time.Hour)

	sb := &Sandbox{ID: "sb_ttlupdate0001", OrgID: "org_1", ProjectID: "prj_1",
		State: StatePaused, Desired: "running", Tier: "process", MemMiB: 512, VCPUs: 1,
		CreatedAt: created, UpdatedAt: created}
	if err := s.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	rows, _ := s.SandboxesForReconcile(ctx)
	if hasID(rows, sb.ID) {
		t.Fatal("paused sandbox with no TTL appeared in the reconcile set")
	}

	sb.TTLAbsoluteSecs = 60 // already in the past relative to CreatedAt
	if err := s.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.SandboxesForReconcile(ctx)
	if !hasID(rows, sb.ID) {
		t.Fatal("TTL set after creation never reached the reconciler: the deadline did not follow the update")
	}

	sb.TTLAbsoluteSecs = 0 // cleared
	if err := s.UpdateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}
	rows, _ = s.SandboxesForReconcile(ctx)
	if hasID(rows, sb.ID) {
		t.Fatal("cleared TTL still selects the row")
	}
}

// TestTTLDeadlineBackfilled: databases written before the column existed must
// still have their absolute TTLs enforced after migration. The pre-migration
// state is reproduced by clearing the column through the raw handle — a type
// assertion that silently failed would make this test assert nothing.
func TestTTLDeadlineBackfilled(t *testing.T) {
	path := t.TempDir() + "/backfill.db"
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	created := time.Now().Add(-2 * time.Hour)
	if err := s.CreateSandbox(ctx, &Sandbox{ID: "sb_backfill00001", OrgID: "org_1", ProjectID: "prj_1",
		State: StatePaused, Desired: "running", Tier: "process", MemMiB: 512, VCPUs: 1,
		TTLAbsoluteSecs: 60, CreatedAt: created, UpdatedAt: created}); err != nil {
		t.Fatal(err)
	}

	// Reproduce a row written before ttl_deadline_ms existed.
	raw, ok := s.(*sqlStore)
	if !ok {
		t.Fatalf("OpenSQLite returned %T, not *sqlStore — this test can no longer reach the raw handle", s)
	}
	if _, err := raw.db.ExecContext(ctx, `UPDATE sandboxes SET ttl_deadline_ms = 0`); err != nil {
		t.Fatal(err)
	}
	// Confirm the precondition: without the backfill the row is invisible.
	pre, err := s.SandboxesForReconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if hasID(pre, "sb_backfill00001") {
		t.Fatal("precondition failed: the row is still selected with ttl_deadline_ms = 0, so this test proves nothing")
	}
	s.Close()

	s2, err := OpenSQLite(path) // re-open runs migrate + backfill
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	rows, err := s2.SandboxesForReconcile(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !hasID(rows, "sb_backfill00001") {
		t.Fatal("a pre-migration row's absolute TTL is not enforced after migration")
	}
}

// TestQuotaCountStateListIsExhaustive: the quota switched from
// `state NOT IN ('destroyed','destroying')` to a positive IN list for the
// index path. The two must agree, or a quota silently stops counting a state.
func TestQuotaCountStateListIsExhaustive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()
	all := []SandboxState{StateCreating, StateRunning, StatePaused, StateArchived,
		StateDestroying, StateDestroyed, StateError}
	for i, st := range all {
		if err := s.CreateSandbox(ctx, &Sandbox{
			ID: "sb_quota" + string(rune('a'+i)) + "0000000", OrgID: "org_1", ProjectID: "prj_q",
			ExternalUserID: "u1", State: st, Desired: "running", Tier: "process",
			MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Everything except destroyed and destroying counts against the quota.
	want := len(all) - 2
	got, err := s.CountSandboxesByProject(ctx, "prj_q")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("project quota counted %d of %d live states — the IN list has drifted from the SandboxState constants", got, want)
	}
	gotUser, err := s.CountSandboxesByUser(ctx, "prj_q", "u1")
	if err != nil {
		t.Fatal(err)
	}
	if gotUser != want {
		t.Fatalf("per-user quota counted %d, want %d", gotUser, want)
	}
}
