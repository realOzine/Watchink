package client

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/transport/api"
	"watchink/internal/transport/api/apitest"
)

func recv(t *testing.T, ch <-chan app.Event) app.Event {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event within 5 s")
	}
	return app.Event{}
}

func wantClosed(t *testing.T, ch <-chan app.Event) {
	t.Helper()
	select {
	case ev, ok := <-ch:
		if ok {
			t.Fatalf("unexpected event %+v", ev)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("event channel not closed")
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSubscribeDeliversTypedEvents(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()

	events, reset, err := r.c.Subscribe(ctx, 0)
	if err != nil || reset {
		t.Fatalf("Subscribe: reset=%v err=%v", reset, err)
	}
	waitUntil(t, func() bool { return r.h.Stats().Subscribers == 1 })

	vid := "vid_1"
	r.fake.Emit(app.Event{Type: app.EventJobUpdated, Job: &app.Job{JobID: "job_1", VideoID: &vid, Status: app.JobRunning, Rev: 17, Progress: 0.42,
		Stages: []app.JobStage{{Stage: app.StageAcquire, Status: app.StageSucceeded, Progress: 1}}}})
	r.fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: vid, Title: "多行\n标题", Status: app.VideoReady}})
	r.fake.Emit(app.Event{Type: app.EventToolsUpdated, Tools: &app.ToolsStatus{Ready: true, Items: []app.ToolItem{{Name: "ffmpeg", Status: app.ToolReady}}}})

	ev := recv(t, events)
	if ev.ID != 1 || ev.Type != app.EventJobUpdated || ev.Job == nil || ev.Job.Rev != 17 || *ev.Job.VideoID != vid ||
		ev.Job.Stages[0].Stage != app.StageAcquire || ev.Video != nil || ev.Tools != nil {
		t.Fatalf("job event = %+v", ev)
	}
	ev = recv(t, events)
	if ev.ID != 2 || ev.Type != app.EventVideoUpdated || ev.Video == nil || ev.Video.Title != "多行\n标题" || ev.Job != nil {
		t.Fatalf("video event = %+v", ev)
	}
	ev = recv(t, events)
	if ev.ID != 3 || ev.Type != app.EventToolsUpdated || ev.Tools == nil || !ev.Tools.Ready || ev.Tools.Items[0].Name != "ffmpeg" {
		t.Fatalf("tools event = %+v", ev)
	}

	// Cancelling the context closes the channel and frees the server side.
	cancel()
	wantClosed(t, events)
	waitUntil(t, func() bool { return r.h.Stats().Subscribers == 0 && r.fake.SubscriberCount() == 0 })
}

func TestSubscribeResumeAndReset(t *testing.T) {
	r := newRig(t)
	r.fake.ReplayCap = 5
	for i := 0; i < 8; i++ { // IDs 1..8, buffer keeps 4..8
		r.fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_1"}})
	}

	// Resume inside the buffer: only the newer events are replayed.
	ctx1, cancel1 := context.WithCancel(ctxT(t))
	events, reset, err := r.c.Subscribe(ctx1, 6)
	if err != nil || reset {
		t.Fatalf("resume: reset=%v err=%v", reset, err)
	}
	if a, b := recv(t, events), recv(t, events); a.ID != 7 || b.ID != 8 {
		t.Fatalf("replayed %d, %d; want 7, 8", a.ID, b.ID)
	}
	call, _ := r.fake.LastCall("Subscribe")
	if call.Args[0] != int64(6) {
		t.Fatalf("service saw afterID %v", call.Args[0])
	}
	cancel1()
	wantClosed(t, events)

	// Older than the buffer: reset, then only live events.
	ctx2, cancel2 := context.WithCancel(ctxT(t))
	defer cancel2()
	events, reset, err = r.c.Subscribe(ctx2, 1)
	if err != nil || !reset {
		t.Fatalf("stale position: reset=%v err=%v", reset, err)
	}
	waitUntil(t, func() bool { return r.fake.SubscriberCount() == 1 })
	r.fake.Emit(app.Event{Type: app.EventEngineStopping, Reason: "idle"})
	ev := recv(t, events)
	if ev.ID != 9 || ev.Type != app.EventEngineStopping || ev.Reason != "idle" {
		t.Fatalf("after reset got %+v (the initial reset must not be delivered as an event)", ev)
	}
	// engine.stopping ends the subscription without reconnect attempts.
	wantClosed(t, events)

	// Ahead of this engine run (IDs restart after an engine restart): reset.
	ctx3, cancel3 := context.WithCancel(ctxT(t))
	defer cancel3()
	_, reset, err = r.c.Subscribe(ctx3, 5000)
	if err != nil || !reset {
		t.Fatalf("future position: reset=%v err=%v", reset, err)
	}

	r.fake.SetError("Subscribe", app.E(app.CodeInternal, "boom"))
	_, _, err = r.c.Subscribe(ctxT(t), 0)
	wantCode(t, err, app.CodeInternal)
}

func TestSubscribeHeartbeatKeepsIdleStreamAlive(t *testing.T) {
	// Heartbeat 20 ms, client watchdog 300 ms: a silent second must not
	// trigger a reconnect.
	r := newRig(t)
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()
	events, _, err := r.c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if n := r.fake.CallCount("Subscribe"); n != 1 {
		t.Fatalf("stream was reconnected %d times despite heartbeats", n-1)
	}
	r.fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_1"}})
	if ev := recv(t, events); ev.ID != 1 {
		t.Fatalf("event after idle period: %+v", ev)
	}
}

func TestSubscribeWatchdogReconnectsSilentStream(t *testing.T) {
	// No heartbeat at all: the watchdog must drop the dead connection and
	// resume from the last seen ID.
	fake := apitest.New()
	h := api.NewHandler(api.Options{Service: fake, Token: rtToken, HeartbeatInterval: time.Hour})
	srv := httptest.NewServer(h)
	defer func() { h.Close(); srv.CloseClientConnections(); srv.Close() }()
	c := New(Options{BaseURL: srv.URL, Token: rtToken, StreamIdleTimeout: 150 * time.Millisecond})
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()

	events, _, err := c.Subscribe(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_1"}})
	if ev := recv(t, events); ev.ID != 1 {
		t.Fatalf("first event: %+v", ev)
	}
	waitUntil(t, func() bool { return fake.CallCount("Subscribe") >= 2 })
	call, _ := fake.LastCall("Subscribe")
	if call.Args[0] != int64(1) {
		t.Fatalf("reconnect resumed from %v, want 1", call.Args[0])
	}
	fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_2"}})
	if ev := recv(t, events); ev.ID != 2 || ev.Video.VideoID != "vid_2" {
		t.Fatalf("event after reconnect: %+v", ev)
	}
}

// flakyProxy is a TCP proxy whose live connections can be cut, to simulate
// a dropped event stream.
type flakyProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  []net.Conn
}

func newFlakyProxy(t *testing.T, target string) *flakyProxy {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &flakyProxy{ln: ln, target: target}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp4", target)
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go func() { _, _ = copyConn(up, c) }()
			go func() { _, _ = copyConn(c, up) }()
		}
	}()
	t.Cleanup(func() { ln.Close(); p.cut() })
	return p
}

func copyConn(dst, src net.Conn) (int64, error) {
	defer dst.Close()
	buf := make([]byte, 32<<10)
	var n int64
	for {
		m, err := src.Read(buf)
		if m > 0 {
			if _, werr := dst.Write(buf[:m]); werr != nil {
				return n, werr
			}
			n += int64(m)
		}
		if err != nil {
			return n, err
		}
	}
}

func (p *flakyProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		c.Close()
	}
	p.conns = nil
}

func TestSubscribeReconnectsWithLastEventID(t *testing.T) {
	fake := apitest.New()
	fake.ReplayCap = 3
	// Port 0: the client reaches the handler through the proxy's port.
	h := api.NewHandler(api.Options{Service: fake, Token: rtToken, HeartbeatInterval: 20 * time.Millisecond})
	srv := httptest.NewServer(h)
	defer func() { h.Close(); srv.CloseClientConnections(); srv.Close() }()
	proxy := newFlakyProxy(t, srv.Listener.Addr().String())
	c := New(Options{BaseURL: "http://" + proxy.ln.Addr().String(), Token: rtToken})
	defer c.Close()
	ctx, cancel := context.WithCancel(ctxT(t))
	defer cancel()

	events, reset, err := c.Subscribe(ctx, 0)
	if err != nil || reset {
		t.Fatalf("Subscribe: %v %v", reset, err)
	}
	emit := func() int64 {
		return fake.Emit(app.Event{Type: app.EventVideoUpdated, Video: &app.Video{VideoID: "vid_1"}})
	}
	emit()
	emit()
	if a, b := recv(t, events), recv(t, events); a.ID != 1 || b.ID != 2 {
		t.Fatalf("got %d, %d", a.ID, b.ID)
	}

	// Drop the connection; an event emitted while disconnected is replayed.
	waitUntil(t, func() bool { return fake.SubscriberCount() == 1 })
	proxy.cut()
	waitUntil(t, func() bool { return fake.SubscriberCount() == 0 })
	emit() // 3
	if ev := recv(t, events); ev.ID != 3 || ev.Type != app.EventVideoUpdated {
		t.Fatalf("after reconnect: %+v", ev)
	}
	call, _ := fake.LastCall("Subscribe")
	if call.Args[0] != int64(2) {
		t.Fatalf("reconnect sent Last-Event-ID %v, want 2", call.Args[0])
	}

	// Drop again and let the replay buffer (3 events) overflow: the client
	// must be told to reset, then keep receiving live events.
	waitUntil(t, func() bool { return fake.SubscriberCount() == 1 })
	proxy.cut()
	waitUntil(t, func() bool { return fake.SubscriberCount() == 0 })
	for i := 0; i < 6; i++ {
		emit() // 4..9; buffer keeps 7..9, position 3 is lost
	}
	if ev := recv(t, events); ev.Type != EventReset || ev.ID != 0 {
		t.Fatalf("want a reset event after an unresumable reconnect, got %+v", ev)
	}
	waitUntil(t, func() bool { return fake.SubscriberCount() == 1 })
	if id := emit(); id != 10 {
		t.Fatalf("emit id %d", id)
	}
	if ev := recv(t, events); ev.ID != 10 {
		t.Fatalf("live event after reset: %+v", ev)
	}
}

func TestSubscribeGivesUpWhenEngineIsGone(t *testing.T) {
	old := reconnectDelays
	reconnectDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	defer func() { reconnectDelays = old }()

	fake := apitest.New()
	h := api.NewHandler(api.Options{Service: fake, Token: rtToken, HeartbeatInterval: 20 * time.Millisecond})
	srv := httptest.NewServer(h)
	c := New(Options{BaseURL: srv.URL, Token: rtToken})
	events, _, err := c.Subscribe(ctxT(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	// The engine dies without saying goodbye.
	srv.CloseClientConnections()
	srv.Listener.Close()
	wantClosed(t, events)
	h.Close()
	srv.Close()

	// A new engine run on the same address rejects the old token: stop too.
	fake2 := apitest.New()
	h2 := api.NewHandler(api.Options{Service: fake2, Token: rtToken, HeartbeatInterval: 20 * time.Millisecond})
	var mu sync.Mutex
	current := http.Handler(h2)
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hh := current
		mu.Unlock()
		hh.ServeHTTP(w, r)
	}))
	defer srv2.Close()
	c2 := New(Options{BaseURL: srv2.URL, Token: rtToken})
	events, _, err = c2.Subscribe(ctxT(t), 0)
	if err != nil {
		t.Fatal(err)
	}
	h3 := api.NewHandler(api.Options{Service: apitest.New(), Token: "token-of-the-next-run"})
	defer h3.Close()
	mu.Lock()
	current = h3
	mu.Unlock()
	h2.Close() // ends the stream; the reconnect now gets 401
	wantClosed(t, events)
}
