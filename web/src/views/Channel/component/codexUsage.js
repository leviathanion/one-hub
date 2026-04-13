import axios from 'axios';

export const CODEX_CHANNEL_TYPE = 101;
const CODEX_USAGE_PREVIEW_MAX_IDS = 100;

const silentAPI = axios.create({
  baseURL: import.meta.env.VITE_APP_SERVER || '/'
});

function buildSilentConfig(signal) {
  const config = {};
  const token = localStorage.getItem('token');
  if (token) {
    config.headers = {
      Authorization: token
    };
  }
  if (signal) {
    config.signal = signal;
  }
  return config;
}

export function isCodexChannel(channel) {
  return Number(channel?.type) === CODEX_CHANNEL_TYPE;
}

export function normalizeCodexUsageSnapshot(source) {
  if (!source || typeof source !== 'object') {
    return null;
  }

  const channelID = Number(source.channel_id);
  if (!Number.isInteger(channelID) || channelID <= 0) {
    return null;
  }

  const account =
    source.account && typeof source.account === 'object'
      ? {
          user_id: source.account.user_id || '',
          email: source.account.email || '',
          account_id: source.account.account_id || ''
        }
      : undefined;

  const normalized = {
    channel_id: channelID,
    plan_type: source.plan_type,
    allowed: source.allowed,
    limit_reached: source.limit_reached,
    fetched_at: source.fetched_at,
    windows: Array.isArray(source.windows) ? source.windows : []
  };

  if (account) {
    normalized.account = account;
  }
  if (source.raw !== undefined) {
    normalized.raw = source.raw;
  }
  if (source.upstream_status !== undefined && source.upstream_status !== null) {
    normalized.upstream_status = source.upstream_status;
  }

  return normalized;
}

export function getCodexUsageWindow(snapshot, windowKey) {
  if (!Array.isArray(snapshot?.windows)) {
    return null;
  }
  return snapshot.windows.find((window) => window?.window_key === windowKey) || null;
}

function readOptionalFiniteNumber(value) {
  if (value === null || value === undefined || value === '') {
    return null;
  }
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed : null;
}

export function resolveCodexUsageRatio(windowData) {
  // Missing windows happen during loading, failures, or upstream omissions. Keep
  // that state as unknown instead of coercing it to 0%, otherwise the admin UI
  // paints broken data as a healthy green reading.
  if (!windowData || typeof windowData !== 'object') {
    return null;
  }

  const usageRatio = readOptionalFiniteNumber(windowData?.usage_ratio);
  if (usageRatio != null) {
    return Math.max(0, Math.min(1, usageRatio));
  }

  const usedPercent = readOptionalFiniteNumber(windowData?.used_percent);
  if (usedPercent != null) {
    return Math.max(0, Math.min(1, usedPercent / 100));
  }

  return null;
}

export function formatCodexWindowSummary(windowData) {
  if (!windowData) {
    return '--';
  }

  const used = readOptionalFiniteNumber(windowData?.used);
  const limit = readOptionalFiniteNumber(windowData?.limit);
  if (used != null && limit != null && limit > 0) {
    return `${formatCompactNumber(used)}/${formatCompactNumber(limit)}`;
  }

  const usedPercent = readOptionalFiniteNumber(windowData?.used_percent);
  if (usedPercent != null) {
    return formatPercent(usedPercent);
  }

  return '--';
}

export function formatCodexPlanType(planType) {
  const normalized = String(planType || '')
    .trim()
    .toLowerCase();
  switch (normalized) {
    case 'free':
      return 'Free';
    case 'plus':
      return 'Plus';
    case 'pro':
      return 'Pro';
    case 'team':
      return 'Team';
    case 'enterprise':
      return 'Enterprise';
    default:
      return String(planType || '').trim() || '--';
  }
}

export function formatCodexResetAt(windowData) {
  const resetsAt = Number(windowData?.resets_at);
  if (!Number.isFinite(resetsAt) || resetsAt <= 0) {
    return '--';
  }

  try {
    return new Date(resetsAt * 1000).toLocaleString();
  } catch (error) {
    return '--';
  }
}

export function formatCodexResetCountdown(windowData, t, nowSeconds = Date.now() / 1000) {
  if (typeof t !== 'function') {
    return '--';
  }

  const resetInSeconds = readOptionalFiniteNumber(windowData?.resets_in_seconds);
  const resetAt = readOptionalFiniteNumber(windowData?.resets_at);

  let remainingSeconds = null;
  if (resetAt != null && resetAt > 0) {
    remainingSeconds = Math.ceil(resetAt - nowSeconds);
  } else if (resetInSeconds != null) {
    remainingSeconds = Math.ceil(resetInSeconds);
  }

  if (remainingSeconds == null) {
    return '--';
  }
  if (remainingSeconds <= 0) {
    return t('channel_row.codexResetReached');
  }

  const daySeconds = 24 * 60 * 60;
  const hourSeconds = 60 * 60;
  const minuteSeconds = 60;

  if (remainingSeconds < minuteSeconds) {
    return `${remainingSeconds}${t('channel_row.codexDurationSecondShort')}`;
  }

  if (remainingSeconds < hourSeconds) {
    const minutes = Math.floor(remainingSeconds / minuteSeconds);
    const seconds = remainingSeconds % minuteSeconds;
    if (seconds === 0) {
      return `${minutes}${t('channel_row.codexDurationMinuteShort')}`;
    }
    return `${minutes}${t('channel_row.codexDurationMinuteShort')} ${seconds}${t('channel_row.codexDurationSecondShort')}`;
  }

  if (remainingSeconds < daySeconds) {
    const hours = Math.floor(remainingSeconds / hourSeconds);
    const minutes = Math.floor((remainingSeconds % hourSeconds) / minuteSeconds);
    if (minutes === 0) {
      return `${hours}${t('channel_row.codexDurationHourShort')}`;
    }
    return `${hours}${t('channel_row.codexDurationHourShort')} ${minutes}${t('channel_row.codexDurationMinuteShort')}`;
  }

  const days = Math.floor(remainingSeconds / daySeconds);
  const hours = Math.floor((remainingSeconds % daySeconds) / hourSeconds);
  if (hours === 0) {
    return `${days}${t('channel_row.codexDurationDayShort')}`;
  }
  return `${days}${t('channel_row.codexDurationDayShort')} ${hours}${t('channel_row.codexDurationHourShort')}`;
}

export function formatCodexFetchedAt(snapshot) {
  const fetchedAt = Number(snapshot?.fetched_at);
  if (!Number.isFinite(fetchedAt) || fetchedAt <= 0) {
    return '--';
  }

  try {
    return new Date(fetchedAt * 1000).toLocaleString();
  } catch (error) {
    return '--';
  }
}

function createEmptyPreviewResult() {
  return {
    snapshotByChannelId: {},
    failedChannelIDs: []
  };
}

function reducePreviewItems(items) {
  return (items || []).reduce((accumulator, item) => {
    const fallbackChannelID = Number(item?.channel_id);
    const snapshot = normalizeCodexUsageSnapshot(item?.preview);
    if (item?.ok && snapshot) {
      accumulator.snapshotByChannelId[snapshot.channel_id] = snapshot;
    } else if (Number.isInteger(fallbackChannelID) && fallbackChannelID > 0) {
      accumulator.failedChannelIDs.push(fallbackChannelID);
    } else if (snapshot?.channel_id) {
      accumulator.failedChannelIDs.push(snapshot.channel_id);
    }
    return accumulator;
  }, createEmptyPreviewResult());
}

function chunkChannelIDs(ids, chunkSize) {
  const chunks = [];
  for (let index = 0; index < ids.length; index += chunkSize) {
    chunks.push(ids.slice(index, index + chunkSize));
  }
  return chunks;
}

export async function fetchCodexUsagePreviewMap(channels, signal, options = {}) {
  const includeTaggedMembers = options?.includeTaggedMembers === true;
  const ids = Array.from(
    new Set(
      (channels || [])
        .filter((channel) => isCodexChannel(channel) && (includeTaggedMembers || !channel?.tag))
        .map((channel) => Number(channel?.id))
        .filter((channelID) => Number.isInteger(channelID) && channelID > 0)
    )
  );

  if (!ids.length) {
    return {
      snapshotByChannelId: {},
      failedChannelIDs: []
    };
  }

  const mergedResult = createEmptyPreviewResult();
  // The backend limit is a resource boundary, not a hint. Split on the client and
  // keep requests sequential so a large tag group does not fan out into an
  // unbounded number of concurrent upstream preview fetches. That is a deliberate
  // latency-vs-load trade-off: admin pages may load a bit slower, but we protect
  // the usage endpoint and upstream refresh path from bursty preview storms.
  for (const batchIDs of chunkChannelIDs(ids, CODEX_USAGE_PREVIEW_MAX_IDS)) {
    try {
      const res = await silentAPI.post('/api/channel/codex/usage/previews', { ids: batchIDs }, buildSilentConfig(signal));
      const items = Array.isArray(res?.data?.data?.items) ? res.data.data.items : [];
      const batchResult = reducePreviewItems(items);

      Object.assign(mergedResult.snapshotByChannelId, batchResult.snapshotByChannelId);
      mergedResult.failedChannelIDs.push(...batchResult.failedChannelIDs);
    } catch (error) {
      if (signal?.aborted || axios.isCancel(error) || error?.code === 'ERR_CANCELED') {
        throw error;
      }

      // Trade-off: preview batches are independent and have no all-or-nothing
      // consistency requirement. Preserve earlier successful batches so admins see
      // partial data instead of a full miss, and mark only the failed batch stale.
      mergedResult.failedChannelIDs.push(...batchIDs);
    }
  }

  return mergedResult;
}

export async function fetchCodexUsageDetail(channelID, refresh = true, signal) {
  if (!channelID) {
    return null;
  }

  const res = await silentAPI.get(`/api/channel/${channelID}/codex/usage`, {
    ...buildSilentConfig(signal),
    params: refresh ? { refresh: 1 } : undefined
  });

  if (!res?.data) {
    return null;
  }

  return {
    ...res.data,
    data: normalizeCodexUsageSnapshot(res.data.data)
  };
}

function formatCompactNumber(value) {
  if (!Number.isFinite(value)) {
    return '--';
  }
  if (Number.isInteger(value)) {
    return String(value);
  }
  return value.toFixed(value >= 10 ? 1 : 2).replace(/\.0$/, '');
}

function formatPercent(value) {
  if (!Number.isFinite(value)) {
    return '--';
  }
  const normalized = Math.max(0, value);
  if (Math.abs(normalized - Math.round(normalized)) < 0.05) {
    return `${Math.round(normalized)}%`;
  }
  return `${normalized.toFixed(1)}%`;
}
