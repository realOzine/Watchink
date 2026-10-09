package bench

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"watchink/internal/app"
	"watchink/internal/events"
	"watchink/internal/jobs"
	"watchink/internal/notes"
	"watchink/internal/store"
)

// lecture is a one-hour lecture: about 900 segments, 60 cited points.
var lecture = Shape{Videos: 100, Segments: 900, Points: 60}

// BenchmarkAPI measures each read endpoint end to end (HTTP, auth, service,
// SQLite, JSON) with as many concurrent clients as GOMAXPROCS. "hot" touches
// HotVideos videos, "cold" cycles through the whole library.
func BenchmarkAPI(b *testing.B) {
	eng, err := StartEngine(context.Background(), lecture)
	if err != nil {
		b.Fatal(err)
	}
	defer eng.Close()
	lib := eng.Library
	for _, set := range []struct {
		name   string
		videos []string
	}{{"hot", lib.VideoIDs[:HotVideos]}, {"cold", lib.VideoIDs}} {
		for _, ep := range Endpoints {
			b.Run(set.name+"/"+ep.Name, func(b *testing.B) {
				var n, bytes atomic.Int64
				b.ReportAllocs()
				b.ResetTimer()
				b.RunParallel(func(pb *testing.PB) {
					for pb.Next() {
						status, size, err := eng.Get(ep.Path(set.videos, lib.JobIDs, int(n.Add(1))))
						if err != nil || status != 200 {
							b.Errorf("%s: status %d, err %v", ep.Name, status, err)
							return
						}
						bytes.Add(size)
					}
				})
				b.ReportMetric(float64(bytes.Load())/float64(b.N), "resp-B/op")
			})
		}
	}
}

// BenchmarkInsertTranscript is the write a finished transcription does.
func BenchmarkInsertTranscript(b *testing.B) {
	for _, n := range []int{900, 5000} {
		b.Run(fmt.Sprintf("%d_segments", n), func(b *testing.B) {
			ctx := context.Background()
			st, err := store.Open(ctx, b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			defer st.Close()
			lib, err := Seed(ctx, st, Shape{Videos: 1, Segments: 1, Points: 1})
			if err != nil {
				b.Fatal(err)
			}
			art, err := st.PutArtifact(ctx, &store.Artifact{VideoID: lib.VideoIDs[0], Kind: store.KindASRRaw, Fingerprint: "bench-insert"})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				segs := Segments(n)
				tr := &store.Transcript{VideoID: lib.VideoIDs[0], ArtifactID: art.ID, Language: "zh", Engine: "bench", Model: "bench"}
				if err := st.InsertTranscript(ctx, tr, segs); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkScheduler measures the scheduler's own cost per job: a job of
// three stages that do nothing, so what is left is state transitions,
// SQLite writes and event publishing.
func BenchmarkScheduler(b *testing.B) {
	ctx := context.Background()
	st, err := store.Open(ctx, b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	// A job can be announced as succeeded more than once, so count each
	// job ID a single time.
	var done sync.WaitGroup
	var finished sync.Map
	sched := jobs.New(jobs.Options{Store: st, Publisher: events.New(0), OnJobChanged: func(j app.Job) {
		if j.Status == app.JobSucceeded || j.Status == app.JobFailed {
			if _, dup := finished.LoadOrStore(j.JobID, true); !dup {
				done.Done()
			}
		}
	}})
	noop := func(context.Context, *jobs.StageContext) (*jobs.StageResult, error) { return &jobs.StageResult{}, nil }
	sched.Register("bench", func(*store.Job) ([]jobs.StageDef, error) {
		return []jobs.StageDef{
			{Name: "a", Resource: jobs.ResourceIO, Weight: 1, Run: noop},
			{Name: "b", Deps: []string{"a"}, Resource: jobs.ResourceIO, Weight: 1, Run: noop},
			{Name: "c", Deps: []string{"b"}, Resource: jobs.ResourceCPU, Weight: 1, Run: noop},
		}, nil
	})
	if err := sched.Start(ctx); err != nil {
		b.Fatal(err)
	}
	defer sched.Stop(ctx, true)
	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	done.Add(b.N)
	for i := 0; i < b.N; i++ {
		if _, err := sched.Submit(ctx, jobs.JobParams{Kind: "bench"}); err != nil {
			b.Fatal(err)
		}
	}
	done.Wait()
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "jobs/s")
}

// BenchmarkText covers the pure text processing of a five-hour transcript.
func BenchmarkText(b *testing.B) {
	rows := Segments(5000)
	segs := make([]app.Segment, len(rows))
	for i, r := range rows {
		segs[i] = app.Segment{SegmentID: app.SegmentID("tr_01JBENCH0000000000000000000", i+1), Idx: i + 1, StartMs: r.StartMs, EndMs: r.EndMs, Text: r.Text}
	}
	srt := notes.SRT(segs)
	b.Run("chunk_5000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			notes.ChunkSegments(segs, notes.Options{})
		}
	})
	b.Run("srt_5000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			notes.SRT(segs)
		}
	})
	b.Run("vtt_5000", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			notes.VTT(segs)
		}
	})
	b.Run("paginate_page", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			notes.Paginate(srt, 100_000, 20_000)
		}
	})
}
