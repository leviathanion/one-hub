import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// CHANNEL_UI_BROWSER=/path/to/chromium node --test src/views/Channel/component/ChannelModelsField.browser.test.mjs
test('生产构建中的渠道模型选择交互', { skip: !process.env.CHANNEL_UI_BROWSER, timeout: 120000 }, async (t) => {
  const { build, preview } = await import('vite');
  const { chromium } = await import('playwright-core');
  const { default: config } = await import('../../../../vite.config.mjs');
  const root = fileURLToPath(new URL('../../../../', import.meta.url));
  const outDir = await mkdtemp(path.join(tmpdir(), 'channel-models-browser-'));
  let server;
  let browser;
  try {
    const buildConfig = {
      ...config,
      configFile: false,
      root,
      publicDir: false,
      logLevel: 'error',
      build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/channel-models.html') } }
    };
    await build(buildConfig);
    server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0 } });
    browser = await chromium.launch({ executablePath: process.env.CHANNEL_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
    const page = await browser.newPage({ viewport: { width: 1440, height: 1000 } });
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    page.on('console', (message) => {
      if (/flushSync|React error|Invalid prop/.test(message.text())) errors.push(message.text());
    });
    const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/channel-models.html`;
    const input = page.locator('#channel-models-label');
    const ids = () => page.evaluate(() => window.formValues.models.map((model) => model.id));
    const activeIndex = () => input.getAttribute('aria-activedescendant').then((id) => Number(id?.split('-option-')[1] ?? -1));
    const ready = async (query = '') => {
      await page.goto(url + query);
      await page.waitForFunction(() => !!window.formValues);
    };

    await t.test('三千候选保留完整浏览，跨窗口键盘导航和首尾循环正确', async () => {
      await ready();
      await input.click();
      assert.ok((await page.locator('[role=option]').count()) <= 15);
      assert.equal(await page.locator('[role=option]').first().getAttribute('aria-setsize'), '3000');
      await input.press('End');
      assert.equal(await activeIndex(), 2999);
      await input.press('Enter');
      assert.deepEqual(await ids(), ['model-2999']);
      await input.press('Home');
      assert.equal(await activeIndex(), 0);
      await input.press('ArrowUp');
      assert.equal(await activeIndex(), 2999);
      await input.press('PageUp');
      assert.equal(await activeIndex(), 2994);
      await input.press('PageDown');
      assert.equal(await activeIndex(), 2999);
      await input.press('ArrowDown');
      assert.equal(await activeIndex(), 0);
      for (let i = 0; i < 25; i++) await input.press('ArrowDown');
      assert.equal(await activeIndex(), 25);
      const activeId = await input.getAttribute('aria-activedescendant');
      assert.ok(await page.locator(`#${activeId}`).isVisible());
      assert.ok((await page.locator('[role=option]').count()) <= 15);
      const list = page.locator('[role=listbox]');
      await list.evaluate((element) => {
        element.scrollTop = element.scrollHeight;
      });
      await page.locator('[data-option-index="2999"]').waitFor();
      assert.ok((await page.locator('[role=option]').count()) <= 15);
      // 滚出窗口的高亮行仍挂载，屏幕阅读器引用有效。
      assert.equal(await page.locator(`#${activeId}`).count(), 1);
      const positions = await page
        .locator('[role=option]')
        .evaluateAll((elements) => elements.map((element) => Number(element.getAttribute('aria-posinset'))));
      assert.deepEqual(
        positions,
        [...positions].sort((a, b) => a - b)
      );
      await list.evaluate((element) => {
        element.scrollTop = 1000 * 48;
      });
      await page.locator('[data-option-index="999"]').hover();
      await input.press('ArrowDown');
      assert.equal(await activeIndex(), 1000);
    });

    await t.test('筛选后正确重置窗口，目录对象与输入按 ID 去重，未知模型可以添加', async () => {
      await ready();
      await input.click();
      await input.press('End');
      await input.fill('model-0001');
      await page.waitForFunction(() => document.querySelectorAll('[role=option]').length === 1);
      await input.press('ArrowDown');
      await input.press('Enter');
      assert.deepEqual(await ids(), ['model-0001']);
      await input.fill('model-0001,new-model, new-model,中文模型,');
      assert.deepEqual(await ids(), ['model-0001', 'new-model', '中文模型']);
      await input.fill('future-model');
      await input.press('Enter');
      assert.deepEqual(await ids(), ['model-0001', 'new-model', '中文模型', 'future-model']);
      await input.fill('');
      assert.equal(await page.locator('[role=option]').first().getAttribute('aria-setsize'), '3000');
      await input.press('Escape');
      assert.equal(await page.locator('[role=listbox]').count(), 0);
    });

    await t.test('重新打开时挂载远端已选项，输入法确认不会提前添加模型', async () => {
      await ready('?selected=1&last=1');
      await input.click();
      await page.locator('[data-option-index="2999"]').waitFor();
      assert.equal(await activeIndex(), 2999);
      assert.equal(await page.locator('[data-option-index="2999"]').getAttribute('aria-selected'), 'true');
      await input.press('Escape');
      await input.click();
      await page.locator('[data-option-index="2999"]').waitFor();
      assert.equal(await activeIndex(), 2999);
      await input.press('ArrowUp');
      assert.equal(await activeIndex(), 2998);
      await input.press('ArrowDown');
      assert.equal(await activeIndex(), 2999);
      await input.fill('中文候选');
      await input.dispatchEvent('keydown', { key: 'Enter', code: 'Enter', isComposing: true });
      assert.deepEqual(await ids(), ['model-2999']);
      await input.press('Enter');
      assert.deepEqual(await ids(), ['model-2999', '中文候选']);
    });

    await t.test('连续删除后立即读取与保存得到最新集合，普通表单布局跳过模型更新', async () => {
      await ready('?n=900&selected=600');
      await input.click();
      const before = await page.evaluate(() => window.layoutRenders);
      for (let i = 0; i < 10; i++) await input.press('Backspace');
      assert.equal((await ids()).length, 590);
      assert.equal(await page.evaluate(() => window.layoutRenders), before);
      await input.press('Escape');
      await page.locator('#snapshot').click();
      assert.equal(await page.evaluate(() => window.snapshot.models.length), 590);
      await page.locator('#save').click();
      await page.waitForFunction(() => !!window.submitted);
      assert.equal(await page.evaluate(() => window.submitted.models.length), 590);
      assert.equal(await page.evaluate(() => window.submitted.models.at(-1).id), 'model-0589');
      await page.locator('#name').fill('更新后的名称');
      assert.equal(await page.evaluate(() => window.formValues.name), '更新后的名称');
      assert.ok((await page.evaluate(() => window.layoutRenders)) > before);
    });

    await t.test('禁用状态切换后不能删除，恢复后支持首项、中间项和末项删除', async () => {
      await ready('?selected=5');
      await page.locator('#toggle-disabled').click();
      assert.equal(await input.isDisabled(), true);
      assert.equal(await page.locator('.MuiChip-deleteIcon').count(), 0);
      await input.dispatchEvent('keydown', { key: 'Backspace', code: 'Backspace' });
      assert.equal((await ids()).length, 5);
      await page.locator('#toggle-disabled').click();
      assert.equal(await input.isEnabled(), true);
      await page.locator('[data-tag-index="0"] .MuiChip-deleteIcon').click();
      assert.deepEqual(await ids(), ['model-0001', 'model-0002', 'model-0003', 'model-0004']);
      await page.locator('[data-tag-index="1"] .MuiChip-deleteIcon').click();
      assert.deepEqual(await ids(), ['model-0001', 'model-0003', 'model-0004']);
      await input.focus();
      await input.press('ArrowLeft');
      await page.keyboard.press('Delete');
      assert.deepEqual(await ids(), ['model-0001', 'model-0003']);
      await input.focus();
      await input.press('Backspace');
      await input.press('Backspace');
      await page.getByText('至少选择一个模型').waitFor();
      assert.deepEqual(await ids(), []);
      await page.locator('#save').click();
      assert.equal(await page.evaluate(() => window.submitted), undefined);
    });
    assert.deepEqual(errors, []);
  } finally {
    await browser?.close();
    if (server) {
      server.httpServer.closeAllConnections();
      await new Promise((resolve) => server.httpServer.close(resolve));
    }
    await rm(outDir, { recursive: true, force: true });
  }
});
