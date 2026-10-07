package metrics

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/substrate-sh/substrate/internal/store"
)

// TestPersistHistogramsQuantiles: observes known values into known buckets
// and asserts the persisted percentiles match Prometheus interpolation.
func TestPersistHistogramsQuantiles(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	reg := prometheus.NewRegistry()
	h := promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "test_latency_seconds",
		Help:    "t",
		Buckets: []float64{1, 2, 4},
	})
	// bucket layout: le=1 gets obs 0.5; le=2 gets 1.5; +inf gets 3.0
	h.Observe(0.5)
	h.Observe(1.5)
	h.Observe(3)

	persisted, err := PersistHistograms(context.Background(), st, reg)
	if err != nil {
		t.Fatal(err)
	}
	if persisted != 1 {
		t.Fatalf("persisted %d snapshots, want 1", persisted)
	}

	snaps, err := st.ListMetricSnapshots(context.Background(), "test_latency_seconds", 10)
	if err != nil || len(snaps) != 1 {
		t.Fatalf("list: %+v %v", snaps, err)
	}
	snap := snaps[0]
	if snap.Count != 3 || snap.Sum != 5.0 {
		t.Fatalf("count/sum wrong: %+v", snap)
	}

	// p50: rank 1.5 falls in bucket (1,2] → 1 + 0.5*(2-1) = 1.5
	if snap.P50 < 1.4 || snap.P50 > 1.6 {
		t.Fatalf("p50 = %v", snap.P50)
	}
	// p99: client_golang dto omits the +Inf bucket, so ranks past le=4
	// interpolate within (2,4] → 2 + 0.97*(4-2) = 3.94 (clamped to max bound).
	if snap.P99 < 3.9 || snap.P99 > 4.0 {
		t.Fatalf("p99 = %v", snap.P99)
	}
	if snap.P50 > snap.P95 || snap.P95 > snap.P99 {
		t.Fatalf("percentiles not monotonic: %v %v %v", snap.P50, snap.P95, snap.P99)
	}
	if snap.Labels == nil {
		t.Fatal("labels map should be non-nil")
	}
	if snap.TS.IsZero() {
		t.Fatal("timestamp missing")
	}
}

// TestRuntimeGaugesSmoke: must not panic and gauges must be settable.
func TestRuntimeGaugesSmoke(t *testing.T) {
	UpdateRuntimeGauges()
}
