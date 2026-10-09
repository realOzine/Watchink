import { useQuery, useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState, type ReactNode } from 'react';
import { api } from '../api';
import { keys } from '../api/keys';
import type { Segment, Video } from '../api/types';
import { zh } from '../i18n/zh';
import type { PlayerController } from '../lib/player';
import { useStore } from '../lib/store';
import { findSegmentIndex, formatMs } from '../lib/time';
import s from './Player.module.css';
import { Button, Spinner } from './ui';

const t = zh.video;
/** Ask for a fresh signed URL this long before the current one expires (§10.2). */
const REFRESH_MARGIN_MS = 5 * 60_000;

function Position({ controller, durationMs }: { controller: PlayerController; durationMs: number | null }) {
  const ms = useStore(controller.time);
  return <div className={s.position}>{t.positionOnly(formatMs(ms, durationMs))}</div>;
}

/** The segment under the playhead, shown under the player like a caption. */
export function NowPlaying({ controller, segments }: { controller: PlayerController; segments: Segment[] }) {
  const ms = useStore(controller.time);
  const seg = segments[findSegmentIndex(segments, ms)];
  return (
    <div className={s.caption} aria-live="off">
      <span className={s.captionLabel}>{t.nowPlaying}</span>
      {seg?.text ?? ''}
    </div>
  );
}

export function Player({ video, controller }: { video: Video; controller: PlayerController }) {
  const qc = useQueryClient();
  const videoId = video.video_id;
  const ref = useRef<HTMLVideoElement | null>(null);
  /** URL for which one automatic refresh after a media error was already tried. */
  const retriedFor = useRef<string | null>(null);
  const [failed, setFailed] = useState(false);

  const playback = useQuery({
    queryKey: keys.playback(videoId),
    enabled: video.playback_ready,
    staleTime: Infinity,
    gcTime: 0,
    queryFn: async ({ signal }) => {
      const p = await api.getPlayback(videoId, signal);
      // The URL may be engine-relative (`/media/...`); on desktop that is another origin.
      return { ...p, src: await api.resolveUrl(p.url) };
    },
  });
  const src = playback.data?.src ?? null;
  const expiresAt = playback.data?.expires_at ?? null;

  // Refresh the signed URL shortly before it expires.
  useEffect(() => {
    if (!expiresAt) return;
    const at = new Date(expiresAt).getTime();
    if (Number.isNaN(at)) return;
    const delay = Math.max(30_000, at - Date.now() - REFRESH_MARGIN_MS);
    const timer = setTimeout(() => void qc.invalidateQueries({ queryKey: keys.playback(videoId) }), delay);
    return () => clearTimeout(timer);
  }, [expiresAt, qc, videoId]);

  // A new source restarts at 0, so carry the position over.
  useEffect(() => {
    setFailed(false);
    controller.hold();
  }, [src, controller]);

  useEffect(() => () => controller.attach(null), [controller]);

  function onMediaError() {
    if (src && retriedFor.current !== src) {
      // Most likely an expired or revoked token: fetch a new URL once.
      retriedFor.current = src;
      controller.hold();
      void qc.invalidateQueries({ queryKey: keys.playback(videoId) });
      return;
    }
    setFailed(true);
  }

  function reload() {
    retriedFor.current = null;
    setFailed(false);
    controller.hold();
    void qc.invalidateQueries({ queryKey: keys.playback(videoId) });
  }

  let overlay: ReactNode = null;
  if (!video.playback_ready) {
    const preparing = video.status === 'processing';
    overlay = (
      <div className={s.overlay} role="status">
        <div className={s.overlayTitle}>
          {preparing && <Spinner />}
          {preparing ? t.preparingPlayback : t.noPlayback}
        </div>
        <div className={s.overlayHint}>{preparing ? t.preparingPlaybackHint : t.noPlaybackHint}</div>
        <Position controller={controller} durationMs={video.duration_ms} />
      </div>
    );
  } else if (failed || playback.isError) {
    overlay = (
      <div className={s.overlay} role="alert">
        <div className={s.overlayTitle}>{t.cannotDecode}</div>
        <div className={s.overlayHint}>{t.cannotDecodeHint}</div>
        <Position controller={controller} durationMs={video.duration_ms} />
        <Button small onClick={reload}>
          {t.reloadPlayback}
        </Button>
      </div>
    );
  } else if (!src) {
    overlay = (
      <div className={s.overlay} role="status">
        <Spinner />
      </div>
    );
  }

  return (
    <div className={s.frame}>
      {src && !failed && (
        <video
          key={src}
          ref={(el) => {
            ref.current = el;
            if (el) controller.attach(el);
          }}
          className={s.video}
          src={src}
          controls
          playsInline
          preload="metadata"
          aria-label={t.playerLabel}
          onLoadedMetadata={() => controller.ready()}
          onTimeUpdate={() => controller.sync()}
          onSeeked={() => controller.sync()}
          onError={onMediaError}
        />
      )}
      {overlay}
    </div>
  );
}
