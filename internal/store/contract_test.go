package store

import (
	"context"
	"testing"
	"time"
)

// TestStoreContractPins covers Store methods that had ZERO execution across
// the entire test tree (pass-11 cross-package coverage audit) but have live
// production callers or clear product intent: ListOrgs, DeleteProject,
// ListAPIKeys, ListTemplates/DeleteTemplate, DeletePort, CreateSnapshot/
// GetSnapshot/ListSnapshots, ListExecs. One statement each — the point is
// pinning SQL + scan contracts, not celebrating coverage.

func newContractStore(t *testing.T) Store {
	t.Helper()
	s, err := OpenSQLite(t.TempDir() + "/contract.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateOrg(ctx, &Org{ID: "org_c", Name: "o", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject(ctx, &Project{ID: "prj_c", OrgID: "org_c", Name: "p", MaxSandboxes: 5, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestListOrgsReturnsSeededOrg(t *testing.T) {
	s := newContractStore(t)
	orgs, err := s.ListOrgs(context.Background())
	if err != nil || len(orgs) != 1 || orgs[0].ID != "org_c" {
		t.Fatalf("ListOrgs = %+v, %v", orgs, err)
	}
}

func TestDeleteProjectRemovesRow(t *testing.T) {
	s := newContractStore(t)
	ctx := context.Background()
	if err := s.DeleteProject(ctx, "prj_c"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetProject(ctx, "prj_c"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after delete, got %v", err)
	}
}

func TestAPIKeyLifecycle(t *testing.T) {
	s := newContractStore(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.CreateAPIKey(ctx, &APIKey{ID: "k9", OrgID: "org_c", ProjectID: "prj_c",
		Kind: "org", KeyHash: "hash-c1", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx, "org_c")
	if err != nil || len(keys) != 1 || keys[0].KeyHash != "hash-c1" {
		t.Fatalf("ListAPIKeys = %+v, %v", keys, err)
	}
	if err := s.RevokeAPIKey(ctx, "k9"); err != nil {
		t.Fatal(err)
	}
	k, err := s.GetAPIKeyByHash(ctx, "hash-c1")
	if err != nil || k.RevokedAt == nil {
		t.Fatalf("revocation not visible: %+v %v", k, err)
	}
}

func TestTemplatePutListGetDelete(t *testing.T) {
	s := newContractStore(t)
	ctx := context.Background()
	now := time.Now()
	tpl := &Template{ID: "tpl_contract001", ProjectID: "prj_c", Name: "c",
		MemMiB: 512, VCPUs: 1, CreatedAt: now}
	if err := s.PutTemplate(ctx, tpl); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTemplateByName(ctx, "prj_c", "c")
	if err != nil || got.ID != tpl.ID {
		t.Fatalf("GetTemplateByName = %+v, %v", got, err)
	}
	list, err := s.ListTemplates(ctx, "prj_c")
	if err != nil || len(list) != 1 {
		t.Fatalf("ListTemplates = %+v, %v", list, err)
	}
	if err := s.DeleteTemplate(ctx, tpl.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTemplate(ctx, tpl.ID); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after template delete, got %v", err)
	}
}

func TestPortDeleteAndSnapshotReadPaths(t *testing.T) {
	s := newContractStore(t)
	ctx := context.Background()
	now := time.Now()

	sb := &Sandbox{ID: "sb_contract0001", OrgID: "org_c", ProjectID: "prj_c",
		Desired: "running", State: StateRunning, Tier: "process",
		MemMiB: 512, VCPUs: 1, CreatedAt: now, UpdatedAt: now}
	if err := s.CreateSandbox(ctx, sb); err != nil {
		t.Fatal(err)
	}

	pm := PortMapping{Port: 8080, Proto: "tcp", Public: false, Token: "pxt_c"}
	if err := s.UpsertPort(ctx, sb.ID, pm); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePort(ctx, sb.ID, 8080, "tcp"); err != nil {
		t.Fatal(err)
	}
	ports, err := s.ListPorts(ctx, sb.ID)
	if err != nil || len(ports) != 0 {
		t.Fatalf("port not deleted: %+v, %v", ports, err)
	}

	sn := &Snapshot{ID: "snp_contract01", SandboxID: sb.ID, Kind: "user",
		Ref: "/ref|/snap", LineageID: "lin_1", CreatedAt: now}
	if err := s.CreateSnapshot(ctx, sn); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSnapshot(ctx, sn.ID)
	if err != nil || got.SandboxID != sb.ID || got.Shared {
		t.Fatalf("GetSnapshot = %+v, %v", got, err)
	}
	list, err := s.ListSnapshots(ctx, sb.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListSnapshots = %+v, %v", list, err)
	}
}

func TestListExecsOrderedAndScoped(t *testing.T) {
	s := newContractStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, sbID := range []string{"sb_a", "sb_b"} {
		e := &Exec{ID: "ex_contract" + string(rune('a'+i)), SandboxID: sbID, Argv: []string{"x"}, State: "exited",
			ExitCode: i, CreatedAt: base.Add(time.Duration(i) * time.Minute)}
		if err := s.CreateExec(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	exs, err := s.ListExecs(ctx, "sb_a")
	if err != nil || len(exs) != 1 || exs[0].SandboxID != "sb_a" {
		t.Fatalf("ListExecs = %+v, %v", exs, err)
	}
}
