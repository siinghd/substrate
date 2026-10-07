package metrics

import (
	"os"
	"testing"
	"time"
)

// TestProcResolverMeasuresRealProcess attempts the real thing: burn CPU and
// assert the resolver observes the delta and resident memory. This is the
// production stats path — if it reports zeros, the usage ledger is dead.
// The spinner runs in-process so the measured PID is unambiguous (a shell
// wrapper would fork the burner into a different PID).
func TestProcResolverMeasuresRealProcess(t *testing.T) {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				x := 1
				for i := 0; i < 100000; i++ {
					x += i
				}
				_ = x
			}
		}
	}()
	defer func() { close(stop); <-done }()

	pid := os.Getpid()
	r := NewProcResolver(func(id string) (int, bool) {
		if id == "sb_busy000000001" {
			return pid, true
		}
		return 0, false
	})

	first := r.Stats("sb_busy000000001")
	if first == nil {
		t.Fatal("resolver returned nil for live pid")
	}
	if first.MemPeakKB <= 0 {
		t.Fatalf("RSS not observed: %+v", first)
	}
	time.Sleep(400 * time.Millisecond) // let the spinner accumulate ticks
	second := r.Stats("sb_busy000000001")
	if second.CPUUS <= 0 {
		t.Fatalf("no CPU delta measured across ticks: %+v", second)
	}

	// Unknown sandbox: no sample, no crash.
	if got := r.Stats("sb_unknown00001"); got != nil {
		t.Fatalf("unknown sandbox produced stats: %+v", got)
	}
}
