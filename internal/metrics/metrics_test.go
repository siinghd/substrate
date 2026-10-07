package metrics

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/store"
)

func httpMiddlewareTestTarget() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		w.Write([]byte(`{"ok":true}`))
	})
}

type capRecorder struct {
	http.ResponseWriter
	status int
}

func (r *capRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func httptestResponseRecorder() *capRecorder {
	return &capRecorder{ResponseWriter: httptest.NewRecorder(), status: 200}
}

func httptestRequest(method, target string, body io.Reader) *http.Request {
	return httptest.NewRequest(method, target, body)
}

func TestHTTPMiddlewareRecordsLatencyAndStatus(t *testing.T) {
	handler := httpMiddlewareTestTarget()
	rec := httptestResponseRecorder()
	req := httptestRequest("GET", "/v1/sandboxes/sb_0123456789012", nil)
	HTTPMiddleware(handler).ServeHTTP(rec, req)
	if rec.status != 201 {
		t.Fatalf("status passthrough broken: %d", rec.status)
	}
}

func TestNormalizedRouteCollapsesIDs(t *testing.T) {
	cases := map[string]string{
		"/v1/sandboxes":                     "/v1/sandboxes",
		"/v1/sandboxes/sb_0123456789012":    "/v1/sandboxes/{var}",
		"/v1/execs/ex_0123456789012/stream": "/v1/execs/{var}/stream",
		"/healthz":                          "/healthz",
	}
	for in, want := range cases {
		r := httptestRequest("GET", in, nil)
		if got := normalizedRoute(r); got != want {
			t.Fatalf("route(%q)=%q want %q", in, got, want)
		}
	}
}

// TestRouteCardinalityBounded: hostile paths must not grow the label domain.
func TestRouteCardinalityBounded(t *testing.T) {
	distinct := map[string]bool{}
	for i := 0; i < 5000; i++ {
		p := "/v1/sandboxes/sb_x/fs/attacker" + string(rune('a'+i%26)) + "/" + randomName(i)
		r := httptestRequest("GET", p, nil)
		distinct[normalizedRoute(r)] = true
	}
	if len(distinct) > 4 {
		t.Fatalf("unbounded route cardinality: %d distinct routes", len(distinct))
	}
}

func randomName(i int) string {
	return "f" + strconv.Itoa(i) + "x" + strconv.Itoa(i*7)
}

func TestSpanPersistsSlowStages(t *testing.T) {
	var persisted []store.Event
	SetEventSink(func(_ context.Context, e store.Event) error {
		persisted = append(persisted, e)
		return nil
	})
	t.Cleanup(func() { SetEventSink(nil) })

	slowThreshold = 1 * time.Millisecond
	defer func() { slowThreshold = 250 * time.Millisecond }()

	ctx, done := StartSpan(context.Background(), "sandbox.create", "sb_1", "driver_start")
	time.Sleep(3 * time.Millisecond)
	done()

	if len(persisted) != 1 || persisted[0].Type != "sandbox.create.stage.driver_start" {
		t.Fatalf("slow span not persisted: %+v", persisted)
	}

	persisted = nil
	fastThreshold := slowThreshold
	_ = fastThreshold
	ctx2, done2 := StartSpan(context.Background(), "sandbox.create", "sb_1", "fast_stage")
	done2() // completes instantly; below threshold
	time.Sleep(5 * time.Millisecond)
	if len(persisted) != 0 {
		t.Fatalf("fast span should not persist: %+v", persisted)
	}
	_ = ctx
	_ = ctx2
}
