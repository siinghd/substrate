package metrics

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"

	"github.com/substrate-sh/substrate/internal/store"
)

// PersistHistograms gathers every registered histogram and writes timestamped
// percentile snapshots to the ledger. This is the durable half of the metrics
// module: Prometheus holds the live window; SQL holds the trend history.
// Call on a fixed cadence (60s) — O(histograms × buckets) per tick.
func PersistHistograms(ctx context.Context, st store.Store, g prometheus.Gatherer) (int, error) {
	families, err := g.Gather()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	total := 0
	var batch []store.MetricSnapshot

	for _, mf := range families {
		if mf.GetType() != dto.MetricType_HISTOGRAM {
			continue
		}
		name := mf.GetName()
		// Skip Go runtime / client-library self-metrics: they would flood the
		// queryable ledger with non-product rows every tick.
		if strings.HasPrefix(name, "go_") || strings.HasPrefix(name, "promhttp_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			h := m.GetHistogram()
			if h == nil || h.GetSampleCount() == 0 {
				continue
			}
			labels := map[string]string{}
			for _, lp := range m.GetLabel() {
				labels[lp.GetName()] = lp.GetValue()
			}
			p50 := histogramQuantile(0.5, h)
			p95 := histogramQuantile(0.95, h)
			p99 := histogramQuantile(0.99, h)

			snap := store.MetricSnapshot{
				Name:   mf.GetName(),
				Labels: labels,
				Count:  h.GetSampleCount(),
				Sum:    h.GetSampleSum(),
				P50:    p50,
				P95:    p95,
				P99:    p99,
				TS:     now,
			}
			batch = append(batch, snap)
			total++
		}
	}
	if len(batch) > 0 {
		if err := st.InsertMetricSnapshots(ctx, batch); err != nil {
			return 0, err
		}
		metricSnapshotsPersisted.Add(float64(len(batch)))
	}
	return total, nil
}

// histogramQuantile implements the standard cumulative-bucket interpolation
// used by Prometheus: find the bucket where the target rank falls, then
// interpolate linearly within it; the +Inf bucket assumes the width of the
// last finite bucket.
func histogramQuantile(q float64, h *dto.Histogram) float64 {
	rank := q * float64(h.GetSampleCount())
	var cum float64
	buckets := h.GetBucket()
	lastBound := 0.0
	for i, b := range buckets {
		cum = float64(b.GetCumulativeCount())
		if cum >= rank {
			upper := b.GetUpperBound()
			lower := lastBound
			if i > 0 {
				lower = buckets[i-1].GetUpperBound()
			} else {
				lower = upper / 2 // first finite bucket: assume half-width below
			}
			prevCum := 0.0
			if i > 0 {
				prevCum = float64(buckets[i-1].GetCumulativeCount())
			}
			if cum == prevCum {
				return lower
			}
			frac := (rank - prevCum) / (cum - prevCum)
			return lower + frac*(upper-lower)
		}
		lastBound = b.GetUpperBound()
	}
	return lastBound
}

var metricSnapshotsPersisted = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "substrate", Name: "metric_snapshots_persisted_total",
	Help: "Histogram percentile rows written to the ledger",
})

// RuntimeGauges updates process resource gauges — goroutines and heap are the
// two signals that catch leak regressions between releases.
func UpdateRuntimeGauges() {
	runtimeGoroutines.Set(float64(runtime.NumGoroutine()))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	runtimeHeapBytes.Set(float64(ms.HeapAlloc))
}

var (
	runtimeGoroutines = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "runtime_goroutines",
		Help: "Live goroutines",
	})
	runtimeHeapBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "runtime_heap_bytes",
		Help: "Heap bytes allocated",
	})
)
