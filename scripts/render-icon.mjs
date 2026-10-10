// Renders build/appicon.svg to build/appicon.png (1024x1024, transparent
// outside the tile) with the Chromium that Playwright installed for the
// frontend tests. Run from the repository root:
//
//   node scripts/render-icon.mjs && make icon
import { createRequire } from 'node:module';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

process.env.PLAYWRIGHT_BROWSERS_PATH ??= '0';
const require = createRequire(resolve('frontend/package.json'));
const { chromium } = require('@playwright/test');

const svg = readFileSync('build/appicon.svg', 'utf8');
const out = process.argv[2] ?? 'build/appicon.png';
const browser = await chromium.launch();
const page = await browser.newPage({ viewport: { width: 1024, height: 1024 }, deviceScaleFactor: 1 });
await page.setContent(`<style>html,body{margin:0;background:transparent}svg{display:block}</style>${svg}`);
await page.screenshot({ path: out, omitBackground: true });
await browser.close();
console.log('wrote', out);
