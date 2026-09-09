import assert from 'node:assert/strict';
import test from 'node:test';

import { buildManualPriceRequest } from './manualPriceRequest.mjs';

test('manual price request strips view-only fields and keeps the CAS version', () => {
  const request = buildManualPriceRequest(
    {
      id: 7,
      isNew: false,
      model_info: { name: 'view only' },
      model: 'gpt-5',
      type: 'tokens',
      channel_type: 1,
      input: 2,
      output: 10,
      locked: true,
      extra_ratios: { cached_tokens: 0.1 },
      rate_rules: {}
    },
    12
  );
  assert.deepEqual(request, {
    expected_version: 12,
    model: 'gpt-5',
    type: 'tokens',
    channel_type: 1,
    input: 2,
    output: 10,
    locked: true,
    extra_ratios: { cached_tokens: 0.1 },
    rate_rules: {}
  });
});

test('manual price request preserves omitted rate_rules', () => {
  const request = buildManualPriceRequest({ model: 'm', type: 'tokens', channel_type: 1, input: 1, output: 2, extra_ratios: {} }, 3);
  assert.equal(Object.hasOwn(request, 'rate_rules'), false);
});
