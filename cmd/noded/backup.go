package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/store"
)

// runBackups snapshots the store on a schedule, verifies each snapshot, and
// rotates old ones.
//
// Verification is not optional here. "The backup job ran" and "the backup
// works" are different claims, and the gap between them is normally discovered
// during a restore, which is the worst possible time. Each snapshot is opened
// and read before it counts as a success.
func runBackups(ctx context.Context, st store.Store, dir string, every time.Duration, keep int, log *logging.Logger) {
	b, ok := st.(store.Backuper)
	if !ok {
		log.Info("store does not support in-process backup; use pg_dump for postgres")
		return
	}
	const prefix = "substrate-"
	snap := func() {
		start := time.Now()
		path := filepath.Join(dir, store.BackupName(prefix, start))
		size, err := b.Backup(ctx, path)
		if err != nil {
			log.Error("backup FAILED", "err", err.Error(), "path", path)
			return
		}
		if verr := store.VerifyBackup(ctx, path); verr != nil {
			// A snapshot that does not verify is worse than none: it will be
			// counted as protection until someone tries to use it.
			log.Error("backup wrote but DOES NOT VERIFY", "err", verr.Error(), "path", path)
			return
		}
		removed, _ := store.RotateBackups(dir, prefix, keep)
		log.Info("backup ok", "path", path, "bytes", size,
			"took_ms", time.Since(start).Milliseconds(), "rotated", len(removed))
	}

	snap() // one at startup, so a node that never survives to its first tick still has one
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			snap()
		}
	}
}
