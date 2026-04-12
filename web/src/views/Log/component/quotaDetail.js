import Decimal from 'decimal.js';

const DEFAULT_QUOTA_PER_UNIT = 500000;

const EXTRA_TOKEN_FIELDS = [
  {
    key: 'input_text_tokens',
    label: 'logPage.inputTextTokens',
    ratioKey: 'input_text_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'output_text_tokens',
    label: 'logPage.outputTextTokens',
    ratioKey: 'output_text_tokens_ratio',
    bucket: 'output'
  },
  {
    key: 'input_audio_tokens',
    label: 'logPage.inputAudioTokens',
    ratioKey: 'input_audio_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'output_audio_tokens',
    label: 'logPage.outputAudioTokens',
    ratioKey: 'output_audio_tokens_ratio',
    bucket: 'output'
  },
  {
    key: 'cached_tokens',
    label: 'logPage.cachedTokens',
    ratioKey: 'cached_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'cached_write_tokens',
    label: 'logPage.cachedWriteTokens',
    ratioKey: 'cached_write_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'cached_read_tokens',
    label: 'logPage.cachedReadTokens',
    ratioKey: 'cached_read_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'reasoning_tokens',
    label: 'logPage.reasoningTokens',
    ratioKey: 'reasoning_tokens_ratio',
    bucket: 'output'
  },
  {
    key: 'input_image_tokens',
    label: 'logPage.inputImageTokens',
    ratioKey: 'input_image_tokens_ratio',
    bucket: 'input'
  },
  {
    key: 'output_image_tokens',
    label: 'logPage.outputImageTokens',
    ratioKey: 'output_image_tokens_ratio',
    bucket: 'output'
  }
];

function toNumber(value, fallback = 0) {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : fallback;
}

function getQuotaPerUnit() {
  if (typeof window === 'undefined' || !window.localStorage) {
    return DEFAULT_QUOTA_PER_UNIT;
  }

  const quotaPerUnit = Number(window.localStorage.getItem('quota_per_unit'));
  return Number.isFinite(quotaPerUnit) && quotaPerUnit > 0 ? quotaPerUnit : DEFAULT_QUOTA_PER_UNIT;
}

export function getGroupRatio(metadata) {
  return toNumber(metadata?.group_ratio, 1);
}

export function getStoredOriginalQuota(metadata) {
  const originalQuota = toNumber(metadata?.original_quota, NaN);
  if (Number.isFinite(originalQuota)) {
    return originalQuota;
  }

  const legacyOriginalQuota = toNumber(metadata?.origin_quota, NaN);
  return Number.isFinite(legacyOriginalQuota) ? legacyOriginalQuota : null;
}

export function calculateTokenBreakdown(item) {
  const promptTokens = toNumber(item?.prompt_tokens);
  const completionTokens = toNumber(item?.completion_tokens);
  const metadata = item?.metadata;

  if (!metadata) {
    return {
      totalInputTokens: promptTokens,
      totalOutputTokens: completionTokens,
      show: false,
      tokenDetails: []
    };
  }

  let totalInputTokens = promptTokens;
  let totalOutputTokens = completionTokens;
  let show = false;

  const tokenDetails = EXTRA_TOKEN_FIELDS.map(({ key, label, ratioKey, bucket }) => {
    const value = toNumber(metadata[key]);
    if (value <= 0) {
      return null;
    }

    const rate = toNumber(metadata[ratioKey], 1);
    const tokens = Math.trunc(value * (rate - 1));
    if (tokens !== 0) {
      if (bucket === 'input') {
        totalInputTokens += tokens;
      } else {
        totalOutputTokens += tokens;
      }
      show = true;
    }

    return {
      key,
      label,
      tokens,
      value,
      rate,
      labelParams: { ratio: rate }
    };
  }).filter(Boolean);

  return {
    totalInputTokens,
    totalOutputTokens,
    show,
    tokenDetails
  };
}

function calculateOriginalExtraBillingQuota(extraBilling) {
  if (!extraBilling || typeof extraBilling !== 'object') {
    return new Decimal(0);
  }

  const quotaPerUnit = getQuotaPerUnit();

  return Object.values(extraBilling).reduce((sum, item) => {
    const price = new Decimal(toNumber(item?.price));
    const callCount = toNumber(item?.call_count);

    if (price.lte(0) || callCount <= 0) {
      return sum;
    }

    return sum.plus(price.mul(quotaPerUnit).ceil().mul(callCount));
  }, new Decimal(0));
}

function calculateActualExtraBillingQuota(originalExtraBillingQuota, groupRatio) {
  if (!originalExtraBillingQuota || originalExtraBillingQuota.lte(0)) {
    return new Decimal(0);
  }

  return originalExtraBillingQuota.mul(groupRatio).ceil();
}

export function calculateQuotaDetail(item, tokenBreakdown = calculateTokenBreakdown(item)) {
  const metadata = item?.metadata || {};
  const quota = toNumber(item?.quota);
  const priceType = metadata?.price_type || 'tokens';
  const groupRatio = getGroupRatio(metadata);
  const inputRatio = toNumber(metadata?.input_ratio);
  const outputRatio = toNumber(metadata?.output_ratio);
  const { totalInputTokens, totalOutputTokens } = tokenBreakdown;
  const storedOriginalQuota = getStoredOriginalQuota(metadata);
  const originalExtraBillingQuota = calculateOriginalExtraBillingQuota(metadata?.extra_billing);
  const actualExtraBillingQuota = calculateActualExtraBillingQuota(originalExtraBillingQuota, groupRatio);

  if (priceType === 'times') {
    const originalInputQuota = new Decimal(inputRatio).mul(1000);
    const actualInputQuota = originalInputQuota.mul(groupRatio);
    const computedActualQuota = actualInputQuota.floor().plus(actualExtraBillingQuota);

    return {
      quota,
      priceType,
      groupRatio,
      inputRatio,
      outputRatio,
      totalInputTokens,
      totalOutputTokens,
      originalInputQuota: originalInputQuota.toNumber(),
      originalOutputQuota: 0,
      actualInputQuota: actualInputQuota.toNumber(),
      actualOutputQuota: 0,
      originalExtraBillingQuota: originalExtraBillingQuota.toNumber(),
      actualExtraBillingQuota: actualExtraBillingQuota.toNumber(),
      originalQuota: storedOriginalQuota ?? originalInputQuota.plus(originalExtraBillingQuota).toNumber(),
      actualQuota: quota || computedActualQuota.toNumber(),
      computedActualQuota: computedActualQuota.toNumber(),
      usedStoredOriginalQuota: storedOriginalQuota !== null
    };
  }

  const originalInputQuota = new Decimal(totalInputTokens).mul(inputRatio);
  const originalOutputQuota = new Decimal(totalOutputTokens).mul(outputRatio);
  const originalQuotaComputed = originalInputQuota.plus(originalOutputQuota).plus(originalExtraBillingQuota);

  const actualInputQuota = originalInputQuota.mul(groupRatio);
  const actualOutputQuota = originalOutputQuota.mul(groupRatio);
  const totalTokens = totalInputTokens + totalOutputTokens;
  const actualTokenQuota = totalTokens > 0 ? actualInputQuota.plus(actualOutputQuota).ceil() : new Decimal(0);

  let computedActualQuota = actualTokenQuota.plus(actualExtraBillingQuota);
  if (inputRatio * groupRatio !== 0 && computedActualQuota.lte(0) && totalTokens > 0) {
    computedActualQuota = new Decimal(1);
  }

  const reverseCalculatedOriginalQuota = groupRatio === 0 || quota <= 0 ? new Decimal(0) : new Decimal(quota).div(groupRatio);
  const originalQuota =
    storedOriginalQuota ?? (originalQuotaComputed.gt(0) ? originalQuotaComputed.toNumber() : reverseCalculatedOriginalQuota.toNumber());

  return {
    quota,
    priceType,
    groupRatio,
    inputRatio,
    outputRatio,
    totalInputTokens,
    totalOutputTokens,
    originalInputQuota: originalInputQuota.toNumber(),
    originalOutputQuota: originalOutputQuota.toNumber(),
    actualInputQuota: actualInputQuota.toNumber(),
    actualOutputQuota: actualOutputQuota.toNumber(),
    originalExtraBillingQuota: originalExtraBillingQuota.toNumber(),
    actualExtraBillingQuota: actualExtraBillingQuota.toNumber(),
    originalQuota,
    actualQuota: quota || computedActualQuota.toNumber(),
    computedActualQuota: computedActualQuota.toNumber(),
    usedStoredOriginalQuota: storedOriginalQuota !== null
  };
}

// Trade-off: derive original billing from raw ratios and token usage instead of
// reversing actual quota, because actual quota has already gone through minimum
// charge and rounding in the backend.
export function calculateOriginalQuota(item) {
  return calculateQuotaDetail(item).originalQuota;
}
