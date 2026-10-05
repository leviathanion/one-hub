import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { buildManualPriceRequest } from './manualPriceRequest.mjs';
import { createPricingDraft, markPricingDraftStale, PRICING_STALE_MESSAGE } from './pricingMutationRecovery.mjs';
import { buildChannelAffinityUpdates, buildCodexHintUpdates, createRoutingDraft, markRoutingDraftStale } from '../../Setting/component/routingSettings.mjs';

const [singleSource, modalSource, pricingSource, operationSource] = await Promise.all([
  readFile(new URL('../single.jsx', import.meta.url), 'utf8'),
  readFile(new URL('./EditModal.jsx', import.meta.url), 'utf8'),
  readFile(new URL('../index.jsx', import.meta.url), 'utf8'),
  readFile(new URL('../../Setting/component/OperationSetting.jsx', import.meta.url), 'utf8')
]);

test('single price edits submit the version captured when the dialog opened', () => {
  const draft = createPricingDraft({ model: 'gpt-5', input: 1, output: 2 }, 7);
  const request = buildManualPriceRequest({ model: 'gpt-5', input: 3, output: 2 }, draft.baseVersion);
  assert.equal(request.expected_version, 7);
  assert.match(singleSource, /onConflict=\{reloadData\}/);
  assert.match(singleSource, /expectedVersion=\{expectedVersion\}/);
  assert.match(modalSource, /onSaveSingle\([\s\S]*draftVersion/);
  assert.match(modalSource, /expected_version: draftVersion/);
});

test('a 409 makes the old draft stale and requires an explicit rebuild', () => {
  const draft = createPricingDraft({ model: 'gpt-5', input: 1, output: 2 }, 7);
  const stale = markPricingDraftStale(draft);
  assert.equal(stale.stale, true);
  assert.equal(stale.baseVersion, 7);
  assert.equal(stale.original.output, 2);
  const rebuilt = createPricingDraft({ model: 'gpt-5', input: 1, output: 99 }, 8);
  assert.equal(rebuilt.stale, false);
  assert.equal(rebuilt.baseVersion, 8);
  assert.equal(rebuilt.original.output, 99);
  assert.match(modalSource, /PRICING_STALE_MESSAGE/);
  assert.match(modalSource, /disabled=\{draft\?\.stale/);
  assert.match(modalSource, /加载最新数据并重新编辑/);
  assert.equal(PRICING_STALE_MESSAGE.length > 0, true);
});

test('batch and routing drafts retain independent CAS versions', () => {
  const routing = createRoutingDraft(
    {
      channelAffinityForm: { max_entries: 50000 },
      channelAffinityBackendDefault: false,
      PreferredChannelWaitMilliseconds: '250',
      PreferredChannelWaitPollMilliseconds: '50'
    },
    'channelAffinity',
    11
  );
  assert.equal(markRoutingDraftStale(routing).baseVersion, 11);
  assert.deepEqual(
    buildChannelAffinityUpdates(
      { channelAffinityBackendDefault: false },
      { ChannelAffinitySetting: 'old' },
      { ChannelAffinitySetting: 'override' },
      '{"max_entries":100}'
    ),
    [{ key: 'ChannelAffinitySetting', value: '{"max_entries":100}' }]
  );
  assert.deepEqual(buildCodexHintUpdates('{"model_regex":"gpt-5"}', '{}'), [
    { key: 'CodexRoutingHintSetting', value: '{"model_regex":"gpt-5"}' }
  ]);
  assert.match(modalSource, /expected_version: draftVersion/);
  assert.match(operationSource, /draftVersion/);
  assert.match(operationSource, /routingDrafts\[section\]\?\.stale/);
  assert.match(operationSource, /加载最新数据并重新编辑/);
});

test('latest reload waits for both pricing snapshots and keeps stale drafts on failure', () => {
  assert.match(pricingSource, /const reloadData = useCallback\(async \(\) => \{/);
  assert.match(pricingSource, /Promise\.all\(\[fetchModelList\(\), fetchPrices\(\)\]\)/);
  assert.match(pricingSource, /return modelListLoaded && pricesLoaded/);
  assert.match(modalSource, /const loaded = await onConflict\(\);/);
  assert.match(modalSource, /if \(!loaded\) \{/);
  assert.match(modalSource, /setLoadingLatest\(false\);/);
  assert.match(modalSource, /onCancel\?\.\(\);/);
});
