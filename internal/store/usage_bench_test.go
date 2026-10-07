package store

import (
	"context"
	"testing"
	"time"
)

// BenchmarkUsageInsertBatch quantifies the PASS-4 change: the sampler used to
// issue one INSERT per sample per rollup (N round trips on a single-conn
// SQLite); it now issues ceil(N/100) batched statements.
func BenchmarkUsageInsertPerRow(b *testing.B) {
	benchUsage(b, func(s Store, ctx context.Context, rows []UsageSample) {
		for _, u := range rows {
			if err := s.InsertUsageSample(ctx, u); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkUsageInsertBatched(b *testing.B) {
	benchUsage(b, func(s Store, ctx context.Context, rows []UsageSample) {
		if err := s.InsertUsageSamples(ctx, rows); err != nil {
			b.Fatal(err)
		}
	})
}

func benchUsage(b *testing.B, insert func(Store, context.Context, []UsageSample)) {
	s, err := OpenSQLite(b.TempDir() + "/bench.db")
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	rows := make([]UsageSample, 100)
	for i := range rows {
		rows[i] = UsageSample{
			SandboxID: "sb_benchmark001", TS: time.Now(),
			CPUUS: int64(i), MemPeakKB: 512 << 10,
			ProjectID: "prj_default", ExternalUserID: "user_1", Running: true,
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		insert(s, ctx, rows)
	}
}
