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
