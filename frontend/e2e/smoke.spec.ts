import { expect, test, type Page } from '@playwright/test';
import { PORTS } from '../playwright.config';

const SHOTS = '../.devdata/tmp/frontend/shots';
const READY_TITLE = '深度学习优化方法：从 SGD 到 AdamW';
const NO_MEDIA_TITLE = '线性代数复习：特征值与特征向量';

async function openVideo(page: Page, title: string) {
  await page.goto('/');
  await page.getByRole('searchbox').fill(title.slice(0, 6));
  await page.getByRole('link', { name: title }).click();
  await expect(page.getByRole('heading', { level: 1, name: title }).first()).toBeVisible();
}

test('library lists videos, filters by title and paginates', async ({ page }) => {
  await page.goto('/');
  await expect(page.getByRole('heading', { name: '资料库' })).toBeVisible();
  await expect(page.locator('tbody tr')).toHaveCount(30);
  await page.getByRole('button', { name: '加载更多' }).click();
  await expect(page.locator('tbody tr')).toHaveCount(38);
  await expect(page.getByRole('button', { name: '加载更多' })).toHaveCount(0);
  await page.getByRole('searchbox').fill('线性代数');
  await expect(page.locator('tbody tr')).toHaveCount(1);
  await page.screenshot({ path: `${SHOTS}/library.png` });
});

test('note citation seeks the player and highlights the transcript segment', async ({ page }) => {
  await openVideo(page, READY_TITLE);
  const note = page.getByTestId('note');
  // The note was read through several offset pages (mock --page-chars 1500).
  await expect(note.getByRole('heading', { name: '术语' })).toBeVisible();
  await expect(note).not.toContainText('watchink:'); // front matter stripped
  expect(await page.evaluate(() => (window as unknown as { __xss?: boolean }).__xss)).toBeUndefined();
  await expect(note.locator('script')).toHaveCount(0);
  await expect(note.getByRole('link', { name: 'Adam 论文' })).toBeVisible();
  await expect(note.getByRole('link', { name: '不安全链接' })).toHaveCount(0);
  await expect(page.locator('video')).toHaveJSProperty('readyState', 4, { timeout: 15_000 }).catch(() => undefined);

  const cite = note.locator('button[data-segment-id]').nth(4);
  const segmentId = await cite.getAttribute('data-segment-id');
  await cite.hover();
  await expect(page.getByRole('tooltip')).toContainText(/[一-龥]/);
  await page.screenshot({ path: `${SHOTS}/note-hover.png` });
  await cite.click();

  await expect(page.getByRole('tab', { name: '字幕' })).toHaveAttribute('aria-selected', 'true');
  const row = page.getByTestId('transcript').locator(`[data-segment-id="${segmentId}"]`);
  await expect(row).toBeVisible();
  await expect(row).toHaveAttribute('data-cited', 'true');
  await expect(row).toHaveAttribute('data-current', 'true');
  await expect(row).toBeInViewport();
  // The real media element moved to the cited time.
  await expect.poll(() => page.locator('video').evaluate((v: HTMLVideoElement) => v.currentTime)).toBeGreaterThan(60);
  await page.screenshot({ path: `${SHOTS}/transcript-cited.png` });

  // Clicking another transcript row seeks too, and the highlight follows.
  const other = page.getByTestId('transcript').locator('button[data-segment-id]').first();
  const before = await page.locator('video').evaluate((v: HTMLVideoElement) => v.currentTime);
  await other.click();
  await expect(other).toHaveAttribute('data-current', 'true');
  await expect.poll(() => page.locator('video').evaluate((v: HTMLVideoElement) => v.currentTime)).not.toBe(before);

  // The note keeps its place when coming back.
  await page.getByRole('tab', { name: '笔记' }).click();
  await expect(cite).toBeInViewport();

  await page.getByRole('tab', { name: '信息' }).click();
  await expect(page.getByText('丢弃的引用')).toBeVisible();
  await expect(page.getByText('note-v0.1')).toBeVisible();
});

test('deep link with ?t= survives a refresh and positions the transcript', async ({ page }) => {
  await openVideo(page, READY_TITLE);
  const url = new URL(page.url());
  await page.goto(`${url.pathname}?t=1800000`);
  await page.reload();
  await page.getByRole('tab', { name: '字幕' }).click();
  const current = page.getByTestId('transcript').locator('[data-current="true"]');
  await expect(current).toBeVisible();
  await expect(current).toContainText(/0:(29|30):\d\d/);
  await expect.poll(() => page.locator('video').evaluate((v: HTMLVideoElement) => Math.round(v.currentTime))).toBe(1800);
});

test('video without a playback file explains itself and citations still work', async ({ page }) => {
  await openVideo(page, NO_MEDIA_TITLE);
  await expect(page.getByText('没有可播放的文件')).toBeVisible();
  await expect(page.locator('video')).toHaveCount(0);
  await page.getByTestId('note').locator('button[data-segment-id]').first().click();
  await expect(page.getByTestId('transcript').locator('[data-cited="true"]')).toBeVisible();
});

test('export downloads a file in web mode', async ({ page }) => {
  await openVideo(page, READY_TITLE);
  await page.getByRole('button', { name: /导出/ }).click();
  const download = page.waitForEvent('download');
  await page.getByRole('menuitem', { name: /SRT/ }).click();
  expect((await download).suggestedFilename()).toMatch(/\.srt$/);
});

test('importing a link opens the video with live stage progress; jobs page follows it', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: '导入视频' }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.getByRole('tab', { name: '视频链接' }).click();
  await dialog.getByRole('textbox', { name: '视频链接' }).fill(`https://www.bilibili.com/video/BV1e2e${Date.now()}`);
  await dialog.getByLabel('视频语言').selectOption('zh');
  await dialog.getByRole('button', { name: '开始导入' }).click();

  await expect(page).toHaveURL(/\/videos\/vid_/);
  const panel = page.getByTestId('job-panel');
  await expect(panel).toBeVisible();
  // Stages advance from the event stream alone (no polling while it is open).
  await expect(panel.locator('[data-stage="acquire"][data-status="succeeded"]')).toBeVisible({ timeout: 15_000 });
  await expect(page.getByText('正在准备播放文件')).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/processing.png` });

  await page.getByRole('navigation').getByRole('link', { name: /^任务/ }).click();
  await expect(page.getByRole('heading', { name: '任务' })).toBeVisible();
  const running = page.locator('li[data-status="running"]').first();
  await expect(running).toBeVisible();
  const bar = running.getByRole('progressbar', { name: '总进度' });
  const first = Number(await bar.getAttribute('aria-valuenow'));
  await expect.poll(async () => Number(await bar.getAttribute('aria-valuenow')), { timeout: 15_000 }).toBeGreaterThan(first);
  await expect(running.getByRole('button', { name: '取消任务' })).toBeEnabled();
  await expect(running.getByRole('button', { name: '重试' })).toBeDisabled();

  // A failed job shows message, hint, a link to settings, and can be retried.
  await page.getByRole('button', { name: '失败', exact: true }).click();
  const failed = page.locator('li[data-status="failed"]').first();
  await expect(failed).toContainText('在线模型鉴权失败（HTTP 401）');
  await expect(failed).toContainText('在设置中更新 API Key 后重试该任务');
  await expect(failed.getByRole('link', { name: '前往设置' })).toBeVisible();
  await expect(failed.getByRole('button', { name: '重试' })).toBeEnabled();
  await expect(failed.getByRole('button', { name: '取消任务' })).toBeDisabled();
  await expect(failed.getByText('复用')).toHaveCount(0);

  // Cards fold: finished jobs start collapsed, the rest open; the title toggles.
  const failedToggle = failed.getByRole('button', { expanded: true });
  await failedToggle.click();
  await expect(failed.getByRole('progressbar', { name: '总进度' })).toBeHidden();
  await failed.getByRole('button', { expanded: false }).click();
  await expect(failed.getByRole('progressbar', { name: '总进度' })).toBeVisible();
  await page.getByRole('button', { name: '已完成', exact: true }).click();
  const done = page.locator('li[data-status="succeeded"]').first();
  await expect(done.getByRole('progressbar', { name: '总进度' })).toBeHidden();
  await done.getByRole('button', { expanded: false }).click();
  await expect(done.getByRole('progressbar', { name: '总进度' })).toBeVisible();
  await page.getByRole('button', { name: '失败', exact: true }).click();
  await page.screenshot({ path: `${SHOTS}/jobs.png`, fullPage: true });
});

test('uploading a file reports progress and starts a job', async ({ page }) => {
  await page.goto('/');
  await page.getByRole('button', { name: '导入视频' }).first().click();
  const dialog = page.getByRole('dialog');
  await dialog.locator('input[type=file]').setInputFiles({
    name: `讲座录像-${Date.now()}.mp4`,
    mimeType: 'video/mp4',
    buffer: Buffer.alloc(256 * 1024, 1),
  });
  await expect(dialog.getByText(/讲座录像.*256 KB/)).toBeVisible();
  await dialog.getByRole('button', { name: '开始导入' }).click();
  await expect(page).toHaveURL(/\/videos\/vid_/);
  await expect(page.getByRole('heading', { level: 1 })).toContainText('讲座录像');
});

test('settings loads every section and can save and test the model', async ({ page }) => {
  await page.goto('/settings');
  await expect(page.getByRole('heading', { name: '在线模型' })).toBeVisible();
  await expect(page.getByTestId('key-status')).toHaveText(/已配置 ····4f2a/);
  await expect(page.getByText('字幕文本、视频标题和时长')).toBeVisible();
  await expect(page.locator('tr[data-tool="ffmpeg"]')).toHaveAttribute('data-status', 'ready');
  await expect(page.getByTestId('data-dir')).toContainText('Watchink');
  // Only the content pane scrolls: nothing inside it may make the document
  // itself scrollable, or the whole shell shifts when the pane hits bottom.
  expect(await page.evaluate(() => document.documentElement.scrollHeight - window.innerHeight)).toBe(0);
  await expect(page.getByText('watchink config set-data-dir <新路径>')).toBeVisible();
  // Agent setup is one prompt to paste into the agent, plus usage examples.
  const agent = page.locator('[data-section="agent"]');
  await expect(agent.locator('pre')).toContainText('- 命令：watchink');
  await expect(agent.locator('pre')).toContainText('- 参数：mcp');
  await expect(agent.getByRole('button', { name: '复制' })).toHaveCount(1);
  await expect(agent.getByRole('listitem').first()).toContainText('导入 Watchink');

  // The optional browser UI is a switch with its address.
  const web = page.locator('[data-section="web"]');
  const sw = web.getByRole('switch', { name: /允许在浏览器中使用/ });
  await expect(sw).toBeChecked();
  await expect(web.getByTestId('web-address')).toContainText('http://127.0.0.1');
  // The switch reflects the saved setting, so it flips once the save returns.
  await sw.click();
  await expect(page.getByText('已关闭 Web 界面')).toBeVisible();
  await expect(sw).not.toBeChecked();
  await expect(web.getByTestId('web-address')).toHaveCount(0);
  await sw.click();
  await expect(sw).toBeChecked();
  await expect(web.getByTestId('web-address')).toBeVisible();
  await expect(page.getByTestId('engine-version')).toHaveText('0.1.0-mock');

  await page.getByLabel('服务商预设').selectOption('siliconflow');
  await expect(page.getByLabel('接口地址（base_url）')).toHaveValue('https://api.siliconflow.cn/v1');
  // The model is chosen from the list the endpoint offers…
  await expect(page.getByLabel('模型名').locator('option', { hasText: 'mock-mini' })).toBeAttached();
  await page.getByLabel('模型名').selectOption('mock-mini');
  // …and can still be typed by hand for a model the list does not show.
  await page.getByLabel('模型名').selectOption({ label: '手动输入模型名…' });
  await page.getByLabel('模型名').fill('Qwen/Qwen2.5-72B-Instruct');
  await page.getByRole('button', { name: '保存并测试连接' }).click();
  await expect(page.getByText(/连接正常（\d+ ms，结构化输出：json_schema）/)).toBeVisible();
  await page.reload();
  await expect(page.getByLabel('模型名')).toHaveValue('Qwen/Qwen2.5-72B-Instruct');
  await expect(page.getByLabel('API Key')).toHaveValue(''); // never echoed back

  // A lazy item downloads with live progress from tools.updated events.
  const deno = page.locator('tr[data-tool="deno"]');
  await deno.getByRole('button', { name: '下载' }).click();
  await expect(deno).toHaveAttribute('data-status', 'downloading');
  await expect(deno).toHaveAttribute('data-status', 'ready', { timeout: 15_000 });
  await page.screenshot({ path: `${SHOTS}/settings.png`, fullPage: true });
});

test('unknown routes show a not-found page', async ({ page }) => {
  await page.goto('/nope/nothing');
  await expect(page.getByRole('heading', { name: '页面不存在' })).toBeVisible();
});

test('a browser sees a notice, not a login form, while the Web UI is off', async ({ page }) => {
  await page.goto(`http://127.0.0.1:${PORTS.unauthorized}/`);
  await expect(page.getByRole('heading', { name: 'Web 界面没有开启' })).toBeVisible();
  await expect(page.getByText('watchink web', { exact: true })).toBeVisible();
  await expect(page.locator('input')).toHaveCount(0);
});

test('first run redirects to the wizard, which prepares tools and can be dismissed', async ({ page }) => {
  const base = `http://127.0.0.1:${PORTS.fresh}`;
  await page.goto(`${base}/`);
  await expect(page).toHaveURL(`${base}/setup`);
  await expect(page.getByRole('heading', { name: '首次配置' })).toBeVisible();
  await expect(page.getByText(/共需下载约 6\d\d(\.\d)? MB/)).toBeVisible();
  await page.screenshot({ path: `${SHOTS}/setup.png`, fullPage: true });
  await page.getByRole('button', { name: /下载缺失的必需项/ }).click();
  await expect(page.getByText('必需的工具和模型都已就绪。')).toBeVisible({ timeout: 15_000 });

  await page.getByRole('button', { name: '下一步' }).click();
  await expect(page.getByText('字幕文本、视频标题和时长')).toBeVisible();
  await page.getByLabel('服务商预设').selectOption('deepseek');
  await expect(page.getByLabel('模型名').locator('option', { hasText: 'mock-reasoner' })).toBeAttached();
  await page.getByLabel('模型名').selectOption('mock-reasoner');
  await page.getByLabel('API Key').fill('sk-test-12345678');
  await page.getByRole('button', { name: '保存并测试连接' }).click();
  await expect(page.getByText(/连接正常/)).toBeVisible();
  await expect(page.getByTestId('key-status')).toHaveText(/已配置 ····5678/);

  await page.getByRole('button', { name: '下一步' }).click();
  await expect(page.getByText('工具和在线模型都已就绪。')).toBeVisible();
  await page.getByRole('button', { name: '进入资料库' }).click();
  await expect(page).toHaveURL(`${base}/`);
  await page.reload();
  await expect(page).toHaveURL(`${base}/`); // dismissed: no redirect loop
});

test('dark scheme and a narrow window stay usable', async ({ browser }) => {
  const context = await browser.newContext({ colorScheme: 'dark', viewport: { width: 420, height: 800 } });
  const page = await context.newPage();
  await openVideo(page, READY_TITLE);
  await expect(page.getByTestId('note').getByRole('heading', { name: '概览' })).toBeVisible();
  // No horizontal overflow of the page at phone width.
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
  await page.screenshot({ path: `${SHOTS}/narrow-dark.png` });
  await context.close();
});
