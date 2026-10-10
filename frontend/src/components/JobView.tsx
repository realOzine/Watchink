import type { Job, JobStage } from '../api/types';
import { zh } from '../i18n/zh';
import { formatPercent } from '../lib/format';
import s from './JobView.module.css';
import { Badge, ProgressBar, cx } from './ui';

const MARK: Record<string, string> = { succeeded: '✓', failed: '!', cancelled: '–' };

function StageRow({ stage }: { stage: JobStage }) {
  const name = zh.stages[stage.stage] ?? stage.stage;
  const status = zh.status.stage[stage.status] ?? stage.status;
  return (
    <li className={s.stage} data-stage={stage.stage} data-status={stage.status}>
      <span className={cx(s.dot, s[`dot-${stage.status}`])} aria-hidden="true">
        {MARK[stage.status] ?? ''}
      </span>
      <span className={cx(s.stageName, stage.status === 'pending' && s.pending)}>
        <span>{name}</span>
        {stage.reused && (
          <Badge tone="info" plain title={zh.jobs.reusedTitle}>
            {zh.jobs.reused}
          </Badge>
        )}
        {stage.stage === 'playback' && <span className={s.stageMeta}>{zh.jobs.playbackSeparate}</span>}
      </span>
      <span className={s.stageMeta}>
        {stage.status === 'running' ? formatPercent(stage.progress) : status}
      </span>
      {stage.status === 'running' && (
        <div className={s.stageBar}>
          <ProgressBar value={stage.progress > 0 ? stage.progress : null} label={`${name} ${status}`} />
        </div>
      )}
      {stage.error && <div className={s.stageError}>{stage.error.message}</div>}
    </li>
  );
}

/** Overall progress plus the per-stage list of a job; re-renders live from the cache. */
export function JobProgress({ job }: { job: Job }) {
  return (
    <div>
      <div className={s.overall}>
        <ProgressBar value={job.status === 'queued' ? null : job.progress} label={zh.jobs.overall} />
        <span className={s.overallPct}>{formatPercent(job.progress)}</span>
      </div>
      <ol className={s.stages} aria-label={zh.jobs.stagesLabel}>
        {(job.stages ?? []).map((st) => (
          <StageRow key={st.stage} stage={st} />
        ))}
      </ol>
    </div>
  );
}
