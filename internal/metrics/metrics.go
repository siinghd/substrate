// Package metrics implements billing-grade metering from the host, never the
// guest (plan §8): cgroup v2 gives authoritative cpu/memory/io; nftables
// counters give per-tap bytes. Samples land in the usage ledger attributed to
// (project, external_user_id) so customers can bill their own users.
package metrics

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/store"
)

type LiveResolver interface {
	LiveIDs() []string
}

type Sampler struct {
	store    store.Store
	cgroupFn func(sandboxID string) *CgroupStats
	log      *logging.Logger
	interval time.Duration
	rollup   time.Duration

	mu      sync.Mutex
	pending []store.UsageSample
}

type CgroupStats struct {
	CPUUS     int64
	MemPeakKB int64
	RxBytes   int64
	TxBytes   int64
}

func NewSampler(st store.Store, log *logging.Logger) *Sampler {
	return &Sampler{store: st, log: log, interval: 15 * time.Second, rollup: time.Minute}
}

// SetCgroupResolver overrides stats sourcing (tests / process-tier fallback).
// Synchronized: production wiring happens before Run, but nothing here should
// depend on call ordering.
func (s *Sampler) SetCgroupResolver(fn func(sandboxID string) *CgroupStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cgroupFn = fn
}

var (
	gaugeLive = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "sandboxes_live",
		Help: "Live machines on this node",
	})
	histRestore = promauto.NewHistogram(prometheus.HistogramOpts{
		Namespace: "substrate", Name: "restore_latency_ms",
		Help:    "Create/resume latency to usable guest",
		Buckets: []float64{25, 50, 100, 200, 300, 500, 1000, 3000, 8000},
	})
	countRestores = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "restores_total",
		Help: "Machine starts by kind",
	})
	gaugeCacheBytes = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "chunk_cache_bytes",
		Help: "Node-local snapshot chunk cache occupancy",
	})
)

func ObserveRestore(ms float64, restored bool) {
	histRestore.Observe(ms)
	countRestores.Inc()
	if restored {
		restoreKind.WithLabelValues("resume").Inc()
	} else {
		restoreKind.WithLabelValues("create").Inc()
	}
}

var restoreKind = promauto.NewCounterVec(prometheus.CounterOpts{
	Namespace: "substrate", Name: "machine_starts_total",
	Help: "VMM starts by kind (create | restore)",
}, []string{"kind"})

var (
	gaugeReconcileQueue = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "reconcile_queue_depth",
		Help: "Sandboxes queued for convergence",
	})
	countReconcileDropped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "reconcile_enqueue_deferred_total",
		Help: "Convergence enqueues deferred to a later tick (queue full or already in flight)",
	})
)

// SetReconcileQueueDepth / AddReconcileDropped expose reconciler backpressure.
// A queue that is persistently deep, or a rising deferred count, is the signal
// that convergence is falling behind desired state.
func SetReconcileQueueDepth(n int) { gaugeReconcileQueue.Set(float64(n)) }
func AddReconcileDropped(n int)    { countReconcileDropped.Add(float64(n)) }

var countEventsDropped = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "substrate", Name: "events_dropped_total",
	Help: "Lifecycle audit events discarded (ring overflow or failed persist)",
})

// IncEventsDropped / AddEventsDropped make lost audit records visible. A Warn
// nobody greps is indistinguishable from the event never having happened.
func IncEventsDropped()      { countEventsDropped.Inc() }
func AddEventsDropped(n int) { countEventsDropped.Add(float64(n)) }

var countCellCrossTenant = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "substrate", Name: "cell_cross_tenant_denied_total",
	Help: "Cell proxy requests refused because the caller does not own the sandbox",
})

// IncCellCrossTenantDenied records an attempt to reach another tenant's
// sandbox through the cell. A nonzero rate is either a broken client or an
// enumeration attempt; either way it must be visible.
func IncCellCrossTenantDenied() { countCellCrossTenant.Inc() }

var (
	countSandboxStarts = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate", Name: "sandbox_starts_total",
		Help: "Sandbox starts by isolation tier and outcome",
	}, []string{"tier", "outcome"})
	countSandboxStops = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate", Name: "sandbox_stops_total",
		Help: "Sandbox stops by isolation tier and reason (killed | exited)",
	}, []string{"tier", "reason"})
	countDriverErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate", Name: "driver_errors_total",
		Help: "Driver operations that failed, by tier and operation",
	}, []string{"tier", "op"})
	// Reconnects are NOT errors. They were briefly counted as one, which would
	// have polluted the exact series an operator alerts on for "this node
	// cannot run sandboxes" with a routine recovery event.
	countAgentReconnects = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate", Name: "agent_reconnects_total",
		Help: "Guest agent reconnections after a transport drop, by tier (recovery, not failure)",
	}, []string{"tier"})
)

// IncSandboxStart / IncSandboxStop / IncDriverError are the driver-level
// counters. Label values are driver-supplied constants, never anything a
// tenant controls, so cardinality stays bounded (tiers x a fixed outcome set).
func IncSandboxStart(tier, outcome string) { countSandboxStarts.WithLabelValues(tier, outcome).Inc() }
func IncSandboxStop(tier, reason string)   { countSandboxStops.WithLabelValues(tier, reason).Inc() }
func IncDriverError(tier, op string)       { countDriverErrors.WithLabelValues(tier, op).Inc() }

// IncAgentReconnect records a guest agent coming back after its session
// dropped. The sandbox survived; nothing failed.
func IncAgentReconnect(tier string) { countAgentReconnects.WithLabelValues(tier).Inc() }

func SetLive(n int)              { gaugeLive.Set(float64(n)) }
func SetChunkCacheBytes(n int64) { gaugeCacheBytes.Set(float64(n)) }

// Run samples until ctx dies; flushes 1-minute rollups into the ledger.
// Attribution is batched: one sandbox listing per tick, O(live) total work.
func (s *Sampler) Run(ctx context.Context, resolve LiveResolver, attr func(id string) (projectID, extUserID string)) {
	tick := time.NewTicker(s.interval)
	rollup := time.NewTicker(s.rollup)
	defer tick.Stop()
	defer rollup.Stop()

	for {
		select {
		case <-ctx.Done():
			// The run context is already cancelled; flushing with it would
			// fail every insert and silently drop billing samples on every
			// restart. Flush under a fresh, short-lived context.
			flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			s.flush(flushCtx)
			cancel()
			return
		case <-tick.C:
			now := time.Now()
			live := resolve.LiveIDs()
			if len(live) == 0 {
				continue
			}
			// Batch attribution once per tick instead of per-sandbox queries.
			attrs := make(map[string][2]string, len(live))
			for _, id := range live {
				projectID, extUser := attr(id)
				attrs[id] = [2]string{projectID, extUser}
			}
			// COLLECT OUTSIDE THE LOCK.
			//
			// cgroupFn is an arbitrary driver callback: on the process tier it
			// reads procfs, and on the gVisor tier it used to fork+exec a
			// 119MB `runsc state` with a 3s timeout, once per sandbox. Running
			// that loop under s.mu meant one slow sandbox blocked flush() --
			// the thing that actually persists billing -- for as long as the
			// whole sweep took. The lock exists to protect `pending`, not to
			// serialize I/O against every live sandbox.
			//
			// Complexity: O(n) resolver calls where n = live sandboxes (each
			// now O(1) with no process spawn), and the lock is held for an
			// O(n) slice append with no I/O under it.
			// Snapshot the resolver under the lock (SetCgroupResolver writes
			// it under the same lock), then release it for the whole sweep.
			s.mu.Lock()
			fn := s.cgroupFn
			s.mu.Unlock()
			batch := make([]store.UsageSample, 0, len(live))
			for _, id := range live {
				var cs *CgroupStats
				if fn != nil {
					cs = fn(id)
				}
				if cs == nil {
					continue
				}
				a := attrs[id]
				batch = append(batch, store.UsageSample{
					SandboxID: id, TS: now,
					CPUUS: cs.CPUUS, MemPeakKB: cs.MemPeakKB,
					RxBytes: cs.RxBytes, TxBytes: cs.TxBytes,
					Running: true, ProjectID: a[0], ExternalUserID: a[1],
					// Each sample carries the span it accounts for, so billing
					// never has to assume a cadence (it used to assume 60s
					// against this 15s ticker: a 4x overbill).
					IntervalS: intervalSeconds(s.interval),
				})
			}
			s.mu.Lock()
			s.pending = append(s.pending, batch...)
			s.mu.Unlock()
		case <-rollup.C:
			s.flush(ctx)
		}
	}
}

// intervalSeconds rounds the sampler cadence to whole seconds, never below 1:
// a sub-second interval must not round to a zero-length billing span.
func intervalSeconds(d time.Duration) int64 {
	n := int64(d.Round(time.Second) / time.Second)
	if n < 1 {
		return 1
	}
	return n
}

func (s *Sampler) flush(ctx context.Context) {
	s.mu.Lock()
	batch := s.pending
	s.pending = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return
	}
	// One batched statement per 100 rows. On failure the samples go back to
	// the head of the pending queue so a transient store blip does not lose
	// billing data.
	//
	// The requeue is CAPPED. It used to be unbounded: a store outage grew
	// pending forever (and re-copied the whole slice on every retry, so the
	// work was quadratic in outage length) until the node OOMed — taking every
	// sandbox on it down over a database problem. Past the cap the oldest
	// samples are dropped and COUNTED, because losing bounded billing data
	// visibly beats losing the node silently.
	if err := s.store.InsertUsageSamples(ctx, batch); err != nil {
		if s.log != nil {
			s.log.Warn("usage batch insert failed; requeueing", "rows", len(batch), "err", err)
		}
		s.mu.Lock()
		s.pending = append(batch, s.pending...)
		if over := len(s.pending) - maxPendingSamples; over > 0 {
			s.pending = s.pending[over:]
			countUsageDropped.Add(float64(over))
			if s.log != nil {
				s.log.Error("usage buffer full; dropped oldest samples",
					"dropped", over, "cap", maxPendingSamples)
			}
		}
		gaugeUsagePending.Set(float64(len(s.pending)))
		s.mu.Unlock()
		countUsageFlushErrors.Inc()
		return
	}
	s.mu.Lock()
	gaugeUsagePending.Set(float64(len(s.pending)))
	s.mu.Unlock()
}

// maxPendingSamples bounds the in-memory billing buffer. At the design centre
// (2,000 resident sandboxes, one sample per 15s => 8,000 rows/min) this holds
// roughly 30 minutes of outage before anything is dropped.
const maxPendingSamples = 250_000

var (
	countUsageDropped = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "usage_samples_dropped_total",
		Help: "Billing samples discarded because the pending buffer was full",
	})
	countUsageFlushErrors = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "usage_flush_errors_total",
		Help: "Failed usage-ledger flushes",
	})
	gaugeUsagePending = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "substrate", Name: "usage_samples_pending",
		Help: "Billing samples buffered in memory awaiting flush",
	})
)

// Pending returns and clears buffered samples (used by tests).
func (s *Sampler) Pending() []store.UsageSample {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.pending
	s.pending = nil
	return out
}
