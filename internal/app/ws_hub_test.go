package app

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

func TestWSHubFanOutsToAllSubscribers(t *testing.T) {
	t.Parallel()
	h := newWSHub()
	chA, cancelA := h.subscribe()
	defer cancelA()
	chB, cancelB := h.subscribe()
	defer cancelB()

	ev := wsEvent{Kind: "ping", At: time.Now().UTC(), Data: json.RawMessage(`{}`)}
	h.publish(ev)

	for _, c := range []<-chan wsEvent{chA, chB} {
		select {
		case got := <-c:
			if got.Kind != "ping" {
				t.Fatalf("got kind=%q, want ping", got.Kind)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber did not receive event")
		}
	}
}

func TestWSHubCancelRemovesSubscriber(t *testing.T) {
	t.Parallel()
	h := newWSHub()
	_, cancel := h.subscribe()
	if got := h.subscriberCount(); got != 1 {
		t.Fatalf("subscriberCount=%d, want 1", got)
	}
	cancel()
	if got := h.subscriberCount(); got != 0 {
		t.Fatalf("subscriberCount=%d after cancel, want 0", got)
	}
	// Idempotent: a second cancel must not panic or remove any other
	// subscriber that may exist.
	cancel()
}

func TestWSHubDropsSlowSubscriberWithoutBlocking(t *testing.T) {
	t.Parallel()
	h := newWSHub()

	// One slow subscriber that never reads.
	_, cancelSlow := h.subscribe()
	defer cancelSlow()

	// One fast subscriber that drains.
	chFast, cancelFast := h.subscribe()
	defer cancelFast()

	// Publish more events than the per-subscriber buffer holds. Each call
	// must return immediately even though the slow subscriber is full.
	const burst = wsSubscriberBuffer * 4
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < burst; i++ {
			h.publish(wsEvent{Kind: "tick"})
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publish blocked behind slow subscriber")
	}

	// The fast subscriber should still receive at least the buffer's worth.
	got := drain(chFast, wsSubscriberBuffer, time.Second)
	if got < wsSubscriberBuffer {
		t.Fatalf("fast subscriber received only %d of >=%d events", got, wsSubscriberBuffer)
	}
}

func TestWSHubConcurrentSubscribePublish(t *testing.T) {
	t.Parallel()
	h := newWSHub()
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Publisher goroutine: keeps publishing until stopped.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				h.publish(wsEvent{Kind: "tick"})
			}
		}
	}()

	// Many subscribers churning subscribe/cancel.
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				ch, cancel := h.subscribe()
				select {
				case <-ch:
				case <-time.After(100 * time.Millisecond):
				}
				cancel()
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if got := h.subscriberCount(); got != 0 {
		t.Fatalf("leftover subscribers: %d", got)
	}
}

func drain(ch <-chan wsEvent, want int, timeout time.Duration) int {
	deadline := time.After(timeout)
	got := 0
	for got < want {
		select {
		case _, ok := <-ch:
			if !ok {
				return got
			}
			got++
		case <-deadline:
			return got
		}
	}
	return got
}
