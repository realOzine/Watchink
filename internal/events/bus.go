// Package events is the in-memory event bus behind GET /api/v1/events
// (docs/v0.md §13.3): a ring buffer of the most recent events for replay
// after a reconnect, and a non-blocking fan-out to live subscribers.
package events

import (
	"context"
	"sync"
	"time"

	"watchink/internal/app"
)

const (
	// DefaultCapacity is the number of events kept for replay (§13.3).
	DefaultCapacity = 1000
	// DefaultSubscriberQueue is how many undelivered live events one
	// subscriber may accumulate before it is dropped.
	DefaultSubscriberQueue = 256
)

// Bus assigns event IDs, keeps the last events for replay and fans them out.
// All methods are safe for concurrent use.
type Bus struct {
	mu     sync.Mutex
	ring   []app.Event // circular; ring[(head+i)%len] is the i-th oldest
	head   int
	n      int
	lastID int64
	queue  int
	subs   map[*subscriber]struct{}
	closed bool
}

type subscriber struct {
	ch   chan app.Event
	stop func() bool // detaches the context watcher
}

// New returns a bus that replays up to capacity events (DefaultCapacity when
// capacity <= 0).
//
// Event IDs start just above the engine start time in Unix milliseconds, so
// every ID of a previous engine run is smaller than every ID of this run: a
// client reconnecting with a stale Last-Event-ID falls below the replay
// window and is told to reset instead of silently missing events.
func New(capacity int) *Bus {
	return newBus(capacity, DefaultSubscriberQueue, time.Now().UnixMilli())
}

func newBus(capacity, queue int, seed int64) *Bus {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	if queue <= 0 {
		queue = DefaultSubscriberQueue
	}
	return &Bus{
		ring:   make([]app.Event, capacity),
		lastID: seed,
		queue:  queue,
		subs:   map[*subscriber]struct{}{},
	}
}

// Publish assigns the next ID to ev, stores it for replay, delivers it to
// every subscriber and returns it with the ID set. It never blocks: a
// subscriber whose queue is full is dropped (its channel is closed) and is
// expected to reconnect with its last seen ID.
func (b *Bus) Publish(ev app.Event) app.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lastID++
	ev.ID = b.lastID
	if b.closed {
		return ev
	}
	if b.n < len(b.ring) {
		b.ring[(b.head+b.n)%len(b.ring)] = ev
		b.n++
	} else {
		b.ring[b.head] = ev
		b.head = (b.head + 1) % len(b.ring)
	}
	for s := range b.subs {
		select {
		case s.ch <- ev:
		default:
			b.dropLocked(s)
		}
	}
	return ev
}

// Subscribe returns a channel of events with ID > afterID.
//
//   - afterID == 0: live events only, reset is false.
//   - afterID within the replay buffer (including "already up to date"): the
//     missed events are replayed first, then live events follow with no gap
//     and no duplicate; reset is false.
//   - afterID older than the buffer, or greater than the last assigned ID
//     (an ID from another engine run): reset is true — the caller must do a
//     full refresh — and only live events follow.
//
// The channel is closed when ctx ends, when the bus is closed, or when the
// subscriber falls more than the queue size behind. A consumer that sees the
// channel close while its ctx is still alive should resubscribe with the last
// ID it received.
func (b *Bus) Subscribe(ctx context.Context, afterID int64) (ch <-chan app.Event, reset bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var replay []app.Event
	if afterID != 0 {
		// floor is the newest ID a client may hold without having missed
		// an event that is no longer buffered.
		floor := b.lastID
		if b.n > 0 {
			floor = b.ring[b.head].ID - 1
		}
		switch {
		case afterID > b.lastID || afterID < floor:
			reset = true
		default:
			// IDs in the ring are consecutive, so the offset is direct.
			skip := int(afterID - floor)
			replay = make([]app.Event, 0, b.n-skip)
			for i := skip; i < b.n; i++ {
				replay = append(replay, b.ring[(b.head+i)%len(b.ring)])
			}
		}
	}

	// The replay is queued before the lock is released, so the first live
	// event lands right behind it; the extra capacity keeps the replay from
	// counting against the live queue bound.
	s := &subscriber{ch: make(chan app.Event, len(replay)+b.queue)}
	for _, ev := range replay {
		s.ch <- ev
	}
	if b.closed || ctx.Err() != nil {
		close(s.ch)
		return s.ch, reset
	}
	b.subs[s] = struct{}{}
	s.stop = context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.dropLocked(s)
	})
	return s.ch, reset
}

// dropLocked removes s and closes its channel exactly once. Sends happen
// only under b.mu and only to registered subscribers, so closing here cannot
// race with a send.
func (b *Bus) dropLocked(s *subscriber) {
	if _, ok := b.subs[s]; !ok {
		return
	}
	delete(b.subs, s)
	close(s.ch)
	if s.stop != nil {
		s.stop()
	}
}

// Subscribers returns the number of live subscribers (idle exit, §5.4).
func (b *Bus) Subscribers() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs)
}

// LastID returns the most recently assigned event ID (the seed before the
// first Publish).
func (b *Bus) LastID() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastID
}

// Close closes every subscriber channel. Later Subscribe calls return a
// closed channel and later Publish calls are discarded.
func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for s := range b.subs {
		b.dropLocked(s)
	}
}
