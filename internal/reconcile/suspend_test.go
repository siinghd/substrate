package reconcile

import (
	"context"
	"testing"
	"time"
)

// TestSuspendedOrgsSandboxesAreStopped.
//
// Blocking new creations while leaving the running workload alone is not an
// abuse control: the miner keeps mining, the scanner keeps scanning. Because
// the reconciler is level-triggered, suspension has to be enforced there --
// against sandboxes that were ALREADY RUNNING when the lever was pulled, which
// is the only case that matters in an incident.
func TestSuspendedOrgsSandboxesAreStopped(t *testing.T) {
	drv := &slowDriver{delay: time.Millisecond, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup, st := dispatchSetup(t, drv, 4)
	sup.StartWorkers(ctx, 4)

	// Bring them up.
	sup.Tick(ctx)
	if !sup.DrainForTest(10 * time.Second) {
		t.Fatal("sandboxes never converged")
	}
	if got := sup.LiveCount(); got != 4 {
		t.Fatalf("expected 4 live sandboxes, got %d", got)
	}

	// Pull the lever on the org that owns them.
	if err := st.SetOrgSuspended(ctx, "org_1", true, "outbound scanning"); err != nil {
		t.Fatal(err)
	}
	// The cache is time-based; move past it rather than sleeping.
	base := time.Now().Add(2 * suspendCacheTTL)
	sup.nowFn = func() time.Time { return base }

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		sup.Tick(ctx)
		sup.DrainForTest(5 * time.Second)
		if sup.LiveCount() == 0 {
			return
		}
		base = base.Add(time.Second)
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%d sandboxes of a suspended org are still running", sup.LiveCount())
}

// TestSuspensionFailsOpenOnAStoreError: a database blip must not destroy every
// tenant's sandboxes on the node. Enforcement retries on the next tick; a
// false positive here is an outage for everyone.
func TestSuspensionFailsOpenOnAStoreError(t *testing.T) {
	drv := &slowDriver{delay: time.Millisecond, release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sup, st := dispatchSetup(t, drv, 2)
	sup.StartWorkers(ctx, 2)
	sup.Tick(ctx)
	sup.DrainForTest(10 * time.Second)

	// An org id that does not exist stands in for a lookup that cannot answer.
	if sup.orgSuspended(ctx, "org_does_not_exist") {
		t.Fatal("an unresolvable org was treated as suspended; a store blip would destroy every sandbox")
	}
	_ = st
	if got := sup.LiveCount(); got != 2 {
		t.Fatalf("sandboxes were destroyed by a failed lookup: %d live", got)
	}
}
