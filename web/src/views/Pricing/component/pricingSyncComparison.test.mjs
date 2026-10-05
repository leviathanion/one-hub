import assert from 'node:assert/strict';
import test from 'node:test';
import { pricingSyncRows } from './pricingSyncComparison.mjs';
const t = (key, options) => `${key}${options?.range || ''}`;
const rows = (before, after) => pricingSyncRows(before, after, t, String);

test('zero values, removed extras and unknown meters remain visible', () => {
  const changes = rows(
    { input: 1, extra_ratios: { cached_tokens: 0.5, old_meter: 2 } },
    { input: 0, extra_ratios: { cached_tokens: 0, future_meter: 3 } }
  );
  assert.equal(changes.find((r) => r.key === 'input').after, '0');
  assert.deepEqual(
    changes.filter((r) => r.key.startsWith('extra:')).map((r) => [r.key, r.before, r.after]),
    [
      ['extra:cached_tokens', '0.5', '0'],
      ['extra:future_meter', '', '3'],
      ['extra:old_meter', '2', '']
    ]
  );
});
test('empty extra and rule configuration changes are visible even alongside base price changes', () => {
  const changes = rows({ input: 1, extra_ratios: null, rate_rules: null }, { input: 2, extra_ratios: {}, rate_rules: {} });
  assert.deepEqual(
    changes.map((r) => r.key),
    ['input', 'extras', 'rules']
  );
  assert.equal(changes.find((r) => r.key === 'rules').after, 'pricingSync.emptyRules');
  assert.equal(rows({ extra_ratios: null }, { extra_ratios: {} })[0].key, 'extras');
});
test('all condition dimensions, cache factors, timezone and removed rules have readable changes', () => {
  const rules = {
    version: 2,
    service_tier: [{ id: 'tier', when: { service_tier: ['flex'] }, multipliers: { all: 0.5 } }],
    speed: [{ id: 'speed', when: { speed: ['fast'] }, multipliers: { input: 2 } }],
    long_context: [
      {
        id: 'context',
        when: { input_tokens: { gt: 100, lte: 200 }, speed: ['fast'] },
        multipliers: { input: 2, output: 3, extra_multipliers: { cached_tokens: 4 } }
      }
    ],
    schedule: {
      timezone: 'Asia/Shanghai',
      rules: [{ id: 'night', when: { weekdays: [1], time: { start: '23:00', end: '07:00' } }, multipliers: { all: 0 } }]
    }
  };
  const changes = rows({ rate_rules: rules }, { rate_rules: {} });
  assert.equal(changes.length, 4);
  const text = changes.map((r) => r.before).join('\n');
  for (const expected of ['flex', 'fast', '> 100', '≤ 200', '4×', 'Asia/Shanghai', '23:00–07:00', '0×'])
    assert.ok(text.includes(expected), expected);
  assert.ok(changes.every((r) => r.after === ''));
  assert.ok(!text.includes('"multipliers"'));
});
test('rule order changes and timezone-only changes remain visible', () => {
  const a = { id: 'a', when: {}, multipliers: { all: 1 } };
  const b = { id: 'b', when: {}, multipliers: { all: 2 } };
  assert.equal(
    rows(
      { rate_rules: { schedule: { timezone: 'UTC', rules: [a, b] } } },
      { rate_rules: { schedule: { timezone: 'Asia/Shanghai', rules: [b, a] } } }
    )[0].key,
    'rule:schedule'
  );
});
