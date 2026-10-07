// Package store is the canonical persistence layer for sandboxes, templates,
// credentials, events and metering. One SQL dialect source (?-placeholders)
// runs on SQLite (standalone) and Postgres (cell) — both support ON CONFLICT
// upserts; timestamps are unix-millis INTEGERs for portability.
package store

import (
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type SandboxState string

const (
	StateCreating   SandboxState = "creating"
	StateRunning    SandboxState = "running"
	StatePaused     SandboxState = "paused"
	StateArchived   SandboxState = "archived"
	StateDestroying SandboxState = "destroying"
	StateDestroyed  SandboxState = "destroyed"
	StateError      SandboxState = "error"
)

type PortMapping struct {
	Port   int    `json:"port"`
	Proto  string `json:"proto"`
	Public bool   `json:"public"`
	Token  string `json:"-"`
}

type Sandbox struct {
	ID              string            `json:"id"`
	OrgID           string            `json:"org_id"`
	ProjectID       string            `json:"project_id"`
	TemplateID      string            `json:"template_id,omitempty"`
	ExternalUserID  string            `json:"external_user_id,omitempty"`
	Desired         string            `json:"-"`
	State           SandboxState      `json:"state"`
	Tier            string            `json:"isolation"` // hv | kernel | process
	NodeID          string            `json:"-"`
	MemMiB          int               `json:"mem_mib"`
	VCPUs           int               `json:"vcpus"`
	Env             map[string]string `json:"env,omitempty"`
	LineageID       string            `json:"-"`
	PauseRef        string            `json:"-"`
	Error           string            `json:"error,omitempty"`
	TTLIdleSecs     int               `json:"ttl_idle_secs"`
	TTLAbsoluteSecs int               `json:"ttl_absolute_secs,omitempty"`
	LastActivityMS  int64             `json:"-"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
	StartedAt       *time.Time        `json:"started_at,omitempty"`
	PausedAt        *time.Time        `json:"paused_at,omitempty"`
}

type Template struct {
	ID         string    `json:"id"`
	ProjectID  string    `json:"project_id"`
	Name       string    `json:"name"`
	RootfsPath string    `json:"-"`
	RootfsSize int64     `json:"rootfs_size_bytes"`
	KernelPath string    `json:"-"`
	MemMiB     int       `json:"mem_mib"`
	VCPUs      int       `json:"vcpus"`
	NetPolicy  string    `json:"network_policy,omitempty"`
	WarmCmd    string    `json:"warm_cmd,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type APIKey struct {
	ID        string     `json:"id"`
	OrgID     string     `json:"org_id"`
	ProjectID string     `json:"project_id,omitempty"`
	Kind      string     `json:"kind"` // org | project
	KeyHash   string     `json:"-"`
	CreatedAt time.Time  `json:"created_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
}

type Exec struct {
	ID             string     `json:"id"`
	SandboxID      string     `json:"sandbox_id"`
	Argv           []string   `json:"argv"`
	State          string     `json:"state"` // running | exited | killed | error
	ExitCode       int        `json:"exit_code"`
	TTY            bool       `json:"tty"`
	LogPath        string     `json:"-"`
	IdempotencyKey string     `json:"-"`
	CreatedAt      time.Time  `json:"created_at"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
}

type Snapshot struct {
	ID        string    `json:"id"`
	SandboxID string    `json:"sandbox_id"`
	Kind      string    `json:"kind"` // template | pause | user
	Ref       string    `json:"-"`
	SizeBytes int64     `json:"size_bytes"`
	Shared    bool      `json:"shared"`
	LineageID string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
}

type Event struct {
	ID        string          `json:"id"`
	TS        time.Time       `json:"ts"`
	OrgID     string          `json:"org_id,omitempty"`
	ProjectID string          `json:"project_id,omitempty"`
	SandboxID string          `json:"sandbox_id,omitempty"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type UsageSample struct {
	SandboxID      string
	TS             time.Time
	CPUUS          int64
	MemPeakKB      int64
	RxBytes        int64
	TxBytes        int64
	Running        bool
	ExternalUserID string
	ProjectID      string
	// IntervalS is the wall-clock span this sample accounts for. Zero means
	// "use the schema default"; the sampler always sets it explicitly.
	IntervalS int64
}

// DefaultSampleIntervalS backfills usage rows written without an explicit
// interval (rows from before the column existed default to it in SQL too).
const DefaultSampleIntervalS = 15

func ms(t time.Time) int64 { return t.UnixMilli() }
func fromMS(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.UnixMilli(n).UTC()
}
