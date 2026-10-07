// Command cell is the hosted control-plane unit: Postgres-backed registry,
// scheduler, node enrollment, and the v1 API routed across nodes.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/substrate-sh/substrate/internal/auth"
	"github.com/substrate-sh/substrate/internal/cell"
	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/metrics"
	"github.com/substrate-sh/substrate/internal/store"
)

// validateNodeAddr refuses addresses that would let a node registration
// redirect cell traffic at infrastructure. Policy (encoded in main_test.go):
// link-local/metadata space (IMDS), non-literal hosts, and unspecified
// addresses are refused — loopback and RFC1918 private ranges are ACCEPTED
// because cells and nodes deploy inside one trusted VPC and same-host dev
// cells use loopback. Enrollment itself is the trust boundary (it requires
// SUBSTRATE_ENROLL_TOKEN); tightening further belongs to the hardened tier,
// not a silent default flip that breaks VPC deployments.
func validateNodeAddr(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return fmt.Errorf("must be host:port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("literal IP required in cell mode")
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("link-local/unspecified addresses not routable")
	}
	return nil
}

func main() {
	cfgPath := "-"
	for i, a := range os.Args {
		if a == "--config" && i+1 < len(os.Args) {
			cfgPath = os.Args[i+1]
		}
	}
	if err := run(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "cell:", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log := logging.New(cfg.Log.Level)
	if cfg.Cell.DatabaseURL == "" {
		return fmt.Errorf("cell.database_url (SUBSTRATE_DB_URL) required")
	}

	st, err := store.OpenPostgres(cfg.Cell.DatabaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer st.Close()

	sqlDB, err := rawDB(cfg.Cell.DatabaseURL)
	if err != nil {
		return err
	}
	reg, err := cell.NewRegistry(sqlDB)
	if err != nil {
		return fmt.Errorf("node registry: %w", err)
	}
	proxy := cell.NewProxy(reg)

	ctx := context.Background()
	if err := bootstrapCell(ctx, st); err != nil {
		return fmt.Errorf("bootstrap: %w", err)
	}

	nodeKey := resolveNodeKey()
	if nodeKey == "" {
		return fmt.Errorf("SUBSTRATE_NODE_API_KEY required: the well-known default credential would let anyone impersonate this cell to every enrolled node")
	}

	// Node-plane signing secret: heartbeats are authenticated with HMAC
	// tokens minted at enrollment. Signing under an empty key would make
	// those tokens forgeable by anyone (capacity spoofing + APIAddr poisoning
	// → credential exfiltration), so refuse to start without one.
	nodeSecret, err := resolveNodeSecret()
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"role":"cell"}`)
	})
	mux.Handle("/metrics", promhttp.Handler())

	// Node plane: enrollment + heartbeats. Both are pre-auth token-guessing
	// surfaces and get the strictest limiter.
	nodePlane := func(h http.Handler) http.Handler {
		return metrics.RateLimitMiddleware(metrics.NewRateLimiter(0.5, 5), remoteIPIdentity)(h)
	}
	mux.Handle("POST /v1/node/enroll", nodePlane(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				NodeID        string   `json:"node_id"`
				Name          string   `json:"name"`
				Region        string   `json:"region"`
				Tier          string   `json:"tier"`
				BYO           bool     `json:"byo"`
				APIAddr       string   `json:"api_addr"`
				MemTotalMiB   int      `json:"mem_total_mib"`
				SnapshotCache []string `json:"snapshot_cache"`
				EnrollToken   string   `json:"enroll_token"`
			}
			if err := jsonDecode(r, &req); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			want := os.Getenv("SUBSTRATE_ENROLL_TOKEN")
			if want == "" || subtle.ConstantTimeCompare([]byte(req.EnrollToken), []byte(want)) != 1 {
				http.Error(w, "invalid enrollment token", http.StatusUnauthorized)
				return
			}
			if !ids.ValidNodeID(req.NodeID) || req.APIAddr == "" {
				http.Error(w, "valid node_id and api_addr required", 400)
				return
			}
			if err := validateNodeAddr(req.APIAddr); err != nil {
				http.Error(w, "api_addr rejected: "+err.Error(), 400)
				return
			}
			now := time.Now()
			n := &cell.Node{
				ID: req.NodeID, Name: req.Name, Region: orDefault(req.Region, "local"),
				Tier: orDefault(req.Tier, "hv"), BYO: req.BYO, APIAddr: req.APIAddr,
				MemTotalMiB: req.MemTotalMiB, SnapshotCache: req.SnapshotCache,
				EnrolledAt: now, LastHeartbeat: now,
			}
			if err := reg.Register(r.Context(), n); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Node credential: HMAC(nodeID) under the cell secret. Heartbeats must
			// present it — otherwise any host could spoof capacity for a node.
			nodeToken, err := auth.MintSandboxToken(nodeSecret, "substrate-cell", n.ID,
				[]string{"node"}, 365*24*time.Hour)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{
				"node_id":    n.ID,
				"status":     "enrolled",
				"node_token": nodeToken,
			})
		})))

	mux.Handle("POST /v1/node/heartbeat", nodePlane(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var hb struct {
			NodeID        string   `json:"node_id"`
			NodeToken     string   `json:"node_token"`
			APIAddr       string   `json:"api_addr"`
			MemAllocMiB   int      `json:"mem_alloc_mib"`
			LiveSandboxes int      `json:"live_sandboxes"`
			SnapshotCache []string `json:"snapshot_cache"`
		}
		if err := jsonDecode(r, &hb); err != nil || hb.NodeID == "" {
			http.Error(w, "bad heartbeat", 400)
			return
		}
		if _, err := auth.VerifySandboxToken(nodeSecret, hb.NodeToken, hb.NodeID); err != nil {
			http.Error(w, "heartbeat authentication failed", http.StatusUnauthorized)
			return
		}
		if hb.APIAddr != "" {
			if err := validateNodeAddr(hb.APIAddr); err != nil {
				http.Error(w, "api_addr rejected: "+err.Error(), 400)
				return
			}
		}
		ok, err := reg.Heartbeat(r.Context(), hb.NodeID, hb.APIAddr, hb.MemAllocMiB, hb.LiveSandboxes, hb.SnapshotCache, time.Now())
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !ok {
			http.Error(w, "unknown node; re-enroll", 404)
			return
		}
		w.WriteHeader(204)
	})))

	// Control surface: create schedules; everything else proxies.
	//
	// authz resolves the caller's IDENTITY, not just its validity. The
	// previous version discarded key.OrgID, so downstream routing had nothing
	// to check ownership against — and the cell forwards to nodes with one
	// privileged credential, so the node could not tell tenants apart either.
	authz := func(next func(http.ResponseWriter, *http.Request, *store.APIKey)) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			key, err := st.GetAPIKeyByHash(r.Context(), auth.HashKey(tok))
			if err != nil || key.RevokedAt != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if key.OrgID == "" {
				// A key with no tenant cannot be scoped; refuse rather than
				// fall through to an unscoped identity.
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r, key)
		})
	}

	mux.Handle("POST /v1/sandboxes", authz(func(w http.ResponseWriter, r *http.Request, key *store.APIKey) {
		body, err := readAll(r)
		if err != nil {
			writeResp(w, 400, nil, fmt.Errorf("body too large or unreadable"))
			return
		}
		// The caller's real org, not a hardcoded literal: this is what the
		// placement records and what every later request is checked against.
		out, status, err := proxy.SchedulerCreate(r.Context(), reg, body, key.OrgID, nodeKey)
		writeResp(w, status, out, err)
	}))

	mux.Handle("/v1/sandboxes/{id}", authz(func(w http.ResponseWriter, r *http.Request, key *store.APIKey) {
		id := r.PathValue("id")
		body, err := readAll(r)
		if err != nil {
			writeResp(w, 400, nil, fmt.Errorf("body too large or unreadable"))
			return
		}
		out, status, err := proxy.Forward(r.Context(), key.OrgID, id, r.Method, "/v1/sandboxes/"+id, body, nodeKey)
		writeResp(w, status, out, err)
	}))

	mux.Handle("/v1/sandboxes/{id}/", authz(func(w http.ResponseWriter, r *http.Request, key *store.APIKey) {
		id := r.PathValue("id")
		body, err := readAll(r)
		if err != nil {
			writeResp(w, 400, nil, fmt.Errorf("body too large or unreadable"))
			return
		}
		// r.URL.Path is DECODED, so %2e%2e%2f arrives here as "../" -- and the
		// ServeMux's own cleanPath runs on the ESCAPED path, so it sees no dot
		// segments and never redirects. Concatenating the suffix therefore let
		// a caller walk out of /v1/sandboxes/{id} and reach any route on the
		// node, carrying the cell's privileged node credential: a cross-tenant
		// read with a legitimate key. Clean the whole target and require it to
		// still be inside this sandbox.
		target, ok := forwardTarget(id, r.URL)
		if !ok {
			writeResp(w, http.StatusNotFound, nil, fmt.Errorf("no such sandbox path"))
			return
		}
		out, status, err := proxy.Forward(r.Context(), key.OrgID, id, r.Method, target, body, nodeKey)
		writeResp(w, status, out, err)
	}))

	// Fleet inventory is operator data, not tenant data: node ids, capacities
	// and api_addrs are exactly what an attacker needs to move laterally.
	// A tenant sees only the nodes bound to its own org (its BYO capacity).
	mux.Handle("GET /v1/nodes", authz(func(w http.ResponseWriter, r *http.Request, key *store.APIKey) {
		nodes, err := reg.List(r.Context(), r.URL.Query().Get("region"))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		mine := make([]*cell.Node, 0, len(nodes))
		for _, n := range nodes {
			if n.OrgID != "" && n.OrgID == key.OrgID {
				mine = append(mine, n)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"nodes": mine})
	}))

	// Ledger retention: cells own the authoritative copy once metering moves
	// server-side; prune on the same cadence as noded.
	pruneTicker := time.NewTicker(60 * time.Minute)
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
			}
		}
	}()

	// Metrics persistence: same contract as noded — timestamped percentile
	// snapshots of every product histogram land in the SQL ledger every 60s
	// so hosted-mode latency trends are graphable across restarts.
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

	apiLn, err := net.Listen("tcp", cfg.Cell.ListenHTTP)
	if err != nil {
		return err
	}
	// Instrumentation sits INSIDE the limiter: shed traffic must not be
	// measured, or a flood mints label sets faster than it is dropped.
	cellHandler := bodyLimit(metrics.RateLimitMiddleware(metrics.NewRateLimiter(100, 200), remoteIPIdentity)(
		metrics.HTTPMiddleware(mux)))
	srv := &http.Server{
		Handler:           metrics.RequestIDMiddleware(cellHandler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("cell listening", "addr", cfg.Cell.ListenHTTP)
	return srv.Serve(apiLn)
}

// resolveNodeSecret picks the node-plane HMAC signing secret, failing closed
// when unset or too short: tokens under an empty key are forgeable by anyone.
func resolveNodeSecret() (string, error) {
	secret := os.Getenv("SUBSTRATE_NODE_SECRET")
	if secret == "" {
		secret = os.Getenv("SUBSTRATE_JWT_SECRET")
	}
	if secret == "" {
		return "", fmt.Errorf("SUBSTRATE_NODE_SECRET (or SUBSTRATE_JWT_SECRET) required: node tokens would be forgeable under an empty HMAC key")
	}
	if len(secret) < 32 && os.Getenv("SUBSTRATE_INSECURE_DEV") != "1" {
		return "", fmt.Errorf("node secret must be at least 32 characters (got %d); short HMAC keys are offline-forgeable", len(secret))
	}
	return secret, nil
}

// resolveNodeKey returns the cell→node credential, or "" when unset and the
// operator has not opted into insecure dev mode. A source-committed default
// here defeats the entire bearer model: anyone who reads the OSS repo can
// drive every node's API as the cell.
func resolveNodeKey() string {
	key := os.Getenv("SUBSTRATE_NODE_API_KEY")
	if key == "" {
		if os.Getenv("SUBSTRATE_INSECURE_DEV") == "1" {
			return "sk_node_internal"
		}
		return ""
	}
	return key
}

// remoteIPIdentity keys rate buckets on peer IP (cell endpoints are
// pre-auth; O(1) per request, bounded by distinct peers).
// forwardTarget builds the exact path+query the cell will send to the node.
//
// This is the whole computation the handler performs, in one testable place --
// deliberately, so a test exercises what actually runs rather than a helper the
// handler might not be using.
func forwardTarget(id string, u *url.URL) (string, bool) {
	target, ok := containedSandboxPath(id, u.Path)
	if !ok {
		return "", false
	}
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	return target, true
}

// containedSandboxPath cleans a proxied request path and confirms it still
// addresses the given sandbox. Returns the cleaned path to forward.
func containedSandboxPath(id, rawPath string) (string, bool) {
	base := "/v1/sandboxes/" + id
	cleaned := path.Clean(rawPath)
	if cleaned != base && !strings.HasPrefix(cleaned, base+"/") {
		return "", false
	}
	// A NUL or a stray control byte has no business in a forwarded path.
	for i := 0; i < len(cleaned); i++ {
		if cleaned[i] < 0x20 || cleaned[i] == 0x7f {
			return "", false
		}
	}
	return cleaned, true
}

func remoteIPIdentity(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return "ip_" + host
}

func bootstrapCell(ctx context.Context, st store.Store) error {
	orgs, err := st.ListOrgs(ctx)
	if err != nil || len(orgs) > 0 {
		return err
	}
	key := os.Getenv("SUBSTRATE_API_KEY")
	if key == "" {
		if os.Getenv("SUBSTRATE_INSECURE_DEV") != "1" {
			return fmt.Errorf("SUBSTRATE_API_KEY required (set SUBSTRATE_INSECURE_DEV=1 to allow the well-known dev key)")
		}
		key = "sk_cell_key"
	}
	now := time.Now()
	if err := st.CreateOrg(ctx, &store.Org{ID: "org_cell", Name: "cell", CreatedAt: now}); err != nil {
		return err
	}
	if err := st.CreateProject(ctx, &store.Project{ID: "prj_cell", OrgID: "org_cell", Name: "default", CreatedAt: now}); err != nil {
		return err
	}
	return st.CreateAPIKey(ctx, &store.APIKey{
		ID: "k_cell", OrgID: "org_cell", ProjectID: "prj_cell",
		Kind: "org", KeyHash: auth.HashKey(key), CreatedAt: now,
	})
}

// maxCellBody bounds every request body on the cell plane (proxy forwards,
// enrollment, heartbeats): an unbounded readAll was a memory-exhaustion DoS.
const maxCellBody = 8 << 20 // 8MiB

func bodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxCellBody)
		next.ServeHTTP(w, r)
	})
}

func jsonDecode(r *http.Request, v any) error {
	dec := jsonNewDecoder(r.Body)
	return dec.Decode(v)
}

// readAll drains a bounded request body. Errors (including the
// MaxBytesReader over-limit signal) are reported, never silently truncated.
func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

func writeResp(w http.ResponseWriter, status int, body []byte, err error) {
	if err != nil && status == 0 {
		status = 500
	}
	if status == 0 {
		status = 200
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if len(body) > 0 {
		w.Write(body)
		return
	}
	if err != nil {
		w.Write([]byte(`{"error":` + quote(err.Error()) + `}`))
	}
}

func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
