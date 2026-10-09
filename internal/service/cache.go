package service

import (
	"container/list"
	"sync"
)

// lru is a small least-recently-used cache for data that never changes once
// written: transcript versions and note versions are immutable (§6.3), so
// their rows can be kept in memory without any invalidation. It exists
// because reading them back row by row from SQLite dominates the read API
// (see internal/bench).
type lru[V any] struct {
	mu    sync.Mutex
	cap   int
	order *list.List // front = most recently used
	items map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func newLRU[V any](capacity int) *lru[V] {
	return &lru[V]{cap: capacity, order: list.New(), items: make(map[string]*list.Element, capacity)}
}

// peek returns the cached value for key without loading it.
func (c *lru[V]) peek(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		var zero V
		return zero, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*lruEntry[V]).value, true
}

// get returns the cached value for key, loading and storing it on a miss.
// Concurrent misses for the same key may both load; the values are equal, so
// the last one simply wins.
func (c *lru[V]) get(key string, load func() (V, error)) (V, error) {
	c.mu.Lock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		v := el.Value.(*lruEntry[V]).value
		c.mu.Unlock()
		return v, nil
	}
	c.mu.Unlock()

	v, err := load()
	if err != nil {
		return v, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.order.MoveToFront(el)
		el.Value.(*lruEntry[V]).value = v
		return v, nil
	}
	c.items[key] = c.order.PushFront(&lruEntry[V]{key, v})
	if c.order.Len() > c.cap {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.items, oldest.Value.(*lruEntry[V]).key)
	}
	return v, nil
}
