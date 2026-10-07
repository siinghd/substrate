//go:build e2e

package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Regression probe (real noded binary): a long-running exec must survive its
// create-request returning. The exec ctx-watcher used to KILL the exec the
// moment ServeHTTP returned (request contexts cancel on handler return).
func TestE2ELongExecSurvivesCreateReturn(t *testing.T) {
	e := startNoded(t)

	code, body := e.do("POST", "/v1/templates", map[string]any{"name": "longexec"}, nil)
	if code != 201 {
		t.Fatalf("template: %d %s", code, body)
	}
	scode, sbody := e.doIdem("POST", "/v1/sandboxes", `{"template_name":"longexec"}`, "longexec-sb")
	if scode != 201 && scode != 202 {
		t.Fatalf("sandbox: %d %s", scode, sbody)
	}
	var sb struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	json.Unmarshal([]byte(sbody), &sb)

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(sb.State, "running") && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		_, b := e.do("GET", "/v1/sandboxes/"+sb.ID, nil, nil)
		json.Unmarshal(b, &sb)
	}
	if !strings.Contains(sb.State, "running") {
		t.Fatalf("sandbox never ran; state=%s", sb.State)
	}

	code, body = e.do("POST", "/v1/sandboxes/"+sb.ID+"/exec",
		map[string]any{"argv": []string{"sh", "-c", "sleep 3 && echo survived"}}, nil)
	if code != 201 {
		t.Fatalf("exec create: %d %s", code, body)
	}
	var ex struct {
		ExecID string `json:"exec_id"`
	}
	json.Unmarshal(body, &ex)

	var last string
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_, b := e.do("GET", "/v1/execs/"+ex.ExecID, nil, nil)
		last = string(b)
		if strings.Contains(last, `"killed"`) || strings.Contains(last, `"error"`) {
			t.Fatalf("LONG EXEC KILLED BY CREATE-REQUEST CONTEXT: %s", last)
		}
		if strings.Contains(last, `"exited"`) && strings.Contains(last, `"exit_code":0`) {
			return // survived and completed cleanly
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("exec never exited cleanly; last=%s", last)
}
