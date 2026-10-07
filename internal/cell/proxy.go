// Cell API: the same v1 surface, but Create schedules onto nodes and every
// sandbox-scoped call proxies to the owning node over its node-API address.
package cell

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/substrate-sh/substrate/internal/ids"
	"github.com/substrate-sh/substrate/internal/metrics"
)

type Proxy struct {
	Registry *Registry
	HTTP     *http.Client
}

func NewProxy(reg *Registry) *Proxy {
	return &Proxy{
		Registry: reg,
		HTTP: &http.Client{
			Timeout: 30 * time.Second,
			// NEVER follow a redirect. The cell forwards with a privileged
			// node credential, and Go re-sends Authorization on a same-host
			// redirect: a node-side ServeMux 301 (which any path the mux
			// cleans produces) would carry that credential to a path the cell
			// never authorized. The caller is told about the redirect instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// ErrNotOwner is returned when a caller asks the cell to act on a sandbox
// belonging to a different tenant.
var ErrNotOwner = errors.New("no such sandbox")

// Forward sends a request to the node owning the sandbox, preserving method,
// path and body. The cell's credential is exchanged for the node's.
//
// TENANCY IS ENFORCED HERE AND NOWHERE ELSE. The cell authenticates the caller
// and then speaks to the node with a single privileged node credential, so
// every tenant's traffic reaches the node as the same principal and every
// node-side org check passes. Routing on sandbox id alone therefore handed any
// authenticated key full read/write/destroy over every other tenant's
// sandboxes. callerOrg must be the authenticated org of the request.
func (p *Proxy) Forward(ctx context.Context, callerOrg, sandboxID, method, path string, body []byte, nodeKey string) ([]byte, int, error) {
	// Sandbox ids become URL path segments toward the node API; only
	// platform-generated shapes are ever forwarded (blocks %2F and
	// dot-segment tricks riding in the id).
	if !ids.ValidSandboxID(sandboxID) {
		return nil, 404, fmt.Errorf("no such sandbox")
	}
	nodeID, ownerOrg, ok := p.Registry.PlacementOwner(ctx, sandboxID)
	if !ok {
		return nil, 0, fmt.Errorf("no placement recorded for %s", sandboxID)
	}
	// Answer 404, not 403: confirming that someone else's sandbox exists is
	// itself a cross-tenant disclosure (ids are enumerable).
	if callerOrg == "" || ownerOrg == "" || callerOrg != ownerOrg {
		metrics.IncCellCrossTenantDenied()
		return nil, http.StatusNotFound, ErrNotOwner
	}
	node, err := p.Registry.Get(ctx, nodeID)
	if err != nil {
		return nil, 0, fmt.Errorf("node %s unknown: %w", nodeID, err)
	}
	url := "http://" + node.APIAddr + path
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+nodeKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, 502, fmt.Errorf("node unreachable: %w", err)
	}
	defer resp.Body.Close()
	out, err := readBounded(resp.Body, 64<<20)
	if err != nil {
		// A truncated or failed read must NEVER masquerade as a complete
		// upstream response (silent data corruption).
		return nil, 502, fmt.Errorf("node response unreadable: %w", err)
	}
	return out, resp.StatusCode, nil
}

// readBounded drains r up to limit bytes; one extra byte proves overflow so
// silent truncation cannot pass as success. O(body) time, bounded memory via
// the reader's buffering.
func readBounded(r io.Reader, limit int64) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > limit {
		return nil, fmt.Errorf("response exceeds %d byte limit", limit)
	}
	return out, nil
}

// SchedulerCreate is the create path in cell mode: schedule → forward →
// record placement. On node failure the placement is dropped so retries can
// land elsewhere (level-triggered recovery).
func (p *Proxy) SchedulerCreate(ctx context.Context, reg *Registry, body []byte, orgID, nodeKey string) ([]byte, int, error) {
	placeStart := time.Now()
	var req struct {
		TemplateID string `json:"template_id"`
		Region     string `json:"region"`
		MemMiB     int    `json:"mem_mib"`
		Tier       string `json:"isolation"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &req); err != nil {
			// Garbage must not silently provision defaults.
			return nil, http.StatusBadRequest, fmt.Errorf("malformed create body: %w", err)
		}
	}
	if req.Tier == "" {
		req.Tier = "hv"
	}
	if req.MemMiB == 0 {
		req.MemMiB = 512
	}

	node, err := reg.Place(ctx, PlacementRequest{
		Region: req.Region, Tier: req.Tier, OrgID: orgID,
		TemplateID: req.TemplateID, MemMiB: req.MemMiB,
	})
	if err != nil {
		return nil, http.StatusServiceUnavailable, err
	}

	url := "http://" + node.APIAddr + "/v1/sandboxes"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, 500, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+nodeKey)
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Idempotency-Key", ids.Idempotency())
	resp, err := p.HTTP.Do(httpReq)
	if err != nil {
		return nil, 502, fmt.Errorf("node unreachable: %w", err)
	}
	out, err := readBounded(resp.Body, 16<<20)
	resp.Body.Close()
	if err != nil {
		return nil, 502, fmt.Errorf("node create response unreadable: %w", err)
	}
	if resp.StatusCode >= 400 {
		return out, resp.StatusCode, fmt.Errorf("node create failed: %d", resp.StatusCode)
	}

	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out, &created); err != nil || created.ID == "" {
		// A 2xx without a usable id leaves a sandbox the cell can never
		// route; that is a node-side failure, not a success.
		return out, http.StatusBadGateway, fmt.Errorf("node returned no sandbox id")
	}
	if err := reg.AssignPlacement(ctx, created.ID, node.ID, orgID); err != nil {
		return out, resp.StatusCode, fmt.Errorf("placement persist failed: %w", err)
	}
	metrics.ObserveStage("cell.create", "placement", time.Since(placeStart))
	return out, resp.StatusCode, nil
}

var _ = strings.TrimSpace
