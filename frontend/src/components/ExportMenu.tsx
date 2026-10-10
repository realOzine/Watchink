import { useEffect, useRef, useState } from 'react';
import { api } from '../api';
import { zh } from '../i18n/zh';
import { describeError } from '../lib/describeError';
import { pushToast } from '../lib/toast';
import { platform } from '../platform';
import s from '../pages/VideoDetail.module.css';
import { Button } from './ui';

const t = zh.video;
const NOTE_FORMATS = ['md', 'md-raw', 'json'];
const TRANSCRIPT_FORMATS = ['srt', 'vtt', 'txt'];

/** md / md-raw / json need a note; srt / vtt / txt need a transcript. */
export function ExportMenu({ videoId, hasNote, hasTranscript }: { videoId: string; hasNote: boolean; hasTranscript: boolean }) {
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const wrap = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);

  useEffect(() => {
    if (!open) return;
    wrap.current?.querySelector<HTMLButtonElement>('[role="menuitem"]:not(:disabled)')?.focus();
    const onDown = (e: PointerEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false);
    };
    document.addEventListener('pointerdown', onDown);
    return () => document.removeEventListener('pointerdown', onDown);
  }, [open]);

  async function run(format: string) {
    setOpen(false);
    setBusy(true);
    try {
      const exp = await api.createExport(videoId, format);
      const result = await platform.saveExport(exp, await api.resolveUrl(exp.download_url));
      if (!result.saved) return;
      const reveal = platform.revealInFileManager;
      pushToast({
        kind: 'success',
        message: result.path ? t.exportSaved(result.path) : t.exportStarted(exp.filename),
        // Reveal the engine's own copy: the binding only accepts paths under the data dir.
        action: reveal ? { label: t.reveal, onClick: () => void reveal(exp.path) } : undefined,
      });
    } catch (e) {
      const view = describeError(e);
      pushToast({ kind: 'error', message: view.message, detail: view.hint ?? undefined });
    } finally {
      setBusy(false);
    }
  }

  function onKeyDown(e: React.KeyboardEvent) {
    const items = [...(wrap.current?.querySelectorAll<HTMLButtonElement>('[role="menuitem"]:not(:disabled)') ?? [])];
    const i = items.indexOf(document.activeElement as HTMLButtonElement);
    if (e.key === 'Escape') {
      setOpen(false);
      trigger.current?.focus();
    } else if (e.key === 'ArrowDown') {
      e.preventDefault();
      items[(i + 1) % items.length]?.focus();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      items[(i - 1 + items.length) % items.length]?.focus();
    }
  }

  return (
    <div className={s.menuWrap} ref={wrap}>
      <Button
        ref={trigger}
        variant="primary"
        aria-haspopup="menu"
        aria-expanded={open}
        disabled={busy || (!hasNote && !hasTranscript)}
        onClick={() => setOpen((o) => !o)}
      >
        {busy ? t.exporting : t.export} ▾
      </Button>
      {open && (
        <div className={s.menu} role="menu" aria-label={t.exportTitle} onKeyDown={onKeyDown}>
          {[...NOTE_FORMATS, ...TRANSCRIPT_FORMATS].map((format) => {
            const f = t.exportFormats[format]!;
            const enabled = NOTE_FORMATS.includes(format) ? hasNote : hasTranscript;
            return (
              <button
                key={format}
                type="button"
                role="menuitem"
                className={s.menuItem}
                data-format={format}
                disabled={!enabled}
                onClick={() => void run(format)}
              >
                {f.label}
                {f.desc && <span className={s.menuDesc}>{f.desc}</span>}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
