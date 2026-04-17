import { buildSecretOptionUpdates } from './secretOptionState.mjs';

const trimSingleTrailingSlash = (value) => {
  if (typeof value !== 'string' || !value.endsWith('/')) {
    return value;
  }
  return value.slice(0, -1);
};

// Trade-off: provider enablement now shares the same explicit Save boundary as
// its credentials. This removes instant toggles, but it keeps draft fields,
// validation, and secret clears in one atomic submit path.
export const SYSTEM_PROVIDER_SECTION_CONFIGS = Object.freeze({
  GitHubOAuthEnabled: Object.freeze({
    toggleKey: 'GitHubOAuthEnabled',
    keys: Object.freeze(['GitHubOAuthEnabled', 'GitHubClientId', 'GitHubClientSecret']),
    secretKeys: Object.freeze(['GitHubClientSecret']),
    transformers: Object.freeze({})
  }),
  WeChatAuthEnabled: Object.freeze({
    toggleKey: 'WeChatAuthEnabled',
    keys: Object.freeze(['WeChatAuthEnabled', 'WeChatServerAddress', 'WeChatAccountQRCodeImageURL', 'WeChatServerToken']),
    secretKeys: Object.freeze(['WeChatServerToken']),
    transformers: Object.freeze({
      WeChatServerAddress: trimSingleTrailingSlash
    })
  }),
  LarkAuthEnabled: Object.freeze({
    toggleKey: 'LarkAuthEnabled',
    keys: Object.freeze(['LarkAuthEnabled', 'LarkClientId', 'LarkClientSecret']),
    secretKeys: Object.freeze(['LarkClientSecret']),
    transformers: Object.freeze({})
  }),
  OIDCAuthEnabled: Object.freeze({
    toggleKey: 'OIDCAuthEnabled',
    keys: Object.freeze(['OIDCAuthEnabled', 'OIDCClientId', 'OIDCClientSecret', 'OIDCIssuer', 'OIDCScopes', 'OIDCUsernameClaims']),
    secretKeys: Object.freeze(['OIDCClientSecret']),
    transformers: Object.freeze({})
  }),
  TurnstileCheckEnabled: Object.freeze({
    toggleKey: 'TurnstileCheckEnabled',
    keys: Object.freeze(['TurnstileCheckEnabled', 'TurnstileSiteKey', 'TurnstileSecretKey']),
    secretKeys: Object.freeze(['TurnstileSecretKey']),
    transformers: Object.freeze({})
  })
});

export const SYSTEM_PROVIDER_TOGGLE_KEYS = Object.freeze(Object.keys(SYSTEM_PROVIDER_SECTION_CONFIGS));

const systemProviderToggleKeySet = new Set(SYSTEM_PROVIDER_TOGGLE_KEYS);

export const isSystemProviderToggleKey = (key) => systemProviderToggleKeySet.has(key);

export const getSystemProviderSectionConfig = (toggleKey) => SYSTEM_PROVIDER_SECTION_CONFIGS[toggleKey] || null;

const buildRegularOptionUpdates = ({
  keys,
  transformers = {},
  originInputs = {},
  sourceInputs = {},
  normalizeOptionPayloadValue = (value) => value
}) =>
  keys.flatMap((key) => {
    const transform = transformers[key];
    const nextValue = transform ? transform(sourceInputs[key]) : sourceInputs[key];

    if (originInputs[key] === nextValue) {
      return [];
    }

    return [
      {
        key,
        value: normalizeOptionPayloadValue(nextValue)
      }
    ];
  });

export const buildSystemProviderSectionUpdates = (
  sectionConfig,
  { originInputs = {}, sourceInputs = {}, secretStates = {}, normalizeOptionPayloadValue = (value) => value } = {}
) => {
  if (!sectionConfig) {
    return [];
  }

  const regularKeys = sectionConfig.keys.filter((key) => !sectionConfig.secretKeys.includes(key));

  return [
    ...buildRegularOptionUpdates({
      keys: regularKeys,
      transformers: sectionConfig.transformers,
      originInputs,
      sourceInputs,
      normalizeOptionPayloadValue
    }),
    ...buildSecretOptionUpdates(sectionConfig.secretKeys, secretStates)
  ];
};
