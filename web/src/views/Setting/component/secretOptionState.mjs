export const SECRET_OPTION_ACTIONS = Object.freeze({
  UNCHANGED: 'unchanged',
  REPLACE: 'replace',
  CLEAR: 'clear'
});

export const SYSTEM_SECRET_OPTION_KEYS = [
  'SMTPToken',
  'GitHubClientSecret',
  'WeChatServerToken',
  'LarkClientSecret',
  'TurnstileSecretKey',
  'OIDCClientSecret'
];

export const OPERATION_SECRET_OPTION_KEYS = ['CFWorkerImageKey'];

export const SECRET_OPTION_DEPENDENT_TOGGLES = Object.freeze({
  GitHubClientSecret: 'GitHubOAuthEnabled',
  WeChatServerToken: 'WeChatAuthEnabled',
  LarkClientSecret: 'LarkAuthEnabled',
  TurnstileSecretKey: 'TurnstileCheckEnabled',
  OIDCClientSecret: 'OIDCAuthEnabled'
});

export const SECRET_OPTION_TOGGLE_LABEL_KEYS = Object.freeze({
  GitHubOAuthEnabled: 'setting_index.systemSettings.configureLoginRegister.gitHubOAuth',
  WeChatAuthEnabled: 'setting_index.systemSettings.configureLoginRegister.weChatAuth',
  LarkAuthEnabled: 'setting_index.systemSettings.configureLoginRegister.larkAuth',
  TurnstileCheckEnabled: 'setting_index.systemSettings.configureLoginRegister.turnstileCheck',
  OIDCAuthEnabled: 'setting_index.systemSettings.configureLoginRegister.oidcAuth'
});

const createSecretState = (configured = false) => ({
  configured,
  draft: '',
  action: SECRET_OPTION_ACTIONS.UNCHANGED
});

export const createInitialSecretStates = (keys) =>
  keys.reduce((acc, key) => {
    acc[key] = createSecretState(false);
    return acc;
  }, {});

export const mergeSecretStatesFromMeta = (keys, sensitiveOptionsMeta = {}) =>
  keys.reduce((acc, key) => {
    acc[key] = createSecretState(Boolean(sensitiveOptionsMeta?.[key]?.configured));
    return acc;
  }, {});

export const updateSecretOptionDraft = (prevStates, key, draft) => ({
  ...prevStates,
  [key]: {
    ...(prevStates[key] || createSecretState(false)),
    draft,
    action: draft.trim() === '' ? SECRET_OPTION_ACTIONS.UNCHANGED : SECRET_OPTION_ACTIONS.REPLACE
  }
});

export const markSecretOptionForClear = (prevStates, key) => ({
  ...prevStates,
  [key]: {
    ...(prevStates[key] || createSecretState(false)),
    draft: '',
    action: SECRET_OPTION_ACTIONS.CLEAR
  }
});

export const resetSecretOptionAction = (prevStates, key) => ({
  ...prevStates,
  [key]: {
    ...(prevStates[key] || createSecretState(false)),
    draft: '',
    action: SECRET_OPTION_ACTIONS.UNCHANGED
  }
});

// Trade-off: an empty draft cannot mean "clear", because redacted reads make
// "front-end does not know the stored value" indistinguishable from "no secret
// is stored". Clearing must therefore be an explicit action.
export const buildSecretOptionUpdates = (keys, secretStates) =>
  keys.flatMap((key) => {
    const state = secretStates[key];
    if (!state) {
      return [];
    }
    if (state.action === SECRET_OPTION_ACTIONS.REPLACE) {
      return [{ key, value: state.draft }];
    }
    if (state.action === SECRET_OPTION_ACTIONS.CLEAR) {
      return [{ key, value: '' }];
    }
    return [];
  });

export const getSecretOptionStatus = (state) => {
  if (!state) {
    return 'notConfigured';
  }
  switch (state.action) {
    case SECRET_OPTION_ACTIONS.REPLACE:
      return 'pendingReplace';
    case SECRET_OPTION_ACTIONS.CLEAR:
      return 'pendingClear';
    default:
      return state.configured ? 'configured' : 'notConfigured';
  }
};

export const getSecretOptionPlaceholder = (t, state, fallbackPlaceholder) => {
  if (state?.action === SECRET_OPTION_ACTIONS.CLEAR) {
    return t('setting_index.secretOptions.pendingClearPlaceholder');
  }
  if (state?.configured) {
    return t('setting_index.secretOptions.replacePlaceholder');
  }
  return fallbackPlaceholder;
};

export const getSecretClearBlockedMessage = (key, sourceInputs, t) => {
  const toggleKey = SECRET_OPTION_DEPENDENT_TOGGLES[key];
  if (!toggleKey || sourceInputs?.[toggleKey] !== 'true') {
    return '';
  }
  const featureLabelKey = SECRET_OPTION_TOGGLE_LABEL_KEYS[toggleKey];
  const feature = featureLabelKey ? t(featureLabelKey) : toggleKey;
  return t('setting_index.secretOptions.clearBlocked', { feature });
};
