// Package substrate is the Go SDK for the Substrate sandbox platform.
//
//	c, _ := substrate.New("http://localhost:8080", "sk_...")
//	sb, _ := c.Sandboxes.Create(ctx, substrate.CreateOptions{TemplateName: "demo"})
//	res, _ := sb.Exec(ctx, []string{"bash", "-lc", "make test"}, nil)
//	fmt.Println(res.Output)
package substrate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("[%d] %s: %s", e.Status, e.Code, e.Message)
	if e.Fix != "" {
		s += " (fix: " + e.Fix + ")"
	}
	return s
}

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL, apiKey string) (*Client, error) {
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}, nil
}

func idemKey() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "go-" + hex.EncodeToString(b)
}

// escapePath escapes one URL path segment.
func escapePath(s string) string { return url.PathEscape(s) }

// guestFSPath escapes a sandbox-absolute path while preserving "/" as the
// separator: a "?" or "#" in a file name must never become a query string or
// fragment on the wire.
func guestFSPath(p string) string {
	return strings.ReplaceAll(url.PathEscape(p), "%2F", "/")
}

func (c *Client) do(ctx context.Context, method, path string, body any, opts reqOpts) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	} else if opts.rawBody != nil {
		rd = bytes.NewReader(opts.rawBody)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return nil, err
	}
	token := c.APIKey
	if opts.token != "" {
		token = opts.token
	}
	req.Header.Set("Authorization", "Bearer "+token)
	switch {
	case body != nil:
		req.Header.Set("Content-Type", "application/json")
	case opts.rawBody != nil:
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if opts.idempotency {
		req.Header.Set("Idempotency-Key", idemKey())
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20+1))
	if err != nil {
		// A failed mid-body read must surface, not masquerade as complete
		// JSON/partial file content.
		return nil, fmt.Errorf("%s %s: response read failed: %w", method, path, err)
	}
	if int64(len(raw)) > 64<<20 {
		return nil, fmt.Errorf("%s %s: response exceeds 64MiB limit", method, path)
	}
	if resp.StatusCode >= 400 {
		var ae APIError
		ae.Status = resp.StatusCode
		if json.Unmarshal(raw, &ae) != nil {
			ae.Code = "http_error"
			ae.Message = strings.TrimSpace(string(raw))
		}
		return nil, &ae
	}
	return raw, nil
}

type reqOpts struct {
	idempotency bool
	token       string
	rawBody     []byte
}

// ---- types ----

type Sandbox struct {
	ID             string            `json:"id"`
	State          string            `json:"state"`
	TemplateID     string            `json:"template_id"`
	ExternalUserID string            `json:"external_user_id"`
	Isolation      string            `json:"isolation"`
	MemMiB         int               `json:"mem_mib"`
	VCPUs          int               `json:"vcpus"`
	Error          string            `json:"error,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	env            map[string]string `json:"-"`
}

type CreateOptions struct {
	TemplateName   string
	TemplateID     string
	ExternalUserID string
	Env            map[string]string
	MemMiB         int
	VCPUs          int
	TTLIdleSecs    int
	NoWait         bool
	WaitTimeout    time.Duration
}

type ExecResult struct {
	ExecID   string
	ExitCode int
	Output   string
}

func (r *ExecResult) OK() bool { return r.ExitCode == 0 }

// ---- sandboxes ----

type SandboxService struct{ c *Client }

func (s *SandboxService) Create(ctx context.Context, o CreateOptions) (*Sandbox, error) {
	body := map[string]any{}
	if o.TemplateName != "" {
		body["template_name"] = o.TemplateName
	}
	if o.TemplateID != "" {
		body["template_id"] = o.TemplateID
	}
	if o.ExternalUserID != "" {
		body["external_user_id"] = o.ExternalUserID
	}
	if o.Env != nil {
		body["env"] = o.Env
	}
	if o.MemMiB > 0 {
		body["mem_mib"] = o.MemMiB
	}
	if o.VCPUs > 0 {
		body["vcpus"] = o.VCPUs
	}
	if o.TTLIdleSecs > 0 {
		body["ttl_idle_secs"] = o.TTLIdleSecs
	}
	raw, err := s.c.do(ctx, "POST", "/v1/sandboxes", body, reqOpts{idempotency: true})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	if err := json.Unmarshal(raw, &sb); err != nil {
		return nil, err
	}
	if o.NoWait {
		return &sb, nil
	}
	timeout := o.WaitTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if err := (&SandboxService{c: s.c}).Wait(ctx, sb.ID, []string{"running"}, timeout); err != nil {
		return &sb, err
	}
	return s.Get(ctx, sb.ID)
}

func (s *SandboxService) Get(ctx context.Context, id string) (*Sandbox, error) {
	raw, err := s.c.do(ctx, "GET", "/v1/sandboxes/"+escapePath(id), nil, reqOpts{})
	if err != nil {
		return nil, err
	}
	var sb Sandbox
	return &sb, json.Unmarshal(raw, &sb)
}

func (s *SandboxService) List(ctx context.Context, externalUserID, state string) ([]*Sandbox, error) {
	q := ""
	if externalUserID != "" || state != "" {
		v := url.Values{}
		if externalUserID != "" {
			v.Set("external_user_id", externalUserID)
		}
		if state != "" {
			v.Set("state", state)
		}
		q = "?" + v.Encode()
	}
	raw, err := s.c.do(ctx, "GET", "/v1/sandboxes"+q, nil, reqOpts{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Sandboxes []*Sandbox `json:"sandboxes"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Sandboxes, nil
}

func (s *SandboxService) Delete(ctx context.Context, id string) error {
	_, err := s.c.do(ctx, "DELETE", "/v1/sandboxes/"+escapePath(id), nil, reqOpts{idempotency: true})
	return err
}

// Wait polls until the sandbox reaches one of states.
func (s *SandboxService) Wait(ctx context.Context, id string, states []string, timeout time.Duration) error {
	if len(states) == 0 {
		states = []string{"running"}
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sb, err := s.Get(ctx, id)
		if err == nil {
			for _, want := range states {
				if sb.State == want {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("sandbox %s did not reach %v within %s", id, states, timeout)
}

// Exec runs a command to completion, streaming output through onOutput if set.
func (c *Client) Exec(ctx context.Context, sandboxID string, argv []string, env map[string]string, onOutput func(string)) (*ExecResult, error) {
	raw, err := c.do(ctx, "POST", "/v1/sandboxes/"+escapePath(sandboxID)+"/exec",
		map[string]any{"argv": argv, "env": env}, reqOpts{idempotency: true})
	if err != nil {
		return nil, err
	}
	var started struct {
		ExecID string `json:"exec_id"`
	}
	if err := json.Unmarshal(raw, &started); err != nil {
		return nil, err
	}

	var output []byte
	offset := int64(0)
	deadline := time.Now().Add(10 * time.Minute)
	exited := false
	for !exited && time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, "GET",
			fmt.Sprintf("%s/v1/execs/%s/stream?offset=%d", c.BaseURL, started.ExecID, offset), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, err
		}
		buf := make([]byte, 32<<10)
		var line []byte
		event := ""
		flushLine := func() {
			l := string(line)
			line = line[:0]
			switch {
			case l == "event: exit":
				exited = true
			case strings.HasPrefix(l, "id: "):
				if v, err := strconv.ParseInt(strings.TrimPrefix(l, "id: "), 10, 64); err == nil {
					offset = v
				}
			case strings.HasPrefix(l, "event: "):
				event = strings.TrimPrefix(l, "event: ")
			case strings.HasPrefix(l, "data: ") && event == "output":
				chunk := strings.ReplaceAll(strings.ReplaceAll(l[6:], `\n`, "\n"), `\r`, "\r")
				output = append(output, chunk...)
				if onOutput != nil {
					onOutput(chunk)
				}
			}
		}
	readLoop:
		for {
			n, err := resp.Body.Read(buf)
			for i := 0; i < n; i++ {
				if buf[i] == '\n' {
					flushLine()
					if exited {
						break readLoop
					}
				} else {
					line = append(line, buf[i])
				}
			}
			if err != nil {
				break
			}
		}
		resp.Body.Close()
		if !exited {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(150 * time.Millisecond):
			}
		}
	}
	if !exited {
		return nil, fmt.Errorf("exec %s timed out", started.ExecID)
	}
	stRaw, err := c.do(ctx, "GET", "/v1/execs/"+started.ExecID, nil, reqOpts{})
	if err != nil {
		return nil, err
	}
	var st struct {
		ExitCode int `json:"exit_code"`
	}
	_ = json.Unmarshal(stRaw, &st)
	return &ExecResult{ExecID: started.ExecID, ExitCode: st.ExitCode, Output: string(output)}, nil
}

// FsRead downloads a file from the sandbox.
func (c *Client) FsRead(ctx context.Context, sandboxID, path string) ([]byte, error) {
	return c.fsOp(ctx, "GET", sandboxID, path, nil)
}

// FsWrite uploads a file; parents are created guest-side.
func (c *Client) FsWrite(ctx context.Context, sandboxID, path string, data []byte) error {
	_, err := c.fsOp(ctx, "PUT", sandboxID, path, data)
	return err
}

func (c *Client) fsOp(ctx context.Context, method, sandboxID, path string, data []byte) ([]byte, error) {
	raw, err := c.do(ctx, method, "/v1/sandboxes/"+escapePath(sandboxID)+"/fs"+guestFSPath(path),
		nil, reqOpts{rawBody: data, idempotency: data != nil})
	return raw, err
}

// ExposePort returns the preview URL (+token unless public).
func (c *Client) ExposePort(ctx context.Context, sandboxID string, port int, public bool) (map[string]any, error) {
	raw, err := c.do(ctx, "POST", "/v1/sandboxes/"+escapePath(sandboxID)+"/ports",
		map[string]any{"port": port, "public": public}, reqOpts{idempotency: true})
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}

// MintToken issues a sandbox-scoped JWT for end-user agents.
func (c *Client) MintToken(ctx context.Context, sandboxID string, scopes []string, ttlSecs int) (string, error) {
	raw, err := c.do(ctx, "POST", "/v1/sandboxes/"+escapePath(sandboxID)+"/tokens",
		map[string]any{"scopes": scopes, "ttl_secs": ttlSecs}, reqOpts{idempotency: true})
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

func (s *SandboxService) Pause(ctx context.Context, id string) error {
	_, err := s.c.do(ctx, "POST", "/v1/sandboxes/"+escapePath(id)+"/pause", map[string]any{}, reqOpts{idempotency: true})
	return err
}

func (s *SandboxService) Resume(ctx context.Context, id string) error {
	_, err := s.c.do(ctx, "POST", "/v1/sandboxes/"+escapePath(id)+"/resume", map[string]any{}, reqOpts{idempotency: true})
	return err
}

// Sandboxes returns the sandbox service.
func (c *Client) Sandboxes() *SandboxService { return &SandboxService{c} }
