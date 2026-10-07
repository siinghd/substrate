// Package metrics — rate limiting (see ratelimit.go).
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// TokenBucket is an O(1) per-request limiter: fixed refill rate, bounded
// burst, constant memory per principal.
type TokenBucket struct {
	tokens   float64
	last     time.Time
	capacity float64
	refill   float64 // tokens per second

	// intrusive LRU links, owned by the shard
	key        string
	prev, next *TokenBucket
}

// rlShard is one striped partition of the bucket table with its own lock and
// its own LRU list.
//
// The previous single map did a FULL SCAN of every bucket on insert once it
// filled, deleted nothing (all buckets had just been touched), and then threw
// the entire map away. That made a full table cost O(n) per request behind one
// global mutex — the limiter became the throughput ceiling at the design
// centre — and the wipe re-granted every legitimate tenant a fresh full burst,
// so the flood also disabled limiting for everyone else.
type rlShard struct {
	mu      sync.Mutex
	buckets map[string]*TokenBucket
	max     int
	head    *TokenBucket // most recently used
	tail    *TokenBucket // least recently used
}

func (s *rlShard) unlink(b *TokenBucket) {
	if b.prev != nil {
		b.prev.next = b.next
	} else {
		s.head = b.next
	}
	if b.next != nil {
		b.next.prev = b.prev
	} else {
		s.tail = b.prev
	}
	b.prev, b.next = nil, nil
}

func (s *rlShard) pushFront(b *TokenBucket) {
	b.prev = nil
	b.next = s.head
	if s.head != nil {
		s.head.prev = b
	}
	s.head = b
	if s.tail == nil {
		s.tail = b
	}
}

// evictLRU drops exactly one bucket: the least recently used. O(1), and it
// never touches anyone else's state.
func (s *rlShard) evictLRU() {
	if s.tail == nil {
		return
	}
	victim := s.tail
	s.unlink(victim)
	delete(s.buckets, victim.key)
	ratelimitEvictionsTotal.Inc()
}

const rlShards = 16

type RateLimiter struct {
	shards [rlShards]*rlShard

	ratePerSec float64
	burst      int
	now        func() time.Time
}

var (
	rateLimitedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "rate_limited_total",
		Help: "Requests rejected by the rate limiter",
	})
	ratelimitEvictionsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Namespace: "substrate", Name: "ratelimit_evictions_total",
		Help: "Rate-limiter buckets evicted because the table hit its cap (a sustained rise means key churn, i.e. an attacker cycling principals)",
	})
)

// maxBucketsTotal bounds the whole table; each shard gets an equal slice.
const maxBucketsTotal = 10000

func NewRateLimiter(ratePerSec float64, burst int) *RateLimiter {
	rl := &RateLimiter{ratePerSec: ratePerSec, burst: burst, now: time.Now}
	per := maxBucketsTotal / rlShards
	if per < 1 {
		per = 1
	}
	for i := range rl.shards {
		rl.shards[i] = &rlShard{buckets: make(map[string]*TokenBucket), max: per}
	}
	return rl
}

// shardFor spreads principals across locks so one hot key cannot serialize
// every other principal's requests.
func (rl *RateLimiter) shardFor(principal string) *rlShard {
	var h uint32 = 2166136261
	for i := 0; i < len(principal); i++ {
		h ^= uint32(principal[i])
		h *= 16777619
	}
	return rl.shards[h%rlShards]
}

// Allow consumes one token for principal. O(1) worst case: at most one LRU
// eviction, no scans.
func (rl *RateLimiter) Allow(principal string) bool {
	now := rl.now()
	s := rl.shardFor(principal)
	s.mu.Lock()
	defer s.mu.Unlock()

	b, ok := s.buckets[principal]
	if ok {
		s.unlink(b)
	} else {
		if len(s.buckets) >= s.max {
			s.evictLRU()
		}
		b = &TokenBucket{
			tokens: float64(rl.burst), last: now,
			capacity: float64(rl.burst), refill: rl.ratePerSec, key: principal,
		}
		s.buckets[principal] = b
	}
	s.pushFront(b)

	elapsed := now.Sub(b.last).Seconds()
	b.tokens += elapsed * b.refill
	if b.tokens > b.capacity {
		b.tokens = b.capacity
	}
	b.last = now
	if b.tokens < 1 {
		rateLimitedTotal.Inc()
		return false
	}
	b.tokens--
	return true
}

// Len reports the number of live buckets (tests / diagnostics).
func (rl *RateLimiter) Len() int {
	n := 0
	for _, s := range rl.shards {
		s.mu.Lock()
		n += len(s.buckets)
		s.mu.Unlock()
	}
	return n
}

// RateLimitMiddleware rejects principals exceeding rate/burst with 429.
// Identity falls back to bearer-token prefix (never raw key material as map key).
// Exempt paths (health/metrics) bypass the limiter so monitoring survives floods.
func RateLimitMiddleware(rl *RateLimiter, identity func(r *http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt := r.URL.Path == "/healthz" || r.URL.Path == "/metrics"; !exempt {
				id := identity(r)
				if id != "" && !rl.Allow(id) {
					w.Header().Set("Retry-After", "1")
					http.Error(w, `{"code":"rate_limited","message":"too many requests"}`, http.StatusTooManyRequests)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
