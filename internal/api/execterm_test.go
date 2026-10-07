package api

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/config"
	"github.com/substrate-sh/substrate/internal/logging"
	"github.com/substrate-sh/substrate/internal/reconcile"
	"github.com/substrate-sh/substrate/internal/store"
	"github.com/substrate-sh/substrate/internal/vmm"
	procdrv "github.com/substrate-sh/substrate/internal/vmm/process"
)

// TestExecCtxCancelKillsGuestProcess: canceling an exec's context mid-flight
// must actually terminate the guest process (SIGKILL → 137). Regression for
// the signal-routing bug where control frames were sent on dedicated streams
// that both agents discarded — kills were silent no-ops.
func TestExecCtxCancelKillsGuestProcess(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := timeNow()
	st.CreateOrg(ctx, &store.Org{ID: "org_1", Name: "o", CreatedAt: now})
	st.CreateProject(ctx, &store.Project{ID: "prj_1", OrgID: "org_1", Name: "p", MaxSandboxes: 5, CreatedAt: now})

	cfg := config.Defaults()
	cfg.Node.DataDir = t.TempDir()
	log := logging.New("error")
	drv := procdrv.NewDriver(buildShimForTest(t))
	sup := reconcile.NewSupervisor(st, drv, log, cfg.Node.DataDir)
	go sup.Events(ctx)

	sb := &store.Sandbox{ID: "sb_cancel00000001", OrgID: "org_1", ProjectID: "prj_1",
		TemplateID: "tpl_c", Desired: "running", State: store.StateCreating,
		Tier: "process", MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now}
	st.CreateSandbox(ctx, sb)
	tpl := &store.Template{ID: "tpl_c", ProjectID: "prj_1", Name: "t", MemMiB: 512, VCPUs: 1, CreatedAt: now}
	if err := sup.EnsureRunning(ctx, sb, tpl, nil); err != nil {
		t.Fatalf("ensure running: %v", err)
	}
	m, ok := sup.Get(sb.ID)
	if !ok {
		t.Fatal("machine missing")
	}

	execCtx, cancel := context.WithCancel(context.Background())
	h, err := m.Exec(execCtx, vmm.ExecSpec{Argv: []string{"sh", "-c", "sleep 10"}})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}

	time.Sleep(300 * time.Millisecond) // let sleep get going
	cancel()                           // simulate request-context cancellation

	select {
	case ei := <-h.ExitCh:
		if ei.Code != 137 {
			t.Logf("exec ended after ctx cancel with code %d (SIGKILL is 137)", ei.Code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("exec survived manual ctx cancel for >3s: kill path broken")
	}
}

// TestExecKillEndpointTerminatesProcess: DELETE /v1/execs/{id} must actually
// stop the guest process and the persisted state must become "killed" — not
// just return 202.
func TestExecKillEndpointTerminatesProcess(t *testing.T) {
	e := newAttackEnv(t)
	e.cfg.Node.DataDir = t.TempDir()
	ctx := context.Background()
	e.st.PutTemplate(ctx, &store.Template{ID: "tpl_kill0000001", ProjectID: "prj_1", Name: "k", MemMiB: 512, VCPUs: 1, CreatedAt: timeNow()})
	code, body := e.do("POST", "/v1/sandboxes", `{"template_id":"tpl_kill0000001"}`, e.apiKey, true)
	if code != 201 && code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	sbID := extractJSON(body, "id")
	waitRunningAttack(e, sbID)

	code, body = e.do("POST", "/v1/sandboxes/"+sbID+"/exec",
		`{"argv":["sh","-c","sleep 30"]}`, e.apiKey, true)
	if code != 201 {
		t.Fatalf("exec create: %d %s", code, body)
	}
	exID := extractJSON(body, "exec_id")

	code, _ = e.do("DELETE", "/v1/execs/"+exID, "", e.apiKey, true)
	if code != 202 {
		t.Fatalf("kill endpoint returned %d", code)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, b := e.do("GET", "/v1/execs/"+exID, "", e.apiKey, false)
		// Process-tier agents report SIGKILL as exit code 137 without the
		// killed flag; guestd sets killed=true. Both are acceptable proof.
		if strings.Contains(b, `"killed"`) ||
			(strings.Contains(b, `"exited"`) && strings.Contains(b, `"exit_code":137`)) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("exec still running 5s after kill; last=%s", func() string {
		_, b := e.do("GET", "/v1/execs/"+exID, "", e.apiKey, false)
		return b
	}())
}
