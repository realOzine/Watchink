// Command loadtest drives a real engine (service + HTTP handler + SQLite on
// a seeded temporary data directory) with concurrent clients and reports
// throughput and latency percentiles per endpoint and for a mixed workload.
//
//	go run ./internal/bench/cmd/loadtest -videos 200 -segments 900 -c 32 -d 5s
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"watchink/internal/bench"
	"watchink/internal/store"
)

func main() {
	videos := flag.Int("videos", 200, "videos in the seeded library")
	segments := flag.Int("segments", 900, "transcript segments per video (900 ≈ a one-hour lecture)")
	points := flag.Int("points", 60, "cited points per note")
	conc := flag.Int("c", 32, "concurrent clients")
	dur := flag.Duration("d", 5*time.Second, "duration per scenario")
	cold := flag.Bool("cold", false, "cycle through every video instead of a hot set")
	writers := flag.Int("writers", 0, "background writers storing new transcripts while the clients read")
	flag.Parse()

	fmt.Fprintf(os.Stderr, "seeding %d videos × %d segments…\n", *videos, *segments)
	t0 := time.Now()
	eng, err := bench.StartEngine(context.Background(), bench.Shape{Videos: *videos, Segments: *segments, Points: *points})
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
	defer eng.Close()
	fmt.Fprintf(os.Stderr, "seeded in %s\n", time.Since(t0).Round(time.Millisecond))

	set := eng.Library.VideoIDs[:min(bench.HotVideos, len(eng.Library.VideoIDs))]
	access := fmt.Sprintf("hot set of %d videos", len(set))
	if *cold {
		set, access = eng.Library.VideoIDs, "cold: every video in turn"
	}
	fmt.Printf("library: %d videos × %d segments · %d clients · %s per scenario · %s\n\n", *videos, *segments, *conc, *dur, access)
	stopWriters := func() {}
	if *writers > 0 {
		stopWriters = startWriters(eng, *writers, *segments)
		fmt.Printf("background load: %d writers, each storing %d-segment transcripts in a loop\n\n", *writers, *segments)
	}
	fmt.Printf("%-18s %10s %9s %9s %9s %9s %10s\n", "endpoint", "req/s", "p50", "p95", "p99", "max", "resp")

	for _, ep := range bench.Endpoints {
		report(ep.Name, run(eng, *conc, *dur, func(i int) string { return ep.Path(set, eng.Library.JobIDs, i) }))
	}
	// Mixed: every endpoint in turn, the way a session interleaves them.
	n := len(bench.Endpoints)
	report("mixed", run(eng, *conc, *dur, func(i int) string {
		return bench.Endpoints[i%n].Path(set, eng.Library.JobIDs, i/n)
	}))
	stopWriters()
}

// startWriters models jobs finishing while people read: each writer stores a
// full transcript (the largest write the engine does) in a loop, through a
// second handle on the same database, so it competes with the engine for
// SQLite's single write lock exactly as another transaction would. The
// returned function stops the writers and prints their rate.
func startWriters(eng *bench.Engine, n, segments int) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.Open(ctx, eng.DataDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadtest: writers:", err)
		os.Exit(1)
	}
	var wg sync.WaitGroup
	var written, failed atomic.Int64
	start := time.Now()
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; ctx.Err() == nil; i += n {
				videoID := eng.Library.VideoIDs[i%len(eng.Library.VideoIDs)]
				art, err := st.PutArtifact(ctx, &store.Artifact{VideoID: videoID, Kind: store.KindASRRaw, Fingerprint: fmt.Sprintf("loadtest-%d-%d", w, i)})
				if err == nil {
					tr := &store.Transcript{VideoID: videoID, ArtifactID: art.ID, Language: "zh", Engine: "loadtest", Model: "loadtest"}
					err = st.InsertTranscript(ctx, tr, bench.Segments(segments))
				}
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					failed.Add(1)
					continue
				}
				written.Add(1)
			}
		}()
	}
	return func() {
		took := time.Since(start)
		cancel()
		wg.Wait()
		st.Close()
		fmt.Printf("\nwriters: %d transcripts stored (%.0f/s, %.0f segments/s), %d failed\n",
			written.Load(), float64(written.Load())/took.Seconds(), float64(written.Load())*float64(segments)/took.Seconds(), failed.Load())
	}
}

type result struct {
	lat    []time.Duration
	bytes  int64
	errors int64
	took   time.Duration
}

func run(eng *bench.Engine, conc int, d time.Duration, path func(i int) string) result {
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		res     result
		counter atomic.Int64
	)
	deadline := time.Now().Add(d)
	start := time.Now()
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lat []time.Duration
			var bytes, errs int64
			for time.Now().Before(deadline) {
				t := time.Now()
				status, size, err := eng.Get(path(int(counter.Add(1))))
				lat = append(lat, time.Since(t))
				bytes += size
				if err != nil || status != 200 {
					errs++
				}
			}
			mu.Lock()
			res.lat = append(res.lat, lat...)
			res.bytes += bytes
			res.errors += errs
			mu.Unlock()
		}()
	}
	wg.Wait()
	res.took = time.Since(start)
	return res
}

func report(name string, r result) {
	slices.Sort(r.lat)
	n := len(r.lat)
	if n == 0 {
		fmt.Printf("%-18s no requests completed\n", name)
		return
	}
	pct := func(p float64) time.Duration { return r.lat[min(int(float64(n)*p), n-1)] }
	ms := func(d time.Duration) string { return fmt.Sprintf("%.2fms", float64(d.Microseconds())/1000) }
	fmt.Printf("%-18s %10.0f %9s %9s %9s %9s %9.1fK", name, float64(n)/r.took.Seconds(), ms(pct(0.50)), ms(pct(0.95)), ms(pct(0.99)), ms(r.lat[n-1]), float64(r.bytes)/float64(n)/1024)
	if r.errors > 0 {
		fmt.Printf("  %d errors", r.errors)
	}
	fmt.Println()
}
