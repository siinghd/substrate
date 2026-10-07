package process

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/substrate-sh/substrate/internal/proto"
	"github.com/substrate-sh/substrate/internal/vmm"
)

// startTestMachine boots the real shim via the process driver so fs/exec
// tests exercise the actual SGP/1 wire path.
func startTestMachine(t *testing.T) vmm.Machine {
	t.Helper()
	dir := t.TempDir()
	shimBin := buildShimBinary(t, dir)
	d := NewDriver(shimBin)
	m, err := d.Create(context.Background(), vmm.Spec{
		ID:      "sb_fschunktest01",
		WorkDir: dir,
	})
	if err != nil {
		t.Fatalf("create machine: %v", err)
	}
	t.Cleanup(func() { m.Kill() })
	return m
}

func buildShimBinary(t *testing.T, dir string) string {
	t.Helper()
	bin := filepath.Join(dir, "shim-test-bin")
	wd, _ := os.Getwd()
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
			break
		}
		wd = filepath.Dir(wd)
	}
	build := exec.Command("go", "build", "-o", bin, "./cmd/shim")
	build.Dir = wd
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build shim: %v\n%s", err, out)
	}
	return bin
}

// TestFsReadBeyondFrameCap: reading a multi-MiB file must return every byte.
// The old shim sent the whole file as ONE frame; anything over 1MiB failed
// the frame cap and the client silently got OK + size with NO data.
func TestFsReadBeyondFrameCap(t *testing.T) {
	m := startTestMachine(t)
	ctx := context.Background()

	want := make([]byte, 3*1024*1024+777) // > 1MiB frame cap, not a multiple of it
	for i := range want {
		want[i] = byte(i * 31)
	}
	guestPath := "/big.bin"
	os.WriteFile(filepath.Join(workdirOf(m), "big.bin"), want, 0o644)

	got, err := m.Fs().Read(ctx, guestPath, 0, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("read returned %d bytes, want %d (mismatch beyond frame cap)", len(got), len(want))
	}
}

// TestFsReadOffsetLengthWindow checks seek+limit against the chunked reader.
func TestFsReadOffsetLengthWindow(t *testing.T) {
	m := startTestMachine(t)
	ctx := context.Background()

	body := bytes.Repeat([]byte("abcdefgh"), 400*1024) // 3.2MiB
	os.WriteFile(filepath.Join(workdirOf(m), "win.bin"), body, 0o644)

	got, err := m.Fs().Read(ctx, "/win.bin", 10, 100)
	if err != nil {
		t.Fatalf("read window: %v", err)
	}
	if !bytes.Equal(got, body[10:110]) {
		t.Fatalf("window read mismatch: got %d bytes", len(got))
	}
}

// TestFsWriteBeyondFrameCap: writing multi-MiB bodies must land exactly.
// The old host client sent one frame and failed on anything over 1MiB.
func TestFsWriteBeyondFrameCap(t *testing.T) {
	m := startTestMachine(t)
	ctx := context.Background()

	want := make([]byte, 2*1024*1024+12345)
	rand.Read(want)

	if err := m.Fs().Write(ctx, "/uploaded.bin", want, false); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(workdirOf(m), "uploaded.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("wrote %d bytes on disk, want %d", len(got), len(want))
	}
}

// TestFsListCapsEntries: a huge directory must not overrun the frame cap or
// hang the response.
func TestFsListCapsEntries(t *testing.T) {
	m := startTestMachine(t)
	dir := filepath.Join(workdirOf(m), "many")
	os.MkdirAll(dir, 0o755)
	for i := 0; i < 2500; i++ {
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("entry-%05d", i)), []byte("x"), 0o644)
	}

	done := make(chan int, 1)
	go func() {
		es, err := m.Fs().List(context.Background(), "/many")
		if err != nil {
			t.Errorf("list: %v", err)
			done <- -1
			return
		}
		done <- len(es)
	}()
	select {
	case n := <-done:
		if n < 0 {
			return
		}
		if n > proto.MaxFrameSize { // sanity: never
			t.Fatalf("impossible entry count %d", n)
		}
		if n != 2000 {
			t.Fatalf("list returned %d entries, want the 2000 cap", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("list of 2500-entry directory timed out — unbounded response likely overran the frame cap")
	}
}

// TestExecChattyStderrNoDeadlock pins the concurrent-drain contract: a child
// flooding stderr while stdout stays silent used to wedge both pipes shut
// when they were drained sequentially.
func TestExecChattyStderrNoDeadlock(t *testing.T) {
	m := startTestMachine(t)

	h, err := m.Exec(context.Background(), vmm.ExecSpec{
		Argv: []string{"/bin/sh", "-c", "i=0; while [ $i -lt 3000 ]; do echo to-stderr-$i >&2; i=$((i+1)); done"},
	})
	if err != nil {
		t.Fatal(err)
	}
	drained := make(chan int64, 1)
	go func() {
		var total int64
		buf := make([]byte, 64<<10)
		for {
			n, err := h.Stdout.Read(buf)
			total += int64(n)
			if err != nil {
				drained <- total
				return
			}
		}
	}()
	select {
	case <-drained:
		// exit observed, pipes closed — no deadlock
	case <-time.After(20 * time.Second):
		t.Fatal("exec with chatty stderr deadlocked (sequential drain regression)")
	}
	ei := <-h.ExitCh
	if ei.Code != 0 {
		t.Fatalf("exit code %d", ei.Code)
	}
}

func workdirOf(m vmm.Machine) string { return m.(*machine).WorkDir() }
