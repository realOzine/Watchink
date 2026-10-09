import {
  useEffect,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type ReactNode,
  type Ref,
} from 'react';
import { Link } from 'react-router';
import { zh } from '../i18n/zh';
import { clamp01 } from '../lib/format';
import { describeError, needsSettings } from '../lib/describeError';
import s from './ui.module.css';

export function cx(...parts: (string | false | null | undefined)[]): string {
  return parts.filter(Boolean).join(' ');
}

type Variant = 'default' | 'primary' | 'accent' | 'ghost' | 'danger';

export function buttonClass(variant: Variant = 'default', small = false): string {
  return cx(s.button, variant !== 'default' && s[variant], small && s.small);
}

interface ButtonProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: Variant;
  small?: boolean;
  ref?: Ref<HTMLButtonElement>;
}

export function Button({ variant, small, className, type, ...rest }: ButtonProps) {
  return <button type={type ?? 'button'} className={cx(buttonClass(variant, small), className)} {...rest} />;
}

export function ProgressBar({ value, label }: { value: number | null; label: string }) {
  const v = value == null ? null : clamp01(value);
  return (
    <div
      className={cx(s.progress, v == null && s.progressIndeterminate)}
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={v == null ? undefined : Math.round(v * 100)}
    >
      <div className={s.progressFill} style={{ width: `${(v ?? 0) * 100}%` }} />
    </div>
  );
}

export type Tone = 'neutral' | 'ok' | 'danger' | 'warn' | 'info' | 'active';

export function Badge({ tone = 'neutral', plain, title, children }: { tone?: Tone; plain?: boolean; title?: string; children: ReactNode }) {
  return (
    <span className={cx(s.badge, tone !== 'neutral' && s[`tone-${tone}`], plain && s.plain)} title={title}>
      {children}
    </span>
  );
}

const STATUS_TONE: Record<string, Tone> = {
  queued: 'info',
  pending: 'neutral',
  running: 'active',
  processing: 'active',
  downloading: 'active',
  cancelling: 'warn',
  succeeded: 'ok',
  ready: 'ok',
  failed: 'danger',
  cancelled: 'neutral',
  interrupted: 'warn',
  missing: 'warn',
};

export function StatusBadge({ status, labels }: { status: string; labels: Record<string, string> }) {
  return <Badge tone={STATUS_TONE[status] ?? 'neutral'}>{labels[status] ?? status}</Badge>;
}

export function Spinner() {
  return <span className={s.spinner} aria-hidden="true" />;
}

export function Loading({ label = zh.app.loading }: { label?: string }) {
  return (
    <div className={s.loading} role="status">
      <Spinner />
      <span>{label}</span>
    </div>
  );
}

export function EmptyState({ title, body, children }: { title: string; body?: ReactNode; children?: ReactNode }) {
  return (
    <div className={s.state}>
      <h2 className={s.stateTitle}>{title}</h2>
      {body && <p className={s.stateBody}>{body}</p>}
      {children && <div className={s.stateActions}>{children}</div>}
    </div>
  );
}

export function Notice({
  tone = 'neutral',
  title,
  children,
  actions,
  role,
}: {
  tone?: 'neutral' | 'danger' | 'warn' | 'ok' | 'info';
  title?: ReactNode;
  children?: ReactNode;
  actions?: ReactNode;
  role?: 'alert' | 'status';
}) {
  return (
    <div className={cx(s.notice, tone !== 'neutral' && s[`notice-${tone}`])} role={role}>
      {title && <div className={s.noticeTitle}>{title}</div>}
      {children && <div>{children}</div>}
      {actions && <div className={s.noticeActions}>{actions}</div>}
    </div>
  );
}

/** Message + hint (+ link to settings when the user has to fix configuration). */
export function ErrorNotice({ error, actions, title }: { error: unknown; actions?: ReactNode; title?: string }) {
  const view = describeError(error);
  const toSettings = needsSettings(view);
  return (
    <Notice
      tone="danger"
      role="alert"
      title={title ?? view.title}
      actions={
        (actions || toSettings) && (
          <>
            {actions}
            {toSettings && (
              <Link className={buttonClass('default', true)} to="/settings">
                {zh.errors.goSettings}
              </Link>
            )}
          </>
        )
      }
    >
      <div>{view.message}</div>
      {view.hint && (
        <div className={s.noticeHint}>
          {zh.errors.hint}：{view.hint}
        </div>
      )}
    </Notice>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  return (
    <div className={s.state}>
      <ErrorNotice
        error={error}
        title={zh.app.loadFailed}
        actions={
          onRetry && (
            <Button small onClick={onRetry}>
              {zh.app.retry}
            </Button>
          )
        }
      />
    </div>
  );
}

export function Field({
  label,
  hint,
  htmlFor,
  children,
}: {
  label: string;
  hint?: ReactNode;
  htmlFor: string;
  children: ReactNode;
}) {
  return (
    <div className={s.field}>
      <label className={s.fieldLabel} htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {hint && <div className={s.fieldHint}>{hint}</div>}
    </div>
  );
}

export const inputClass = s.input!;

async function copyText(text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    // Clipboard API is unavailable on insecure origins and some WebViews.
    try {
      const ta = document.createElement('textarea');
      ta.value = text;
      ta.style.position = 'fixed';
      ta.style.opacity = '0';
      document.body.appendChild(ta);
      ta.select();
      const ok = document.execCommand('copy');
      ta.remove();
      return ok;
    } catch {
      return false;
    }
  }
}

export function CopyButton({ text, className }: { text: string; className?: string }) {
  const [state, setState] = useState<'idle' | 'ok' | 'fail'>('idle');
  useEffect(() => {
    if (state === 'idle') return;
    const t = setTimeout(() => setState('idle'), 1800);
    return () => clearTimeout(t);
  }, [state]);
  return (
    <Button
      small
      variant="accent"
      className={className}
      onClick={() => void copyText(text).then((ok) => setState(ok ? 'ok' : 'fail'))}
    >
      <span aria-live="polite">
        {state === 'ok' ? zh.app.copied : state === 'fail' ? zh.app.copyFailed : zh.app.copy}
      </span>
    </Button>
  );
}

export function CodeBlock({ code }: { code: string }) {
  return (
    <div className={s.code}>
      <pre tabIndex={0}>
        <code>{code}</code>
      </pre>
      <CopyButton text={code} className={s.codeCopy} />
    </div>
  );
}

/**
 * Modal built on the native <dialog>: showModal() gives a real focus trap,
 * Escape handling and an inert background without extra code.
 */
export function Dialog({
  open,
  title,
  onClose,
  children,
}: {
  open: boolean;
  title: string;
  onClose: () => void;
  children: ReactNode;
}) {
  const ref = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);
  return (
    <dialog
      ref={ref}
      className={s.dialog}
      aria-labelledby="dialog-title"
      onClose={onClose}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
      onClick={(e) => {
        if (e.target === ref.current) onClose(); // click on the backdrop
      }}
    >
      {open && (
        <>
          <div className={s.dialogHeader}>
            <h2 id="dialog-title" className={s.dialogTitle}>
              {title}
            </h2>
            <Button variant="ghost" small onClick={onClose} aria-label={zh.app.close}>
              ✕
            </Button>
          </div>
          <div className={s.dialogBody}>{children}</div>
        </>
      )}
    </dialog>
  );
}
