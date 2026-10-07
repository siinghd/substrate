package metrics

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterExemptsHealthAndMetrics(t *testing.T) {
	rl := NewRateLimiter(0.001, 1) // effectively always limited
	handler := RateLimitMiddleware(rl, func(r *http.Request) string { return "p1" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))

	// burn the bucket first
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
	}

	// /healthz and /metrics must still pass
	for _, path := range []string{"/healthz", "/metrics"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("%s blocked by limiter (%d) — monitoring blinded under flood", path, rec.Code)
		}
	}
	// regular API still limited
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/y", nil))
	if rec.Code != 429 {
		t.Fatalf("API should be limited: got %d", rec.Code)
	}
}
