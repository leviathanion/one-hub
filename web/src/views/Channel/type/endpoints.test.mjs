import assert from 'node:assert/strict';
import test from 'node:test';
import { createEndpointPreset, previewEndpoint, updateEndpoint } from './endpoints.mjs';

const definitions = [
  { id: 'openai.responses', default_path: '/v1/responses', default_enabled: true },
  { id: 'anthropic.messages', default_path: '/v1/messages', default_enabled: false }
];

test('新建显式写入预设，已有缺失接口在编辑时保持关闭', () => {
  assert.deepEqual(createEndpointPreset(definitions), {
    'openai.responses': { enabled: true, upstream_url: '' },
    'anthropic.messages': { enabled: false, upstream_url: '' }
  });
  assert.deepEqual(updateEndpoint({}, 'openai.responses', { upstream_url: '/custom' }).endpoints['openai.responses'], {
    enabled: false,
    upstream_url: '/custom'
  });
});

test('关闭并序列化保存后保留原始地址，再次开启恢复；修改不污染原表单', () => {
  const original = {
    vendor: { untouched: true },
    endpoints: { 'openai.responses': { enabled: true, upstream_url: ' /tenant%2Fa/responses?k=1&k=2 ' } }
  };
  const disabled = updateEndpoint(original, 'openai.responses', { enabled: false });
  const saved = JSON.parse(JSON.stringify(disabled));
  assert.equal(saved.endpoints['openai.responses'].upstream_url, original.endpoints['openai.responses'].upstream_url);
  assert.equal(saved.endpoints['openai.responses'].enabled, false);
  assert.equal(original.endpoints['openai.responses'].enabled, true);
  assert.deepEqual(updateEndpoint(saved, 'openai.responses', { enabled: true }), original);
  assert.equal(disabled.endpoints.openai, undefined);
});

test('预览使用地址拼接，保留路径前缀、转义、查询及完整地址覆盖', () => {
  const definition = definitions[1];
  assert.equal(previewEndpoint('https://example.com/root/', {}, definition), 'https://example.com/root/v1/messages');
  assert.equal(
    previewEndpoint('https://example.com/root//', { upstream_url: '/tenant%2Fa?k=1&k=2' }, definition),
    'https://example.com/root//tenant%2Fa?k=1&k=2'
  );
  assert.equal(
    previewEndpoint('https://gateway.ai.cloudflare.com/root', {}, definition),
    'https://gateway.ai.cloudflare.com/root/messages'
  );
  assert.equal(
    previewEndpoint('https://example.com', { upstream_url: 'https://claude.example/api' }, definition),
    'https://claude.example/api'
  );
  assert.equal(previewEndpoint('', {}, definition), '');
});
