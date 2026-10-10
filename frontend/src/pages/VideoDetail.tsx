import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { Link, useParams, useSearchParams } from 'react-router';
import { ApiError } from '../api';
import { useJob, useJobAction, useNote, useTranscript, useVideo } from '../api/hooks';
import { canCancelJob, canRetryJob, isJobActive } from '../api/keys';
import type { Citation, Job, Segment } from '../api/types';
import { ExportMenu } from '../components/ExportMenu';
import { InfoPanel } from '../components/InfoPanel';
import { JobProgress } from '../components/JobView';
import { NoteView } from '../components/NoteView';
import { NowPlaying, Player } from '../components/Player';
import { TranscriptPanel, type TranscriptTarget } from '../components/TranscriptPanel';
import { Button, EmptyState, ErrorNotice, ErrorState, Loading, StatusBadge, buttonClass, cx } from '../components/ui';
import { zh } from '../i18n/zh';
import { createPlayerController } from '../lib/player';
import { formatDuration, parseTimeParam } from '../lib/time';
import s from './VideoDetail.module.css';

const t = zh.video;
type Tab = 'note' | 'transcript' | 'info';
const TABS: Tab[] = ['note', 'transcript', 'info'];
const NO_SEGMENTS: Segment[] = [];

/** Stage list shown in place of the note while (or after) the job did not produce one. */
function JobPanel({ job }: { job: Job }) {
  const cancel = useJobAction('cancel');
  const retry = useJobAction('retry');
  const title =
    job.status === 'queued'
      ? t.queuedTitle
      : job.status === 'failed'
        ? t.failedTitle
        : job.status === 'cancelled'
          ? t.cancelledTitle
          : job.status === 'interrupted'
            ? t.interruptedTitle
            : t.processingTitle;
  return (
    <div className={s.processing} data-testid="job-panel">
      <div>
        <h2 className={s.processingTitle}>{title}</h2>
        {isJobActive(job.status) && <p className={s.processingBody}>{t.processingBody}</p>}
      </div>
      {job.error && <ErrorNotice error={job.error} />}
      <JobProgress job={job} />
      <div className={s.actions}>
        {canCancelJob(job.status) && (
          <Button variant="danger" disabled={cancel.isPending} onClick={() => cancel.mutate(job.job_id)}>
            {zh.jobs.cancel}
          </Button>
        )}
        {canRetryJob(job.status) && (
          <Button variant="primary" disabled={retry.isPending} onClick={() => retry.mutate(job.job_id)}>
            {zh.jobs.retry}
          </Button>
        )}
        <Link className={buttonClass()} to="/jobs">
          {t.viewJobs}
        </Link>
      </div>
    </div>
  );
}

export function VideoDetailPage() {
  const { id = '' } = useParams();
  const [params] = useSearchParams();
  const video = useVideo(id);
  const v = video.data;
  const job = useJob(v?.latest_job_id).data;

  const controller = useMemo(() => createPlayerController(), [id]);
  const [tab, setTab] = useState<Tab>('note');
  const [target, setTarget] = useState<TranscriptTarget | null>(null);
  const nonce = useRef(0);

  const note = useNote(id, !!v?.note_id);
  const transcript = useTranscript(id, !!v?.transcript_id);
  const segments = transcript.data?.segments ?? NO_SEGMENTS;

  // `/videos/:id?t=<ms>` opens at that time (§10.3).
  const tParam = parseTimeParam(params.get('t'));
  useEffect(() => {
    if (tParam != null) controller.seek(tParam);
  }, [tParam, controller]);

  useEffect(() => {
    setTab('note');
    setTarget(null);
  }, [id]);

  const onCite = useCallback(
    (c: Citation) => {
      controller.seek(c.start_ms, { play: true });
      setTarget({ segmentId: c.segment_id, nonce: ++nonce.current });
      setTab('transcript');
    },
    [controller],
  );

  if (video.isPending) return <Loading />;
  if (video.isError || !v) {
    if (video.error instanceof ApiError && video.error.code === 'NOT_FOUND') {
      return (
        <EmptyState title={t.notFound}>
          <Link className={buttonClass('primary')} to="/">
            {zh.app.backToLibrary}
          </Link>
        </EmptyState>
      );
    }
    return <ErrorState error={video.error} onRetry={() => void video.refetch()} />;
  }

  const active = isJobActive(job?.status);
  const noteStageDone = job?.stages?.some((st) => st.stage === 'note_render' && st.status === 'succeeded') ?? false;
  // §7.2: the note is readable as soon as note_render finished, even while
  // `playback` is still running. Until then the stage list takes its place.
  const showJob = !!job && ((active && !noteStageDone) || (!v.note_id && job.status !== 'succeeded'));

  function onTabKey(e: React.KeyboardEvent) {
    const dir = e.key === 'ArrowRight' ? 1 : e.key === 'ArrowLeft' ? -1 : 0;
    if (!dir) return;
    const next = TABS[(TABS.indexOf(tab) + dir + TABS.length) % TABS.length]!;
    setTab(next);
    document.getElementById(`tab-${next}`)?.focus();
  }

  let notePanel: React.ReactNode;
  if (showJob && job) notePanel = <JobPanel job={job} />;
  else if (!v.note_id) notePanel = <EmptyState title={t.noNote} body={t.noNoteHint} />;
  else if (note.isPending) notePanel = <Loading label={t.noteLoading} />;
  else if (note.isError) notePanel = <ErrorState error={note.error} onRetry={() => void note.refetch()} />;
  else {
    notePanel = (
      <>
        {active && <div className={s.strip}>{t.stillPreparingPlayback}</div>}
        <NoteView
          markdown={note.data.content}
          citations={note.data.citations}
          durationMs={v.duration_ms}
          onCite={onCite}
        />
      </>
    );
  }

  let transcriptPanel: React.ReactNode;
  if (!v.transcript_id) transcriptPanel = <EmptyState title={t.noTranscript} />;
  else if (transcript.isPending) transcriptPanel = <Loading label={t.transcriptLoading} />;
  else if (transcript.isError) {
    transcriptPanel = <ErrorState error={transcript.error} onRetry={() => void transcript.refetch()} />;
  } else {
    transcriptPanel = (
      <TranscriptPanel segments={segments} controller={controller} durationMs={v.duration_ms} target={target} />
    );
  }

  const panels: Record<Tab, React.ReactNode> = {
    note: notePanel,
    transcript: transcriptPanel,
    info: <InfoPanel video={v} note={note.data} transcript={transcript.data} />,
  };

  return (
    <div className={s.layout}>
      <section className={s.left} aria-label={t.playerLabel}>
        <div className={s.head}>
          <div>
            <Link className={s.back} to="/">
              ← {zh.app.nav.library}
            </Link>
            <h1 className={s.title}>{v.title || zh.library.untitled}</h1>
            <div className={s.meta}>
              <StatusBadge status={v.status} labels={zh.status.video} />
              <span>{zh.source[v.source.type] ?? v.source.type}</span>
              <span>{formatDuration(v.duration_ms)}</span>
            </div>
          </div>
          <ExportMenu videoId={v.video_id} hasNote={!!v.note_id} hasTranscript={!!v.transcript_id} />
        </div>
        <div className={s.playerBox}>
          <Player video={v} controller={controller} />
        </div>
        {segments.length > 0 && (
          <div className={s.caption}>
            <NowPlaying controller={controller} segments={segments} />
          </div>
        )}
      </section>

      <section className={s.right}>
        <div className={s.tabs} role="tablist" aria-label={t.tabsLabel} onKeyDown={onTabKey}>
          {TABS.map((name) => (
            <button
              key={name}
              id={`tab-${name}`}
              type="button"
              role="tab"
              aria-selected={tab === name}
              aria-controls={`panel-${name}`}
              tabIndex={tab === name ? 0 : -1}
              className={cx(s.tab, tab === name && s.tabActive)}
              onClick={() => setTab(name)}
            >
              {t.tabs[name]}
            </button>
          ))}
        </div>
        <div className={s.panels}>
          {TABS.map((name) => (
            <div
              key={name}
              id={`panel-${name}`}
              role="tabpanel"
              aria-labelledby={`tab-${name}`}
              className={cx(s.panel, tab !== name && s.panelHidden)}
              inert={tab !== name}
            >
              {panels[name]}
            </div>
          ))}
        </div>
      </section>
    </div>
  );
}
