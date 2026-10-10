package cli

import (
	"context"
	"math"
	"time"

	"watchink/internal/app"
)

// minPollInterval protects against a hot loop if an engine answers a long
// poll immediately without a new revision.
const minPollInterval = 200 * time.Millisecond

// waitJob long-polls GetJob (wait_seconds ≤ 30 with after_rev, §13.2) until
// the job is terminal or timeout elapses (0 = no limit). onUpdate is called
// for every new revision. timedOut is true when the job was still active at
// the deadline; the last known job is returned in every case.
func (c *call) waitJob(r app.Remote, job *app.Job, timeout time.Duration, onUpdate func(*app.Job)) (last *app.Job, timedOut bool, err error) {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for !job.Terminal() {
		waitSeconds := app.MaxWaitSeconds
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return job, true, nil
			}
			waitSeconds = min(waitSeconds, max(1, int(math.Ceil(remaining.Seconds()))))
		}
		started := time.Now()
		ctx, cancel := c.pollCtx(deadline, waitSeconds)
		next, err := r.GetJob(ctx, app.GetJobRequest{JobID: job.JobID, WaitSeconds: waitSeconds, AfterRev: job.Rev})
		cancel()
		if err != nil {
			if c.ctx.Err() == nil && !deadline.IsZero() && !time.Now().Before(deadline) {
				// Our own deadline cut the poll short: that is a wait
				// timeout, not a request failure.
				return job, true, nil
			}
			return job, false, err
		}
		if next.Rev != job.Rev || next.Status != job.Status {
			job = next
			if onUpdate != nil {
				onUpdate(job)
			}
			continue
		}
		job = next
		if elapsed := time.Since(started); elapsed < minPollInterval {
			pause := minPollInterval - elapsed
			if !deadline.IsZero() {
				pause = min(pause, max(0, time.Until(deadline)))
			}
			select {
			case <-c.ctx.Done():
				return job, false, c.ctx.Err()
			case <-time.After(pause):
			}
		}
	}
	return job, false, nil
}

// pollCtx bounds one long poll: never past the wait deadline, and never
// longer than the poll itself plus the request timeout.
func (c *call) pollCtx(deadline time.Time, waitSeconds int) (context.Context, context.CancelFunc) {
	limit := time.Now().Add(time.Duration(waitSeconds)*time.Second + requestSlack(time.Duration(c.timeout)))
	if !deadline.IsZero() && deadline.Before(limit) {
		limit = deadline
	}
	return context.WithDeadline(c.ctx, limit)
}

func requestSlack(timeout time.Duration) time.Duration {
	if timeout > 0 {
		return timeout
	}
	return 15 * time.Second
}

// progressPrinter returns an onUpdate callback that writes one stderr line
// per visible change.
func (c *call) progressPrinter() func(*app.Job) {
	last := ""
	return func(j *app.Job) {
		if line := progressLine(j); line != last {
			last = line
			c.logf("%s", line)
		}
	}
}

// waitFailure maps the outcome of a wait to the §13.4 exit codes: failed → 8,
// cancelled → 9, still running at the deadline → 11. It returns nil when the
// job succeeded.
//
// With --json the error object is accompanied by the job itself under
// "job". cancelled and "still running" are job states, not error codes
// (§7.7), so they are reported as CONFLICT with details.job_status; the exit
// code is what distinguishes them.
func waitFailure(job *app.Job, timedOut bool) *failure {
	extra := map[string]any{"job": job}
	detail := func(e *app.Error) *app.Error {
		e = e.WithDetail("job_id", job.JobID).WithDetail("job_status", job.Status)
		if job.VideoID != nil {
			e = e.WithDetail("video_id", *job.VideoID)
		}
		return e
	}
	if timedOut {
		e := app.E(app.CodeConflict, "等待超时，任务仍在运行").
			WithHint("任务会继续在 Engine 中执行；用 `watchink jobs get " + job.JobID + " --wait 秒数` 继续等待")
		e.Retryable, e.UserActionRequired = true, false
		return &failure{err: detail(e), exit: app.ExitWaitTimeout, extra: extra}
	}
	switch job.Status {
	case app.JobSucceeded:
		return nil
	case app.JobFailed:
		var e app.Error
		if job.Error != nil {
			// Copy so the job embedded in the output keeps its own details.
			e = *job.Error
			e.Details = make(map[string]any, len(job.Error.Details)+3)
			for k, v := range job.Error.Details {
				e.Details[k] = v
			}
		} else {
			e = *app.E(app.CodeInternal, "任务失败，但没有记录错误信息")
		}
		return &failure{err: detail(&e), exit: app.ExitJobFailed, extra: extra}
	case app.JobCancelled:
		e := app.E(app.CodeConflict, "任务已被取消").
			WithHint("用 `watchink jobs retry " + job.JobID + "` 重新执行")
		return &failure{err: detail(e), exit: app.ExitJobCancelled, extra: extra}
	default:
		// interrupted: the engine stopped while the job was running; it
		// resumes on the next engine start (§7.6).
		e := app.E(app.CodeEngineUnavailable, "Engine 已停止，任务被中断").
			WithHint("任务会在 Engine 下次启动时自动续跑；用 `watchink jobs get " + job.JobID + "` 查看")
		return &failure{err: detail(e), exit: app.ExitEngine, extra: extra}
	}
}
