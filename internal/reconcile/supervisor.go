// Package reconcile is noded's level-triggered core (plan §5.4): the control
// plane writes desired state, the node continuously converges actual state.
// It also owns idle-pause enforcement, TTL expiry and the userspace OOM
// policy (§7.5) — pause is cheap and lossless, so pressure relief never
// destroys user state.
package reconcile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/substrate-sh/substrate/internal/metrics"

	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// Supervisor owns live machines on this node and converges them toward
// desired state. One goroutine per machine for I/O; the loop itself is O(live
// machines) per tick with no nested scans.
type Supervisor struct {
	suspMu    sync.Mutex
	suspCache map[string]suspEntry

	// network programs per-sandbox namespaces; nil means no networking.
	network Network

	mu      sync.RWMutex
	machine map[string]vmm.Machine
	// createdAt mirrors each live sandbox's creation time so victim selection
	// under memory pressure needs no query. See PressurePause.
	createdAt map[string]time.Time

	store   store.Store
	driver  vmm.Driver
	log     Logger
	dataDir string
	nowFn   func() time.Time

	events chan Event
	subsMu sync.Mutex
	subs   map[chan Event]struct{}

	tplMu    sync.RWMutex
	tplCache map[string]tplEntry

	startMu      sync.Mutex
	lastStartTry map[string]time.Time

	// ensureLocks serializes EnsureRunning PER SANDBOX ID (striped to bound
	// memory): check-then-act over s.machine raced two concurrent first
	// touches into double driver.Create — two VMMs for one sandbox, one
	// orphaned. Different IDs hash to different stripes and start in parallel.
	ensureLocks [64]sync.Mutex

	// Convergence worker pool. Tick enqueues; workers do the blocking VMM and
	// disk work. inflight dedups so a sandbox already queued or being worked
	// is never queued twice — without it a saturated pool would fill the queue
	// with duplicates of the same slow sandbox and starve every other one.
	work     chan string
	workMu   sync.Mutex
	inflight map[string]struct{}
	workers  atomic.Int32
	workerWG sync.WaitGroup

	// draining is set by Shutdown. Once set, nothing may start a new machine:
	// otherwise the tick loop and the convergence pool -- both still running on
	// the root context during the HTTP drain -- keep creating machines behind
	// the sweep, and every one of them is orphaned at exit.
	draining atomic.Bool
}

// reconcileQueueDepth bounds queued convergence work. Level-triggered
// reconciliation makes overflow safe: the next tick re-derives the same needs.
const reconcileQueueDepth = 4096

// defaultReconcileWorkers is the convergence concurrency. Each worker may hold
// a VMM create/restore, so this also bounds simultaneous restores — which is
// the thing that would otherwise thrash disk and page cache after a restart.
const defaultReconcileWorkers = 16

func (s *Supervisor) workersRunning() bool { return s.workers.Load() > 0 }

// StartWorkers brings up the convergence pool. Callers that never start it
// keep the old inline behaviour, so this is an additive interface change.
func (s *Supervisor) StartWorkers(ctx context.Context, n int) {
	if n <= 0 {
		n = defaultReconcileWorkers
	}
	for i := 0; i < n; i++ {
		s.workerWG.Add(1)
		s.workers.Add(1)
		go func() {
			defer s.workerWG.Done()
			defer s.workers.Add(-1)
			for {
				select {
				case <-ctx.Done():
					return
				case id := <-s.work:
					s.converge(ctx, id, s.nowFn())
					s.workMu.Lock()
					delete(s.inflight, id)
					s.workMu.Unlock()
					metrics.SetReconcileQueueDepth(len(s.work))
				}
			}
		}()
	}
}

// enqueue schedules convergence for one sandbox, reporting whether it was
// accepted. Already-queued ids and a full queue both return false: neither is
// an error, because the next tick re-derives the work from current state.
func (s *Supervisor) enqueue(id string) bool {
	s.workMu.Lock()
	if _, dup := s.inflight[id]; dup {
		s.workMu.Unlock()
		return false
	}
	s.inflight[id] = struct{}{}
	s.workMu.Unlock()

	select {
	case s.work <- id:
		return true
	default:
		s.workMu.Lock()
		delete(s.inflight, id)
		s.workMu.Unlock()
		return false
	}
}

// DrainForTest blocks until the queue is empty and nothing is in flight.
func (s *Supervisor) DrainForTest(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.workMu.Lock()
		n := len(s.inflight)
		s.workMu.Unlock()
		if n == 0 && len(s.work) == 0 {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func (s *Supervisor) idLock(id string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(id))
	return &s.ensureLocks[h.Sum32()%uint32(len(s.ensureLocks))]
}

const startRetryBackoff = 10 * time.Second
const maxStartTryEntries = 4096

// canTryStart throttles convergence retries so a broken driver doesn't spin.
func (s *Supervisor) canTryStart(id string, now time.Time) bool {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if len(s.lastStartTry) > maxStartTryEntries {
		for k, t := range s.lastStartTry {
			if now.Sub(t) > startRetryBackoff {
				delete(s.lastStartTry, k)
			}
		}
	}
	if t, ok := s.lastStartTry[id]; ok && now.Sub(t) < startRetryBackoff {
		return false
	}
	s.lastStartTry[id] = now
	return true
}

// tplEntry is a cached template plus its expiry. The cache used to be
// write-once with no invalidation and no TTL: once a node had booted a
// template, an updated rootfs, kernel or warm command never took effect until
// noded restarted — so a template rebuilt to patch a vulnerable image kept
// booting the vulnerable one.
type tplEntry struct {
	tpl     *store.Template
	expires time.Time
}

// tplTTL bounds staleness even when the write did not pass through this node
// (a cell-mode node never sees the API call that changed a template).
const tplTTL = 30 * time.Second

// maxTplCache bounds the map. Templates are per-project and few, but nothing
// upstream guarantees that.
const maxTplCache = 1024

func (s *Supervisor) template(ctx context.Context, id string) (*store.Template, error) {
	now := s.nowFn()
	s.tplMu.RLock()
	e, ok := s.tplCache[id]
	s.tplMu.RUnlock()
	if ok && now.Before(e.expires) {
		return e.tpl, nil
	}
	tpl, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		// Serve a stale entry rather than failing a start on a transient
		// store blip; freshness is best-effort, availability is not.
		if ok {
			return e.tpl, nil
		}
		return nil, err
	}
	s.tplMu.Lock()
	if len(s.tplCache) >= maxTplCache {
		for k, v := range s.tplCache {
			if now.After(v.expires) {
				delete(s.tplCache, k)
			}
		}
		if len(s.tplCache) >= maxTplCache {
			s.tplCache = map[string]tplEntry{}
		}
	}
	s.tplCache[id] = tplEntry{tpl: tpl, expires: now.Add(tplTTL)}
	s.tplMu.Unlock()
	return tpl, nil
}

// InvalidateTemplate drops a cached template so the next start re-reads it.
// Called by the API on write; the TTL covers writes this node never sees.
func (s *Supervisor) InvalidateTemplate(id string) {
	s.tplMu.Lock()
	delete(s.tplCache, id)
	s.tplMu.Unlock()
}

type Event struct {
	SandboxID string
	Type      string
	Data      map[string]any
	TS        time.Time
}

// Logger is the subset of logging.Logger the supervisor needs.
type Logger interface {
	Debug(msg string, kv ...any)
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
	Error(msg string, kv ...any)
}

func NewSupervisor(st store.Store, drv vmm.Driver, log Logger, dataDir string) *Supervisor {
	return &Supervisor{
		machine:      map[string]vmm.Machine{},
		createdAt:    map[string]time.Time{},
		store:        st,
		driver:       drv,
		log:          log,
		dataDir:      dataDir,
		nowFn:        time.Now,
		events:       make(chan Event, 1024),
		subs:         map[chan Event]struct{}{},
		tplCache:     map[string]tplEntry{},
		work:         make(chan string, reconcileQueueDepth),
		inflight:     map[string]struct{}{},
		lastStartTry: map[string]time.Time{},
	}
}

func (s *Supervisor) Subscribe() <-chan Event {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	ch := make(chan Event, 256)
	s.subs[ch] = struct{}{}
	return ch
}

func (s *Supervisor) emit(ev Event) {
	if ev.TS.IsZero() {
		ev.TS = s.nowFn()
	}
	select {
	case s.events <- ev:
	default:
		// The ring is still non-blocking — emit is called while holding
		// per-sandbox locks, so blocking here would stall convergence. But a
		// dropped event is a lost AUDIT record, so it is counted, not just
		// logged: a Warn nobody greps is indistinguishable from never having
		// happened. The drain batches its inserts, which is what keeps the
		// consumer ahead of the producer in the first place.
		metrics.IncEventsDropped()
		s.log.Warn("event ring full; dropping", "sandbox", ev.SandboxID, "type", ev.Type)
	}
}

// eventBatchMax / eventBatchWait bound the drain's batching. Small enough that
// an idle node still persists an event within a tick, large enough that a
// lifecycle burst costs one round trip instead of dozens.
const eventBatchMax = 128
const eventBatchWait = 250 * time.Millisecond

// Events drains emitted events into persistence + subscribers. Run once.
//
// Events are persisted in BATCHES. One INSERT per event made the drain slower
// than the producer during a lifecycle burst, so the ring filled and emit
// discarded audit records — while usage samples on the very same store were
// already batched. Subscribers are still fed one event at a time, immediately,
// because SSE consumers want latency, not throughput.
func (s *Supervisor) Events(ctx context.Context) {
	batch := make([]*store.Event, 0, eventBatchMax)
	timer := time.NewTimer(eventBatchWait)
	defer timer.Stop()
	if !timer.Stop() {
		<-timer.C
	}
	timerArmed := false

	flush := func(ctx context.Context) {
		if len(batch) == 0 {
			return
		}
		if err := s.store.AppendEvents(ctx, batch); err != nil {
			s.log.Error("event batch persist failed", "events", len(batch), "err", err.Error())
			metrics.AddEventsDropped(len(batch))
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Persist what is already buffered under a fresh context: the run
			// context is dead, and flushing with it would fail every insert
			// and lose the tail of the audit trail on every restart.
			flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			flush(flushCtx)
			cancel()
			return
		case ev := <-s.events:
			data, _ := jsonMarshal(ev.Data)
			batch = append(batch, &store.Event{
				ID: eventID(), TS: ev.TS, SandboxID: ev.SandboxID, Type: ev.Type, Data: data,
			})
			// Fan out immediately; only persistence is batched.
			s.subsMu.Lock()
			for ch := range s.subs {
				select {
				case ch <- ev:
				default:
				}
			}
			s.subsMu.Unlock()

			if len(batch) >= eventBatchMax {
				flush(ctx)
				if timerArmed && !timer.Stop() {
					<-timer.C
				}
				timerArmed = false
				continue
			}
			if !timerArmed {
				timer.Reset(eventBatchWait)
				timerArmed = true
			}
		case <-timer.C:
			timerArmed = false
			flush(ctx)
		}
	}
}

func (s *Supervisor) Tier() string { return s.driver.Caps().Tier }

func (s *Supervisor) Get(id string) (vmm.Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.machine[id]
	return m, ok
}

// VMMpid exposes the host-side VMM/shim PID of a live sandbox for the usage
// sampler. Drivers that don't track a process (jailed firecracker) simply
// don't implement the capability and report not-ok.
func (s *Supervisor) VMMpid(id string) (int, bool) {
	m, ok := s.Get(id)
	if !ok {
		return 0, false
	}
	pr, hasPID := m.(interface{ VMMpid() (int, bool) })
	if !hasPID {
		return 0, false
	}
	return pr.VMMpid()
}

func (s *Supervisor) LiveCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.machine)
}

// LiveIDs snapshots the live sandbox ids for samplers.
func (s *Supervisor) LiveSandboxes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.machine))
	for id := range s.machine {
		out = append(out, id)
	}
	return out
}

// EnsureRunning starts (or restores) a sandbox machine if not already live.
// Network programs and reclaims a sandbox's networking. Optional: a supervisor
// with none runs sandboxes with no network, which is what every deployment did
// before this was wired -- EnsureRunning has always taken a *vmm.NetInfo, and
// every caller in the tree passed nil.
type Network interface {
	Setup(sandboxID string) (*vmm.NetInfo, error)
	Teardown(sandboxID string) error
}

// SetNetwork installs the network provider. Called once at wiring time.
func (s *Supervisor) SetNetwork(n Network) { s.network = n }

func (s *Supervisor) EnsureRunning(ctx context.Context, sb *store.Sandbox, tpl *store.Template, netInfo *vmm.NetInfo) error {
	lock := s.idLock(sb.ID)
	lock.Lock()
	defer lock.Unlock()
	// Re-check under the lock: a concurrent starter may have won while we
	// waited — starting twice for one sandbox is the bug this prevents.
	if _, ok := s.Get(sb.ID); ok {
		return nil
	}
	// Refuse to start anything once shutdown has begun, or the drain can never
	// converge and the machine is left running with nobody to stop it.
	if s.draining.Load() {
		return fmt.Errorf("node is shutting down")
	}
	// Program networking here rather than trusting callers: the parameter has
	// existed all along and every call site passed nil, so sandboxes shipped
	// with no network at all.
	if netInfo == nil && s.network != nil {
		ni, nerr := s.network.Setup(sb.ID)
		if nerr != nil {
			return fmt.Errorf("program sandbox network: %w", nerr)
		}
		netInfo = ni
	}
	spec := vmm.Spec{
		ID: sb.ID,
		Template: vmm.TemplateSpec{
			Name:       tpl.Name,
			RootfsPath: tpl.RootfsPath,
			KernelPath: tpl.KernelPath,
			MemMiB:     sb.MemMiB,
			VCPUs:      sb.VCPUs,
			WarmCmd:    tpl.WarmCmd,
		},
		Net:     netInfo,
		WorkDir: s.workdir(sb),
	}
	start := s.nowFn()
	var m vmm.Machine
	var err error
	if sb.PauseRef != "" && s.driver.Caps().Pause {
		mem, snap := splitPauseRef(sb.PauseRef)
		m, err = s.driver.Restore(ctx, spec, mem, snap)
	} else {
		m, err = s.driver.Create(ctx, spec)
	}
	if err != nil {
		// Hand the namespace and its addresses back. Without this a node that
		// fails to start sandboxes leaks a netns and a /30 per attempt, and
		// the reconciler retries forever.
		if s.network != nil {
			if terr := s.network.Teardown(sb.ID); terr != nil {
				s.log.Warn("network teardown after failed start", "sandbox", sb.ID, "err", terr.Error())
			}
		}
		return fmt.Errorf("start machine: %w", err)
	}

	s.mu.Lock()
	s.machine[sb.ID] = m
	s.createdAt[sb.ID] = sb.CreatedAt
	s.mu.Unlock()

	go s.watch(sb.ID, m)

	latMS := s.nowFn().Sub(start).Milliseconds()
	now := s.nowFn()
	sb.State = store.StateRunning
	sb.StartedAt = &now
	sb.Error = ""
	if err := s.store.UpdateSandbox(ctx, sb); err != nil {
		s.log.Warn("state persist failed", "sandbox", sb.ID, "err", err.Error())
	}
	metrics.ObserveRestore(float64(latMS), sb.PauseRef != "")
	s.emit(Event{SandboxID: sb.ID, Type: "running", Data: map[string]any{
		"restore_ms": latMS, "restored": sb.PauseRef != "",
	}})
	s.log.Info("sandbox running", "sandbox", sb.ID, "latency_ms", latMS)
	return nil
}

func (s *Supervisor) workdir(sb *store.Sandbox) string {
	return s.dataDir + "/sandboxes/" + sb.ID
}

func splitPauseRef(ref string) (mem, snap string) {
	for i := 0; i < len(ref); i++ {
		if ref[i] == '|' {
			return ref[:i], ref[i+1:]
		}
	}
	return ref, ""
}

// watch reconciles one machine's unexpected death.
func (s *Supervisor) watch(id string, m vmm.Machine) {
	<-m.Done()
	s.mu.Lock()
	current, ok := s.machine[id]
	if ok && current == m {
		delete(s.machine, id)
		delete(s.createdAt, id)
	}
	s.mu.Unlock()
	s.emit(Event{SandboxID: id, Type: "machine_exited"})
}

// Pause snapshots a live sandbox and frees its RAM (plan §3.6).
//
// Takes the per-sandbox lock for the same reason EnsureRunning does. Without
// it an idle-pause from Tick could interleave with a resume from an API
// request: pause kills the VMM and clears the map entry, the resume sees
// not-live and starts a SECOND machine writing state=running, and then pause
// finishes by writing state=paused plus a PauseRef read before any of that.
// The row then claims paused while a machine is live, and the stale PauseRef
// gets restored over it later.
func (s *Supervisor) Pause(ctx context.Context, id string) error {
	lock := s.idLock(id)
	lock.Lock()
	defer lock.Unlock()
	m, ok := s.Get(id)
	if !ok {
		return vmm.ErrMachineGone
	}
	if !m.Caps().Pause {
		return vmm.ErrUnsupported
	}
	dest := s.workdir(&store.Sandbox{ID: id}) + "/pause"
	ref, err := m.Pause(ctx, dest)
	if err != nil {
		return err
	}
	sb, err := s.store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	sb.PauseRef = ref
	sb.State = store.StatePaused
	now := s.nowFn()
	sb.PausedAt = &now
	if err := s.store.UpdateSandbox(ctx, sb); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.machine, id)
	delete(s.createdAt, id)
	s.mu.Unlock()
	s.emit(Event{SandboxID: id, Type: "paused"})
	return nil
}

// Resume brings a paused sandbox back; implicit on any API touch (§3.6).
// Resume delegates to EnsureRunning, which takes the per-sandbox lock. Do not
// lock here as well: sync.Mutex is not reentrant and this would self-deadlock.
func (s *Supervisor) Resume(ctx context.Context, sb *store.Sandbox, tpl *store.Template, netInfo *vmm.NetInfo) error {
	if _, ok := s.Get(sb.ID); ok {
		return nil
	}
	return s.EnsureRunning(ctx, sb, tpl, netInfo)
}

// Destroy stops and removes everything for a sandbox: machine, state, and
// on-disk scratch (workdir + pause images). Snapshots of a destroyed lineage
// are gone by definition (plan §5.5 GC semantics).
//
// Holds the per-sandbox lock: racing EnsureRunning, destroy killed the machine
// and then a start already in flight registered a fresh one — leaving a live
// VMM with no owning row, on the network, invisible to the reconciler.
func (s *Supervisor) Destroy(ctx context.Context, id string) error {
	lock := s.idLock(id)
	lock.Lock()
	defer lock.Unlock()
	if m, ok := s.Get(id); ok {
		_ = m.Kill()
		s.mu.Lock()
		delete(s.machine, id)
		delete(s.createdAt, id)
		s.mu.Unlock()
	}
	sb, err := s.store.GetSandbox(ctx, id)
	if err != nil {
		return err
	}
	// Remove on-disk scratch AFTER the machine is down so nothing re-creates it.
	os.RemoveAll(s.workdir(&store.Sandbox{ID: id}))
	// Driver-private state too. Removing only our own workdir left the whole
	// Firecracker jail tree behind on every destroy — a per-sandbox rootfs copy
	// plus a full-RAM memfile, so a node churning sandboxes fills its disk with
	// the remains of sandboxes that no longer exist.
	if c, ok := s.driver.(vmm.Cleaner); ok {
		if cerr := c.Cleanup(ctx, id); cerr != nil {
			s.log.Warn("driver cleanup failed; on-disk state may leak", "sandbox", id, "err", cerr.Error())
		}
	}
	// The namespace, its veth pair, its firewall chain and its /30 all go
	// back. A leak here is not just disk: the address block is finite, so a
	// node that never released would eventually refuse to start anything.
	if s.network != nil {
		if nerr := s.network.Teardown(id); nerr != nil {
			s.log.Warn("network teardown failed; namespace may leak", "sandbox", id, "err", nerr.Error())
		}
	}
	sb.State = store.StateDestroyed
	sb.Desired = "destroyed"
	return s.store.UpdateSandbox(ctx, sb)
}

// Tick is the periodic convergence pass: desired vs actual, idle pauses, TTLs.
//
// Tick DISPATCHES; it does not converge. The previous version called
// EnsureRunning, Pause and Destroy inline, so one tick cost the SUM of every
// VMM operation it decided to do. After a node restart with 2,000 sandboxes
// desired-running that is 2,000 x ~300ms ≈ 10 minutes of strictly serial
// work — during which no TTL expired, no destroy ran and no idle pause
// happened, because they all share this one loop. One slow snapshot or one
// hung os.RemoveAll stalled the entire node's reconciliation.
//
// Now the loop does arithmetic only — O(rows) with no blocking I/O — and hands
// each sandbox needing attention to a bounded worker pool. Reconciliation is
// level-triggered, so the queue is deliberately LOSSY: if it is full, dropping
// an enqueue costs nothing, because the next tick re-derives the same need
// from the same rows. That is what makes a bounded queue safe here.
//
// With no workers started (tests, embedders) Tick converges inline exactly as
// before, so the dispatch split is opt-in.
func (s *Supervisor) Tick(ctx context.Context) {
	sbs, err := s.store.SandboxesForReconcile(ctx)
	if err != nil {
		s.log.Error("reconcile list failed", "err", err)
		return
	}
	if s.draining.Load() {
		return
	}
	now := s.nowFn()
	async := s.workersRunning()
	queued, dropped := 0, 0
	for _, sb := range sbs {
		if !s.needsConverge(sb, now) {
			continue
		}
		if !async {
			s.converge(ctx, sb.ID, now)
			continue
		}
		if s.enqueue(sb.ID) {
			queued++
		} else {
			dropped++
		}
	}
	metrics.SetReconcileQueueDepth(len(s.work))
	if dropped > 0 {
		metrics.AddReconcileDropped(dropped)
		s.log.Warn("reconcile queue saturated; deferring to next tick",
			"queued", queued, "deferred", dropped)
	}
}

// needsConverge is a pure predicate over one row plus liveness: no I/O, so the
// dispatch loop stays cheap at 100k defined sandboxes. It only has to be
// conservative in one direction — a false positive costs one no-op job.
func (s *Supervisor) needsConverge(sb store.ReconcileRow, now time.Time) bool {
	if sb.Desired == "destroyed" {
		return true
	}
	// A suspended org's sandboxes always need attention, however healthy they
	// look. Without this a HEALTHY running sandbox is never enqueued (see the
	// live branch below), so the suspension lever would block new creations
	// while the abusive workload kept running.
	if sb.OrgSuspended {
		return true
	}
	if sb.TTLAbsoluteSecs > 0 && now.Sub(sb.CreatedAt) > time.Duration(sb.TTLAbsoluteSecs)*time.Second {
		return true
	}
	_, live := s.Get(sb.ID)
	if sb.Desired == "paused" {
		return live
	}
	if !live {
		return sb.State != store.StatePaused
	}
	return sb.TTLIdleSecs > 0
}

// converge applies the desired state for ONE sandbox. It re-reads the row
// rather than trusting the dispatch snapshot: by the time a worker picks the
// job up, a pause or destroy committed by another path may have moved it, and
// acting on the stale copy would undo that.
func (s *Supervisor) converge(ctx context.Context, id string, now time.Time) {
	sb, err := s.store.GetSandbox(ctx, id)
	if err != nil {
		return
	}
	if sb.Desired == "destroyed" {
		_ = s.Destroy(ctx, sb.ID)
		return
	}
	// A SUSPENDED ORG'S SANDBOXES ARE STOPPED, not merely blocked from being
	// created. Suspension exists for abuse -- mining, outbound scanning, spam
	// -- and a control that leaves the running workload alone stops nothing.
	// Checked here, in the level-triggered loop, so it applies to sandboxes
	// that were already running when the lever was pulled.
	if s.orgSuspended(ctx, sb.OrgID) {
		s.emit(Event{SandboxID: sb.ID, Type: "org_suspended"})
		s.log.Warn("stopping sandbox: org is suspended", "sandbox", sb.ID, "org", sb.OrgID)
		_ = s.Destroy(ctx, sb.ID)
		return
	}
	// Absolute TTL is enforced regardless of residency: a paused sandbox must
	// still be destroyed at its deadline, otherwise idle-paused sandboxes pin
	// their disk forever and TTL limits are evadable by pausing first.
	if sb.TTLAbsoluteSecs > 0 && now.Sub(sb.CreatedAt) > time.Duration(sb.TTLAbsoluteSecs)*time.Second {
		s.emit(Event{SandboxID: sb.ID, Type: "ttl_expired"})
		_ = s.Destroy(ctx, sb.ID)
		return
	}
	if sb.Desired == "paused" {
		if _, live := s.Get(sb.ID); live {
			_ = s.Pause(ctx, sb.ID)
		}
		return
	}

	_, live := s.Get(sb.ID)
	if !live && sb.State != store.StatePaused {
		if !s.canTryStart(sb.ID, now) {
			return
		}
		tpl, terr := s.template(ctx, sb.TemplateID)
		if terr != nil {
			s.log.Warn("template missing for sandbox", "sandbox", sb.ID, "template", sb.TemplateID)
			return
		}
		if err := s.EnsureRunning(ctx, sb, tpl, nil); err != nil {
			s.log.Warn("converge start failed", "sandbox", sb.ID, "err", err)
		}
		return
	}
	if live {
		s.enforceTTLs(ctx, sb, now)
	}
}

// orgSuspended reports whether an org is suspended, with a short cache.
//
// converge runs per sandbox per tick, so an uncached lookup would be one extra
// query per resident sandbox per 2s -- the same N+1-on-a-timer shape flagged
// elsewhere in this codebase. A few seconds of staleness is acceptable for a
// control measured in minutes; a per-tick query at 2,000 sandboxes is not.
func (s *Supervisor) orgSuspended(ctx context.Context, orgID string) bool {
	if orgID == "" {
		return false
	}
	now := s.nowFn()
	s.suspMu.Lock()
	if e, ok := s.suspCache[orgID]; ok && now.Before(e.until) {
		s.suspMu.Unlock()
		return e.suspended
	}
	s.suspMu.Unlock()

	org, err := s.store.GetOrg(ctx, orgID)
	if err != nil {
		// Fail OPEN on a lookup error: a database blip must not destroy every
		// tenant's sandboxes on the node. Suspension is enforced again on the
		// next tick.
		return false
	}
	susp := org.SuspendedAt != nil
	s.suspMu.Lock()
	if s.suspCache == nil {
		s.suspCache = map[string]suspEntry{}
	}
	s.suspCache[orgID] = suspEntry{suspended: susp, until: now.Add(suspendCacheTTL)}
	s.suspMu.Unlock()
	return susp
}

const suspendCacheTTL = 15 * time.Second

type suspEntry struct {
	suspended bool
	until     time.Time
}

func (s *Supervisor) enforceTTLs(ctx context.Context, sb *store.Sandbox, now time.Time) {
	lastActivity := time.UnixMilli(sb.LastActivityMS)
	if sb.LastActivityMS == 0 {
		lastActivity = sb.CreatedAt
	}
	if sb.TTLIdleSecs > 0 && now.Sub(lastActivity) > time.Duration(sb.TTLIdleSecs)*time.Second {
		if err := s.Pause(ctx, sb.ID); err != nil && err != vmm.ErrUnsupported {
			s.log.Debug("idle pause skipped", "sandbox", sb.ID, "err", err)
		} else if err == nil {
			s.log.Info("idle pause", "sandbox", sb.ID, "idle_for", now.Sub(lastActivity).String())
		}
	}
	// Absolute TTL for LIVE sandboxes is enforced in Tick before this point
	// (it must also cover paused sandboxes); nothing left to do here.
}

// Shutdown stops every machine this supervisor owns, so the agent does not
// leave them behind.
//
// noded's signal handler drained HTTP and cancelled the root context, and
// nothing ever touched s.machine. The VMMs genuinely outlive the agent — the
// jailer daemonizes Firecracker, and the process driver sets Setpgid with no
// Pdeathsig — so a routine SIGTERM on a host with 800 live sandboxes left 800
// running VMs holding their RAM, vsock CIDs and taps. The replacement noded
// starts with an empty machine map while the rows still say "running", so
// within one tick it starts a SECOND VMM per sandbox, re-staging the template
// over the same rootfs image the first VM still has open as a block device:
// two VMs writing one ext4 image.
//
// hv-tier machines are PAUSED where the driver supports it, so their state is
// preserved for the next agent; everything else is killed. Machines are stopped
// concurrently and bounded by ctx, because 800 sequential shutdowns would
// exceed any drain deadline.
func (s *Supervisor) Shutdown(ctx context.Context) {
	// STOP ACCEPTING NEW WORK FIRST. The convergence pool and the 2s tick loop
	// run on the root context, which is still live during the HTTP drain, so a
	// single snapshot of the live set was a lost race: any machine started by
	// a draining worker or an API resumeIfNeeded AFTER the snapshot was never
	// visited, and was orphaned when the process exited. Pausing 800 machines
	// takes far longer than a tick, so this was not a narrow window.
	s.draining.Store(true)

	s.log.Info("stopping live machines before exit")
	// Re-read the live set each round: a machine that appeared between rounds
	// must still be stopped. Draining is set, so the set can only shrink, which
	// is what makes this terminate.
	for round := 0; ; round++ {
		s.mu.RLock()
		ids := make([]string, 0, len(s.machine))
		for id := range s.machine {
			ids = append(ids, id)
		}
		s.mu.RUnlock()
		if len(ids) == 0 {
			return
		}
		if err := ctx.Err(); err != nil {
			s.log.Warn("shutdown deadline reached with machines still live",
				"remaining", len(ids), "rounds", round)
			return
		}
		s.stopBatch(ctx, ids)
	}
}

// stopBatch stops one snapshot of machines, bounded by ctx.
func (s *Supervisor) stopBatch(ctx context.Context, ids []string) {

	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, id := range ids {
		select {
		case <-ctx.Done():
			s.log.Warn("shutdown deadline reached; remaining machines left running", "stopped_before", id)
			wg.Wait()
			return
		default:
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := s.Pause(ctx, id); err == nil {
				return
			}
			// No pause support (or it failed): kill, so nothing is orphaned.
			if m, ok := s.Get(id); ok {
				_ = m.Kill()
				s.mu.Lock()
				delete(s.machine, id)
				delete(s.createdAt, id)
				s.mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
}

// PressurePause pauses the newest live sandbox under memory pressure — the
// userspace OOM authority preferring loss over destruction (§7.5).
//
// Victim selection reads the in-memory live set, not the store. It used to run
// ListSandboxes(state=running) with no LIMIT, materialising every running row
// at the exact moment the host was out of memory: the OOM-relief path was
// itself a large allocation, and could fail (or deepen the pressure) precisely
// when it was needed. O(live) arithmetic, zero I/O, zero allocation.
func (s *Supervisor) PressurePause(ctx context.Context) bool {
	s.mu.RLock()
	var victim string
	var newest time.Time
	for id := range s.machine {
		c := s.createdAt[id]
		if victim == "" || c.After(newest) {
			victim, newest = id, c
		}
	}
	s.mu.RUnlock()
	if victim == "" {
		return false
	}
	return s.Pause(ctx, victim) == nil
}

func eventID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return "ev_" + hex.EncodeToString(b)
}

func jsonMarshal(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}
