package api

import "sync"

// execWaiter is a tiny pub-sub keyed by exec id. Exec streams used to poll
// the store at 10Hz each — on the single-connection SQLite that serialized
// against ALL API traffic once a handful of streams were open (100 streams =
// ~1000 queries/s of pure overhead). The exec-exit path now notifies each
// subscriber exactly once; streams otherwise sleep. Fallback polling remains
// at a low rate to bound any missed wakeup.
//
// Complexity: register/unregister/notify are O(subscribers-per-exec);
// memory is O(open streams) — bounded by live connections, not history.
type execWaiter struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newExecWaiter() *execWaiter {
	return &execWaiter{subs: map[string]map[chan struct{}]struct{}{}}
}

// subscribe registers a wake channel for execID. Returns the channel and an
// unsubscribe func (idempotent, safe via defer).
func (w *execWaiter) subscribe(execID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	w.mu.Lock()
	if w.subs[execID] == nil {
		w.subs[execID] = map[chan struct{}]struct{}{}
	}
	w.subs[execID][ch] = struct{}{}
	w.mu.Unlock()
	return ch, func() {
		// Close under the same mutex notify sends under (see notify).
		w.mu.Lock()
		if set := w.subs[execID]; set != nil {
			delete(set, ch)
			if len(set) == 0 {
				delete(w.subs, execID)
			}
		}
		close(ch)
		w.mu.Unlock()
	}
}

// notify wakes all current subscribers of execID. Sends happen UNDER the
// lock and are non-blocking (cap-1 buffer + default): unsubscribe closes its
// channel while holding the same mutex, so a send can never race a close
// (the previous snapshot-then-send-after-unlock pattern panicked with
// "send on closed channel" when a stream unsubscribed mid-notify).
func (w *execWaiter) notify(execID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for ch := range w.subs[execID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
