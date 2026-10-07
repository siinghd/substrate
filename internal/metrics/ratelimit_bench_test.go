package metrics

import (
	"testing"
)

func BenchmarkRateLimiterAllowParallel(b *testing.B) {
	rl := NewRateLimiter(1000000, 1000000) // effectively unlimited: measure pure op cost
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			rl.Allow("bench-principal")
			i++
		}
	})
}

func BenchmarkRateLimiterAllowDistinct(b *testing.B) {
	rl := NewRateLimiter(1000000, 1000000)
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			rl.Allow("principal-" + itoa(i%1024))
			i++
		}
	})
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[pos:])
}
