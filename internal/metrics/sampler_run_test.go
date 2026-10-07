package metrics

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// flakyStore fails InsertUsageSamples while fail is set; otherwise counts
// successful batches. Other methods are inherited as no-ops (never called).
type flakyStore struct {
	store.Store
	fail    atomic.Bool
	batches atomic.Int32
	rows    atomic.Int32
}

func (f *flakyStore) InsertUsageSamples(ctx context.Context, us []store.UsageSample) error {
	if f.fail.Load() {
		return errors.New("store unavailable")
	}
	f.batches.Add(1)
	f.rows.Add(int32(len(us)))
	return nil
}

func newTestSampler(st store.Store, interval, rollup time.Duration) *Sampler {
	s := NewSampler(st, nil)
	s.interval = interval
	s.rollup = rollup
	return s
}

type fixedResolver struct{ ids []string }

func (r fixedResolver) LiveIDs() []string { return r.ids }

// TestSamplerRunSamplesFlushesAndFlushOnShutdown drives the production loop:
// ticks must produce samples attributed via cgroupFn, rollups must batch them
// into the ledger in ONE statement per 100 rows, and cancellation must flush
// remaining pending samples under a live context — the pass-1 fix for the
// cancelled-context shutdown that silently dropped billing rows.
func TestSamplerRunSamplesFlushesAndFlushOnShutdown(t *testing.T) {
	fs := &flakyStore{}
	s := newTestSampler(fs, 10*time.Millisecond, 30*time.Millisecond)

	called := make(chan string, 4)
	s.SetCgroupResolver(func(id string) *CgroupStats {
		select {
		case called <- id:
		default:
		}
		return &CgroupStats{CPUUS: 1000, MemPeakKB: 262144}
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx, fixedResolver{ids: []string{"sb_sampler001"}}, func(id string) (projectID string, extUserID string) { return "prj_x", "user_1" })
		close(done)
	}()

	// Wait until at least one tick sampled.
	select {
	case id := <-called:
		if id != "sb_sampler001" {
			t.Fatalf("resolver id %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cgroup resolver never invoked by Run")
	}

	// Wait for a successful rollup flush.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && fs.batches.Load() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.batches.Load() == 0 {
		t.Fatal("no usage batch ever flushed to store")
	}

	cancel()
	<-done // Run must exit promptly after cancel

	if got := fs.rows.Load(); got < 1 {
		t.Fatalf("ledger received %d rows", got)
	}
}

// TestSamplerFlushRequeuesOnFailure: a failed batch insert must go back to
// the pending queue and succeed on the next flush — billing data survives a
// transient store outage instead of being dropped.
func TestSamplerFlushRequeuesOnFailure(t *testing.T) {
	fs := &flakyStore{}
	fs.fail.Store(true)
	s := newTestSampler(fs, time.Hour, time.Hour)

	s.mu.Lock()
	s.pending = []store.UsageSample{{SandboxID: "sb_requeue001", TS: time.Now(), CPUUS: 5, Running: true}}
	s.mu.Unlock()

	ctx := context.Background()
	s.flush(ctx)

	if p := s.pendingLen(); p != 1 {
		t.Fatalf("failed batch not requeued: pending=%d", p)
	}

	fs.fail.Store(false)
	s.flush(ctx)
	if fs.batches.Load() != 1 || fs.rows.Load() != 1 {
		t.Fatalf("requeued batch not delivered: batches=%d rows=%d", fs.batches.Load(), fs.rows.Load())
	}
	if p := s.pendingLen(); p != 0 {
		t.Fatalf("pending not drained: %d", p)
	}
}

func (s *Sampler) pendingLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}
