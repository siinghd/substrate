package metrics

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/substrate-sh/substrate/internal/store"
)

// Span is one named stage timing (e.g. sandbox.create: placement, driver
// start, guest handshake). Spans ≥ slowThreshold persist into the events
// ledger — queryable SQL-side for trend graphs; all of them feed Prometheus.
type Span struct {
	Operation string
	SandboxID string
	Stage     string
}

type spanKey struct{}

var slowThreshold = 250 * time.Millisecond

// StartSpan attaches a stage timer to ctx. Persisted on EndSpan.
func StartSpan(ctx context.Context, op, sandboxID, stage string) (context.Context, func()) {
	start := time.Now()
	done := func() {
		d := time.Since(start)
		StageDuration.WithLabelValues(op, stage).Observe(d.Seconds())
		if d >= slowThreshold && sandboxID != "" {
			data, _ := json.Marshal(map[string]any{
				"stage":       stage,
				"duration_ms": d.Milliseconds(),
			})
			_ = appendEvent(ctx, store.Event{
				ID: eventID(), TS: time.Now(), SandboxID: sandboxID,
				Type: op + ".stage." + stage, Data: data,
			})
		}
	}
	return context.WithValue(ctx, spanKey{}, done), done
}

// weak sink indirection avoids an import cycle with store consumers that also
// import metrics; wired once at boot.
var eventSink func(context.Context, store.Event) error

func appendEvent(ctx context.Context, e store.Event) error {
	if eventSink == nil {
		return nil
	}
	return eventSink(ctx, e)
}

// SetEventSink wires the persistence sink exactly once at startup.
func SetEventSink(fn func(context.Context, store.Event) error) { eventSink = fn }

func eventID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return "ev_" + hex.EncodeToString(b)
}

// ObserveStage records a named stage duration (Prometheus histogram; slow
// spans additionally persist via the events sink).
func ObserveStage(operation, stage string, d time.Duration) {
	StageDuration.WithLabelValues(operation, stage).Observe(d.Seconds())
}

// StageDuration is exported for cross-package stage instrumentation.
var StageDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
	Namespace: "substrate",
	Name:      "stage_duration_seconds",
	Help:      "Per-stage timings: placement, driver start, handshake, fs, exec",
	Buckets:   []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
}, []string{"operation", "stage"})
