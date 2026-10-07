package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
)

// cellReporter enrolls this node with its control-plane cell and keeps
// heartbeats flowing (plan §5.3: nodes always dial out; the cell never
// connects in). Heartbeats carry capacity + snapshot-cache hints so the
// scheduler can place against reality.
type cellReporter struct {
	cfg  *config.Config
	log  *logging.Logger
	st   store.Store
	sup  *reconcile.Supervisor
	http *http.Client
}

func startCellReporter(ctx context.Context, cfg *config.Config, log *logging.Logger, st store.Store, sup *reconcile.Supervisor) error {
	cellURL := os.Getenv("SUBSTRATE_CELL_URL")
	if cellURL == "" {
		return fmt.Errorf("node mode requires SUBSTRATE_CELL_URL")
	}
	cr := &cellReporter{
		cfg: cfg, log: log.With("component", "cell-reporter"), st: st, sup: sup,
		http: &http.Client{Timeout: 10 * time.Second},
	}
	go cr.loop(ctx, strings.TrimRight(cellURL, "/"))
	return nil
}

func (cr *cellReporter) loop(ctx context.Context, cellURL string) {
	enrolled, nodeToken := cr.enroll(cellURL)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if !enrolled {
				enrolled, nodeToken = cr.enroll(cellURL)
				continue
			}
			if err := cr.heartbeat(ctx, cellURL, nodeToken); err != nil {
				if errors.Is(err, errDeregistered) {
					// The cell lost our registration (restart, eviction).
					// Without this reset the node logged a warning every 15s
					// forever and dropped off the scheduler until process
					// restart — silent fleet-capacity loss.
					cr.log.Warn("cell deregistered this node; re-enrolling")
					enrolled, nodeToken = false, ""
					continue
				}
				cr.log.Warn("heartbeat failed", "err", err.Error())
			}
		}
	}
}

var errDeregistered = errors.New("deregistered")

func (cr *cellReporter) enroll(cellURL string) (bool, string) {
	token := os.Getenv("SUBSTRATE_ENROLL_TOKEN")
	if token == "" {
		cr.log.Warn("no SUBSTRATE_ENROLL_TOKEN; cannot enroll yet")
		return false, ""
	}
	body, _ := json.Marshal(map[string]any{
		"node_id":        cr.cfg.Node.ID,
		"name":           cr.cfg.Node.Name,
		"region":         cr.cfg.Node.Region,
		"tier":           cr.sup.Tier(),
		"byo":            os.Getenv("SUBSTRATE_BYO") == "1",
		"api_addr":       cr.cfg.Node.AdvertiseAddr,
		"mem_total_mib":  cr.cfg.Node.MemTotalMiB,
		"snapshot_cache": cr.cacheHints(),
		"enroll_token":   token,
	})
	resp, err := cr.http.Post(cellURL+"/v1/node/enroll", "application/json", bytes.NewReader(body))
	if err != nil {
		cr.log.Warn("enroll failed", "err", err.Error())
		return false, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		cr.log.Warn("enroll rejected", "status", resp.StatusCode)
		return false, ""
	}
	var out struct {
		NodeToken string `json:"node_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.NodeToken == "" {
		// A 2xx without a token would leave us "enrolled" yet unable to
		// authenticate a single heartbeat.
		cr.log.Warn("enroll response missing node_token", "status", resp.StatusCode)
		return false, ""
	}
	cr.log.Info("enrolled with cell", "cell", cellURL)
	return true, out.NodeToken
}

func (cr *cellReporter) heartbeat(ctx context.Context, cellURL, nodeToken string) error {
	body, _ := json.Marshal(map[string]any{
		"node_id":        cr.cfg.Node.ID,
		"node_token":     nodeToken,
		"api_addr":       cr.cfg.Node.AdvertiseAddr,
		"mem_alloc_mib":  cr.memAllocated(ctx),
		"live_sandboxes": cr.sup.LiveCount(),
		"snapshot_cache": cr.cacheHints(),
	})
	resp, err := cr.http.Post(cellURL+"/v1/node/heartbeat", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 404 {
		return errDeregistered
	}
	return nil
}

// memAllocated totals configured memory over the sandboxes actually live on
// this node. It used to full-scan the sandbox table every 15s and filter in Go
// — 100k rows materialised to produce one integer. The live set is already in
// memory, so ask the store only about those: O(live), not O(defined).
func (cr *cellReporter) memAllocated(ctx context.Context) int {
	live := cr.sup.LiveSandboxes()
	if len(live) == 0 {
		return 0
	}
	total, err := cr.st.SumMemMiB(ctx, live)
	if err != nil {
		return 0
	}
	return total
}

// cacheHints names the templates this node wants kept warm. Previously a
// second full table scan on the same 15s beat; now one DISTINCT query.
func (cr *cellReporter) cacheHints() []string {
	ids, err := cr.st.DistinctTemplateIDs(context.Background())
	if err != nil {
		return []string{}
	}
	if ids == nil {
		return []string{}
	}
	return ids
}
