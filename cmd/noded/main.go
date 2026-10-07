// Command noded is the substrate node agent (plan §5.3): one static binary
// that supervises VMMs, programs networking, serves the v1 REST API in
// standalone mode, and dials out to a control-plane cell in fleet mode.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/substrate-sh/substrate/internal/api"
	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/gateway"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
	fc "github.com/substrate-sh/substrate/internal/vmm/firecracker"
	gvisordrv "github.com/substrate-sh/substrate/internal/vmm/gvisor"
	procdrv "github.com/substrate-sh/substrate/internal/vmm/process"
)

func main() {
	cfgPath := "-"
	for i, a := range os.Args {
		if a == "--config" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
		}
	}
	if err := run(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "noded:", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := logging.New(cfg.Log.Level)

	if err := os.MkdirAll(cfg.Node.DataDir, 0o755); err != nil {
		return err
	}
	for _, sub := range []string{"execs", "sandboxes", "artifacts"} {
		os.MkdirAll(filepath.Join(cfg.Node.DataDir, sub), 0o755)
	}

	var st store.Store
	switch cfg.Mode {
	case "cell":
		st, err = store.OpenPostgres(cfg.Cell.DatabaseURL)
	default:
		dbPath := filepath.Join(cfg.Node.DataDir, "substrate.db")
		st, err = store.OpenSQLite(dbPath)
	}
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := bootstrap(ctx, st); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	drv, err := buildDriver(ctx, cfg, log)
	if err != nil {
		return err
	}

	sup := reconcile.NewSupervisor(st, drv, log, cfg.Node.DataDir)
	// Sandbox networking. Failing here is fatal on purpose: a node that
	// silently serves sandboxes with no network looks healthy and is useless,
	// and one that serves them with no EGRESS POLICY is worse than useless.
	var guestIP func(string) (string, bool)
	netProv, nerr := newNetnsProvider(cfg, log)
	if nerr != nil {
		return fmt.Errorf("sandbox networking: %w", nerr)
	}
	if netProv != nil {
		sup.SetNetwork(netProv)
		guestIP = netProv.GuestIP
	}
	go sup.Events(ctx)
	if cfg.Backup.Enabled {
		iv := time.Duration(cfg.Backup.IntervalMins) * time.Minute
		if iv <= 0 {
			iv = time.Hour
		}
		go runBackups(ctx, st, cfg.Backup.Dir, iv, cfg.Backup.Keep, log)
	} else {
		log.Warn("scheduled backups are DISABLED; the store is not being copied anywhere")
	}
	procRes := metrics.NewProcResolver(func(id string) (int, bool) { return sup.VMMpid(id) })
	metrics.SetEventSink(func(ctx context.Context, e store.Event) error {
		return st.AppendEvent(ctx, &e)
	})

	srv := api.NewServer(cfg, log, st, sup)
	apiHandler := metrics.HTTPMiddleware(srv.Routes())

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	mux.Handle("/v1/", apiHandler)
	mux.Handle("/metrics", promhttp.Handler())

	// Per-principal token bucket: 50 req/s sustained, burst 100. O(1)/req.
	// Health/metrics stay OUTSIDE the limiter: an anonymous flood must not
	// blind monitoring exactly when it's needed.
	limiter := metrics.NewRateLimiter(50, 100)
	rootHandler := metrics.RateLimitMiddleware(limiter, limiterIdentity())(mux)

	apiLn, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		return fmt.Errorf("api listen: %w", err)
	}
	httpSrv := &http.Server{Handler: rootHandler, ReadHeaderTimeout: 10 * time.Second}
	var gwSrv *http.Server
	go func() {
		log.Info("api listening", "addr", cfg.API.Listen, "mode", cfg.Mode, "driver", cfg.Driver.Name)
		if err := httpSrv.Serve(apiLn); err != nil && err != http.ErrServerClosed {
			log.Error("api server died", "err", err.Error())
			cancel()
		}
	}()

	// Graceful drain on SIGINT/SIGTERM: stop accepting, finish in-flight
	// requests (≤10s), then release the rest with the process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("shutting down", "signal", sig.String())
		drainCtx, dcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer dcancel()
		_ = httpSrv.Shutdown(drainCtx)
		if gwSrv != nil {
			_ = gwSrv.Shutdown(drainCtx)
		}
		// Stop the VMs before the process exits. Without this they are
		// daemonized orphans: still holding RAM, taps and vsock CIDs, while
		// the next noded reads state=running, sees an empty machine map, and
		// starts a second VMM for each one on top of the same disk image.
		sup.Shutdown(drainCtx)
		cancel()
	}()

	if cfg.Gateway.Enabled {
		resolver := newStoreResolver(st, cfg, log, guestIP)
		gw := gateway.New(resolver, cfg.Gateway.BaseDomain, cfg.Gateway.TLSCert, cfg.Gateway.TLSKey)
		gwLn, err := net.Listen("tcp", cfg.Gateway.Listen)
		if err != nil {
			return fmt.Errorf("gateway listen: %w", err)
		}
		// Preview ingress is internet-facing and mostly unauthenticated: throttle
		// per peer IP (never XFF — spoofable) with generous limits so NAT'd
		// browser traffic is unaffected while floods get shed early.
		gwLimiter := metrics.NewRateLimiter(200, 400)
		gwIdentity := func(r *http.Request) string {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			return "gwip_" + host
		}
		// The limiter goes OUTSIDE the instrumentation, not inside: with it
		// inside, every shed 429 was still measured, so the very flood the
		// limiter exists to drop kept minting metric label sets. Requests that
		// are refused must be cheap.
		//
		// Preview paths are arbitrary tenant application paths, so they are
		// deliberately NOT run through the API's route-label middleware — that
		// middleware normalizes against known API segments, which is meaningless
		// here. gateway traffic is measured by the limiter's own counters.
		gwSrv = &http.Server{
			Handler:           metrics.RateLimitMiddleware(gwLimiter, gwIdentity)(gw),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       5 * time.Minute,
			IdleTimeout:       2 * time.Minute,
		}
		go func() {
			log.Info("gateway listening", "addr", cfg.Gateway.Listen, "domain", cfg.Gateway.BaseDomain)
			if err := gwSrv.Serve(gwLn); err != nil && err != http.ErrServerClosed {
				log.Error("gateway died", "err", err.Error())
			}
		}()
	}

	if cfg.Mode == "node" {
		if err := startCellReporter(ctx, cfg, log, st, sup); err != nil {
			log.Warn("cell reporting disabled", "err", err.Error())
		}
	}

	// Ledger retention: prune idempotency (>25h), events/usage (>30d) every
	// 10 minutes — O(stale rows), invisible in steady state.
	// Metrics persistence: snapshot histogram percentiles + runtime gauges to
	// the SQL ledger every 60s so latency trends are graphable across restarts.
	metrics.UpdateRuntimeGauges()
	snapTicker := time.NewTicker(60 * time.Second)
	defer snapTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-snapTicker.C:
				metrics.UpdateRuntimeGauges()
				if n, err := metrics.PersistHistograms(ctx, st, prometheus.DefaultGatherer); err != nil {
					log.Warn("metric snapshot failed", "err", err.Error())
				} else if n > 0 {
					log.Debug("metric snapshots persisted", "count", n)
				}
			}
		}
	}()

	pruneTicker := time.NewTicker(10 * time.Minute)
	defer pruneTicker.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pruneTicker.C:
				if n, err := st.PruneBefore(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
					log.Warn("retention prune failed", "err", err.Error())
				} else if n > 0 {
					log.Info("retention pruned", "rows", n)
				}
				pruneExecLogs(filepath.Join(cfg.Node.DataDir, "execs"), 30*24*time.Hour, log)
			}
		}
	}()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Convergence runs on a bounded pool, not inline in the tick loop: one
	// slow restore or snapshot must not stall TTL enforcement, destroys and
	// idle pauses for every other sandbox on the node.
	sup.StartWorkers(ctx, 0)
	sampler := metrics.NewSampler(st, log)
	// Production stats source: host procfs accounting for each sandbox's
	// shim/VMM process. Without this the usage ledger never receives samples.
	sampler.SetCgroupResolver(func(id string) *metrics.CgroupStats {
		return procRes.Stats(id)
	})
	attrCache := map[string][2]string{}
	const attrCacheMax = 8192
	go func() {
		sampler.Run(ctx, liveResolver{sup}, func(id string) (string, string) {
			if a, ok := attrCache[id]; ok {
				return a[0], a[1]
			}
			sb, err := st.GetSandbox(ctx, id)
			if err != nil {
				return "", ""
			}
			// Bound the cache: destroyed sandboxes would otherwise pin
			// entries forever on a long-lived node.
			if len(attrCache) >= attrCacheMax {
				dropped := 0
				for k := range attrCache {
					delete(attrCache, k)
					if dropped++; dropped >= attrCacheMax/2 {
						break
					}
				}
			}
			attrCache[id] = [2]string{sb.ProjectID, sb.ExternalUserID}
			return sb.ProjectID, sb.ExternalUserID
		})
	}()
	log.Info("noded ready")
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			live := sup.LiveSandboxes()
			metrics.SetLive(len(live))
			procRes.Sync(live)
			sup.Tick(ctx)
		}
	}
}

func bootstrap(ctx context.Context, st store.Store) error {
	orgs, err := st.ListOrgs(ctx)
	if err != nil || len(orgs) > 0 {
		return err
	}
	now := time.Now()
	org := &store.Org{ID: "org_local", Name: "local", CreatedAt: now}
	if err := st.CreateOrg(ctx, org); err != nil {
		return err
	}
	proj := &store.Project{ID: "prj_default", OrgID: org.ID, Name: "default", MaxSandboxes: 1000, CreatedAt: now}
	if err := st.CreateProject(ctx, proj); err != nil {
		return err
	}
	key := os.Getenv("SUBSTRATE_API_KEY")
	if key == "" {
		if os.Getenv("SUBSTRATE_INSECURE_DEV") != "1" {
			return fmt.Errorf("SUBSTRATE_API_KEY required (set SUBSTRATE_INSECURE_DEV=1 to allow the well-known dev key)")
		}
		key = "sk_dev_local_key"
	}
	return st.CreateAPIKey(ctx, &store.APIKey{
		ID: "k_bootstrap", OrgID: org.ID, ProjectID: proj.ID,
		Kind: "project", KeyHash: hashKey(key), CreatedAt: now,
	})
}

func hashKey(k string) string { return auth.HashKey(k) }

// pruneExecLogs deletes exec output logs older than maxAge. Without this the
// execs dir grows without bound (the SQL prune never touches files).
func pruneExecLogs(dir string, maxAge time.Duration, log *logging.Logger) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // dir absent: nothing to prune
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && log != nil {
			log.Debug("exec log prune failed", "file", e.Name(), "err", err.Error())
		}
	}
}

func buildDriver(ctx context.Context, cfg *config.Config, log *logging.Logger) (vmm.Driver, error) {
	switch cfg.Driver.Name {
	case "firecracker":
		return fc.NewDriver(fc.Config{
			FirecrackerBin: cfg.Driver.FirecrackerBin,
			JailerBin:      cfg.Driver.JailerBin,
			JailBaseDir:    cfg.Node.JailDir,
			NetnsDir:       "/var/run/netns",
			Log:            log,
		}), nil
	case "gvisor":
		// tier-`kernel`: a real isolation boundary on machines with no
		// /dev/kvm. Uses the same substrate-shim agent and the same SGP/1
		// protocol as every other tier; only the containment differs.
		shimBin := filepath.Join(cfg.Node.DataDir, "artifacts", "substrate-shim")
		if err := buildShim(shimBin); err != nil {
			return nil, fmt.Errorf("build shim: %w", err)
		}
		return gvisordrv.NewDriver(gvisordrv.Config{
			RunscBin:  cfg.Driver.RunscBin,
			ShimBin:   shimBin,
			StateDir:  filepath.Join(cfg.Node.DataDir, "gvisor-state"),
			BundleDir: filepath.Join(cfg.Node.DataDir, "gvisor"),
			SockDir:   cfg.Driver.GvisorSockDir,
			Platform:  cfg.Driver.GvisorPlatform,
			// Shared, node-owned template trees -- one per template, not one
			// per sandbox -- and a per-sandbox write ceiling.
			TemplateDir:    filepath.Join(cfg.Node.DataDir, "gvisor-templates"),
			OverlaySizeMiB: cfg.Driver.GvisorOverlaySizeMiB,
			Log:            func(msg string, kv ...any) { log.Debug(msg, kv...) },
		}), nil
	default:
		shimBin := filepath.Join(cfg.Node.DataDir, "artifacts", "substrate-shim")
		if err := buildShim(shimBin); err != nil {
			return nil, fmt.Errorf("build shim: %w", err)
		}
		d := procdrv.NewDriver(shimBin)
		d.SetLogger(func(msg string, kv ...any) { log.Debug(msg, kv...) })
		return d, nil
	}
}

func buildShim(dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	cmd := exec.Command("go", "build", "-o", dest, "./cmd/shim")
	cmd.Dir = wd
	// CGO_ENABLED=0, for the same reason the Makefile sets it: this binary is
	// mounted into a TENANT-supplied rootfs, and a dynamically linked agent
	// needs a loader and libc that a musl/distroless/scratch image does not
	// have. This is the path production actually takes -- the Makefile flag
	// alone would not have covered it, and the gvisor driver's PT_INTERP check
	// would have rejected every create on this tier.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
