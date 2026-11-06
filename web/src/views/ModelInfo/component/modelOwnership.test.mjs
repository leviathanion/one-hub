import assert from 'node:assert/strict';
import test from 'node:test';
import { buildOwnershipBatch, CLEAR_OWNERSHIP, parseOwnershipModels } from './modelOwnership.mjs';

test('粘贴精确模型名去重但保留大小写，拒绝通配和超大批次', () => {
  assert.deepEqual(parseOwnershipModels(' Model-A\r\nmodel-a\nModel-A\n'), ['Model-A', 'model-a']);
  assert.throws(() => parseOwnershipModels('family*'), /通配/);
  assert.throws(() => parseOwnershipModels('a'.repeat(101)), /100/);
  assert.throws(() => parseOwnershipModels(Array.from({ length: 501 }, (_, i) => `model-${i}`).join('\n')), /500/);
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

test('服务提供方按 ID 映射，冲突和未匹配不会生成部分写入', () => {
  const provider = { id: 'cloudflare-workers-ai', name: 'Cloudflare Workers AI' };
  const other = { id: 'other-host', name: 'Cloudflare Workers AI' };
  const base = {
    selected: new Set(['a', 'b']),
    method: 'source',
    owners: { 7: { name: 'Cloudflare Workers AI' }, 8: { name: '其他平台' } },
    providerOwners: {},
    suggestions: {
      a: { status: 'matched', providers: [provider] },
      b: { status: 'conflict', providers: [provider, other] }
    }
  };
  assert.equal(buildOwnershipBatch(base).items.length, 0);
  assert.equal(buildOwnershipBatch({ ...base, providerOwners: { [provider.id]: '7' } }).items.length, 0);
  assert.deepEqual(buildOwnershipBatch({ ...base, selected: new Set(['a']), providerOwners: { [provider.id]: '7' } }).items, [
    { model: 'a', owned_by_id: 7 }
  ]);
  assert.equal(buildOwnershipBatch({ ...base, selected: new Set(['unknown']) }).items.length, 0);
  assert.equal(buildOwnershipBatch({ ...base, selected: new Set(['constructor', 'toString', '__proto__']) }).items.length, 0);
  assert.equal(
    buildOwnershipBatch({
      ...base,
      selected: new Set(['a']),
      suggestions: { a: { status: 'matched', providers: [{ id: 'constructor', name: '特殊标识' }] } }
    }).items.length,
    0
  );
  const sameName = {
    ...base,
    suggestions: { a: { status: 'matched', providers: [provider] }, b: { status: 'matched', providers: [other] } }
  };
  assert.equal(buildOwnershipBatch({ ...sameName, providerOwners: { [provider.name]: '7' } }).items.length, 0);
  assert.equal(buildOwnershipBatch({ ...sameName, providerOwners: { [provider.id]: '7' } }).items.length, 0);
  assert.deepEqual(buildOwnershipBatch({ ...sameName, providerOwners: { [provider.id]: '7', [other.id]: '8' } }).items, [
    { model: 'a', owned_by_id: 7 },
    { model: 'b', owned_by_id: 8 }
  ]);
});
