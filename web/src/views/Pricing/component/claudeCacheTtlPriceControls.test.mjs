import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import {
  addExtraRatio,
  getAddedExtraRatioConfigs,
  getExtraRatioLabel,
  removeExtraRatio
} from './extraRatiosState.mjs';
import { buildManualPriceRequest } from './manualPriceRequest.mjs';

const configSource = await readFile(new URL('./config.js', import.meta.url), 'utf8');
const { extraRatiosConfig } = await import(`data:text/javascript;charset=utf-8,${encodeURIComponent(configSource)}`);
const zhCN = JSON.parse(await readFile(new URL('../../../i18n/locales/zh_CN.json', import.meta.url), 'utf8'));

const ttlKeys = ['ephemeral_5m_input_tokens', 'ephemeral_1h_input_tokens'];

test('Claude TTL 倍率配置与通用缓存写入键独立且保留输入方向', () => {
  const keys = extraRatiosConfig.map(({ key }) => key);
  assert.equal(new Set(keys).size, keys.length);
  assert.ok(keys.includes('cache_creation_input_tokens'));
  for (const key of ttlKeys) {
    const option = extraRatiosConfig.find((item) => item.key === key);
    assert.ok(option, `缺少 ${key} 配置`);
    assert.equal(option.isPrompt, true);
  }
});

test('保存与删除 TTL 覆盖不会自动写入不存在的 TTL 键', () => {
  const original = { cache_creation_input_tokens: 0 };
  const withTTL = addExtraRatio(original, ttlKeys[0]);
  assert.deepEqual(withTTL, { cache_creation_input_tokens: 0, [ttlKeys[0]]: 1 });
  assert.equal(getAddedExtraRatioConfigs(extraRatiosConfig, withTTL).some(({ key }) => key === ttlKeys[0]), true);

  const removed = removeExtraRatio(withTTL, ttlKeys[0]);
  assert.deepEqual(removed, original);
  assert.deepEqual(original, { cache_creation_input_tokens: 0 });

  const request = buildManualPriceRequest(
    {
      model: 'claude-i030',
      type: 'tokens',
      channel_type: 1,
      input: 1,
      output: 1,
      locked: false,
      extra_ratios: original
    },
    7
  );
  assert.deepEqual(request.extra_ratios, original);
  for (const key of ttlKeys) {
    assert.equal(Object.hasOwn(request.extra_ratios, key), false);
  }
});

test('删除 TTL 覆盖后标签仍说明通用倍率回退关系', () => {
  const t = (key, options) => zhCN.modelpricePage[key.replace('modelpricePage.', '')] || options.defaultValue;
  assert.equal(getExtraRatioLabel(t, ttlKeys[0]), 'Claude 5 分钟缓存写入倍率（未设置时回退到 cache_creation_input_tokens）');
  assert.equal(getExtraRatioLabel(t, ttlKeys[1]), 'Claude 1 小时缓存写入倍率（未设置时回退到 cache_creation_input_tokens）');
});
