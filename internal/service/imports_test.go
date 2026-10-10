package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"watchink/internal/app"
	"watchink/internal/config"
)

// openForTest opens a service on a temporary data directory whose import
// root is dir. The scheduler is not started, so imported jobs stay queued
// and no stage (and no tool download) ever runs.
func openForTest(t *testing.T, dir string) *Service {
	t.Helper()
	t.Setenv(config.EnvConfigDir, filepath.Join(dir, "cfg"))
	dataDir := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[security]\nimport_roots = [%q]\n", dir)
	if err := os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), Options{DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func writeMedia(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestConcurrentImportsOfOneFile is under contention: many entry points
// import the same file at the same moment. Exactly one of them creates the
// video and its job; every other caller is handed that same job.
func TestConcurrentImportsOfOneFile(t *testing.T) {
	// The temp dir may sit behind a symlink (/var → /private/var on macOS);
	// import roots are compared after resolving links.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := openForTest(t, dir)
	path := writeMedia(t, dir, "lecture.mp4", "not really a video, only its bytes matter here")

	const callers = 24
	resps := make([]*app.ImportResponse, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resps[i], errs[i] = s.Import(context.Background(), app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: path}, RequestedBy: "test"})
		}()
	}
	close(start)
	wg.Wait()

	created := 0
	for i, r := range resps {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if r.VideoID != resps[0].VideoID || r.JobID != resps[0].JobID {
			t.Fatalf("caller %d got video %s job %s, caller 0 got %s %s", i, r.VideoID, r.JobID, resps[0].VideoID, resps[0].JobID)
		}
		if !r.Deduplicated {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d callers believe they created the job, want exactly 1", created)
	}
	videos, err := s.ListVideos(context.Background(), app.ListVideosRequest{})
	if err != nil || len(videos.Videos) != 1 {
		t.Fatalf("library has %d videos (err %v), want 1", len(videos.Videos), err)
	}
	jobs, err := s.ListJobs(context.Background(), app.ListJobsRequest{})
	if err != nil || len(jobs.Jobs) != 1 {
		t.Fatalf("there are %d jobs (err %v), want 1", len(jobs.Jobs), err)
	}
}

// TestConcurrentImportsOfDifferentFiles: contention must not merge distinct
// sources or lose any of them.
func TestConcurrentImportsOfDifferentFiles(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := openForTest(t, dir)
	const files = 12
	var wg sync.WaitGroup
	seen := sync.Map{}
	for i := range files {
		path := writeMedia(t, dir, fmt.Sprintf("lecture-%d.mp4", i), fmt.Sprintf("distinct content %d", i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := s.Import(context.Background(), app.ImportRequest{Source: app.ImportSource{Type: app.SourceFile, Path: path}})
			if err != nil {
				t.Errorf("import %s: %v", path, err)
				return
			}
			if _, dup := seen.LoadOrStore(r.VideoID, true); dup || r.Deduplicated {
				t.Errorf("import %s was merged into another video (%s)", path, r.VideoID)
			}
		}()
	}
	wg.Wait()
	videos, err := s.ListVideos(context.Background(), app.ListVideosRequest{Limit: 50})
	if err != nil || len(videos.Videos) != files {
		t.Fatalf("library has %d videos (err %v), want %d", len(videos.Videos), err, files)
	}
}
