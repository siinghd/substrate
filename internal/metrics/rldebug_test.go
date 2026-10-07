package metrics

import (
	"fmt"
	"testing"
	"time"
)

// TestRateLimiterRefillsPerPrincipal is the basic contract the old TestRLDebug
// only logged: a principal spends its burst and then refills at the configured
// rate.
func TestRateLimiterRefillsPerPrincipal(t *testing.T) {
	rl := NewRateLimiter(1, 2)
	now := time.Now()
	rl.now = func() time.Time { return now }

	if !rl.Allow("victim") || !rl.Allow("victim") {
		t.Fatal("burst of 2 was not honoured")
	}
	if rl.Allow("victim") {
		t.Fatal("third request inside the same instant was allowed")
	}
	now = now.Add(time.Second)
	if !rl.Allow("victim") {
		t.Fatal("bucket did not refill after a second")
	}
}

// TestRateLimiterEvictionDoesNotWipeOtherPrincipals: the old limiter scanned
// the whole table on every insert once full, deleted nothing (every bucket had
// just been touched) and then threw the entire map away — so a key-churn flood
// re-granted every legitimate tenant a fresh full burst, disabling limiting for
// everyone. Eviction must remove exactly one victim.
func TestRateLimiterEvictionDoesNotWipeOtherPrincipals(t *testing.T) {
	rl := NewRateLimiter(1, 2)
	now := time.Now()
	rl.now = func() time.Time { return now }

	// An honest tenant spends its whole burst.
	const honest = "ip_10.0.0.1"
	if !rl.Allow(honest) || !rl.Allow(honest) {
		t.Fatal("honest tenant could not spend its burst")
	}
	if rl.Allow(honest) {
		t.Fatal("honest tenant was not limited after its burst")
	}

	// Flood far past the table cap with fresh keys, keeping the honest
	// tenant recently used so LRU never picks it.
	for i := 0; i < maxBucketsTotal*3; i++ {
		rl.Allow(fmt.Sprintf("ip_flood_%d", i))
		if i%50 == 0 {
			rl.Allow(honest) // touch it; still expected to be denied
		}
	}
	if rl.Allow(honest) {
		t.Fatal("after a key-churn flood the honest tenant got a fresh burst: eviction wiped live state")
	}
	if n := rl.Len(); n > maxBucketsTotal {
		t.Fatalf("bucket table holds %d entries, cap is %d", n, maxBucketsTotal)
	}
}

// TestRateLimiterTableStaysBounded: memory must be O(cap), not O(distinct keys
// ever seen).
func TestRateLimiterTableStaysBounded(t *testing.T) {
	rl := NewRateLimiter(100, 100)
	for i := 0; i < maxBucketsTotal*5; i++ {
		rl.Allow(fmt.Sprintf("k%d", i))
	}
	if n := rl.Len(); n > maxBucketsTotal {
		t.Fatalf("bucket table grew to %d, cap is %d", n, maxBucketsTotal)
	}
}
