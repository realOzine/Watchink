import { QueryClient, type InfiniteData } from '@tanstack/react-query';
import { beforeEach, describe, expect, it } from 'vitest';
import { applyEventFrame } from './events';
import { keys } from './keys';
import type { Job, JobList, ToolsStatus, Video, VideoList } from './types';

const job = (over: Partial<Job> = {}): Job => ({
  job_id: 'job_1',
  kind: 'process_video',
  video_id: 'vid_1',
  status: 'running',
  rev: 5,
  attempt: 1,
  stage: 'transcribe',
  progress: 0.3,
  stages: [],
  error: null,
  result: { transcript_id: null, note_id: null },
  requested_by: 'web',
  created_at: '2026-10-09T12:00:00Z',
  started_at: null,
  finished_at: null,
  updated_at: '2026-10-09T12:00:01Z',
  ...over,
});

const video = (over: Partial<Video> = {}): Video => ({
  video_id: 'vid_1',
  title: '梯度下降',
  source: { type: 'file', uri: null, platform_id: null },
  duration_ms: 1000,
  language: 'zh',
  status: 'processing',
  latest_job_id: 'job_1',
  transcript_id: null,
  note_id: null,
  playback_ready: false,
  web_url: '',
  created_at: '',
  updated_at: '',
  ...over,
});

const frame = (event: string, data: unknown) => ({ id: '1', event, data: JSON.stringify(data) });
const pages = <T,>(page: T): InfiniteData<T, string | null> => ({ pages: [page], pageParams: [null] });

describe('applyEventFrame', () => {
  let qc: QueryClient;
  const invalid = (key: readonly unknown[]) => qc.getQueryState(key)?.isInvalidated ?? false;

  beforeEach(() => {
    qc = new QueryClient();
  });

  it('writes job.updated into the job entry and patches cached lists in place', () => {
    qc.setQueryData(keys.job('job_1'), job());
    qc.setQueryData(keys.jobs(''), pages<JobList>({ jobs: [job(), job({ job_id: 'job_2' })], next_cursor: null }));

    const next = job({ rev: 6, progress: 0.5 });
    expect(applyEventFrame(qc, frame('job.updated', next))).toBe('applied');

    expect(qc.getQueryData<Job>(keys.job('job_1'))).toEqual(next);
    const list = qc.getQueryData<InfiniteData<JobList>>(keys.jobs(''))!;
    expect(list.pages[0]!.jobs.map((j) => [j.job_id, j.progress])).toEqual([
      ['job_1', 0.5],
      ['job_2', 0.3],
    ]);
    // A pure progress update must not trigger a refetch.
    expect(invalid(keys.jobs(''))).toBe(false);
    expect(invalid(keys.job('job_1'))).toBe(false);
  });

  it('ignores a frame with an older rev', () => {
    qc.setQueryData(keys.job('job_1'), job({ rev: 9, progress: 0.9 }));
    applyEventFrame(qc, frame('job.updated', job({ rev: 8, progress: 0.1 })));
    expect(qc.getQueryData<Job>(keys.job('job_1'))!.progress).toBe(0.9);
  });

  it('refetches lists for a job they do not contain yet', () => {
    qc.setQueryData(keys.jobs(''), pages<JobList>({ jobs: [job({ job_id: 'job_2' })], next_cursor: null }));
    applyEventFrame(qc, frame('job.updated', job({ job_id: 'job_new', status: 'queued' })));
    expect(invalid(keys.jobs(''))).toBe(true);
    expect(qc.getQueryData<Job>(keys.job('job_new'))!.status).toBe('queued');
  });

  it('leaves a status-filtered list alone when the job cannot belong to it', () => {
    qc.setQueryData(keys.jobs('failed'), pages<JobList>({ jobs: [], next_cursor: null }));
    applyEventFrame(qc, frame('job.updated', job({ rev: 6 })));
    expect(invalid(keys.jobs('failed'))).toBe(false);
    applyEventFrame(qc, frame('job.updated', job({ rev: 7, status: 'failed' })));
    expect(invalid(keys.jobs('failed'))).toBe(true);
  });

  it('refreshes video, note and transcript when a job finishes', () => {
    qc.setQueryData(keys.job('job_1'), job());
    qc.setQueryData(keys.jobs(''), pages<JobList>({ jobs: [job()], next_cursor: null }));
    qc.setQueryData(keys.video('vid_1'), video());
    qc.setQueryData(keys.note('vid_1'), { content: 'old' });
    qc.setQueryData(keys.transcript('vid_1'), { segments: [] });
    qc.setQueryData(keys.engine, { active_jobs: 1 });

    applyEventFrame(
      qc,
      frame('job.updated', job({ rev: 20, status: 'succeeded', progress: 1, result: { transcript_id: 'tr_1', note_id: 'note_1' } })),
    );
    for (const key of [keys.jobs(''), keys.video('vid_1'), keys.note('vid_1'), keys.transcript('vid_1'), keys.engine]) {
      expect(invalid(key), JSON.stringify(key)).toBe(true);
    }
  });

  it('refreshes the note as soon as the job reports a note id, before it finishes', () => {
    qc.setQueryData(keys.job('job_1'), job());
    qc.setQueryData(keys.note('vid_1'), { content: 'old' });
    applyEventFrame(qc, frame('job.updated', job({ rev: 6, result: { transcript_id: null, note_id: 'note_2' } })));
    expect(invalid(keys.note('vid_1'))).toBe(true);
  });

  it('applies video.updated to the entry and lists, and invalidates what changed', () => {
    qc.setQueryData(keys.video('vid_1'), video());
    qc.setQueryData(keys.videos(''), pages<VideoList>({ videos: [video()], next_cursor: null }));
    qc.setQueryData(keys.note('vid_1'), { content: 'old' });
    qc.setQueryData(keys.playback('vid_1'), { url: 'x' });

    const sameStatus = video({ title: '新标题', playback_ready: true });
    expect(applyEventFrame(qc, frame('video.updated', sameStatus))).toBe('applied');
    expect(qc.getQueryData<Video>(keys.video('vid_1'))!.title).toBe('新标题');
    expect(qc.getQueryData<InfiniteData<VideoList>>(keys.videos(''))!.pages[0]!.videos[0]!.title).toBe('新标题');
    expect(invalid(keys.videos(''))).toBe(false);
    expect(invalid(keys.playback('vid_1'))).toBe(true);
    expect(invalid(keys.note('vid_1'))).toBe(false);

    applyEventFrame(qc, frame('video.updated', video({ status: 'ready', note_id: 'note_1', playback_ready: true })));
    expect(invalid(keys.videos(''))).toBe(true);
    expect(invalid(keys.note('vid_1'))).toBe(true);
  });

  it('only refetches title-filtered video lists the video could match', () => {
    qc.setQueryData(keys.videos('线性'), pages<VideoList>({ videos: [], next_cursor: null }));
    applyEventFrame(qc, frame('video.updated', video({ video_id: 'vid_9', title: '梯度下降' })));
    expect(invalid(keys.videos('线性'))).toBe(false);
    applyEventFrame(qc, frame('video.updated', video({ video_id: 'vid_8', title: '线性代数' })));
    expect(invalid(keys.videos('线性'))).toBe(true);
  });

  it('stores tools.updated and refreshes the engine when readiness flips', () => {
    const tools = (ready: boolean): ToolsStatus => ({ items: [], ready, missing_download_bytes: 0, default_asr_model: 'm' });
    qc.setQueryData(keys.tools, tools(false));
    qc.setQueryData(keys.engine, { tools_ready: false });
    applyEventFrame(qc, frame('tools.updated', tools(false)));
    expect(invalid(keys.engine)).toBe(false);
    applyEventFrame(qc, frame('tools.updated', tools(true)));
    expect(qc.getQueryData<ToolsStatus>(keys.tools)!.ready).toBe(true);
    expect(invalid(keys.engine)).toBe(true);
  });

  it('invalidates everything on reset', () => {
    qc.setQueryData(keys.engine, {});
    qc.setQueryData(keys.video('vid_1'), video());
    qc.setQueryData(keys.jobs(''), pages<JobList>({ jobs: [], next_cursor: null }));
    expect(applyEventFrame(qc, { id: null, event: 'reset', data: '{}' })).toBe('reset');
    for (const key of [keys.engine, keys.video('vid_1'), keys.jobs('')]) expect(invalid(key)).toBe(true);
  });

  it('reports engine.stopping and ignores malformed or unknown frames', () => {
    expect(applyEventFrame(qc, frame('engine.stopping', { reason: 'idle' }))).toBe('stopping');
    expect(applyEventFrame(qc, { id: '2', event: 'job.updated', data: '{not json' })).toBe('ignored');
    expect(applyEventFrame(qc, frame('job.updated', { rev: 1 }))).toBe('ignored');
    expect(applyEventFrame(qc, frame('job.updated', null))).toBe('ignored');
    expect(applyEventFrame(qc, frame('something.else', {}))).toBe('ignored');
    expect(qc.getQueryCache().getAll()).toHaveLength(0);
  });
});
