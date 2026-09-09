import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import {
  DEFAULT_PRICING_UPDATE_URL,
  canAcceptDefaultPricingUrl,
  createPricingFetchController,
  resolveDefaultPricingUrl
} from './pricingFetchState.mjs';

const componentSource = await readFile(new URL('./CheckUpdates.jsx', import.meta.url), 'utf8');

test('关闭态挂载的默认地址响应仍可被接受，打开时已有非空地址可用', () => {
  const defaultController = createPricingFetchController();
  const catalogController = createPricingFetchController();
  const defaultGeneration = defaultController.begin();

  // 关闭态 effect 只会使目录代次失效，不应使默认地址请求失效。
  catalogController.begin();
  catalogController.invalidate();

  assert.equal(
    canAcceptDefaultPricingUrl({
      controller: defaultController,
      generation: defaultGeneration,
      currentUrl: '',
      cachedUrl: '',
      userEdited: false
    }),
    true
  );
  assert.equal(resolveDefaultPricingUrl('https://prices.example.test/catalog.json'), 'https://prices.example.test/catalog.json');
  assert.notEqual(resolveDefaultPricingUrl('https://prices.example.test/catalog.json'), '');
});

test('默认地址失败保留既定 fallback，空响应也不会留下空 URL', () => {
  assert.equal(resolveDefaultPricingUrl(), DEFAULT_PRICING_UPDATE_URL);
  assert.equal(resolveDefaultPricingUrl(null), DEFAULT_PRICING_UPDATE_URL);
  assert.equal(resolveDefaultPricingUrl('  '), DEFAULT_PRICING_UPDATE_URL);
});

test('已有缓存或用户手填 URL 时，迟到默认响应不能覆盖当前值', () => {
  const controller = createPricingFetchController();
  const generation = controller.begin();

  assert.equal(
    canAcceptDefaultPricingUrl({
      controller,
      generation,
      currentUrl: '',
      cachedUrl: 'https://cached.example.test/prices.json',
      userEdited: false
    }),
    false
  );
  assert.equal(
    canAcceptDefaultPricingUrl({
      controller,
      generation,
      currentUrl: 'https://typed.example.test/prices.json',
      cachedUrl: 'https://typed.example.test/prices.json',
      userEdited: true
    }),
    false
  );
});

test('快速关闭、重新打开和卸载分别淘汰目录请求，卸载再淘汰默认请求', () => {
  const defaultController = createPricingFetchController();
  const catalogController = createPricingFetchController();
  const defaultGeneration = defaultController.begin();
  const firstCatalogGeneration = catalogController.begin();

  // 关闭：只取消目录/预览；重新打开可建立新目录代次。
  catalogController.invalidate();
  assert.equal(defaultController.isCurrent(defaultGeneration), true);
  assert.equal(catalogController.isCurrent(firstCatalogGeneration), false);

  const reopenedCatalogGeneration = catalogController.begin();
  assert.equal(catalogController.isCurrent(reopenedCatalogGeneration), true);
  assert.equal(catalogController.isCurrent(firstCatalogGeneration), false);

  // 卸载：两个独立工作都失效，迟到响应不能再写入状态。
  defaultController.invalidate();
  catalogController.invalidate();
  assert.equal(defaultController.isCurrent(defaultGeneration), false);
  assert.equal(catalogController.isCurrent(reopenedCatalogGeneration), false);
});

test('目录预览只接受最新代次，旧响应不能重新展示', () => {
  const catalogController = createPricingFetchController();
  const firstGeneration = catalogController.begin();
  const secondGeneration = catalogController.begin();

  assert.equal(catalogController.accept(firstGeneration, [{ model: 'stale' }]), null);
  assert.deepEqual(catalogController.accept(secondGeneration, [{ model: 'current' }]), [{ model: 'current' }]);
});

test('组件保留默认地址和目录两个独立取消控制器', () => {
  assert.match(componentSource, /const defaultUrlController = useRef\(null\)/);
  assert.match(componentSource, /const catalogRequestController = useRef\(null\)/);
  assert.match(componentSource, /defaultUrlController\.current\.begin\(\)/);
  assert.match(componentSource, /catalogRequestController\.current\.begin\(\)/);

  const closeEffect = componentSource.match(/useEffect\(\(\) => \{\n    if \(!open\) \{[\s\S]*?\n  \}, \[open\]\);/);
  assert.ok(closeEffect, '关闭态目录 effect 应存在');
  assert.match(closeEffect[0], /catalogRequestController\.current\.invalidate\(\)/);
  assert.doesNotMatch(closeEffect[0], /defaultUrlController\.current\.invalidate\(\)/);
});
