// Package netns implements per-sandbox networking (plan §4): one network
// namespace per sandbox holding exactly one tap; point-to-point routed /32s
// allocated from the node's RFC6598 block; no shared L2 bridge. The metadata
// endpoint drop is the first rule programmed, always (§4.2).
package netns

import (
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/substrate-sh/substrate/internal/ids"
)

// Allocator hands out point-to-point /30 SUBNETS from the node's block.
//
// It used to hand out single /32 addresses, on the assumption that a routed
// /32 with proxy_arp is enough. It is not, on the tier this node actually
// runs: gVisor's netstack cannot source-select from a /32, so a sandbox with
// one gets ICMP but answers "Network unreachable" to every TCP connect and UDP
// send. Verified on a live sandbox before this was changed.
//
// A /30 spends four addresses per sandbox (network, host, guest, broadcast),
// so a block must be sized N*4. See BlockCapacity.
type Allocator struct {
	mu    sync.Mutex
	base  net.IP
	slots int // number of /30s in the block
	used  []bool
	next  int
}

// Subnet is one sandbox's point-to-point link.
type Subnet struct {
	Network net.IP // .0 -- the /30 itself
	HostIP  net.IP // .1 -- host end of the veth, the guest's default gateway
	GuestIP net.IP // .2 -- inside the netns
}

// CIDR renders the /30 for NAT and firewall rules.
func (s Subnet) CIDR() string { return s.Network.String() + "/30" }

func NewAllocator(cidr string) (*Allocator, error) {
	_, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("node block: %w", err)
	}
	ones, bits := ipnet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("only IPv4 blocks supported")
	}
	size := 1 << (bits - ones)
	if size < 16 {
		return nil, fmt.Errorf("block too small: %s", cidr)
	}
	// One /30 per sandbox. The first is skipped so the block's own network
	// address is never handed to a tenant.
	slots := size / 4
	return &Allocator{base: ipnet.IP, slots: slots, used: make([]bool, slots)}, nil
}

// BlockCapacity reports how many sandboxes a block can address, which is a
// quarter of its addresses. The default /22 holds 255 -- far short of the
// plan's 2,000-resident target, which needs a /19. Surfaced as a number rather
// than left for an operator to discover at allocation #256.
func BlockCapacity(cidr string) (int, error) {
	a, err := NewAllocator(cidr)
	if err != nil {
		return 0, err
	}
	return a.slots - 1, nil
}

// Allocate returns the next free /30. Slot 0 is reserved so the block's own
// network address is never issued.
func (a *Allocator) Allocate() (Subnet, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := 0; i < a.slots; i++ {
		idx := (a.next + i) % a.slots
		if idx == 0 || a.used[idx] {
			continue
		}
		a.used[idx] = true
		a.next = (idx + 1) % a.slots
		// Full 32-bit add: a per-octet carry truncates for blocks wider than
		// /16 -- and the plan's block is 100.64/10 -- so allocation #65536
		// silently collided with #1.
		net0 := ipToU32(a.base) + uint32(idx)*4
		return Subnet{
			Network: u32ToIP(net0),
			HostIP:  u32ToIP(net0 + 1),
			GuestIP: u32ToIP(net0 + 2),
		}, nil
	}
	return Subnet{}, fmt.Errorf("address pool exhausted (%d sandboxes); size the node block at 4 addresses per sandbox", a.slots-1)
}

func ipToU32(ip net.IP) uint32 { return binary.BigEndian.Uint32(ip.To4()) }
func u32ToIP(v uint32) net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, v)
	return ip
}

// Release frees the /30 containing ip. Takes any address in the subnet so a
// caller holding either end can hand it back.
func (a *Allocator) Release(ip net.IP) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v4 := ip.To4()
	if v4 == nil {
		return
	}
	idx := int(ipToU32(v4)-ipToU32(a.base)) / 4
	if idx > 0 && idx < a.slots {
		a.used[idx] = false
	}
}

func (a *Allocator) InBlock(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	v := int(ipToU32(v4))
	base := int(ipToU32(a.base))
	return v >= base && v < base+a.slots*4
}

func (a *Allocator) Used() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, u := range a.used {
		if u {
			n++
		}
	}
	return n
}

// Runner executes system commands; swapped for a recorder in tests and
// dry-run mode so unprivileged hosts can run the full stack.
type Runner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(name string, args ...string) error { return runCmd(name, args...) }
func (execRunner) Output(name string, args ...string) ([]byte, error) {
	return outputCmd(name, args...)
}

// Manager programs sandbox networks.
type Manager struct {
	run       Runner
	alloc     *Allocator
	blockCIDR string
	dryRun    bool
	withTap   bool
	mtu       int
	uplink    string
	natReady  bool
	mu        sync.Mutex
	sandboxes map[string]*SandboxNet
}

type SandboxNet struct {
	ID        string
	Subnet    Subnet
	GuestIP   net.IP
	TapName   string
	NSName    string
	VethHost  string
	VethPeer  string
	GatewayIP net.IP
}

// Options configure a Manager. Zero values are sane for the kernel tier.
type Options struct {
	// WithTap creates a tap inside each namespace. Required by the hv
	// (Firecracker) tier, wrong for the kernel tier -- runsc attaches netstack
	// to whatever it finds in the namespace and two interfaces is ambiguous.
	WithTap bool
	// MTU for both ends of the veth. 0 means 1500.
	MTU int
	// Uplink is the host interface egress is masqueraded out of. Empty means
	// "discover the default route's interface".
	Uplink string
	DryRun bool
}

func NewManager(blockCIDR string, dryRun bool) (*Manager, error) {
	return NewManagerWithOptions(blockCIDR, Options{DryRun: dryRun, WithTap: true})
}

func NewManagerWithOptions(blockCIDR string, o Options) (*Manager, error) {
	alloc, err := NewAllocator(blockCIDR)
	if err != nil {
		return nil, err
	}
	var r Runner = execRunner{}
	if o.DryRun {
		r = NewRecorder()
	}
	if o.MTU <= 0 {
		o.MTU = 1500
	}
	return &Manager{
		run:       r,
		alloc:     alloc,
		blockCIDR: blockCIDR,
		dryRun:    o.DryRun,
		withTap:   o.WithTap,
		mtu:       o.MTU,
		uplink:    o.Uplink,
		sandboxes: map[string]*SandboxNet{},
	}, nil
}

// EnsureNodeEgress programs the node-wide egress path exactly once.
//
// None of this existed. The result was a network that looked healthy and was
// not: ICMP crossed (the host's own rules let it), while DNS and every TCP
// connection timed out. A sandbox that can ping but cannot resolve is worse
// than one with no network, because the operator believes it works.
//
// Three separate things were missing:
//
//  1. ip_forward. Off by default on most hosts; without it the veth is a
//     dead end.
//  2. NAT. Sandboxes carry RFC6598 addresses, which are not routable on the
//     internet. Nothing anywhere masqueraded them, so no reply could ever
//     come back.
//  3. A FORWARD accept. On any host running ufw or Docker the filter FORWARD
//     policy is DROP -- measured on this one -- and substrate's own nftables
//     table cannot override it: every base chain on a hook runs, so one
//     table's `policy accept` does not undo another's drop. The accept has to
//     be programmed into the chain that is dropping.
func (m *Manager) EnsureNodeEgress() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.natReady {
		return nil
	}
	uplink := m.uplink
	if uplink == "" {
		var err error
		if uplink, err = m.defaultUplink(); err != nil {
			return err
		}
		m.uplink = uplink
	}
	if err := m.run.Run("sysctl", "-qw", "net.ipv4.ip_forward=1"); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	steps := [][]string{
		{"-t", "nat", "-C", "POSTROUTING", "-s", m.blockCIDR, "-o", uplink, "-j", "MASQUERADE"},
		{"-t", "nat", "-A", "POSTROUTING", "-s", m.blockCIDR, "-o", uplink, "-j", "MASQUERADE"},
		{"-C", "FORWARD", "-s", m.blockCIDR, "-j", "ACCEPT"},
		{"-I", "FORWARD", "1", "-s", m.blockCIDR, "-j", "ACCEPT"},
		{"-C", "FORWARD", "-d", m.blockCIDR, "-j", "ACCEPT"},
		{"-I", "FORWARD", "1", "-d", m.blockCIDR, "-j", "ACCEPT"},
	}
	// Each pair is check-then-add: -C succeeds when the rule is already there,
	// which makes this idempotent across node restarts instead of stacking a
	// duplicate rule on every boot.
	for i := 0; i < len(steps); i += 2 {
		if err := m.run.Run("iptables", steps[i]...); err == nil {
			continue // already present
		}
		if err := m.run.Run("iptables", steps[i+1]...); err != nil {
			return fmt.Errorf("program egress rule %v: %w", steps[i+1], err)
		}
	}
	m.natReady = true
	return nil
}

// EnsureBaseRuleset programs the node-wide nftables table exactly once.
//
// BaseRuleset() has always returned the correct rules -- IMDS drop first, both
// address families, the dispatch map -- and nothing anywhere ever ran them. The
// per-sandbox chains jump from a map in a table that did not exist, so the
// entire egress policy was unreachable even after this package was wired in.
//
// Idempotent: `nft add` on an existing table or chain is a no-op, and the
// per-rule adds are re-run on every node boot, which is the same behaviour a
// systemd unit would give.
func (m *Manager) EnsureBaseRuleset() error {
	for _, rule := range BaseRuleset() {
		if err := m.run.Run("nft", rule...); err != nil {
			return fmt.Errorf("nft %v: %w", rule, err)
		}
	}
	return nil
}

// defaultUplink reports the interface the default route uses.
func (m *Manager) defaultUplink() (string, error) {
	out, err := m.run.Output("ip", "route", "show", "default")
	if err != nil {
		return "", fmt.Errorf("find uplink: %w", err)
	}
	f := strings.Fields(string(out))
	for i := 0; i < len(f)-1; i++ {
		if f[i] == "dev" {
			return f[i+1], nil
		}
	}
	if m.dryRun {
		return "eth0", nil
	}
	return "", fmt.Errorf("no default route: set network.uplink_interface")
}

const linkLocalGateway = "169.254.0.1"

// Setup creates the netns + tap + veth + routes + nftables policy for one
// sandbox. Idempotent-ish: fails loudly if the namespace already exists.
func (m *Manager) Setup(sandboxID string) (*SandboxNet, error) {
	if !ids.ValidSandboxID(sandboxID) {
		return nil, fmt.Errorf("refusing hostile sandbox id %q", sandboxID)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.sandboxes[sandboxID]; ok {
		return existing, nil
	}
	if m.mtu <= 0 {
		m.mtu = 1500
	}
	sub, err := m.alloc.Allocate()
	if err != nil {
		return nil, err
	}
	n := &SandboxNet{
		ID:        sandboxID,
		Subnet:    sub,
		GuestIP:   sub.GuestIP,
		TapName:   tapName(sandboxID),
		NSName:    "sb-" + sandboxID,
		VethHost:  "vb" + shortID(sandboxID),
		VethPeer:  "vp" + shortID(sandboxID),
		GatewayIP: sub.HostIP,
	}

	// Everything created below is torn down together if ANY step fails.
	// Setup's error path used to release only the IP address, leaving the
	// netns, the tap, the veth pair and the host route behind — and there is
	// no orphan sweeper anywhere, so a node that hit a transient failure
	// accumulated dead namespaces until interface names or the address block
	// ran out.
	created := false
	defer func() {
		if !created {
			m.cleanupPartial(n)
		}
	}()

	// The point-to-point link. A /30 per sandbox, NOT a routed /32.
	//
	// The /32 form is more frugal and does not work: gVisor's netstack cannot
	// source-select from a /32, so the sandbox gets ICMP but answers "Network
	// unreachable" to every TCP connect and UDP send. Confirmed on a live
	// sandbox. A /30 gives netstack a real subnet to bind against.
	steps := [][]string{
		{"ip", "netns", "add", n.NSName},
		{"ip", "link", "add", n.VethHost, "type", "veth", "peer", "name", n.VethPeer},
		{"ip", "link", "set", n.VethPeer, "netns", n.NSName},
		{"ip", "addr", "add", sub.HostIP.String() + "/30", "dev", n.VethHost},
		{"ip", "link", "set", n.VethHost, "mtu", strconv.Itoa(m.mtu)},
		{"ip", "link", "set", n.VethHost, "up"},
		{"ip", "-n", n.NSName, "link", "set", "lo", "up"},
		{"ip", "-n", n.NSName, "addr", "add", sub.GuestIP.String() + "/30", "dev", n.VethPeer},
		{"ip", "-n", n.NSName, "link", "set", n.VethPeer, "mtu", strconv.Itoa(m.mtu)},
		{"ip", "-n", n.NSName, "link", "set", n.VethPeer, "up"},
		{"ip", "-n", n.NSName, "route", "add", "default", "via", sub.HostIP.String()},
	}
	// The tap is only for the hv tier: a Firecracker guest needs a tap to
	// attach its virtio-net device to. The kernel (gVisor) tier attaches
	// netstack straight to the veth and must NOT get one -- runsc would find
	// two interfaces and pick unpredictably.
	if m.withTap {
		steps = append(steps,
			// `ip link add <name> type tap` is NOT valid -- iproute2 answers
			// "Unknown device type" -- so sandbox networking failed on its
			// very first step on any real host. Taps are created with
			// `ip tuntap`.
			[]string{"ip", "-n", n.NSName, "tuntap", "add", "dev", n.TapName, "mode", "tap"},
			[]string{"ip", "-n", n.NSName, "link", "set", n.TapName, "mtu", strconv.Itoa(m.mtu)},
			[]string{"ip", "-n", n.NSName, "addr", "add", linkLocalGateway + "/30", "dev", n.TapName},
			[]string{"ip", "-n", n.NSName, "link", "set", n.TapName, "up"},
			// Route the guest's own address at the tap and forward inside the
			// namespace, or nothing the VM sends can reach the veth uplink.
			[]string{"ip", "netns", "exec", n.NSName, "sysctl", "-qw", "net.ipv4.ip_forward=1"},
		)
	}

	for _, c := range steps {
		if err := m.run.Run(c[0], c[1:]...); err != nil {
			return nil, fmt.Errorf("%v: %w", c, err)
		}
	}

	// Policy is programmed against the HOST-SIDE veth: the tap lives inside
	// the sandbox's netns, so a host-table iifname match on it never fires.
	for _, rule := range EgressRules(n.VethHost, sub.GuestIP.String(), m.blockCIDR) {
		if err := m.run.Run("nft", rule...); err != nil {
			return nil, fmt.Errorf("nft rule %v: %w", rule, err)
		}
	}

	created = true
	m.sandboxes[sandboxID] = n
	return n, nil
}

// cleanupPartial undoes whatever Setup managed to create. Every step is
// best-effort: the namespace may not exist, the route may not have been added.
func (m *Manager) cleanupPartial(n *SandboxNet) {
	for _, rule := range TeardownRules(n.VethHost) {
		_ = m.run.Run("nft", rule...)
	}
	_ = m.run.Run("ip", "link", "del", n.VethHost)
	_ = m.run.Run("ip", "netns", "del", n.NSName)
	m.alloc.Release(n.GuestIP)
}

func (m *Manager) Teardown(sandboxID string) error {
	if !ids.ValidSandboxID(sandboxID) {
		return fmt.Errorf("refusing hostile sandbox id %q", sandboxID)
	}
	m.mu.Lock()
	n, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	delete(m.sandboxes, sandboxID)
	m.mu.Unlock()

	// Unhook the policy FIRST. Per-sandbox rules used to be appended straight
	// into the shared forward chain and never removed, so the chain grew for
	// the life of the node and a later sandbox reusing an interface name
	// inherited a dead tenant's rules.
	for _, rule := range TeardownRules(n.VethHost) {
		_ = m.run.Run("nft", rule...)
	}
	if m.withTap {
		_ = m.run.Run("ip", "-n", n.NSName, "link", "del", n.TapName)
	}
	// Deleting the namespace takes the veth peer with it, and a veth pair dies
	// together — but delete the host end explicitly too, in case the namespace
	// was already gone.
	_ = m.run.Run("ip", "link", "del", n.VethHost)
	err := m.run.Run("ip", "netns", "del", n.NSName)
	m.alloc.Release(n.GuestIP)
	return err
}

func (m *Manager) Get(sandboxID string) (*SandboxNet, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, ok := m.sandboxes[sandboxID]
	return n, ok
}

func tapName(id string) string { return "tap" + shortID(id) }

func shortID(id string) string {
	if len(id) > 8 {
		id = id[len(id)-8:]
	}
	return id
}
