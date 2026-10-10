import { useEffect, useRef, type ReactNode } from 'react';
import { Link, NavLink, Outlet, useLocation, useNavigate } from 'react-router';
import { unauthorizedStore } from '../api/client';
import { streamStatusStore } from '../api/events';
import { useEngine } from '../api/hooks';
import { zh } from '../i18n/zh';
import { safeStorage, useStore } from '../lib/store';
import { dismissToast, toastStore } from '../lib/toast';
import s from './AppShell.module.css';
import { Button, CodeBlock, buttonClass, cx } from './ui';

export const SETUP_DISMISSED_KEY = 'wk.setup.dismissed';

export function Page({ title, actions, children }: { title: string; actions?: ReactNode; children: ReactNode }) {
  return (
    <div className={s.page}>
      <header className={s.pageHeader}>
        <h1 className={s.pageTitle}>{title}</h1>
        {actions}
      </header>
      {children}
    </div>
  );
}

function Toasts() {
  const toasts = useStore(toastStore);
  return (
    <div className={s.toasts} role="region" aria-label="通知" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={cx(s.toast, s[`toast-${t.kind}`])} role={t.kind === 'error' ? 'alert' : 'status'}>
          <div className={s.toastText}>
            <div>{t.message}</div>
            {t.detail && <div className={s.toastDetail}>{t.detail}</div>}
            {t.action?.to && (
              <Link to={t.action.to} onClick={() => dismissToast(t.id)}>
                {t.action.label}
              </Link>
            )}
            {t.action?.onClick && (
              <Button small onClick={t.action.onClick}>
                {t.action.label}
              </Button>
            )}
          </div>
          <Button variant="ghost" small aria-label={zh.app.close} onClick={() => dismissToast(t.id)}>
            ✕
          </Button>
        </div>
      ))}
    </div>
  );
}

function StreamBanner() {
  const status = useStore(streamStatusStore);
  if (status === 'open' || status === 'connecting') return null;
  return (
    <div className={s.banner} role="status">
      {status === 'stopping' ? zh.banner.stopping : zh.banner.down}
    </div>
  );
}

/** The browser UI is optional (settings web.enabled); this is what a browser sees while it is off. */
export function UnauthorizedNotice() {
  return (
    <main className={s.fullNotice}>
      <h1>{zh.unauthorized.title}</h1>
      <p>{zh.unauthorized.body}</p>
      <CodeBlock code={zh.unauthorized.command} />
    </main>
  );
}

export function NotFound() {
  return (
    <main className={s.fullNotice}>
      <h1>{zh.app.notFoundTitle}</h1>
      <p>{zh.app.notFoundBody}</p>
      <Link className={buttonClass('primary')} to="/">
        {zh.app.backToLibrary}
      </Link>
    </main>
  );
}

/** Sends a first-time user to the wizard once, unless they already dismissed it. */
function useFirstRunRedirect() {
  const engine = useEngine();
  const navigate = useNavigate();
  const location = useLocation();
  const checked = useRef(false);
  useEffect(() => {
    if (checked.current || !engine.data) return;
    checked.current = true;
    const incomplete = !engine.data.tools_ready || !engine.data.llm_configured;
    if (incomplete && safeStorage.get(SETUP_DISMISSED_KEY) !== '1' && location.pathname === '/') {
      void navigate('/setup', { replace: true });
    }
  }, [engine.data, location.pathname, navigate]);
  return engine;
}

export function AppShell() {
  const unauthorized = useStore(unauthorizedStore);
  const engine = useFirstRunRedirect();
  const location = useLocation();
  const mainRef = useRef<HTMLElement>(null);

  // Route changes move focus to the content, so keyboard and screen-reader
  // users do not stay stranded on the navigation link they activated.
  const firstRender = useRef(true);
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    mainRef.current?.focus({ preventScroll: true });
    mainRef.current?.scrollTo({ top: 0 });
  }, [location.pathname]);

  if (unauthorized) return <UnauthorizedNotice />;

  const active = engine.data?.active_jobs ?? 0;
  const link = ({ isActive }: { isActive: boolean }) => cx(s.link, isActive && s.active);
  return (
    <div className={s.shell}>
      <a className={s.skip} href="#main">
        {zh.app.skipToContent}
      </a>
      <aside className={s.side}>
        <div className={s.brand}>
          <div className={s.brandName}>{zh.app.name}</div>
          <div className={s.brandTag}>{zh.app.tagline}</div>
        </div>
        <nav className={s.nav} aria-label="主导航">
          <NavLink to="/" end className={link}>
            {zh.app.nav.library}
          </NavLink>
          <NavLink to="/jobs" className={link}>
            {zh.app.nav.jobs}
            {active > 0 && (
              <span className={s.count} aria-label={`${active} 个进行中`}>
                {active}
              </span>
            )}
          </NavLink>
          <NavLink to="/settings" className={link}>
            {zh.app.nav.settings}
          </NavLink>
        </nav>
        <div className={s.spacer} />
        {engine.data && <div className={s.version}>v{engine.data.version}</div>}
      </aside>
      <div className={s.main}>
        <StreamBanner />
        <main id="main" ref={mainRef} className={s.content} tabIndex={-1}>
          <Outlet />
        </main>
      </div>
      <Toasts />
    </div>
  );
}
