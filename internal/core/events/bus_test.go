package events

import (
	"context"
	"sync"
	"testing"
	"time"

	"watchink/internal/app"
)

const seed = int64(1_000_000)

func ev() app.Event { return app.Event{Type: app.EventJobUpdated} }

// recv reads one event or fails after a timeout.
func recv(t *testing.T, ch <-chan app.Event) app.Event {
	t.Helper()
	select {
	case e, ok := <-ch:
		if !ok {
			t.Fatal("channel closed, expected an event")
		}
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return app.Event{}
}

func expectIDs(t *testing.T, ch <-chan app.Event, ids ...int64) {
	t.Helper()
	for _, want := range ids {
		if got := recv(t, ch).ID; got != want {
			t.Fatalf("event ID = %d, want %d", got, want)
		}
	}
}

func expectNone(t *testing.T, ch <-chan app.Event) {
	t.Helper()
	select {
	case e, ok := <-ch:
		t.Fatalf("unexpected receive: %+v (open=%v)", e, ok)
	case <-time.After(30 * time.Millisecond):
	}
}

func expectClosed(t *testing.T, ch <-chan app.Event) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("channel was not closed")
		}
	}
}

func waitSubscribers(t *testing.T, b *Bus, want int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if b.Subscribers() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("Subscribers() = %d, want %d", b.Subscribers(), want)
}

func TestNewSeedsIDsFromWallClock(t *testing.T) {
	before := time.Now().UnixMilli()
	b := New(0)
	after := time.Now().UnixMilli()
	if len(b.ring) != DefaultCapacity {
		t.Fatalf("default capacity = %d", len(b.ring))
	}
	first := b.Publish(ev()).ID
	if first <= before || first > after+1 {
		t.Fatalf("first ID %d not seeded from the clock [%d, %d]", first, before, after)
	}
	if second := b.Publish(ev()).ID; second != first+1 {
		t.Fatalf("IDs not consecutive: %d then %d", first, second)
	}
	if b.LastID() != first+1 {
		t.Fatalf("LastID = %d", b.LastID())
	}
	// An ID from an earlier engine run (smaller seed) must reset.
	_, reset := b.Subscribe(t.Context(), before-10_000)
	if !reset {
		t.Fatal("an ID from a previous run must trigger a reset")
	}
}

func TestPublishKeepsPayload(t *testing.T) {
	b := newBus(4, 4, seed)
	ch, _ := b.Subscribe(t.Context(), 0)
	job := &app.Job{JobID: "job_1", Rev: 7}
	out := b.Publish(app.Event{Type: app.EventJobUpdated, Job: job})
	got := recv(t, ch)
	if got.ID != out.ID || got.Type != app.EventJobUpdated || got.Job != job {
		t.Fatalf("payload changed: %+v", got)
	}
}

func TestSubscribeLiveOnly(t *testing.T) {
	b := newBus(10, 10, seed)
	b.Publish(ev())
	b.Publish(ev())
	ch, reset := b.Subscribe(t.Context(), 0)
	if reset {
		t.Fatal("afterID 0 must not reset")
	}
	expectNone(t, ch)
	b.Publish(ev())
	expectIDs(t, ch, seed+3)
}

func TestReplayBoundaries(t *testing.T) {
	// Capacity 5, 8 events published: the buffer holds seed+4 .. seed+8.
	const capN, total = 5, 8
	tests := []struct {
		name      string
		afterID   int64
		wantReset bool
		replay    []int64
	}{
		{"oldest replayable (just before the buffer)", seed + 3, false, []int64{seed + 4, seed + 5, seed + 6, seed + 7, seed + 8}},
		{"first buffered", seed + 4, false, []int64{seed + 5, seed + 6, seed + 7, seed + 8}},
		{"middle", seed + 6, false, []int64{seed + 7, seed + 8}},
		{"up to date", seed + 8, false, nil},
		{"one too old", seed + 2, true, nil},
		{"evicted long ago", seed + 1, true, nil},
		{"the seed itself", seed, true, nil},
		{"previous engine run", seed - 500, true, nil},
		{"negative", -1, true, nil},
		{"from the future", seed + 9, true, nil},
		{"far future", seed + 1_000_000, true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := newBus(capN, 4, seed)
			for i := 0; i < total; i++ {
				b.Publish(ev())
			}
			ch, reset := b.Subscribe(t.Context(), tc.afterID)
			if reset != tc.wantReset {
				t.Fatalf("reset = %v, want %v", reset, tc.wantReset)
			}
			// A live event published after Subscribe must follow the
			// replay directly: no gap, no duplicate.
			live := b.Publish(ev()).ID
			expectIDs(t, ch, append(append([]int64{}, tc.replay...), live)...)
			expectNone(t, ch)
		})
	}
}

func TestReplayBeforeAnyEviction(t *testing.T) {
	b := newBus(10, 4, seed)
	// Nothing published yet: only the seed is "up to date".
	if _, reset := b.Subscribe(t.Context(), seed); reset {
		t.Fatal("seed with an empty buffer must not reset")
	}
	if _, reset := b.Subscribe(t.Context(), seed-1); !reset {
		t.Fatal("an older ID with an empty buffer must reset")
	}
	if _, reset := b.Subscribe(t.Context(), seed+1); !reset {
		t.Fatal("a future ID with an empty buffer must reset")
	}
	b.Publish(ev())
	b.Publish(ev())
	b.Publish(ev())
	ch, reset := b.Subscribe(t.Context(), seed+1)
	if reset {
		t.Fatal("unexpected reset")
	}
	expectIDs(t, ch, seed+2, seed+3)
	// Every event of this run is still buffered, so the seed replays all.
	ch, reset = b.Subscribe(t.Context(), seed)
	if reset {
		t.Fatal("unexpected reset")
	}
	expectIDs(t, ch, seed+1, seed+2, seed+3)
}

func TestReplayLargerThanQueue(t *testing.T) {
	// The replay must not count against the live queue bound.
	b := newBus(100, 2, seed)
	for i := 0; i < 100; i++ {
		b.Publish(ev())
	}
	ch, reset := b.Subscribe(t.Context(), seed)
	if reset {
		t.Fatal("unexpected reset")
	}
	b.Publish(ev())
	b.Publish(ev())
	for i := int64(1); i <= 102; i++ {
		expectIDs(t, ch, seed+i)
	}
	if b.Subscribers() != 1 {
		t.Fatal("subscriber dropped although it stayed within its queue")
	}
}

func TestContextCancelClosesChannel(t *testing.T) {
	b := newBus(10, 10, seed)
	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := b.Subscribe(ctx, 0)
	other, _ := b.Subscribe(t.Context(), 0)
	if b.Subscribers() != 2 {
		t.Fatalf("Subscribers = %d", b.Subscribers())
	}
	cancel()
	expectClosed(t, ch)
	waitSubscribers(t, b, 1)
	// Publishing after a subscriber left must not panic or block.
	b.Publish(ev())
	expectIDs(t, other, seed+1)

	// An already-cancelled context yields a closed channel immediately.
	dead, _ := b.Subscribe(ctx, 0)
	expectClosed(t, dead)
	if b.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d", b.Subscribers())
	}
}

func TestSlowSubscriberIsDropped(t *testing.T) {
	const queue = 3
	b := newBus(100, queue, seed)
	slow, _ := b.Subscribe(t.Context(), 0)
	fast, _ := b.Subscribe(t.Context(), 0)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			b.Publish(ev())
			recv(t, fast) // the fast subscriber keeps up
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	if b.Subscribers() != 1 {
		t.Fatalf("Subscribers = %d, want 1 (slow one dropped)", b.Subscribers())
	}
	// The slow subscriber still drains what was queued, then sees the close.
	var last int64
	n := 0
	for e := range slow {
		n++
		last = e.ID
	}
	if n != queue || last != seed+queue {
		t.Fatalf("slow subscriber got %d events ending at %d, want %d ending at %d", n, last, queue, seed+queue)
	}
	// Reconnecting with the last seen ID recovers the rest without a reset.
	ch, reset := b.Subscribe(t.Context(), last)
	if reset {
		t.Fatal("reconnect inside the buffer must not reset")
	}
	for id := last + 1; id <= seed+20; id++ {
		expectIDs(t, ch, id)
	}
}

func TestClose(t *testing.T) {
	b := newBus(10, 10, seed)
	ch, _ := b.Subscribe(t.Context(), 0)
	b.Publish(ev())
	b.Close()
	b.Close() // idempotent
	expectIDs(t, ch, seed+1)
	expectClosed(t, ch)
	if b.Subscribers() != 0 {
		t.Fatalf("Subscribers = %d", b.Subscribers())
	}
	if id := b.Publish(ev()).ID; id != seed+2 {
		t.Fatalf("Publish after Close returned ID %d", id)
	}
	late, _ := b.Subscribe(t.Context(), 0)
	expectClosed(t, late)
}

// TestConcurrent publishes from several goroutines while subscribers come
// and go; every subscriber must observe strictly consecutive IDs.
func TestConcurrent(t *testing.T) {
	const publishers, perPublisher, subscribers = 4, 500, 8
	total := publishers * perPublisher
	b := newBus(total, total, seed) // nobody is dropped, nothing is evicted

	var wg sync.WaitGroup
	errs := make(chan string, subscribers*2)

	consume := func(afterID int64) {
		defer wg.Done()
		if afterID == seed+int64(total) {
			// A late joiner that arrives after the last publish has
			// nothing left to observe (fast machines without -race).
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ch, reset := b.Subscribe(ctx, afterID)
		if reset {
			errs <- "unexpected reset"
			return
		}
		var prev int64
		timeout := time.After(10 * time.Second)
		for {
			select {
			case e, ok := <-ch:
				if !ok {
					errs <- "channel closed early"
					return
				}
				if prev != 0 && e.ID != prev+1 {
					errs <- "gap or duplicate"
					return
				}
				if afterID != 0 && prev == 0 && e.ID != afterID+1 {
					errs <- "replay did not start right after afterID"
					return
				}
				prev = e.ID
				if e.ID == seed+int64(total) {
					return
				}
			case <-timeout:
				errs <- "timed out"
				return
			}
		}
	}

	for i := 0; i < subscribers; i++ {
		wg.Add(1)
		go consume(seed) // replay everything published so far, then live
	}
	var pubs sync.WaitGroup
	for p := 0; p < publishers; p++ {
		pubs.Add(1)
		go func() {
			defer pubs.Done()
			for i := 0; i < perPublisher; i++ {
				b.Publish(ev())
				if i%50 == 0 {
					// Churn: short-lived subscribers and counters.
					ctx, cancel := context.WithCancel(context.Background())
					b.Subscribe(ctx, 0)
					b.Subscribers()
					cancel()
				}
			}
		}()
	}
	// Late joiners replay from the middle while publishing continues.
	for i := 0; i < subscribers; i++ {
		wg.Add(1)
		time.Sleep(time.Millisecond)
		go consume(b.LastID())
	}
	pubs.Wait()
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	waitSubscribers(t, b, 0)
	if b.LastID() != seed+int64(total) {
		t.Fatalf("LastID = %d", b.LastID())
	}
}
