// Requests handled inline opt out of the shared toast so each failure has one owner.
export const pricingSyncRequestOptions = { skipErrorNotification: true };

export const pricingSyncFailure = (failure, stage, t) => {
  const data = failure.response?.data || failure.data || {};
  const detail = data.message || failure.message || '';
  if (data.code === 'duplicate_price_model') {
    return { message: t('pricingSync.duplicate', { model: data.model }), invalidCatalog: true, retry: false };
  }
  if (data.code === 'invalid_price_catalog') {
    return { message: t('pricingSync.invalidCatalog', { detail }), invalidCatalog: true, retry: false };
  }
  if (stage !== 'fetch' && failure.response?.status === 409) {
    return { message: t('pricingSync.stalePreview'), retry: true, invalidCatalog: false };
  }
  const transient = !failure.data && (!failure.response || failure.response.status >= 500 || failure.response.status === 429);
  return {
    message: t(`pricingSync.${stage}Failed`, { detail }),
    // Refreshing a read-only preview is safe, including after a failed apply.
    // Never offer to replay the mutation itself.
    retry: stage === 'apply' || (stage === 'preview' && (transient || failure.response?.status === 409)),
    invalidCatalog: false
  };
};
