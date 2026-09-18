import assert from 'node:assert/strict';
import test from 'node:test';
import {
  calculatePrice,
  calculateQuotaDetail,
  calculateTokenBreakdown,
  getBasePriceRatio,
  getBillingCostRows,
  getTokenBillingDetails
} from './quotaDetail.js';

function recordedLog() {
  return {
    quota: 1360230,
    prompt_tokens: 272001,
    completion_tokens: 10,
    metadata: {
      price_type: 'tokens',
      group_ratio: 0.5,
      input_ratio: 10,
      output_ratio: 45,
      billing_input_multiplier: 4,
      billing_output_multiplier: 3,
      effective_service_tier: 'fast',
      token_billing: {
        status: 'priceable',
        base_input_ratio: 2.5,
        base_output_ratio: 15,
        input_units: 272001,
        output_units: 10,
        charge: 1360230,
        rules: [
          { kind: 'long_context', input_threshold: 272000, input_multiplier: 2, output_multiplier: 1.5 },
          { kind: 'fast_priority', input_multiplier: 2, output_multiplier: 2 }
        ]
      }
    }
  };
}

test('Long-context and fast detail does not multiply already-effective rates twice', () => {
  const log = recordedLog();
  const before = structuredClone(log);
  const detail = calculateQuotaDetail(log);
  assert.equal(detail.computedActualQuota, 1360230);
  assert.equal(detail.actualQuota, log.quota);
  assert.equal(detail.originalQuota, 2720460);
  assert.deepEqual(
    getTokenBillingDetails(log.metadata).rules.map((rule) => rule.kind),
    ['long_context', 'fast_priority']
  );
  assert.deepEqual(log, before);
});

test('Historical logs do not invent rule breakdown from aggregate multipliers', () => {
  const log = recordedLog();
  delete log.metadata.token_billing;
  assert.equal(getTokenBillingDetails(log.metadata), null);
  assert.equal(calculateQuotaDetail(log).actualQuota, log.quota);
  assert.equal(log.metadata.billing_input_multiplier, 4);
});

test('Persisted zero charge always wins over a positive reference calculation', () => {
  for (const priceType of ['tokens', 'times']) {
    const log = {
      quota: 0,
      prompt_tokens: 100,
      completion_tokens: 10,
      metadata: { price_type: priceType, input_ratio: 1, output_ratio: 2 }
    };
    const detail = calculateQuotaDetail(log);
    assert.ok(detail.computedActualQuota > 0);
    assert.equal(detail.actualQuota, 0);
  }
});

test('Token calculations use logged fractional units instead of dropping cache fractions', () => {
  const log = recordedLog();
  log.prompt_tokens = 10;
  log.completion_tokens = 3;
  Object.assign(log.metadata, { cached_tokens: 9, cached_tokens_ratio: 0.1, input_ratio: 5, output_ratio: 30, group_ratio: 1 });
  Object.assign(log.metadata.token_billing, { input_units: 1.9000000000000004, output_units: 3, charge: 100 });
  log.quota = 100;
  const breakdown = calculateTokenBreakdown(log);
  assert.equal(breakdown.totalInputTokens, log.metadata.token_billing.input_units);
  assert.ok(Math.abs(breakdown.tokenDetails[0].tokens + 8.1) < 1e-12);
  assert.equal(calculateQuotaDetail(log).actualQuota, 100);
  assert.equal(calculateQuotaDetail(log).computedActualQuota, 100);
});

test('缓存证据明细只渲染新键', () => {
  const newKeys = calculateTokenBreakdown({
    prompt_tokens: 10,
    completion_tokens: 0,
    metadata: {
      cache_creation_input_tokens: 4,
      cache_creation_input_tokens_ratio: 2,
      cache_read_input_tokens: 3,
      cache_read_input_tokens_ratio: 0.1
    }
  });
  assert.deepEqual(
    newKeys.tokenDetails.map(({ key }) => key),
    ['cache_creation_input_tokens', 'cache_read_input_tokens']
  );
});

test('Missing or conflicting token evidence is not shown as a token charge in tool-only logs', () => {
  const log = recordedLog();
  Object.assign(log.metadata.token_billing, { status: 'conflicting_evidence', rules: [], input_units: 0, output_units: 0, charge: 0 });
  log.quota = 5000;
  const detail = calculateQuotaDetail(log);
  assert.equal(detail.actualInputQuota, 0);
  assert.equal(detail.actualOutputQuota, 0);
  assert.equal(detail.actualQuota, 5000);
});

test('Empty rules and zero prices are recorded facts, while incomplete details remain unknown', () => {
  const log = recordedLog();
  Object.assign(log.metadata.token_billing, { base_input_ratio: 0, base_output_ratio: 0, charge: 0, rules: [] });
  assert.notEqual(getTokenBillingDetails(log.metadata), null);
  assert.equal(getTokenBillingDetails({ token_billing: {} }), null);
  log.metadata.token_billing.rules = [null];
  assert.equal(getTokenBillingDetails(log.metadata), null);
});

test('Displayed unit prices use the same configurable quota conversion as displayed charges', (t) => {
  const originalWindow = globalThis.window;
  t.after(() => {
    if (originalWindow === undefined) delete globalThis.window;
    else globalThis.window = originalWindow;
  });
  globalThis.window = { localStorage: { getItem: () => '500000' } };
  assert.equal(calculatePrice(2.5, 1, false), '5');
  assert.equal(calculatePrice(2.5, 1, true), '0.005');
  globalThis.window = { localStorage: { getItem: () => '1000000' } };
  assert.equal(calculatePrice(2.5, 1, false), '2.5');
  assert.equal(calculatePrice(2.5, 0.5, false), '1.25');
  assert.equal(calculatePrice(2.5, 1, true), '0.0025');
  assert.equal(calculatePrice(0, 1, false), '0');
  assert.equal(calculatePrice(2.5, 0, false), '0');
});

test('费用行直接使用已调价的单价，实扣金额不由分项覆盖', () => {
  const item = recordedLog();
  const before = structuredClone(item);
  const rows = getBillingCostRows(item);
  assert.equal(rows[0].count, 272001);
  assert.equal(rows[0].ratio, 5);
  assert.equal(rows[0].baseRatio, 2.5);
  assert.deepEqual(rows[0].multipliers, [2, 2, 0.5]);
  assert.equal(rows[0].amount, 1360005);
  assert.equal(rows[1].ratio, 22.5);
  assert.equal(rows[1].baseRatio, 15);
  assert.deepEqual(rows[1].multipliers, [1.5, 2, 0.5]);
  assert.equal(rows[1].amount, 225);
  assert.deepEqual(item, before);
  item.quota = 0;
  assert.equal(getBillingCostRows(item)[0].amount, 1360005);
  assert.equal(item.quota, 0);
});

test('缓存用量包含在输入中，不另加一遍费用或向用户展示加权用量', () => {
  const item = recordedLog();
  item.prompt_tokens = 10;
  Object.assign(item.metadata, { cached_tokens: 9, cached_tokens_ratio: 0.1, input_ratio: 5, group_ratio: 1 });
  item.metadata.token_billing.input_units = 1.9000000000000004;
  const input = getBillingCostRows(item)[0];
  assert.equal(input.count, 10);
  assert.equal(input.adjustments[0].value, 9);
  assert.equal(input.adjustments[0].ratio, 0.5);
  assert.ok(Math.abs(input.amount - 9.5) < 1e-12);
});

test('旧日志缺少价格或缓存单价时标记未知，不当作免费', () => {
  const item = { prompt_tokens: 10, completion_tokens: 1, metadata: { cached_tokens: 9, input_ratio: 5 } };
  const rows = getBillingCostRows(item);
  assert.equal(rows[0].amount, null);
  assert.equal(rows[0].adjustments[0].ratio, null);
  assert.equal(rows[1].amount, null);
  assert.equal(rows[1].ratio, null);
  assert.equal(getBillingCostRows({ metadata: {} })[0].count, null);
});

test('超出可显示范围的参考金额保持未知，不展示 Infinity 或影响实扣', () => {
  const item = { quota: 100, prompt_tokens: 100, completion_tokens: 0, metadata: { input_ratio: 1e308 } };
  assert.equal(getBillingCostRows(item)[0].amount, null);
  assert.equal(item.quota, 100);
});

test('DeepSeek 与 Claude 缓存用量沿用同一分解，不忽略缓存折扣', () => {
  const rows = getBillingCostRows({
    prompt_tokens: 10,
    completion_tokens: 0,
    metadata: {
      input_ratio: 1,
      output_ratio: 0,
      deepseek_cache_hit_tokens: 9,
      deepseek_cache_hit_tokens_ratio: 0.1
    }
  });
  assert.equal(rows[0].adjustments[0].key, 'deepseek_cache_hit_tokens');
  assert.ok(Math.abs(rows[0].amount - 1.9) < 1e-12);
  const claude = getBillingCostRows({
    prompt_tokens: 10,
    completion_tokens: 0,
    metadata: {
      input_ratio: 1,
      ephemeral_1h_input_tokens: 5,
      ephemeral_1h_input_tokens_ratio: 2
    }
  });
  assert.equal(claude[0].amount, 15);
});

test('工具单价只应用分组优惠，不叠加 long-context 或 fast', () => {
  const item = recordedLog();
  item.metadata.extra_billing = { web_search: { service_type: 'web_search', type: '', price: 0.01, call_count: 2 } };
  const tool = getBillingCostRows(item)[2];
  assert.equal(tool.label, 'webSearch');
  assert.equal(tool.count, 2);
  assert.equal(tool.ratio, 0.005);
  assert.equal(tool.baseRatio, 0.01);
  assert.deepEqual(tool.multipliers, [0.5]);
  assert.equal(tool.amount, 5000);
  item.metadata.group_ratio = 0;
  assert.equal(getBillingCostRows(item)[2].amount, 0);
});

test('仅工具收费不将冲突 Token 用量显示为收费；按次日志不猜测调用次数', () => {
  const item = recordedLog();
  item.metadata.token_billing.status = 'conflicting_evidence';
  const input = getBillingCostRows(item)[0];
  assert.equal(input.notCharged, true);
  assert.equal(input.amount, 0);
  assert.deepEqual(input.adjustments, []);
  item.metadata.price_type = 'times';
  assert.equal(getBillingCostRows(item)[0].count, null);
  assert.equal(getBillingCostRows(item)[0].amount, null);
});

test('原价只读取日志中的基础价，不乘分组优惠或倒推缺失价格', () => {
  const item = recordedLog();
  assert.equal(getBasePriceRatio(item.metadata, 'input'), 2.5);
  item.metadata.group_ratio = 0;
  assert.equal(getBasePriceRatio(item.metadata, 'input'), 2.5);
  item.metadata.token_billing.base_input_ratio = 0;
  assert.equal(getBasePriceRatio(item.metadata, 'input'), 0);
  delete item.metadata.token_billing;
  assert.equal(getBasePriceRatio(item.metadata, 'input'), null);
  assert.equal(getBasePriceRatio({ price_type: 'times', input_ratio: 2.5, group_ratio: 0.5 }, 'input'), 2.5);
  assert.equal(getBasePriceRatio({ price_type: 'times', input_ratio: 2.5 }, 'output'), null);
});

test('新规则的加权单位已包含倍率；输入免费仍可解释缓存收费', () => {
  const item = {
    quota: 20, prompt_tokens: 100, completion_tokens: 10,
    metadata: {
      price_type: 'tokens', group_ratio: 1, input_ratio: 0, output_ratio: 0,
      cached_tokens: 80, cached_tokens_ratio: 0.1,
      effective_extra_ratios: { cached_tokens: 0.25 },
      token_billing: {
        units_include_rules: true, status: 'priceable', base_input_ratio: 2.5, base_output_ratio: 15,
        input_units: 8, output_units: 0, charge: 20,
        rules: [{ kind: 'schedule', id: 'free', input_multiplier: 0, output_multiplier: 0, extra_multipliers: { cached_tokens: 1 } }]
      }
    }
  };
  const details = calculateQuotaDetail(item);
  assert.equal(details.computedActualQuota, 20);
  const rows = getBillingCostRows(item);
  assert.equal(rows[0].amount, 20);
  assert.equal(rows[0].adjustments[0].ratio, 0.25);
});
