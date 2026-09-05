package channel

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testManager(t *testing.T) *Manager {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewManager(testConfig(), logger, nil)
}

// TestEverySubscriberSeesEveryEvent is the property the web UI depends on:
// two surfaces driving this client must not steal each other's events.
func TestEverySubscriberSeesEveryEvent(t *testing.T) {
	m := testManager(t)

	a, closeA := m.Subscribe()
	defer closeA()
	b, closeB := m.Subscribe()
	defer closeB()

	if got := m.SubscriberCount(); got != 2 {
		t.Fatalf("SubscriberCount() = %d, want 2", got)
	}

	m.emit(Event{Kind: EventIncoming, Channel: 1, Text: "from 1001"})

	for name, ch := range map[string]<-chan Event{"a": a, "b": b} {
		select {
		case e := <-ch:
			if e.Text != "from 1001" {
				t.Errorf("subscriber %s got %q", name, e.Text)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %s received nothing", name)
		}
	}
}

// TestSlowSubscriberDoesNotBlockOthers: a consumer that stops reading must lose
// its own events only, and must never stall the manager.
func TestSlowSubscriberDoesNotBlockOthers(t *testing.T) {
	m := testManager(t)

	slow, closeSlow := m.Subscribe() // never read from
	defer closeSlow()
	fast, closeFast := m.Subscribe()
	defer closeFast()

	// Overflow the slow subscriber's buffer several times over.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < eventBuffer*3; i++ {
			m.emit(Event{Text: "x"})
			<-fast // keep the fast one drained
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on a subscriber that stopped reading")
	}

	if len(slow) != eventBuffer {
		t.Errorf("slow subscriber holds %d events, want its buffer full at %d",
			len(slow), eventBuffer)
	}
}

// TestUnsubscribeStopsDelivery guards against delivering to a closed channel.
func TestUnsubscribeStopsDelivery(t *testing.T) {
	m := testManager(t)

	ch, unsubscribe := m.Subscribe()
	unsubscribe()
	unsubscribe() // must be idempotent

	if got := m.SubscriberCount(); got != 0 {
		t.Errorf("SubscriberCount() = %d after unsubscribe, want 0", got)
	}
	// Emitting must not panic on the closed channel.
	m.emit(Event{Text: "after unsubscribe"})

	if _, open := <-ch; open {
		t.Error("channel should be closed and drained")
	}
}

// TestChangeListenersAllFire: both the REPL status line and the web UI need
// telling, so every registered listener must run.
func TestChangeListenersAllFire(t *testing.T) {
	m := testManager(t)

	var mu sync.Mutex
	fired := map[string]int{}
	note := func(name string) func() {
		return func() {
			mu.Lock()
			fired[name]++
			mu.Unlock()
		}
	}

	removeA := m.AddChangeListener(note("a"))
	m.AddChangeListener(note("b"))

	m.stateChanged()

	mu.Lock()
	if fired["a"] != 1 || fired["b"] != 1 {
		t.Errorf("listeners fired %v, want each once", fired)
	}
	mu.Unlock()

	// A removed listener must stop firing while the other continues.
	removeA()
	m.stateChanged()

	mu.Lock()
	defer mu.Unlock()
	if fired["a"] != 1 {
		t.Errorf("removed listener fired again: %v", fired)
	}
	if fired["b"] != 2 {
		t.Errorf("remaining listener did not fire: %v", fired)
	}
}
