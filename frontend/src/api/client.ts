import type { Connection, Platform } from '../platform/types';
import { createStore } from '../lib/store';
import { ApiError, NetworkError, decodeApiError } from './errors';
import type {
  EngineInfo,
  Export,
  ImportResponse,
  Job,
  JobList,
  LLMModelList,
  LLMTestResult,
  Note,
  Playback,
  Segment,
  Settings,
  ToolsStatus,
  Transcript,
  Upload,
  Video,
  VideoList,
} from './types';

export const API_PREFIX = '/api/v1';

/** Set once any call is finally rejected as unauthorised; drives the full-page notice. */
export const unauthorizedStore = createStore(false);

type Query = Record<string, string | number | boolean | null | undefined>;

export interface RequestOptions {
  query?: Query;
  body?: unknown;
  signal?: AbortSignal;
}

export type ImportSourceInput =
  | { type: 'file'; path: string }
  | { type: 'url'; url: string }
  | { type: 'upload'; upload_id: string };

export interface ImportInput {
  source: ImportSourceInput;
  language?: string;
  reprocess_from?: 'acquire' | 'transcribe' | 'note';
}

/** JSON merge patch shaped like Settings (PATCH /settings). */
export type SettingsPatch = { [K in keyof Settings]?: Partial<Settings[K]> };

export interface FullNote extends Omit<Note, 'next_offset'> {
  /** All pages concatenated, front matter included. */
  content: string;
}

export interface FullTranscript extends Omit<Transcript, 'truncated' | 'next_start_ms' | 'content'> {
  segments: Segment[];
}

export function buildUrl(baseUrl: string, path: string, query?: Query): string {
  let url = `${baseUrl}${API_PREFIX}${path}`;
  if (query) {
    const params = new URLSearchParams();
    for (const [k, v] of Object.entries(query)) {
      if (v != null && v !== '') params.set(k, String(v));
    }
    const qs = params.toString();
    if (qs) url += `?${qs}`;
  }
  return url;
}

function isAbort(e: unknown): boolean {
  return e instanceof DOMException && e.name === 'AbortError';
}

export function createClient(platform: Platform, fetchImpl: typeof fetch = (...a) => fetch(...a)) {
  async function once<T>(conn: Connection, method: string, path: string, opts: RequestOptions): Promise<T> {
    const headers: Record<string, string> = { Accept: 'application/json', ...conn.headers };
    if (method !== 'GET') Object.assign(headers, conn.mutationHeaders);
    let body: string | undefined;
    if (opts.body !== undefined) {
      headers['Content-Type'] = 'application/json';
      body = JSON.stringify(opts.body);
    }
    let res: Response;
    try {
      res = await fetchImpl(buildUrl(conn.baseUrl, path, opts.query), {
        method,
        headers,
        body,
        credentials: conn.credentials,
        signal: opts.signal,
        cache: 'no-store',
      });
    } catch (e) {
      if (isAbort(e)) throw e;
      throw new NetworkError(e);
    }
    const text = await res.text();
    if (!res.ok) throw decodeApiError(res.status, text);
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new ApiError({ code: 'INTERNAL', message: 'invalid JSON response' }, res.status);
    }
  }

  /**
   * Desktop only: after a network failure or a rejected token the engine may
   * have been restarted, so ask the shell again (it re-spawns the engine) and
   * retry exactly once.
   */
  async function request<T>(method: string, path: string, opts: RequestOptions = {}): Promise<T> {
    try {
      try {
        return await once<T>(await platform.connect(), method, path, opts);
      } catch (e) {
        const recoverable = e instanceof NetworkError || (e instanceof ApiError && e.unauthorized);
        if (!platform.canReconnect || !recoverable) throw e;
        return await once<T>(await platform.connect({ refresh: true }), method, path, opts);
      }
    } catch (e) {
      if (e instanceof ApiError && e.unauthorized) unauthorizedStore.set(true);
      throw e;
    }
  }

  const get = <T>(path: string, query?: Query, signal?: AbortSignal) => request<T>('GET', path, { query, signal });
  const post = <T>(path: string, body?: unknown) => request<T>('POST', path, { body: body ?? {} });
  const id = encodeURIComponent;

  return {
    request,

    /** Resolves an engine-relative URL (`/media/...`, `/api/v1/exports/...`) for this mode. */
    async resolveUrl(url: string): Promise<string> {
      const conn = await platform.connect();
      return new URL(url, conn.baseUrl || window.location.origin).toString();
    },

    engine: (signal?: AbortSignal) => get<EngineInfo>('/engine', undefined, signal),

    listVideos: (q: { limit?: number; cursor?: string | null; title_contains?: string }, signal?: AbortSignal) =>
      get<VideoList>('/videos', q, signal),
    getVideo: (videoId: string, signal?: AbortSignal) => get<Video>(`/videos/${id(videoId)}`, undefined, signal),
    getPlayback: (videoId: string, signal?: AbortSignal) =>
      get<Playback>(`/videos/${id(videoId)}/playback`, undefined, signal),

    /** Reads every page of the note via offset / next_offset (long text). */
    async getFullNote(videoId: string, signal?: AbortSignal): Promise<FullNote> {
      let page = await get<Note>(`/videos/${id(videoId)}/note`, { format: 'markdown' }, signal);
      const first = page;
      let content = page.content;
      const citations = [...(page.citations ?? [])];
      const seen = new Set(citations.map((c) => `${c.ref_no}:${c.segment_id}`));
      let offset = 0;
      while (page.next_offset != null && page.next_offset > offset) {
        offset = page.next_offset;
        // Pin the version so a note regenerated mid-read cannot be spliced in.
        page = await get<Note>(
          `/videos/${id(videoId)}/note`,
          { format: 'markdown', offset, note_id: first.note_id },
          signal,
        );
        content += page.content;
        for (const c of page.citations ?? []) {
          const key = `${c.ref_no}:${c.segment_id}`;
          if (!seen.has(key)) {
            seen.add(key);
            citations.push(c);
          }
        }
      }
      const { next_offset: _next, ...rest } = first;
      return { ...rest, content, citations };
    },

    /** Reads all segments, following `truncated` / `next_start_ms`. */
    async getFullTranscript(videoId: string, signal?: AbortSignal): Promise<FullTranscript> {
      let page = await get<Transcript>(`/videos/${id(videoId)}/transcript`, { format: 'json' }, signal);
      const first = page;
      const segments: Segment[] = [];
      const seen = new Set<string>();
      const add = (list: Segment[] | null) => {
        for (const s of list ?? []) {
          if (!seen.has(s.segment_id)) {
            seen.add(s.segment_id);
            segments.push(s);
          }
        }
      };
      add(page.segments);
      let cursor = -1;
      while (page.truncated && page.next_start_ms != null && page.next_start_ms > cursor) {
        cursor = page.next_start_ms;
        page = await get<Transcript>(
          `/videos/${id(videoId)}/transcript`,
          { format: 'json', start_ms: cursor },
          signal,
        );
        add(page.segments);
      }
      segments.sort((a, b) => a.start_ms - b.start_ms || a.idx - b.idx);
      const { truncated: _t, next_start_ms: _n, content: _c, ...rest } = first;
      return { ...rest, segments };
    },

    createExport: (videoId: string, format: string) => post<Export>(`/videos/${id(videoId)}/exports`, { format }),

    listJobs: (
      q: { status?: string; video_id?: string; limit?: number; cursor?: string | null },
      signal?: AbortSignal,
    ) => get<JobList>('/jobs', q, signal),
    getJob: (jobId: string, signal?: AbortSignal) => get<Job>(`/jobs/${id(jobId)}`, undefined, signal),
    cancelJob: (jobId: string) => post<Job>(`/jobs/${id(jobId)}/cancel`),
    retryJob: (jobId: string) => post<Job>(`/jobs/${id(jobId)}/retry`),

    importVideo: (input: ImportInput) =>
      post<ImportResponse>('/imports', { ...input, requested_by: platform.requestedBy }),

    /**
     * Streams a file as the raw request body. XHR (not fetch) because it is the
     * only way to observe upload progress in every browser.
     */
    async upload(file: File, onProgress: (fraction: number) => void, signal?: AbortSignal): Promise<Upload> {
      const conn = await platform.connect();
      return new Promise<Upload>((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', buildUrl(conn.baseUrl, '/uploads'));
        xhr.withCredentials = conn.credentials === 'include';
        const headers = {
          Accept: 'application/json',
          'Content-Type': 'application/octet-stream',
          'X-Filename': encodeURIComponent(file.name),
          ...conn.headers,
          ...conn.mutationHeaders,
        };
        for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
        xhr.upload.onprogress = (e) => {
          if (e.lengthComputable && e.total > 0) onProgress(e.loaded / e.total);
        };
        xhr.onload = () => {
          if (xhr.status >= 200 && xhr.status < 300) {
            try {
              resolve(JSON.parse(xhr.responseText) as Upload);
            } catch {
              reject(new ApiError({ code: 'INTERNAL', message: 'invalid JSON response' }, xhr.status));
            }
            return;
          }
          const err = decodeApiError(xhr.status, xhr.responseText);
          if (err.unauthorized) unauthorizedStore.set(true);
          reject(err);
        };
        xhr.onerror = () => reject(new NetworkError());
        xhr.onabort = () => reject(new DOMException('Aborted', 'AbortError'));
        signal?.addEventListener('abort', () => xhr.abort(), { once: true });
        xhr.send(file);
      });
    },

    getSettings: (signal?: AbortSignal) => get<Settings>('/settings', undefined, signal),
    patchSettings: (patch: SettingsPatch) => request<Settings>('PATCH', '/settings', { body: patch }),
    /** Write-only; an empty value deletes the stored key. */
    setLlmApiKey: (value: string) =>
      request<void>('PUT', '/settings/secrets/llm_api_key', { body: { value } }),
    testLlm: () => post<LLMTestResult>('/settings/llm/test'),
    /** Models the endpoint offers; base_url / api_key override the saved values for this call only. */
    listLlmModels: (req: { base_url?: string; api_key?: string } = {}, signal?: AbortSignal) =>
      request<LLMModelList>('POST', '/settings/llm/models', { body: req, signal }),

    getTools: (signal?: AbortSignal) => get<ToolsStatus>('/tools', undefined, signal),
    prepareTools: (items: string[] = []) => post<Job>('/tools/prepare', { items }),
    updateTool: (name: string) => post<ToolsStatus>('/tools/update', { name }),
    importTool: (name: string, path: string) => post<ToolsStatus>('/tools/import', { name, path }),
  };
}

export type ApiClient = ReturnType<typeof createClient>;
