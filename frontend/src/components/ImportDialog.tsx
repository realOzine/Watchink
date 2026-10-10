import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useId, useRef, useState, type DragEvent, type FormEvent } from 'react';
import { useNavigate } from 'react-router';
import { api, type ImportInput } from '../api';
import { applyJob } from '../api/events';
import { canRetryJob, keys } from '../api/keys';
import type { ImportResponse } from '../api/types';
import { zh } from '../i18n/zh';
import { formatBytes, formatPercent } from '../lib/format';
import { pushToast } from '../lib/toast';
import { platform } from '../platform';
import s from './ImportDialog.module.css';
import { Button, Dialog, ErrorNotice, Field, Notice, ProgressBar, cx, inputClass } from './ui';

const t = zh.importDialog;
type Tab = 'file' | 'url';
type Phase = { kind: 'idle' } | { kind: 'uploading'; fraction: number } | { kind: 'submitting' };

export function ImportDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const uid = useId();
  const local = platform.localFile;

  const [tab, setTab] = useState<Tab>('file');
  const [file, setFile] = useState<File | null>(null);
  const [path, setPath] = useState<string | null>(null);
  const [url, setUrl] = useState('');
  const [language, setLanguage] = useState('auto');
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });
  const [error, setError] = useState<unknown>(null);
  const [validation, setValidation] = useState<string | null>(null);
  const [previous, setPrevious] = useState<ImportResponse | null>(null);
  const [dragOver, setDragOver] = useState(false);
  const abortRef = useRef<AbortController | null>(null);
  const fileInput = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) return;
    // Reset when closed so the next import starts clean; abort a running upload.
    abortRef.current?.abort();
    setFile(null);
    setPath(null);
    setUrl('');
    setPhase({ kind: 'idle' });
    setError(null);
    setValidation(null);
    setPrevious(null);
  }, [open]);

  const busy = phase.kind !== 'idle';

  function finish(res: ImportResponse) {
    void qc.invalidateQueries({ queryKey: keys.videosAll });
    void qc.invalidateQueries({ queryKey: keys.jobsAll });
    void qc.invalidateQueries({ queryKey: keys.engine });
    if ((res.warnings ?? []).includes('MODEL_NOT_CONFIGURED')) {
      pushToast({
        kind: 'info',
        message: t.modelWarning,
        action: { label: zh.errors.goSettings, to: '/settings' },
      });
    }
    // a previous failed / cancelled job is returned as-is, never retried silently.
    if (canRetryJob(res.status)) {
      setPrevious(res);
      setPhase({ kind: 'idle' });
      return;
    }
    if (res.status === 'already_processed') pushToast({ kind: 'info', message: t.alreadyProcessed });
    else if (res.deduplicated) pushToast({ kind: 'info', message: t.deduplicated });
    else pushToast({ kind: 'success', message: t.queued });
    onClose();
    void navigate(`/videos/${encodeURIComponent(res.video_id)}`);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (busy) return;
    setError(null);
    setValidation(null);
    setPrevious(null);
    try {
      let source: ImportInput['source'];
      if (tab === 'url') {
        const trimmed = url.trim();
        if (!/^https?:\/\/\S+$/i.test(trimmed)) {
          setValidation(t.urlInvalid);
          return;
        }
        source = { type: 'url', url: trimmed };
        setPhase({ kind: 'submitting' });
      } else if (local.kind === 'pick') {
        if (!path) {
          setValidation(t.needFile);
          return;
        }
        source = { type: 'file', path };
        setPhase({ kind: 'submitting' });
      } else {
        if (!file) {
          setValidation(t.needFile);
          return;
        }
        const ctl = new AbortController();
        abortRef.current = ctl;
        setPhase({ kind: 'uploading', fraction: 0 });
        const up = await api.upload(file, (fraction) => setPhase({ kind: 'uploading', fraction }), ctl.signal);
        source = { type: 'upload', upload_id: up.upload_id };
        setPhase({ kind: 'submitting' });
      }
      finish(await api.importVideo({ source, language }));
    } catch (err) {
      setPhase({ kind: 'idle' });
      if (err instanceof DOMException && err.name === 'AbortError') return;
      setError(err);
    }
  }

  async function retryPrevious() {
    if (!previous) return;
    setPhase({ kind: 'submitting' });
    setError(null);
    try {
      const job = await api.retryJob(previous.job_id);
      if (job && typeof job.job_id === 'string') applyJob(qc, job);
      pushToast({ kind: 'success', message: zh.jobs.retried });
      onClose();
      void navigate(`/videos/${encodeURIComponent(previous.video_id)}`);
    } catch (err) {
      setError(err);
    } finally {
      setPhase({ kind: 'idle' });
    }
  }

  function onDrop(e: DragEvent) {
    e.preventDefault();
    setDragOver(false);
    const f = e.dataTransfer.files?.[0];
    if (f && !busy) {
      setFile(f);
      setValidation(null);
    }
  }

  const tabButton = (value: Tab, label: string) => (
    <button
      type="button"
      role="tab"
      id={`${uid}-tab-${value}`}
      aria-selected={tab === value}
      aria-controls={`${uid}-panel`}
      className={cx(s.tab, tab === value && s.tabActive)}
      disabled={busy}
      onClick={() => {
        setTab(value);
        setValidation(null);
      }}
    >
      {label}
    </button>
  );

  return (
    <Dialog open={open} title={t.title} onClose={onClose}>
      <form className={s.form} onSubmit={(e) => void submit(e)} noValidate>
        <div className={s.tabs} role="tablist" aria-label={t.title}>
          {tabButton('file', t.tabFile)}
          {tabButton('url', t.tabUrl)}
        </div>

        <div id={`${uid}-panel`} role="tabpanel" aria-labelledby={`${uid}-tab-${tab}`}>
          {tab === 'url' ? (
            <Field label={t.urlLabel} hint={t.urlHint} htmlFor={`${uid}-url`}>
              <input
                id={`${uid}-url`}
                className={inputClass}
                type="url"
                inputMode="url"
                autoComplete="off"
                spellCheck={false}
                placeholder={t.urlPlaceholder}
                value={url}
                disabled={busy}
                onChange={(e) => setUrl(e.target.value)}
              />
            </Field>
          ) : local.kind === 'pick' ? (
            path ? (
              <div className={s.picked}>
                <span className={s.pickedName}>{path}</span>
                <Button small disabled={busy} onClick={() => void local.pick().then((p) => p && setPath(p))}>
                  {t.rechoose}
                </Button>
              </div>
            ) : (
              <Button
                onClick={() =>
                  void local.pick().then((p) => {
                    if (p) {
                      setPath(p);
                      setValidation(null);
                    }
                  })
                }
              >
                {t.pick}
              </Button>
            )
          ) : file ? (
            <div className={s.picked}>
              <span className={s.pickedName}>
                {file.name} · {formatBytes(file.size)}
              </span>
              <Button small disabled={busy} onClick={() => setFile(null)}>
                {t.rechoose}
              </Button>
            </div>
          ) : (
            <div
              className={cx(s.drop, dragOver && s.dropOver)}
              onDragOver={(e) => {
                e.preventDefault();
                setDragOver(true);
              }}
              onDragLeave={() => setDragOver(false)}
              onDrop={onDrop}
            >
              <div className={s.dropTitle}>{t.dropTitle}</div>
              <div>{t.dropOr}</div>
              <Button onClick={() => fileInput.current?.click()}>{t.browse}</Button>
              <input
                ref={fileInput}
                className="visually-hidden"
                type="file"
                accept="video/*,.mkv,.flv,.ts,.mov,.m4v,.webm,.avi"
                tabIndex={-1}
                aria-label={t.browse}
                onChange={(e) => {
                  const f = e.target.files?.[0];
                  if (f) {
                    setFile(f);
                    setValidation(null);
                  }
                }}
              />
              <div className={s.hint}>{t.dropHint}</div>
            </div>
          )}
        </div>

        <Field label={t.language} hint={t.languageHint} htmlFor={`${uid}-lang`}>
          <select
            id={`${uid}-lang`}
            className={inputClass}
            value={language}
            disabled={busy}
            onChange={(e) => setLanguage(e.target.value)}
          >
            {Object.entries(zh.language).map(([value, label]) => (
              <option key={value} value={value}>
                {label}
              </option>
            ))}
          </select>
        </Field>

        {validation && (
          <Notice tone="warn" role="alert">
            {validation}
          </Notice>
        )}
        {error != null && <ErrorNotice error={error} />}
        {previous && (
          <Notice
            tone="warn"
            role="alert"
            title={t.previousFailedTitle}
            actions={
              <>
                <Button small variant="primary" disabled={busy} onClick={() => void retryPrevious()}>
                  {t.retryNow}
                </Button>
                <Button
                  small
                  onClick={() => {
                    onClose();
                    void navigate(`/videos/${encodeURIComponent(previous.video_id)}`);
                  }}
                >
                  {t.viewVideo}
                </Button>
              </>
            }
          >
            {t.previousFailed(zh.status.job[previous.status] ?? previous.status)}
          </Notice>
        )}

        {phase.kind === 'uploading' && (
          <div className={s.uploadRow}>
            <span>{t.uploading(formatPercent(phase.fraction))}</span>
            <ProgressBar value={phase.fraction} label={t.uploading(formatPercent(phase.fraction))} />
          </div>
        )}

        <div className={s.footer}>
          {phase.kind === 'uploading' ? (
            <Button onClick={() => abortRef.current?.abort()}>{t.cancelUpload}</Button>
          ) : (
            <Button onClick={onClose}>{zh.app.cancel}</Button>
          )}
          <Button type="submit" variant="primary" disabled={busy}>
            {phase.kind === 'submitting' ? t.submitting : t.submit}
          </Button>
        </div>
      </form>
    </Dialog>
  );
}
