package metrics

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// nopStore accepts every insert; the sampler flushes on ctx cancel and a nil
// store would panic there rather than exercise anything.
type nopStore struct{ store.Store }

func (nopStore) InsertUsageSamples(ctx context.Context, us []store.UsageSample) error { return nil }

type fakeLive struct{ ids []string }

func (f fakeLive) LiveIDs() []string { return f.ids }

// TestSamplerDoesNotHoldTheLockAcrossStatCollection.
//
// The sampler's cgroup resolver is an arbitrary driver callback: on the gVisor
// tier it used to fork+exec a 119MB `runsc state` per sandbox with a 3s
// timeout. That loop ran under the SAME mutex that guards flush(), so one slow
// sandbox blocked the write of every billing sample already collected, and at
// the 2,000-resident target a broken runsc could stall billing indefinitely.
//
// This test attempts exactly that: a resolver that blocks, and a concurrent
// flush that must still get through. It deadlocks (times out) against the old
// lock discipline.
func TestSamplerDoesNotHoldTheLockAcrossStatCollection(t *testing.T) {
	s := NewSampler(nopStore{}, nil)
	s.interval = 10 * time.Millisecond
	s.rollup = time.Hour // never auto-flush; we drive it

	// Pre-load a sample so the concurrent flush has real work to do.
	s.mu.Lock()
	s.pending = append(s.pending, store.UsageSample{SandboxID: "sb_preexisting01"})
	s.mu.Unlock()

	entered := make(chan struct{})
	release := make(chan struct{})
	var calls int64
	s.SetCgroupResolver(func(id string) *CgroupStats {
		if atomic.AddInt64(&calls, 1) == 1 {
			close(entered)
			<-release // the "slow sandbox"
		}
		return &CgroupStats{CPUUS: 1}
	})

	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("sb_%013d", i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, fakeLive{ids: ids}, func(string) (string, string) { return "proj", "user" })

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("sampler never called the resolver")
	}

	// While collection is stuck, the lock must be free. Anything that needs
	// s.mu -- flush, in production -- has to be able to proceed.
	got := make(chan int, 1)
	go func() {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		got <- n
	}()

	select {
	case n := <-got:
		if n < 1 {
			t.Fatalf("expected the pre-existing pending sample to be reachable, got %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sampler holds s.mu across stat collection: a single slow sandbox blocks every billing flush on the node")
	}
	close(release)
}

// TestSamplerAppendsEveryCollectedSample guards the refactor above: moving
// collection out of the lock must not lose or duplicate samples.
func TestSamplerAppendsEveryCollectedSample(t *testing.T) {
	s := NewSampler(nopStore{}, nil)
	s.interval = 10 * time.Millisecond
	s.rollup = time.Hour
	s.SetCgroupResolver(func(id string) *CgroupStats { return &CgroupStats{CPUUS: 7} })

	ids := []string{"sb_0000000000001", "sb_0000000000002", "sb_0000000000003"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx, fakeLive{ids: ids}, func(string) (string, string) { return "proj", "user" })

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		n := len(s.pending)
		s.mu.Unlock()
		if n >= len(ids) {
			seen := map[string]int{}
			s.mu.Lock()
			for _, p := range s.pending {
				seen[p.SandboxID]++
				if p.CPUUS != 7 {
					t.Fatalf("sample for %s lost its stats: %+v", p.SandboxID, p)
				}
			}
			s.mu.Unlock()
			for _, id := range ids {
				if seen[id] == 0 {
					t.Fatalf("sandbox %s produced no sample", id)
				}
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("sampler never produced a full tick of samples")
}
