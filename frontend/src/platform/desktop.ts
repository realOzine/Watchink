import type { AgentSetup, Connection, Platform } from './types';

/**
 * Methods bound by the Wails v2 shell at `window.go.desktop.App`.
 * Every binding returns a promise. This is the only file that touches
 * the Wails runtime.
 */
export interface DesktopBindings {
  /** Ensures the engine is running (re-spawning it if needed). */
  EngineInfo(): Promise<{ base_url: string; token: string; version: string }>;
  /** Absolute path, or '' when the dialog was cancelled. */
  PickVideoFile(): Promise<string>;
  /** Saved path, or '' when the dialog was cancelled. */
  SaveExport(export_id: string, suggested_name: string): Promise<string>;
  RevealInFileManager(path: string): Promise<void>;
  OpenExternal(url: string): Promise<void>;
  AgentSetupInfo(): Promise<AgentSetup>;
}

declare global {
  interface Window {
    go?: { desktop?: { App?: DesktopBindings } };
  }
}

export function desktopBindings(): DesktopBindings | null {
  return (typeof window !== 'undefined' && window.go?.desktop?.App) || null;
}

/** Desktop shell: cross-origin calls to the loopback engine with a bearer token. */
export function createDesktopPlatform(app: DesktopBindings): Platform {
  let current: Promise<Connection> | null = null;

  const load = (): Promise<Connection> =>
    app.EngineInfo().then((info) => ({
      baseUrl: info.base_url.replace(/\/+$/, ''),
      headers: { Authorization: `Bearer ${info.token}` },
      mutationHeaders: {},
      credentials: 'omit' as RequestCredentials,
    }));

  return {
    mode: 'desktop',
    requestedBy: 'desktop',
    // The embedded asset server has no history fallback; hash routes survive a reload.
    router: 'hash',
    canReconnect: true,
    connect(opts) {
      if (!current || opts?.refresh) {
        const attempt = load();
        current = attempt;
        // A failed lookup must not be cached, or the app could never recover.
        attempt.catch(() => {
          if (current === attempt) current = null;
        });
      }
      return current;
    },
    localFile: {
      kind: 'pick',
      pick: () => app.PickVideoFile().then((p) => p || null),
    },
    saveExport: (exp) =>
      app.SaveExport(exp.export_id, exp.filename).then((path) => ({ saved: path !== '', path: path || null })),
    openExternal(url) {
      if (/^https?:\/\//i.test(url)) void app.OpenExternal(url);
    },
    revealInFileManager: (path) => app.RevealInFileManager(path),
    agentSetup: () => app.AgentSetupInfo(),
  };
}
