import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./other.js', import.meta.url), 'utf8');
const { isSelfHostedResponsesWSEnabled, normalizeChannelOtherForRequest, normalizeOpenAICompatibleOtherForRequest } = await import(
  `data:text/javascript;charset=utf-8,${encodeURIComponent(source)}`
);
const configSource = await readFile(new URL('./Config.js', import.meta.url), 'utf8');
const { typeConfig } = await import(`data:text/javascript;charset=utf-8,${encodeURIComponent(configSource)}`);

test('所有依赖 Other(JSON) 的渠道类型都会显示输入框', () => {
  const actual = Object.entries(typeConfig)
    .filter(([, config]) => config.fields?.other === true)
    .map(([type]) => Number(type))
    .sort((left, right) => left - right);

  assert.deepEqual(actual, [1, 3, 8, 17, 18, 24, 25, 42, 55, 101]);
});

test('自建 Responses WS 风险提示只读取 channel.other JSON', () => {
  assert.equal(isSelfHostedResponsesWSEnabled('{"responses_ws_self_hosted":true}'), true);
  assert.equal(isSelfHostedResponsesWSEnabled('{"self_hosted":true}'), false);
  assert.equal(isSelfHostedResponsesWSEnabled('{"responses_ws_self_hosted":false}'), false);
});

test('channel.other 是 Responses WS 配置的唯一所有者', () => {
  const originalOther = '{ "responses_ws_native": true, "responses_ws_self_hosted": false, "vendor_extra": {"owner":"ops"} }';
  const values = normalizeChannelOtherForRequest({
    type: 8,
    other: originalOther,
    responses_ws_native: false,
    responses_ws_self_hosted: true
  });

  assert.equal('responses_ws_native' in values, false);
  assert.equal('responses_ws_self_hosted' in values, false);
  assert.equal(values.other, originalOther);
});

test('无效 channel.other JSON 在提交前失败', () => {
  assert.throws(() => normalizeChannelOtherForRequest({ type: 1, other: '{invalid' }), /JSON/);
});

test('Azure 空配置补入默认 API 版本', () => {
  const values = normalizeChannelOtherForRequest({
    type: 3,
    other: '',
    responses_ws_native: false,
    responses_ws_self_hosted: true
  });

  assert.deepEqual(JSON.parse(values.other), {
    api_version: '2024-05-01-preview'
  });
});

test('临时 OpenAI 兼容请求也不发送顶层虚拟字段', () => {
  const values = normalizeOpenAICompatibleOtherForRequest({
    other: '{"responses_ws_native":true}',
    responses_ws_native: true,
    responses_ws_self_hosted: true
  });

  assert.equal('responses_ws_native' in values, false);
  assert.equal('responses_ws_self_hosted' in values, false);
  assert.deepEqual(JSON.parse(values.other), { responses_ws_native: true });
});
