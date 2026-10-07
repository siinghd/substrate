package metrics

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// ProcResolver is the production stats source for the usage sampler. The
// sampler was wired with a nil cgroupFn outside tests, which meant zero
// usage samples ever reached the ledger in real deployments — billing-grade
// metering (plan §8) silently didn't exist. This resolver reads HOST procfs
// accounting for the VMM/shim process of each live sandbox.
//
// Semantics match the sampler's contract: each returned CgroupStats carries
// DELTA cpu microseconds since the previous tick (the rollup SUMs them into
// cpu seconds) and current resident memory (rollup MAXes it into a peak).
//
// Known gaps, documented rather than hidden: firecracker's jailed+daemonized
// VMM does not expose a tracked PID yet (those sandboxes sample mem only via
// the guest-reported fallback once wired), and per-tap network counters are
// reported as zero until nftables counter export lands.
type ProcResolver struct {
	src PIDSource

	mu   sync.Mutex
	last map[string]procTicks
}

// PIDSource maps a sandbox ID to the host PID of its VMM/shim process.
type PIDSource func(sandboxID string) (pid int, ok bool)

func NewProcResolver(src PIDSource) *ProcResolver {
	return &ProcResolver{src: src, last: map[string]procTicks{}}
}

// userHZ is the fixed procfs clock tick userspace constant (_SC_CLK_TCK);
// Linux exports jiffies in /proc/<pid>/stat divided by this value.
const userHZ = 100

type procTicks struct{ total uint64 }

func (r *ProcResolver) Stats(sandboxID string) *CgroupStats {
	pid, ok := r.src(sandboxID)
	if !ok || pid <= 0 {
		r.forget(sandboxID)
		return nil
	}
	ticks, err := readProcessTicks(pid)
	if err != nil {
		// Process gone between listing and read: forget baseline so a
		// resurrected PID never produces a bogus giant delta.
		r.forget(sandboxID)
		return nil
	}
	rssKB, err := ReadProcRSS(pid)
	if err != nil {
		rssKB = 0
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	prev, seen := r.last[sandboxID]
	r.last[sandboxID] = procTicks{total: ticks}
	if !seen || ticks < prev.total {
		return &CgroupStats{MemPeakKB: rssKB} // first tick: baseline only
	}
	deltaTicks := ticks - prev.total
	return &CgroupStats{
		CPUUS:     int64(deltaTicks) * (1e6 / userHZ),
		MemPeakKB: rssKB,
		RxBytes:   0,
		TxBytes:   0,
	}
}

func (r *ProcResolver) forget(sandboxID string) {
	r.mu.Lock()
	delete(r.last, sandboxID)
	r.mu.Unlock()
}

// Sync drops tick baselines for sandboxes no longer live. Without it the
// map grew one entry per destroyed sandbox forever: once a sandbox leaves
// the sampler's live list, Stats is never called for it again and forget()
// never runs. Call once per sampler tick with the current live set —
// O(live) per tick.
func (r *ProcResolver) Sync(liveIDs []string) {
	live := make(map[string]struct{}, len(liveIDs))
	for _, id := range liveIDs {
		live[id] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.last {
		if _, ok := live[id]; !ok {
			delete(r.last, id)
		}
	}
}

// readProcessTicks sums utime+stime+cutime+cstime from /proc/<pid>/stat:
// guest workloads run as (grand)children of the shim, and reaped children
// accumulate into cutime/cstime, so the sum tracks actual compute delivered.
func readProcessTicks(pid int) (uint64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(b)
	// Fields after the parenthesized comm (which may contain spaces).
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 > len(s) {
		return 0, fmt.Errorf("stat parse: no comm close")
	}
	f := strings.Fields(s[i+2:])
	// Remaining fields start at state (field 3); utime=14, stime=15,
	// cutime=16, cstime=17 → indices 11..14 here.
	if len(f) < 15 {
		return 0, fmt.Errorf("stat parse: short")
	}
	var total uint64
	for _, idx := range []int{11, 12, 13, 14} {
		v, err := strconv.ParseUint(f[idx], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("stat parse: %w", err)
		}
		total += v
	}
	return total, nil
}

// ReadProcRSS reads VmRSS (KB) for a host pid from /proc/<pid>/status.
// Exported for reuse by measurement tooling (bench/phase0 resident sampling).
func ReadProcRSS(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				return 0, fmt.Errorf("status VmRSS parse")
			}
			kb, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return 0, err
			}
			return kb, nil
		}
	}
	return 0, fmt.Errorf("no VmRSS line")
}
