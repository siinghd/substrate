//go:build e2e

package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// countOpenFDs reads /proc/<pid>/fd — Linux-only, fine for the e2e harness.
func countOpenFDs(pid int) (int, error) {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// TestE2EConcurrencySoak: parallel full lifecycles against one server.
// Asserts zero failures under contention AND bounded file-descriptor growth
// (shim sockets, exec logs, SSE bodies must all be released).
func TestE2EConcurrencySoak(t *testing.T) {
	e := startNoded(t)

	code, body := e.do("POST", "/v1/templates", map[string]any{"name": "soak"}, nil)
	if code != 201 {
		t.Fatalf("template: %d %s", code, body)
	}

	nodedPID := int(e.cmd.Process.Pid)
	fdsBefore, err := countOpenFDs(nodedPID)
	if err != nil {
		t.Fatalf("fd baseline: %v", err)
	}

	const workers = 8
	const cycles = 3

	var mu sync.Mutex
	failures := []string{}

	var wg sync.WaitGroup
	start := time.Now()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for c := 0; c < cycles; c++ {
				tag := fmt.Sprintf("w%d-c%d", w, c)
				idem := "soak-" + tag

				code, body := e.doIdem("POST", "/v1/sandboxes",
					fmt.Sprintf(`{"template_name":"soak","external_user_id":"%s"}`, tag), idem)
				if code != 201 && code != 202 {
					mu.Lock()
					failures = append(failures, tag+": create "+fmt.Sprint(code)+" "+body)
					mu.Unlock()
					continue
				}
				sbID := extractJSON(body, "id")

				// wait running
				running := false
				for t2 := 0; t2 < 100; t2++ {
					_, b := e.do("GET", "/v1/sandboxes/"+sbID, nil, nil)
					if strings.Contains(string(b), `"running"`) {
						running = true
						break
					}
					time.Sleep(50 * time.Millisecond)
				}
				if !running {
					mu.Lock()
					failures = append(failures, tag+": never ran")
					mu.Unlock()
				} else {
					code, _, exErr := e.rawExec(sbID)
					if exErr != nil || code != 201 {
						mu.Lock()
						failures = append(failures, tag+": exec "+fmt.Sprint(code))
						mu.Unlock()
					}

					code2, _, exErr2 := e.rawPut(sbID, "/fs/soak.txt", "data")
					if exErr2 != nil || code2 != 201 {
						mu.Lock()
						failures = append(failures, tag+": fs write "+fmt.Sprint(code2))
						mu.Unlock()
					}
				}

				dcode, _, derr := e.raw("DELETE", "/v1/sandboxes/"+sbID, "")
				if derr != nil || dcode != 202 {
					mu.Lock()
					failures = append(failures, tag+": destroy "+fmt.Sprint(code))
					mu.Unlock()
				}
			}
		}(w)
	}
	wg.Wait()
	wall := time.Since(start)

	if len(failures) > 0 {
		t.Fatalf("soak failures (%d):\n%s", len(failures), strings.Join(failures, "\n"))
	}

	// fd stability: allow small variance for runtime churn, but a per-cycle
	// leak (socket/log/pipes) would show up as ~workers*cycles*K growth.
	time.Sleep(500 * time.Millisecond)
	fdsAfter, err := countOpenFDs(nodedPID)
	if err != nil {
		t.Fatalf("fd after: %v", err)
	}
	growth := fdsAfter - fdsBefore
	t.Logf("soak: %d lifecycles in %s; fds %d -> %d (growth %d)", workers*cycles, wall.Round(time.Millisecond), fdsBefore, fdsAfter, growth)
	const maxGrowthPerLifecycle = 4
	if growth > workers*cycles*maxGrowthPerLifecycle {
		t.Fatalf("fd leak suspected: grew by %d across %d lifecycles (before=%d after=%d)",
			growth, workers*cycles, fdsBefore, fdsAfter)
	}
}

func (e *e2eEnv) doIdem(method, path string, body string, key string) (int, string) {
	req, _ := http.NewRequest(method, e.baseURL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	resp.Body.Close()
	return resp.StatusCode, string(buf)
}

var _ = strconv.Itoa

// raw issues an authenticated request with a raw string body and unique
// idempotency key. Retries on 429 with backoff — the behavior real SDKs
// implement — and captures the body for diagnostics.
func (e *e2eEnv) raw(method, path, body string) (int, []byte, error) {
	var lastStatus int
	var lastBody []byte
	for attempt := 0; attempt < 10; attempt++ {
		req, err := http.NewRequest(method, e.baseURL+path, strings.NewReader(body))
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		if attempt == 0 {
			req.Header.Set("Idempotency-Key", "soak-"+method+"-"+path+"-"+fmt.Sprint(time.Now().UnixNano()))
		} else {
			req.Header.Set("Idempotency-Key", fmt.Sprintf("soak-%s-%s-%d-%d", method, path, time.Now().UnixNano(), attempt))
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil, err
		}
		out := make([]byte, 0, 4096)
		tmp := make([]byte, 8192)
		for {
			n, rerr := resp.Body.Read(tmp)
			out = append(out, tmp[:n]...)
			if rerr != nil {
				break
			}
		}
		resp.Body.Close()
		lastStatus = resp.StatusCode
		lastBody = out
		if resp.StatusCode != http.StatusTooManyRequests {
			return lastStatus, lastBody, nil
		}
		time.Sleep(120 * time.Millisecond)
	}
	return lastStatus, lastBody, nil
}

func (e *e2eEnv) rawExec(sbID string) (int, []byte, error) {
	return e.raw("POST", "/v1/sandboxes/"+sbID+"/exec", `{"argv":["sh","-c","echo soak-ok"]}`)
}

func (e *e2eEnv) rawPut(sbID, path, body string) (int, []byte, error) {
	return e.raw("PUT", "/v1/sandboxes/"+sbID+path, body)
}
