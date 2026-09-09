const hasRatio = (value, key) => Object.prototype.hasOwnProperty.call(value || {}, key);

export const getExtraRatioLabel = (t, key) => t(`modelpricePage.${key}`, { defaultValue: key });

export const getAvailableExtraRatioConfigs = (configs, value = {}) => configs.filter(({ key }) => !hasRatio(value, key));

export const getAddedExtraRatioConfigs = (configs, value = {}) => configs.filter(({ key }) => hasRatio(value, key));

export const addExtraRatio = (value = {}, key) => {
  if (!key || hasRatio(value, key)) return value;
  return { ...value, [key]: 1 };
};

export const updateExtraRatio = (value = {}, key, ratio) => ({ ...value, [key]: ratio });

export const removeExtraRatio = (value = {}, key) => {
  if (!hasRatio(value, key)) return value;
  const next = { ...value };
  delete next[key];
  return next;
};
