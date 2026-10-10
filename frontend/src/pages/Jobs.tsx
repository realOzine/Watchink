import { useId, useState } from 'react';
import { useQueryClient } from '@tanstack/react-query';
import { Link } from 'react-router';
import { flushListRefresh } from '../api/events';
import { useJobAction, useJobs, useVideo } from '../api/hooks';
import { canCancelJob, canRetryJob, isJobActive, keys } from '../api/keys';
import type { Job } from '../api/types';
import { Page } from '../components/AppShell';
import { JobProgress } from '../components/JobView';
import { Button, EmptyState, ErrorNotice, ErrorState, Loading, StatusBadge, buttonClass, cx } from '../components/ui';
import { zh } from '../i18n/zh';
import { pushToast } from '../lib/toast';
import { formatInstant } from '../lib/time';
import s from './pages.module.css';

const t = zh.jobs;

function JobCard({ job }: { job: Job }) {
  const video = useVideo(job.video_id).data;
  const cancel = useJobAction('cancel');
  const retry = useJobAction('retry');
  const kind = zh.jobKind[job.kind] ?? job.kind;
  const title = job.video_id ? (video?.title ?? job.video_id) : kind;
  // Jobs that need attention (in progress, or stopped short of success) start
  // open; finished ones start collapsed so the list stays scannable. Once the
  // user toggles a card, their choice wins over later status changes.
  const [chosen, setChosen] = useState<boolean | null>(null);
  const open = chosen ?? (isJobActive(job.status) || job.status !== 'succeeded');
  const bodyId = useId();
  return (
    <li className={cx(s.card, s.job)} data-job-id={job.job_id} data-status={job.status}>
      <div className={s.jobHead}>
        <div>
          <button
            type="button"
            className={s.jobToggle}
            aria-expanded={open}
            aria-controls={bodyId}
            title={open ? t.collapse : t.expand}
            onClick={() => setChosen(!open)}
          >
            <span className={cx(s.chevron, open && s.chevronOpen)} aria-hidden="true">
              ▸
            </span>
            <span className={s.jobTitle}>{title}</span>
          </button>
          <div className={s.jobMeta}>
            <StatusBadge status={job.status} labels={zh.status.job} />
            <span>{kind}</span>
            {job.attempt > 1 && <span>{t.attempt(job.attempt)}</span>}
            <span>
              {t.created} {formatInstant(job.created_at)}
            </span>
            {job.finished_at && (
              <span>
                {t.finished} {formatInstant(job.finished_at)}
              </span>
            )}
            {job.requested_by && <span>{t.requestedBy(job.requested_by)}</span>}
          </div>
        </div>
        <div className={s.jobActions}>
          {job.video_id && (
            <Link className={buttonClass('accent', true)} to={`/videos/${encodeURIComponent(job.video_id)}`}>
              {t.openVideo}
            </Link>
          )}
          {/* cancel for queued / running, retry for failed / cancelled / interrupted. */}
          <Button
            small
            variant="danger"
            disabled={!canCancelJob(job.status) || cancel.isPending}
            onClick={() =>
              cancel.mutate(job.job_id, { onSuccess: () => pushToast({ kind: 'info', message: t.cancelled }) })
            }
          >
            {job.status === 'cancelling' ? t.cancelConfirm : t.cancel}
          </Button>
          <Button
            small
            variant={canRetryJob(job.status) ? 'primary' : 'default'}
            disabled={!canRetryJob(job.status) || retry.isPending}
            onClick={() =>
              retry.mutate(job.job_id, { onSuccess: () => pushToast({ kind: 'success', message: t.retried }) })
            }
          >
            {t.retry}
          </Button>
        </div>
      </div>
      <div id={bodyId} className={s.jobBody} hidden={!open}>
        {job.error && (
          <ErrorNotice
            error={job.error}
            title={
              job.error.stage
                ? `${zh.errors.codes[job.error.code] ?? job.error.code} · ${zh.stages[job.error.stage] ?? job.error.stage}`
                : undefined
            }
          />
        )}
        <JobProgress job={job} />
      </div>
    </li>
  );
}

export function JobsPage() {
  const [status, setStatus] = useState('');
  const qc = useQueryClient();
  const q = useJobs(status);
  const jobs = q.data?.pages.flatMap((p) => p.jobs ?? []) ?? [];

  let body;
  if (q.isPending) body = <Loading />;
  else if (q.isError) body = <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  else if (jobs.length === 0) body = <EmptyState title={status ? t.emptyFiltered : t.empty} />;
  else {
    body = (
      <>
        <ul className={s.jobs} style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          {jobs.map((job) => (
            <JobCard key={job.job_id} job={job} />
          ))}
        </ul>
        {q.hasNextPage && (
          <div className={s.more}>
            <Button disabled={q.isFetchingNextPage} onClick={() => void q.fetchNextPage().finally(() => flushListRefresh(qc, keys.jobsAll))}>
              {q.isFetchingNextPage ? zh.library.loadingMore : t.loadMore}
            </Button>
          </div>
        )}
      </>
    );
  }

  return (
    <Page title={t.title}>
      <div className={s.toolbar}>
        <div className={s.chips} role="group" aria-label={t.filterLabel}>
          {t.filters.map((f) => (
            <button
              key={f.value}
              type="button"
              className={cx(s.chip, status === f.value && s.chipActive)}
              aria-pressed={status === f.value}
              onClick={() => setStatus(f.value)}
            >
              {f.label}
            </button>
          ))}
        </div>
      </div>
      {body}
    </Page>
  );
}
