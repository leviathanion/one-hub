import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// PRICING_UI_BROWSER=/usr/bin/chromium node --test src/views/Pricing/component/pricingSync.browser.test.mjs
test(
  'price sync follows fetch, readable review, and explicit apply',
  { skip: !process.env.PRICING_UI_BROWSER, timeout: 120000 },
  async (t) => {
    const { build, preview } = await import('vite');
    const { chromium } = await import('playwright-core');
    const { default: config } = await import('../../../../vite.config.mjs');
    const root = fileURLToPath(new URL('../../../../', import.meta.url));
    const outDir = await mkdtemp(path.join(tmpdir(), 'pricing-sync-browser-'));
    let server;
    let browser;
    try {
      const buildConfig = {
        ...config,
        configFile: false,
        root,
        publicDir: false,
        logLevel: 'error',
        build: { outDir, emptyOutDir: true, rollupOptions: { input: path.join(root, 'tests/fixtures/pricing-sync.html') } }
      };
      await build(buildConfig);
      server = await preview({ ...buildConfig, preview: { host: '127.0.0.1', port: 0, open: false } });
      browser = await chromium.launch({ executablePath: process.env.PRICING_UI_BROWSER, headless: true, args: ['--no-sandbox'] });
      const page = await browser.newPage({ viewport: { width: 1280, height: 1000 } });
      page.setDefaultTimeout(5000);
      const errors = [];
      page.on('pageerror', (e) => errors.push(e.message));
      const source = [
        {
          model: 'sync-model',
          type: 'tokens',
          input: 1,
          output: 4,
          locked: false,
          extra_ratios: { cached_tokens: 0, future_meter: 0.25 },
          rate_rules: {
            version: 2,
            long_context: [
              {
                id: 'large-context',
                when: { input_tokens: { gt: 200000 } },
                multipliers: { input: 2, output: 1.5, extra_multipliers: { cached_tokens: 2 } }
              }
            ]
          }
        }
      ];
      const gallerySource = [
        { model: 'gpt-4.1', type: 'tokens', input: 1, output: 4, locked: false },
        {
          model: 'claude-sonnet-4-5',
          type: 'tokens',
          input: 1.5,
          output: 7.5,
          locked: false,
          extra_ratios: { cached_tokens: 0.1 }
        },
        { model: 'deepseek-chat', type: 'tokens', input: 0.14, output: 0.21, locked: false }
      ];
      const before = { ...source[0], input: 2, output: 3, extra_ratios: { cached_tokens: 0.5 }, rate_rules: {} };
      const requests = [];
      let fetchCount = 0;
      let modelsDevData;
      let previewFailure;
      let applyFailure;
      let holdCatalog;
      let holdPreview;
      let delayedMode;
      let holdApply;
      let gallery = false;
      let onlyLocked = false;
      await page.route('**/api/prices/modelsdev', async (route) => {
        fetchCount++;
        if (holdCatalog) await holdCatalog;
        await route.fulfill({ json: { success: true, data: modelsDevData } });
      });
      await page.route('**/api/prices/sync/preview', async (route) => {
        const body = route.request().postDataJSON();
        requests.push({ kind: 'preview', ...body });
        if (holdPreview && (!delayedMode || delayedMode === body.mode)) await holdPreview;
        if (previewFailure) return route.fulfill(previewFailure);
        let changes = onlyLocked
          ? [{ action: 'locked', model: 'locked-model', before, after: before }]
          : [
              {
                action: body.mode === 'add' ? 'add' : 'update',
                model: 'sync-model',
                before: body.mode === 'add' ? null : before,
                after: source[0]
              },
              ...(body.mode === 'overwrite'
                ? [
                    { action: 'delete', model: 'local-only', before, after: null },
                    { action: 'locked', model: 'locked-model', before, after: before }
                  ]
                : [])
            ];
        if (gallery) {
          const price = (model, input, output) => ({ model, type: 'tokens', input, output, locked: false });
          const added = { action: 'add', model: 'deepseek-chat', before: null, after: price('deepseek-chat', 0.14, 0.21) };
          const updates = [
            { action: 'update', model: 'gpt-4.1', before: price('gpt-4.1', 1.5, 6), after: price('gpt-4.1', 1, 4) },
            {
              action: 'update',
              model: 'claude-sonnet-4-5',
              before: { ...price('claude-sonnet-4-5', 1.5, 7.5), extra_ratios: { cached_tokens: 0.25 } },
              after: { ...price('claude-sonnet-4-5', 1.5, 7.5), extra_ratios: { cached_tokens: 0.1 } }
            }
          ];
          changes =
            body.mode === 'add'
              ? [added]
              : body.mode === 'update'
                ? updates
                : [
                    added,
                    ...updates,
                    { action: 'delete', model: 'legacy-model', before: price('legacy-model', 2, 6), after: null },
                    { action: 'locked', model: 'local-custom-model', before, after: before }
                  ];
        }
        await route.fulfill({ json: { success: true, data: { base_version: 7, digest: `digest-${body.mode}`, plan: { changes } } } });
      });
      await page.route('**/api/prices/sync/apply', async (route) => {
        requests.push({ kind: 'apply', ...route.request().postDataJSON() });
        if (holdApply) await holdApply;
        return route.fulfill(applyFailure || { json: { success: true } });
      });
      const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/pricing-sync.html`;
      const apply = () => page.locator('.MuiDialogActions-root .MuiLoadingButton-root');
      const mode = (name) => page.getByRole('radio', { name: new RegExp(`^${name}`) });
      const fetchModelsDev = () => page.getByRole('button', { name: 'Fetch from models.dev' }).click();
      const waitPreview = () => page.getByRole('region', { name: 'Price change summary', exact: true }).waitFor();
      const expectSource = async () => {
        assert.equal(await page.getByRole('radiogroup').count(), 0);
        assert.equal(await apply().count(), 0);
        assert.equal(await page.getByRole('table').count(), 0);
        assert.equal(await page.getByRole('textbox').count(), 0);
      };
      const screenshot = async (name) => {
        if (!process.env.PRICING_UI_SCREENSHOTS) return;
        await mkdir(process.env.PRICING_UI_SCREENSHOTS, { recursive: true });
        await page.evaluate(() => document.fonts.ready);
        await page
          .getByRole('dialog')
          .screenshot({ path: path.join(process.env.PRICING_UI_SCREENSHOTS, `${name}.png`), animations: 'disabled' });
      };
      const ready = async (query = '') => {
        requests.length = 0;
        fetchCount = 0;
        previewFailure = applyFailure = holdCatalog = holdPreview = delayedMode = holdApply = null;
        onlyLocked = false;
        gallery = query.includes('gallery');
        modelsDevData = { url: 'https://models.dev/api.json', prices: gallery ? gallerySource : source, skipped: 2, candidates: [] };
        await page.goto(url + query);
        await page.getByRole('dialog').waitFor();
        await expectSource();
      };

      const localizedSource = {
        zh_CN: {
          title: '同步模型价格',
          intro: '从 models.dev 获取报价，再核对变化并确认应用。',
          fetch: '从 models.dev 获取'
        },
        en_US: {
          title: 'Sync model prices',
          intro: 'Fetch quotes from models.dev, then review and apply the changes.',
          fetch: 'Fetch from models.dev'
        },
        zh_HK: {
          title: '同步模型價格',
          intro: '從 models.dev 取得報價，再核對變更並確認套用。',
          fetch: '從 models.dev 取得'
        },
        ja_JP: {
          title: 'モデル価格を同期',
          intro: 'models.dev から価格を取得し、変更内容を確認して適用します。',
          fetch: 'models.dev から取得'
        }
      };
      const expectLocalizedSource = async (language) => {
        const { title, intro, fetch } = localizedSource[language];
        const dialog = page.getByRole('dialog', { name: title, exact: true });
        await dialog.waitFor();
        await dialog.getByText(intro, { exact: true }).waitFor();
        await dialog.getByRole('button', { name: fetch, exact: true }).waitFor();
        assert.doesNotMatch(await dialog.textContent(), /pricingSync\.|modelsDev\./);
      };

      await t.test('production translation resources localize the source dialog in every supported language', async () => {
        for (const language of Object.keys(localizedSource)) {
          await ready(`?lang=${language}`);
          await expectLocalizedSource(language);
        }
      });

      await t.test('an open dialog follows language changes without fetching quotes', async () => {
        await ready('?lang=zh_CN');
        await expectLocalizedSource('zh_CN');
        const dialog = await page.getByRole('dialog').elementHandle();
        for (const language of ['en_US', 'zh_CN']) {
          await page.locator('#fixture-language').selectOption(language);
          await expectLocalizedSource(language);
          assert.equal(await dialog.evaluate((element) => element === document.querySelector('[role="dialog"]')), true);
        }
        assert.deepEqual(requests, []);
        assert.equal(fetchCount, 0);
      });

      await t.test('models.dev first, then radio modes and review; fetching again and reopening reset the session', async () => {
        await ready();
        assert.deepEqual(requests, []);
        await fetchModelsDev();
        await waitPreview();
        assert.equal(await mode('Add Only').isChecked(), true);
        assert.equal(await page.getByRole('button', { name: 'Fetch from models.dev', exact: true }).count(), 0);
        assert.equal(await page.getByRole('textbox').count(), 0);
        await page.getByRole('button', { name: 'Fetch again' }).click();
        await waitPreview();
        await page.getByRole('button', { name: 'Cancel', exact: true }).click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        await page.getByRole('button', { name: 'Open sync' }).click();
        await expectSource();
      });

      await t.test('models.dev prices have readable changes and a single apply bound to the chosen mode', async () => {
        await ready();
        await fetchModelsDev();
        await waitPreview();
        assert.deepEqual(requests, [{ kind: 'preview', mode: 'add', source }]);
        for (const [name, value] of [
          ['Update Only', 'update'],
          ['Overwrite All', 'overwrite'],
          ['Add Only', 'add']
        ]) {
          await mode(name).check();
          await waitPreview();
          assert.deepEqual(requests.at(-1), { kind: 'preview', mode: value, source });
          assert.equal(await mode(name).isChecked(), true);
        }
        await mode('Overwrite All').check();
        await waitPreview();
        const changeList = page.getByRole('region', { name: 'Price changes', exact: true });
        const text = await changeList.textContent();
        assert.match(text, /Input multiplier2\s*→\s*1/);
        assert.match(text, /Cache Ratio0.5\s*→\s*0/);
        assert.match(text, /future_meterNot configured\s*→\s*0.25/);
        assert.match(text, /200000/);
        assert.match(text, /Input 2×/);
        assert.match(text, /Output 1.5×/);
        assert.equal(await page.locator('pre').count(), 0);
        assert.equal(await page.getByText(/base_version|digest-|"extra_ratios"/).count(), 0);
        await page.getByRole('region', { name: 'Models to remove', exact: true }).waitFor();
        await page.getByText('Locked; the current price will be kept.').waitFor();
        assert.equal(await apply().textContent(), 'Apply 2 changes');
        assert.equal(await page.getByRole('button', { name: 'Recalculate changes' }).count(), 0);
        await page.getByRole('button', { name: 'Fetch again' }).click();
        await waitPreview();
        await mode('Overwrite All').check();
        await waitPreview();
        assert.equal(await changeList.textContent(), text);
        assert.equal(
          requests.some((r) => r.kind === 'apply'),
          false
        );
        await apply().click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        assert.deepEqual(requests.at(-1), { kind: 'apply', mode: 'overwrite', source, base_version: 7, digest: 'digest-overwrite' });
      });

      await t.test('quote sources explain selection and skipped candidates without rendering the whole catalog', async () => {
        await ready();
        modelsDevData.candidates = [
          { model: 'sync-model', provider: 'openai', selected: true },
          { model: 'sync-model', provider: 'host', selected: false, reason: 'another provider selected' },
          { model: 'ambiguous-model', provider: 'host', selected: false, reason: 'ambiguous providers' },
          { model: 'invalid-official', provider: 'host', selected: false, reason: 'official provider price is invalid' },
          { model: 'unsupported-model', provider: 'host', selected: false, reason: 'unsupported cost field future_cost' },
          ...Array.from({ length: 21 }, (_, index) => ({ model: `extra-${index}`, provider: 'host', selected: true }))
        ];
        await fetchModelsDev();
        await waitPreview();
        const details = page.locator('details');
        assert.equal(await details.getByRole('listitem').count(), 0);
        await details.locator('summary').click();
        await details.getByText('Selected provider quote', { exact: true }).first().waitFor();
        await details.getByText('Not selected: another provider quote was chosen').waitFor();
        await details.getByText('Skipped: multiple sources for this model are ambiguous').waitFor();
        await details.getByText('Skipped: the official provider quote is invalid').waitFor();
        await details.getByText('Skipped: unsupported cost field future_cost').waitFor();
        assert.equal(await details.getByRole('listitem').count(), 25);
        await details.getByRole('button', { name: 'Next sources page' }).click();
        await details.getByText('extra-20 · host', { exact: true }).waitFor();
        assert.equal(await details.getByRole('listitem').count(), 1);
        assert.deepEqual(requests, [{ kind: 'preview', mode: 'add', source }]);
      });

      await t.test('rapid mode changes cancel stale previews and only the latest mode can apply', async () => {
        await ready();
        await fetchModelsDev();
        await waitPreview();
        let release;
        delayedMode = 'update';
        holdPreview = new Promise((resolve) => {
          release = resolve;
        });
        const failed = page.waitForEvent('requestfailed', { predicate: (request) => request.url().endsWith('/sync/preview') });
        await mode('Update Only').check();
        await page.getByRole('status').getByText('Calculating price changes…').waitFor();
        assert.equal(await page.getByRole('button', { name: 'Fetch from models.dev', exact: true }).count(), 0);
        assert.equal(await page.getByRole('button', { name: 'Recalculate changes' }).count(), 0);
        assert.equal(await apply().isDisabled(), true);
        await mode('Overwrite All').check();
        await failed;
        await waitPreview();
        release();
        await apply().click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        assert.equal(requests.at(-1).digest, 'digest-overwrite');
      });

      await t.test('invalid catalogs stay on the source screen with no pointless retry', async () => {
        await ready();
        for (const data of [{ prices: null, skipped: 3 }, { candidates: [] }]) {
          modelsDevData = data;
          await fetchModelsDev();
          await page.getByText(/Could not fetch models.dev quotes/).waitFor();
          await expectSource();
          assert.equal(requests.length, 0);
          assert.equal(await page.getByRole('button', { name: 'Recalculate changes' }).count(), 0);
        }
        modelsDevData = { prices: source, skipped: 0 };
        previewFailure = {
          json: { success: false, code: 'duplicate_price_model', model: 'sync-model', message: 'duplicate remote price model "sync-model"' }
        };
        await fetchModelsDev();
        await page.getByText(/duplicate quotes for “sync-model”/).waitFor();
        await expectSource();
        assert.equal(await page.getByRole('button', { name: 'Recalculate changes' }).count(), 0);
        assert.equal(await page.getByText(/duplicate remote price model/).count(), 0);
      });

      await t.test('all skipped quotes retain source reasons without preview or apply', async () => {
        await ready();
        modelsDevData = {
          prices: [],
          skipped: 1,
          candidates: [{ model: 'unsupported-model', provider: 'openai', selected: false, reason: 'unsupported cost field future_cost' }]
        };
        await fetchModelsDev();
        await page.getByText('No quotes can be synced.', { exact: false }).waitFor();
        await page.locator('details summary').click();
        await page.getByText('Skipped: unsupported cost field future_cost').waitFor();
        assert.equal(await page.getByRole('radiogroup').count(), 0);
        assert.equal(await apply().count(), 0);
        assert.deepEqual(requests, []);
        modelsDevData = { prices: source, skipped: 0, candidates: [] };
        await page.getByRole('button', { name: 'Fetch again' }).click();
        await waitPreview();
        assert.deepEqual(requests, [{ kind: 'preview', mode: 'add', source }]);
      });

      await t.test('preview retry uses the fetched source and reports a failure once', async () => {
        await ready();
        previewFailure = { status: 503, json: { success: false, message: 'temporarily unavailable' } };
        await fetchModelsDev();
        const retry = page.getByRole('button', { name: 'Recalculate changes', exact: true });
        await retry.waitFor();
        assert.equal(await page.getByText(/temporarily unavailable/).count(), 1);
        assert.equal(await apply().isDisabled(), true);
        assert.equal(await page.getByRole('button', { name: 'Fetch from models.dev', exact: true }).count(), 0);
        previewFailure = null;
        await retry.click();
        await waitPreview();
        assert.equal(fetchCount, 1);
        assert.deepEqual(requests.at(-1), { kind: 'preview', mode: 'add', source });
      });

      await t.test('apply is exclusive and a failed apply cannot reuse its old preview or replay itself', async () => {
        await ready();
        await fetchModelsDev();
        await waitPreview();
        let release;
        holdApply = new Promise((resolve) => {
          release = resolve;
        });
        applyFailure = { json: { success: false, message: 'publication changed' } };
        await apply().click();
        assert.equal(await page.getByRole('button', { name: 'Fetch again' }).isDisabled(), true);
        assert.equal(await page.getByRole('button', { name: 'Cancel', exact: true }).isDisabled(), true);
        assert.equal(await mode('Overwrite All').isDisabled(), true);
        await apply().dispatchEvent('click');
        release();
        await page.getByText(/The apply result is unconfirmed/).waitFor();
        assert.equal(await apply().isDisabled(), true);
        await page.getByRole('button', { name: 'Recalculate changes' }).click();
        await waitPreview();
        assert.equal(requests.filter((r) => r.kind === 'apply').length, 1);
        assert.equal(fetchCount, 1);
      });

      await t.test('locked-only results cannot apply', async () => {
        await ready();
        onlyLocked = true;
        await fetchModelsDev();
        await waitPreview();
        await page.getByText('Locked; the current price will be kept.').waitFor();
        assert.equal(await apply().isDisabled(), true);
      });

      await t.test('closing an in-flight fetch cancels it before reopening', async () => {
        await ready();
        let release;
        holdCatalog = new Promise((resolve) => {
          release = resolve;
        });
        const fetched = page.waitForRequest('**/api/prices/modelsdev');
        await fetchModelsDev();
        await fetched;
        await expectSource();
        const failed = page.waitForEvent('requestfailed', { predicate: (request) => request.url().endsWith('/modelsdev') });
        await page.getByRole('button', { name: 'Cancel', exact: true }).click();
        await failed;
        release();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        await page.getByRole('button', { name: 'Open sync' }).click();
        await expectSource();
        assert.deepEqual(requests, []);
      });

      await t.test('Chinese desktop and mobile screenshots use the actual themed dialog', async () => {
        await page.setViewportSize({ width: 1440, height: 1080 });
        await ready('?lang=zh_CN&gallery=1');
        await screenshot('01-source-desktop');
        await page.getByRole('button', { name: '从 models.dev 获取' }).click();
        await page.getByRole('region', { name: '价格变化汇总', exact: true }).waitFor();
        await mode('只更新现有').check();
        await page.getByRole('region', { name: '价格变动', exact: true }).waitFor();
        await screenshot('02-review-desktop');
        await mode('覆盖所有').check();
        await page.getByRole('region', { name: '删除模型', exact: true }).waitFor();
        await screenshot('03-overwrite-desktop');
        await page.getByRole('region', { name: '删除模型', exact: true }).scrollIntoViewIfNeeded();
        await screenshot('04-removals-desktop');
        await page.setViewportSize({ width: 390, height: 844 });
        await ready('?lang=zh_CN&gallery=1');
        await screenshot('05-source-mobile');
        await page.getByRole('button', { name: '从 models.dev 获取' }).click();
        await page.getByRole('region', { name: '价格变化汇总', exact: true }).waitFor();
        await mode('只更新现有').check();
        const changeList = page.getByRole('region', { name: '价格变动', exact: true });
        await changeList.waitFor();
        await screenshot('06-review-mobile');
        await changeList.scrollIntoViewIfNeeded();
        assert.equal(await apply().textContent(), '确认应用 2 项变更');
        assert.equal(await page.locator('pre').count(), 0);
        assert.equal(await page.getByRole('dialog').evaluate((el) => el.scrollWidth <= el.clientWidth), true);
        assert.equal(await changeList.evaluate((el) => el.scrollWidth <= el.clientWidth), true);
        await screenshot('07-changes-mobile');
        await ready('?lang=zh_CN');
        await page.getByRole('button', { name: '从 models.dev 获取' }).click();
        await page.getByRole('region', { name: '价格变化汇总', exact: true }).waitFor();
        await mode('只更新现有').check();
        const rule = page.getByRole('row').filter({ hasText: 'large-context' });
        await rule.scrollIntoViewIfNeeded();
        assert.equal(await rule.evaluate((el) => el.scrollWidth <= el.clientWidth), true);
        await screenshot('08-rules-mobile');
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
  }
);
