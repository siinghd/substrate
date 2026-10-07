package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backuper is an OPTIONAL store capability: write a consistent copy of the
// database to a path.
//
// There was no backup story at all. A node kept every org, project, API key,
// sandbox record and billing sample in one SQLite file with nothing copying it
// anywhere: one bad disk and every customer's account and unbilled usage was
// gone, with no way to answer "what did we lose?".
type Backuper interface {
	// Backup writes a consistent snapshot to destPath and returns its size.
	Backup(ctx context.Context, destPath string) (int64, error)
}

// Backup snapshots a SQLite store.
//
// `VACUUM INTO` and not a file copy: the database runs in WAL mode, so the
// .db file on its own is an incomplete picture -- recent commits live in the
// -wal until a checkpoint. Copying just the .db yields a backup that restores
// to a silently older state, which is the worst kind of backup because it
// restores successfully.
//
// VACUUM INTO takes SQLite's own consistent view and writes a standalone,
// already-compacted database. It does not block writers for the whole run.
func (s *sqlStore) Backup(ctx context.Context, destPath string) (int64, error) {
	if s.dialect != "sqlite" {
		return 0, fmt.Errorf("Backup is sqlite-only; for postgres use pg_dump against cell.database_url")
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o700); err != nil {
		return 0, err
	}
	// VACUUM INTO refuses to overwrite. Write to a temp name and rename, so a
	// backup interrupted halfway never replaces a good one with a partial file.
	tmp := destPath + ".partial"
	_ = os.Remove(tmp)
	if strings.ContainsAny(tmp, "'") {
		return 0, fmt.Errorf("refusing backup path containing a quote: %q", tmp)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM INTO '"+tmp+"'"); err != nil {
		_ = os.Remove(tmp)
		return 0, fmt.Errorf("vacuum into: %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, destPath); err != nil {
		return 0, err
	}
	fi, err := os.Stat(destPath)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// VerifyBackup opens a backup file and checks it is a usable database, not
// merely a file that exists.
//
// A backup nobody has restored is not a backup. This is cheap enough to run on
// every snapshot, so "the backup ran" and "the backup works" stop being
// different claims discovered months apart.
func VerifyBackup(ctx context.Context, path string) error {
	st, err := OpenSQLite(path)
	if err != nil {
		return fmt.Errorf("backup will not open: %w", err)
	}
	defer st.Close()
	// Read the tables that matter for recovery. An empty-but-valid file opens
	// fine, so query rather than trust the handle.
	if _, err := st.ListOrgs(ctx); err != nil {
		return fmt.Errorf("backup opens but orgs are unreadable: %w", err)
	}
	return nil
}

// RotateBackups keeps the newest `keep` files matching prefix in dir.
// Unbounded backups fill the disk that the node is also running sandboxes on,
// which turns a safety measure into an outage.
func RotateBackups(dir, prefix string, keep int) ([]string, error) {
	if keep < 1 {
		keep = 1
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	// Names embed an RFC3339 timestamp, so lexical order is chronological.
	sort.Sort(sort.Reverse(sort.StringSlice(names)))
	var removed []string
	for i := keep; i < len(names); i++ {
		p := filepath.Join(dir, names[i])
		if err := os.Remove(p); err == nil {
			removed = append(removed, p)
		}
	}
	return removed, nil
}

// BackupName is the timestamped file name for a snapshot taken at t.
func BackupName(prefix string, t time.Time) string {
	return fmt.Sprintf("%s%s.db", prefix, t.UTC().Format("20060102T150405Z"))
}
