import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./OperationSetting.jsx', import.meta.url), 'utf8');

test('Channel affinity editor accepts every kind used by the built-in rules', () => {
  const kindsMatch = source.match(/const CHANNEL_AFFINITY_KINDS = \[([^\]]+)\]/);
  assert.ok(kindsMatch, 'CHANNEL_AFFINITY_KINDS must remain explicit');

  const editableKinds = new Set([...kindsMatch[1].matchAll(/'([^']+)'/g)].map((match) => match[1]));
  const builtInKinds = new Set([...source.matchAll(/\n\s+kind: '([^']+)'/g)].map((match) => match[1]));
  assert.deepEqual([...editableKinds].sort(), [...builtInKinds].sort());
});

test('Recommended routing hint template is model-independent', () => {
  const recommended = source.match(/const CODEX_ROUTING_HINT_RECOMMENDED = \{([\s\S]*?)\n\};/);
  assert.ok(recommended, 'recommended routing hint template must remain explicit');
  assert.match(recommended[1], /prompt_cache_key_strategy: 'auto'/);
  assert.match(recommended[1], /model_regex: ''/);
  assert.doesNotMatch(recommended[1], /gpt-|sol|terra|luna/i);
});

test('Options UI preserves server source and version semantics', () => {
  assert.match(source, /newInputs\[item\.key\] = effective/);
  assert.match(source, /newSources\[item\.key\] = item\.source/);
  assert.match(source, /expected_version: optionVersion\.current/);
});

test('Routing affinity and Codex cache keys have independent cards and save actions', () => {
  const affinityStart = source.indexOf("<SubCard title={t('setting_index.operationSettings.codexSettings.affinitySectionTitle')}");
  const hintStart = source.indexOf("<SubCard title={t('setting_index.operationSettings.codexSettings.codexHintSectionTitle')}");
  const cardsEnd = source.indexOf('{renderCodexHelpDialog()}', hintStart);
  assert.ok(affinityStart >= 0 && hintStart > affinityStart && cardsEnd > hintStart);

  const affinity = source.slice(affinityStart, hintStart);
  assert.match(affinity, /submitRoutingSettings\('channelAffinity'\)/);
  assert.match(affinity, /PreferredChannelWaitMilliseconds/);
  assert.match(affinity, /renderChannelAffinityRuleEditor/);
  assert.doesNotMatch(affinity, /codexRoutingHintForm|CodexRoutingHintSetting/);

  const hint = source.slice(hintStart, cardsEnd);
  assert.match(hint, /submitRoutingSettings\('codexHint'\)/);
  assert.match(hint, /CodexRoutingHintStrategy/);
  assert.doesNotMatch(hint, /channelAffinityForm|PreferredChannelWait/);
  assert.doesNotMatch(source, /applyCodexRecommendedPreset|applyCodexSafeDefaults|submitConfig\('codex'\)/);
});

test('Retired settings are absent while immediate feature toggles remain', () => {
  for (const optionName of ['ChatImageRequestProxy', 'CFWorkerImageUrl', 'CFWorkerImageKey', 'QuotaRemindThreshold']) {
    assert.equal(source.match(new RegExp(optionName, 'g'))?.length ?? 0, 0, `${optionName} must not have a field or save path`);
  }
  assert.doesNotMatch(source, /submitConfig\('other'\)/);

  for (const optionName of ['MjNotifyEnabled', 'ClaudeAPIEnabled', 'GeminiAPIEnabled']) {
    assert.match(source, new RegExp(`name="${optionName}"`), `${optionName} must remain an immediate toggle`);
  }
  assert.match(source, /otherSettings\.title/);
});
