package vmm

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// TestBufPipeWriteWakesParkedReader: Write never broadcast, so a reader that
// had drained the buffer stayed parked until CloseWrite — exec output did not
// stream, it arrived in one lump at process exit.
func TestBufPipeWriteWakesParkedReader(t *testing.T) {
	bp := newBufPipe(1 << 20)

	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, err := bp.Read(buf)
		if err != nil {
			got <- "err:" + err.Error()
			return
		}
		got <- string(buf[:n])
	}()

	// Let the reader park on the empty buffer before anything is written.
	time.Sleep(50 * time.Millisecond)
	if _, err := bp.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if s != "hello" {
			t.Fatalf("read %q, want %q", s, "hello")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reader never woke: Write does not signal the condition variable, so output cannot stream")
	}
}

// TestBufPipeDoesNotDeadlockPastLimit: past the limit the writer parks too.
// With no wakeup from Write, both goroutines slept forever — and because the
// writer is the exec pump, its proto stream stopped being drained and
// head-of-line blocked every other operation on that sandbox.
func TestBufPipeDoesNotDeadlockPastLimit(t *testing.T) {
	const limit = 64 << 10
	const total = 5 * limit // comfortably past the high-water mark

	bp := newBufPipe(limit)
	var out bytes.Buffer
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		// Park on empty first, exactly as copyExecOutput does.
		time.Sleep(50 * time.Millisecond)
		_, _ = io.Copy(&out, bp)
	}()

	payload := bytes.Repeat([]byte("x"), 4096)
	done := make(chan error, 1)
	go func() {
		written := 0
		for written < total {
			n, err := bp.Write(payload)
			if err != nil {
				done <- err
				return
			}
			written += n
		}
		bp.CloseWrite()
		done <- nil
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("DEADLOCK: writer parked at the limit and the reader was never woken — an exec producing more than the limit wedges the sandbox")
	}

	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("reader never finished after CloseWrite")
	}
	if out.Len() != total {
		t.Fatalf("read back %d bytes, wrote %d", out.Len(), total)
	}
}

// TestBufPipeMemoryStaysBounded: backpressure must still apply — the point of
// the wakeup fix is not to turn the pipe into an unbounded buffer.
func TestBufPipeMemoryStaysBounded(t *testing.T) {
	const limit = 8 << 10
	bp := newBufPipe(limit)
	payload := bytes.Repeat([]byte("y"), 1024)

	// Fill past the limit with no reader; the next Write must block.
	for i := 0; i < limit/1024; i++ {
		if _, err := bp.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
	blocked := make(chan struct{})
	go func() { _, _ = bp.Write(payload); close(blocked) }()
	select {
	case <-blocked:
		t.Fatal("Write past the limit did not block: backpressure is gone and memory is unbounded")
	case <-time.After(200 * time.Millisecond):
	}
	// Draining releases it.
	buf := make([]byte, 4096)
	if _, err := bp.Read(buf); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(3 * time.Second):
		t.Fatal("Read did not wake the blocked writer")
	}
}
