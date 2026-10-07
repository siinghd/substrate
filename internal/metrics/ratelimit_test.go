package metrics

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRateLimiterBurstAndRefill(t *testing.T) {
	rl := NewRateLimiter(10, 5)
	allowed := 0
	for i := 0; i < 20; i++ {
		if rl.Allow("p1") {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("burst not enforced: allowed %d, want 5", allowed)
	}
}

func TestRateLimiterPerPrincipalIsolation(t *testing.T) {
	rl := NewRateLimiter(1, 2)
	t0 := time.Unix(0, 0)
	rl.now = func() time.Time { return t0 }
	for i := 0; i < 2; i++ {
		if !rl.Allow("victim") {
			t.Fatalf("victim limited at i=%d before burst exhausted", i)
		}
	}
	if rl.Allow("victim") {
		t.Fatal("victim burst not consumed")
	}
	if !rl.Allow("attacker") {
		t.Fatal("attacker bucket polluted by victim")
	}
}

func TestRateLimitMiddleware429(t *testing.T) {
	rl := NewRateLimiter(1, 3)
	handler := RateLimitMiddleware(rl, func(r *http.Request) string { return "p1" })(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	var got429 int
	for i := 0; i < 10; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/x", nil))
		if rec.Code == http.StatusTooManyRequests {
			got429++
		}
	}
	if got429 == 0 {
		t.Fatal("limiter never fired")
	}
}

func TestRateLimiterConcurrent(t *testing.T) {
	rl := NewRateLimiter(1000, 1000)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				rl.Allow("p")
			}
		}(i)
	}
	wg.Wait()
}
