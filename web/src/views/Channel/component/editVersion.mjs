// OAuth persists independently of the open form. Advance the form's version
// only if a fresh server snapshot still has exactly the configuration it read.
const independentFields = new Set([
  'key', 'version', 'test_time', 'response_time', 'balance', 'balance_updated_time', 'used_quota'
]);

const canonical = (value) => {
  if (Array.isArray(value)) return value.map(canonical);
  if (value && typeof value === 'object') {
    return Object.fromEntries(Object.keys(value).sort().map((key) => [key, canonical(value[key])]));
  }
  return value;
};

export function versionAfterCredentialSave(baseline, latest, savedKey) {
  if (!baseline || !latest || latest.key !== savedKey || !Number.isSafeInteger(latest.version)) return undefined;
  const configuration = (snapshot) => canonical(Object.fromEntries(
    Object.entries(snapshot).filter(([key]) => !independentFields.has(key))
  ));
  return JSON.stringify(configuration(baseline)) === JSON.stringify(configuration(latest)) ? latest.version : undefined;
}

export function assertOAuthSessionVersion(baseline, session) {
  if (session.operation === 'create' && !baseline) return;
  if (
    !baseline ||
    !['edit', 'recover'].includes(session.operation) ||
    Number(baseline.id) !== session.channel_id ||
    !Number.isSafeInteger(session.expected_version) ||
    baseline.version !== session.expected_version
  ) {
    throw new Error('渠道配置已变化，当前输入已保留。请重新打开渠道核对后再授权。');
  }
}

export function versionAfterOAuthAuthorization(baseline, session, result) {
  assertOAuthSessionVersion(baseline, session);
  if (result.operation !== session.operation || Number(result.channel_id || 0) !== session.channel_id) {
    throw new Error('授权结果与当前渠道不匹配，请重新打开渠道核对。');
  }
  if (session.operation === 'recover') {
    if (result.credential_saved !== true || !Number.isSafeInteger(result.version) || result.version <= session.expected_version) {
      throw new Error('未确认凭据恢复后的渠道版本，请重新打开渠道核对。');
    }
    return result.version;
  }
  if (
    typeof result.key !== 'string' ||
    !result.key.trim() ||
    (session.operation === 'edit' && result.expected_version !== session.expected_version)
  ) {
    throw new Error('授权草稿不完整，请重新开始授权。');
  }
  return session.expected_version;
}
