package bench

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"

	"watchink/internal/api"
	"watchink/internal/config"
	"watchink/internal/service"
	"watchink/internal/store"
)

// Token is the bearer token of an Engine started by StartEngine.
const Token = "bench-token"

// Engine is a real service behind the real HTTP handler, on a seeded
// temporary data directory. Only the listener is httptest's.
type Engine struct {
	URL string
	// DataDir is the engine's data directory, for runs that add their own
	// load next to the HTTP clients.
	DataDir string
	Library *Library
	Client  *http.Client
	close   func()
}

// Close stops the engine and removes its data directory.
func (e *Engine) Close() { e.close() }

// StartEngine seeds a library of the given shape and serves it.
func StartEngine(ctx context.Context, shape Shape) (*Engine, error) {
	dir, err := os.MkdirTemp("", "watchink-bench-")
	if err != nil {
		return nil, err
	}
	// Keep the user's real secrets and location file out of the run.
	os.Setenv(config.EnvConfigDir, dir+"/cfg")
	dataDir := dir + "/data"

	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	lib, err := Seed(ctx, st, shape)
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, err
	}

	svc, err := service.Open(ctx, service.Options{DataDir: dataDir})
	if err != nil {
		return nil, err
	}
	h := api.NewHandler(api.Options{Service: svc, Token: Token})
	srv := httptest.NewServer(h)
	svc.SetBaseURL(srv.URL)
	runCtx, cancel := context.WithCancel(context.Background())
	if err := svc.Start(runCtx); err != nil {
		cancel()
		return nil, err
	}
	tr := &http.Transport{MaxIdleConns: 512, MaxIdleConnsPerHost: 512}
	return &Engine{
		URL: srv.URL, DataDir: dataDir, Library: lib, Client: &http.Client{Transport: tr},
		close: func() {
			tr.CloseIdleConnections()
			srv.Close()
			h.Close()
			_ = svc.Stop(context.Background(), true)
			cancel()
			_ = svc.Close()
			os.RemoveAll(dir)
		},
	}, nil
}

// Get performs one authenticated GET and returns the status and body size.
func (e *Engine) Get(path string) (status int, size int64, err error) {
	req, err := http.NewRequest(http.MethodGet, e.URL+path, nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+Token)
	resp, err := e.Client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	size, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, size, err
}

// Endpoint is one read request of the workload.
type Endpoint struct {
	Name string
	// Path returns the request path for the i-th call. videos and jobs are
	// the IDs the run is allowed to touch.
	Path func(videos, jobs []string, i int) string
}

// Endpoints is the read workload: what the library, video and job views of
// the UI, and an agent reading through MCP, ask for.
var Endpoints = []Endpoint{
	{"videos_list", func([]string, []string, int) string { return "/api/v1/videos?limit=50" }},
	{"video_get", func(v, _ []string, i int) string { return "/api/v1/videos/" + pick(v, i) }},
	{"note_markdown", func(v, _ []string, i int) string { return "/api/v1/videos/" + pick(v, i) + "/note" }},
	{"transcript_full", func(v, _ []string, i int) string {
		return "/api/v1/videos/" + pick(v, i) + "/transcript?max_chars=-1"
	}},
	{"transcript_srt", func(v, _ []string, i int) string {
		return "/api/v1/videos/" + pick(v, i) + "/transcript?format=srt&max_chars=-1"
	}},
	// After the full reads on purpose: the video page loads the whole
	// transcript first, then an agent or the player asks for windows.
	{"transcript_window", func(v, _ []string, i int) string {
		return "/api/v1/videos/" + pick(v, i) + "/transcript?start_ms=600000&end_ms=900000"
	}},
	{"jobs_list", func([]string, []string, int) string { return "/api/v1/jobs?limit=50" }},
	{"job_get", func(_, j []string, i int) string { return "/api/v1/jobs/" + pick(j, i) }},
}

// HotVideos is how many videos the "hot" runs touch: one person and an
// agent work on a handful of videos at a time, which is what the engine's
// caches of immutable rows are sized for. "Cold" runs cycle through the
// whole library, so every request is the first one for its video.
const HotVideos = 8

func pick(ids []string, i int) string { return ids[i%len(ids)] }
