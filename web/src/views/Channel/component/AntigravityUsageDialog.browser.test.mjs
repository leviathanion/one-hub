import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// CHANNEL_UI_BROWSER=/path/to/chromium node --test src/views/Channel/component/AntigravityUsageDialog.browser.test.mjs
test('反重力额度详情的真实组件交互', { skip: !process.env.CHANNEL_UI_BROWSER, timeout: 120000 }, async (t) => {
  const { build, preview } = await import('vite');
  const { chromium } = await import('playwright-core');
  const { default: config } = await import('../../../../vite.config.mjs');
  const root = fileURLToPath(new URL('../../../../', import.meta.url));
  const outDir = await mkdtemp(path.join(tmpdir(), 'antigravity-usage-browser-'));
  let server;
  let browser;
  try {
    const buildConfig = {
      ...config,
      configFile: false,
      root,
      publicDir: false,
      logLevel: 'error',
      build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/antigravity-usage.html') } }
    };
    await build(buildConfig);
    server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0, open: false } });
    browser = await chromium.launch({ executablePath: process.env.CHANNEL_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
    const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/antigravity-usage.html`;
    const success = (channelID, models) => ({ success: true, data: { channel_id: channelID, fetched_at: 1794000000, models } });

    await t.test('仅打开或刷新时查询，准确区分零、满额与未知，并展示动态模型', async () => {
      const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
      let calls = 0;
      await page.route('**/api/channel/*/antigravity/usage', async (route) => {
        calls++;
        await route.fulfill({
          json: success(7, [
            { model_id: 'gemini-new-version', display_name: '新模型', remaining_fraction: 0, reset_time: '2027-01-01T00:00:00Z' },
            { model_id: 'full-model', remaining_fraction: 1 },
            { model_id: 'unknown-model' },
            { model_id: 'null-model', remaining_fraction: null },
            { model_id: 'invalid-model', remaining_fraction: -1 },
            { model_id: 'overflow-model', remaining_fraction: 1.1 },
            { model_id: 'string-model', remaining_fraction: '0.5' }
          ])
        });
      });
      await page.goto(url);
      await page.locator('#open-7').waitFor();
      assert.equal(calls, 0);
      await page.locator('#open-7').focus();
      await page.keyboard.press('Enter');
      const row = (name) => page.getByRole('row').filter({ hasText: name });
      await row('gemini-new-version').waitFor();
      assert.equal(await row('gemini-new-version').locator('td').first().textContent(), '0%');
      assert.match(await row('gemini-new-version').locator('td').last().textContent(), /2027/);
      assert.equal(await row('full-model').locator('td').first().textContent(), '100%');
      for (const name of ['unknown-model', 'null-model', 'invalid-model', 'overflow-model', 'string-model']) {
        assert.deepEqual(await row(name).locator('td').allTextContents(), ['未知', '未知']);
      }
      assert.equal(calls, 1);
      if (process.env.ANTIGRAVITY_USAGE_SCREENSHOT) {
        await page.screenshot({ path: process.env.ANTIGRAVITY_USAGE_SCREENSHOT, fullPage: true });
      }
      await page.getByRole('button', { name: '刷新', exact: true }).click();
      await page.getByRole('button', { name: '刷新', exact: true }).waitFor({ state: 'visible' });
      await row('gemini-new-version').waitFor();
      assert.equal(calls, 2);
      await page.getByRole('button', { name: '关闭', exact: true }).click();
      await page.getByRole('dialog').waitFor({ state: 'hidden' });
      assert.equal(calls, 2);
      await page.close();
    });

    await t.test('刷新失败会清除旧额度并提供重新查询', async () => {
      const page = await browser.newPage();
      let calls = 0;
      await page.route('**/api/channel/7/antigravity/usage', async (route) => {
        calls++;
        await route.fulfill({
          json:
            calls === 2
              ? { success: false, message: '上游额度查询失败' }
              : success(7, [{ model_id: 'visible-model', remaining_fraction: 0.5 }])
        });
      });
      await page.goto(url);
      await page.locator('#open-7').click();
      await page.getByRole('row').filter({ hasText: 'visible-model' }).waitFor();
      await page.getByRole('button', { name: '刷新', exact: true }).click();
      await page.getByRole('alert').filter({ hasText: '上游额度查询失败' }).waitFor();
      assert.equal(await page.getByRole('table').count(), 0);
      if (process.env.ANTIGRAVITY_USAGE_ERROR_SCREENSHOT) {
        await page.screenshot({ path: process.env.ANTIGRAVITY_USAGE_ERROR_SCREENSHOT, fullPage: true });
      }
      await page.getByRole('button', { name: '刷新', exact: true }).click();
      await page.getByRole('row').filter({ hasText: 'visible-model' }).waitFor();
      assert.equal(calls, 3);
      await page.close();
    });

    await t.test('切换渠道和关闭弹窗使旧响应失效', async () => {
      const page = await browser.newPage();
      let releaseOld;
      const held = new Promise((resolve) => {
        releaseOld = resolve;
      });
      let oldRequested;
      const requested = new Promise((resolve) => {
        oldRequested = resolve;
      });
      await page.route('**/api/channel/*/antigravity/usage', async (route) => {
        if (route.request().url().includes('/7/')) {
          oldRequested();
          await held;
          await route.fulfill({ json: success(7, [{ model_id: 'old-channel-model', remaining_fraction: 0 }]) }).catch(() => {});
        } else {
          await route.fulfill({ json: success(8, [{ model_id: 'new-channel-model', remaining_fraction: 1 }]) });
        }
      });
      await page.goto(url);
      await page.locator('#open-7').click();
      await requested;
      // 模拟父级选择另一渠道；弹窗的 modal focus trap 不参与这项父级生命周期测试。
      await page.locator('#open-8').evaluate((element) => element.click());
      await page.getByRole('row').filter({ hasText: 'new-channel-model' }).waitFor();
      releaseOld();
      await page.getByRole('button', { name: '关闭', exact: true }).click();
      await page.getByRole('dialog').waitFor({ state: 'hidden' });
      assert.equal(await page.getByText('old-channel-model', { exact: true }).count(), 0);
      await page.locator('#open-8').click();
      await page.getByRole('row').filter({ hasText: 'new-channel-model' }).waitFor();
      assert.equal(await page.getByText('old-channel-model', { exact: true }).count(), 0);
      await page.close();
    });
  } finally {
    await browser?.close();
    if (server) {
      server.httpServer.closeAllConnections();
      await new Promise((resolve) => server.httpServer.close(resolve));
    }
    await rm(outDir, { recursive: true, force: true });
  }
});
