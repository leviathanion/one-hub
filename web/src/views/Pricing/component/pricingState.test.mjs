import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import {
  addExtraRatio,
  getAddedExtraRatioConfigs,
  getAvailableExtraRatioConfigs,
  getExtraRatioLabel,
  removeExtraRatio,
  updateExtraRatio
} from './extraRatiosState.mjs';
import { stablePricingJson } from './pricingComparison.mjs';
import { createPricingFetchController } from './pricingFetchState.mjs';

const configSource = await readFile(new URL('./config.js', import.meta.url), 'utf8');
const { extraRatiosConfig } = await import(`data:text/javascript;charset=utf-8,${encodeURIComponent(configSource)}`);

test('价格策略分组键不受对象键顺序影响', () => {
  assert.equal(stablePricingJson({ b: 2, a: { y: 2, x: 1 } }), stablePricingJson({ a: { x: 1, y: 2 }, b: 2 }));
});

test('cache_write_tokens 与 cache_creation_input_tokens 是两个独立的扩展倍率配置', () => {
  const keys = extraRatiosConfig.map(({ key }) => key);

  assert.ok(keys.includes('cache_write_tokens'));
  assert.ok(keys.includes('cache_creation_input_tokens'));
  assert.equal(new Set(keys).size, keys.length);
  assert.equal(
    extraRatiosConfig.some(({ name }) => name !== undefined),
    false
  );
});

test('cache_write_tokens 可新增、查看、编辑和删除', () => {
  const original = {};
  const added = addExtraRatio(original, 'cache_write_tokens');
  assert.deepEqual(added, { cache_write_tokens: 1 });
  assert.equal(
    getAddedExtraRatioConfigs(extraRatiosConfig, added).some(({ key }) => key === 'cache_write_tokens'),
    true
  );
  assert.equal(
    getAvailableExtraRatioConfigs(extraRatiosConfig, added).some(({ key }) => key === 'cache_write_tokens'),
    false
  );

  const updated = updateExtraRatio(added, 'cache_write_tokens', '1.25');
  assert.deepEqual(updated, { cache_write_tokens: '1.25' });

  const removed = removeExtraRatio(updated, 'cache_write_tokens');
  assert.deepEqual(removed, {});
  assert.deepEqual(original, {});
});

test('input_audio_transcription 可新增、查看、编辑和删除，并保留未知扩展倍率', () => {
  const original = { future_usage: 9 };
  const added = addExtraRatio(original, 'input_audio_transcription');
  assert.deepEqual(added, { future_usage: 9, input_audio_transcription: 1 });
  assert.equal(
    getAddedExtraRatioConfigs(extraRatiosConfig, added).some(({ key }) => key === 'input_audio_transcription'),
    true
  );
  assert.equal(
    getAvailableExtraRatioConfigs(extraRatiosConfig, added).some(({ key }) => key === 'input_audio_transcription'),
    false
  );

  const updated = updateExtraRatio(added, 'input_audio_transcription', '4');
  assert.deepEqual(updated, { future_usage: 9, input_audio_transcription: '4' });
  const removed = removeExtraRatio(updated, 'input_audio_transcription');
  assert.deepEqual(removed, { future_usage: 9 });
  assert.deepEqual(original, { future_usage: 9 });
});

test('扩展倍率在编辑器和价格列表共用本地化名称规则', () => {
  const translations = {
    'modelpricePage.input_audio_transcription': '输入音频转写倍率'
  };
  const t = (key, options) => translations[key] || options.defaultValue;

  assert.equal(getExtraRatioLabel(t, 'input_audio_transcription'), '输入音频转写倍率');
  assert.equal(getExtraRatioLabel(t, 'future_usage'), 'future_usage');
});

test('价格拉取只接受最新请求，URL 变化会使在途响应失效', () => {
  const controller = createPricingFetchController();
  const first = controller.begin();
  const second = controller.begin();

  assert.equal(controller.accept(first, [{ model: 'stale' }]), null);
  assert.deepEqual(controller.accept(second, [{ model: 'current' }]), [{ model: 'current' }]);

  controller.invalidate();
  assert.equal(controller.accept(second, [{ model: 'stale-after-url-change' }]), null);
  assert.equal(controller.accept(controller.begin(), { data: 'invalid' }), null);
});

test('默认 URL 初始化响应在用户编辑后失效', () => {
  const controller = createPricingFetchController();
  const initialization = controller.begin();
  controller.invalidate();

  assert.equal(controller.isCurrent(initialization), false);
});
