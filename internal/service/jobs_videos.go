package service

import (
	"context"
	"strings"
	"time"

	"watchink/internal/app"
	"watchink/internal/pipeline"
	"watchink/internal/store"
)

// Job and video reads, and the cancel / retry operations. They are thin:
// the scheduler owns the state machine, the store owns the queries.

// ListJobs implements app.Service.
func (s *Service) ListJobs(ctx context.Context, req app.ListJobsRequest) (*app.JobList, error) {
	f := store.JobFilter{VideoID: req.VideoID, Limit: req.Limit, Cursor: req.Cursor}
	if req.Status != "" {
		f.Statuses = strings.Split(req.Status, ",")
	}
	rows, next, err := s.st.ListJobs(ctx, f)
	if err != nil {
		return nil, err
	}
	out := &app.JobList{Jobs: make([]app.Job, len(rows))}
	for i, r := range rows {
		out.Jobs[i] = s.sched.ToDTO(r)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

// GetJob implements app.Service, long-polling when WaitSeconds > 0.
func (s *Service) GetJob(ctx context.Context, req app.GetJobRequest) (*app.Job, error) {
	var j app.Job
	var err error
	if req.WaitSeconds > 0 {
		wait := min(req.WaitSeconds, app.MaxWaitSeconds)
		j, err = s.sched.Wait(ctx, req.JobID, req.AfterRev, time.Duration(wait)*time.Second)
	} else {
		j, err = s.sched.Get(ctx, req.JobID)
	}
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// CancelJob implements app.Service.
func (s *Service) CancelJob(ctx context.Context, jobID string) (*app.Job, error) {
	j, err := s.sched.Cancel(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// RetryJob implements app.Service.
func (s *Service) RetryJob(ctx context.Context, jobID string) (*app.Job, error) {
	j, err := s.sched.Retry(ctx, jobID)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

// ListVideos implements app.Service.
func (s *Service) ListVideos(ctx context.Context, req app.ListVideosRequest) (*app.VideoList, error) {
	rows, next, err := s.st.ListVideos(ctx, store.VideoFilter{TitleContains: req.TitleContains, Limit: req.Limit, Cursor: req.Cursor})
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	latest, err := s.st.LatestJobsOfVideos(ctx, ids)
	if err != nil {
		return nil, err
	}
	playback, err := s.st.ListArtifactsOfVideos(ctx, ids, store.KindPlayback)
	if err != nil {
		return nil, err
	}
	out := &app.VideoList{Videos: make([]app.Video, len(rows))}
	for i, r := range rows {
		out.Videos[i] = s.videoDTO(r, latest[r.ID], s.usable(playback[r.ID]) != nil)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

// GetVideo implements app.Service.
func (s *Service) GetVideo(ctx context.Context, videoID string) (*app.Video, error) {
	v, err := s.st.GetVideo(ctx, videoID)
	if err != nil {
		return nil, err
	}
	latest, err := s.st.LatestJobOfVideo(ctx, videoID)
	if err != nil {
		return nil, err
	}
	dto := s.videoDTO(v, latest, s.playbackArtifact(ctx, videoID) != nil)
	return &dto, nil
}

func (s *Service) videoDTO(v *store.Video, latest *store.JobWithStages, playbackReady bool) app.Video {
	dto := app.Video{
		VideoID: v.ID, Title: v.Title, Source: pipeline.VideoSource(v), DurationMs: v.DurationMs,
		Language: strPtr(v.Language), TranscriptID: strPtr(v.CurrentTranscriptID), NoteID: strPtr(v.CurrentNoteID),
		WebURL: s.base() + "/videos/" + v.ID, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt,
		PlaybackReady: playbackReady,
	}
	// status is derived from the latest job and the current note.
	dto.Status = app.VideoProcessing
	if latest != nil {
		dto.LatestJobID = &latest.ID
		switch latest.Status {
		case app.JobSucceeded:
			dto.Status = app.VideoReady
		case app.JobFailed, app.JobInterrupted:
			dto.Status = app.VideoFailed
		case app.JobCancelled:
			dto.Status = app.VideoCancelled
		}
		// A failed reprocess does not hide the note the video already has.
		if dto.Status != app.VideoProcessing && v.CurrentNoteID != "" {
			dto.Status = app.VideoReady
		}
	}
	return dto
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// playbackArtifact returns the newest usable playback artifact of a video.
func (s *Service) playbackArtifact(ctx context.Context, videoID string) *store.Artifact {
	arts, err := s.st.ListArtifacts(ctx, videoID, store.KindPlayback)
	if err != nil {
		return nil
	}
	return s.usable(arts)
}

// usable returns the newest artifact whose file is still on disk.
func (s *Service) usable(arts []*store.Artifact) *store.Artifact {
	for i := len(arts) - 1; i >= 0; i-- {
		if s.st.ArtifactPresent(arts[i]) {
			return arts[i]
		}
	}
	return nil
}
