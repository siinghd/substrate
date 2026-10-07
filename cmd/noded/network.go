package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/netns"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// netnsProvider adapts internal/netns to reconcile.Network.
//
// This adapter is the piece that never existed. `internal/netns` was written,
// tested against a real kernel, and then wired to nothing: every call site
// passed a nil *vmm.NetInfo, so every sandbox on every tier ran with no
// network AND with none of the egress policy that package programs -- no IMDS
// drop, no peer isolation, no rate limit. The policy was real and inert.
type netnsProvider struct {
	mgr     *netns.Manager
	nsDir   string
	withTap bool
	log     *logging.Logger
}

// newNetnsProvider builds the provider for a driver tier, or returns nil when
// networking is disabled.
func newNetnsProvider(cfg *config.Config, log *logging.Logger) (*netnsProvider, error) {
	// The process tier is not an isolation boundary -- guest code runs as the
	// node agent's own user on the host, sharing its network stack. A
	// per-sandbox namespace there isolates nothing that is not already shared,
	// and programming one needs root, which dev boxes and CI do not have. So
	// this tier gets no networking and says so, rather than refusing to boot.
	if cfg.Driver.Name == "process" {
		log.Info("process tier: sandboxes share the host network (dev only, not an isolation boundary)")
		return nil, nil
	}
	if !cfg.Network.Enabled || cfg.Network.NodeBlockCIDR == "" {
		// Explicit opt-out. Loud, because a sandbox with no network cannot
		// install a package or call an API, and that is very hard to diagnose
		// from inside one.
		log.Warn("sandbox networking is DISABLED by config; sandboxes will have NO network",
			"driver", cfg.Driver.Name)
		return nil, nil
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf(
			"sandbox networking needs root to create namespaces and program nftables (running as uid %d); "+
				"run noded as root, or set network.enabled=false to run sandboxes with no network",
			os.Geteuid())
	}
	// Only the hv tier wants a tap: a Firecracker guest attaches its
	// virtio-net device to one. gVisor attaches netstack straight to the veth,
	// and a second interface in the namespace makes its choice ambiguous.
	withTap := cfg.Driver.Name == "firecracker"
	mgr, err := netns.NewManagerWithOptions(cfg.Network.NodeBlockCIDR, netns.Options{
		WithTap: withTap,
		MTU:     cfg.Network.MTU,
		Uplink:  cfg.Network.UplinkInterface,
	})
	if err != nil {
		return nil, err
	}
	// Node-wide egress: ip_forward, NAT for the block, and a FORWARD accept.
	// Done once, here, because without it a sandbox can ping and nothing else
	// -- DNS and every TCP connection time out against the host's own DROP
	// policy, which is what ufw and Docker both leave behind.
	if err := mgr.EnsureNodeEgress(); err != nil {
		return nil, fmt.Errorf("program node egress: %w", err)
	}
	if err := mgr.EnsureBaseRuleset(); err != nil {
		return nil, fmt.Errorf("program base ruleset: %w", err)
	}
	capacity, err := netns.BlockCapacity(cfg.Network.NodeBlockCIDR)
	if err != nil {
		return nil, err
	}
	log.Info("sandbox networking programmed",
		"block", cfg.Network.NodeBlockCIDR, "capacity_sandboxes", capacity, "tap", withTap)
	return &netnsProvider{mgr: mgr, nsDir: "/var/run/netns", withTap: withTap, log: log}, nil
}

func (p *netnsProvider) Setup(sandboxID string) (*vmm.NetInfo, error) {
	n, err := p.mgr.Setup(sandboxID)
	if err != nil {
		return nil, err
	}
	return &vmm.NetInfo{
		NSName: n.NSName,
		// The path is what a driver that JOINS the namespace needs; the name
		// alone is only useful to `ip netns exec`.
		NSPath:    filepath.Join(p.nsDir, n.NSName),
		TapName:   n.TapName,
		GuestIP:   n.GuestIP.String(),
		GatewayIP: n.GatewayIP.String(),
	}, nil
}

func (p *netnsProvider) Teardown(sandboxID string) error { return p.mgr.Teardown(sandboxID) }

// GuestIP resolves a live sandbox's own address, for the preview gateway.
//
// Without this the gateway has no way to reach an isolated sandbox: its
// resolver refuses to route rather than falling back to host loopback (which
// would point every tenant's preview URL at the node itself). So previews were
// refused for the hv and kernel tiers -- you could run a web server in a
// sandbox and nothing could ever reach it.
func (p *netnsProvider) GuestIP(sandboxID string) (string, bool) {
	n, ok := p.mgr.Get(sandboxID)
	if !ok || n.GuestIP == nil {
		return "", false
	}
	return n.GuestIP.String(), true
}
