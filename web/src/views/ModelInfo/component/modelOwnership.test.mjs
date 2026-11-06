import assert from 'node:assert/strict';
import test from 'node:test';
import { buildOwnershipBatch, CLEAR_OWNERSHIP, MAX_OWNERSHIP_BATCH, parseOwnershipModels } from './modelOwnership.mjs';

test('粘贴精确模型名去重但保留大小写，拒绝通配和超大批次', () => {
  assert.deepEqual(parseOwnershipModels(' Model-A\r\nmodel-a\nModel-A\n'), ['Model-A', 'model-a']);
  assert.throws(() => parseOwnershipModels('family*'), /通配/);
  assert.throws(() => parseOwnershipModels('a'.repeat(101)), /100/);
  const models = Array.from({ length: MAX_OWNERSHIP_BATCH }, (_, i) => `model-${i}`);
  assert.equal(MAX_OWNERSHIP_BATCH, 5000);
  assert.deepEqual(parseOwnershipModels(models.join('\n')), models);
  assert.throws(() => parseOwnershipModels([...models, 'extra-model'].join('\n')), /5000/);
});

test('手动设置和清空只提交归属，目标必须存在且清空不能只补空缺', () => {
  const base = {
    selected: new Set(['new-model', 'existing']),
    method: 'manual',
    target: '7',
    mode: 'fill_missing',
    owners: { 7: { name: '本地归属' } }
  };
  assert.deepEqual(buildOwnershipBatch(base), {
    items: [
      { model: 'new-model', owned_by_id: 7 },
      { model: 'existing', owned_by_id: 7 }
    ]
  });
  assert.match(buildOwnershipBatch({ ...base, target: '8' }).error, /有效/);
  assert.match(buildOwnershipBatch({ ...base, target: CLEAR_OWNERSHIP }).error, /覆盖/);
  assert.deepEqual(buildOwnershipBatch({ ...base, target: CLEAR_OWNERSHIP, mode: 'replace' }).items, [
    { model: 'new-model', owned_by_id: null },
    { model: 'existing', owned_by_id: null }
  ]);
});

test('来源名称不自动映射，只提交明确映射的模型，其余保留不修改', () => {
  const base = {
    selected: new Set(['a', 'conflict', 'unmatched', 'unqueried', 'unmapped', 'invalid-owner']),
    method: 'source',
    owners: { 7: { name: 'lab-a' } },
    providerOwners: {},
    suggestions: {
      a: { status: 'matched', providers: [{ id: 'lab-a', name: '服务商' }] },
      conflict: {
        status: 'conflict',
        providers: [
          { id: 'lab-a', name: '服务商 A' },
          { id: 'lab-b', name: '服务商 B' }
        ]
      },
      unmatched: { status: 'unmatched', providers: [] },
      unmapped: { status: 'matched', providers: [{ id: 'lab-b', name: '服务商' }] },
      'invalid-owner': { status: 'matched', providers: [{ id: 'lab-invalid', name: '服务商' }] }
    }
  };
  const empty = buildOwnershipBatch(base);
  assert.deepEqual(empty.items, []);
  assert.deepEqual(empty.unresolved, [...base.selected]);
  assert.ok(empty.error);
  const partial = buildOwnershipBatch({ ...base, providerOwners: { 'lab-a': '7', 'lab-invalid': '99' } });
  assert.deepEqual(partial.items, [{ model: 'a', owned_by_id: 7 }]);
  assert.deepEqual(partial.unresolved, ['conflict', 'unmatched', 'unqueried', 'unmapped', 'invalid-owner']);
  assert.equal(partial.error, undefined);
  const complete = buildOwnershipBatch({ ...base, selected: new Set(['a']), providerOwners: { 'lab-a': '7' } });
  assert.deepEqual(complete.items, [{ model: 'a', owned_by_id: 7 }]);
  assert.deepEqual(complete.unresolved, []);
});

test('原型属性不能作为归属建议或来源映射', () => {
  const base = {
    method: 'source',
    owners: { 7: { name: 'lab-a' } },
    providerOwners: {},
    suggestions: {}
  };
  const unknown = buildOwnershipBatch({ ...base, selected: new Set(['constructor', 'toString', '__proto__']) });
  assert.deepEqual(unknown.items, []);
  assert.deepEqual(unknown.unresolved, ['constructor', 'toString', '__proto__']);
  const inheritedProvider = buildOwnershipBatch({
    ...base,
    selected: new Set(['a']),
    suggestions: { a: { status: 'matched', providers: [{ id: 'constructor', name: '服务商' }] } }
  });
  assert.deepEqual(inheritedProvider.items, []);
  assert.deepEqual(inheritedProvider.unresolved, ['a']);
});

test('五千个模型可以统一设置或来源映射，超限不能提交', () => {
  const models = Array.from({ length: MAX_OWNERSHIP_BATCH }, (_, i) => `model-${i}`);
  const base = {
    selected: new Set(models),
    method: 'manual',
    target: '7',
    mode: 'fill_missing',
    owners: { 7: { name: '本地归属' } }
  };
  assert.equal(buildOwnershipBatch(base).items.length, 5000);
  const source = buildOwnershipBatch({
    ...base,
    method: 'source',
    suggestions: Object.fromEntries(models.map((model) => [model, { status: 'matched', providers: [{ id: 'lab-a', name: '服务商' }] }])),
    providerOwners: { 'lab-a': '7' }
  });
  assert.equal(source.items.length, 5000);
  assert.equal(source.error, undefined);
  const overflow = buildOwnershipBatch({ ...base, selected: new Set([...models, 'extra-model']) });
  assert.deepEqual(overflow.items, []);
  assert.match(overflow.error, /5000/);
});

test('同名服务提供方按 ID 独立映射，显示名称不能作为映射键', () => {
  const provider = { id: 'cloudflare-workers-ai', name: 'Cloudflare Workers AI' };
  const other = { id: 'other-host', name: 'Cloudflare Workers AI' };
  const base = {
    selected: new Set(['a', 'b']),
    method: 'source',
    owners: { 7: { name: 'Cloudflare Workers AI' }, 8: { name: '其他平台' } },
    suggestions: { a: { status: 'matched', providers: [provider] }, b: { status: 'matched', providers: [other] } }
  };
  assert.equal(buildOwnershipBatch({ ...base, providerOwners: { [provider.name]: '7' } }).items.length, 0);
  const partial = buildOwnershipBatch({ ...base, providerOwners: { [provider.id]: '7' } });
  assert.deepEqual(partial.items, [{ model: 'a', owned_by_id: 7 }]);
  assert.deepEqual(partial.unresolved, ['b']);
  assert.deepEqual(buildOwnershipBatch({ ...base, providerOwners: { [provider.id]: '7', [other.id]: '8' } }).items, [
    { model: 'a', owned_by_id: 7 },
    { model: 'b', owned_by_id: 8 }
  ]);
});
