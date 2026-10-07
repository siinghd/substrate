package api

import (
	"sync"
	"testing"
	"time"
)

func TestExecWaiterNotifyWakesAllSubscribers(t *testing.T) {
	w := newExecWaiter()
	ch1, done1 := w.subscribe("ex_1")
	defer done1()
	ch2, done2 := w.subscribe("ex_1")
	defer done2()
	chOther, doneOther := w.subscribe("ex_2")
	defer doneOther()

	w.notify("ex_1")
	for _, ch := range []<-chan struct{}{ch1, ch2} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("subscriber not woken by notify")
		}
	}
	select {
	case <-chOther:
		t.Fatal("unrelated exec was woken")
	default:
	}

	// Coalescing: a second notify must not block even though nobody drained
	// the first signal yet.
	done := make(chan struct{})
	go func() { w.notify("ex_1"); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notify blocked on undrained subscriber")
	}
}

func TestExecWaiterUnsubscribeStopsDelivery(t *testing.T) {
	w := newExecWaiter()
	ch, done := w.subscribe("ex_x")
	done() // unsubscribe closes the channel
	if _, ok := <-ch; ok {
		t.Fatal("channel still open after unsubscribe")
	}
	w.notify("ex_x") // must not panic on the removed subscriber
}

func TestExecWaiterConcurrentSubscribeNotify(t *testing.T) {
	w := newExecWaiter()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ch, done := w.subscribe("ex_race")
			w.notify("ex_race")
			<-ch
			done()
		}(i)
	}
	wg.Wait()
}
