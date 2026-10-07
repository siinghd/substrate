package gvisor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// shimInGuest is where the agent binary lives inside the sandbox rootfs, and
// agentSock is its listening socket inside the bind-mounted directory.
const (
	shimInGuest  = "substrate-shim"
	sockDirGuest = "/sock"
	agentSock    = "agent.sock"
	agentToken   = "agent.token"
)

type machine struct {
	id     string
	cid    string
	spec   vmm.Spec
	drv    *Driver
	bundle string

	// ln stays open for the machine's whole life so the agent can reconnect
	// after a transport error; token authenticates it when it does.
	ln    net.Listener
	token string

	// sess is REPLACED on reconnect, so every use goes through session().
	sessMu sync.RWMutex
	sess   *vmm.GuestSession

	// sentryPID is read once from runsc's --pid-file at start. See VMMpid.
	sentryPID int

	mu       sync.Mutex
	killed   bool
	done     chan struct{}
	doneOnce sync.Once
}

var _ vmm.Machine = (*machine)(nil)

func (m *machine) ID() string { return m.id }

// session returns the live guest session. It is swapped when the agent
// reconnects, so nothing may hold on to the pointer across a call.
func (m *machine) session() *vmm.GuestSession {
	m.sessMu.RLock()
	defer m.sessMu.RUnlock()
	return m.sess
}

func (m *machine) setSession(sess *vmm.GuestSession) {
	m.sessMu.Lock()
	m.sess = sess
	m.sessMu.Unlock()
}

func (m *machine) Caps() vmm.Caps {
	return vmm.Caps{Tier: "kernel", Pause: false, Snapshot: false}
}

func (m *machine) Exec(ctx context.Context, spec vmm.ExecSpec) (*vmm.ExecHandle, error) {
	return m.session().Exec(ctx, spec)
}

func (m *machine) Fs() *vmm.FsClient     { return m.session().Fs() }
func (m *machine) Ports() []vmm.PortInfo { p, _ := m.session().WaitPorts(0); return p }

func (m *machine) Reseed(ctx context.Context, vmgenid string) (vmm.ReseedAck, error) {
	return m.session().Reseed(vmgenid, 10*time.Second)
}

func (m *machine) Metrics(ctx context.Context) (*vmm.GuestMetrics, error) {
	gm, ok := m.session().LatestMetrics()
	if !ok {
		return nil, fmt.Errorf("no guest metrics yet")
	}
	return gm, nil
}

// Pause is unsupported on this tier for now. runsc has checkpoint/restore, so
// this is future work rather than a wall -- but reporting a capability the
// reconciler would then call is worse than reporting the truth, because
// idle-pause would fail on every sandbox on the node. Caps().Pause is false, so
// the supervisor never gets here.
func (m *machine) Pause(ctx context.Context, destDir string) (string, error) {
	return "", vmm.ErrUnsupported
}

// Shutdown asks the guest to stop, then kills. The sandbox is a real kernel
// boundary, so a graceful stop is worth attempting.
func (m *machine) Shutdown(ctx context.Context) error {
	killCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, _ = m.drv.runsc(killCtx, "kill", "-all", m.cid, "TERM")
	select {
	case <-m.done:
		return nil
	case <-time.After(3 * time.Second):
	}
	return m.Kill()
}

func (m *machine) Kill() error {
	m.mu.Lock()
	if m.killed {
		m.mu.Unlock()
		return nil
	}
	m.killed = true
	m.mu.Unlock()

	if m.ln != nil {
		m.ln.Close() // stop accepting reconnects; this teardown is final
	}
	m.session().Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// -all: kill every process in the sandbox, not just its init.
	//
	// "already gone" is the COMMON case, not an error: the agent is the
	// sandbox's init and it exits when we close the session above, which stops
	// the container before this runs. Logging that as a failure would train
	// operators to ignore this line, and then a real kill failure would go
	// unnoticed.
	metrics.IncSandboxStop(tierName, "killed")
	if _, err := m.drv.runsc(ctx, "kill", "-all", m.cid, "KILL"); err != nil && !alreadyGone(err) {
		metrics.IncDriverError(tierName, "kill")
		m.drv.cfg.Log("gvisor kill failed", "sandbox", m.id, "err", err.Error())
	}
	if _, err := m.drv.runsc(ctx, "delete", "-force", m.cid); err != nil && !alreadyGone(err) {
		m.drv.cfg.Log("gvisor delete failed", "sandbox", m.id, "err", err.Error())
	}
	// The tracker started by Driver.track removes the registry entry when Done
	// closes, so every death path is covered, not just this one.
	m.markDone()
	return nil
}

// alreadyGone reports whether a runsc error just means the sandbox has already
// stopped or been removed.
func alreadyGone(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not running") ||
		strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "no such container")
}

func (m *machine) markDone() { m.doneOnce.Do(func() { close(m.done) }) }

func (m *machine) Done() <-chan struct{} { return m.done }

func (m *machine) WorkDir() string { return m.bundle }

// supervise closes Done when the sandbox stops for any reason.
//
// Without it a sandbox that died on its own -- OOM inside the guest, the
// sentry crashing, an operator running `runsc delete` -- would stay "live" in
// the supervisor's map forever and the reconciler would never restart it. Same
// defect the Firecracker driver had before PASS-18.
func (m *machine) supervise() {
	// EVENT, NOT POLL.
	//
	// This used to fork+exec `runsc state` on a timer, per machine.
	// At the stated target of 2,000 resident sandboxes that is 1,000 fork+execs
	// of a ~119MB binary PER SECOND, forever, before any sandbox does work --
	// plus a timer and a goroutine each, and per-container state-dir locking
	// that contends with real Kill/Cleanup during churn.
	//
	// It is also unnecessary: waiting on the session costs one blocked
	// goroutine and zero syscalls.
	//
	// A DROPPED SESSION IS NO LONGER A DEAD SANDBOX. It used to be -- the agent
	// served exactly one connection and exited, taking the sandbox and
	// everything written into it with it, so a mux desync or an unclean noded
	// exit was unrecoverable data loss. The agent now reconnects, so a session
	// ending means "wait for it to come back", and only a sandbox that never
	// comes back is dead.
	for {
		select {
		case <-m.done:
			return // Kill() owns the teardown
		case <-m.session().Done():
		}

		m.mu.Lock()
		killed := m.killed
		m.mu.Unlock()
		if killed {
			return
		}

		sess, err := m.reaccept()
		if err == nil {
			m.setSession(sess)
			m.drv.cfg.Log("guest agent reconnected", "sandbox", m.id)
			metrics.IncAgentReconnect(tierName)
			continue
		}
		m.drv.cfg.Log("guest agent did not come back", "sandbox", m.id, "err", err.Error())
		break
	}
	m.drv.cfg.Log("gvisor sandbox exited", "sandbox", m.id)
	metrics.IncSandboxStop(tierName, "exited")

	// Reclaim the container record. Leaving it behind made the sandbox
	// permanently unstartable: `runsc run` refuses an id that already exists,
	// so every reconcile retry failed and its failure path deleted the bundle.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := m.drv.runsc(ctx, "delete", "-force", m.cid); err != nil && !alreadyGone(err) {
		m.drv.cfg.Log("could not reclaim stopped container", "sandbox", m.id, "err", err.Error())
	}
	m.markDone()
}

// reaccept waits for the agent to dial back in after its session dropped, and
// proves it is still OUR agent before handing it the sandbox.
//
// The window is bounded: a live sandbox's agent reconnects immediately (its
// dial loop is already running), so anything longer than this means the sandbox
// is gone and the machine should be torn down. Waiting forever would pin a dead
// sandbox's registry entry, its listener and its goroutine.
func (m *machine) reaccept() (*vmm.GuestSession, error) {
	if m.ln == nil {
		return nil, fmt.Errorf("no listener")
	}
	deadline := time.Now().Add(reacceptWindow)
	for time.Now().Before(deadline) {
		// IS THE SANDBOX EVEN ALIVE? Ask before waiting.
		//
		// Without this the window became the death-detection latency: a
		// sandbox that genuinely stopped stayed "live" in the registry for the
		// whole reconnect window, so Done() fired ~45s late, the reconciler
		// could not converge, and an operator saw a hung sandbox. Caught by
		// three existing tests timing out at 30s -- the reconnect fix had
		// quietly traded one defect for another.
		//
		// This forks `runsc state`, which an earlier pass removed from the
		// per-tick path for good reason. It is fine HERE: this runs once per
		// session-drop event, not once per sandbox per tick, so the cost is
		// O(1) per drop rather than O(live sandboxes) per second.
		if !m.containerRunning() {
			return nil, fmt.Errorf("sandbox is gone")
		}
		// Short accept slices, so a sandbox that dies WHILE we wait is noticed
		// within one slice rather than at the end of the window.
		wait := reacceptPoll
		if left := time.Until(deadline); left < wait {
			wait = left
		}
		sess, err := acceptSession(m.ln, wait)
		if err != nil {
			if isTimeout(err) {
				continue // nobody yet; loop re-checks liveness
			}
			return nil, err
		}
		hello, aerr := authenticate(sess, m.token, 10*time.Second)
		if aerr == nil {
			_ = hello
			return sess, nil
		}
		// NOT fatal: the socket is inside the sandbox, so a rejected
		// connection is most likely guest code trying to impersonate the
		// agent. Drop it and keep waiting for the real one -- letting a
		// forged connection end the wait would turn impersonation into a
		// reliable way to kill your own sandbox on someone else's schedule.
		sess.Close()
		metrics.IncDriverError(tierName, "agent_auth_failed")
		m.drv.cfg.Log("rejected an unauthenticated agent connection", "sandbox", m.id)
	}
	return nil, fmt.Errorf("agent did not reconnect within %s", reacceptWindow)
}

// reacceptWindow must be comfortably shorter than the agent's own give-up
// window, so the host is still listening while the agent is still dialling.
// reacceptPoll bounds how long a DEAD sandbox can look alive.
const (
	reacceptWindow = 45 * time.Second
	reacceptPoll   = 2 * time.Second
)

// containerRunning reports whether the sandbox still exists as far as runsc is
// concerned.
func (m *machine) containerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	st, err := m.drv.state(ctx, m.cid)
	if err != nil {
		return false
	}
	return st.Pid > 0 && !strings.EqualFold(st.Status, "stopped")
}

// isTimeout reports whether an accept failed because nothing arrived in time,
// as opposed to the listener being closed or broken.
func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// VMMpid reports the sentry pid for usage accounting, mirroring the process
// tier's hook so the metrics sampler works unchanged on this tier.
//
// FIELD READ, NOT A SUBPROCESS.
//
// This used to fork+exec `runsc state` -- a ~119MB binary -- on every call, and
// the caller is the usage sampler, which calls it once per live sandbox per
// tick WHILE HOLDING the mutex that also guards the billing flush. At the
// stated target of 2,000 resident sandboxes that is 2,000 serialized
// fork+execs per tick, each with a 3s timeout: a single unhealthy sandbox
// could stall the tick for 3s, and a broken runsc could stall it for 100
// minutes -- during which not one billing sample is written.
//
// A container's sentry pid is fixed for the container's whole life (a restart
// is a fresh Create with a fresh machine), so there was never anything to poll.
// runsc writes it to --pid-file at start and it is read once, there.
func (m *machine) VMMpid() (int, bool) {
	if m.sentryPID <= 0 {
		return 0, false
	}
	// A dead sandbox's pid may already have been recycled by the host; a
	// sample attributed to whatever now owns that pid is worse than no sample.
	select {
	case <-m.done:
		return 0, false
	default:
	}
	return m.sentryPID, true
}

var _ = os.Getenv
