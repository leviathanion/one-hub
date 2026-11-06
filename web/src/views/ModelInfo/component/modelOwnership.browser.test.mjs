import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

test('两个页面独立提供手动归属与 models.dev 映射', { skip: !process.env.PRICING_UI_BROWSER, timeout: 180000 }, async () => {
  const { build, preview } = await import('vite');
  const { chromium } = await import('playwright-core');
  const { default: config } = await import('../../../../vite.config.mjs');
  const root = fileURLToPath(new URL('../../../../', import.meta.url));
  const outDir = await mkdtemp(path.join(tmpdir(), 'model-ownership-browser-'));
  let server;
  let browser;
  try {
    const buildConfig = {
      ...config,
      configFile: false,
      root,
      publicDir: false,
      logLevel: 'error',
      build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/model-ownership.html') } }
    };
    await build(buildConfig);
    server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0, open: false } });
    browser = await chromium.launch({ executablePath: process.env.PRICING_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
    const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
    page.setDefaultTimeout(7000);
    const errors = [];
    page.on('pageerror', (error) => errors.push(error.message));
    const owners = { 1: { name: 'OpenAI' }, 1001: { name: '自定义归属' }, 1003: { name: 'Cloudflare' } };
    const openAI = { id: 'openai', name: 'OpenAI' };
    const otherHost = { id: 'host-other', name: '其他平台' };
    const sameNameHost = { id: 'openai-compatible', name: 'OpenAI' };
    const cloudflare = { id: 'cloudflare-workers-ai', name: 'Cloudflare Workers AI' };
    const moonshot = { id: 'moonshotai', name: 'Moonshot AI' };
    const cloudflareModels = ['@cf/moonshotai/kimi-k2.6', '@cf/moonshotai/kimi-k2.7-code', '@cf/nvidia/nemotron-3-120b-a12b'];
    const moonshotModel = 'kimi-k2.6';
    const infos = new Map([['owned', { id: 1, model: 'owned', name: '已有说明', description: '保留描述', owned_by_id: 1 }]]);
    const candidates = [
      'owned',
      'new-model',
      'source-a',
      'source-b',
      'source-conflict',
      'source-unknown',
      'source-same-name',
      ...cloudflareModels,
      moonshotModel,
      'source-unmapped',
      ...Array.from({ length: 25 }, (_, i) => `page-${i}`)
    ];
    const writes = [];
    const sourceRequests = [];
    let failNext = true;
    let failOwner = true;
    let reads = 0;
    let priceReads = 0;
    let releaseSource;
    let sourceWait = null;
    await page.route('**/api/**', async (route) => {
      const request = route.request();
      const url = new URL(request.url());
      let data;
      if (url.pathname === '/api/ownedby') data = owners;
      else if (url.pathname === '/api/model_info/') {
        reads++;
        data = [...infos.values()];
      } else if (url.pathname === '/api/model_info/ownership/catalog') {
        data = [...new Set([...candidates, ...infos.keys()])].map((model) => ({
          model,
          name: infos.get(model)?.name || '',
          owned_by_id: infos.get(model)?.owned_by_id ?? null,
          has_info: infos.has(model)
        }));
      } else if (url.pathname === '/api/model_ownedby/' && request.method() === 'POST') {
        const body = request.postDataJSON();
        if (failOwner) {
          failOwner = false;
          await route.fulfill({ status: 400, json: { success: false, message: '归属分类暂时保存失败' } });
          return;
        }
        owners[body.id] = { name: body.name };
        data = {};
      } else if (url.pathname === '/api/model_info/ownership/suggestions') {
        const { models } = request.postDataJSON();
        sourceRequests.push(models);
        if (sourceWait) await sourceWait;
        data = models.map((model) =>
          model === 'source-conflict'
            ? { model, providers: [openAI, otherHost], status: 'conflict' }
            : model === 'source-unknown'
              ? { model, providers: [], status: 'unmatched' }
              : {
                  model,
                  providers: [
                    cloudflareModels.includes(model)
                      ? cloudflare
                      : model === moonshotModel
                        ? moonshot
                        : model === 'source-unmapped'
                          ? otherHost
                          : model === 'source-same-name'
                            ? sameNameHost
                            : openAI
                  ],
                  status: 'matched'
                }
        );
      } else if (url.pathname === '/api/model_info/ownership/batch') {
        const body = request.postDataJSON();
        writes.push(body);
        if (failNext) {
          failNext = false;
          await route.fulfill({ status: 500, json: { success: false, message: '保存暂时失败，请重试' } });
          return;
        }
        data = { created: 0, updated: 0, unchanged: 0, skipped: 0 };
        for (const item of body.items) {
          const existing = infos.get(item.model);
          if (body.mode === 'fill_missing' && existing?.owned_by_id != null) {
            data.skipped++;
            continue;
          }
          if (!existing) {
            data.created++;
            infos.set(item.model, { id: infos.size + 1, model: item.model, name: '', owned_by_id: item.owned_by_id });
          } else {
            data.updated++;
            existing.owned_by_id = item.owned_by_id;
          }
        }
      } else if (url.pathname === '/api/prices/model_list') data = ['owned', 'new-model'];
      else if (url.pathname === '/api/prices') {
        priceReads++;
        data = [
          {
            model: 'owned',
            type: 'tokens',
            input: 1,
            output: 2,
            locked: false,
            extra_ratios: {},
            rate_rules: {},
            model_info: infos.get('owned')
          }
        ];
      } else throw new Error(`Unexpected request: ${request.method()} ${url.pathname}`);
      await route.fulfill({ json: { success: true, data, version: 1 } });
    });
    const url = `${server.resolvedUrls.local[0]}tests/fixtures/model-ownership.html`;
    let editorTitle = '手动设置归属';
    const editor = () => page.getByRole('dialog', { name: editorTitle, exact: true });
    const open = async (method = 'manual') => {
      await page.getByRole('button', { name: '手动设置归属', exact: true }).waitFor();
      await page.getByRole('button', { name: '从 models.dev 匹配归属', exact: true }).waitFor();
      editorTitle = method === 'source' ? '从 models.dev 匹配归属' : '手动设置归属';
      await page.getByRole('button', { name: editorTitle, exact: true }).click();
      await editor().getByRole('checkbox', { name: '选择本页模型', exact: true }).waitFor();
      assert.equal(
        await editor()
          .getByRole('combobox', { name: /^每页模型数/ })
          .textContent(),
        '25'
      );
      assert.equal(
        await editor()
          .getByRole('combobox', { name: /^设置方式/ })
          .count(),
        0
      );
      if (method === 'manual') {
        await editor()
          .getByRole('combobox', { name: /^目标归属/ })
          .waitFor();
        assert.equal(await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).count(), 0);
        assert.equal(await editor().getByRole('table', { name: 'models.dev 归属映射', exact: true }).count(), 0);
      } else {
        await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).waitFor();
        assert.equal(
          await editor()
            .getByRole('combobox', { name: /^目标归属/ })
            .count(),
          0
        );
      }
      assert.equal(
        await editor()
          .getByRole('combobox', { name: /^当前归属/ })
          .textContent(),
        '未设置归属'
      );
      await choose('当前归属', '全部归属');
      await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).waitFor();
    };
    const choose = async (label, option) => {
      await editor()
        .getByRole('combobox', { name: new RegExp(`^${label}`) })
        .click();
      await page.getByRole('option', { name: option, exact: true }).click();
    };
    const search = async (model) => {
      await editor().getByRole('textbox', { name: '搜索模型', exact: true }).fill(model);
    };
    const select = async (model) => {
      await search(model);
      await editor()
        .getByRole('checkbox', { name: `选择 ${model}`, exact: true })
        .check();
    };

    await page.goto(url);
    await open();
    await editor().getByRole('checkbox', { name: '选择本页模型', exact: true }).check();
    await editor().getByRole('button', { name: '下一页模型' }).click();
    await editor().getByRole('status').filter({ hasText: '已选 25 / 5000 个，其中 25 个不在当前页' }).waitFor();
    await choose('每页模型数', '50');
    await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).waitFor();
    assert.equal(await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).isChecked(), true);
    await editor().getByRole('status').filter({ hasText: '已选 25 / 5000 个，其中 0 个不在当前页' }).waitFor();
    await choose('每页模型数', '25');
    await editor().getByRole('status').filter({ hasText: '已选 25 / 5000 个，其中 0 个不在当前页' }).waitFor();
    await editor().getByRole('button', { name: '清空选择' }).click();
    await editor().getByRole('textbox', { name: '粘贴模型名（每行一个）' }).fill('temporary-model\nowned\ntemporary-model');
    await editor().getByRole('button', { name: '加入选择' }).click();
    await search('owned');
    const ownedRow = editor()
      .getByRole('row')
      .filter({ has: page.getByRole('checkbox', { name: '选择 owned', exact: true }) });
    await ownedRow.getByText('已有说明', { exact: true }).waitFor();
    await ownedRow.getByText('OpenAI', { exact: true }).waitFor();
    await search('temporary-model');
    await editor().getByRole('checkbox', { name: '选择 temporary-model', exact: true }).uncheck();
    await editor().getByRole('row').filter({ hasText: 'temporary-model' }).getByText('尚无模型信息', { exact: true }).waitFor();
    await editor().getByRole('button', { name: '取消', exact: true }).click();
    await open();
    await search('temporary-model');
    await editor().getByText('没有符合条件的模型，也可以在下方粘贴精确模型名。', { exact: true }).waitFor();
    assert.equal(await editor().getByRole('checkbox', { name: '选择 temporary-model', exact: true }).count(), 0);
    await select('owned');
    await editor().getByRole('textbox', { name: '粘贴模型名（每行一个）' }).fill('new-model\nBrandNew\nnew-model');
    await editor().getByRole('button', { name: '加入选择' }).click();
    await choose('目标归属', '自定义归属');
    const beforeSaveReads = reads;
    await editor().getByRole('button', { name: '保存 3 个模型的归属', exact: true }).click();
    await editor().getByText('保存暂时失败，请重试', { exact: true }).waitFor();
    assert.equal(
      await editor()
        .getByRole('combobox', { name: /^目标归属/ })
        .textContent(),
      '自定义归属'
    );
    assert.equal(await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).isChecked(), true);
    await editor().getByRole('button', { name: '保存 3 个模型的归属', exact: true }).click();
    await editor().waitFor({ state: 'hidden' });
    await page.getByRole('row').filter({ hasText: 'BrandNew' }).waitFor();
    assert.ok(reads > beforeSaveReads);
    assert.equal(infos.get('owned').owned_by_id, 1);
    assert.equal(infos.get('owned').description, '保留描述');
    assert.deepEqual(writes[0], {
      mode: 'fill_missing',
      items: [
        { model: 'owned', owned_by_id: 1001 },
        { model: 'new-model', owned_by_id: 1001 },
        { model: 'BrandNew', owned_by_id: 1001 }
      ]
    });

    await open('source');
    await editor()
      .getByRole('textbox', { name: '粘贴模型名（每行一个）' })
      .fill('source-a\nsource-conflict\nsource-unknown\nsource-unmapped\nconstructor');
    await editor().getByRole('button', { name: '加入选择' }).click();
    await search('constructor');
    await editor().getByRole('row').filter({ hasText: 'constructor' }).getByText('尚未查询', { exact: true }).waitFor();
    await editor().getByRole('checkbox', { name: '选择 constructor', exact: true }).uncheck();
    await search('source-');
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await editor()
      .getByRole('combobox', { name: /^OpenAI（openai）映射到本系统的归属/ })
      .waitFor();
    assert.deepEqual(sourceRequests[0], ['source-a', 'source-conflict', 'source-unknown', 'source-unmapped']);
    const mapping = () => editor().getByRole('table', { name: 'models.dev 归属映射', exact: true });
    await mapping().getByRole('columnheader', { name: 'models.dev 服务提供方', exact: true }).waitFor();
    await mapping().getByRole('columnheader', { name: '映射到本系统的归属', exact: true }).waitFor();
    const sourceRow = () =>
      mapping()
        .getByRole('row')
        .filter({ has: page.getByRole('combobox', { name: /^OpenAI（openai）映射到本系统的归属/ }) });
    await sourceRow()
      .getByText(/1 个模型/)
      .waitFor();
    await sourceRow().locator('summary').click();
    await sourceRow().getByText('source-a', { exact: true }).waitFor();
    assert.equal(
      await editor()
        .getByRole('combobox', { name: /^OpenAI（openai）映射到本系统的归属/ })
        .textContent(),
      '暂不设置'
    );
    assert.equal(await editor().getByRole('button', { name: '保存 0 个模型的归属', exact: true }).isDisabled(), true);
    await choose('OpenAI（openai）映射到本系统的归属', 'OpenAI');
    assert.equal(await editor().getByRole('button', { name: '保存 1 个模型的归属', exact: true }).isEnabled(), true);
    await editor()
      .getByText(/已选 4 个模型，可保存 1 个/)
      .waitFor();
    await editor()
      .getByText(/其余 3 个保留原样/)
      .waitFor();
    await search('source-');
    await editor().getByText('提供方冲突：OpenAI（openai）、其他平台（host-other）', { exact: true }).waitFor();
    await editor().getByText('未查到服务提供方', { exact: true }).waitFor();
    await editor().getByRole('checkbox', { name: '选择 source-b', exact: true }).check();
    assert.equal(await editor().getByRole('button', { name: '保存 1 个模型的归属', exact: true }).isEnabled(), true);
    await editor()
      .getByText(/已选 5 个模型，可保存 1 个/)
      .waitFor();
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await sourceRow()
      .getByText(/2 个模型/)
      .waitFor();
    await sourceRow().getByText('source-b', { exact: true }).waitFor();
    assert.equal(await editor().getByRole('button', { name: '保存 2 个模型的归属', exact: true }).isEnabled(), true);
    await editor().getByRole('button', { name: '新建归属分类', exact: true }).click();
    const create = page.getByRole('dialog').filter({ has: page.locator('input[name="id"]') });
    await create.locator('input[name="id"]').fill('1002');
    await create.locator('input[name="name"]').fill('实验室');
    await create.getByRole('button', { name: '提交', exact: true }).click();
    await create.getByText('归属分类暂时保存失败', { exact: true }).waitFor();
    assert.equal(await create.locator('input[name="name"]').inputValue(), '实验室');
    await create.getByRole('button', { name: '提交', exact: true }).click();
    await create.waitFor({ state: 'hidden' });
    await choose('OpenAI（openai）映射到本系统的归属', '实验室');
    if (process.env.OWNERSHIP_UI_SCREENSHOT) await editor().screenshot({ path: process.env.OWNERSHIP_UI_SCREENSHOT });
    await editor().getByRole('button', { name: '保存 2 个模型的归属', exact: true }).click();
    await editor().waitFor({ state: 'hidden' });
    assert.deepEqual(writes.at(-1).items, [
      { model: 'source-a', owned_by_id: 1002 },
      { model: 'source-b', owned_by_id: 1002 }
    ]);
    for (const model of ['source-conflict', 'source-unknown', 'source-unmapped']) assert.equal(infos.has(model), false);

    await open('source');
    await editor().getByRole('textbox', { name: '粘贴模型名（每行一个）' }).fill('source-a\nsource-same-name');
    await editor().getByRole('button', { name: '加入选择' }).click();
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await choose('OpenAI（openai）映射到本系统的归属', 'OpenAI');
    const sameNameSelection = editor().getByRole('combobox', { name: 'OpenAI（openai-compatible）映射到本系统的归属', exact: true });
    assert.equal(await sameNameSelection.textContent(), '暂不设置');
    assert.equal(await mapping().getByRole('row').count(), 3);
    assert.equal(await editor().getByRole('button', { name: '保存 1 个模型的归属', exact: true }).isEnabled(), true);
    await editor().getByRole('button', { name: '取消', exact: true }).click();

    await open('source');
    await editor()
      .getByRole('textbox', { name: '粘贴模型名（每行一个）' })
      .fill([...cloudflareModels, moonshotModel].join('\n'));
    await editor().getByRole('button', { name: '加入选择' }).click();
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    const cloudflareRow = mapping()
      .getByRole('row')
      .filter({ has: page.getByText('cloudflare-workers-ai', { exact: true }) });
    const moonshotRow = mapping()
      .getByRole('row')
      .filter({ has: page.getByText('moonshotai', { exact: true }) });
    await cloudflareRow.getByText('Cloudflare Workers AI', { exact: true }).waitFor();
    await cloudflareRow.getByText(/3 个模型/).waitFor();
    await moonshotRow.getByText('Moonshot AI', { exact: true }).waitFor();
    await moonshotRow.getByText(/1 个模型/).waitFor();
    await cloudflareRow.locator('summary').click();
    for (const model of cloudflareModels) await cloudflareRow.getByText(model, { exact: true }).waitFor();
    assert.equal(await cloudflareRow.getByText(moonshotModel, { exact: true }).count(), 0);
    await choose('Cloudflare Workers AI（cloudflare-workers-ai）映射到本系统的归属', 'Cloudflare');
    assert.equal(await editor().getByRole('button', { name: '保存 3 个模型的归属', exact: true }).isEnabled(), true);
    await choose('Moonshot AI（moonshotai）映射到本系统的归属', '自定义归属');
    if (process.env.OWNERSHIP_UI_SCREENSHOT) await editor().screenshot({ path: process.env.OWNERSHIP_UI_SCREENSHOT });
    await editor().getByRole('button', { name: '保存 4 个模型的归属', exact: true }).click();
    await editor().waitFor({ state: 'hidden' });
    assert.deepEqual(writes.at(-1), {
      mode: 'fill_missing',
      items: [...cloudflareModels.map((model) => ({ model, owned_by_id: 1003 })), { model: moonshotModel, owned_by_id: 1001 }]
    });
    for (const model of cloudflareModels) assert.equal(infos.get(model).owned_by_id, 1003);
    assert.equal(infos.get(moonshotModel).owned_by_id, 1001);

    await open('source');
    await select('source-a');
    sourceWait = new Promise((resolve) => {
      releaseSource = resolve;
    });
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await editor().getByRole('button', { name: '正在查询…', exact: true }).waitFor();
    await editor().getByRole('button', { name: '取消', exact: true }).click();
    await open();
    releaseSource();
    sourceWait = null;
    await editor().getByRole('status').filter({ hasText: '已选 0 / 5000' }).waitFor();
    assert.equal(
      await editor()
        .getByRole('combobox', { name: /^OpenAI（openai）映射到本系统的归属/ })
        .count(),
      0
    );
    await editor().getByRole('button', { name: '取消', exact: true }).click();

    await page.goto(`${url}?pricing`);
    await open('source');
    await select('source-a');
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await mapping().getByRole('columnheader', { name: 'models.dev 服务提供方', exact: true }).waitFor();
    await mapping().getByRole('columnheader', { name: '映射到本系统的归属', exact: true }).waitFor();
    assert.equal(await editor().getByRole('button', { name: '保存 0 个模型的归属', exact: true }).isDisabled(), true);
    await editor().getByRole('button', { name: '取消', exact: true }).click();
    await open();
    await select('owned');
    await choose('目标归属', '清空归属');
    assert.equal(
      await editor()
        .getByRole('combobox', { name: /^已有归属处理/ })
        .textContent(),
      '覆盖所选模型的归属'
    );
    const pricesBefore = priceReads;
    const refreshedPrices = page.waitForResponse((response) => new URL(response.url()).pathname === '/api/prices');
    await editor().getByRole('button', { name: '清空 1 个模型的归属', exact: true }).click();
    await editor().waitFor({ state: 'hidden' });
    await refreshedPrices;
    assert.ok(priceReads > pricesBefore);
    assert.deepEqual(writes.at(-1), { mode: 'replace', items: [{ model: 'owned', owned_by_id: null }] });
    const bulkModels = Array.from({ length: 2001 }, (_, index) => `bulk-model-${index}`);
    candidates.push(...bulkModels);
    await open('source');
    await select('owned');
    await search('bulk-model-');
    await editor().getByRole('button', { name: '全选筛选结果（2001 个）', exact: true }).click();
    await editor().getByRole('status').filter({ hasText: '已选 2002 / 5000' }).waitFor();
    await editor().getByRole('button', { name: '下一页模型' }).click();
    assert.equal(await editor().getByRole('checkbox', { name: '选择本页模型', exact: true }).isChecked(), true);
    await editor().getByRole('button', { name: '取消选择筛选结果', exact: true }).click();
    await editor().getByRole('status').filter({ hasText: '已选 1 / 5000' }).waitFor();
    await search('owned');
    assert.equal(await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).isChecked(), true);
    await editor().getByRole('checkbox', { name: '选择 owned', exact: true }).uncheck();
    await search('bulk-model-');
    await editor().getByRole('button', { name: '全选筛选结果（2001 个）', exact: true }).click();
    await editor().getByRole('status').filter({ hasText: '已选 2001 / 5000' }).waitFor();
    await editor().getByRole('button', { name: '查询所选模型的归属', exact: true }).click();
    await sourceRow()
      .getByText(/2001 个模型/)
      .waitFor();
    assert.deepEqual(sourceRequests.at(-1), bulkModels);
    await choose('OpenAI（openai）映射到本系统的归属', '自定义归属');
    await editor().getByRole('button', { name: '保存 2001 个模型的归属', exact: true }).click();
    await editor().waitFor({ state: 'hidden' });
    assert.deepEqual(writes.at(-1), {
      mode: 'fill_missing',
      items: bulkModels.map((model) => ({ model, owned_by_id: 1001 }))
    });
    assert.equal(infos.get('owned').owned_by_id, null);
    assert.deepEqual(errors, []);
  } finally {
    await browser?.close();
    await new Promise((resolve) => (server ? server.httpServer.close(resolve) : resolve()));
    await rm(outDir, { recursive: true, force: true });
  }
});
