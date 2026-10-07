package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestBackupRestoresRealData: the only test that matters for a backup is
// whether the data comes back. Write rows, snapshot, open the snapshot as a
// fresh database, and read the rows out of it.
func TestBackupRestoresRealData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	now := time.Now()
	if err := st.CreateOrg(ctx, &Org{ID: "org_backup01", Name: "acme", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(ctx, &Project{ID: "prj_backup01", OrgID: "org_backup01", Name: "prod", MaxSandboxes: 10, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateAPIKey(ctx, &APIKey{ID: "key_backup01", OrgID: "org_backup01", Kind: "org", KeyHash: "deadbeef", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(dir, "snap.db")
	size, err := st.(Backuper).Backup(ctx, dest)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if size <= 0 {
		t.Fatalf("backup is %d bytes", size)
	}
	if fi, _ := os.Stat(dest); fi != nil && fi.Mode().Perm() != 0o600 {
		t.Errorf("backup is mode %o; it holds every API key hash and account row", fi.Mode().Perm())
	}
	if err := VerifyBackup(ctx, dest); err != nil {
		t.Fatalf("verify: %v", err)
	}

	// RESTORE. Open the snapshot as its own database and read the data back.
	restored, err := OpenSQLite(dest)
	if err != nil {
		t.Fatalf("open restored: %v", err)
	}
	defer restored.Close()
	orgs, err := restored.ListOrgs(ctx)
	if err != nil || len(orgs) != 1 || orgs[0].Name != "acme" {
		t.Fatalf("org did not survive the backup: %+v (%v)", orgs, err)
	}
	projs, err := restored.ListProjects(ctx, "org_backup01")
	if err != nil || len(projs) != 1 {
		t.Fatalf("project did not survive: %+v (%v)", projs, err)
	}
	keys, err := restored.ListAPIKeys(ctx, "org_backup01")
	if err != nil || len(keys) != 1 {
		t.Fatalf("api key did not survive: %+v (%v)", keys, err)
	}
}

// TestBackupCapturesWritesAfterTheLastCheckpoint: the store runs in WAL mode,
// so a plain copy of the .db file silently omits recent commits and restores to
// an older state. Write, snapshot immediately, and demand the write is there.
func TestBackupCapturesWritesAfterTheLastCheckpoint(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "live.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 50; i++ {
		if err := st.CreateOrg(ctx, &Org{ID: "org_wal" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + "0000", Name: "n", CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	dest := filepath.Join(dir, "snap.db")
	if _, err := st.(Backuper).Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	restored, err := OpenSQLite(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	orgs, err := restored.ListOrgs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(orgs) != 50 {
		t.Fatalf("snapshot holds %d of 50 orgs: writes after the last WAL checkpoint were lost", len(orgs))
	}
}

// TestBackupDoesNotClobberAGoodSnapshotOnFailure: an interrupted backup must
// not leave a truncated file where a working one used to be.
func TestBackupDoesNotClobberAGoodSnapshotOnFailure(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, _ := OpenSQLite(filepath.Join(dir, "live.db"))
	defer st.Close()
	st.CreateOrg(ctx, &Org{ID: "org_keepgood1", Name: "good", CreatedAt: time.Now()})

	dest := filepath.Join(dir, "snap.db")
	if _, err := st.(Backuper).Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(dest)

	// A destination the write cannot succeed into.
	if _, err := st.(Backuper).Backup(ctx, filepath.Join(dir, "nope\x00bad", "x.db")); err == nil {
		t.Fatal("expected a failure for an impossible path")
	}
	after, _ := os.ReadFile(dest)
	if len(after) != len(before) {
		t.Fatal("a failed backup damaged the previous good snapshot")
	}
	if err := VerifyBackup(ctx, dest); err != nil {
		t.Fatalf("previous snapshot no longer verifies: %v", err)
	}
}

func TestRotateBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var names []string
	for i := 0; i < 10; i++ {
		n := BackupName("substrate-", base.Add(time.Duration(i)*time.Hour))
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
		names = append(names, n)
	}
	removed, err := RotateBackups(dir, "substrate-", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 7 {
		t.Fatalf("removed %d, want 7", len(removed))
	}
	// The three NEWEST must survive; deleting the wrong end is the failure
	// that leaves you with only ancient backups.
	for _, keep := range names[7:] {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("rotation deleted a recent backup %s", keep)
		}
	}
}
