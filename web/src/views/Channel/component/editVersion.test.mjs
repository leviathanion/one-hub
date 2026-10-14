import test from 'node:test';
import assert from 'node:assert/strict';
import { assertOAuthSessionVersion, versionAfterCredentialSave, versionAfterOAuthAuthorization } from './editVersion.mjs';

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

test('OAuth 草稿保留授权开始版本，不能采用较新的表单或服务器版本', () => {
  const session = { operation: 'edit', channel_id: 1, expected_version: 0 };
  const result = { ...session, key: '{\n  "access_token": "draft"\n}' };
  assert.equal(versionAfterOAuthAuthorization(baseline, session, result), 0);
  assert.throws(() => assertOAuthSessionVersion(baseline, { ...session, expected_version: 1 }), /配置已变化/);
  assert.throws(() => versionAfterOAuthAuthorization({ ...baseline, version: 2 }, session, result), /配置已变化/);
  assert.throws(() => versionAfterOAuthAuthorization(baseline, session, { ...result, expected_version: 2 }), /草稿不完整/);
  assert.equal(baseline.key, 'old');
});

test('OAuth 恢复只返回可推进的版本，不读取或替换表单凭据与其他配置', () => {
  const session = { operation: 'recover', channel_id: 1, expected_version: 0 };
  const result = { operation: 'recover', channel_id: 1, version: 1, credential_saved: true };
  assert.equal(versionAfterOAuthAuthorization(baseline, session, result), 1);
  assert.equal(baseline.version, 0);
  assert.equal(baseline.key, 'old');
  for (const patch of [{ operation: 'edit' }, { channel_id: 2 }, { version: 0 }, { credential_saved: false }, { version: undefined }]) {
    assert.throws(() => versionAfterOAuthAuthorization(baseline, session, { ...result, ...patch }));
  }
});

test('OAuth 新建支持完整多行凭据，拒绝缺失凭据与错误操作结果', () => {
  const session = { operation: 'create', channel_id: 0 };
  assert.equal(versionAfterOAuthAuthorization(null, session, { operation: 'create', key: '{\n "token": "draft"\n}' }), undefined);
  assert.throws(() => versionAfterOAuthAuthorization(null, session, { operation: 'create', key: '' }), /草稿不完整/);
  assert.throws(() => versionAfterOAuthAuthorization(null, session, { operation: 'recover', credential_saved: true }), /不匹配/);
});
