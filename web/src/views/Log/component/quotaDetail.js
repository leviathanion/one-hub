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
  },
  ...[
    ['cache_write_tokens', 'input'],
    ['claude_cache_write_5m_tokens', 'input'],
    ['claude_cache_write_1h_tokens', 'input'],
    ['tool_use_prompt_tokens', 'input'],
    ['input_video_tokens', 'input'],
    ['output_video_tokens', 'output'],
    ['deepseek_cache_hit_tokens', 'input'],
    ['deepseek_cache_miss_tokens', 'input']
  ].map(([key, bucket]) => ({ key, bucket, label: `logPage.quotaDetail.tokenKinds.${key}`, ratioKey: `${key}_ratio` }))
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

export function calculatePrice(ratio, groupDiscount, isTimes) {
  const value = new Decimal(ratio || 0)
    .mul(groupDiscount || 0)
    .mul(isTimes ? 1000 : 1000000)
    .div(getQuotaPerUnit());
  return value.toFixed(6).replace(/(\.\d*?[1-9])0+$|\.0*$/, '$1');
}

export function formatBillingNumber(value, locale) {
  return new Intl.NumberFormat(locale?.replaceAll('_', '-'), { maximumFractionDigits: 6 }).format(value);
}

export function getGroupRatio(metadata) {
  return toNumber(metadata?.group_ratio, 1);
}

export function getTokenBillingDetails(metadata) {
  const details = metadata?.token_billing;
  if (!details || typeof details.status !== 'string' || !Array.isArray(details.rules)) return null;
  for (const key of ['base_input_ratio', 'base_output_ratio', 'input_units', 'output_units', 'charge']) {
    if (typeof details[key] !== 'number' || !Number.isFinite(details[key]) || details[key] < 0) return null;
  }
  if (details.rules.some((rule) => !rule || typeof rule.kind !== 'string')) return null;
  return details;
}

export function getBasePriceRatio(metadata, bucket) {
  // 缺价时后端记录的零是占位值，不是免费定价。
  if (Array.isArray(metadata?.billing_diagnostics) && metadata.billing_diagnostics.includes('price_policy_missing_at_settlement'))
    return null;
  const value =
    metadata?.price_type === 'times'
      ? bucket === 'input'
        ? metadata.input_ratio
        : null
      : getTokenBillingDetails(metadata)?.[`base_${bucket}_ratio`];
  return typeof value === 'number' && Number.isFinite(value) && value >= 0 ? value : null;
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
    const tokens = value * (rate - 1);
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
      bucket,
      tokens,
      value,
      rate,
      labelParams: { ratio: rate }
    };
  }).filter(Boolean);

  const recorded = getTokenBillingDetails(metadata);
  return {
    totalInputTokens: recorded ? recorded.input_units : totalInputTokens,
    totalOutputTokens: recorded ? recorded.output_units : totalOutputTokens,
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
  const recorded = getTokenBillingDetails(metadata);
  const inputRatio = toNumber(recorded?.units_include_rules ? recorded.base_input_ratio : metadata?.input_ratio);
  const outputRatio = toNumber(recorded?.units_include_rules ? recorded.base_output_ratio : metadata?.output_ratio);
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
      actualQuota: quota,
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
    actualQuota: quota,
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

// 分项金额只用于解释费用，不复刻后端的取整、最低扣费或结算规则。
export function getBillingCostRows(item) {
  const metadata = item?.metadata || {};
  const groupRatio = getGroupRatio(metadata);
  const details = getTokenBillingDetails(metadata);
  const breakdown = calculateTokenBreakdown(item);
  const validNumber = (value) => typeof value === 'number' && Number.isFinite(value) && value >= 0;
  const rate = (value) => (validNumber(value) && validNumber(value * groupRatio) ? value * groupRatio : null);
  const groupMultipliers = groupRatio === 1 ? [] : [groupRatio];
  let rows;
  if (metadata.price_type === 'times') {
    // 按次计费也可能是多次操作，日志未记录次数时不能假定为一次。
    rows = [
      {
        key: 'times',
        label: 'perCallBilling',
        kind: 'times',
        count: null,
        ratio: rate(metadata.input_ratio),
        baseRatio: getBasePriceRatio(metadata, 'input'),
        multipliers: groupMultipliers,
        amount: null
      }
    ];
  } else {
    const notCharged = details !== null && details.status !== 'priceable';
    rows = ['input', 'output'].map((bucket) => {
      const ratio = rate(metadata[`${bucket}_ratio`]);
      const count = item?.[bucket === 'input' ? 'prompt_tokens' : 'completion_tokens'];
      const units = bucket === 'input' ? breakdown.totalInputTokens : breakdown.totalOutputTokens;
      const parts = breakdown.tokenDetails.filter((part) => part.bucket === bucket);
      const completeRates = details !== null || parts.every((part) => validNumber(metadata[`${part.key}_ratio`]));
      return {
        key: bucket,
        label: bucket,
        kind: 'tokens',
        count: validNumber(count) ? count : null,
        ratio,
        baseRatio: getBasePriceRatio(metadata, bucket),
        multipliers:
          details?.status === 'priceable'
            ? [...details.rules.map((rule) => rule[`${bucket}_multiplier`]).filter((value) => value !== 1), ...groupMultipliers]
            : null,
        notCharged,
        amount: notCharged
          ? 0
          : ratio !== null && validNumber(count) && validNumber(units) && completeRates
            ? new Decimal(units).mul(details?.units_include_rules ? rate(details[`base_${bucket}_ratio`]) : ratio).toNumber()
            : null,
        adjustments: notCharged
          ? []
          : parts.map((part) => ({
              ...part,
              ratio: details?.units_include_rules
                ? rate(metadata.effective_extra_ratios?.[part.key])
                : ratio !== null && validNumber(metadata[`${part.key}_ratio`])
                  ? ratio * metadata[`${part.key}_ratio`]
                  : null
            }))
      };
    });
  }
  const toolLabels = {
    web_search: 'webSearch',
    web_search_preview: 'webSearch',
    file_search: 'fileSearch',
    code_interpreter: 'codeInterpreter',
    image_generation: 'imageGeneration'
  };
  for (const [key, data] of Object.entries(metadata.extra_billing || {})) {
    if (!data || typeof data !== 'object') continue;
    const ratio = rate(data.price);
    const count = validNumber(data.call_count) ? data.call_count : null;
    const service = data.service_type || key;
    rows.push({
      key: `tool:${key}`,
      label: toolLabels[service] || service,
      variant: data.type,
      kind: 'tool',
      count,
      ratio,
      baseRatio: validNumber(data.price) ? data.price : null,
      multipliers: groupMultipliers,
      amount: ratio !== null && count !== null ? new Decimal(ratio).mul(count).mul(getQuotaPerUnit()).toNumber() : null
    });
  }
  for (const row of rows) {
    if (!validNumber(row.amount)) row.amount = null;
  }
  return rows;
}
