package store

import (
	"context"
	"testing"
	"time"
)

func TestPruneBeforeRemovesStaleRowsOnly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	now := time.Now()
	old := now.Add(-48 * time.Hour)

	// stale idempotency + fresh one
	s.PutIdempotency(ctx, &IdempotencyRecord{Key: "k-old", Path: "/p", ReqHash: "h", Status: 200, Body: "{}", CreatedAt: old})
	s.PutIdempotency(ctx, &IdempotencyRecord{Key: "k-new", Path: "/p", ReqHash: "h", Status: 200, Body: "{}", CreatedAt: now})

	// stale event + fresh event
	s.AppendEvent(ctx, &Event{ID: "ev_old", TS: old, Type: "created"})
	s.AppendEvent(ctx, &Event{ID: "ev_new", TS: now, Type: "created"})

	// stale usage + fresh usage
	s.InsertUsageSample(ctx, UsageSample{SandboxID: "sb_1", TS: old})
	s.InsertUsageSample(ctx, UsageSample{SandboxID: "sb_1", TS: now})

	pruned, err := s.PruneBefore(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if pruned != 3 {
		t.Fatalf("pruned %d rows, want 3", pruned)
	}

	evs, _ := s.ListEvents(ctx, EventFilter{})
	if len(evs) != 1 || evs[0].ID != "ev_new" {
		t.Fatalf("fresh events must survive: %+v", evs)
	}
	if _, err := s.GetIdempotency(ctx, "k-new", "/p"); err != nil {
		t.Fatal("fresh idempotency pruned")
	}
	if _, err := s.GetIdempotency(ctx, "k-old", "/p"); err == nil {
		t.Fatal("stale idempotency survived")
	}
}
