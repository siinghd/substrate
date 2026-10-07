// Package config loads noded / control-plane configuration from YAML plus
// environment overrides. Secrets never live in the file; they arrive via env.
package config

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mode string `yaml:"mode"` // standalone | node | cell

	Node      NodeConfig     `yaml:"node"`
	API       APIConfig      `yaml:"api"`
	Gateway   GatewayConfig  `yaml:"gateway"`
	Driver    DriverConfig   `yaml:"driver"`
	Network   NetworkConfig  `yaml:"network"`
	Snapshots SnapshotConfig `yaml:"snapshots"`
	Cell      CellConfig     `yaml:"cell"`
	Limits    LimitsConfig   `yaml:"limits"`
	Log       LogConfig      `yaml:"log"`
	Backup    BackupConfig   `yaml:"backup"`
	Billing   BillingConfig  `yaml:"billing"`
}

type NodeConfig struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	Region        string `yaml:"region"`
	DataDir       string `yaml:"data_dir"`
	JailDir       string `yaml:"jail_dir"`
	CacheDir      string `yaml:"cache_dir"`
	AdvertiseAddr string `yaml:"advertise_addr"`
	MemTotalMiB   int    `yaml:"mem_total_mib"`
}

type APIConfig struct {
	Listen        string `yaml:"listen"`
	PublicBaseURL string `yaml:"public_base_url"`
	JWTSecret     string `yaml:"-"` // SUBSTRATE_JWT_SECRET
	// AdminKey gates tenant provisioning (/v1/admin/*). Environment only,
	// never YAML and never the database: a database compromise must not yield
	// the ability to mint tenants. Empty disables the admin API entirely.
	AdminKey string `yaml:"-"` // SUBSTRATE_ADMIN_KEY
}

type GatewayConfig struct {
	Enabled    bool   `yaml:"enabled"`
	Listen     string `yaml:"listen"`
	BaseDomain string `yaml:"base_domain"` // e.g. run.localhost:8080 or {port}-{sb}.run.example.dev
	// AllowProcessTierPreviews opts into preview URLs for the process tier.
	// OFF by default: a process-tier sandbox is a host process, so its ports
	// live on the node's own loopback with no per-sandbox address. Proxying
	// there means two sandboxes on the same port share one target, and a
	// tenant-chosen port can reach node-local services. Only enable on a
	// single-tenant dev box.
	AllowProcessTierPreviews bool   `yaml:"allow_process_tier_previews"`
	TLSCert                  string `yaml:"tls_cert"`
	TLSKey                   string `yaml:"tls_key"`
}

type DriverConfig struct {
	Name           string `yaml:"name"` // firecracker | process
	FirecrackerBin string `yaml:"firecracker_bin"`
	JailerBin      string `yaml:"jailer_bin"`
	KernelImage    string `yaml:"kernel_image"`
	AllowProcess   bool   `yaml:"allow_process"`
	// RunscBin is the gVisor runtime for the tier-`kernel` driver. gVisor's
	// systrap platform needs no /dev/kvm, which is the whole point of the
	// tier: it is the only real isolation boundary available on the large
	// class of servers that cannot run Firecracker.
	RunscBin string `yaml:"runsc_bin"`
	// GvisorPlatform selects gVisor's platform. Leave empty for systrap
	// (no KVM required); "kvm" is faster where /dev/kvm exists.
	GvisorPlatform string `yaml:"gvisor_platform"`
	// GvisorSockDir holds per-sandbox agent sockets. Kept short and separate
	// from the data dir because a unix socket path is capped near 108 bytes.
	GvisorSockDir string `yaml:"gvisor_sock_dir"`
	// GvisorOverlaySizeMiB caps how much one sandbox may write. Guest writes
	// go to a private overlay, never to the shared template; without a cap one
	// guest filled the node's data volume for every other tenant on it.
	GvisorOverlaySizeMiB int `yaml:"gvisor_overlay_size_mib"`
}

type NetworkConfig struct {
	NodeBlockCIDR string `yaml:"node_block_cidr"` // RFC6598 /22 for this node, e.g. 100.64.0.0/22
	MTU           int    `yaml:"mtu"`
	EgressPolicy  string `yaml:"egress_policy"` // open (default) | allowlist
	// UplinkInterface is the host NIC sandbox egress is masqueraded out of.
	// Empty means "the interface the default route uses".
	UplinkInterface string `yaml:"uplink_interface"`
	// Enabled turns per-sandbox networking on. Off means sandboxes get no
	// network at all -- which is what every deployment silently had before the
	// netns layer was wired into noded.
	Enabled       bool `yaml:"enabled"`
	BandwidthMbit int  `yaml:"bandwidth_mbit"`
}

type SnapshotConfig struct {
	ChunkSizeKB    int    `yaml:"chunk_size_kb"`
	CacheSizeGB    int    `yaml:"cache_size_gb"`
	ObjectStoreURL string `yaml:"object_store_url"`
}

type CellConfig struct {
	DatabaseURL string `yaml:"database_url"` // postgres://...
	ListenHTTP  string `yaml:"listen_http"`
	ListenNodes string `yaml:"listen_nodes"` // mTLS tunnel listener for nodes
	TLSCert     string `yaml:"tls_cert"`
	TLSKey      string `yaml:"tls_key"`
	EnrollToken string `yaml:"-"`
}

type LimitsConfig struct {
	MaxSandboxesPerProject int `yaml:"max_sandboxes_per_project"`
	MaxSandboxesPerUser    int `yaml:"max_sandboxes_per_user"`
	// MaxMemMiB / MaxVCPUs cap what a caller may request. Both reach a real
	// resource limit -- a cgroup memory ceiling and a CPU share -- with no
	// ceiling of their own, so one request could ask for a terabyte of memory
	// or thousands of vCPUs and the node would try to honour it.
	MaxMemMiB int `yaml:"max_mem_mib"`
	MaxVCPUs  int `yaml:"max_vcpus"`
	// MaxTTLAbsoluteSecs bounds how long a sandbox may pin resources.
	MaxTTLAbsoluteSecs int `yaml:"max_ttl_absolute_secs"`

	DefaultIdlePauseSecs  int `yaml:"default_idle_pause_secs"`
	RetentionDays         int `yaml:"retention_days"`
	MemoryOvercommitRatio int `yaml:"memory_overcommit_ratio"`
}

// BackupConfig controls scheduled store snapshots.
type BackupConfig struct {
	// Enabled defaults ON. A node holding every org, key and unbilled usage
	// sample with nothing copying it anywhere is one bad disk from losing all
	// of it.
	Enabled bool `yaml:"enabled"`
	// Dir holds snapshots. Put it on a DIFFERENT volume from node.data_dir:
	// a backup on the disk that failed is not a backup.
	Dir string `yaml:"dir"`
	// IntervalMins between snapshots.
	IntervalMins int `yaml:"interval_mins"`
	// Keep bounds how many snapshots are retained. Unbounded backups fill the
	// disk the node also runs sandboxes on.
	Keep int `yaml:"keep"`
}

// BillingConfig holds the price list. One node-wide plan for now: per-org
// plans need a schema migration and are the obvious next step. Prices are in
// NANO-CENTS per unit (1e-9 of a cent) so a per-second price does not round to
// zero -- see internal/billing.
type BillingConfig struct {
	PlanID   string `yaml:"plan_id"`
	Currency string `yaml:"currency"`
	// All zero by default: a deployment that has not chosen prices produces
	// zero invoices rather than guessing a number and charging it.
	SandboxSecondNanoCents int64 `yaml:"sandbox_second_nano_cents"`
	CPUSecondNanoCents     int64 `yaml:"cpu_second_nano_cents"`
	GiBHourNanoCents       int64 `yaml:"gib_hour_nano_cents"`
	EgressGBNanoCents      int64 `yaml:"egress_gb_nano_cents"`
	IncludedCents          int64 `yaml:"included_cents"`
	MinimumCents           int64 `yaml:"minimum_cents"`
}

type LogConfig struct {
	Level string `yaml:"level"`
	JSON  bool   `yaml:"json"`
}

func Defaults() *Config {
	return &Config{
		Mode: "standalone",
		Node: NodeConfig{
			ID:       "",
			Name:     hostname(),
			Region:   "local",
			DataDir:  "/var/lib/substrate",
			JailDir:  "/srv/jail",
			CacheDir: "/var/cache/substrate",
		},
		API: APIConfig{
			Listen: ":8080",
		},
		Gateway: GatewayConfig{
			Enabled:    true,
			Listen:     ":8081",
			BaseDomain: "run.localhost",
			// Off by default: see AllowProcessTierPreviews.
			AllowProcessTierPreviews: false,
		},
		Driver: DriverConfig{
			Name:                 "process",
			FirecrackerBin:       "/usr/local/bin/firecracker",
			JailerBin:            "/usr/local/bin/jailer",
			KernelImage:          "/var/lib/substrate/artifacts/vmlinux",
			AllowProcess:         true,
			RunscBin:             "runsc",
			GvisorSockDir:        "/run/substrate-gvisor/sock",
			GvisorOverlaySizeMiB: 2048,
		},
		Network: NetworkConfig{
			Enabled: true,
			// A /30 per sandbox, so a block addresses a QUARTER of its
			// addresses. /19 = 2,046 sandboxes, which matches the plan's
			// 2,000-resident target; the old /22 default would have run out
			// at 255.
			NodeBlockCIDR: "100.64.0.0/19",
			MTU:           1500,
			EgressPolicy:  "open",
			BandwidthMbit: 100,
		},
		Snapshots: SnapshotConfig{
			ChunkSizeKB: 512,
			CacheSizeGB: 50,
		},
		Limits: LimitsConfig{
			MaxSandboxesPerProject: 1000,
			MaxSandboxesPerUser:    20,
			DefaultIdlePauseSecs:   86400,
			MaxMemMiB:              16384,
			MaxVCPUs:               16,
			MaxTTLAbsoluteSecs:     30 * 24 * 3600,
			RetentionDays:          30,
			MemoryOvercommitRatio:  3,
		},
		Log:     LogConfig{Level: "info"},
		Billing: BillingConfig{PlanID: "default", Currency: "usd"},
		Backup: BackupConfig{
			Enabled:      true,
			Dir:          "/var/backups/substrate",
			IntervalMins: 60,
			Keep:         48, // two days of hourly snapshots
		},
	}
}

func Load(path string) (*Config, error) {
	cfg := Defaults()
	if path != "" && path != "-" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnv(cfg)
	if cfg.Node.ID == "" {
		cfg.Node.ID = "nd_" + hostname()
	}
	// Dev convenience without the old footgun: INSECURE_DEV used to leave the
	// JWT secret EMPTY (anyone could forge sandbox tokens). Generate an
	// ephemeral per-boot secret instead — dev flows keep working, tokens are
	// unforgable, and production still refuses to boot without a real secret.
	if cfg.API.JWTSecret == "" && os.Getenv("SUBSTRATE_INSECURE_DEV") == "1" {
		b := make([]byte, 32)
		if _, err := cryptorand.Read(b); err != nil {
			return nil, fmt.Errorf("generate dev jwt secret: %w", err)
		}
		cfg.API.JWTSecret = "dev_" + hex.EncodeToString(b)
	}
	if err := validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	str := func(key string, dst *string) {
		if v := os.Getenv(key); v != "" {
			*dst = v
		}
	}
	integer := func(key string, dst *int) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	str("SUBSTRATE_MODE", &cfg.Mode)
	str("SUBSTRATE_NODE_ID", &cfg.Node.ID)
	str("SUBSTRATE_ADVERTISE_ADDR", &cfg.Node.AdvertiseAddr)
	str("SUBSTRATE_REGION", &cfg.Node.Region)
	str("SUBSTRATE_DATA_DIR", &cfg.Node.DataDir)
	str("SUBSTRATE_LISTEN", &cfg.API.Listen)
	str("SUBSTRATE_PUBLIC_BASE_URL", &cfg.API.PublicBaseURL)
	str("SUBSTRATE_JWT_SECRET", &cfg.API.JWTSecret)
	str("SUBSTRATE_ADMIN_KEY", &cfg.API.AdminKey)
	str("SUBSTRATE_DRIVER", &cfg.Driver.Name)
	str("SUBSTRATE_FIRECRACKER_BIN", &cfg.Driver.FirecrackerBin)
	str("SUBSTRATE_JAILER_BIN", &cfg.Driver.JailerBin)
	str("SUBSTRATE_KERNEL_IMAGE", &cfg.Driver.KernelImage)
	str("SUBSTRATE_NODE_BLOCK", &cfg.Network.NodeBlockCIDR)
	str("SUBSTRATE_GATEWAY_LISTEN", &cfg.Gateway.Listen)
	str("SUBSTRATE_GATEWAY_DOMAIN", &cfg.Gateway.BaseDomain)
	str("SUBSTRATE_DB_URL", &cfg.Cell.DatabaseURL)
	str("SUBSTRATE_CELL_HTTP", &cfg.Cell.ListenHTTP)
	str("SUBSTRATE_CELL_NODES", &cfg.Cell.ListenNodes)
	str("SUBSTRATE_ENROLL_TOKEN", &cfg.Cell.EnrollToken)
	integer("SUBSTRATE_IDLE_PAUSE_SECS", &cfg.Limits.DefaultIdlePauseSecs)
	integer("SUBSTRATE_MAX_SB_PER_PROJECT", &cfg.Limits.MaxSandboxesPerProject)
	integer("SUBSTRATE_CACHE_GB", &cfg.Snapshots.CacheSizeGB)
}

func validate(cfg *Config) error {
	switch cfg.Mode {
	case "standalone", "node", "cell":
	default:
		return fmt.Errorf("mode must be standalone|node|cell, got %q", cfg.Mode)
	}
	switch cfg.Driver.Name {
	case "firecracker", "gvisor", "process":
	default:
		return fmt.Errorf("driver must be firecracker|gvisor|process, got %q", cfg.Driver.Name)
	}
	if cfg.Driver.Name == "process" && !cfg.Driver.AllowProcess {
		return fmt.Errorf("process driver requires driver.allow_process=true")
	}
	if cfg.API.JWTSecret == "" && os.Getenv("SUBSTRATE_INSECURE_DEV") != "1" {
		return fmt.Errorf("SUBSTRATE_JWT_SECRET must be set (or SUBSTRATE_INSECURE_DEV=1 for dev)")
	}
	// Short HMAC secrets make sandbox JWTs offline-forgeable; enforce a floor
	// outside explicit dev mode.
	if len(cfg.API.JWTSecret) > 0 && len(cfg.API.JWTSecret) < 32 && os.Getenv("SUBSTRATE_INSECURE_DEV") != "1" {
		return fmt.Errorf("SUBSTRATE_JWT_SECRET must be at least 32 characters (got %d)", len(cfg.API.JWTSecret))
	}
	return nil
}

// ValidateForTest exposes validation to test packages.
func ValidateForTest(cfg *Config) error { return validate(cfg) }

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "localhost"
	}
	return h
}
