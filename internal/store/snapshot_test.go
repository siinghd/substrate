package store

import (
	"context"
	"testing"
	"time"
)

func TestMetricSnapshotsRoundtripAndPrune(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	snaps := []MetricSnapshot{
		{Name: "http_request_duration_seconds", Labels: map[string]string{"method": "GET", "route": "/v1/x", "status": "200"},
			Count: 10, Sum: 2.5, P50: 0.05, P95: 0.25, P99: 0.9, TS: now},
	}
	if err := s.InsertMetricSnapshots(ctx, snaps); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListMetricSnapshots(ctx, "http_request_duration_seconds", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %+v %v", got, err)
	}
	g := got[0]
	if g.Labels["method"] != "GET" || g.P50 != 0.05 || g.P99 != 0.9 || g.Count != 10 {
		t.Fatalf("roundtrip mismatch: %+v", g)
	}

	if _, err := s.PruneBefore(ctx, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// snapshot is fresh → nothing pruned from it; other tables may contribute 0
	left, err := s.ListMetricSnapshots(ctx, "http_request_duration_seconds", 10)
	if err != nil || len(left) != 1 {
		t.Fatalf("fresh snapshot pruned incorrectly: %+v %v", left, err)
	}
}
