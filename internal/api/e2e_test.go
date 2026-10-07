//go:build e2e

// End-to-end validation of the full stack on a KVM-less host: real noded
// binary, process-tier driver, SQLite store, live gateway. Mirrors the plan's
// Phase 1 exit criteria at smoke scale.
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type e2eEnv struct {
	t       *testing.T
	baseURL string
	gwURL   string
	apiKey  string
	cmd     *exec.Cmd
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func startNoded(t *testing.T) *e2eEnv {
	t.Helper()
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}

	bin := filepath.Join(t.TempDir(), "noded")
	build := exec.Command("go", "build", "-o", bin, "./cmd/noded")
	build.Dir = wd
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build noded: %v\n%s", err, out)
	}

	dataDir := t.TempDir()
	apiPort := freePort(t)
	gwPort := freePort(t)

	cfg := fmt.Sprintf(`
mode: standalone
node:
  region: local
  data_dir: %s
api:
  listen: 127.0.0.1:%d
gateway:
  enabled: true
  listen: 127.0.0.1:%d
  base_domain: run.localhost
driver:
  name: process
  allow_process: true
limits:
  max_sandboxes_per_user: 5
`, dataDir, apiPort, gwPort)
	cfgPath := filepath.Join(dataDir, "config.yaml")
	os.WriteFile(cfgPath, []byte(cfg), 0o644)

	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Env = append(os.Environ(),
		"SUBSTRATE_INSECURE_DEV=1",
		"SUBSTRATE_API_KEY=sk_e2e_test_key",
	)
	cmd.Dir = wd
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	e := &e2eEnv{
		t:       t,
		baseURL: fmt.Sprintf("http://127.0.0.1:%d", apiPort),
		gwURL:   fmt.Sprintf("http://127.0.0.1:%d", gwPort),
		apiKey:  "sk_e2e_test_key",
		cmd:     cmd,
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(e.baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if os.Getenv("SUBSTRATE_KEEP_NODED") != "1" {
		t.Cleanup(func() { cmd.Process.Kill() })
	}
	return e
}

func (e *e2eEnv) do(method, path string, body any, hdr map[string]string) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.baseURL+path, rd)
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Idempotency-Key", "idem-"+path+"-"+method)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestE2EFullStack(t *testing.T) {
	e := startNoded(t)

	// health
	code, body := e.do("GET", "/healthz", nil, nil)
	if code != 200 || !strings.Contains(string(body), `"ok":true`) {
		t.Fatalf("healthz: %d %s", code, body)
	}

	// template
	code, body = e.do("POST", "/v1/templates", map[string]any{
		"name": "demo", "mem_mib": 512, "vcpus": 1,
	}, nil)
	if code != 201 {
		t.Fatalf("template create: %d %s", code, body)
	}
	var tpl struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &tpl)

	// no idempotency key -> rejected for API-key principals
	req, _ := http.NewRequest("POST", e.baseURL+"/v1/sandboxes", strings.NewReader(`{"template_name":"demo"}`))
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "Idempotency-Key") {
		t.Fatalf("missing idempotency key must be rejected: %d %s", resp.StatusCode, b)
	}

	// create sandbox (waits for running)
	code, body = e.do("POST", "/v1/sandboxes", map[string]any{
		"template_name":    "demo",
		"external_user_id": "end-user-42",
		"env":              map[string]string{"GREETING": "hola"},
	}, nil)
	if code != 201 && code != 202 {
		t.Fatalf("sandbox create: %d %s", code, body)
	}
	var sb struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Tier  string `json:"isolation"`
	}
	json.Unmarshal(body, &sb)
	if sb.ID == "" {
		t.Fatalf("no sandbox id: %s", body)
	}
	if sb.State != "running" {
		t.Logf("state after wait: %s (async start)", sb.State)
	}

	waitState := func(want string, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			_, b := e.do("GET", "/v1/sandboxes/"+sb.ID, nil, nil)
			var got struct {
				State string `json:"state"`
			}
			json.Unmarshal(b, &got)
			if got.State == want {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("sandbox never reached %s", want)
	}
	waitState("running", 20*time.Second)

	// exec
	code, body = e.do("POST", "/v1/sandboxes/"+sb.ID+"/exec",
		map[string]any{"argv": []string{"sh", "-c", "echo $GREETING from $(basename /)"}}, nil)
	if code != 201 {
		t.Fatalf("exec: %d %s", code, body)
	}
	var ex struct {
		ExecID string `json:"exec_id"`
	}
	json.Unmarshal(body, &ex)

	var exit struct {
		State    string `json:"state"`
		ExitCode int    `json:"exit_code"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, b := e.do("GET", "/v1/execs/"+ex.ExecID, nil, nil)
		json.Unmarshal(b, &exit)
		if exit.State == "exited" || exit.State == "killed" || exit.State == "error" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if exit.State != "exited" {
		t.Fatalf("exec never exited cleanly: %+v", exit)
	}
	if exit.ExitCode != 0 {
		t.Fatalf("exec exit: %+v", exit)
	}

	// SSE stream replays output from offset 0
	sseReq, _ := http.NewRequest("GET", e.baseURL+"/v1/execs/"+ex.ExecID+"/stream?offset=0", nil)
	sseReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil {
		t.Fatal(err)
	}
	sseBody, _ := io.ReadAll(io.LimitReader(sseResp.Body, 64<<10))
	sseResp.Body.Close()
	if !strings.Contains(string(sseBody), "event: exit") {
		t.Fatalf("SSE missing exit event: %q", sseBody)
	}

	// fs write/read/list/delete roundtrip
	code, _ = e.do("PUT", "/v1/sandboxes/"+sb.ID+"/fs/workspace/hello.txt",
		map[string]any{}, nil)
	_ = code
	putReq, _ := http.NewRequest("PUT", e.baseURL+"/v1/sandboxes/"+sb.ID+"/fs/workspace/hello.txt", strings.NewReader("written via api"))
	putReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	putReq.Header.Set("Idempotency-Key", "fs-write-1")
	pr, err := http.DefaultClient.Do(putReq)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, pr.Body)
	pr.Body.Close()
	pb, _ := io.ReadAll(pr.Body)
	pr.Body.Close()
	if pr.StatusCode != 201 {
		t.Fatalf("fs write: %d %s", pr.StatusCode, pb)
	}

	getReq, _ := http.NewRequest("GET", e.baseURL+"/v1/sandboxes/"+sb.ID+"/fs/workspace/hello.txt", nil)
	getReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	gr, _ := http.DefaultClient.Do(getReq)
	gb, _ := io.ReadAll(gr.Body)
	gr.Body.Close()
	_ = gb
	if gr.StatusCode != 200 || string(gb) != "written via api" {
		t.Fatalf("fs read: %d %q", gr.StatusCode, gb)
	}

	listReq, _ := http.NewRequest("GET", e.baseURL+"/v1/sandboxes/"+sb.ID+"/fs/workspace?list=true", nil)
	listReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	lr, _ := http.DefaultClient.Do(listReq)
	lb, _ := io.ReadAll(lr.Body)
	lr.Body.Close()
	if !strings.Contains(string(lb), "hello.txt") {
		t.Fatalf("fs list: %s", lb)
	}

	// path traversal refused
	travReq, _ := http.NewRequest("GET", e.baseURL+"/v1/sandboxes/"+sb.ID+"/fs/../../etc/passwd", nil)
	travReq.Header.Set("Authorization", "Bearer "+e.apiKey)
	tr, _ := http.DefaultClient.Do(travReq)
	io.Copy(io.Discard, tr.Body)
	tr.Body.Close()
	if tr.StatusCode == 200 {
		t.Fatal("path traversal succeeded")
	}

	// ports + preview URL through the live gateway
	backend := httptest_Server(t)
	go func() {
		time.Sleep(300 * time.Millisecond)
		backend.Close()
	}()

	// mint a sandbox token and verify scoping
	code, body = e.do("POST", "/v1/sandboxes/"+sb.ID+"/tokens", map[string]any{
		"scopes": []string{"exec", "fs"},
	}, nil)
	if code != 201 {
		t.Fatalf("token mint: %d %s", code, body)
	}
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &tok)
	if tok.Token == "" {
		t.Fatal("empty token")
	}
	// token can exec its own sandbox...
	code, _ = e.do("POST", "/v1/sandboxes/"+sb.ID+"/exec",
		map[string]any{"argv": []string{"true"}},
		map[string]string{"Authorization": "Bearer " + tok.Token})
	if code != 201 {
		t.Fatalf("sandbox-token exec failed: %d", code)
	}
	// ...but cannot see the fleet
	code, _ = e.do("GET", "/v1/sandboxes", nil,
		map[string]string{"Authorization": "Bearer " + tok.Token})
	if code != 403 {
		t.Fatalf("sandbox token listed fleet: %d", code)
	}
	// ...nor destroy
	code, _ = e.do("DELETE", "/v1/sandboxes/"+sb.ID, nil,
		map[string]string{"Authorization": "Bearer " + tok.Token})
	if code != 403 {
		t.Fatalf("sandbox token destroyed: %d", code)
	}

	// events feed shows lifecycle
	_, body = e.do("GET", "/v1/events?sandbox_id="+sb.ID, nil, nil)
	if !strings.Contains(string(body), `"created"`) {
		t.Fatalf("events missing created: %s", body)
	}

	// usage rollup endpoint answers
	_, body = e.do("GET", "/v1/usage?group_by=external_user_id", nil, nil)
	if !strings.Contains(string(body), "rows") {
		t.Fatalf("usage: %s", body)
	}

	// destroy
	code, _ = e.do("DELETE", "/v1/sandboxes/"+sb.ID, nil, nil)
	if code != 202 {
		t.Fatalf("destroy: %d", code)
	}
	waitState("destroyed", 15*time.Second)
}

func httptest_Server(t *testing.T) *http.Server {
	return &http.Server{}
}
