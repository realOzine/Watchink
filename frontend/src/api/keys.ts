/** TanStack Query keys. List keys share a one-element prefix so events can target them. */
export const keys = {
  engine: ['engine'] as const,
  settings: ['settings'] as const,
  tools: ['tools'] as const,
  agentSetup: ['agent-setup'] as const,
  videosAll: ['videos'] as const,
  videos: (title: string) => ['videos', { title }] as const,
  video: (id: string) => ['video', id] as const,
  note: (id: string) => ['note', id] as const,
  transcript: (id: string) => ['transcript', id] as const,
  playback: (id: string) => ['playback', id] as const,
  jobsAll: ['jobs'] as const,
  jobs: (status: string) => ['jobs', { status }] as const,
  job: (id: string) => ['job', id] as const,
};

export const ACTIVE_JOB_STATUSES: readonly string[] = ['queued', 'running', 'cancelling'];
export const isJobActive = (status: string | null | undefined): boolean =>
  status != null && ACTIVE_JOB_STATUSES.includes(status);
/** cancel is allowed for queued / running. */
export const canCancelJob = (status: string): boolean => status === 'queued' || status === 'running';
/** retry is allowed for failed / cancelled / interrupted. */
export const canRetryJob = (status: string): boolean =>
  status === 'failed' || status === 'cancelled' || status === 'interrupted';
