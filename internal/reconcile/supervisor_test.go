package reconcile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// fakeDriver + fakeMachine simulate the process tier deterministically.
type fakeMachine struct {
	id     string
	done   chan struct{}
	paused bool
}

func (f *fakeMachine) ID() string     { return f.id }
func (f *fakeMachine) Caps() vmm.Caps { return vmm.Caps{Tier: "process", Pause: true} }
func (f *fakeMachine) Exec(ctx context.Context, spec vmm.ExecSpec) (*vmm.ExecHandle, error) {
	return nil, errors.New("not implemented")
}
func (f *fakeMachine) Fs() *vmm.FsClient     { return nil }
func (f *fakeMachine) Ports() []vmm.PortInfo { return nil }
func (f *fakeMachine) Reseed(ctx context.Context, g string) (vmm.ReseedAck, error) {
	return vmm.ReseedAck{}, nil
}
func (f *fakeMachine) Metrics(ctx context.Context) (*vmm.GuestMetrics, error) {
	return &vmm.GuestMetrics{}, nil
}
func (f *fakeMachine) Pause(ctx context.Context, destDir string) (string, error) {
	f.paused = true
	close(f.done)
	return filepath.Join(destDir, "mem") + "|" + filepath.Join(destDir, "snap"), nil
}
func (f *fakeMachine) Shutdown(ctx context.Context) error { return f.Kill() }
func (f *fakeMachine) Kill() error {
	select {
	case <-f.done:
	default:
		close(f.done)
	}
	return nil
}
func (f *fakeMachine) Done() <-chan struct{} { return f.done }

type fakeDriver struct {
	created  int
	restored int
	fail     bool
}

func (d *fakeDriver) Name() string   { return "fake" }
func (d *fakeDriver) Caps() vmm.Caps { return vmm.Caps{Tier: "process", Pause: true} }
func (d *fakeDriver) Create(ctx context.Context, spec vmm.Spec) (vmm.Machine, error) {
	if d.fail {
		return nil, errors.New("injected")
	}
	d.created++
	return &fakeMachine{id: spec.ID, done: make(chan struct{})}, nil
}
func (d *fakeDriver) Restore(ctx context.Context, spec vmm.Spec, mem, snap string) (vmm.Machine, error) {
	if d.fail {
		return nil, errors.New("injected")
	}
	d.restored++
	return &fakeMachine{id: spec.ID, done: make(chan struct{})}, nil
}

func setup(t *testing.T) (*Supervisor, *fakeDriver, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	now := time.Now()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", CreatedAt: now})
	st.PutTemplate(ctx, &store.Template{ID: "tpl_1", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now})

	drv := &fakeDriver{}
	log := newTestLogger()
	sup := NewSupervisor(st, drv, log, t.TempDir())
	go sup.Events(context.Background())
	return sup, drv, st
}

func newTestLogger() *testLogger { return &testLogger{} }

type testLogger struct{}

func (l *testLogger) Debug(msg string, kv ...any) {}
func (l *testLogger) Info(msg string, kv ...any)  {}
func (l *testLogger) Warn(msg string, kv ...any)  {}
func (l *testLogger) Error(msg string, kv ...any) {}

func TestEnsureRunningAndPauseResumeCycle(t *testing.T) {
	sup, drv, st := setup(t)
	ctx := context.Background()

	sb := &store.Sandbox{
		ID: "sb_1", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateCreating, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	st.CreateSandbox(ctx, sb)
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
		t.Fatal(err)
	}
	if drv.created != 1 || sup.LiveCount() != 1 {
		t.Fatalf("create: %d %d", drv.created, sup.LiveCount())
	}
	// idempotent
	if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
		t.Fatal(err)
	}
	if drv.created != 1 {
		t.Fatalf("double create: %d", drv.created)
	}

	if err := sup.Pause(ctx, "sb_1"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSandbox(ctx, "sb_1")
	if got.State != store.StatePaused || got.PauseRef == "" {
		t.Fatalf("pause state: %+v", got)
	}
	if sup.LiveCount() != 0 {
		t.Fatalf("machine not freed after pause: %d", sup.LiveCount())
	}

	// resume path uses Restore
	sb2, _ := st.GetSandbox(ctx, "sb_1")
	if err := sup.Resume(ctx, sb2, tpl, nil); err != nil {
		t.Fatal(err)
	}
	if drv.restored != 1 {
		t.Fatalf("resume did not restore: %+v", drv)
	}
}

func TestTickIdlePause(t *testing.T) {
	sup, _, st := setup(t)
	ctx := context.Background()

	old := time.Now().Add(-time.Hour)
	sb := &store.Sandbox{
		ID: "sb_idle", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateRunning, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, TTLIdleSecs: 60,
		CreatedAt: old, UpdatedAt: old, LastActivityMS: old.UnixMilli(),
	}
	st.CreateSandbox(ctx, sb)

	sup.Tick(ctx)
	// first tick starts it; second tick sees it idle past TTL and pauses
	sup.Tick(ctx)

	got, _ := st.GetSandbox(ctx, "sb_idle")
	if got.State != store.StatePaused {
		t.Fatalf("idle sandbox not paused: state=%s", got.State)
	}
}

func TestTickTTLDestroy(t *testing.T) {
	sup, _, st := setup(t)
	ctx := context.Background()

	old := time.Now().Add(-10 * time.Hour)
	sb := &store.Sandbox{
		ID: "sb_ttl", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateRunning, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, TTLAbsoluteSecs: 3600,
		CreatedAt: old, UpdatedAt: old, LastActivityMS: time.Now().UnixMilli(),
	}
	st.CreateSandbox(ctx, sb)

	sup.Tick(ctx)
	sup.Tick(ctx)

	got, _ := st.GetSandbox(ctx, "sb_ttl")
	if got.State != store.StateDestroyed {
		t.Fatalf("TTL sandbox not destroyed: %s", got.State)
	}
}

func TestDesiredDestroyedConverges(t *testing.T) {
	sup, _, st := setup(t)
	ctx := context.Background()

	sb := &store.Sandbox{
		ID: "sb_kill", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateRunning, Desired: "destroyed", Tier: "process",
		MemMiB: 512, VCPUs: 1, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	st.CreateSandbox(ctx, sb)
	tpl, _ := st.GetTemplate(ctx, "tpl_1")
	sup.EnsureRunning(ctx, sb, tpl, nil)

	// simulate on-disk scratch (real drivers create it: workdir + pause imgs)
	scratch := filepath.Join(sup.dataDir, "sandboxes", sb.ID)
	os.MkdirAll(filepath.Join(scratch, "pause"), 0o755)
	os.WriteFile(filepath.Join(scratch, "agent.sock"), []byte("x"), 0o644)
	if _, err := os.Stat(scratch); err != nil {
		t.Fatalf("workdir missing while running: %v", err)
	}

	sup.Tick(ctx)
	got, _ := st.GetSandbox(ctx, "sb_kill")
	if got.State != store.StateDestroyed || sup.LiveCount() != 0 {
		t.Fatalf("destroy convergence failed: %s live=%d", got.State, sup.LiveCount())
	}
	// disk scratch must be gone after destroy
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Fatal("destroy leaked sandbox workdir on disk")
	}
}

func TestPressurePauseNewestFirst(t *testing.T) {
	sup, _, st := setup(t)
	ctx := context.Background()
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	base := time.Now()
	for i, id := range []string{"sb_old", "sb_new"} {
		ts := base.Add(time.Duration(i) * time.Minute)
		sb := &store.Sandbox{
			ID: id, OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
			State: store.StateRunning, Desired: "running", Tier: "process",
			MemMiB: 512, VCPUs: 1, CreatedAt: ts, UpdatedAt: ts,
		}
		st.CreateSandbox(ctx, sb)
		sup.EnsureRunning(ctx, sb, tpl, nil)
	}

	if !sup.PressurePause(ctx) {
		t.Fatal("pressure pause found no victim")
	}
	victim, _ := st.GetSandbox(ctx, "sb_new")
	if victim.State != store.StatePaused {
		t.Fatalf("newest sandbox should be paused first: %s", victim.State)
	}
	alive, _ := st.GetSandbox(ctx, "sb_old")
	if alive.State != store.StateRunning {
		t.Fatalf("older sandbox disturbed: %s", alive.State)
	}
}

// TestEnsureRunningConcurrentSingleFlight: N goroutines racing EnsureRunning
// for the SAME sandbox must produce exactly ONE machine. Regression for the
// check-then-act double-start race.
func TestEnsureRunningConcurrentSingleFlight(t *testing.T) {
	sup, drv, st := setup(t)
	ctx := context.Background()
	now := time.Now()
	sb := &store.Sandbox{
		ID: "sb_race00000001", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StateCreating, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now,
	}
	st.CreateSandbox(ctx, sb)
	tpl, _ := st.GetTemplate(ctx, "tpl_1")

	const n = 16
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- sup.EnsureRunning(ctx, sb, tpl, nil)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent EnsureRunning failed: %v", err)
		}
	}
	if drv.created != 1 {
		t.Fatalf("double start under concurrency: %d creates (want 1)", drv.created)
	}
	if sup.LiveCount() != 1 {
		t.Fatalf("live count = %d (want 1)", sup.LiveCount())
	}
}

// TestAbsoluteTTLDestroysPausedSandbox: an idle-paused (non-live) sandbox
// must still be destroyed when its absolute TTL expires — pausing must not
// be a TTL evasion or a permanent disk-retention lever.
func TestAbsoluteTTLDestroysPausedSandbox(t *testing.T) {
	sup, _, st := setup(t)
	ctx := context.Background()
	now := time.Now()
	sb := &store.Sandbox{
		ID: "sb_ttlp000000001", OrgID: "org_1", ProjectID: "prj_1", TemplateID: "tpl_1",
		State: store.StatePaused, Desired: "running", Tier: "process",
		MemMiB: 512, VCPUs: 1, TTLAbsoluteSecs: 60,
		PauseRef:  "/fake/mem|/fake/snap",
		CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now,
	}
	st.CreateSandbox(ctx, sb)

	sup.Tick(ctx)

	got, err := st.GetSandbox(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.StateDestroyed {
		t.Fatalf("expired paused sandbox not destroyed: state=%s", got.State)
	}
}
