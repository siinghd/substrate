package metrics

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// countSeries counts live label children of a metric by gathering the default
// registry — this is exactly the memory Prometheus never reclaims.
func countSeries(t *testing.T, name string) int {
	t.Helper()
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if f.GetName() == name {
			return len(f.GetMetric())
		}
	}
	return 0
}

// TestRouteLabelDomainIsBounded: the old comment claimed hostile paths could
// not grow label cardinality. That was true of path CONTENT and false of path
// DEPTH and of the method: alternating known and unknown segments at depth 20
// mints ~10^6 distinct labels, and r.Method is an arbitrary attacker-supplied
// token. Prometheus never evicts label children, so each one is permanently
// retained heap — and one metric_snapshots row per label set per minute.
func TestRouteLabelDomainIsBounded(t *testing.T) {
	ResetRouteLabelsForTest()

	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
	}))

	// (a) deep alternating paths
	for i := 0; i < 400; i++ {
		p := "/v1"
		for d := 0; d < 20; d++ {
			if (i>>(d%16))&1 == 1 {
				p += "/v1"
			} else {
				p += fmt.Sprintf("/x%d", i*31+d)
			}
		}
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", p, nil))
	}
	// (b) attacker-chosen methods
	for i := 0; i < 400; i++ {
		req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
		req.Method = fmt.Sprintf("A%d", i)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}

	n := countSeries(t, "substrate_http_requests_total")
	t.Logf("series after 800 hostile requests: %d", n)
	// With the bounds in place these 800 requests collapse onto a couple of
	// shapes; without them each request mints its own label set.
	if n > 64 {
		t.Fatalf("http_requests_total grew to %d series from 800 hostile requests; the label domain is not bounded", n)
	}
}

// TestSafeMethodAllowlist pins the method label domain.
func TestSafeMethodAllowlist(t *testing.T) {
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
		if safeMethod(m) != m {
			t.Fatalf("real method %q was collapsed", m)
		}
	}
	for _, m := range []string{"A1", "", "get", strings.Repeat("Z", 200), "MOVE"} {
		if got := safeMethod(m); got != "other" {
			t.Fatalf("method %q -> %q, want \"other\"", m, got)
		}
	}
}

// TestNormalizedRouteCapsDepth: a 40-segment path must not produce a
// 40-segment label.
func TestNormalizedRouteCapsDepth(t *testing.T) {
	ResetRouteLabelsForTest()
	p := "/v1" + strings.Repeat("/sandboxes", 40)
	got := normalizedRoute(httptest.NewRequest("GET", p, nil))
	if strings.Count(got, "/") > maxRouteSegments+1 {
		t.Fatalf("route label %q has more than %d segments", got, maxRouteSegments)
	}
	if !strings.HasSuffix(got, "{deep}") {
		t.Fatalf("truncated route %q does not mark itself truncated", got)
	}
}

// TestStatusRecorderSupportsHijack: wrapping the ResponseWriter used to hide
// http.Hijacker, so every WebSocket/Upgrade request through an instrumented
// handler failed.
func TestStatusRecorderSupportsHijack(t *testing.T) {
	var rw http.ResponseWriter = &statusRecorder{ResponseWriter: httptest.NewRecorder(), status: 200}
	if _, ok := rw.(http.Hijacker); !ok {
		t.Fatal("statusRecorder does not implement http.Hijacker: protocol upgrades cannot work through it")
	}
	if _, ok := rw.(http.Flusher); !ok {
		t.Fatal("statusRecorder does not implement http.Flusher: SSE cannot stream through it")
	}
}
