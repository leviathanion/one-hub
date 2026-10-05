import assert from 'node:assert/strict';
import test from 'node:test';
import { pricingSyncFailure } from './pricingSyncFeedback.mjs';
const t = (key, params) => ({ key, params });
test('invalid sources explain their model and never offer pointless preview retry', () => {
  for (const code of ['duplicate_price_model', 'invalid_price_catalog']) {
    const feedback = pricingSyncFailure({ data: { code, model: 'm', message: 'bad source' } }, 'preview', t);
    assert.equal(feedback.invalidCatalog, true);
    assert.equal(feedback.retry, false);
  }
});
test('transient preview failures can retry while deterministic rejections cannot', () => {
  for (const status of [429, 500, 503]) assert.equal(pricingSyncFailure({ response: { status } }, 'preview', t).retry, true);
  for (const status of [400, 401, 403]) assert.equal(pricingSyncFailure({ response: { status } }, 'preview', t).retry, false);
  assert.equal(pricingSyncFailure({ data: { success: false, message: 'invalid' } }, 'preview', t).retry, false);
});
test('stale previews are a known rejection; ambiguous apply failures require review without replay', () => {
  assert.equal(pricingSyncFailure({ response: { status: 409 } }, 'apply', t).message.key, 'pricingSync.stalePreview');
  assert.equal(pricingSyncFailure(new Error('network'), 'apply', t).message.key, 'pricingSync.applyFailed');
  assert.equal(pricingSyncFailure(new Error('network'), 'apply', t).retry, true);
});

test('a source HTTP conflict is a fetch error, not an expired local preview', () => {
  const feedback = pricingSyncFailure({ response: { status: 409, data: { message: 'source conflict' } } }, 'fetch', t);
  assert.equal(feedback.message.key, 'pricingSync.fetchFailed');
  assert.equal(feedback.retry, false);
});
