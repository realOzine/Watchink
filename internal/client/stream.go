package client

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"watchink/internal/api"
	"watchink/internal/app"
)

// EventReset is the Type of the synthetic event delivered on a Subscribe
// channel when, after an automatic reconnect, the engine could not resume
// from the last seen event: the consumer must reload its state. (For the
// initial connection the same fact is the `reset` return value instead.)
const EventReset = api.EventReset

// Reconnect policy of Subscribe: a broken stream is retried with these
// delays; when all fail (the engine is gone) the channel is closed.
var reconnectDelays = []time.Duration{
	100 * time.Millisecond, 250 * time.Millisecond, 500 * time.Millisecond,
	time.Second, 2 * time.Second,
}

// eventStream is one open SSE connection.
type eventStream struct {
	resp   *http.Response
	cancel context.CancelFunc
	reset  bool
}

func (s *eventStream) close() {
	s.resp.Body.Close()
	s.cancel()
}

func (c *Client) openEvents(ctx context.Context, afterID int64) (*eventStream, error) {
	resp, cancel, err := c.openStream(ctx, func(ctx context.Context) (*http.Request, error) {
		req, err := c.newRequest(ctx, http.MethodGet, path("events"), nil, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Last-Event-ID", strconv.FormatInt(afterID, 10))
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	return &eventStream{resp: resp, cancel: cancel, reset: resp.Header.Get(api.HeaderStreamReset) == "1"}, nil
}

// Subscribe implements app.Service over the event stream (§13.3). The
// returned channel is closed when ctx is done, after engine.stopping, or
// when the engine stays unreachable. A dropped connection is re-established
// transparently with Last-Event-ID; if the engine then reports that the
// position is lost, an Event{Type: EventReset} is delivered.
func (c *Client) Subscribe(ctx context.Context, afterID int64) (<-chan app.Event, bool, error) {
	if afterID < 0 {
		afterID = 0
	}
	st, err := c.openEvents(ctx, afterID)
	if err != nil {
		return nil, false, err
	}
	out := make(chan app.Event, 64)
	go c.pump(ctx, st, afterID, out)
	return out, st.reset, nil
}

func (c *Client) pump(ctx context.Context, st *eventStream, lastID int64, out chan<- app.Event) {
	defer close(out)
	first := true
	for {
		stopping := c.readEvents(ctx, st, first, &lastID, out)
		st.close()
		if stopping || ctx.Err() != nil {
			return
		}
		first = false
		st = nil
		for _, delay := range reconnectDelays {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
			next, err := c.openEvents(ctx, lastID)
			if err == nil {
				st = next
				break
			}
			// Anything but "not reachable" (e.g. 401 from a new engine run
			// with a new token) will not get better by retrying.
			if !app.IsCode(err, app.CodeEngineUnavailable) {
				return
			}
		}
		if st == nil {
			return
		}
	}
}

// readEvents forwards events until the stream ends. It reports whether
// engine.stopping was seen (no reconnect then).
func (c *Client) readEvents(ctx context.Context, st *eventStream, first bool, lastID *int64, out chan<- app.Event) (stopping bool) {
	// The watchdog ends a connection that went silent: the engine sends a
	// heartbeat every 15 s, so silence means a dead peer.
	watchdog := time.AfterFunc(c.streamIdle, st.cancel)
	defer watchdog.Stop()

	br := bufio.NewReaderSize(st.resp.Body, 64<<10)
	var id, typ string
	var data strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return false
		}
		watchdog.Reset(c.streamIdle)
		line = strings.TrimRight(line, "\r\n")
		if line != "" {
			if line[0] == ':' {
				continue // comment: heartbeat
			}
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "id":
				id = value
			case "event":
				typ = value
			case "data":
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(value)
			}
			continue
		}
		// Blank line: dispatch.
		if typ == "" && data.Len() == 0 {
			id = ""
			continue
		}
		ev, ok := decodeEvent(id, typ, data.String())
		id, typ = "", ""
		data.Reset()
		if !ok {
			continue
		}
		if ev.Type == EventReset && first {
			continue // already reported by Subscribe's reset result
		}
		if ev.ID > 0 {
			*lastID = ev.ID
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return false
		}
		if ev.Type == app.EventEngineStopping {
			return true
		}
	}
}

// decodeEvent rebuilds an app.Event from the bare payload of §13.3.
func decodeEvent(id, typ, data string) (app.Event, bool) {
	ev := app.Event{Type: typ}
	if id != "" {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return ev, false
		}
		ev.ID = n
	}
	var err error
	switch typ {
	case app.EventJobUpdated:
		ev.Job = &app.Job{}
		err = json.Unmarshal([]byte(data), ev.Job)
	case app.EventVideoUpdated:
		ev.Video = &app.Video{}
		err = json.Unmarshal([]byte(data), ev.Video)
	case app.EventToolsUpdated:
		ev.Tools = &app.ToolsStatus{}
		err = json.Unmarshal([]byte(data), ev.Tools)
	case app.EventEngineStopping:
		var p struct {
			Reason string `json:"reason"`
		}
		err = json.Unmarshal([]byte(data), &p)
		ev.Reason = p.Reason
	case EventReset:
	case "":
		return ev, false
	}
	return ev, err == nil
}
