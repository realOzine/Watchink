package store

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"watchink/internal/app"
)

func ptr[T any](v T) *T { return &v }

func TestJobLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "file:1:a", "x")

	j, err := s.CreateJob(ctx, &Job{Kind: app.JobKindProcessVideo, VideoID: v.ID, RequestedBy: "cli",
		Options: []byte(`{"language":"zh"}`)}, []string{"z_first", "a_second", "m_third"})
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != app.JobQueued || j.Rev != 1 || j.Attempt != 1 || !strings.HasPrefix(j.ID, app.PrefixJob) {
		t.Fatalf("new job = %+v", j.Job)
	}
	// Stages come back in creation order, not alphabetically.
	var names []string
	for _, st := range j.Stages {
		names = append(names, st.Stage)
		if st.Status != app.StagePending || st.Attempt != 0 {
			t.Fatalf("new stage = %+v", st)
		}
	}
	if !slices.Equal(names, []string{"z_first", "a_second", "m_third"}) {
		t.Fatalf("stage order = %v", names)
	}

	jobErr := app.E(app.CodeASRFailed, "转写失败").WithStage("a_second").WithDetail("exit", 1)
	tr := "tr_1"
	got, err := s.UpdateJob(ctx, j.ID, JobUpdate{
		IfStatus: []string{app.JobQueued},
		Job:      JobPatch{Status: ptr(app.JobRunning), StartedAt: ptr("2026-01-01T00:00:00Z"), Error: jobErr, Result: &app.JobResult{TranscriptID: &tr}},
		Stages: []StagePatch{
			{Stage: "z_first", Status: ptr(app.StageSucceeded), Progress: ptr(1.0), Fingerprint: ptr("fp"), ArtifactID: ptr("art_1"), Reused: ptr(true), Attempt: ptr(1)},
			{Stage: "a_second", Status: ptr(app.StageFailed), Error: jobErr},
		},
		Event: &EventInput{Type: "stage.failed", Data: map[string]any{"stage": "a_second"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Rev != 2 || got.Status != app.JobRunning || got.StartedAt == "" || got.Error == nil ||
		got.Error.Code != app.CodeASRFailed || got.Error.Stage != "a_second" || !got.Error.Retryable ||
		got.Result.TranscriptID == nil || *got.Result.TranscriptID != "tr_1" || got.Result.NoteID != nil {
		t.Fatalf("after update: %+v err=%+v", got.Job, got.Error)
	}
	st := got.Stage("z_first")
	if st.Status != app.StageSucceeded || !st.Reused || st.ArtifactID != "art_1" || st.Fingerprint != "fp" || st.Attempt != 1 {
		t.Fatalf("stage = %+v", st)
	}
	if got.Stage("a_second").Error == nil || got.Stage("nope") != nil {
		t.Fatal("stage error / lookup")
	}

	// A failed precondition changes nothing.
	if _, err := s.UpdateJob(ctx, j.ID, JobUpdate{IfStatus: []string{app.JobQueued}, Job: JobPatch{Status: ptr(app.JobCancelled)}}); !app.IsCode(err, app.CodeConflict) {
		t.Fatalf("IfStatus mismatch: %v", err)
	}
	// An unknown stage rolls the whole update back.
	if _, err := s.UpdateJob(ctx, j.ID, JobUpdate{Job: JobPatch{Status: ptr(app.JobFailed)}, Stages: []StagePatch{{Stage: "nope", Status: ptr(app.StageFailed)}}}); err == nil {
		t.Fatal("unknown stage accepted")
	}
	if _, err := s.UpdateJob(ctx, "job_missing", JobUpdate{}); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("update missing: %v", err)
	}
	again, _ := s.GetJob(ctx, j.ID)
	if again.Rev != 2 || again.Status != app.JobRunning {
		t.Fatalf("failed updates leaked: %+v", again.Job)
	}

	// Clearing error and nullable columns.
	got, _ = s.UpdateJob(ctx, j.ID, JobUpdate{Job: JobPatch{ClearError: true, StartedAt: ptr("")},
		Stages: []StagePatch{{Stage: "a_second", ClearError: true, Status: ptr(app.StagePending), ArtifactID: ptr("")}}})
	if got.Error != nil || got.StartedAt != "" || got.Stage("a_second").Error != nil || got.Rev != 3 {
		t.Fatalf("clear: %+v", got.Job)
	}

	evs, err := s.ListJobEvents(ctx, j.ID)
	if err != nil || len(evs) != 2 || evs[0].Type != "job.created" || evs[1].Type != "stage.failed" ||
		!strings.Contains(string(evs[1].Data), "a_second") {
		t.Fatalf("events = %+v, %v", evs, err)
	}
	if _, err := s.GetJob(ctx, "job_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("GetJob(missing) = %v", err)
	}
}

func TestJobQueries(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v1 := mustVideo(t, s.Queries, "file:1:a", "a")
	v2 := mustVideo(t, s.Queries, "file:1:b", "b")
	v3 := mustVideo(t, s.Queries, "file:1:c", "c")
	mk := func(kind, video, status string) *JobWithStages {
		t.Helper()
		j, err := s.CreateJob(ctx, &Job{Kind: kind, VideoID: video, Status: status}, []string{"s1", "s2"})
		if err != nil {
			t.Fatal(err)
		}
		return j
	}
	old := mk(app.JobKindProcessVideo, v1.ID, app.JobFailed)
	cur := mk(app.JobKindProcessVideo, v1.ID, app.JobRunning)
	done := mk(app.JobKindProcessVideo, v2.ID, app.JobSucceeded)
	tools := mk(app.JobKindPrepareTools, "", app.JobQueued)
	canc := mk(app.JobKindProcessVideo, v2.ID, app.JobCancelling)

	if tools.VideoID != "" {
		t.Fatalf("job without video: %+v", tools.Job)
	}
	if n, _ := s.CountActiveJobs(ctx); n != 3 {
		t.Fatalf("CountActiveJobs = %d", n)
	}
	if j, _ := s.LatestJobOfVideo(ctx, v1.ID); j == nil || j.ID != cur.ID || len(j.Stages) != 2 {
		t.Fatalf("LatestJobOfVideo = %+v", j)
	}
	if j, err := s.LatestJobOfVideo(ctx, v3.ID); j != nil || err != nil {
		t.Fatalf("LatestJobOfVideo(none) = %v, %v", j, err)
	}
	if j, _ := s.ActiveJobOfVideo(ctx, v1.ID); j == nil || j.ID != cur.ID {
		t.Fatalf("ActiveJobOfVideo = %+v", j)
	}
	if j, _ := s.ActiveJobOfVideo(ctx, v2.ID); j == nil || j.ID != canc.ID {
		t.Fatalf("ActiveJobOfVideo(cancelling) = %+v", j)
	}
	if j, _ := s.ActiveJobOfVideo(ctx, v3.ID); j != nil {
		t.Fatalf("ActiveJobOfVideo(none) = %+v", j)
	}
	if j, _ := s.ActiveJobByKind(ctx, app.JobKindPrepareTools); j == nil || j.ID != tools.ID {
		t.Fatalf("ActiveJobByKind = %+v", j)
	}
	latest, err := s.LatestJobsOfVideos(ctx, []string{v1.ID, v2.ID, v3.ID})
	if err != nil || len(latest) != 2 || latest[v1.ID].ID != cur.ID || latest[v2.ID].ID != canc.ID {
		t.Fatalf("LatestJobsOfVideos = %v, %v", latest, err)
	}
	// Scheduler order: oldest first.
	act, _ := s.JobsByStatus(ctx, app.JobQueued, app.JobRunning)
	if len(act) != 2 || act[0].ID != cur.ID || act[1].ID != tools.ID || len(act[0].Stages) != 2 {
		t.Fatalf("JobsByStatus = %+v", act)
	}

	ids := func(js []*JobWithStages) []string {
		var out []string
		for _, j := range js {
			out = append(out, j.ID)
		}
		return out
	}
	all, next, _ := s.ListJobs(ctx, JobFilter{})
	if !slices.Equal(ids(all), []string{canc.ID, tools.ID, done.ID, cur.ID, old.ID}) || next != "" {
		t.Fatalf("ListJobs = %v next=%q", ids(all), next)
	}
	p1, next, _ := s.ListJobs(ctx, JobFilter{Limit: 2})
	p2, next2, _ := s.ListJobs(ctx, JobFilter{Limit: 2, Cursor: next})
	p3, next3, _ := s.ListJobs(ctx, JobFilter{Limit: 2, Cursor: next2})
	if got := slices.Concat(ids(p1), ids(p2), ids(p3)); !slices.Equal(got, ids(all)) || next3 != "" || len(p1[0].Stages) != 2 {
		t.Fatalf("paged = %v", got)
	}
	if js, _, _ := s.ListJobs(ctx, JobFilter{VideoID: v1.ID}); !slices.Equal(ids(js), []string{cur.ID, old.ID}) {
		t.Fatalf("by video = %v", ids(js))
	}
	if js, _, _ := s.ListJobs(ctx, JobFilter{Statuses: []string{app.JobFailed, app.JobSucceeded}}); !slices.Equal(ids(js), []string{done.ID, old.ID}) {
		t.Fatalf("by status = %v", ids(js))
	}
	if js, _, _ := s.ListJobs(ctx, JobFilter{Kind: app.JobKindPrepareTools}); len(js) != 1 {
		t.Fatalf("by kind = %v", ids(js))
	}
}

func TestRecover(t *testing.T) {
	for _, auto := range []bool{false, true} {
		t.Run(fmt.Sprintf("autoResume=%v", auto), func(t *testing.T) {
			ctx := context.Background()
			s := openTest(t)
			v := mustVideo(t, s.Queries, "file:1:a", "a")
			mk := func(status string, stageStatus ...string) *JobWithStages {
				t.Helper()
				j, err := s.CreateJob(ctx, &Job{Kind: app.JobKindProcessVideo, VideoID: v.ID, Status: status}, []string{"s1", "s2"})
				if err != nil {
					t.Fatal(err)
				}
				var patches []StagePatch
				for i, st := range stageStatus {
					patches = append(patches, StagePatch{Stage: j.Stages[i].Stage, Status: ptr(st), Progress: ptr(0.5), StartedAt: ptr(app.Now())})
				}
				j, err = s.UpdateJob(ctx, j.ID, JobUpdate{Stages: patches})
				if err != nil {
					t.Fatal(err)
				}
				return j
			}
			running := mk(app.JobRunning, app.StageSucceeded, app.StageRunning)
			cancelling := mk(app.JobCancelling, app.StageRunning)
			queued := mk(app.JobQueued)
			failed := mk(app.JobFailed, app.StageFailed)
			interrupted := mk(app.JobInterrupted, app.StageSucceeded)
			succeeded := mk(app.JobSucceeded, app.StageSucceeded, app.StageSucceeded)

			res, err := s.Recover(ctx, auto)
			if err != nil {
				t.Fatal(err)
			}
			status := func(j *JobWithStages) *JobWithStages {
				t.Helper()
				got, err := s.GetJob(ctx, j.ID)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			wantRunning, wantInterrupted := app.JobInterrupted, app.JobInterrupted
			if auto {
				wantRunning, wantInterrupted = app.JobQueued, app.JobQueued
			}
			r := status(running)
			if r.Status != wantRunning || r.Stages[0].Status != app.StageSucceeded || r.Stages[1].Status != app.StagePending ||
				r.Stages[1].Progress != 0 || r.Stages[1].StartedAt != "" {
				t.Fatalf("running job after recovery: %+v %+v", r.Job, r.Stages)
			}
			wantRev := running.Rev + 1
			if auto {
				wantRev++
				if r.FinishedAt != "" {
					t.Fatalf("requeued job keeps finished_at")
				}
			}
			if r.Rev != wantRev {
				t.Fatalf("rev = %d, want %d", r.Rev, wantRev)
			}
			c := status(cancelling)
			if c.Status != app.JobCancelled || c.FinishedAt == "" || c.Stages[0].Status != app.StagePending || c.Rev != cancelling.Rev+1 {
				t.Fatalf("cancelling job after recovery: %+v", c.Job)
			}
			if got := status(interrupted); got.Status != wantInterrupted {
				t.Fatalf("interrupted job: %s", got.Status)
			}
			for _, j := range []*JobWithStages{queued, failed, succeeded} {
				if got := status(j); got.Status != j.Status || got.Rev != j.Rev {
					t.Fatalf("job %s changed: %s rev %d", j.Status, got.Status, got.Rev)
				}
			}
			if !slices.Equal(res.Cancelled, []string{cancelling.ID}) || !slices.Equal(res.Interrupted, []string{running.ID}) {
				t.Fatalf("result = %+v", res)
			}
			if auto {
				if !slices.Equal(res.Requeued, []string{running.ID, interrupted.ID}) || len(res.JobIDs()) != 3 {
					t.Fatalf("requeued = %v, ids = %v", res.Requeued, res.JobIDs())
				}
			} else if len(res.Requeued) != 0 || len(res.JobIDs()) != 2 {
				t.Fatalf("requeued without autoResume: %+v", res)
			}
			evs, _ := s.ListJobEvents(ctx, running.ID)
			var types []string
			for _, e := range evs {
				types = append(types, e.Type)
			}
			want := []string{"job.created", "job.interrupted"}
			if auto {
				want = append(want, "job.resumed")
			}
			if !slices.Equal(types, want) {
				t.Fatalf("events = %v, want %v", types, want)
			}
			// Idempotent: a second run touches nothing.
			res2, err := s.Recover(ctx, auto)
			if err != nil || len(res2.JobIDs()) != 0 {
				t.Fatalf("second Recover = %+v, %v", res2, err)
			}
		})
	}
}

func TestTranscriptsAndSegments(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "file:1:a", "a")
	const n = 1001 // more than two insert batches
	segs := make([]Segment, n)
	for i := range segs {
		segs[i] = Segment{StartMs: int64(i) * 1000, EndMs: int64(i)*1000 + 900, Text: fmt.Sprintf("s%d", i+1)}
	}
	tr := &Transcript{VideoID: v.ID, ArtifactID: "art_1", Language: "zh", Engine: "whispercpp@1", Model: "m"}
	if err := s.InsertTranscript(ctx, tr, segs); err != nil {
		t.Fatal(err)
	}
	if tr.SegmentCount != n || segs[41].ID != app.SegmentID(tr.ID, 42) || segs[41].Idx != 42 {
		t.Fatalf("normalization: %+v %+v", tr, segs[41])
	}
	got, err := s.GetTranscript(ctx, tr.ID)
	if err != nil || got.SegmentCount != n || string(got.Params) != "{}" || got.Language != "zh" {
		t.Fatalf("GetTranscript = %+v, %v", got, err)
	}
	if f, _ := s.FindTranscriptByArtifact(ctx, "art_1"); f == nil || f.ID != tr.ID {
		t.Fatalf("FindTranscriptByArtifact = %+v", f)
	}
	if f, err := s.FindTranscriptByArtifact(ctx, "art_none"); f != nil || err != nil {
		t.Fatalf("FindTranscriptByArtifact(none) = %v, %v", f, err)
	}
	all, _ := s.Segments(ctx, tr.ID)
	if len(all) != n || all[0].Idx != 1 || all[n-1].Idx != n || all[n-1].Text != "s1001" {
		t.Fatalf("Segments: %d", len(all))
	}

	rng := func(start, end *int64, limit int) []int {
		t.Helper()
		ss, err := s.SegmentsInRange(ctx, tr.ID, start, end, limit)
		if err != nil {
			t.Fatal(err)
		}
		var idx []int
		for _, x := range ss {
			idx = append(idx, x.Idx)
		}
		return idx
	}
	for _, tc := range []struct {
		name       string
		start, end *int64
		limit      int
		want       []int
	}{
		{"inside one segment", ptr(int64(2100)), ptr(int64(2200)), 0, []int{3}},
		{"spanning", ptr(int64(1500)), ptr(int64(4000)), 0, []int{2, 3, 4}},
		{"end is exclusive", ptr(int64(0)), ptr(int64(1000)), 0, []int{1}},
		{"start at a segment end excludes it", ptr(int64(900)), ptr(int64(1001)), 0, []int{2}},
		{"open start", nil, ptr(int64(1500)), 0, []int{1, 2}},
		{"open end with limit", ptr(int64(999_000)), nil, 0, []int{1000, 1001}},
		{"limit", nil, nil, 3, []int{1, 2, 3}},
		{"gap between segments", ptr(int64(950)), ptr(int64(990)), 0, nil},
	} {
		if got := rng(tc.start, tc.end, tc.limit); !slices.Equal(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}

	byID, err := s.SegmentsByIDs(ctx, []string{segs[9].ID, "seg_missing", segs[2].ID, segs[9].ID})
	if err != nil || len(byID) != 2 || byID[0].Idx != 3 || byID[1].Idx != 10 || byID[0].DTO().SegmentID != segs[2].ID {
		t.Fatalf("SegmentsByIDs = %+v, %v", byID, err)
	}
	if none, err := s.SegmentsByIDs(ctx, nil); len(none) != 0 || err != nil {
		t.Fatalf("SegmentsByIDs(nil) = %v, %v", none, err)
	}

	// A failing insert leaves no partial transcript behind.
	bad := &Transcript{ID: tr.ID, VideoID: v.ID, ArtifactID: "a", Language: "zh", Engine: "e", Model: "m"}
	if err := s.InsertTranscript(ctx, bad, []Segment{{Text: "x"}}); err == nil {
		t.Fatal("duplicate transcript accepted")
	}
	tr2 := &Transcript{VideoID: v.ID, ArtifactID: "art_2", Language: "en", Engine: "e", Model: "m"}
	if err := s.InsertTranscript(ctx, tr2, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListTranscripts(ctx, v.ID); len(list) != 2 || list[0].ID != tr2.ID {
		t.Fatalf("ListTranscripts = %+v", list)
	}
	if err := s.InsertTranscript(ctx, &Transcript{VideoID: v.ID}, make([]Segment, maxSegments+1)); err == nil {
		t.Fatal("too many segments accepted")
	}
}

func TestNotesAndCitations(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	v := mustVideo(t, s.Queries, "file:1:a", "a")
	segs := []Segment{{StartMs: 0, EndMs: 10, Text: "first"}, {StartMs: 10, EndMs: 20, Text: "second"}}
	tr := &Transcript{VideoID: v.ID, ArtifactID: "art_1", Language: "zh", Engine: "e", Model: "m"}
	if err := s.InsertTranscript(ctx, tr, segs); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CurrentNote(ctx, v.ID); n != nil || err != nil {
		t.Fatalf("CurrentNote(none) = %v, %v", n, err)
	}
	if _, err := s.CurrentNote(ctx, "vid_missing"); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("CurrentNote(unknown video) = %v", err)
	}
	newNote := func() *Note {
		return &Note{VideoID: v.ID, TranscriptID: tr.ID, Title: "T", MDArtifactID: "a", JSONArtifactID: "b",
			LLMProvider: "p", LLMModel: "m", PromptVersion: "1", CreatedByJob: "job_1", Coverage: []byte(`{"chunks":2}`)}
	}
	// The usual composition: note + current pointer in one transaction.
	n1 := newNote()
	err := s.Write(ctx, func(tx *Tx) error {
		if err := tx.InsertNote(ctx, n1, []Citation{
			{RefNo: 2, PointID: "p2", SegmentID: segs[1].ID, StartMs: 10, EndMs: 20},
			{RefNo: 1, PointID: "p1", SegmentID: segs[0].ID, StartMs: 0, EndMs: 10},
		}); err != nil {
			return err
		}
		_, err := tx.UpdateVideo(ctx, v.ID, VideoPatch{CurrentNoteID: &n1.ID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if n1.Version != 1 || !strings.HasPrefix(n1.ID, app.PrefixNote) {
		t.Fatalf("note 1 = %+v", n1)
	}
	n2 := newNote()
	if err := s.InsertNote(ctx, n2, nil); err != nil || n2.Version != 2 {
		t.Fatalf("note 2 = %+v, %v", n2, err)
	}
	if next, _ := s.NextNoteVersion(ctx, v.ID); next != 3 {
		t.Fatalf("NextNoteVersion = %d", next)
	}
	cur, err := s.CurrentNote(ctx, v.ID)
	if err != nil || cur.ID != n1.ID || string(cur.Coverage) != `{"chunks":2}` {
		t.Fatalf("CurrentNote = %+v, %v", cur, err)
	}
	if list, _ := s.ListNotes(ctx, v.ID); len(list) != 2 || list[0].Version != 2 {
		t.Fatalf("ListNotes = %+v", list)
	}
	cits, err := s.ListCitations(ctx, n1.ID)
	if err != nil || len(cits) != 2 || cits[0].RefNo != 1 || cits[0].Text != "first" || cits[1].Text != "second" || cits[1].SegmentID != segs[1].ID {
		t.Fatalf("ListCitations = %+v, %v", cits, err)
	}
	// A citation of an unknown segment aborts the whole note.
	n3 := newNote()
	if err := s.InsertNote(ctx, n3, []Citation{{RefNo: 1, PointID: "p", SegmentID: "seg_missing"}}); err == nil {
		t.Fatal("citation of a missing segment accepted")
	}
	if _, err := s.GetNote(ctx, n3.ID); !app.IsCode(err, app.CodeNotFound) {
		t.Fatalf("partial note left behind: %v", err)
	}
}
