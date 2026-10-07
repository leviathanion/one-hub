import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

test('模型归属可设置与清空，模型价格使用精确模型的组合信息', { skip: !process.env.PRICING_UI_BROWSER, timeout: 120000 }, async () => {
  const { build, preview } = await import('vite');
  const { chromium } = await import('playwright-core');
  const { default: config } = await import('../../../../vite.config.mjs');
  const root = fileURLToPath(new URL('../../../../', import.meta.url));
  const outDir = await mkdtemp(path.join(tmpdir(), 'model-catalog-browser-'));
  let server;
  let browser;
  try {
    const buildConfig = {
      ...config,
      configFile: false,
      root,
      publicDir: false,
      logLevel: 'error',
      build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/model-catalog.html') } }
    };
    await build(buildConfig);
    server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0, open: false } });
    browser = await chromium.launch({ executablePath: process.env.PRICING_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
    const page = await browser.newPage({ viewport: { width: 1280, height: 1100 } });
    page.setDefaultTimeout(5000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    let info = {
      id: 7,
      model: 'catalog-v1',
      name: '',
      description: '精确模型说明',
      owned_by_id: null,
      context_length: 128000,
      max_tokens: 4096,
      input_modalities: '["text"]',
      output_modalities: '["text"]',
      tags: '["精确标签"]'
    };
    let infoReads = 0;
    const writes = [];
    await page.route('**/api/**', async (route) => {
      const url = new URL(route.request().url());
      let data;
      if (url.pathname === '/api/ownedby') data = { 1: { name: 'OpenAI' }, 1001: { name: '自定义归属' } };
      else if (url.pathname === '/api/prices/model_list') data = ['catalog-v1'];
      else if (url.pathname === '/api/model_info/7') data = info;
      else if (url.pathname === '/api/model_info/') {
        if (route.request().method() === 'PUT') {
          const body = route.request().postDataJSON();
          writes.push(body);
          info = { ...info, ...body };
          data = info;
        } else {
          infoReads++;
          data = [info];
        }
      } else if (url.pathname === '/api/user_group_map') data = { default: { name: '默认', ratio: 1 } };
      else if (url.pathname === '/api/available_model')
        data = {
          'catalog-v1': {
            owned_by: 'unknown',
            groups: ['default'],
            price: {
              model: 'catalog-*',
              type: 'tokens',
              input: 1,
              output: 2,
              model_info: { ...info, input_modalities: ['text'], output_modalities: ['text'], tags: ['精确标签'] }
            }
          }
        };
      else throw new Error(`Unexpected request: ${url.pathname}`);
      await route.fulfill({ json: { success: true, data } });
    });
    const baseUrl = server.resolvedUrls.local[0];
    await page.goto(`${baseUrl}tests/fixtures/model-catalog.html`);
    const row = () => page.getByRole('row').filter({ hasText: 'catalog-v1' });
    const edit = async () => {
      await row().getByRole('button').click();
      await page.getByRole('menuitem', { name: '编辑', exact: true }).click();
      await page.getByRole('dialog').waitFor();
    };
    await edit();
    await page.getByRole('combobox', { name: '模型归属' }).click();
    await page.getByRole('option', { name: '自定义归属', exact: true }).click();
    await page.getByRole('button', { name: '提交', exact: true }).click();
    await page.getByRole('dialog').waitFor({ state: 'hidden' });
    await row().getByText('自定义归属', { exact: true }).waitFor();
    assert.equal(writes[0].owned_by_id, 1001);
    assert.equal(writes[0].name, '', '仅归属目录无需补造名称即可保存');
    assert.equal(Object.hasOwn(writes[0], 'channel_type'), false);
    await edit();
    await page.getByRole('combobox', { name: '模型归属' }).click();
    await page.getByRole('option', { name: '未设置', exact: true }).click();
    await page.getByRole('button', { name: '提交', exact: true }).click();
    await page.getByRole('dialog').waitFor({ state: 'hidden' });
    await row().getByText('未设置', { exact: true }).waitFor();
    assert.equal(writes[1].owned_by_id, null);
    await page.route('**/catalog', (route) =>
      route.fulfill({
        json: {
          data: [
            {
              model: info.model,
              model_info: {
                ...info,
                owned_by_id: 1,
                input_modalities: ['text'],
                output_modalities: ['text'],
                tags: ['精确标签']
              }
            }
          ]
        }
      })
    );
    await page.getByRole('button', { name: '批量导入', exact: true }).click();
    await page.getByRole('textbox', { name: '数据源地址' }).fill('/catalog');
    await page.getByRole('button', { name: '获取数据', exact: true }).click();
    await page.getByText('覆盖已存在的', { exact: true }).click();
    await page.getByRole('button', { name: '开始导入', exact: true }).click();
    await page.getByRole('dialog').waitFor({ state: 'hidden' });
    await row().getByText('OpenAI', { exact: true }).waitFor();
    assert.equal(writes[2].id, 7, '覆盖目录必须更新已有记录');
    assert.equal(writes[2].owned_by_id, 1);
    assert.equal(writes[2].name, '', '导入保留显式空名称');
    const readsBeforePrices = infoReads;
    await page.goto(`${baseUrl}tests/fixtures/model-catalog.html?price`);
    await page.getByText('精确模型说明', { exact: true }).waitFor();
    await page.getByText('精确标签', { exact: true }).first().waitFor();
    assert.equal(infoReads, readsBeforePrices, '模型价格应使用组合响应，无需重新请求目录');
    assert.deepEqual(errors, []);
  } finally {
    await browser?.close();
    await new Promise((resolve) => (server ? server.httpServer.close(resolve) : resolve()));
    await rm(outDir, { recursive: true, force: true });
  }
});
