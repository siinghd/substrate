package main

import (
	"context"
	"net"
	"strconv"
	"strings"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/gateway"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/store"
)

// storeResolver turns a preview hostname into a proxy target.
//
// SECURITY: the target must be an address THE PLATFORM assigned to that
// sandbox — never one derived from a tenant-supplied port number alone. The
// previous version returned "127.0.0.1:<tenant's port>" for every tier, so a
// customer could expose port 8080 and reach the node's OWN API (and its
// unauthenticated /metrics, and any other service bound on the node) from the
// open internet. It also gave two sandboxes on the same port the same target.
type storeResolver struct {
	st  store.Store
	cfg *config.Config
	log *logging.Logger

	// net resolves a sandbox's own guest address. Required for the isolated
	// tiers; when it cannot answer, there is NO route — falling back to the
	// node's loopback is the bug this whole type exists to prevent.
	net func(sandboxID string) (string, bool)

	// reservedPorts are ports belonging to the node itself. Never proxy to
	// them, whoever asks.
	reservedPorts map[int]struct{}
}

func newStoreResolver(st store.Store, cfg *config.Config, log *logging.Logger,
	guestIP func(string) (string, bool)) *storeResolver {
	return &storeResolver{st: st, cfg: cfg, log: log, net: guestIP,
		reservedPorts: nodeReservedPorts(cfg)}
}

// nodeReservedPorts collects the ports this process listens on. A preview
// target pointing at one of them is an SSRF into our own control surface.
func nodeReservedPorts(cfg *config.Config) map[int]struct{} {
	out := map[int]struct{}{}
	for _, addr := range []string{cfg.API.Listen, cfg.Gateway.Listen, cfg.Cell.ListenHTTP, cfg.Cell.ListenNodes} {
		if p, ok := portOf(addr); ok {
			out[p] = struct{}{}
		}
	}
	return out
}

func portOf(addr string) (int, bool) {
	if addr == "" {
		return 0, false
	}
	if !strings.Contains(addr, ":") {
		return 0, false
	}
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return 0, false
	}
	return p, true
}

func (r *storeResolver) Resolve(ctx context.Context, sandboxID string, port int) (*gateway.Route, bool) {
	sb, err := r.st.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, false
	}
	ports, err := r.st.ListPorts(ctx, sandboxID)
	if err != nil {
		return nil, false
	}
	for _, p := range ports {
		if p.Port != port || p.Proto != "tcp" {
			continue
		}
		target, ok := r.targetFor(sb, port)
		if !ok {
			return nil, false
		}
		return &gateway.Route{
			SandboxID: sandboxID, Port: port, Target: target,
			Public: p.Public, Token: p.Token, Tier: sb.Tier, State: string(sb.State),
		}, true
	}
	return nil, false
}

// targetFor resolves the address to proxy to, failing closed.
func (r *storeResolver) targetFor(sb *store.Sandbox, port int) (string, bool) {
	if _, reserved := r.reservedPorts[port]; reserved {
		// A tenant asked us to proxy the internet into one of our own
		// listeners. Refuse regardless of tier.
		if r.log != nil {
			r.log.Warn("refused preview to a node-reserved port",
				"sandbox", sb.ID, "port", port)
		}
		return "", false
	}

	switch sb.Tier {
	case "hv", "kernel":
		// Isolated tiers have their own address. No address, no route: the
		// old loopback fallback pointed every hv preview at the host.
		if r.net == nil {
			if r.log != nil {
				r.log.Warn("no per-sandbox network resolver; refusing preview for isolated tier",
					"sandbox", sb.ID, "tier", sb.Tier)
			}
			return "", false
		}
		ip, ok := r.net(sb.ID)
		if !ok || ip == "" {
			return "", false
		}
		return net.JoinHostPort(ip, strconv.Itoa(port)), true

	default:
		// Process tier: the "guest" is a host process, so its listening
		// socket really is on host loopback and there is no per-sandbox
		// address to target. That means preview URLs cannot be isolated
		// between process-tier sandboxes, so they are opt-in.
		if !r.cfg.Gateway.AllowProcessTierPreviews {
			if r.log != nil {
				r.log.Warn("process-tier previews are disabled; refusing",
					"sandbox", sb.ID, "port", port)
			}
			return "", false
		}
		return net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), true
	}
}
