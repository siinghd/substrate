package process

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/vmm"
)

func buildShim(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "shim")
	wd, _ := os.Getwd()
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/shim")
	cmd.Dir = wd
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	return bin
}

func TestProcessDriverLifecycle(t *testing.T) {
	shim := buildShim(t)
	d := NewDriver(shim)

	rootfs := t.TempDir()
	os.MkdirAll(filepath.Join(rootfs, "workspace"), 0o755)
	os.WriteFile(filepath.Join(rootfs, "workspace", "seed.txt"), []byte("planted"), 0o644)

	spec := vmm.Spec{
		ID:       "sb_proctest1",
		Template: vmm.TemplateSpec{Name: "test", RootfsPath: rootfs, MemMiB: 512, VCPUs: 1},
	}
	m, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Kill()
	if m.ID() != spec.ID || m.Caps().Tier != "process" {
		t.Fatalf("machine meta wrong: %s %+v", m.ID(), m.Caps())
	}

	ctx := context.Background()

	h, err := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"sh", "-c", "echo hello-process; echo err-line 1>&2"}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	buf := make([]byte, 4096)
	doneReading := make(chan struct{})
	go func() {
		for {
			n, err := h.Stdout.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				close(doneReading)
				return
			}
		}
	}()
	ei := <-h.ExitCh
	select {
	case <-doneReading:
	case <-time.After(2 * time.Second):
	}
	if ei.Code != 0 || !bytes.Contains(out.Bytes(), []byte("hello-process")) {
		t.Fatalf("exec: code=%d out=%q", ei.Code, out.String())
	}

	fh, _ := m.Exec(ctx, vmm.ExecSpec{Argv: []string{"sh", "-c", "cat /proc/self/comm; exit 3"}})
	ei = <-fh.ExitCh
	if ei.Code != 3 {
		t.Fatalf("exit code propagation: %+v", ei)
	}

	fs := m.Fs()
	data, err := fs.Read(ctx, "/workspace/seed.txt", 0, 0)
	if err != nil || string(data) != "planted" {
		t.Fatalf("read seeded file: %q %v", data, err)
	}
	if err := fs.Write(ctx, "/workspace/out.txt", []byte("written-by-host"), false); err != nil {
		t.Fatal(err)
	}
	entries, err := fs.List(ctx, "/workspace")
	if err != nil || len(entries) < 2 {
		t.Fatalf("list: %+v %v", entries, err)
	}
	st, err := fs.Stat(ctx, "/workspace/out.txt")
	if err != nil || st.Size != int64(len("written-by-host")) {
		t.Fatalf("stat: %+v %v", st, err)
	}
	if _, err := fs.Read(ctx, "/../../etc/passwd", 0, 0); err == nil {
		t.Fatal("path escape not refused")
	}

	// append must preserve existing content (regression: shim used tmp+rename
	// which silently truncated on append)
	if err := fs.Write(ctx, "/workspace/log.txt", []byte("line1\n"), false); err != nil {
		t.Fatal(err)
	}
	if err := fs.Write(ctx, "/workspace/log.txt", []byte("line2\n"), true); err != nil {
		t.Fatal(err)
	}
	got, err := fs.Read(ctx, "/workspace/log.txt", 0, 0)
	if err != nil || string(got) != "line1\nline2\n" {
		t.Fatalf("append truncated prior content: %q %v", got, err)
	}
	if err := fs.Remove(ctx, "/workspace/seed.txt"); err != nil {
		t.Fatal(err)
	}

	ack, err := m.Reseed(ctx, "test-vmgenid-1234")
	if err != nil || ack.MachineID == "" {
		t.Fatalf("reseed: %+v %v", ack, err)
	}
	pm, ok := m.(*machine)
	if !ok {
		t.Fatal("not a process machine")
	}
	mid, _ := os.ReadFile(filepath.Join(pm.WorkDir(), "etc/machine-id"))
	if len(mid) < 16 {
		t.Fatalf("machine-id not regenerated in workdir: %q", mid)
	}

	if _, err := m.Pause(ctx, t.TempDir()); err != vmm.ErrUnsupported {
		t.Fatalf("pause should be unsupported on process tier, got %v", err)
	}

	if err := m.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-m.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("machine did not die after kill")
	}
}
