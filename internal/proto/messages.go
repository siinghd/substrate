package proto

import "encoding/json"

type Hello struct {
	Proto    int      `json:"proto"`
	Agent    string   `json:"agent"`
	Version  string   `json:"version"`
	BootID   string   `json:"boot_id"`
	Features []string `json:"features"`
	// Token authenticates the AGENT to the host on tiers where the host
	// listens and the agent dials in (gVisor). The socket is bind-mounted into
	// the sandbox, so once the host keeps its listener open to allow
	// reconnects, guest code could otherwise dial it and impersonate the
	// agent -- and an agent is what reports the sandbox's own CPU and memory
	// usage, so impersonation is a billing-integrity problem, not just noise.
	//
	// Optional and omitempty: the vsock tiers do not use it and guestd does
	// not send it. The host requires it only when it issued one.
	Token string `json:"token,omitempty"`
}

type ExecRequest struct {
	ExecID string            `json:"exec_id"`
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	TTY    bool              `json:"tty,omitempty"`
	Rows   uint16            `json:"rows,omitempty"`
	Cols   uint16            `json:"cols,omitempty"`
	User   string            `json:"user,omitempty"`
}

type ExecDataMeta struct {
	ExecID string `json:"exec_id"`
	FD     string `json:"fd"`
	EOF    bool   `json:"eof,omitempty"`
}

type ExecExit struct {
	ExecID string `json:"exec_id"`
	Code   int    `json:"code"`
	Killed bool   `json:"killed,omitempty"`
}

type ExecSignal struct {
	ExecID string `json:"exec_id"`
	Signal string `json:"signal"`
}

type ExecResize struct {
	ExecID string `json:"exec_id"`
	Rows   uint16 `json:"rows"`
	Cols   uint16 `json:"cols"`
}

type FsRequest struct {
	Op     string `json:"op"`
	Path   string `json:"path"`
	Offset int64  `json:"offset,omitempty"`
	Length int64  `json:"length,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Append bool   `json:"append,omitempty"`
}

type FsEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Dir  bool   `json:"dir"`
}

type FsResponse struct {
	OK      bool      `json:"ok"`
	Error   string    `json:"error,omitempty"`
	Size    int64     `json:"size,omitempty"`
	Entries []FsEntry `json:"entries,omitempty"`
}

type PortAnnounce struct {
	Port  uint16 `json:"port"`
	Proto string `json:"proto"`
}

type PortsAnnounce struct {
	Ports []PortAnnounce `json:"ports"`
}

type GuestMetrics struct {
	MemTotalKB uint64 `json:"mem_total_kb"`
	MemAvailKB uint64 `json:"mem_avail_kb"`
	Load1      uint32 `json:"load1"`
	PIDs       uint32 `json:"pids"`
	UptimeS    uint64 `json:"uptime_s"`
}

type Reseed struct {
	VMGenID string `json:"vmgenid"`
}

type ReseedAck struct {
	MachineID      string `json:"machine_id"`
	SSHFingerprint string `json:"ssh_fingerprint"`
}

func Encode(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func Decode[T any](payload []byte) (T, error) {
	var v T
	err := json.Unmarshal(payload, &v)
	return v, err
}
