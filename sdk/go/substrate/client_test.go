package substrate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCreateWaitExecFlow(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" && (r.Method == "POST" || r.Method == "DELETE") {
			t.Errorf("missing idempotency key on %s %s", r.Method, r.URL.Path)
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"id": "sb_1", "state": "creating"})
		case r.Method == "GET" && r.URL.Path == "/v1/sandboxes/sb_1":
			json.NewEncoder(w).Encode(map[string]any{"id": "sb_1", "state": "running"})
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes/sb_1/exec":
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"exec_id": "ex_1"})
		case strings.HasPrefix(r.URL.Path, "/v1/execs/ex_1/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("event: output\ndata: hel\n"))
			w.Write([]byte("event: output\ndata: lo\\n\n"))
			w.Write([]byte("event: exit\ndata: {\"exit_code\":0}\n\n"))
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/sandboxes/sb_1/fs/"):
			w.WriteHeader(201)
		case r.Method == "GET" && r.URL.Path == "/v1/sandboxes/sb_1/fs/tmp/x.txt":
			w.Write([]byte("data"))
		case r.Method == "POST" && r.URL.Path == "/v1/sandboxes/sb_1/tokens":
			json.NewEncoder(w).Encode(map[string]any{"token": "tok123"})
		case r.URL.Path == "/v1/execs/ex_1":
			json.NewEncoder(w).Encode(map[string]any{"id": "ex_1", "exit_code": 0})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "sk_test")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	sb, err := c.Sandboxes().Create(ctx, CreateOptions{TemplateName: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID != "sb_1" || sb.State != "running" {
		t.Fatalf("sandbox: %+v", sb)
	}

	res, err := c.Exec(ctx, sb.ID, []string{"echo", "hi"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK() || res.Output != "hello\n" {
		t.Fatalf("exec result: %+v %q", res.ExitCode, res.Output)
	}

	if err := c.FsWrite(ctx, sb.ID, "/tmp/x.txt", []byte("data")); err != nil {
		t.Fatal(err)
	}
	got, err := c.FsRead(ctx, sb.ID, "/tmp/x.txt")
	if err != nil || string(got) != "data" {
		t.Fatalf("fs roundtrip: %q %v", got, err)
	}

	tok, err := c.MintToken(ctx, sb.ID, []string{"exec"}, 3600)
	if err != nil || tok == "" {
		t.Fatalf("token: %q %v", tok, err)
	}
	_ = time.Second
}
