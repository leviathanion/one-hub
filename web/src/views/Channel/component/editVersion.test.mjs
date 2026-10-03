import test from 'node:test';
import assert from 'node:assert/strict';
import { versionAfterCredentialSave } from './editVersion.mjs';

const baseline = { id: 1, version: 0, key: 'old', name: 'original', other: '{}', plugin: { b: 2, a: 1 }, used_quota: 1 };

test('credential save can advance form version without adopting telemetry or resetting draft', () => {
  const latest = { ...baseline, version: 2, key: 'authorized', used_quota: 100, plugin: { a: 1, b: 2 } };
  assert.equal(versionAfterCredentialSave(baseline, latest, 'authorized'), 2);
  assert.equal(baseline.version, 0);
});

test('concurrent configuration or credential changes must leave the form stale', () => {
  for (const patch of [{ name: 'concurrent' }, { other: '{"new":true}' }, { key: 'later-rotation' }, { id: 2 }]) {
    assert.equal(versionAfterCredentialSave(baseline, { ...baseline, key: 'authorized', version: 3, ...patch }, 'authorized'), undefined);
  }
});
