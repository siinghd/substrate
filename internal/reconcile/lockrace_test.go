package reconcile

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// countingDriver is fakeDriver with atomic counters and a deliberate delay, so
// concurrent starts genuinely overlap instead of finishing before the next one
// begins. It also records every machine it hands out, so a leaked (never
// killed) machine is detectable at the end.
type countingDriver struct {
	delay time.Duration

	created  atomic.Int64
	restored atomic.Int64

	mu    sync.Mutex
	given []*fakeMachine
}

func (d *countingDriver) Name() string   { return "counting" }
func (d *countingDriver) Caps() vmm.Caps { return vmm.Caps{Tier: "process", Pause: true} }

func (d *countingDriver) new(id string) vmm.Machine {
	time.Sleep(d.delay)
	m := &fakeMachine{id: id, done: make(chan struct{})}
	d.mu.Lock()
	d.given = append(d.given, m)
	d.mu.Unlock()
	return m
}

func (d *countingDriver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	d.created.Add(1)
	return d.new(spec.ID), nil
}

func (d *countingDriver) Restore(ctx context.Context, spec vmm.Spec, mem, snap string) (vmm.Machine, error) {
	d.restored.Add(1)
	return d.new(spec.ID), nil
}

// liveMachines counts machines the driver produced that were never killed.
func (d *countingDriver) liveMachines() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, m := range d.given {
		select {
		case <-m.done:
		default:
			n++
		}
	}
	return n
}

func raceSetup(t *testing.T, delay time.Duration) (*Supervisor, *countingDriver, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(t.TempDir() + "/race.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", CreatedAt: now})
	st.PutTemplate(ctx, &store.Template{ID: "tpl_1", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	drv := &countingDriver{delay: delay}
	sup := NewSupervisor(st, drv, newTestLogger(), t.TempDir())
	go sup.Events(ctx)
	return sup, drv, st
}

func seedSandbox(t *testing.T, st store.Store, id string) *store.Sandbox {
	t.Helper()
	sb := &store.Sandbox{
		ID: id, OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateCreating, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := st.CreateSandbox(context.Background(), sb); err != nil {
		t.Fatal(err)
	}
	return sb
}

// TestPauseResumeRaceKeepsRowAndLivenessAgreeing: Pause used to skip the
// per-sandbox lock that EnsureRunning takes, so an idle-pause from Tick could
// interleave with a resume from an API request. Pause killed the VMM and
// cleared the map, the resume saw not-live and started a second machine
// writing state=running, then Pause finished by writing state=paused with a
// PauseRef captured before any of it. The row claimed paused while a machine
// was live — and that stale PauseRef would later be restored on top of it.
func TestPauseResumeRaceKeepsRowAndLivenessAgreeing(t *testing.T) {
	sup, _, st := raceSetup(t, 2*time.Millisecond)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")
	sb := seedSandbox(t, st, "sb_race00000001")

	if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
		t.Fatal(err)
	}

	// Several pausers and resumers, not one of each: the losing interleaving
	// is narrow, and a single pair rarely lands in it.
	const rounds = 80
	const workers = 6
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				_ = sup.Pause(ctx, sb.ID)
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				cur, err := st.GetSandbox(ctx, sb.ID)
				if err != nil {
					continue
				}
				_ = sup.Resume(ctx, cur, tpl, nil)
			}
		}()
	}
	wg.Wait()

	// Quiesce, then the row and the machine table must tell the same story.
	time.Sleep(50 * time.Millisecond)
	got, err := st.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, live := sup.Get(sb.ID)
	if got.State == store.StatePaused && live {
		t.Fatalf("row says paused but a machine is live (pause_ref=%q): the stale PauseRef will be restored over a running VM", got.PauseRef)
	}
	if got.State == store.StateRunning && !live {
		t.Fatal("row says running but no machine is live")
	}
}

// TestDestroyRaceLeavesNoOrphanMachine: Destroy also skipped the lock. Racing
// EnsureRunning it killed the machine and then an in-flight start registered a
// fresh one — a live VMM with no owning row, on the network, invisible to the
// reconciler.
func TestDestroyRaceLeavesNoOrphanMachine(t *testing.T) {
	sup, drv, st := raceSetup(t, 2*time.Millisecond)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	const n = 30
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		sb := seedSandbox(t, st, "sb_destroy"+string(rune('a'+i%26))+string(rune('a'+i/26))+"0000")
		wg.Add(2)
		go func(sb *store.Sandbox) {
			defer wg.Done()
			_ = sup.EnsureRunning(ctx, sb, tpl, nil)
		}(sb)
		go func(sb *store.Sandbox) {
			defer wg.Done()
			_ = sup.Destroy(ctx, sb.ID)
		}(sb)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	// Every sandbox whose row says destroyed must have no live machine, and
	// no machine the driver handed out may still be running unowned.
	for _, m := range sup.LiveSandboxes() {
		sb, err := st.GetSandbox(ctx, m)
		if err != nil {
			t.Fatalf("live machine %s has no row at all", m)
		}
		if sb.State == store.StateDestroyed {
			t.Fatalf("sandbox %s is destroyed in the store but its machine is still live", m)
		}
	}
	if leaked := drv.liveMachines() - sup.LiveCount(); leaked > 0 {
		t.Fatalf("%d machines started but never killed and not in the supervisor's table: orphaned VMMs", leaked)
	}
}

// TestShutdownStopsEveryMachine: noded's signal handler drained HTTP and
// cancelled the context but never touched the machine table. The VMMs outlive
// the agent (the jailer daemonizes Firecracker; the process driver sets Setpgid
// with no Pdeathsig), so a routine SIGTERM left every sandbox running while the
// replacement agent — seeing an empty map and rows that still say "running" —
// started a SECOND VMM for each on top of the same disk image.
func TestShutdownStopsEveryMachine(t *testing.T) {
	sup, drv, st := raceSetup(t, 0)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	const n = 40
	for i := 0; i < n; i++ {
		sb := seedSandbox(t, st, "sb_shut"+string(rune('a'+i%26))+string(rune('a'+i/26))+"00000")
		if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
			t.Fatal(err)
		}
	}
	if sup.LiveCount() != n {
		t.Fatalf("setup: %d live, want %d", sup.LiveCount(), n)
	}

	shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	sup.Shutdown(shutCtx)

	if live := sup.LiveCount(); live != 0 {
		t.Fatalf("%d machines still in the supervisor table after Shutdown", live)
	}
	if orphans := drv.liveMachines(); orphans != 0 {
		t.Fatalf("%d VMMs were started and never stopped: they outlive the agent as orphans", orphans)
	}
}

// TestShutdownRespectsDeadline: shutdown must be bounded, or a drain window can
// be exceeded by one stuck machine.
func TestShutdownRespectsDeadline(t *testing.T) {
	sup, _, st := raceSetup(t, 0)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")
	sb := seedSandbox(t, st, "sb_deadline00001")
	if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
		t.Fatal(err)
	}

	dead, cancel := context.WithCancel(ctx)
	cancel() // already expired
	done := make(chan struct{})
	go func() { sup.Shutdown(dead); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown ignored an expired context")
	}
}

// TestShutdownWinsAgainstConcurrentStarts: Shutdown took ONE snapshot of the
// live set while the convergence pool and the 2s tick loop were still running
// on the root context during the HTTP drain. Pausing 800 machines takes far
// longer than a tick, so any machine started after the snapshot — by a draining
// worker or an API resumeIfNeeded — was never visited and was orphaned at exit.
func TestShutdownWinsAgainstConcurrentStarts(t *testing.T) {
	sup, drv, st := raceSetup(t, time.Millisecond)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	const n = 40
	sbs := make([]*store.Sandbox, 0, n)
	for i := 0; i < n; i++ {
		sbs = append(sbs, seedSandbox(t, st, "sb_drain"+string(rune('a'+i%26))+string(rune('a'+i/26))+"0000"))
	}
	for _, sb := range sbs[:10] {
		if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
			t.Fatal(err)
		}
	}

	// Keep starting machines while the shutdown runs, exactly as a draining
	// worker pool and the tick loop would.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(off int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				_ = sup.EnsureRunning(ctx, sbs[(i+off)%n], tpl, nil)
			}
		}(w * 7)
	}

	shutCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sup.Shutdown(shutCtx)
	close(stop)
	wg.Wait()

	// Nothing may be live, and nothing the driver handed out may still be
	// running: those are the orphans the drain exists to prevent.
	if live := sup.LiveCount(); live != 0 {
		t.Errorf("%d machines still live after Shutdown despite concurrent starts", live)
	}
	if orphans := drv.liveMachines(); orphans != 0 {
		t.Errorf("%d VMMs were started and never stopped: they outlive the agent", orphans)
	}
	// And a start attempted after shutdown must be refused, not silently
	// succeed and leak.
	if err := sup.EnsureRunning(ctx, sbs[0], tpl, nil); err == nil {
		t.Error("EnsureRunning succeeded after Shutdown; the machine has nobody to stop it")
	}
}
