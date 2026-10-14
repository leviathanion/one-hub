import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

test('Antigravity 渠道授权、编辑与恢复', { skip: !process.env.CHANNEL_UI_BROWSER, timeout: 180000 }, async (t) => {
  const { build, preview } = await import('vite');
  const { chromium } = await import('playwright-core');
  const { default: config } = await import('../../../../vite.config.mjs');
  const root = fileURLToPath(new URL('../../../../', import.meta.url));
  const outDir = await mkdtemp(path.join(tmpdir(), 'antigravity-channel-browser-'));
  let server;
  let browser;
  try {
    const buildConfig = {
      ...config,
      configFile: false,
      root,
      publicDir: false,
      logLevel: 'error',
      build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/antigravity-channel.html') } }
    };
    await build(buildConfig);
    server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0, open: false } });
    browser = await chromium.launch({ executablePath: process.env.CHANNEL_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
    const page = await browser.newPage({ viewport: { width: 1440, height: 1200 } });
    page.setDefaultTimeout(10000);
    const pageErrors = [];
    page.on('pageerror', (error) => pageErrors.push(error.message));
    const requests = [];
    let operation = 'edit';
    let startVersion = 7;
    let startHandler;
    let exchangeHandler;
    const draftKey = '{\n  "access_token": "draft-token",\n  "project_id": "test-project"\n}';
    const originalKey = '{\n  "access_token": "old-token"\n}';
    const channel = {
      id: 42,
      version: 7,
      type: 102,
      name: '原名称',
      key: originalKey,
      base_url: '',
      proxy: '',
      other: '',
      test_model: '',
      models: 'gemini-test',
      group: 'default',
      model_mapping: '',
      model_headers: '',
      custom_parameter: '',
      plugin: {},
      tag: '',
      disabled_stream: [],
      pre_cost: 1,
      compatible_response: false,
      only_chat: false
    };
    await page.route('**/api/**', async (route) => {
      const request = route.request();
      const pathname = new URL(request.url()).pathname;
      const body = request.method() === 'GET' ? null : request.postDataJSON();
      requests.push({ pathname, method: request.method(), body });
      let data;
      if (pathname === '/api/channel/endpoints') data = [];
      else if (pathname === '/api/channel/provider_models_list') data = ['gemini-discovered'];
      else if (request.method() === 'GET') data = { ...channel, id: pathname.endsWith('/43') ? 43 : 42, versions: { 42: 7 } };
      else if (pathname.endsWith('/oauth/start')) {
        if (startHandler) return startHandler(route);
        data = {
          operation,
          expected_version: operation === 'create' ? 0 : startVersion,
          session_id: 'session-1',
          auth_url: 'https://accounts.google.com/test'
        };
      } else if (pathname.endsWith('/oauth/exchange-code')) {
        if (exchangeHandler) return exchangeHandler(route);
        data =
          operation === 'recover'
            ? { operation, channel_id: 42, version: 8, credential_saved: true }
            : {
                operation,
                channel_id: operation === 'create' ? 0 : 42,
                expected_version: operation === 'create' ? 0 : startVersion,
                key: draftKey
              };
      } else data = {};
      await route.fulfill({ json: { success: true, data } });
    });
    const baseURL = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/antigravity-channel.html`;
    const ready = async (mode = 'edit') => {
      requests.length = 0;
      startHandler = null;
      exchangeHandler = null;
      await page.goto(`${baseURL}?mode=${mode}`);
      if (mode === 'edit' || mode === 'tag-edit') await page.locator('#channel-key-label').waitFor();
    };
    const authorize = async () => {
      await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).click();
      await page.getByLabel('完整回调 URL').fill('http://localhost:51121/oauth-callback?code=code-1&state=state-1');
      await page.getByRole('button', { name: '提交回调 URL' }).click();
    };
    const exchanges = () => requests.filter((request) => request.pathname.endsWith('/oauth/exchange-code'));
    const save = async () => page.locator('form button[type=submit]').click();

    await t.test('新建选择 Antigravity 后使用完整 JSON 授权与现有模型发现', async () => {
      operation = 'create';
      await ready('create');
      await page.locator('#channel-type-label').click();
      await page.getByRole('option', { name: 'Antigravity OAuth', exact: true }).click();
      await page.locator('input[name=name]').fill('新建渠道');
      if (process.env.CHANNEL_UI_SCREENSHOT_DIR) {
        await page.getByRole('dialog').screenshot({
          path: path.join(process.env.CHANNEL_UI_SCREENSHOT_DIR, 'antigravity-channel-create.png'),
          animations: 'disabled'
        });
      }
      await authorize();
      await page.waitForFunction((key) => document.querySelector('#channel-key-label')?.value === key, draftKey);
      assert.equal(await page.locator('textarea#channel-key-label').count(), 1);
      await page.getByText('从 Antigravity 获取模型列表', { exact: true }).click();
      const selector = page.getByRole('dialog', { name: '模型选择器' });
      await selector.getByRole('button', { name: '获取模型', exact: true }).click();
      await selector.getByText('gemini-discovered', { exact: true }).waitFor();
      const discovery = requests.find((request) => request.pathname === '/api/channel/provider_models_list');
      assert.equal(discovery.body.type, 102);
      assert.equal(discovery.body.key, draftKey);
      await selector.getByRole('button', { name: '取消', exact: true }).click();
      await page.locator('#channel-models-label').fill('gemini-test');
      await page.locator('#channel-models-label').press('Enter');
      await save();
      await page.waitForFunction(() => !document.querySelector('form'));
      const create = requests.find((request) => request.pathname === '/api/channel/' && request.method === 'POST');
      assert.equal(create.body.type, 102);
      assert.equal(create.body.key, draftKey);
    });

    await t.test('普通授权形成草稿，取消保存保留表单和授权开始版本', async () => {
      operation = 'edit';
      startVersion = 7;
      await ready();
      await page.locator('input[name=name]').fill('未保存名称');
      await authorize();
      await page.waitForFunction((key) => document.querySelector('#channel-key-label')?.value === key, draftKey);
      assert.equal(await page.locator('input[name=name]').inputValue(), '未保存名称');
      assert.equal(requests.filter((request) => request.method === 'PUT').length, 0);
      await save();
      const confirmation = page.getByRole('dialog', { name: '确认保存渠道修改' });
      await confirmation.getByRole('button', { name: /取消/ }).click();
      assert.equal(await page.locator('#channel-key-label').inputValue(), draftKey);
      assert.equal(requests.filter((request) => request.method === 'PUT').length, 0);
      await save();
      await confirmation.getByRole('button', { name: '确认保存', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('form'));
      const update = requests.find((request) => request.method === 'PUT').body;
      assert.equal(update.expected_version, 7);
      assert.equal(update.key, draftKey);
      assert.equal(update.name, '未保存名称');
      assert.equal(exchanges().length, 1);
    });

    await t.test('恢复确认取消不交换，成功后保留编辑并防止旧凭据回写', async () => {
      operation = 'recover';
      await ready();
      await page.locator('input[name=name]').fill('恢复期间编辑');
      await authorize();
      const confirmation = page.getByRole('dialog', { name: '确认恢复渠道凭据' });
      if (process.env.CHANNEL_UI_SCREENSHOT_DIR) {
        await confirmation.screenshot({
          path: path.join(process.env.CHANNEL_UI_SCREENSHOT_DIR, 'antigravity-credential-recovery.png'),
          animations: 'disabled'
        });
      }
      await confirmation.getByRole('button', { name: '取消', exact: true }).click();
      assert.equal(exchanges().length, 0);
      assert.match(await page.getByLabel('完整回调 URL').inputValue(), /code=code-1/);
      await page.getByRole('button', { name: '提交回调 URL' }).click();
      await confirmation.getByRole('button', { name: '确认恢复', exact: true }).click();
      await page.getByLabel('完整回调 URL').waitFor({ state: 'hidden' });
      assert.equal(await page.locator('input[name=name]').inputValue(), '恢复期间编辑');
      await save();
      await page.getByRole('button', { name: '确认保存', exact: true }).click();
      await page.waitForFunction(() => !document.querySelector('form'));
      const update = requests.find((request) => request.method === 'PUT').body;
      assert.equal(update.expected_version, 8);
      assert.equal(Object.hasOwn(update, 'key'), false);
      assert.equal(update.name, '恢复期间编辑');
      assert.equal(exchanges().length, 1);
    });

    await t.test('授权开始发现并发配置变化时保留编辑，不采用较新版本', async () => {
      operation = 'edit';
      startVersion = 8;
      await ready();
      await page.locator('input[name=name]').fill('仍需核对的编辑');
      await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).click();
      await page.getByText(/渠道配置已变化，当前输入已保留/).waitFor();
      assert.equal(await page.getByLabel('完整回调 URL').count(), 0);
      assert.equal(await page.locator('input[name=name]').inputValue(), '仍需核对的编辑');
      assert.equal(exchanges().length, 0);
    });

    await t.test('关闭后重新打开表单不会接收旧授权开始结果', async () => {
      operation = 'edit';
      startVersion = 7;
      await ready();
      let releaseStart;
      startHandler = (route) =>
        new Promise((resolve) => {
          releaseStart = async () => {
            await route.fulfill({
              json: {
                success: true,
                data: {
                  operation: 'edit',
                  expected_version: 7,
                  session_id: 'closed-session',
                  auth_url: 'https://accounts.google.com/closed'
                }
              }
            });
            resolve();
          };
        });
      const started = page.waitForRequest('**/oauth/start');
      await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).click();
      await started;
      await page.locator('form').getByRole('button', { name: '取消', exact: true }).click();
      await page.locator('form').waitFor({ state: 'hidden' });
      await page.getByRole('button', { name: '重新打开测试表单' }).click();
      await page.locator('#channel-key-label').waitFor();
      await page.locator('input[name=name]').fill('重新打开后的编辑');
      const delivered = page.waitForResponse('**/oauth/start');
      await releaseStart();
      await (await delivered).finished();
      await page.locator('input[name=name]').press('Tab');
      assert.equal(await page.getByLabel('完整回调 URL').count(), 0);
      assert.equal(await page.locator('input[name=name]').inputValue(), '重新打开后的编辑');
      assert.equal(await page.locator('#channel-key-label').inputValue(), originalKey);
      assert.equal(exchanges().length, 0);
    });

    await t.test('切换渠道后不会把旧授权结果写入新渠道', async () => {
      operation = 'edit';
      await ready();
      let releaseExchange;
      exchangeHandler = (route) =>
        new Promise((resolve) => {
          releaseExchange = async () => {
            await route.fulfill({
              json: {
                success: true,
                data: {
                  operation: 'edit',
                  channel_id: 42,
                  expected_version: 7,
                  key: draftKey
                }
              }
            });
            resolve();
          };
        });
      const exchangeStarted = page.waitForRequest('**/oauth/exchange-code');
      await authorize();
      await exchangeStarted;
      // 模拟父页面在请求尚未返回时切换正在编辑的渠道。
      const nextChannel = page.waitForResponse('**/api/channel/43');
      await page.getByRole('button', { name: '切换测试渠道', includeHidden: true }).evaluate((button) => button.click());
      await page.getByLabel('完整回调 URL').waitFor({ state: 'hidden' });
      await nextChannel;
      await page.locator('input[name=name]').fill('新渠道编辑');
      const delivered = page.waitForResponse('**/oauth/exchange-code');
      await releaseExchange();
      await (await delivered).finished();
      await page.locator('input[name=name]').press('Tab');
      assert.equal(await page.locator('#channel-key-label').inputValue(), originalKey);
      assert.equal(await page.locator('input[name=name]').inputValue(), '新渠道编辑');
      assert.equal(await page.getByText('授权成功，凭据已填入表单，保存后生效。', { exact: true }).count(), 0);
      await save();
      await page.getByRole('button', { name: '确认保存', exact: true }).click();
      await page.locator('form').waitFor({ state: 'hidden' });
      const update = requests.find((request) => request.method === 'PUT').body;
      assert.equal(update.id, 43);
      assert.equal(update.expected_version, 7);
      assert.equal(Object.hasOwn(update, 'key'), false);
    });

    await t.test('授权失败保留表单，交换失败后必须重新开始会话', async () => {
      operation = 'edit';
      await ready();
      await page.locator('input[name=name]').fill('失败后保留的编辑');
      startHandler = (route) => route.fulfill({ status: 503, json: { success: false, message: '授权服务暂不可用' } });
      await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).click();
      await page.getByText('错误：授权服务暂不可用', { exact: true }).waitFor();
      assert.equal(await page.getByText('错误：授权服务暂不可用', { exact: true }).count(), 1);
      assert.equal(await page.getByLabel('完整回调 URL').count(), 0);
      startHandler = null;
      exchangeHandler = (route) => route.fulfill({ status: 409, json: { success: false, message: '授权交换结果不确定，请重新授权' } });
      await authorize();
      await page.getByText('错误：授权交换结果不确定，请重新授权', { exact: true }).waitFor();
      await page.getByLabel('完整回调 URL').waitFor({ state: 'hidden' });
      assert.equal(exchanges().length, 1);
      assert.equal(await page.locator('#channel-key-label').inputValue(), originalKey);
      assert.equal(await page.locator('input[name=name]').inputValue(), '失败后保留的编辑');
      exchangeHandler = null;
      await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).click();
      assert.equal(await page.getByLabel('完整回调 URL').inputValue(), '');
      assert.equal(exchanges().length, 1);
      assert.equal(requests.filter((request) => request.pathname.endsWith('/oauth/start')).length, 3);
    });

    await t.test('标签新增接收一份多行凭据，标签编辑不提供批量替换凭据', async () => {
      operation = 'create';
      await ready('tag-create');
      await authorize();
      await page.waitForFunction((key) => document.querySelector('#tag-channel-key')?.value === key, draftKey);
      await page.getByRole('button', { name: '提交', exact: true }).click();
      await page.locator('#tag-channel-key').waitFor({ state: 'hidden' });
      const create = requests.find((request) => request.pathname.endsWith('/channel') && request.method === 'POST');
      assert.equal(create.body.key, draftKey);
      const start = requests.find((request) => request.pathname.endsWith('/oauth/start'));
      assert.deepEqual(start.body, { proxy: 'http://proxy.example' });
      await ready('tag-edit');
      assert.equal(await page.locator('#channel-key-label').isDisabled(), true);
      assert.equal(await page.getByRole('button', { name: 'Antigravity OAuth 授权', exact: true }).count(), 0);
    });
    assert.deepEqual(pageErrors, []);
    assert.equal(await page.evaluate(() => Object.values(localStorage).some((value) => /draft-token|old-token|code-1/.test(value))), false);
  } finally {
    await browser?.close();
    if (server) {
      server.httpServer.closeAllConnections();
      await new Promise((resolve) => server.httpServer.close(resolve));
    }
    await rm(outDir, { recursive: true, force: true });
  }
});
