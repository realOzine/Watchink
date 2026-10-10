import { defineConfig, devices } from '@playwright/test';

// Smoke tests run the production build (internal/webui/dist) against the mock
// engine, which serves the SPA with history fallback exactly like the engine.
const mock = (port: number, extra = '') => ({
  command: `node mock/server.mjs --port ${port} --dist ../internal/webui/dist --page-chars 1500 ${extra}`,
  url: `http://127.0.0.1:${port}/api/v1/health`,
  reuseExistingServer: false,
});

export const PORTS = { main: 47991, unauthorized: 47992, fresh: 47993 };

export default defineConfig({
  testDir: './e2e',
  outputDir: '../.devdata/tmp/frontend/test-results',
  fullyParallel: false,
  workers: 1,
  timeout: 45_000,
  reporter: [['list']],
  use: { baseURL: `http://127.0.0.1:${PORTS.main}`, trace: 'retain-on-failure' },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: [mock(PORTS.main), mock(PORTS.unauthorized, '--require-auth'), mock(PORTS.fresh, '--fresh --speed 4')],
});
