package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// seedSandboxes fills one project with n sandboxes — the design centre is
// 100,000 defined per cell, so benchmarks run there, not at n=10.
func seedSandboxes(tb testing.TB, s Store, n int) {
	tb.Helper()
	ctx := context.Background()
	now := time.Now()
	s.CreateOrg(ctx, &Org{ID: "org_1", Name: "o", CreatedAt: now})
	s.CreateProject(ctx, &Project{ID: "prj_1", OrgID: "org_1", Name: "p", MaxSandboxes: 1 << 30, CreatedAt: now})
	states := []SandboxState{StateRunning, StatePaused, StatePaused, StatePaused, StatePaused,
		StatePaused, StatePaused, StatePaused, StatePaused, StateArchived}
	for i := 0; i < n; i++ {
		s.CreateSandbox(ctx, &Sandbox{
			ID: fmt.Sprintf("sb_%013d", i), OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
			ExternalUserID: fmt.Sprintf("u%d", i%1000),
			State:          states[i%len(states)], Desired: "running", Tier: "process",
			MemMiB: 512, VCPUs: 1,
			CreatedAt: now.Add(-time.Duration(n-i) * time.Second),
			UpdatedAt: now,
		})
	}
}

func benchStore(tb testing.TB, n int) Store {
	tb.Helper()
	s, err := OpenSQLite(tb.TempDir() + "/scale.db")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { s.Close() })
	seedSandboxes(tb, s, n)
	return s
}

const scaleN = 100_000

// BenchmarkListSandboxesAtScale: the API's list path. WHERE project_id = ?
// ORDER BY created_at DESC LIMIT 50 over 100k rows in one project.
func BenchmarkListSandboxesAtScale(b *testing.B) {
	s := benchStore(b, scaleN)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.ListSandboxes(ctx, SandboxFilter{ProjectID: "prj_1", Limit: 50})
		if err != nil || len(rows) != 50 {
			b.Fatalf("list: %v %d", err, len(rows))
		}
	}
}

// BenchmarkCountSandboxesByProjectAtScale: the per-create quota check.
func BenchmarkCountSandboxesByProjectAtScale(b *testing.B) {
	s := benchStore(b, scaleN)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.CountSandboxesByProject(ctx, "prj_1"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSandboxesForReconcileAtScale: runs every 2 seconds on every node.
func BenchmarkSandboxesForReconcileAtScale(b *testing.B) {
	s := benchStore(b, scaleN)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.SandboxesForReconcile(ctx)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(rows)), "rows/op")
	}
}
