import assert from 'node:assert/strict';
import test from 'node:test';
import {
  buildChannelAffinityUpdates,
  buildCodexHintUpdates,
  createRoutingDraft,
  isRoutingDraftDirty,
  markRoutingDraftStale,
  preserveRoutingDrafts
} from './routingSettings.mjs';

test('Affinity save includes only affinity and waiting options', () => {
  const updates = buildChannelAffinityUpdates(
    {
      PreferredChannelWaitMilliseconds: 0,
      PreferredChannelWaitPollMilliseconds: '50',
      channelAffinityBackendDefault: false,
      codexRoutingHintForm: { prompt_cache_key_strategy: 'auto' }
    },
    { PreferredChannelWaitMilliseconds: '250', PreferredChannelWaitPollMilliseconds: '50', ChannelAffinitySetting: '{}' },
    { ChannelAffinitySetting: 'override' },
    '{"enabled":false,"rules":[]}'
  );
  assert.deepEqual(updates, [
    { key: 'PreferredChannelWaitMilliseconds', value: '0' },
    { key: 'ChannelAffinitySetting', value: '{"enabled":false,"rules":[]}' }
  ]);
});

test('Restoring backend affinity defaults removes the override rather than saving an empty value', () => {
  const inputs = { channelAffinityBackendDefault: true };
  assert.deepEqual(buildChannelAffinityUpdates(inputs, {}, { ChannelAffinitySetting: 'override' }, '{}'), [
    { key: 'ChannelAffinitySetting', inherit: true }
  ]);
  assert.deepEqual(buildChannelAffinityUpdates(inputs, {}, { ChannelAffinitySetting: 'default' }, '{}'), []);
});

test('Explicit affinity defaults can be saved even when their value matches the inherited value', () => {
  assert.deepEqual(
    buildChannelAffinityUpdates(
      { channelAffinityBackendDefault: false },
      { ChannelAffinitySetting: '{}' },
      { ChannelAffinitySetting: 'default' },
      '{}'
    ),
    [{ key: 'ChannelAffinitySetting', value: '{}' }]
  );
  assert.deepEqual(
    buildChannelAffinityUpdates(
      { channelAffinityBackendDefault: false },
      { ChannelAffinitySetting: '{}' },
      { ChannelAffinitySetting: 'override' },
      '{}'
    ),
    []
  );
});

test('Codex cache-key save emits only its own option and skips unchanged values', () => {
  assert.deepEqual(buildCodexHintUpdates('{"prompt_cache_key_strategy":"off"}', '{}'), [
    { key: 'CodexRoutingHintSetting', value: '{"prompt_cache_key_strategy":"off"}' }
  ]);
  assert.deepEqual(buildCodexHintUpdates('{}', '{}'), []);
});

const draft = {
  channelAffinityForm: { enabled: false, rules: [] },
  channelAffinityBackendDefault: true,
  PreferredChannelWaitMilliseconds: 0,
  PreferredChannelWaitPollMilliseconds: '100',
  codexRoutingHintForm: { prompt_cache_key_strategy: 'session_id', model_regex: 'draft' }
};
const server = {
  channelAffinityForm: { enabled: true },
  channelAffinityBackendDefault: false,
  PreferredChannelWaitMilliseconds: '250',
  PreferredChannelWaitPollMilliseconds: '50',
  codexRoutingHintForm: { prompt_cache_key_strategy: 'off' },
  ChannelAffinitySetting: 'server value'
};

test('Refreshing after an affinity save preserves the unsaved Codex draft', () => {
  const result = preserveRoutingDrafts(server, draft, server, ['codexHint']);
  assert.deepEqual(result, { ...server, codexRoutingHintForm: draft.codexRoutingHintForm });
  assert.equal(server.codexRoutingHintForm.prompt_cache_key_strategy, 'off');
});

test('Refreshing after a Codex save preserves affinity rules, inheritance and waiting drafts', () => {
  assert.deepEqual(preserveRoutingDrafts(server, draft, server, ['channelAffinity']), {
    ...server,
    ...draft,
    codexRoutingHintForm: server.codexRoutingHintForm
  });
});

test('Failed saves preserve both drafts while reloading server-owned option data', () => {
  assert.deepEqual(preserveRoutingDrafts(server, draft, server, ['channelAffinity', 'codexHint']), { ...server, ...draft });
  assert.deepEqual(preserveRoutingDrafts(server, draft, server, []), server);
});

test('Unedited sections accept current server values rather than retaining stale form state', () => {
  const updatedServer = { ...server, codexRoutingHintForm: { prompt_cache_key_strategy: 'auto' }, PreferredChannelWaitMilliseconds: '500' };
  assert.deepEqual(preserveRoutingDrafts(updatedServer, server, server, ['channelAffinity', 'codexHint']), updatedServer);
});

test('Routing drafts keep their base version and become dirty only against their captured original', () => {
  const inputs = {
    channelAffinityForm: { max_entries: 50000 },
    channelAffinityBackendDefault: false,
    PreferredChannelWaitMilliseconds: '250',
    PreferredChannelWaitPollMilliseconds: '50'
  };
  const draft = createRoutingDraft(inputs, 'channelAffinity', 12);
  inputs.channelAffinityForm.max_entries = 100;
  assert.equal(draft.baseVersion, 12);
  assert.equal(isRoutingDraftDirty(inputs, draft, 'channelAffinity'), true);
  assert.equal(markRoutingDraftStale(draft).stale, true);
});
