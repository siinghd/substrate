package process

import (
	"context"
	"io"
	"runtime"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

// TestExecDoesNotLeakAGoroutinePerRun.
//
// GuestSession.Exec starts a watcher goroutine that kills the exec when its
// context is cancelled. The only production caller passes context.Background()
// -- correctly, because an exec outlives the HTTP request that created it -- so
// that goroutine parked on a channel that would never close: one leaked
// goroutine per exec ever run, each holding the exec's stream alive.
//
// noded is a long-lived process on a node sized for thousands of sandboxes.
// O(execs ever) is unbounded growth measured in days of uptime, so this test
// runs a batch of execs to completion and asserts the goroutine count comes
// back down. It fails by a margin of ~runs against the old code.
func TestExecDoesNotLeakAGoroutinePerRun(t *testing.T) {
	m := startTestMachine(t)
	ctx := context.Background()

	// Warm up: the first exec allocates whatever the session lazily creates,
	// so the baseline is taken after steady state is reached.
	for i := 0; i < 3; i++ {
		runOneExec(t, m, ctx)
	}
	settle()
	base := runtime.NumGoroutine()

	const runs = 60
	for i := 0; i < runs; i++ {
		runOneExec(t, m, ctx)
	}
	settle()
	after := runtime.NumGoroutine()

	// Small slack for the runtime's own scheduling, far below `runs`.
	const slack = 12
	if after > base+slack {
		t.Fatalf("goroutines grew from %d to %d across %d completed execs (slack %d): "+
			"one watcher goroutine is parked per exec and never released",
			base, after, runs, slack)
	}
	t.Logf("goroutines: %d -> %d across %d execs", base, after, runs)
}

// TestExecAlwaysDeliversAnExit: every return path out of the receive loop must
// produce an ExitInfo. The stdout-write-error path used to return without one,
// leaving ExecHandle.Wait blocked forever on a channel nobody would send to --
// which on the API path is a request that never completes.
func TestExecAlwaysDeliversAnExit(t *testing.T) {
	m := startTestMachine(t)
	ctx := context.Background()

	h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"sh", "-c", "yes | head -c 4000000"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	// Abandon stdout immediately: the reader goroutine's writes into the pipe
	// start failing, which is the path that used to return exit-less.
	h.Stdout.Close()

	done := make(chan vmm.ExitInfo, 1)
	go func() {
		ei, _ := h.Wait(context.Background())
		done <- ei
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("exec never delivered an exit after its stdout consumer went away: Wait blocks forever")
	}
}

func runOneExec(t *testing.T, m vmm.Machine, ctx context.Context) {
	t.Helper()
	h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	_, _ = io.Copy(io.Discard, h.Stdout)
	if _, err := h.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
}

// settle gives finished goroutines a chance to actually exit before counting.
func settle() {
	for i := 0; i < 20; i++ {
		runtime.Gosched()
		time.Sleep(25 * time.Millisecond)
	}
	runtime.GC()
	time.Sleep(100 * time.Millisecond)
}
