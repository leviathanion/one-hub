import assert from 'node:assert/strict';
import test from 'node:test';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

// PRICING_UI_BROWSER=/usr/bin/chromium node --test src/views/Pricing/component/pricingSync.browser.test.mjs
test(
  'both pricing sources use the same preview, mode controls and apply flow',
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
      const page = await browser.newPage();
      page.setDefaultTimeout(5000);
      const errors = [];
      page.on('pageerror', (e) => errors.push(e.message));
      const source = [{ model: 'sync-model', type: 'tokens', input: 1, output: 4 }];
      const requests = [];
      let modelsDevData;
      let failPreview = false;
      let holdCatalog;
      let releaseCatalog;
      await page.route('**/api/prices/modelsdev', async (route) => {
        if (holdCatalog) await holdCatalog;
        await route.fulfill({ json: { success: true, data: modelsDevData } });
      });
      await page.route('**/catalog', (route) => route.fulfill({ json: source }));
      await page.route('**/api/prices/sync/preview', async (route) => {
        const body = route.request().postDataJSON();
        requests.push({ kind: 'preview', ...body });
        if (failPreview) return route.fulfill({ status: 409, json: { success: false, message: 'refresh required' } });
        await route.fulfill({
          json: {
            success: true,
            data: {
              base_version: 7,
              digest: `digest-${body.mode}`,
              plan: {
                changes: [{ action: body.mode === 'add' ? 'add' : 'update', model: 'sync-model', before: { input: 2 }, after: source[0] }]
              }
            }
          }
        });
      });
      await page.route('**/api/prices/sync/apply', (route) => {
        requests.push({ kind: 'apply', ...route.request().postDataJSON() });
        return route.fulfill({ json: { success: true } });
      });
      const url = `http://127.0.0.1:${server.httpServer.address().port}/tests/fixtures/pricing-sync.html`;
      const ready = async () => {
        requests.length = 0;
        failPreview = false;
        holdCatalog = null;
        modelsDevData = { url: 'https://models.dev/api.json', prices: source, skipped: 2, candidates: [] };
        await page.goto(url);
        await page.getByRole('button', { name: 'Fetch from models.dev' }).waitFor();
        await expectFetchOnly();
      };
      const apply = () => page.locator('.MuiDialogActions-root .MuiLoadingButton-root');
      const mode = (name) => page.getByRole('group').getByRole('button', { name, exact: true });
      const waitPreview = () => page.getByText('base_version: 7', { exact: true }).waitFor();
      const expectFetchOnly = async () => {
        for (const name of ['Add Only', 'Update Only', 'Overwrite All']) {
          assert.equal(await page.getByRole('button', { name, exact: true }).count(), 0, `${name} requires a fetched catalog`);
        }
        assert.equal(await apply().count(), 0);
        assert.equal(await page.getByText(/Updating selected models replaces context tiers/).count(), 0);
        assert.equal(await page.locator('.MuiCard-root').count(), 0);
      };

      await t.test('URL fetch reveals modes only after data arrives; URL edits and reopen return to fetch-only', async () => {
        await ready();
        assert.deepEqual(requests, []);
        await page.locator('.MuiInputBase-root button').click();
        await waitPreview();
        assert.equal(await mode('Add Only').isVisible(), true);
        assert.equal(await mode('Update Only').isVisible(), true);
        assert.equal(await mode('Overwrite All').isVisible(), true);
        await page.getByRole('textbox').fill('/changed-catalog');
        await expectFetchOnly();
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await waitPreview();
        await page.getByRole('button', { name: 'Cancel', exact: true }).click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        await page.getByRole('button', { name: 'Open sync' }).click();
        await expectFetchOnly();
      });

      await t.test('fetch immediately previews; every existing mode and diff renderer works for both sources', async () => {
        await ready();
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await waitPreview();
        assert.deepEqual(requests, [{ kind: 'preview', mode: 'overwrite', source }]);
        assert.equal(await page.getByRole('checkbox').count(), 0);
        assert.equal(await page.getByRole('combobox').count(), 0);
        await page.getByText(/Skipped 2 models/).waitFor();
        assert.equal(await page.getByRole('textbox').inputValue(), '/catalog');
        const modelsDevDiff = await page.locator('.MuiCard-root').last().textContent();
        for (const [name, value] of [
          ['Add Only', 'add'],
          ['Update Only', 'update'],
          ['Overwrite All', 'overwrite']
        ]) {
          await mode(name).click();
          await waitPreview();
          assert.deepEqual(requests.at(-1), { kind: 'preview', mode: value, source });
          assert.equal(await apply().textContent(), name);
        }
        // The URL icon must use the URL source, not treat its click event as a models.dev flag.
        await page.locator('.MuiInputBase-root button').click();
        await waitPreview();
        assert.equal(await page.getByText(/Skipped 2 models/).count(), 0);
        assert.equal(await page.locator('.MuiCard-root').last().textContent(), modelsDevDiff);
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await waitPreview();
        await mode('Add Only').click();
        await waitPreview();
        assert.equal(
          requests.some((r) => r.kind === 'apply'),
          false
        );
        await apply().click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        assert.deepEqual(requests.at(-1), { kind: 'apply', mode: 'add', source, base_version: 7, digest: 'digest-add' });
      });

      await t.test('empty and failed catalogs clear old previews and cannot apply', async () => {
        await ready();
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await waitPreview();
        modelsDevData = { prices: [], skipped: 3 };
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await page.getByText(/Skipped 3 models/).waitFor();
        await expectFetchOnly();
        assert.equal(requests.length, 1);
        modelsDevData = { candidates: [] };
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await page.waitForFunction(() => !document.querySelector('.MuiInputBase-root input').disabled);
        await expectFetchOnly();
        assert.equal(requests.length, 1);
      });

      await t.test('preview failure can retry in the same mode without fetching the source again', async () => {
        await ready();
        failPreview = true;
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        const retry = page.locator('.MuiButton-root').filter({ hasText: /^Fetch Data$/ });
        await retry.waitFor();
        await page.waitForFunction(() => !document.querySelector('.MuiInputBase-root input').disabled);
        assert.equal(await apply().isDisabled(), true);
        await mode('Update Only').click();
        await page.waitForFunction(() => !document.querySelector('.MuiInputBase-root input').disabled);
        assert.deepEqual(requests.at(-1), { kind: 'preview', mode: 'update', source });
        failPreview = false;
        await retry.click();
        await waitPreview();
        assert.deepEqual(requests.at(-1), { kind: 'preview', mode: 'update', source });
      });

      await t.test('closing during fetch invalidates the catalog before reopening', async () => {
        await ready();
        holdCatalog = new Promise((resolve) => {
          releaseCatalog = resolve;
        });
        const fetched = page.waitForRequest('**/api/prices/modelsdev');
        await page.getByRole('button', { name: 'Fetch from models.dev' }).click();
        await fetched;
        await expectFetchOnly();
        await page.getByRole('button', { name: 'Cancel', exact: true }).click();
        await page.getByRole('dialog').waitFor({ state: 'hidden' });
        const received = page.waitForResponse('**/api/prices/modelsdev');
        releaseCatalog();
        await received;
        await page.getByRole('button', { name: 'Open sync' }).click();
        await expectFetchOnly();
        assert.deepEqual(requests, []);
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
