const canonicalizeJson = (value) => {
  if (Array.isArray(value)) return value.map(canonicalizeJson);
  if (value === null || typeof value !== 'object') return value;

  return Object.keys(value)
    .sort()
    .reduce((result, key) => {
      result[key] = canonicalizeJson(value[key]);
      return result;
    }, {});
};

export const stablePricingJson = (value) => JSON.stringify(canonicalizeJson(value));
