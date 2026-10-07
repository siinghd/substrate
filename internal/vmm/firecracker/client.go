// Package firecracker drives Firecracker microVMs through its API socket,
// wrapped in the jailer (plan §7.2), with GA full snapshots for pause/restore
// (§3.2) and the VMGenID-based uniqueness protocol on every restore (§7.4).
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

// Client talks to one Firecracker instance's API unix socket.
type Client struct {
	sock string
	hc   *http.Client
}

func NewClient(sockPath string) (*Client, error) {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}
	return &Client{sock: sockPath, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, int, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rd)
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("fc %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return raw, resp.StatusCode, fmt.Errorf("fc %s %s: %d: %s", method, path, resp.StatusCode, truncate(raw))
	}
	return raw, resp.StatusCode, nil
}

func truncate(b []byte) string {
	if len(b) > 300 {
		return string(b[:300])
	}
	return string(b)
}

// ---- typed wrappers over the Firecracker REST surface ----

type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

type MachineConfig struct {
	MemSizeMib int  `json:"mem_size_mib"`
	VcpuCount  int  `json:"vcpu_count"`
	SMT        bool `json:"smt"`
}

type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsReadOnly   bool   `json:"is_read_only"`
	IsRootDevice bool   `json:"is_root_device"`
}

type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	GuestMac    string `json:"guest_mac"`
	HostDevName string `json:"host_dev_name"`
}

type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path,omitempty"`
}

type Balloon struct {
	AmountMib             int  `json:"amount_mib"`
	DeflateOnOom          bool `json:"deflate_on_oom"`
	StatsPollingIntervals int  `json:"stats_polling_interval_s,omitempty"`
}

type VMState struct {
	State string `json:"state"` // Started | Paused | Resumed
}

type SnapshotCreate struct {
	MemFilePath  string `json:"mem_file_path"`
	SnapshotPath string `json:"snapshot_path"`
	Type         string `json:"snapshot_type,omitempty"` // Full (default)
	Version      string `json:"version,omitempty"`
}

type MemBackend struct {
	Type string `json:"type"` // File | Uffd
	Path string `json:"path,omitempty"`
}

type SnapshotLoad struct {
	SnapshotPath        string     `json:"snapshot_path"`
	MemBackend          MemBackend `json:"mem_backend"`
	EnableDiffSnapshots bool       `json:"enable_diff_snapshots,omitempty"`
}

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	_, _, err := c.do(ctx, http.MethodPut, "/boot-source", b)
	return err
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	_, _, err := c.do(ctx, http.MethodPut, "/machine-config", m)
	return err
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	_, _, err := c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
	return err
}

func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	_, _, err := c.do(ctx, http.MethodPut, "/network-interfaces/"+n.IfaceID, n)
	return err
}

func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	_, _, err := c.do(ctx, http.MethodPut, "/vsock", v)
	return err
}

func (c *Client) PutBalloon(ctx context.Context, b Balloon) error {
	_, _, err := c.do(ctx, http.MethodPut, "/balloon", b)
	return err
}

func (c *Client) StartInstance(ctx context.Context) error {
	_, _, err := c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
	return err
}

func (c *Client) PatchVM(ctx context.Context, s VMState) error {
	_, _, err := c.do(ctx, http.MethodPatch, "/vm", s)
	return err
}

func (c *Client) CreateSnapshot(ctx context.Context, sc SnapshotCreate) error {
	_, _, err := c.do(ctx, http.MethodPut, "/snapshot/create", sc)
	return err
}

func (c *Client) LoadSnapshot(ctx context.Context, sl SnapshotLoad) error {
	_, _, err := c.do(ctx, http.MethodPut, "/snapshot/load", sl)
	return err
}

func (c *Client) WaitForSocket(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(c.sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return fmt.Errorf("api socket %s never appeared", c.sock)
}
