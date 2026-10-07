package metrics

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/substrate-sh/substrate/internal/store"
)

// TestPersistFiltersInternalCollectors: go_/promhttp_ histograms must not
// flood the queryable ledger.
func TestPersistFiltersInternalCollectors(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	reg := prometheus.NewRegistry()
	product := promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "substrate_http_request_duration_seconds",
		Help:    "t",
		Buckets: []float64{1},
	})
	runtimeHist := promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "go_gc_duration_seconds",
		Help:    "t",
		Buckets: []float64{1},
	})
	promhttpHist := promauto.With(reg).NewHistogram(prometheus.HistogramOpts{
		Name:    "promhttp_metric_handler_requests_seconds",
		Help:    "t",
		Buckets: []float64{1},
	})
	for _, h := range []*prometheus.HistogramVec{} {
		_ = h
	}
	product.Observe(0.5)
	runtimeHist.Observe(0.1)
	promhttpHist.Observe(0.1)

	n, err := PersistHistograms(context.Background(), st, reg)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("persisted %d snapshots, want 1 (product only)", n)
	}

	snaps, err := st.ListMetricSnapshots(ctxHelper(), "", 10000)
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].Name != "substrate_http_request_duration_seconds" {
		t.Fatalf("ledger polluted: %+v", snaps)
	}
	_ = runtimeHist
	_ = promhttpHist
}

func ctxHelper() context.Context { return context.Background() }
