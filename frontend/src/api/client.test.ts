import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Connection, Platform } from '../platform/types';
import { buildUrl, createClient, unauthorizedStore } from './client';
import { ApiError, NetworkError } from './errors';

function fakePlatform(over: Partial<Platform> & { conn?: Partial<Connection> } = {}) {
  const conn: Connection = { baseUrl: '', headers: {}, mutationHeaders: { 'X-WK-Request': '1' }, credentials: 'same-origin', ...over.conn };
  const connect = vi.fn((_opts?: { refresh?: boolean }) => Promise.resolve(conn));
  const platform = {
    mode: 'web',
    requestedBy: 'web',
    router: 'browser',
    canReconnect: false,
    localFile: { kind: 'upload' },
    saveExport: () => Promise.resolve({ saved: true, path: null }),
    openExternal: () => undefined,
    revealInFileManager: null,
    agentSetup: () => Promise.resolve(null),
    ...over,
    connect,
  } as Platform;
  return { platform, connect };
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status });
type Call = [string, RequestInit];

describe('api client', () => {
  beforeEach(() => unauthorizedStore.set(false));

  it('builds URLs under /api/v1 and drops empty query values', () => {
    expect(buildUrl('', '/videos', { limit: 30, cursor: null, title_contains: '' })).toBe('/api/v1/videos?limit=30');
    expect(buildUrl('http://127.0.0.1:47823', '/jobs', { status: 'failed' })).toBe('http://127.0.0.1:47823/api/v1/jobs?status=failed');
  });

  it('web: same-origin cookies, X-WK-Request only on state-changing requests', async () => {
    const fetchMock = vi.fn((_url: string, _init: RequestInit) => Promise.resolve(json({})));
    const api = createClient(fakePlatform().platform, fetchMock as unknown as typeof fetch);
    await api.engine();
    await api.importVideo({ source: { type: 'url', url: 'https://www.bilibili.com/video/BV1' }, language: 'zh' });
    await api.patchSettings({ llm: { model: 'm' } });
    await api.setLlmApiKey('sk-x');

    const calls = fetchMock.mock.calls as unknown as Call[];
    const headers = (i: number) => calls[i]![1].headers as Record<string, string>;
    expect(calls[0]![0]).toBe('/api/v1/engine');
    expect(calls[0]![1].credentials).toBe('same-origin');
    expect(headers(0)['X-WK-Request']).toBeUndefined();
    expect(headers(0).Authorization).toBeUndefined();
    expect(calls.slice(1).map((c) => c[1].method)).toEqual(['POST', 'PATCH', 'PUT']);
    for (const i of [1, 2, 3]) expect(headers(i)['X-WK-Request']).toBe('1');
    expect(JSON.parse(calls[1]![1].body as string)).toEqual({
      source: { type: 'url', url: 'https://www.bilibili.com/video/BV1' },
      language: 'zh',
      requested_by: 'web',
    });
    expect(calls[3]![0]).toBe('/api/v1/settings/secrets/llm_api_key');
    expect(JSON.parse(calls[3]![1].body as string)).toEqual({ value: 'sk-x' });
  });

  it('desktop: bearer token to the absolute engine URL, no cookies', async () => {
    const fetchMock = vi.fn((_url: string, _init: RequestInit) => Promise.resolve(json({})));
    const { platform } = fakePlatform({
      canReconnect: true,
      requestedBy: 'desktop',
      conn: { baseUrl: 'http://127.0.0.1:47823', headers: { Authorization: 'Bearer tok' }, mutationHeaders: {}, credentials: 'omit' },
    });
    await createClient(platform, fetchMock as unknown as typeof fetch).cancelJob('job_1');
    const [url, init] = (fetchMock.mock.calls as unknown as Call[])[0]!;
    expect(url).toBe('http://127.0.0.1:47823/api/v1/jobs/job_1/cancel');
    expect(init.credentials).toBe('omit');
    expect((init.headers as Record<string, string>).Authorization).toBe('Bearer tok');
  });

  it('desktop: re-asks the shell and retries once after a network error', async () => {
    const fetchMock = vi
      .fn<(u: string, i: RequestInit) => Promise<Response>>()
      .mockRejectedValueOnce(new TypeError('Failed to fetch'))
      .mockResolvedValueOnce(json({ version: '1' }));
    const { platform, connect } = fakePlatform({ canReconnect: true });
    const out = await createClient(platform, fetchMock as unknown as typeof fetch).engine();
    expect(out).toEqual({ version: '1' });
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(connect.mock.calls.map((c) => c[0])).toEqual([undefined, { refresh: true }]);
  });

  it('desktop: gives up after the single retry', async () => {
    const fetchMock = vi.fn(() => Promise.reject(new TypeError('Failed to fetch')));
    const { platform } = fakePlatform({ canReconnect: true });
    await expect(createClient(platform, fetchMock as unknown as typeof fetch).engine()).rejects.toBeInstanceOf(NetworkError);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it('web: does not retry, and a 401 raises the unauthorised flag', async () => {
    const down = vi.fn(() => Promise.reject(new TypeError('Failed to fetch')));
    await expect(createClient(fakePlatform().platform, down as unknown as typeof fetch).engine()).rejects.toBeInstanceOf(NetworkError);
    expect(down).toHaveBeenCalledTimes(1);
    expect(unauthorizedStore.get()).toBe(false);

    const denied = vi.fn(() => Promise.resolve(json({ error: { code: 'UNAUTHORIZED', message: 'no session' } }, 401)));
    await expect(createClient(fakePlatform().platform, denied as unknown as typeof fetch).engine()).rejects.toMatchObject({
      code: 'UNAUTHORIZED',
    });
    expect(unauthorizedStore.get()).toBe(true);
  });

  it('turns error envelopes into ApiError and does not retry them', async () => {
    const fetchMock = vi.fn(() =>
      Promise.resolve(json({ error: { code: 'CONFLICT', message: '不能取消', retryable: false, user_action_required: true } }, 409)),
    );
    const { platform } = fakePlatform({ canReconnect: true });
    const p = createClient(platform, fetchMock as unknown as typeof fetch).cancelJob('job_1');
    await expect(p).rejects.toBeInstanceOf(ApiError);
    await expect(p).rejects.toMatchObject({ code: 'CONFLICT', status: 409 });
    expect(fetchMock).toHaveBeenCalledTimes(1);
  });

  it('reads every page of a long note and pins the note version', async () => {
    const page = (content: string, next: number | null, ref: number) => ({
      note_id: 'note_1', video_id: 'vid_1', content, total_chars: 9, next_offset: next,
      citations: [{ ref_no: ref, segment_id: `seg_${ref}`, start_ms: ref, end_ms: ref + 1, point_id: 'p' }],
    });
    const fetchMock = vi
      .fn<(u: string, i: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(json(page('---\na', 3, 1)))
      .mockResolvedValueOnce(json(page(': 1\n', 6, 2)))
      .mockResolvedValueOnce(json(page('---\n# T', null, 2)));
    const note = await createClient(fakePlatform().platform, fetchMock as unknown as typeof fetch).getFullNote('vid_1');
    expect(note.content).toBe('---\na: 1\n---\n# T');
    expect(note.citations.map((c) => c.ref_no)).toEqual([1, 2]);
    expect('next_offset' in note).toBe(false);
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      '/api/v1/videos/vid_1/note?format=markdown',
      '/api/v1/videos/vid_1/note?format=markdown&offset=3&note_id=note_1',
      '/api/v1/videos/vid_1/note?format=markdown&offset=6&note_id=note_1',
    ]);
  });

  it('follows truncated transcripts, de-duplicates and stops if the cursor does not advance', async () => {
    const seg = (i: number) => ({ segment_id: `seg_${i}`, idx: i, start_ms: i * 1000, end_ms: i * 1000 + 900, text: 't' });
    const page = (segments: unknown[], next: number | null) => ({
      transcript_id: 'tr_1', video_id: 'vid_1', language: 'zh', engine: 'e', model: 'm', segment_count: 4, format: 'json',
      segments, truncated: next != null, next_start_ms: next,
    });
    const fetchMock = vi
      .fn<(u: string, i: RequestInit) => Promise<Response>>()
      .mockResolvedValueOnce(json(page([seg(1), seg(2)], 2000)))
      .mockResolvedValueOnce(json(page([seg(2), seg(3), seg(4)], 2000)));
    const tr = await createClient(fakePlatform().platform, fetchMock as unknown as typeof fetch).getFullTranscript('vid_1');
    expect(tr.segments.map((s) => s.idx)).toEqual([1, 2, 3, 4]);
    expect(fetchMock).toHaveBeenCalledTimes(2);
    expect(fetchMock.mock.calls[1]![0]).toBe('/api/v1/videos/vid_1/transcript?format=json&start_ms=2000');
  });
});
