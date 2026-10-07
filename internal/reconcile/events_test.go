package reconcile

import (
	"context"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

// countingEventStore records how many INSERT statements the drain issues.
type countingEventStore struct {
	store.Store
	batches int
	rows    int
}

func (c *countingEventStore) AppendEvents(ctx context.Context, evs []*store.Event) error {
	c.batches++
	c.rows += len(evs)
	return c.Store.AppendEvents(ctx, evs)
}

// TestEventsAreBatchedAndPersisted: one INSERT per event made the drain slower
// than the producer, so a lifecycle burst overflowed the ring and audit
// records were silently discarded. Persistence is now batched — but every
// event must still land, and land within the batching window.
func TestEventsAreBatchedAndPersisted(t *testing.T) {
	base := testStoreFor(t)
	ces := &countingEventStore{Store: base}
	sup := NewSupervisor(ces, &countingDriver{}, newTestLogger(), t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Events(ctx)

	const n = 300
	for i := 0; i < n; i++ {
		sup.emit(Event{SandboxID: "sb_events0000001", Type: "burst"})
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		evs, err := base.ListEvents(context.Background(), store.EventFilter{SandboxID: "sb_events0000001", Limit: 1000})
		if err == nil && len(evs) >= n {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	evs, err := base.ListEvents(context.Background(), store.EventFilter{SandboxID: "sb_events0000001", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != n {
		t.Fatalf("persisted %d of %d emitted events", len(evs), n)
	}
	// 300 events must not have cost 300 round trips.
	if ces.batches >= n {
		t.Fatalf("%d events took %d statements — persistence is not batched", n, ces.batches)
	}
	t.Logf("%d events persisted in %d statements", ces.rows, ces.batches)
}

// TestEventsFlushOnShutdown: the tail of the audit trail must survive a
// shutdown, not sit in a buffer that is dropped with the context.
func TestEventsFlushOnShutdown(t *testing.T) {
	base := testStoreFor(t)
	sup := NewSupervisor(base, &countingDriver{}, newTestLogger(), t.TempDir())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sup.Events(ctx); close(done) }()

	// Fewer than a full batch, so nothing has flushed on size.
	for i := 0; i < 5; i++ {
		sup.emit(Event{SandboxID: "sb_shutdown00001", Type: "tail"})
	}
	time.Sleep(20 * time.Millisecond) // let the drain buffer them
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Events did not return after cancellation")
	}

	evs, err := base.ListEvents(context.Background(), store.EventFilter{SandboxID: "sb_shutdown00001", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 {
		t.Fatalf("persisted %d of 5 buffered events across shutdown — the tail of the audit trail was dropped", len(evs))
	}
}

func testStoreFor(t *testing.T) store.Store {
	t.Helper()
	s, err := store.OpenSQLite(t.TempDir() + "/ev.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
