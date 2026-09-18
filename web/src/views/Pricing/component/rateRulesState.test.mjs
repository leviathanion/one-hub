import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

import { rateRulesApplyToBillingType, rateRulesPayload, validateRateRules } from './rateRulesState.mjs';

const editModalSource = await readFile(new URL('./EditModal.jsx', import.meta.url), 'utf8');
const priceCardSource = await readFile(new URL('./PriceCard.jsx', import.meta.url), 'utf8');
const tableRowSource = await readFile(new URL('./TableRow.jsx', import.meta.url), 'utf8');
const chipsSource = await readFile(new URL('./RateRulesChips.jsx', import.meta.url), 'utf8');

test('single-price edits serialize removal of the last rule as an explicit empty object', () => {
  assert.deepEqual(rateRulesPayload({}, true), {});
});

test('optional batch policy remains omitted when no rule was selected', () => {
  assert.equal(rateRulesPayload({}, false), undefined);
});

test('配置数值规范化但不写入继承字段，显式零保留', () => {
  const draft = {
    version: 2,
    schedule: { rules: [{ id: 'night', when: {}, multipliers: { all: '0.5', extra_multipliers: { cache_read_input_tokens: '0' } } }] }
  };
  const result = rateRulesPayload(draft, true);
  assert.equal(result.schedule.rules[0].multipliers.all, 0.5);
  assert.equal(result.schedule.rules[0].multipliers.extra_multipliers.cache_read_input_tokens, 0);
  assert.equal(Object.hasOwn(result.schedule.rules[0].multipliers, 'input'), false);
  assert.equal(draft.schedule.rules[0].multipliers.all, '0.5');
});

test('非法条件、重复 id、负数和缺少时区阻止提交', () => {
  for (const value of [
    { version: 2, schedule: { rules: [{ id: 'x', when: {}, multipliers: { all: -1 } }] } },
    { version: 2, schedule: { rules: [{ id: 'x', when: { weekdays: [6, 7] }, multipliers: { all: 1 } }] } },
    {
      version: 2,
      speed: [
        { id: 'x', when: { speed: ['fast'] }, multipliers: {} },
        { id: 'x', when: {}, multipliers: {} }
      ]
    },
    { version: 2, long_context: [{ id: 'x', when: { input_tokens: { gt: 200, lte: 100 } }, multipliers: {} }] },
    { flex: { input: 0.5, output: 0.5 } }
  ]) {
    assert.ok(Object.keys(validateRateRules(value)).length > 0);
    assert.throws(() => rateRulesPayload(value, true), /invalid values/);
  }
});

test('per-request pricing keeps retained rate rules visible and editable', () => {
  assert.doesNotMatch(editModalSource, /inputs\.type === 'tokens' && renderRateRulesEditor/);
  assert.doesNotMatch(editModalSource, /formProps\.values\.type === 'tokens' && renderRateRulesEditor/);
  assert.match(editModalSource, /inactiveForTimes/);
});

test('rate rules are active only for token pricing and both lists share that presentation', () => {
  assert.equal(rateRulesApplyToBillingType('tokens'), true);
  assert.equal(rateRulesApplyToBillingType('times'), false);
  assert.equal(rateRulesApplyToBillingType('future'), false);
  assert.match(priceCardSource, /<RateRulesChips[^>]+billingType=\{price\.type\}/);
  assert.match(tableRowSource, /<RateRulesChips[^>]+billingType=\{item\.type\}/);
  assert.match(chipsSource, /rateRulesApplyToBillingType\(billingType\)/);
  assert.match(chipsSource, /pricing_edit\.rateRules\.inactive/);
});

test('分组不能混入其他维度；时区只属于时间配置，规则不接受名称', () => {
  for (const value of [
    { version: 2, timezone: 'UTC' },
    { version: 2, service_tier: [{ id: 'x', when: { service_tier: ['flex'], speed: ['fast'] }, multipliers: {} }] },
    { version: 2, speed: [{ id: 'x', name: 'label', when: { speed: ['fast'] }, multipliers: {} }] },
    { version: 2, schedule: { timezone: 'UTC', rules: [{ id: 'x', when: { service_tier: ['flex'] }, multipliers: {} }] } }
  ]) assert.ok(Object.keys(validateRateRules(value)).length > 0);
  const valid = {version:2,schedule:{timezone:'UTC',rules:[{id:'weekend',when:{weekdays:[6,7]},multipliers:{all:0.8}}]}};
  assert.deepEqual(rateRulesPayload(valid,true),valid);
});

test('非时间分组拒绝重叠，边界相接的长度区间不依赖顺序', () => {
  const rule = (id, when) => ({id,when,multipliers:{all:2}});
  assert.equal(validateRateRules({version:2,speed:[rule('a',{speed:['fast']}),rule('b',{speed:['fast']})]})['speed.1.when'],'overlap');
  const ranges = [rule('a',{input_tokens:{gt:100,lte:200}}),rule('b',{input_tokens:{gt:200}})];
  assert.deepEqual(validateRateRules({version:2,long_context:ranges}),{});
  assert.deepEqual(validateRateRules({version:2,long_context:[...ranges].reverse()}),{});
});
