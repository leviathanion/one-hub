import assert from 'node:assert/strict';
import test from 'node:test';
import {
  createInitialSecretStates,
  getSecretClearBlockedMessage,
  markSecretOptionForClear,
  updateSecretOptionDraft
} from './secretOptionState.mjs';
import { buildSystemProviderSectionUpdates, getSystemProviderSectionConfig } from './systemProviderSections.mjs';

const identity = (value) => value;
const t = (key, vars = {}) => `${key}:${vars.feature || ''}`;

test('buildSystemProviderSectionUpdates only emits the active provider section changes', () => {
  const sectionConfig = getSystemProviderSectionConfig('GitHubOAuthEnabled');
  const originInputs = {
    GitHubOAuthEnabled: 'false',
    GitHubClientId: '',
    OIDCAuthEnabled: 'false',
    OIDCClientId: ''
  };
  const sourceInputs = {
    ...originInputs,
    GitHubOAuthEnabled: 'true',
    GitHubClientId: 'cli_new',
    OIDCAuthEnabled: 'true',
    OIDCClientId: 'oidc_draft'
  };
  let secretStates = createInitialSecretStates(['GitHubClientSecret', 'OIDCClientSecret']);
  secretStates = updateSecretOptionDraft(secretStates, 'GitHubClientSecret', 'sec_new');
  secretStates = updateSecretOptionDraft(secretStates, 'OIDCClientSecret', 'oidc_secret_draft');

  const updates = buildSystemProviderSectionUpdates(sectionConfig, {
    originInputs,
    sourceInputs,
    secretStates,
    normalizeOptionPayloadValue: identity
  });

  assert.deepEqual(updates, [
    { key: 'GitHubOAuthEnabled', value: 'true' },
    { key: 'GitHubClientId', value: 'cli_new' },
    { key: 'GitHubClientSecret', value: 'sec_new' }
  ]);
});

test('buildSystemProviderSectionUpdates allows disabling a provider and clearing its secret in one batch', () => {
  const sectionConfig = getSystemProviderSectionConfig('GitHubOAuthEnabled');
  const originInputs = {
    GitHubOAuthEnabled: 'true',
    GitHubClientId: 'cli_seed'
  };
  const sourceInputs = {
    ...originInputs,
    GitHubOAuthEnabled: 'false'
  };
  const secretStates = markSecretOptionForClear(createInitialSecretStates(['GitHubClientSecret']), 'GitHubClientSecret');

  const updates = buildSystemProviderSectionUpdates(sectionConfig, {
    originInputs,
    sourceInputs,
    secretStates,
    normalizeOptionPayloadValue: identity
  });

  assert.deepEqual(updates, [
    { key: 'GitHubOAuthEnabled', value: 'false' },
    { key: 'GitHubClientSecret', value: '' }
  ]);
});

test('getSecretClearBlockedMessage still blocks clearing a secret while its provider stays enabled', () => {
  const blockedMessage = getSecretClearBlockedMessage('GitHubClientSecret', { GitHubOAuthEnabled: 'true' }, t);

  assert.notEqual(blockedMessage, '');
  assert.match(blockedMessage, /setting_index\.systemSettings\.configureLoginRegister\.gitHubOAuth/);
});

test('buildSystemProviderSectionUpdates normalizes WeChat server addresses before submit', () => {
  const sectionConfig = getSystemProviderSectionConfig('WeChatAuthEnabled');
  const originInputs = {
    WeChatAuthEnabled: 'false',
    WeChatServerAddress: 'https://wechat.old',
    WeChatAccountQRCodeImageURL: ''
  };
  const sourceInputs = {
    ...originInputs,
    WeChatServerAddress: 'https://wechat.new/'
  };
  const secretStates = createInitialSecretStates(['WeChatServerToken']);

  const updates = buildSystemProviderSectionUpdates(sectionConfig, {
    originInputs,
    sourceInputs,
    secretStates,
    normalizeOptionPayloadValue: identity
  });

  assert.deepEqual(updates, [{ key: 'WeChatServerAddress', value: 'https://wechat.new' }]);
});
