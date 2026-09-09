const draftKeys = {
  channelAffinity: [
    'channelAffinityForm',
    'channelAffinityBackendDefault',
    'PreferredChannelWaitMilliseconds',
    'PreferredChannelWaitPollMilliseconds'
  ],
  codexHint: ['codexRoutingHintForm']
};

const cloneDraftValue = (value) => JSON.parse(JSON.stringify(value ?? null));

export function routingSectionDraftValue(inputs, section) {
  if (section === 'channelAffinity') {
    return {
      channelAffinityForm: inputs?.channelAffinityForm,
      channelAffinityBackendDefault: inputs?.channelAffinityBackendDefault,
      PreferredChannelWaitMilliseconds: inputs?.PreferredChannelWaitMilliseconds,
      PreferredChannelWaitPollMilliseconds: inputs?.PreferredChannelWaitPollMilliseconds
    };
  }
  return { codexRoutingHintForm: inputs?.codexRoutingHintForm };
}

export function createRoutingDraft(inputs, section, baseVersion) {
  return {
    baseVersion: Number(baseVersion) || 0,
    original: cloneDraftValue(routingSectionDraftValue(inputs, section)),
    stale: false
  };
}

export function markRoutingDraftStale(draft) {
  return { ...(draft || {}), stale: true };
}

export function isRoutingDraftDirty(inputs, draft, section) {
  return JSON.stringify(routingSectionDraftValue(inputs, section)) !== JSON.stringify(draft?.original);
}

export function preserveRoutingDrafts(nextInputs, currentInputs, originalInputs, sections) {
  const merged = { ...nextInputs };
  for (const section of sections) {
    const dirty = draftKeys[section].some((key) => JSON.stringify(currentInputs[key]) !== JSON.stringify(originalInputs[key]));
    if (!dirty) continue;
    for (const key of draftKeys[section]) {
      merged[key] = currentInputs[key];
    }
  }
  return merged;
}

export function buildChannelAffinityUpdates(inputs, originInputs, optionSources, serializedAffinity) {
  const updates = ['PreferredChannelWaitMilliseconds', 'PreferredChannelWaitPollMilliseconds']
    .filter((key) => originInputs[key] !== inputs[key])
    .map((key) => ({ key, value: String(inputs[key]) }));

  if (inputs.channelAffinityBackendDefault) {
    if (optionSources.ChannelAffinitySetting !== 'default') {
      updates.push({ key: 'ChannelAffinitySetting', inherit: true });
    }
  } else if (originInputs.ChannelAffinitySetting !== serializedAffinity || optionSources.ChannelAffinitySetting === 'default') {
    updates.push({ key: 'ChannelAffinitySetting', value: serializedAffinity });
  }
  return updates;
}

export function buildCodexHintUpdates(serializedHint, originalHint) {
  return serializedHint === originalHint ? [] : [{ key: 'CodexRoutingHintSetting', value: serializedHint }];
}
