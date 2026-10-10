import { memo, useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { Segment } from '../api/types';
import { zh } from '../i18n/zh';
import type { PlayerController } from '../lib/player';
import { useStore } from '../lib/store';
import { findSegmentIndex, formatMs } from '../lib/time';
import s from './TranscriptPanel.module.css';
import { Button, cx } from './ui';

const t = zh.video;
/** Rows per window. Only windows near the viewport are in the DOM. */
const CHUNK = 40;
const ROW_ESTIMATE_PX = 44;

interface ChunkProps {
  index: number;
  segments: Segment[];
  durationMs: number | null;
  rendered: boolean;
  currentId: string | null;
  flashId: string | null;
  flashNonce: number;
  onSeek: (seg: Segment) => void;
  heights: Map<number, number>;
}

const Chunk = memo(function Chunk(p: ChunkProps) {
  const ref = useRef<HTMLDivElement>(null);
  // Remember the real height so an unmounted window keeps its place in the scrollbar.
  useLayoutEffect(() => {
    if (p.rendered && ref.current) p.heights.set(p.index, ref.current.offsetHeight);
  });
  const placeholder = p.heights.get(p.index) ?? p.segments.length * ROW_ESTIMATE_PX;
  return (
    <div ref={ref} data-chunk={p.index} style={p.rendered ? undefined : { height: placeholder }}>
      {p.rendered &&
        p.segments.map((seg) => {
          const current = seg.segment_id === p.currentId;
          const flash = seg.segment_id === p.flashId;
          return (
            <button
              key={flash ? `${seg.segment_id}:${p.flashNonce}` : seg.segment_id}
              type="button"
              className={cx(s.row, current && s.current, flash && s.flash)}
              data-segment-id={seg.segment_id}
              data-current={current ? 'true' : undefined}
              data-cited={flash ? 'true' : undefined}
              aria-current={current ? 'true' : undefined}
              aria-label={`${t.seekTo(formatMs(seg.start_ms, p.durationMs))}：${seg.text}`}
              onClick={() => p.onSeek(seg)}
            >
              <span className={s.time}>{formatMs(seg.start_ms, p.durationMs)}</span>
              <span>{seg.text}</span>
            </button>
          );
        })}
    </div>
  );
});

export interface TranscriptTarget {
  segmentId: string;
  /** Changes on every jump so the same segment can be targeted twice in a row. */
  nonce: number;
}

export interface TranscriptPanelProps {
  segments: Segment[];
  controller: PlayerController;
  durationMs: number | null;
  /** Segment a citation just jumped to: scrolled into view and flashed. */
  target: TranscriptTarget | null;
}

export function TranscriptPanel({ segments, controller, durationMs, target }: TranscriptPanelProps) {
  const scroller = useRef<HTMLDivElement>(null);
  const heights = useRef(new Map<number, number>()).current;
  const [visible, setVisible] = useState<ReadonlySet<number>>(() => new Set([0]));
  const [follow, setFollow] = useState(true);
  const [flash, setFlash] = useState<TranscriptTarget | null>(null);

  const ms = useStore(controller.time);
  const currentIndex = findSegmentIndex(segments, ms);
  const currentId = currentIndex >= 0 ? segments[currentIndex]!.segment_id : null;

  const chunks = useMemo(() => {
    const out: Segment[][] = [];
    for (let i = 0; i < segments.length; i += CHUNK) out.push(segments.slice(i, i + CHUNK));
    return out;
  }, [segments]);
  const indexOf = useMemo(() => new Map(segments.map((seg, i) => [seg.segment_id, i])), [segments]);

  useEffect(() => {
    heights.clear();
  }, [segments, heights]);

  // Window management: one observer tells us which chunks are near the viewport.
  useEffect(() => {
    const root = scroller.current;
    if (!root) return;
    const io = new IntersectionObserver(
      (entries) => {
        setVisible((prev) => {
          const next = new Set(prev);
          for (const e of entries) {
            const i = Number((e.target as HTMLElement).dataset.chunk);
            if (e.isIntersecting) next.add(i);
            else next.delete(i);
          }
          return next;
        });
      },
      { root, rootMargin: '600px 0px' },
    );
    root.querySelectorAll<HTMLElement>('[data-chunk]').forEach((el) => io.observe(el));
    return () => io.disconnect();
  }, [chunks]);

  const scrollToSegment = useCallback((segmentId: string, behavior: ScrollBehavior) => {
    const root = scroller.current;
    const row = root?.querySelector<HTMLElement>(`[data-segment-id="${CSS.escape(segmentId)}"]`);
    if (!root || !row) return;
    const top =
      row.getBoundingClientRect().top - root.getBoundingClientRect().top + root.scrollTop -
      root.clientHeight / 2 + row.offsetHeight / 2;
    // scrollTo on the panel (not scrollIntoView) so the page itself never moves.
    root.scrollTo({ top: Math.max(0, top), behavior });
  }, []);

  // A citation jump: resume following, flash the row, bring it into view.
  useEffect(() => {
    if (!target || !indexOf.has(target.segmentId)) return;
    setFollow(true);
    setFlash(target);
    const clear = setTimeout(() => setFlash((f) => (f?.nonce === target.nonce ? null : f)), 2600);
    return () => clearTimeout(clear);
  }, [target, indexOf]);

  const flashChunk = flash ? Math.floor((indexOf.get(flash.segmentId) ?? 0) / CHUNK) : -1;
  const currentChunk = currentIndex >= 0 ? Math.floor(currentIndex / CHUNK) : -1;

  useLayoutEffect(() => {
    if (!flash) return;
    scrollToSegment(flash.segmentId, 'auto');
    // Chunks above may still swap placeholders for real rows; settle once more.
    const raf = requestAnimationFrame(() => scrollToSegment(flash.segmentId, 'auto'));
    return () => cancelAnimationFrame(raf);
  }, [flash, scrollToSegment]);

  // Follow playback, unless the reader scrolled away on their own.
  useEffect(() => {
    if (!follow || !currentId || flash) return;
    scrollToSegment(currentId, 'smooth');
  }, [currentId, follow, flash, scrollToSegment]);

  const pause = useCallback(() => setFollow(false), []);

  const onSeek = useCallback(
    (seg: Segment) => {
      setFollow(true);
      controller.seek(seg.start_ms, { play: true });
    },
    [controller],
  );

  return (
    <div className={s.panel}>
      <div className={s.bar}>
        <span>{t.segmentCount(segments.length)}</span>
        {follow ? (
          <span>{t.follow}</span>
        ) : (
          <Button
            small
            onClick={() => {
              setFollow(true);
              if (currentId) scrollToSegment(currentId, 'smooth');
            }}
          >
            {t.followPaused} · {t.follow}
          </Button>
        )}
      </div>
      <div
        ref={scroller}
        className={s.scroller}
        data-testid="transcript"
        onWheel={pause}
        onTouchMove={pause}
        onPointerDown={(e) => {
          // Pressing the scroller itself (not a row) means grabbing the scrollbar.
          if (e.target === e.currentTarget) pause();
        }}
        onKeyDown={(e) => {
          if (['ArrowUp', 'ArrowDown', 'PageUp', 'PageDown', 'Home', 'End', ' '].includes(e.key)) pause();
        }}
      >
        {chunks.map((segs, i) => (
          <Chunk
            key={i}
            index={i}
            segments={segs}
            durationMs={durationMs}
            rendered={visible.has(i) || i === currentChunk || i === flashChunk}
            currentId={i === currentChunk ? currentId : null}
            flashId={i === flashChunk && flash ? flash.segmentId : null}
            flashNonce={flash?.nonce ?? 0}
            onSeek={onSeek}
            heights={heights}
          />
        ))}
      </div>
    </div>
  );
}
