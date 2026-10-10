import type { InfiniteData, QueryClient } from '@tanstack/react-query';
import type { Platform } from '../platform/types';
import { createStore } from '../lib/store';
import { API_PREFIX, unauthorizedStore } from './client';
import { isJobActive, keys } from './keys';
import { SseParser, type SseFrame } from './sse';
import type { Job, JobList, ToolsStatus, Video, VideoList } from './types';

/**
 * Paged lists ("videos", "jobs") that an event wanted refreshed while one of
 * their requests was still running.
 */
const deferredLists = new Set<string>();

/**
 * Refreshes a paged list after an event. A plain invalidation would cancel a
 * "load more" request that is in flight and refetch only the pages already
 * shown, so the click would be lost whenever an event arrives at that moment,
 * which is exactly when a job is running. So a list that is fetching is left
 * alone and remembered; flushListRefresh picks it up afterwards.
 */
function refreshList(qc: QueryClient, queryKey: readonly [string]) {
  if (qc.isFetching({ queryKey }) > 0) deferredLists.add(queryKey[0]);
  void qc.invalidateQueries({ queryKey }, { cancelRefetch: false });
}

/** Runs the refresh that refreshList postponed; call when "load more" settles. */
export function flushListRefresh(qc: QueryClient, queryKey: readonly [string]) {
  if (deferredLists.delete(queryKey[0])) void qc.invalidateQueries({ queryKey }, { cancelRefetch: false });
}

/**
 * connecting: first attempt · open: live · down: dropped, retrying ·
 * stopping: the engine announced it is shutting down.
 */
export type StreamStatus = 'connecting' | 'open' | 'down' | 'stopping';
export const streamStatusStore = createStore<StreamStatus>('connecting');

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null;
}

/**
 * Replaces one row in every cached page of the lists under `prefix`.
 * Returns false when some list must be refetched instead: the row changed
 * status (it may have moved in or out of a filter) or a list that could
 * contain it does not show it yet (new item).
 */
function patchList<P, T extends { status: string }>(
  qc: QueryClient,
  prefix: readonly unknown[],
  rows: (page: P) => T[] | null | undefined,
  withRows: (page: P, rows: T[]) => P,
  same: (row: T) => boolean,
  next: T,
  mayBelong: (filter: Record<string, unknown>) => boolean,
): boolean {
  let refetch = false;
  for (const [key, data] of qc.getQueriesData<InfiniteData<P>>({ queryKey: prefix })) {
    if (!data?.pages) continue;
    let found = false;
    let statusChanged = false;
    const pages = data.pages.map((page) => {
      const list = rows(page) ?? [];
      if (!list.some(same)) return page;
      found = true;
      return withRows(
        page,
        list.map((row) => {
          if (!same(row)) return row;
          if (row.status !== next.status) statusChanged = true;
          return next;
        }),
      );
    });
    if (found) qc.setQueryData<InfiniteData<P>>(key, { ...data, pages });
    const filter = isRecord(key[1]) ? key[1] : {};
    if (statusChanged || (!found && mayBelong(filter))) refetch = true;
  }
  return !refetch;
}

export function applyJob(qc: QueryClient, job: Job): void {
  const prev = qc.getQueryData<Job>(keys.job(job.job_id));
  // Replayed or reordered frames must not roll a job back (§7.3: rev only grows).
  if (prev && prev.rev > job.rev) return;
  qc.setQueryData(keys.job(job.job_id), job);

  const inPlace = patchList<JobList, Job>(
    qc,
    keys.jobsAll,
    (p) => p.jobs,
    (p, jobs) => ({ ...p, jobs }),
    (j) => j.job_id === job.job_id,
    job,
    (f) => !f.status || f.status === job.status,
  );
  if (!inPlace) refreshList(qc, keys.jobsAll);

  const statusChanged = prev?.status !== job.status;
  const finished = !isJobActive(job.status);
  if (statusChanged) void qc.invalidateQueries({ queryKey: keys.engine });
  if (job.kind === 'prepare_tools' && statusChanged && finished) {
    void qc.invalidateQueries({ queryKey: keys.tools });
  }
  if (job.video_id != null) {
    const vid = job.video_id;
    const noteChanged = (prev?.result?.note_id ?? null) !== (job.result?.note_id ?? null);
    const transcriptChanged = (prev?.result?.transcript_id ?? null) !== (job.result?.transcript_id ?? null);
    if (statusChanged) {
      void qc.invalidateQueries({ queryKey: keys.video(vid) });
      refreshList(qc, keys.videosAll);
    }
    if (noteChanged || (statusChanged && finished)) void qc.invalidateQueries({ queryKey: keys.note(vid) });
    if (transcriptChanged || (statusChanged && finished)) {
      void qc.invalidateQueries({ queryKey: keys.transcript(vid) });
    }
  }
}

export function applyVideo(qc: QueryClient, video: Video): void {
  const prev = qc.getQueryData<Video>(keys.video(video.video_id));
  qc.setQueryData(keys.video(video.video_id), video);
  const inPlace = patchList<VideoList, Video>(
    qc,
    keys.videosAll,
    (p) => p.videos,
    (p, videos) => ({ ...p, videos }),
    (v) => v.video_id === video.video_id,
    video,
    (f) => typeof f.title !== 'string' || video.title.toLowerCase().includes(f.title.toLowerCase()),
  );
  if (!inPlace) refreshList(qc, keys.videosAll);
  if (!prev) return;
  if (prev.note_id !== video.note_id) void qc.invalidateQueries({ queryKey: keys.note(video.video_id) });
  if (prev.transcript_id !== video.transcript_id) {
    void qc.invalidateQueries({ queryKey: keys.transcript(video.video_id) });
  }
  if (prev.playback_ready !== video.playback_ready) {
    void qc.invalidateQueries({ queryKey: keys.playback(video.video_id) });
  }
}

export function applyTools(qc: QueryClient, tools: ToolsStatus): void {
  const prev = qc.getQueryData<ToolsStatus>(keys.tools);
  qc.setQueryData(keys.tools, tools);
  if (prev?.ready !== tools.ready) void qc.invalidateQueries({ queryKey: keys.engine });
}

export type EventOutcome = 'applied' | 'reset' | 'stopping' | 'ignored';

/**
 * Applies one event-stream frame to the query cache (docs/v0.md §13.3), so
 * pages are live without polling. `data` is the bare object for the event type.
 */
export function applyEventFrame(qc: QueryClient, frame: SseFrame): EventOutcome {
  if (frame.event === 'reset') {
    void qc.invalidateQueries();
    return 'reset';
  }
  let data: unknown;
  try {
    data = JSON.parse(frame.data);
  } catch {
    return 'ignored';
  }
  if (!isRecord(data)) return 'ignored';
  switch (frame.event) {
    case 'job.updated':
      if (typeof data.job_id !== 'string') return 'ignored';
      applyJob(qc, data as unknown as Job);
      return 'applied';
    case 'video.updated':
      if (typeof data.video_id !== 'string') return 'ignored';
      applyVideo(qc, data as unknown as Video);
      return 'applied';
    case 'tools.updated':
      if (!Array.isArray(data.items)) return 'ignored';
      applyTools(qc, data as unknown as ToolsStatus);
      return 'applied';
    case 'engine.stopping':
      return 'stopping';
    default:
      return 'ignored';
  }
}

const HEARTBEAT_TIMEOUT_MS = 45_000; // the engine sends a comment line every 15 s
const BACKOFF_MS = [1000, 2000, 4000, 8000, 15000];

/**
 * Keeps one fetch-based SSE connection alive (fetch rather than EventSource so
 * the desktop Authorization header can be sent). Returns a stop function.
 */
export function startEventStream(
  qc: QueryClient,
  platform: Platform,
  fetchImpl: typeof fetch = (...a) => fetch(...a),
): () => void {
  let stopped = false;
  let controller: AbortController | null = null;
  let lastEventId: string | null = null;
  let dropped = false;
  let failures = 0;
  let timer: ReturnType<typeof setTimeout> | undefined;

  const sleep = (ms: number) =>
    new Promise<void>((resolve) => {
      timer = setTimeout(resolve, ms);
    });

  async function connectOnce(): Promise<void> {
    const conn = await platform.connect({ refresh: dropped && platform.canReconnect });
    controller = new AbortController();
    const headers: Record<string, string> = { Accept: 'text/event-stream', ...conn.headers };
    if (lastEventId != null) headers['Last-Event-ID'] = lastEventId;
    const res = await fetchImpl(`${conn.baseUrl}${API_PREFIX}/events`, {
      headers,
      credentials: conn.credentials,
      signal: controller.signal,
      cache: 'no-store',
    });
    if (res.status === 401) {
      // No point retrying with the same session; the notice explains what to do.
      if (!platform.canReconnect) {
        unauthorizedStore.set(true);
        stopped = true;
      }
      throw new Error('unauthorized');
    }
    if (!res.ok || !res.body) throw new Error(`event stream HTTP ${res.status}`);

    failures = 0;
    streamStatusStore.set('open');
    // Events may have been missed while disconnected, and the replay buffer is
    // finite, so refresh everything after a drop.
    if (dropped) void qc.invalidateQueries();
    dropped = false;

    const parser = new SseParser();
    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let watchdog: ReturnType<typeof setTimeout> | undefined;
    const arm = () => {
      clearTimeout(watchdog);
      watchdog = setTimeout(() => controller?.abort(), HEARTBEAT_TIMEOUT_MS);
    };
    try {
      arm();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        arm();
        for (const frame of parser.push(decoder.decode(value, { stream: true }))) {
          if (frame.id != null) lastEventId = frame.id;
          const outcome = applyEventFrame(qc, frame);
          if (outcome === 'stopping') streamStatusStore.set('stopping');
          else if (streamStatusStore.get() === 'stopping') streamStatusStore.set('open');
        }
      }
    } finally {
      clearTimeout(watchdog);
    }
  }

  void (async () => {
    while (!stopped) {
      try {
        await connectOnce();
      } catch {
        /* fall through to the retry below */
      }
      if (stopped) break;
      dropped = true;
      if (streamStatusStore.get() !== 'stopping') streamStatusStore.set('down');
      await sleep(BACKOFF_MS[Math.min(failures, BACKOFF_MS.length - 1)]!);
      failures++;
    }
  })();

  return () => {
    stopped = true;
    clearTimeout(timer);
    controller?.abort();
  };
}
