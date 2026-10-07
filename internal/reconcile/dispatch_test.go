package reconcile

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// slowDriver blocks in Create for a fixed delay, and optionally blocks one
// specific sandbox until released — standing in for a hung VMM.
type slowDriver struct {
	delay time.Duration

	stuckID string
	release chan struct{}

	// inFlight / peak record how many Creates are running AT ONCE. That is
	// the property "the pool parallelises" actually means, and unlike wall
	// clock it is not a measurement of how busy the machine running the test
	// happens to be.
	inFlight int64
	peak     int64

	mu    sync.Mutex
	given []*fakeMachine
}

// peakConcurrency returns the high-water mark of simultaneous Creates.
func (d *slowDriver) peakConcurrency() int64 { return atomic.LoadInt64(&d.peak) }

func (d *slowDriver) Name() string   { return "slow" }
func (d *slowDriver) Caps() vmm.Caps { return vmm.Caps{Tier: "process", Pause: true} }

func (d *slowDriver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	cur := atomic.AddInt64(&d.inFlight, 1)
	for {
		old := atomic.LoadInt64(&d.peak)
		if cur <= old || atomic.CompareAndSwapInt64(&d.peak, old, cur) {
			break
		}
	}
	defer atomic.AddInt64(&d.inFlight, -1)

	if spec.ID == d.stuckID {
		select {
		case <-d.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	} else {
		time.Sleep(d.delay)
	}
	m := &fakeMachine{id: spec.ID, done: make(chan struct{})}
	d.mu.Lock()
	d.given = append(d.given, m)
	d.mu.Unlock()
	return m, nil
}

func (d *slowDriver) Restore(ctx context.Context, spec vmm.Spec, mem, snap string) (vmm.Machine, error) {
	return d.Create(ctx, spec)
}

func dispatchSetup(t *testing.T, drv vmm.Driver, n int) (*Supervisor, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(t.TempDir() + "/d.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", CreatedAt: now})
	st.PutTemplate(ctx, &store.Template{ID: "tpl_1", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})
	for i := 0; i < n; i++ {
		st.CreateSandbox(ctx, &store.Sandbox{
			ID: fmt.Sprintf("sb_disp%08d", i), OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
			State: store.StateCreating, Desired: "running", Tier: "process",
			MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	sup := NewSupervisor(st, drv, newTestLogger(), t.TempDir())
	go sup.Events(ctx)
	return sup, st
}

// TestTickDispatchesInsteadOfSerializing: Tick used to call the blocking VMM
// create inline, so one pass cost the SUM of every start it decided to do.
// After a node restart with 2,000 sandboxes that is ~10 minutes of strictly
// serial work, and nothing else in the loop — TTLs, destroys, idle pauses —
// runs until it finishes.
func TestTickDispatchesInsteadOfSerializing(t *testing.T) {
	const n = 64
	const delay = 20 * time.Millisecond
	serial := time.Duration(n) * delay

	drv := &slowDriver{delay: delay, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup, _ := dispatchSetup(t, drv, n)
	sup.StartWorkers(ctx, 16)

	start := time.Now()
	sup.Tick(ctx)
	dispatchTook := time.Since(start)

	// Tick itself must be dispatch-only: no blocking VMM work in the loop.
	if dispatchTook > delay*4 {
		t.Fatalf("Tick took %v dispatching %d sandboxes; it is still doing blocking work inline (one create is %v)",
			dispatchTook, n, delay)
	}

	if !sup.DrainForTest(serial) {
		t.Fatalf("convergence did not drain within the old serial cost (%v)", serial)
	}
	total := time.Since(start)
	if got := sup.LiveCount(); got != n {
		t.Fatalf("converged %d of %d sandboxes", got, n)
	}
	// PARALLELISM IS ASSERTED BY OBSERVED CONCURRENCY, NOT BY WALL CLOCK.
	//
	// This used to be `total > serial/2 -> fail`. On a loaded box -- the whole
	// -race suite running in parallel, which is exactly when it matters -- 16
	// goroutines sleeping 20ms are not scheduled promptly, so the assertion
	// measured the machine and failed on correct code. A test that cries wolf
	// under load is worse than no test: it is the thing that answers "did I
	// regress?", and it answered wrong.
	//
	// Peak simultaneous Creates is the property itself, and it is immune to
	// how many cores the test got. Serial execution pins it at exactly 1.
	//
	// Threshold measured, not guessed: 16/16 without -race, 8-11 across eight
	// runs WITH -race (the detector's scheduling overhead lowers it, and part
	// of the path serializes on the store). 4 is decisively parallel and sits
	// well clear of that floor -- an 8 would sit on it, which is how the
	// previous version of this assertion managed to be flaky too.
	const minConcurrent = 4
	if peak := drv.peakConcurrency(); peak < minConcurrent {
		t.Fatalf("peak concurrent creates was %d with 16 workers over %d sandboxes: the pool is not parallelising (serial would be 1)",
			peak, n)
	}
	// Wall clock stays as a loose backstop only: it must beat serial, but is
	// given the full serial budget rather than half of it.
	if total > serial {
		t.Fatalf("convergence took %v, worse than the serial cost %v", total, serial)
	}
	t.Logf("converged %d sandboxes in %v (serial cost would be %v), peak concurrency %d",
		n, total, serial, drv.peakConcurrency())
}

// TestStuckSandboxDoesNotStallOthers: with convergence inline, a single VMM
// that never returns wedged the whole reconciler. The pool must isolate it.
func TestStuckSandboxDoesNotStallOthers(t *testing.T) {
	const n = 24
	drv := &slowDriver{delay: time.Millisecond, stuckID: "sb_disp00000000", release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup, _ := dispatchSetup(t, drv, n)
	sup.StartWorkers(ctx, 8)
	defer close(drv.release)

	sup.Tick(ctx)

	// Every sandbox except the stuck one must come up while it is still hung.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if sup.LiveCount() >= n-1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := sup.LiveCount(); got < n-1 {
		t.Fatalf("only %d of %d sandboxes converged while one VMM was hung: the hang is still contagious", got, n-1)
	}
	if _, live := sup.Get("sb_disp00000000"); live {
		t.Fatal("the deliberately stuck sandbox reported live")
	}
}

// TestEnqueueDedupsAndBounds: the queue is deliberately lossy because
// reconciliation is level-triggered, but it must never double-queue a sandbox
// (which would let one slow sandbox starve every other).
func TestEnqueueDedupsAndBounds(t *testing.T) {
	drv := &countingDriver{}
	sup, _ := dispatchSetup(t, drv, 0)

	if !sup.enqueue("sb_x") {
		t.Fatal("first enqueue rejected")
	}
	if sup.enqueue("sb_x") {
		t.Fatal("same sandbox queued twice")
	}
	// Fill past capacity: overflow must be refused, not grow the queue.
	accepted := 1
	for i := 0; i < reconcileQueueDepth+64; i++ {
		if sup.enqueue(fmt.Sprintf("sb_fill%d", i)) {
			accepted++
		}
	}
	if accepted > reconcileQueueDepth {
		t.Fatalf("accepted %d enqueues, queue capacity is %d", accepted, reconcileQueueDepth)
	}
	sup.workMu.Lock()
	inflight := len(sup.inflight)
	sup.workMu.Unlock()
	if inflight != accepted {
		t.Fatalf("inflight bookkeeping drifted: %d tracked, %d accepted — refused enqueues must not leak entries", inflight, accepted)
	}
}
