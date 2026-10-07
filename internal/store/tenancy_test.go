package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestPutTemplateCannotCrossProjects: ON CONFLICT(id) DO UPDATE had no tenancy
// predicate, so supplying an existing template id let one project overwrite
// another project's row — including rootfs_path, the host image every sandbox
// built from that template boots. The write must be refused, and the victim's
// row must be untouched.
func TestPutTemplateCannotCrossProjects(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	victim := &Template{ID: "tpl_victim00001", ProjectID: "prj_victim", Name: "prod",
		RootfsPath: "/srv/images/victim.ext4", KernelPath: "/srv/vmlinux", MemMiB: 512, VCPUs: 1, CreatedAt: now}
	if err := s.PutTemplate(ctx, victim); err != nil {
		t.Fatalf("seed victim template: %v", err)
	}

	attack := &Template{ID: "tpl_victim00001", ProjectID: "prj_attacker", Name: "pwned",
		RootfsPath: "/tmp/attacker-controlled.ext4", KernelPath: "/srv/vmlinux", MemMiB: 512, VCPUs: 1, CreatedAt: now}
	err := s.PutTemplate(ctx, attack)
	if err == nil {
		t.Fatal("cross-project template upsert succeeded; want ErrConflict")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-project upsert error = %v, want ErrConflict", err)
	}

	got, gerr := s.GetTemplate(ctx, "tpl_victim00001")
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.ProjectID != "prj_victim" {
		t.Fatalf("template was stolen: project_id = %q", got.ProjectID)
	}
	if got.RootfsPath != "/srv/images/victim.ext4" {
		t.Fatalf("victim rootfs was repointed to %q — attacker controls the VM image", got.RootfsPath)
	}
	if got.Name != "prod" {
		t.Fatalf("victim template name overwritten: %q", got.Name)
	}
}

// TestPutTemplateSameProjectStillUpserts: the tenancy guard must not break the
// legitimate update path.
func TestPutTemplateSameProjectStillUpserts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now()

	base := &Template{ID: "tpl_update00001", ProjectID: "prj_1", Name: "app",
		RootfsPath: "/srv/v1.ext4", MemMiB: 512, VCPUs: 1, CreatedAt: now}
	if err := s.PutTemplate(ctx, base); err != nil {
		t.Fatal(err)
	}
	base.RootfsPath = "/srv/v2.ext4"
	base.Name = "app-v2"
	if err := s.PutTemplate(ctx, base); err != nil {
		t.Fatalf("same-project update rejected: %v", err)
	}
	got, err := s.GetTemplate(ctx, "tpl_update00001")
	if err != nil {
		t.Fatal(err)
	}
	if got.RootfsPath != "/srv/v2.ext4" || got.Name != "app-v2" {
		t.Fatalf("update did not apply: %+v", got)
	}
}

// TestGetTemplateByNameIsDeterministic: with no UNIQUE(project_id, name),
// re-registering a name leaves duplicates; the lookup must not return an
// arbitrary one.
func TestGetTemplateByNameIsDeterministic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Now()
	for i, path := range []string{"/srv/old.ext4", "/srv/new.ext4"} {
		if err := s.PutTemplate(ctx, &Template{
			ID: "tpl_dupe00000" + string(rune('a'+i)), ProjectID: "prj_1", Name: "dupe",
			RootfsPath: path, MemMiB: 512, VCPUs: 1,
			CreatedAt: base.Add(time.Duration(i) * time.Minute),
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		got, err := s.GetTemplateByName(ctx, "prj_1", "dupe")
		if err != nil {
			t.Fatal(err)
		}
		if got.RootfsPath != "/srv/new.ext4" {
			t.Fatalf("lookup %d returned %q, want the newest registration", i, got.RootfsPath)
		}
	}
}
