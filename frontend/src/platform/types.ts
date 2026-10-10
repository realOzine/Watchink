import type { Export } from '../api/types';

/** How to reach the engine right now. */
export interface Connection {
  /** '' for same-origin (web); absolute `http://127.0.0.1:<port>` on desktop. */
  baseUrl: string;
  /** Sent with every request. */
  headers: Record<string, string>;
  /** Added to state-changing requests only (cookie CSRF guard). */
  mutationHeaders: Record<string, string>;
  credentials: RequestCredentials;
}

export interface AgentSnippet {
  name: string;
  language: string;
  content: string;
}

export interface AgentSetup {
  executable: string;
  mcp_command: string[];
  snippets: AgentSnippet[];
}

export type LocalFileSource =
  /** Native file dialog; the engine reads the path itself. */
  | { kind: 'pick'; pick(): Promise<string | null> }
  /** Browser upload of the file bytes. */
  | { kind: 'upload' };

export interface SaveResult {
  /** false when the user cancelled the save dialog. */
  saved: boolean;
  /** Where the file went, when the platform knows. */
  path: string | null;
}

/**
 * Everything that differs between the desktop shell and a browser tab
 *. Pages use this interface and never test the mode.
 */
export interface Platform {
  readonly mode: 'web' | 'desktop';
  /** Value for `requested_by` on imports. */
  readonly requestedBy: 'web' | 'desktop';
  /** `browser` = History API paths; `hash` = `#/path` (no server fallback needed). */
  readonly router: 'browser' | 'hash';
  /** True when `connect({refresh:true})` can restart the engine and mint a new token. */
  readonly canReconnect: boolean;
  connect(opts?: { refresh?: boolean }): Promise<Connection>;
  readonly localFile: LocalFileSource;
  saveExport(exp: Export, absoluteDownloadUrl: string): Promise<SaveResult>;
  openExternal(url: string): void;
  /** null when the platform cannot reveal files. */
  readonly revealInFileManager: ((path: string) => Promise<void>) | null;
  /** null when only the generic instructions apply. */
  agentSetup(): Promise<AgentSetup | null>;
}
