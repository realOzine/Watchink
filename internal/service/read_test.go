package service

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"watchink/internal/app"
)

func ms(v int64) *int64 { return &v }

// TestSelectSegments pins the in-memory selection to the two SQL queries it
// stands in for: SegmentsInRange (overlap with a half-open range) and
// SegmentsByIDs (idx order, unknown IDs ignored).
func TestSelectSegments(t *testing.T) {
	all := []app.Segment{
		{SegmentID: "a", Idx: 1, StartMs: 0, EndMs: 1000},
		{SegmentID: "b", Idx: 2, StartMs: 1000, EndMs: 2000},
		{SegmentID: "c", Idx: 3, StartMs: 2000, EndMs: 3000},
		{SegmentID: "d", Idx: 4, StartMs: 3000, EndMs: 4000},
	}
	ids := func(segs []app.Segment) string {
		out := ""
		for _, s := range segs {
			out += s.SegmentID
		}
		return out
	}
	tests := []struct {
		name string
		req  app.GetTranscriptRequest
		want string
	}{
		{"everything", app.GetTranscriptRequest{}, "abcd"},
		{"start is exclusive of segments ending there", app.GetTranscriptRequest{StartMs: ms(1000)}, "bcd"},
		{"start inside a segment keeps it", app.GetTranscriptRequest{StartMs: ms(1500)}, "bcd"},
		{"end is exclusive of segments starting there", app.GetTranscriptRequest{EndMs: ms(2000)}, "ab"},
		{"window", app.GetTranscriptRequest{StartMs: ms(1001), EndMs: ms(2001)}, "bc"},
		{"window past the end", app.GetTranscriptRequest{StartMs: ms(9000)}, ""},
		{"ids come back in idx order", app.GetTranscriptRequest{SegmentIDs: []string{"d", "a"}}, "ad"},
		{"unknown ids are ignored", app.GetTranscriptRequest{SegmentIDs: []string{"zz", "c"}}, "c"},
	}
	for _, tc := range tests {
		if got := ids(selectSegments(all, tc.req)); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestLRU(t *testing.T) {
	c := newLRU[int](2)
	loads := 0
	load := func(v int) func() (int, error) {
		return func() (int, error) { loads++; return v, nil }
	}
	for _, k := range []string{"a", "b", "a"} {
		c.get(k, load(len(k)))
	}
	if loads != 2 {
		t.Fatalf("loads = %d, want 2 (the second read of a is a hit)", loads)
	}
	c.get("c", load(3)) // evicts b: a was used more recently
	if _, ok := c.peek("b"); ok {
		t.Error("b should have been evicted")
	}
	if v, ok := c.peek("a"); !ok || v != 1 {
		t.Errorf("a = %d, %v; want it kept", v, ok)
	}
	// A failed load caches nothing.
	if _, err := c.get("d", func() (int, error) { return 0, errors.New("boom") }); err == nil {
		t.Error("load error was swallowed")
	}
	if _, ok := c.peek("d"); ok {
		t.Error("a failed load must not be cached")
	}
	if !reflect.DeepEqual(len(c.items), c.order.Len()) || c.order.Len() > 2 {
		t.Errorf("cache holds %d entries, cap is 2", c.order.Len())
	}
}

// TestLRUConcurrent hammers a tiny cache from many goroutines so that hits,
// misses, duplicate loads of one key and evictions all interleave. Every
// caller must get the value of its own key, and the cache must stay within
// its capacity. Run with -race.
func TestLRUConcurrent(t *testing.T) {
	c := newLRU[string](4)
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				key := fmt.Sprintf("k%d", (g+i)%9) // 9 keys, 4 slots: constant eviction
				v, err := c.get(key, func() (string, error) { return "v-" + key, nil })
				if err != nil || v != "v-"+key {
					t.Errorf("get(%s) = %q, %v", key, v, err)
					return
				}
				if v, ok := c.peek(key); ok && v != "v-"+key {
					t.Errorf("peek(%s) = %q", key, v)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if c.order.Len() > 4 || len(c.items) != c.order.Len() {
		t.Errorf("cache holds %d entries (%d indexed), cap is 4", c.order.Len(), len(c.items))
	}
}
