import { spawn, type ChildProcess } from 'node:child_process';
import { mkdirSync, writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import react from '@vitejs/plugin-react';
import { defineConfig, type Plugin } from 'vite';

const outDir = resolve(import.meta.dirname, '../internal/webui/dist');
const MOCK_PORT = 47990;
const ENGINE_URL = process.env.WK_ENGINE_URL ?? 'http://127.0.0.1:47823';

/** internal/webui embeds dist/ with go:embed, so the directory must never be empty in git. */
function keepGitkeep(): Plugin {
  return {
    name: 'lt-keep-gitkeep',
    apply: 'build',
    closeBundle() {
      mkdirSync(outDir, { recursive: true });
      writeFileSync(resolve(outDir, '.gitkeep'), '');
    },
  };
}

/** `vite --mode mock` starts mock/server.mjs next to the dev server. */
function mockEngine(): Plugin {
  let child: ChildProcess | undefined;
  return {
    name: 'lt-mock-engine',
    apply: 'serve',
    configureServer(server) {
      child = spawn(process.execPath, ['mock/server.mjs', '--port', String(MOCK_PORT)], {
        cwd: import.meta.dirname,
        stdio: 'inherit',
      });
      server.httpServer?.once('close', () => child?.kill());
      process.once('exit', () => child?.kill());
      // 'exit' does not fire when the dev server is killed by a signal.
      for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP'] as const) {
        process.once(sig, () => {
          child?.kill();
          process.exit(0);
        });
      }
    },
  };
}

export default defineConfig(({ mode }) => {
  const target = mode === 'mock' ? `http://127.0.0.1:${MOCK_PORT}` : ENGINE_URL;
  return {
    // Absolute base: the engine serves the SPA at `/` with history fallback and
    // the desktop shell serves it at the root of its own origin, so `/assets/*`
    // resolves in both, also after a deep-link refresh.
    base: '/',
    plugins: [react(), keepGitkeep(), ...(mode === 'mock' ? [mockEngine()] : [])],
    build: {
      outDir,
      emptyOutDir: true,
      assetsDir: 'assets',
      sourcemap: false,
      target: 'es2022',
      // One local bundle, loaded from disk or loopback: splitting buys nothing here.
      chunkSizeWarningLimit: 800,
    },
    server: {
      port: 5173,
      strictPort: false,
      proxy: {
        '/api': { target, changeOrigin: true },
        '/media': { target, changeOrigin: true },
        '/auth': { target, changeOrigin: true },
      },
    },
    test: {
      include: ['src/**/*.test.ts'],
      environment: 'node',
    },
  };
});
