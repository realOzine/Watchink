import {
  useInfiniteQuery,
  useMutation,
  useQuery,
  useQueryClient,
  type InfiniteData,
} from '@tanstack/react-query';
import { useStore } from '../lib/store';
import { platform } from '../platform';
import { api } from './index';
import { applyJob, applyTools, streamStatusStore } from './events';
import { isJobActive, keys } from './keys';
import type { Job, JobList, VideoList } from './types';

const PAGE_SIZE = 30;

/** Polling is only a fallback for when the event stream is not delivering. */
function useStreamDown(): boolean {
  return useStore(streamStatusStore) !== 'open';
}

export function useEngine() {
  return useQuery({ queryKey: keys.engine, queryFn: ({ signal }) => api.engine(signal), staleTime: 15_000 });
}

export function useSettings() {
  return useQuery({ queryKey: keys.settings, queryFn: ({ signal }) => api.getSettings(signal), staleTime: 30_000 });
}

export function useTools() {
  const down = useStreamDown();
  return useQuery({
    queryKey: keys.tools,
    queryFn: ({ signal }) => api.getTools(signal),
    staleTime: 15_000,
    refetchInterval: (q) => (down && q.state.data?.items.some((i) => i.status === 'downloading') ? 3000 : false),
  });
}

export function useAgentSetup() {
  return useQuery({ queryKey: keys.agentSetup, queryFn: () => platform.agentSetup(), staleTime: Infinity });
}

export function useVideos(title: string) {
  return useInfiniteQuery({
    queryKey: keys.videos(title),
    queryFn: ({ pageParam, signal }) =>
      api.listVideos({ limit: PAGE_SIZE, cursor: pageParam, title_contains: title }, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (last: VideoList) => last.next_cursor ?? undefined,
    placeholderData: (prev: InfiniteData<VideoList, string | null> | undefined) => prev,
  });
}

export function useVideo(videoId: string | null | undefined) {
  return useQuery({
    queryKey: keys.video(videoId ?? ''),
    queryFn: ({ signal }) => api.getVideo(videoId!, signal),
    enabled: !!videoId,
    staleTime: 10_000,
  });
}

export function useNote(videoId: string, enabled: boolean) {
  return useQuery({
    queryKey: keys.note(videoId),
    queryFn: ({ signal }) => api.getFullNote(videoId, signal),
    enabled,
    staleTime: 5 * 60_000,
  });
}

export function useTranscript(videoId: string, enabled: boolean) {
  return useQuery({
    queryKey: keys.transcript(videoId),
    queryFn: ({ signal }) => api.getFullTranscript(videoId, signal),
    enabled,
    staleTime: 5 * 60_000,
  });
}

export function useJobs(status: string) {
  const down = useStreamDown();
  return useInfiniteQuery({
    queryKey: keys.jobs(status),
    queryFn: ({ pageParam, signal }) => api.listJobs({ limit: PAGE_SIZE, cursor: pageParam, status }, signal),
    initialPageParam: null as string | null,
    getNextPageParam: (last: JobList) => last.next_cursor ?? undefined,
    placeholderData: (prev: InfiniteData<JobList, string | null> | undefined) => prev,
    refetchInterval: (q) =>
      down && q.state.data?.pages.some((p) => (p.jobs ?? []).some((j) => isJobActive(j.status))) ? 5000 : false,
  });
}

export function useJob(jobId: string | null | undefined) {
  const down = useStreamDown();
  return useQuery({
    queryKey: keys.job(jobId ?? ''),
    queryFn: ({ signal }) => api.getJob(jobId!, signal),
    enabled: !!jobId,
    staleTime: 10_000,
    refetchInterval: (q) => (down && isJobActive(q.state.data?.status) ? 3000 : false),
  });
}

/** cancel / retry return the updated Job; feed it through the same path as events. */
export function useJobAction(action: 'cancel' | 'retry') {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (jobId: string) => (action === 'cancel' ? api.cancelJob(jobId) : api.retryJob(jobId)),
    onSuccess: (job: Job) => {
      if (job && typeof job.job_id === 'string') applyJob(qc, job);
      else void qc.invalidateQueries({ queryKey: keys.jobsAll });
    },
  });
}

export function usePrepareTools() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (items: string[]) => api.prepareTools(items),
    onSuccess: (job: Job) => {
      if (job && typeof job.job_id === 'string') applyJob(qc, job);
      void qc.invalidateQueries({ queryKey: keys.tools });
    },
  });
}

export function useUpdateTool() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: (name: string) => api.updateTool(name),
    onSuccess: (tools) => {
      if (tools && Array.isArray(tools.items)) applyTools(qc, tools);
      else void qc.invalidateQueries({ queryKey: keys.tools });
    },
  });
}
