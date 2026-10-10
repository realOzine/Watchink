package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"watchink/internal/app"
)

// eventData returns the bare payload object of an event (§13.3): the Job for
// job.updated, the Video for video.updated, and so on.
func eventData(ev app.Event) any {
	switch ev.Type {
	case app.EventJobUpdated:
		if ev.Job != nil {
			return ev.Job
		}
	case app.EventVideoUpdated:
		if ev.Video != nil {
			return ev.Video
		}
	case app.EventToolsUpdated:
		if ev.Tools != nil {
			return ev.Tools
		}
	case app.EventEngineStopping:
		return map[string]string{"reason": ev.Reason}
	}
	return struct{}{}
}

func writeEvent(w io.Writer, ev app.Event) error {
	data, err := json.Marshal(eventData(ev))
	if err != nil {
		return err
	}
	// encoding/json never emits a raw newline, so one data line is enough.
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", ev.ID, ev.Type, data)
	return err
}

// events serves the event stream (§13.3). The frontend reads it with a
// streaming fetch so that it can send the Authorization header.
func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("last_event_id")
	}
	var afterID int64
	if last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			writeError(w, invalid("Last-Event-ID 必须是非负整数").WithDetail("param", "last_event_id"))
			return
		}
		afterID = n
	}

	events, reset, err := h.svc.Subscribe(r.Context(), afterID)
	if err != nil {
		writeError(w, err)
		return
	}
	h.subs.Add(1)
	defer h.subs.Add(-1)

	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	if reset {
		hdr.Set(HeaderStreamReset, "1")
	}
	w.WriteHeader(http.StatusOK)
	if reset {
		// The subscriber's position is gone from the replay buffer: it must
		// reload everything before applying the events that follow.
		if _, err := io.WriteString(w, "event: "+EventReset+"\ndata: {}\n\n"); err != nil {
			return
		}
	} else if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	if rc.Flush() != nil {
		return
	}

	hb := time.NewTicker(h.opts.HeartbeatInterval)
	defer hb.Stop()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			if writeEvent(w, ev) != nil || rc.Flush() != nil {
				return
			}
		case <-hb.C:
			if _, err := io.WriteString(w, ": hb\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-r.Context().Done():
			return
		case <-h.baseCtx.Done():
			// Engine shutdown: deliver what is already queued (notably
			// engine.stopping), then end the stream.
			for {
				select {
				case ev, ok := <-events:
					if !ok || writeEvent(w, ev) != nil {
						_ = rc.Flush()
						return
					}
				default:
					_ = rc.Flush()
					return
				}
			}
		}
	}
}
