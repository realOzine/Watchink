import { memo, useId, useMemo, useRef, useState, type ReactNode } from 'react';
import { createPortal } from 'react-dom';
import Markdown, { type Components } from 'react-markdown';
import remarkGfm from 'remark-gfm';
import type { Citation } from '../api/types';
import { zh } from '../i18n/zh';
import { classifyLink, indexCitations, noteUrlTransform, parseSegmentLink, stripFrontMatter } from '../lib/note';
import { formatMs } from '../lib/time';
import { platform } from '../platform';
import s from './NoteView.module.css';
import { cx } from './ui';

const t = zh.video;
const POPOVER_WIDTH = 340;

interface CiteProps {
  citation: Citation | undefined;
  durationMs: number | null;
  onCite: (c: Citation) => void;
  children: ReactNode;
}

/** A `[0:03:12](wk://seg/…)` link: click seeks, hover / focus previews the cited text. */
function Cite({ citation, durationMs, onCite, children }: CiteProps) {
  const ref = useRef<HTMLButtonElement>(null);
  const tipId = useId();
  const [pos, setPos] = useState<{ left: number; top?: number; bottom?: number } | null>(null);

  if (!citation) {
    return (
      <span className={cx(s.cite, s.citeMissing)} title={t.citationMissing}>
        {children}
      </span>
    );
  }

  const show = () => {
    const r = ref.current?.getBoundingClientRect();
    if (!r) return;
    const width = Math.min(POPOVER_WIDTH, window.innerWidth - 24);
    const left = Math.max(12, Math.min(r.left + r.width / 2 - width / 2, window.innerWidth - width - 12));
    // Open below unless the chip sits in the lower part of the viewport.
    setPos(
      r.bottom > window.innerHeight * 0.62
        ? { left, bottom: window.innerHeight - r.top + 6 }
        : { left, top: r.bottom + 6 },
    );
  };
  const hide = () => setPos(null);
  const start = formatMs(citation.start_ms, durationMs);

  return (
    <>
      <button
        ref={ref}
        type="button"
        className={s.cite}
        data-segment-id={citation.segment_id}
        aria-label={t.citationLabel(start)}
        aria-describedby={pos ? tipId : undefined}
        onClick={() => {
          hide();
          onCite(citation);
        }}
        onMouseEnter={show}
        onMouseLeave={hide}
        onFocus={show}
        onBlur={hide}
        onKeyDown={(e) => e.key === 'Escape' && hide()}
      >
        {children}
      </button>
      {pos &&
        createPortal(
          <span id={tipId} role="tooltip" className={s.popover} style={pos}>
            <span className={s.popoverTime}>
              {start} – {formatMs(citation.end_ms, durationMs)}
            </span>
            {citation.text || t.seekTo(start)}
          </span>,
          document.body,
        )}
    </>
  );
}

export interface NoteViewProps {
  /** Canonical Markdown, front matter included. */
  markdown: string;
  citations: Citation[];
  durationMs: number | null;
  onCite: (c: Citation) => void;
}

/**
 * Renders the canonical note. Raw HTML is never enabled (react-markdown
 * ignores it by default and no rehype-raw is installed); only wk://seg/,
 * https:// and http:// links survive, and images are not loaded.
 */
export const NoteView = memo(function NoteView({ markdown, citations, durationMs, onCite }: NoteViewProps) {
  const body = useMemo(() => stripFrontMatter(markdown), [markdown]);
  const bySegment = useMemo(() => indexCitations(citations), [citations]);

  const components = useMemo<Components>(
    () => ({
      a({ href, children }) {
        const kind = classifyLink(href);
        if (kind === 'segment') {
          return (
            <Cite citation={bySegment.get(parseSegmentLink(href)!)} durationMs={durationMs} onCite={onCite}>
              {children}
            </Cite>
          );
        }
        if (kind === 'external' && href) {
          return (
            <a
              href={href}
              target="_blank"
              rel="noopener noreferrer"
              onClick={(e) => {
                e.preventDefault();
                platform.openExternal(href);
              }}
            >
              {children}
            </a>
          );
        }
        return <span>{children}</span>;
      },
      // Notes are text; never fetch remote images on the reader's behalf.
      img: ({ alt }) => (alt ? <span>{alt}</span> : null),
      table: ({ children }) => (
        <div className={s.tableWrap} tabIndex={0}>
          <table>{children}</table>
        </div>
      ),
    }),
    [bySegment, durationMs, onCite],
  );

  return (
    <article className={s.note} data-testid="note">
      <Markdown remarkPlugins={[remarkGfm]} urlTransform={noteUrlTransform} components={components} skipHtml>
        {body}
      </Markdown>
    </article>
  );
});
