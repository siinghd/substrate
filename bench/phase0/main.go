// Command phase0 is the density/restore spike harness (plan §10, Phase 0).
//
// It measures exactly the two numbers the plan stands on:
//  1. restore latency with networking + uniqueness protocol (not bare VM-create)
//  2. true resident set per sandbox under a realistic agent workload trace
//
// Exit criteria (measured, not vibed):
//
//	p50 restore <= 300ms / p99 <= 1s at 50 concurrent restores
//	resident p50 <= 250MB per 1GB-configured sandbox
//
// Requires /dev/kvm and the firecracker driver; refuses to run otherwise so
// numbers are never polluted by the process tier.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
	fc "github.com/substrate-sh/substrate/internal/vmm/firecracker"
)

type result struct {
	RestoreMS []int64 `json:"restore_ms"`
	P50       int64   `json:"p50_ms"`
	P95       int64   `json:"p95_ms"`
	P99       int64   `json:"p99_ms"`
	N         int     `json:"n"`
	Resident  struct {
		Samples []int64 `json:"samples_kb"`
		P50KB   int64   `json:"p50_kb"`
	} `json:"resident"`
	Pass bool `json:"pass"`
}

func main() {
	concurrency := flag.Int("c", 50, "concurrent restores")
	iterations := flag.Int("n", 100, "total restores")
	template := flag.String("template", "demo", "template name")
	flag.Parse()

	if _, err := os.Stat("/dev/kvm"); err != nil {
		fmt.Fprintln(os.Stderr, "phase0: /dev/kvm unavailable — this harness only produces valid numbers on KVM hardware (plan §10).")
		os.Exit(3)
	}

	cfgPath := "-"
	for i, a := range os.Args {
		if a == "--config" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
		}
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		fatal(err)
	}
	cfg.Driver.Name = "firecracker"

	log := logging.New("info")
	st, err := store.OpenSQLite(cfg.Node.DataDir + "/phase0.db")
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	drv := fc.NewDriver(fc.Config{
		FirecrackerBin: cfg.Driver.FirecrackerBin,
		JailerBin:      cfg.Driver.JailerBin,
		JailBaseDir:    cfg.Node.JailDir,
		Log:            log,
	})
	sup := reconcile.NewSupervisor(st, drv, log, cfg.Node.DataDir)
	go sup.Events(context.Background())

	tpl, err := st.GetTemplateByName(context.Background(), "prj_default", *template)
	if err != nil {
		fatal(fmt.Errorf("template %q not found; create it first", *template))
	}

	fmt.Printf("==> phase0: %d restores at concurrency %d\n", *iterations, *concurrency)
	latencies := make([]int64, 0, *iterations)
	created := make([]string, 0, *iterations)
	var mu sync.Mutex
	sem := make(chan struct{}, *concurrency)
	var wg sync.WaitGroup

	start := time.Now()
	for i := 0; i < *iterations; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx := context.Background()
			now := time.Now()
			sb := &store.Sandbox{
				// Platform-generated ID shape: driver.prepare refuses
				// anything else, so hand-rolled ids like sb_p0_00001 made
				// every create fail on real hardware.
				ID:    ids.Sandbox(),
				OrgID: "org_local", ProjectID: "prj_default",
				TemplateID: tpl.ID, State: store.StateCreating, Desired: "running",
				Tier: "hv", MemMiB: tpl.MemMiB, VCPUs: tpl.VCPUs,
				CreatedAt: now, UpdatedAt: now,
			}
			if err := st.CreateSandbox(ctx, sb); err != nil {
				log.Error("create row failed", "err", err.Error())
				return
			}
			mu.Lock()
			created = append(created, sb.ID)
			mu.Unlock()

			// Per-restore clock. The old code measured time.Since(harness
			// start): every sample carried the semaphore queue wait of all
			// prior restores, inflating p50/p99 by construction.
			t0 := time.Now()
			if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
				log.Error("restore failed", "sandbox", sb.ID, "err", err.Error())
				return
			}
			ms := time.Since(t0).Milliseconds()
			mu.Lock()
			latencies = append(latencies, ms)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	wall := time.Since(start)

	sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
	res := result{RestoreMS: latencies, N: len(latencies)}
	if len(latencies) > 0 {
		res.P50 = pct(latencies, 50)
		res.P95 = pct(latencies, 95)
		res.P99 = pct(latencies, 99)
	}

	// Resident set per live sandbox, sampled from host procfs now that all
	// restores have settled. Never measured before — the field was always
	// empty, silently dropping the second exit criterion.
	var rssSamples []int64
	for _, id := range created {
		if pid, ok := sup.VMMpid(id); ok && pid > 0 {
			if kb, err := metrics.ReadProcRSS(pid); err == nil {
				rssSamples = append(rssSamples, kb)
			}
		}
	}
	sort.Slice(rssSamples, func(a, b int) bool { return rssSamples[a] < rssSamples[b] })
	res.Resident.Samples = rssSamples
	res.Resident.P50KB = pct(rssSamples, 50)

	res.Pass = res.P50 <= 300 && res.P99 <= 1000 && res.N >= *iterations*9/10

	// Tear down everything we booted; a benchmark must not leave 100
	// microVMs holding guest RAM on the host.
	for _, id := range created {
		_ = sup.Destroy(context.Background(), id)
	}

	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	fmt.Printf("wall: %s (%.1f restores/s)\n", wall.Round(time.Millisecond), float64(res.N)/wall.Seconds())
	if !res.Pass {
		fmt.Println("==> FAIL: exit criteria not met (p50<=300ms, p99<=1s). Per plan §11 risk 2: stop and rethink before fleet work.")
		os.Exit(1)
	}
	fmt.Println("==> PASS")
}

func pct(sorted []int64, p int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := len(sorted) * p / 100
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "phase0:", err)
	os.Exit(1)
}
