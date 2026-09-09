export const PRICING_STALE_MESSAGE = '数据已被其他管理员更新，请加载最新数据后重新编辑。';

export const isPricingConflict = (error) => error?.response?.status === 409;

const cloneDraftValue = (value) => {
  if (value === undefined) return undefined;
  return JSON.parse(JSON.stringify(value));
};

export const createPricingDraft = (value, baseVersion) => ({
  baseVersion: Number(baseVersion) || 0,
  original: cloneDraftValue(value),
  stale: false
});

export const markPricingDraftStale = (draft) => ({
  ...(draft || {}),
  stale: true
});

// A conflict only invalidates the current draft. Reloading is deliberately
// left to the explicit "load latest and re-edit" action in the UI.
export const recoverPricingConflict = async (error, { onStale } = {}) => {
  if (!isPricingConflict(error)) return false;
  await onStale?.(error);
  return true;
};
