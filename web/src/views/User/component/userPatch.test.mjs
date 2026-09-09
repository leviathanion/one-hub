import test from 'node:test';
import assert from 'node:assert/strict';
import { buildUserEditPatch } from './userPatch.js';

test('只改显示名不会提交旧分组、权限或额度', () => {
  const original = { username: 'user', display_name: 'before', group: 'default', role: 1, status: 1, quota: 100, used_quota: 50 };
  assert.deepEqual(buildUserEditPatch({ ...original, display_name: 'after', password: '' }, original, '7'), {
    id: 7,
    display_name: 'after'
  });
});

test('明确改组和清空显示名保留操作意图', () => {
  const original = { username: 'user', display_name: 'before', group: 'default' };
  assert.deepEqual(buildUserEditPatch({ ...original, group: 'paid', display_name: '' }, original, 7), {
    id: 7,
    display_name: '',
    group: 'paid'
  });
});
