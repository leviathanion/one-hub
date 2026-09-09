import assert from 'node:assert/strict';
import test from 'node:test';
import {
  createPricingDraft,
  isPricingConflict,
  markPricingDraftStale,
  recoverPricingConflict
} from './pricingMutationRecovery.mjs';

test('price conflict marks a draft stale without implicitly reloading it', async () => {
  let reloads = 0;
  let stale = false;
  assert.equal(
    await recoverPricingConflict({ response: { status: 409 } }, {
      onStale: () => {
        stale = true;
        reloads += 1;
      }
    }),
    true
  );
  assert.equal(stale, true);
  assert.equal(reloads, 1);
  assert.equal(await recoverPricingConflict({ response: { status: 500 } }, { onStale: () => reloads++ }), false);
  assert.equal(reloads, 1);
});

test('a draft keeps its original value and base version until explicitly rebuilt', () => {
  const original = { model: 'gpt-5', input: 1, output: 2 };
  const draft = createPricingDraft(original, 7);
  original.output = 99;
  assert.deepEqual(draft, {
    baseVersion: 7,
    original: { model: 'gpt-5', input: 1, output: 2 },
    stale: false
  });
  assert.equal(markPricingDraftStale(draft).stale, true);
  assert.equal(isPricingConflict({ response: { status: 409 } }), true);
  assert.equal(isPricingConflict({ response: { status: 400 } }), false);
});
