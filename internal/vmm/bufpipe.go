package vmm

import (
	"bytes"
	"io"
	"sync"
)

// bufPipe is a bounded asynchronous pipe: writers never deadlock on a missing
// reader, but backpressure applies once limit bytes are outstanding. Memory is
// O(outstanding bytes), independent of total volume — bounded by limit plus at
// most one in-flight write, since a Write admitted under the limit is accepted
// whole rather than split.
//
// Both Read and Write signal the condition variable. Either one alone is a
// lost wakeup: the buffer has a "not empty" waiter and a "not full" waiter, and
// whichever side changes the state must wake the other.
type bufPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    bytes.Buffer
	limit  int
	closed bool
	rerr   error
}

func newBufPipe(limit int) *bufPipe {
	b := &bufPipe{limit: limit}
	b.cond = sync.NewCond(&b.mu)
	return b
}

func (b *bufPipe) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() >= b.limit && !b.closed {
		b.cond.Wait()
	}
	if b.closed {
		return 0, io.ErrClosedPipe
	}
	n, err := b.buf.Write(p)
	// A reader parked on an empty buffer is woken by NOTHING else: Read is the
	// only other broadcaster and it is the goroutine that is asleep. Without
	// this the reader stayed parked until CloseWrite — i.e. until the process
	// exited, so exec output never streamed — and once the writer also hit the
	// limit and parked, both slept forever and the exec's proto stream stopped
	// being drained.
	b.cond.Broadcast()
	return n, err
}

func (b *bufPipe) Read(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for b.buf.Len() == 0 && !b.closed {
		b.cond.Wait()
	}
	if b.buf.Len() == 0 {
		if b.rerr != nil {
			return 0, b.rerr
		}
		return 0, io.EOF
	}
	n, err := b.buf.Read(p)
	b.cond.Broadcast()
	return n, err
}

func (b *bufPipe) CloseWrite() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.cond.Broadcast()
}

func (b *bufPipe) Close() error {
	b.CloseWithError(nil)
	return nil
}

func (b *bufPipe) CloseWithError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	if err != nil {
		b.rerr = err
	}
	b.cond.Broadcast()
}
