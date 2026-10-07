package metrics

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// failingStore fails every usage insert, standing in for a database outage.
type failingStore struct {
	store.Store
	calls int
}

func (f *failingStore) InsertUsageSamples(ctx context.Context, us []store.UsageSample) error {
	f.calls++
	return errors.New("injected store outage")
}

// TestUsageBufferIsBounded: the flush requeue used to be unbounded, so a store
// outage grew the pending slice until the node OOMed — killing every sandbox
// on the host over a database problem — and re-copied the whole slice on every
// retry, making the work quadratic in outage length.
func TestUsageBufferIsBounded(t *testing.T) {
	fs := &failingStore{}
	s := NewSampler(fs, nil)

	// Far more than the cap, delivered the way an outage would: repeated
	// flushes that all fail.
	const perRound = 50_000
	const rounds = 12 // 600k samples against a 250k cap
	for r := 0; r < rounds; r++ {
		batch := make([]store.UsageSample, perRound)
		for i := range batch {
			batch[i] = store.UsageSample{SandboxID: "sb", TS: time.Now(), Running: true, IntervalS: 15}
		}
		s.mu.Lock()
		s.pending = append(s.pending, batch...)
		s.mu.Unlock()
		s.flush(context.Background())
	}

	s.mu.Lock()
	n := len(s.pending)
	s.mu.Unlock()
	if n > maxPendingSamples {
		t.Fatalf("pending buffer holds %d samples, cap is %d — unbounded growth under a store outage", n, maxPendingSamples)
	}
	if n == 0 {
		t.Fatal("pending buffer emptied entirely; a transient outage must not discard everything")
	}
	if fs.calls != rounds {
		t.Fatalf("flush attempted %d times, want %d", fs.calls, rounds)
	}
}

// TestUsageBufferSurvivesTransientFailure: below the cap, nothing is lost.
func TestUsageBufferSurvivesTransientFailure(t *testing.T) {
	fs := &failingStore{}
	s := NewSampler(fs, nil)
	const n = 100
	batch := make([]store.UsageSample, n)
	for i := range batch {
		batch[i] = store.UsageSample{SandboxID: "sb", TS: time.Now(), Running: true, IntervalS: 15}
	}
	s.mu.Lock()
	s.pending = append(s.pending, batch...)
	s.mu.Unlock()
	s.flush(context.Background())

	s.mu.Lock()
	got := len(s.pending)
	s.mu.Unlock()
	if got != n {
		t.Fatalf("pending = %d after a failed flush, want all %d requeued", got, n)
	}
}
