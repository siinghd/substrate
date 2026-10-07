package metrics

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// HTTPMiddleware instruments every API request: latency histogram by
// route+method+status, throughput counter, error-rate counter. Route labels
// use the matched pattern (e.g. "/v1/sandboxes/{id}") to keep cardinality
// bounded — O(1) label sets, not per-request.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)

		route := normalizedRoute(r)
		method := safeMethod(r.Method)
		status := strconv.Itoa(rec.status)
		httpDuration.WithLabelValues(method, route, status).Observe(time.Since(start).Seconds())
		httpRequestsTotal.WithLabelValues(method, route, status).Inc()
		if rec.status >= 500 {
			httpErrorsTotal.WithLabelValues(method, route).Inc()
		}
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack passes through so protocol upgrades still work. Wrapping the
// ResponseWriter hid the underlying Hijacker, so every WebSocket/Upgrade
// request through an instrumented handler failed.
func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("metrics: underlying ResponseWriter is not a http.Hijacker")
	}
	return h.Hijack()
}

// ReadFrom passes through to the underlying sendfile fast path when present.
func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if rf, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{r.ResponseWriter}, src)
}

// knownMethods bounds the method label. r.Method is an arbitrary attacker-
// supplied token — "AAAA1 / HTTP/1.1" is a valid request line — and Prometheus
// never evicts label children, so an unbounded method label is a permanent
// memory leak driven from the internet, plus one metric_snapshots row per
// label set per minute.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true,
	http.MethodPut: true, http.MethodPatch: true, http.MethodDelete: true,
	http.MethodOptions: true, http.MethodConnect: true, http.MethodTrace: true,
}

func safeMethod(m string) string {
	if knownMethods[m] {
		return m
	}
	return "other"
}

const (
	// maxRouteSegments caps path depth. Without it, alternating known and
	// unknown segments made the label domain grow exponentially with depth:
	// /v1/x/v1/x/... at depth 20 is ~10^6 distinct labels from one client.
	maxRouteSegments = 6
	// maxRouteLabels is the hard ceiling on distinct route labels, whatever
	// the depth cap allows combinatorially. Past it everything collapses to
	// "other", so registry memory and metric_snapshots rows are bounded by a
	// constant rather than by what an attacker chooses to send.
	maxRouteLabels = 256
)

var routeLabels struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// admitRoute bounds the live label set. O(1) per request.
func admitRoute(route string) string {
	routeLabels.mu.Lock()
	defer routeLabels.mu.Unlock()
	if routeLabels.seen == nil {
		routeLabels.seen = make(map[string]struct{}, maxRouteLabels)
	}
	if _, ok := routeLabels.seen[route]; ok {
		return route
	}
	if len(routeLabels.seen) >= maxRouteLabels {
		return "other"
	}
	routeLabels.seen[route] = struct{}{}
	return route
}

// ResetRouteLabelsForTest clears the admitted-label set.
func ResetRouteLabelsForTest() {
	routeLabels.mu.Lock()
	routeLabels.seen = nil
	routeLabels.mu.Unlock()
}

// normalizedRoute maps any concrete path onto a bounded set of route shapes.
//
// Collapsing unknown segments to {var} bounds the label *alphabet* but NOT the
// label *domain*: with unbounded depth an attacker can still mint
// exponentially many distinct shapes by alternating known and unknown
// segments. Depth is therefore capped, and the number of distinct labels ever
// admitted is capped on top of that — the combinatorics of even six segments
// over the known-literal set is far too large to rely on depth alone.
//
// O(min(path segments, maxRouteSegments)); output domain <= maxRouteLabels + 1.
func normalizedRoute(r *http.Request) string {
	segs := splitPath(r.URL.Path)
	truncated := false
	if len(segs) > maxRouteSegments {
		segs = segs[:maxRouteSegments]
		truncated = true
	}
	out := make([]string, 0, len(segs)+1)
	for _, s := range segs {
		if isKnownSegment(s) {
			out = append(out, s)
			continue
		}
		// fs paths are legitimately arbitrary: keep one wildcard level
		if len(out) > 0 && out[len(out)-1] == "fs" {
			out = append(out, "{path}")
			continue
		}
		out = append(out, "{var}")
	}
	if truncated {
		out = append(out, "{deep}")
	}
	return admitRoute(joinPath(out))
}

func splitPath(p string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(p); i++ {
		if i == len(p) || p[i] == '/' {
			if i > start {
				out = append(out, p[start:i])
			}
			start = i + 1
		}
	}
	return out
}

func joinPath(segs []string) string {
	out := ""
	for _, s := range segs {
		out += "/" + s
	}
	if out == "" {
		return "/"
	}
	return out
}

// knownSegments mirrors the Routes() registrations; anything else is a variable.
var knownSegments = map[string]bool{
	"v1": true, "sandboxes": true, "execs": true, "templates": true,
	"events": true, "usage": true, "nodes": true, "node": true,
	"enroll": true, "heartbeat": true,
	"exec": true, "stdin": true, "stream": true, "pause": true,
	"resume": true, "snapshots": true, "ports": true, "tokens": true,
	"healthz": true, "metrics": true,
}

func isKnownSegment(s string) bool { return knownSegments[s] }

// IdempotencyReplayCounter counts server-side-observed client retries: each
// increment is one replay served from the idempotency cache. This is the
// "retries" signal of the metrics spec.
var IdempotencyReplays = promauto.NewCounter(prometheus.CounterOpts{
	Namespace: "substrate", Name: "idempotency_replays_total",
	Help: "Mutations served from the idempotency cache (client retries observed)",
})

// RequestIDMiddleware assigns a per-request correlation id: response header
// X-Request-Id plus context value for handler logs.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" || len(id) > 64 || !validIDChars(id) {
			id = newRequestID()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

type requestIDKey struct{}

// RequestIDFrom returns the correlation id attached by the middleware.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

func validIDChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var (
	httpDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "substrate",
		Name:      "http_request_duration_seconds",
		Help:      "API request latency p50/p95/p99 by route",
		Buckets:   []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"method", "route", "status"})

	httpRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate",
		Name:      "http_requests_total",
		Help:      "API request throughput",
	}, []string{"method", "route", "status"})

	httpErrorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "substrate",
		Name:      "http_errors_total",
		Help:      "5xx error rate by route",
	}, []string{"method", "route"})
)

// IncIdempotencyReplay increments the client-retry counter.
func IncIdempotencyReplay() { IdempotencyReplays.Inc() }
