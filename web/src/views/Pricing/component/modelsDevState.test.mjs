import assert from 'node:assert/strict';
import test from 'node:test';
import { selectModelsDevCandidate, modelsDevSelectedSource, modelsDevSelectionKey, ordinaryPricingMode } from './modelsDevState.mjs';
test('provider conflict requires explicit choice; selected catalog contains one price per model', () => {
  const a = { provider: 'a', model: 'same', price: { model: 'same', input: 2, output: 4 } };
  const b = { provider: 'b', model: 'same', price: { model: 'same', input: 0, output: 0 } };
  let selected = selectModelsDevCandidate({}, a, true);
  assert.deepEqual(modelsDevSelectedSource(selected), [a.price]);
  selected = selectModelsDevCandidate(selected, b, true);
  assert.deepEqual(modelsDevSelectedSource(selected), [b.price]);
  selected = selectModelsDevCandidate(selected, a, false);
  assert.deepEqual(modelsDevSelectedSource(selected), [b.price]);
  assert.deepEqual(modelsDevSelectedSource(selectModelsDevCandidate(selected, b, false)), []);
  assert.notEqual(modelsDevSelectionKey(a), modelsDevSelectionKey(b));
});
test('missing prices are disabled while legitimate zero remains selectable', () => {
  assert.deepEqual(selectModelsDevCandidate({}, { model: 'missing', provider: 'p', reason: 'missing output' }, true), {});
  const zero = { model: 'zero', provider: 'p', price: { model: 'zero', input: 0, output: 0 } };
  assert.deepEqual(modelsDevSelectedSource(selectModelsDevCandidate({}, zero, true)), [zero.price]);
});

test('ordinary catalog mode restores overwrite after models.dev and preserves explicit ordinary choices', () => {
  assert.equal(ordinaryPricingMode('merge'), 'overwrite');
  for (const mode of ['add', 'update', 'overwrite']) assert.equal(ordinaryPricingMode(mode), mode);
});
