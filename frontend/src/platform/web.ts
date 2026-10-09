import type { Platform } from './types';

const connection = {
  baseUrl: '',
  headers: {},
  // Cookie sessions must prove the request came from our own page (§15).
  mutationHeaders: { 'X-WK-Request': '1' },
  credentials: 'same-origin' as RequestCredentials,
};

/** Browser tab served by the engine: same origin, HttpOnly cookie session. */
export function createWebPlatform(): Platform {
  return {
    mode: 'web',
    requestedBy: 'web',
    router: 'browser',
    canReconnect: false,
    connect: () => Promise.resolve(connection),
    localFile: { kind: 'upload' },
    saveExport(exp, absoluteDownloadUrl) {
      const a = document.createElement('a');
      a.href = absoluteDownloadUrl;
      a.download = exp.filename;
      a.rel = 'noopener';
      document.body.appendChild(a);
      a.click();
      a.remove();
      return Promise.resolve({ saved: true, path: null });
    },
    openExternal(url) {
      if (/^https?:\/\//i.test(url)) window.open(url, '_blank', 'noopener,noreferrer');
    },
    revealInFileManager: null,
    agentSetup: () => Promise.resolve(null),
  };
}
