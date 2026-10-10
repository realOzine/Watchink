package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"watchink/internal/app"
)

// ActiveJobStatuses are the statuses counted as "active".
var ActiveJobStatuses = []string{app.JobQueued, app.JobRunning, app.JobCancelling}

// Job is a row of the jobs table.
type Job struct {
	ID          string
	Kind        string // process_video / prepare_tools
	VideoID     string // "" for jobs without a video (prepare_tools)
	Status      string
	Rev         int64
	Attempt     int
	Options     json.RawMessage // JSON object, "{}" when empty
	RequestedBy string
	Error       *app.Error
	Result      app.JobResult
	CreatedAt   string
	StartedAt   string
	FinishedAt  string
	UpdatedAt   string
}

// JobStage is a row of the job_stages table.
type JobStage struct {
	JobID       string
	Stage       string
	Status      string
	Progress    float64
	Fingerprint string
	ArtifactID  string
	Reused      bool
	Attempt     int // number of times the stage was started
	Error       *app.Error
	StartedAt   string
	FinishedAt  string
}

// JobWithStages is a job with its stage rows in creation (DAG) order.
type JobWithStages struct {
	Job
	Stages []JobStage
}

// Stage returns the stage row with the given name, or nil.
func (j *JobWithStages) Stage(name string) *JobStage {
	for i := range j.Stages {
		if j.Stages[i].Stage == name {
			return &j.Stages[i]
		}
	}
	return nil
}

// JobEvent is a row of the job_events table.
type JobEvent struct {
	ID    int64
	JobID string
	At    string
	Type  string
	Data  json.RawMessage
}

// EventInput describes the job_events row written together with a change.
// Data is marshalled to JSON; nil means "{}".
type EventInput struct {
	Type string
	Data any
}

// JobPatch lists the jobs columns an update may change; nil fields are left
// alone. For StartedAt / FinishedAt an empty string stores NULL.
type JobPatch struct {
	Status     *string
	Attempt    *int
	Error      *app.Error // set the error object
	ClearError bool       // store NULL (wins over Error)
	Result     *app.JobResult
	StartedAt  *string
	FinishedAt *string
}

// StagePatch changes one job_stages row; nil fields are left alone. For
// Fingerprint / ArtifactID / StartedAt / FinishedAt an empty string stores
// NULL.
type StagePatch struct {
	Stage       string
	Status      *string
	Progress    *float64
	Fingerprint *string
	ArtifactID  *string
	Reused      *bool
	Attempt     *int
	Error       *app.Error
	ClearError  bool
	StartedAt   *string
	FinishedAt  *string
}

// JobUpdate is one atomic change of a job and any of its stages.
type JobUpdate struct {
	// IfStatus, when not empty, makes the update conditional: the current
	// job status must be one of these, otherwise UpdateJob returns CONFLICT
	// and changes nothing.
	IfStatus []string
	Job      JobPatch
	Stages   []StagePatch
	// Event, when set, is appended to job_events in the same transaction.
	Event *EventInput
}

// JobFilter selects a page of jobs, newest first.
type JobFilter struct {
	Statuses []string // empty = any
	VideoID  string   // "" = any
	Kind     string   // "" = any
	Limit    int
	Cursor   string
}

const (
	jobCols = `id, kind, video_id, status, rev, attempt, options, requested_by, error, result,
		created_at, started_at, finished_at, updated_at`
	stageCols = `job_id, stage, status, progress, fingerprint, artifact_id, reused, attempt, error,
		started_at, finished_at`
)

func scanJob(sc scanner) (*JobWithStages, error) {
	var (
		j                                    JobWithStages
		video, reqBy, errJSON, started, done sql.NullString
		options, result                      string
	)
	if err := sc.Scan(&j.ID, &j.Kind, &video, &j.Status, &j.Rev, &j.Attempt, &options, &reqBy, &errJSON,
		&result, &j.CreatedAt, &started, &done, &j.UpdatedAt); err != nil {
		return nil, err
	}
	j.VideoID, j.RequestedBy, j.StartedAt, j.FinishedAt = video.String, reqBy.String, started.String, done.String
	j.Options = json.RawMessage(options)
	j.Error = decodeError(errJSON)
	// A malformed result must not make the job unreadable.
	_ = json.Unmarshal([]byte(result), &j.Result)
	return &j, nil
}

func scanStage(sc scanner) (JobStage, error) {
	var (
		s                                JobStage
		fp, art, errJSON, started, ended sql.NullString
	)
	if err := sc.Scan(&s.JobID, &s.Stage, &s.Status, &s.Progress, &fp, &art, &s.Reused, &s.Attempt, &errJSON,
		&started, &ended); err != nil {
		return s, err
	}
	s.Fingerprint, s.ArtifactID, s.StartedAt, s.FinishedAt = fp.String, art.String, started.String, ended.String
	s.Error = decodeError(errJSON)
	return s, nil
}

func decodeError(s sql.NullString) *app.Error {
	if !s.Valid || s.String == "" {
		return nil
	}
	var e app.Error
	if err := json.Unmarshal([]byte(s.String), &e); err != nil {
		return app.E(app.CodeInternal, "%s", s.String)
	}
	return &e
}

func encodeError(e *app.Error) any {
	if e == nil {
		return nil
	}
	b, err := json.Marshal(e)
	if err != nil {
		b, _ = json.Marshal(app.E(e.Code, "%s", e.Message))
	}
	return string(b)
}

func encodeJSON(v any) (string, error) {
	if v == nil {
		return "{}", nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", internalErr("无法序列化 JSON", err)
	}
	return string(b), nil
}

func (q *Queries) insertEvent(ctx context.Context, jobID, at string, ev *EventInput) error {
	if ev == nil {
		return nil
	}
	data, err := encodeJSON(ev.Data)
	if err != nil {
		return err
	}
	_, err = q.exec(ctx, `INSERT INTO job_events (job_id, at, type, data) VALUES (?,?,?,?)`, jobID, at, ev.Type, data)
	return err
}

// CreateJob inserts the job (status queued unless set, rev 1, attempt 1), one
// pending job_stages row per stage name in the given order, and a "created"
// event, all in one transaction. Empty ID / timestamps are filled in.
//
// The "at most one active job per video" rule is not enforced here:
// check ActiveJobOfVideo in the same Store.Write transaction.
func (q *Queries) CreateJob(ctx context.Context, j *Job, stages []string) (*JobWithStages, error) {
	if j.ID == "" {
		j.ID = app.NewID(app.PrefixJob)
	}
	if j.Status == "" {
		j.Status = app.JobQueued
	}
	if j.Rev <= 0 {
		j.Rev = 1
	}
	if j.Attempt <= 0 {
		j.Attempt = 1
	}
	if j.CreatedAt == "" {
		j.CreatedAt = app.Now()
	}
	if j.UpdatedAt == "" {
		j.UpdatedAt = j.CreatedAt
	}
	result, err := encodeJSON(j.Result)
	if err != nil {
		return nil, err
	}
	var out *JobWithStages
	err = q.inWrite(ctx, func(q *Queries) error {
		if _, err := q.exec(ctx, `INSERT INTO jobs (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			j.ID, j.Kind, nullable(j.VideoID), j.Status, j.Rev, j.Attempt, rawJSON(j.Options),
			nullable(j.RequestedBy), encodeError(j.Error), result,
			j.CreatedAt, nullable(j.StartedAt), nullable(j.FinishedAt), j.UpdatedAt); err != nil {
			return err
		}
		for _, name := range stages {
			if _, err := q.exec(ctx, `INSERT INTO job_stages (job_id, stage, status) VALUES (?,?,?)`,
				j.ID, name, app.StagePending); err != nil {
				return err
			}
		}
		if err := q.insertEvent(ctx, j.ID, j.CreatedAt, &EventInput{Type: "job.created",
			Data: map[string]any{"kind": j.Kind, "status": j.Status}}); err != nil {
			return err
		}
		var err error
		out, err = q.GetJob(ctx, j.ID)
		return err
	})
	return out, err
}

// GetJob returns the job with its stages, or NOT_FOUND.
func (q *Queries) GetJob(ctx context.Context, id string) (*JobWithStages, error) {
	var out *JobWithStages
	err := q.inRead(ctx, func(q *Queries) error {
		j, ok, err := queryOne(ctx, q, scanJob, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if !ok {
			return notFound("任务", id)
		}
		out = j
		return q.loadStages(ctx, []*JobWithStages{j})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// loadStages fills Stages of every job. rowid order is insertion order,
// i.e. the DAG order the stages were created in.
func (q *Queries) loadStages(ctx context.Context, jobs []*JobWithStages) error {
	if len(jobs) == 0 {
		return nil
	}
	byID := make(map[string]*JobWithStages, len(jobs))
	ids := make([]any, len(jobs))
	for i, j := range jobs {
		byID[j.ID] = j
		ids[i] = j.ID
		j.Stages = nil
	}
	// Chunked so a long listing never exceeds SQLite's variable limit.
	for chunk := range slices.Chunk(ids, 500) {
		stages, err := queryAll(ctx, q, scanStage,
			`SELECT `+stageCols+` FROM job_stages WHERE job_id IN (`+placeholders(len(chunk))+`) ORDER BY rowid`, chunk...)
		if err != nil {
			return err
		}
		for _, s := range stages {
			if j := byID[s.JobID]; j != nil {
				j.Stages = append(j.Stages, s)
			}
		}
	}
	return nil
}

func (q *Queries) selectJobs(ctx context.Context, tail string, args ...any) ([]*JobWithStages, error) {
	var out []*JobWithStages
	err := q.inRead(ctx, func(q *Queries) error {
		jobs, err := queryAll(ctx, q, scanJob, `SELECT `+jobCols+` FROM jobs `+tail, args...)
		if err != nil {
			return err
		}
		out = jobs
		return q.loadStages(ctx, jobs)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListJobs returns one page of jobs with their stages, newest first, and the
// cursor of the next page ("" when there is none).
func (q *Queries) ListJobs(ctx context.Context, f JobFilter) (jobs []*JobWithStages, nextCursor string, err error) {
	limit := clampLimit(f.Limit)
	var (
		where []string
		args  []any
	)
	if f.Cursor != "" {
		after, err := decodeCursor(f.Cursor)
		if err != nil {
			return nil, "", err
		}
		where, args = append(where, "id < ?"), append(args, after)
	}
	if len(f.Statuses) > 0 {
		where = append(where, "status IN ("+placeholders(len(f.Statuses))+")")
		args = append(args, anySlice(f.Statuses)...)
	}
	if f.VideoID != "" {
		where, args = append(where, "video_id = ?"), append(args, f.VideoID)
	}
	if f.Kind != "" {
		where, args = append(where, "kind = ?"), append(args, f.Kind)
	}
	tail := ""
	if len(where) > 0 {
		tail = "WHERE " + strings.Join(where, " AND ") + " "
	}
	tail += "ORDER BY id DESC LIMIT ?"
	args = append(args, limit+1)
	jobs, err = q.selectJobs(ctx, tail, args...)
	if err != nil {
		return nil, "", err
	}
	if len(jobs) > limit {
		jobs = jobs[:limit]
		nextCursor = encodeCursor(jobs[limit-1].ID)
	}
	return jobs, nextCursor, nil
}

// JobsByStatus returns every job in one of the statuses with its stages, in
// creation order (oldest first) — the order the scheduler serves them.
func (q *Queries) JobsByStatus(ctx context.Context, statuses ...string) ([]*JobWithStages, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	return q.selectJobs(ctx, "WHERE status IN ("+placeholders(len(statuses))+") ORDER BY id", anySlice(statuses)...)
}

func (q *Queries) firstJob(ctx context.Context, tail string, args ...any) (*JobWithStages, error) {
	jobs, err := q.selectJobs(ctx, tail, args...)
	if err != nil || len(jobs) == 0 {
		return nil, err
	}
	return jobs[0], nil
}

// LatestJobOfVideo returns the most recently created job of the video, or
// (nil, nil).
func (q *Queries) LatestJobOfVideo(ctx context.Context, videoID string) (*JobWithStages, error) {
	return q.firstJob(ctx, "WHERE video_id = ? ORDER BY id DESC LIMIT 1", videoID)
}

// LatestJobsOfVideos returns the most recently created job (without stages
// filled) of each given video that has one, keyed by video ID. It serves
// video listings, which derive the video status from the latest job.
func (q *Queries) LatestJobsOfVideos(ctx context.Context, videoIDs []string) (map[string]*JobWithStages, error) {
	out := make(map[string]*JobWithStages, len(videoIDs))
	for chunk := range slices.Chunk(videoIDs, 500) {
		jobs, err := queryAll(ctx, q, scanJob, `SELECT `+jobCols+` FROM jobs WHERE id IN (
			SELECT MAX(id) FROM jobs WHERE video_id IN (`+placeholders(len(chunk))+`) GROUP BY video_id)`,
			anySlice(chunk)...)
		if err != nil {
			return nil, err
		}
		for _, j := range jobs {
			out[j.VideoID] = j
		}
	}
	return out, nil
}

// ActiveJobOfVideo returns the queued / running / cancelling job of the
// video, or (nil, nil). There is at most one: Import checks for it in its transaction.
func (q *Queries) ActiveJobOfVideo(ctx context.Context, videoID string) (*JobWithStages, error) {
	return q.firstJob(ctx, "WHERE video_id = ? AND status IN (?,?,?) ORDER BY id DESC LIMIT 1",
		videoID, app.JobQueued, app.JobRunning, app.JobCancelling)
}

// ActiveJobByKind returns the oldest queued / running / cancelling job of
// the kind (e.g. the one prepare_tools job), or (nil, nil).
func (q *Queries) ActiveJobByKind(ctx context.Context, kind string) (*JobWithStages, error) {
	return q.firstJob(ctx, "WHERE kind = ? AND status IN (?,?,?) ORDER BY id LIMIT 1",
		kind, app.JobQueued, app.JobRunning, app.JobCancelling)
}

// CountActiveJobs counts queued + running + cancelling jobs.
func (q *Queries) CountActiveJobs(ctx context.Context) (int, error) {
	n, _, err := queryOne(ctx, q, func(sc scanner) (int, error) {
		var n int
		return n, sc.Scan(&n)
	}, `SELECT COUNT(*) FROM jobs WHERE status IN (?,?,?)`, app.JobQueued, app.JobRunning, app.JobCancelling)
	return n, err
}

// UpdateJob applies u atomically: the job patch, the stage patches and the
// optional event row, with rev = rev + 1 and a fresh updated_at. It
// returns the job as it is after the change.
//
// Errors: NOT_FOUND for an unknown job, CONFLICT when IfStatus does not
// match, INTERNAL for an unknown stage name.
func (q *Queries) UpdateJob(ctx context.Context, jobID string, u JobUpdate) (*JobWithStages, error) {
	var out *JobWithStages
	err := q.inWrite(ctx, func(q *Queries) error {
		if len(u.IfStatus) > 0 {
			cur, ok, err := queryOne(ctx, q, func(sc scanner) (string, error) {
				var s string
				return s, sc.Scan(&s)
			}, `SELECT status FROM jobs WHERE id = ?`, jobID)
			if err != nil {
				return err
			}
			if !ok {
				return notFound("任务", jobID)
			}
			if !slices.Contains(u.IfStatus, cur) {
				return app.E(app.CodeConflict, "任务当前状态为 %s，不允许该操作", cur).
					WithDetail("status", cur).WithDetail("job_id", jobID)
			}
		}
		now := app.Now()
		sets := []string{"rev = rev + 1", "updated_at = ?"}
		args := []any{now}
		p := u.Job
		if p.Status != nil {
			sets, args = append(sets, "status = ?"), append(args, *p.Status)
		}
		if p.Attempt != nil {
			sets, args = append(sets, "attempt = ?"), append(args, *p.Attempt)
		}
		if p.ClearError {
			sets = append(sets, "error = NULL")
		} else if p.Error != nil {
			sets, args = append(sets, "error = ?"), append(args, encodeError(p.Error))
		}
		if p.Result != nil {
			res, err := encodeJSON(*p.Result)
			if err != nil {
				return err
			}
			sets, args = append(sets, "result = ?"), append(args, res)
		}
		if p.StartedAt != nil {
			sets, args = append(sets, "started_at = ?"), append(args, nullable(*p.StartedAt))
		}
		if p.FinishedAt != nil {
			sets, args = append(sets, "finished_at = ?"), append(args, nullable(*p.FinishedAt))
		}
		args = append(args, jobID)
		res, err := q.exec(ctx, `UPDATE jobs SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return notFound("任务", jobID)
		}
		for i := range u.Stages {
			if err := q.patchStage(ctx, jobID, &u.Stages[i]); err != nil {
				return err
			}
		}
		if err := q.insertEvent(ctx, jobID, now, u.Event); err != nil {
			return err
		}
		out, err = q.GetJob(ctx, jobID)
		return err
	})
	return out, err
}

func (q *Queries) patchStage(ctx context.Context, jobID string, p *StagePatch) error {
	var (
		sets []string
		args []any
	)
	if p.Status != nil {
		sets, args = append(sets, "status = ?"), append(args, *p.Status)
	}
	if p.Progress != nil {
		sets, args = append(sets, "progress = ?"), append(args, *p.Progress)
	}
	if p.Fingerprint != nil {
		sets, args = append(sets, "fingerprint = ?"), append(args, nullable(*p.Fingerprint))
	}
	if p.ArtifactID != nil {
		sets, args = append(sets, "artifact_id = ?"), append(args, nullable(*p.ArtifactID))
	}
	if p.Reused != nil {
		sets, args = append(sets, "reused = ?"), append(args, *p.Reused)
	}
	if p.Attempt != nil {
		sets, args = append(sets, "attempt = ?"), append(args, *p.Attempt)
	}
	if p.ClearError {
		sets = append(sets, "error = NULL")
	} else if p.Error != nil {
		sets, args = append(sets, "error = ?"), append(args, encodeError(p.Error))
	}
	if p.StartedAt != nil {
		sets, args = append(sets, "started_at = ?"), append(args, nullable(*p.StartedAt))
	}
	if p.FinishedAt != nil {
		sets, args = append(sets, "finished_at = ?"), append(args, nullable(*p.FinishedAt))
	}
	if len(sets) == 0 {
		return nil
	}
	args = append(args, jobID, p.Stage)
	res, err := q.exec(ctx, `UPDATE job_stages SET `+strings.Join(sets, ", ")+` WHERE job_id = ? AND stage = ?`, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return app.E(app.CodeInternal, "任务 %s 没有阶段 %s", jobID, p.Stage)
	}
	return nil
}

// AddJobEvent appends a job_events row without touching the job.
func (q *Queries) AddJobEvent(ctx context.Context, jobID string, ev EventInput) error {
	return q.insertEvent(ctx, jobID, app.Now(), &ev)
}

// ListJobEvents returns the events of a job, oldest first.
func (q *Queries) ListJobEvents(ctx context.Context, jobID string) ([]JobEvent, error) {
	return queryAll(ctx, q, func(sc scanner) (JobEvent, error) {
		var (
			e    JobEvent
			data string
		)
		err := sc.Scan(&e.ID, &e.JobID, &e.At, &e.Type, &data)
		e.Data = json.RawMessage(data)
		return e, err
	}, `SELECT id, job_id, at, type, data FROM job_events WHERE job_id = ? ORDER BY id`, jobID)
}

// RecoveryResult lists the jobs touched by Recover.
type RecoveryResult struct {
	Cancelled   []string // cancelling → cancelled
	Interrupted []string // running → interrupted (also listed in Requeued when auto-resumed)
	Requeued    []string // interrupted → queued (autoResume only)
}

// JobIDs returns every affected job ID once.
func (r *RecoveryResult) JobIDs() []string {
	ids := slices.Concat(r.Cancelled, r.Interrupted, r.Requeued)
	slices.Sort(ids)
	return slices.Compact(ids)
}

// Recover performs steps 1–2 of the restart recovery in one
// transaction; call it once at engine start, before the scheduler:
//
//  1. running stages → pending; cancelling jobs → cancelled;
//  2. running jobs → interrupted (with an event); when autoResume is true
//     every interrupted job — including those a previous orderly shutdown or
//     a run with auto-resume disabled left behind — goes back to queued.
//
// Every touched job gets rev + 1 per transition and a job_events row.
func (q *Queries) Recover(ctx context.Context, autoResume bool) (*RecoveryResult, error) {
	res := &RecoveryResult{}
	err := q.inWrite(ctx, func(q *Queries) error {
		now := app.Now()
		ids := func(status string) ([]string, error) {
			return queryAll(ctx, q, func(sc scanner) (string, error) {
				var id string
				return id, sc.Scan(&id)
			}, `SELECT id FROM jobs WHERE status = ? ORDER BY id`, status)
		}
		move := func(from, to, event string, finished bool) ([]string, error) {
			list, err := ids(from)
			if err != nil {
				return nil, err
			}
			for _, id := range list {
				set := "status = ?, rev = rev + 1, updated_at = ?"
				args := []any{to, now}
				if finished {
					set += ", finished_at = ?"
					args = append(args, now)
				} else {
					set += ", finished_at = NULL"
				}
				if _, err := q.exec(ctx, `UPDATE jobs SET `+set+` WHERE id = ?`, append(args, id)...); err != nil {
					return nil, err
				}
				if err := q.insertEvent(ctx, id, now, &EventInput{Type: event,
					Data: map[string]any{"recovered": true, "from": from}}); err != nil {
					return nil, err
				}
			}
			return list, nil
		}

		if _, err := q.exec(ctx, `UPDATE job_stages SET status = ?, progress = 0, started_at = NULL, finished_at = NULL
			WHERE status = ?`, app.StagePending, app.StageRunning); err != nil {
			return err
		}
		var err error
		if res.Cancelled, err = move(app.JobCancelling, app.JobCancelled, "job.cancelled", true); err != nil {
			return err
		}
		if res.Interrupted, err = move(app.JobRunning, app.JobInterrupted, "job.interrupted", true); err != nil {
			return err
		}
		if autoResume {
			if res.Requeued, err = move(app.JobInterrupted, app.JobQueued, "job.resumed", false); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}
