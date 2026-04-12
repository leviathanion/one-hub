import { useCallback, useState } from 'react';

import { fetchCodexUsageDetail, fetchCodexUsagePreviewMap } from './codexUsage';

function normalizeChannelIDs(channelIDs) {
  const values = Array.isArray(channelIDs) ? channelIDs : [channelIDs];
  return Array.from(
    new Set(values.map((channelID) => Number(channelID)).filter((channelID) => Number.isInteger(channelID) && channelID > 0))
  );
}

function mergePreviewResult(prev, result) {
  const snapshotByChannelId = result?.snapshotByChannelId || {};
  const failedChannelIDs = normalizeChannelIDs(result?.failedChannelIDs || []);

  if (failedChannelIDs.length === 0 && Object.keys(snapshotByChannelId).length === 0) {
    return prev;
  }

  const next = {
    ...prev
  };

  failedChannelIDs.forEach((channelID) => {
    delete next[channelID];
  });
  Object.entries(snapshotByChannelId).forEach(([channelID, snapshot]) => {
    next[channelID] = snapshot;
  });

  return next;
}

function clearChannelEntries(prev, channelIDs) {
  const normalizedChannelIDs = normalizeChannelIDs(channelIDs);
  if (normalizedChannelIDs.length === 0) {
    return prev;
  }

  const next = {
    ...prev
  };
  normalizedChannelIDs.forEach((channelID) => {
    delete next[channelID];
  });

  return next;
}

function stampSnapshot(snapshot, receivedAtMs = Date.now()) {
  if (!snapshot || typeof snapshot !== 'object') {
    return snapshot;
  }

  return {
    ...snapshot,
    client_received_at_ms: receivedAtMs
  };
}

function getSnapshotFreshnessTime(snapshot) {
  const fetchedAt = Number(snapshot?.fetched_at);
  if (Number.isFinite(fetchedAt) && fetchedAt > 0) {
    return fetchedAt * 1000;
  }

  const receivedAtMs = Number(snapshot?.client_received_at_ms);
  if (Number.isFinite(receivedAtMs) && receivedAtMs > 0) {
    return receivedAtMs;
  }

  return 0;
}

function selectFreshestSnapshot(previewSnapshot, detailSnapshot, preferDetailOnTie = false) {
  if (previewSnapshot && !detailSnapshot) {
    return previewSnapshot;
  }
  if (detailSnapshot && !previewSnapshot) {
    return detailSnapshot;
  }
  if (!previewSnapshot && !detailSnapshot) {
    return null;
  }

  const previewFreshness = getSnapshotFreshnessTime(previewSnapshot);
  const detailFreshness = getSnapshotFreshnessTime(detailSnapshot);

  if (detailFreshness > previewFreshness) {
    return detailSnapshot;
  }
  if (previewFreshness > detailFreshness) {
    return previewSnapshot;
  }

  return preferDetailOnTie ? detailSnapshot : previewSnapshot;
}

export function useCodexUsageStore() {
  // Trade-off: preview and detail stay as separate projections instead of one merged
  // canonical object. We keep the source boundary so a later low-fidelity preview
  // cannot clobber detail-only fields such as account/raw diagnostics, but the cost
  // is duplicated state. To keep that trade-off safe, callers must render through a
  // single freshness-aware selector instead of picking a map precedence ad hoc.
  const [previewSnapshotByChannelId, setPreviewSnapshotByChannelId] = useState({});
  const [detailSnapshotByChannelId, setDetailSnapshotByChannelId] = useState({});
  const [detailLoadingByChannelId, setDetailLoadingByChannelId] = useState({});
  const [detailErrorByChannelId, setDetailErrorByChannelId] = useState({});

  const prefetchPreviews = useCallback(async (channels, signal, options = {}) => {
    const previewResult = await fetchCodexUsagePreviewMap(channels, signal, options);
    if (!signal?.aborted) {
      const receivedAtMs = Date.now();
      const stampedPreviewResult = {
        ...previewResult,
        snapshotByChannelId: Object.fromEntries(
          Object.entries(previewResult?.snapshotByChannelId || {}).map(([channelID, snapshot]) => [
            channelID,
            stampSnapshot(snapshot, receivedAtMs)
          ])
        )
      };
      setPreviewSnapshotByChannelId((prev) => mergePreviewResult(prev, stampedPreviewResult));
    }
    return previewResult;
  }, []);

  const invalidateSnapshots = useCallback((channelIDs) => {
    setPreviewSnapshotByChannelId((prev) => clearChannelEntries(prev, channelIDs));
    setDetailSnapshotByChannelId((prev) => clearChannelEntries(prev, channelIDs));
    setDetailLoadingByChannelId((prev) => clearChannelEntries(prev, channelIDs));
    setDetailErrorByChannelId((prev) => clearChannelEntries(prev, channelIDs));
  }, []);

  const refreshDetail = useCallback(async (channelID, signal) => {
    if (!channelID) {
      return null;
    }

    setDetailLoadingByChannelId((prev) => ({
      ...prev,
      [channelID]: true
    }));
    setDetailErrorByChannelId((prev) => ({
      ...prev,
      [channelID]: ''
    }));

    try {
      const nextPayload = await fetchCodexUsageDetail(channelID, true, signal);

      if (!signal?.aborted && nextPayload?.data?.channel_id) {
        setDetailSnapshotByChannelId((prev) => ({
          ...prev,
          [nextPayload.data.channel_id]: stampSnapshot(nextPayload.data)
        }));
      }

      if (!signal?.aborted) {
        setDetailErrorByChannelId((prev) => ({
          ...prev,
          [channelID]: nextPayload?.success === false ? nextPayload?.message || '' : ''
        }));
      }

      return nextPayload;
    } catch (error) {
      if (!signal?.aborted) {
        setDetailErrorByChannelId((prev) => ({
          ...prev,
          [channelID]: error?.message || 'Failed to load Codex usage'
        }));
      }
      return null;
    } finally {
      if (!signal?.aborted) {
        setDetailLoadingByChannelId((prev) => ({
          ...prev,
          [channelID]: false
        }));
      }
    }
  }, []);

  const getPreviewSnapshot = useCallback(
    (channelID) => selectFreshestSnapshot(previewSnapshotByChannelId[channelID], detailSnapshotByChannelId[channelID], true),
    [detailSnapshotByChannelId, previewSnapshotByChannelId]
  );
  const getDetailSnapshot = useCallback(
    (channelID) => {
      // Trade-off: the dialog prefers freshness over richness when preview data
      // is newer than the cached detail snapshot. That can temporarily hide
      // detail-only fields, but it avoids showing stale account/usage state as
      // if it were current. When timestamps tie, detail still wins.
      return selectFreshestSnapshot(previewSnapshotByChannelId[channelID], detailSnapshotByChannelId[channelID], true);
    },
    [detailSnapshotByChannelId, previewSnapshotByChannelId]
  );
  const isDetailLoading = useCallback((channelID) => detailLoadingByChannelId[channelID] === true, [detailLoadingByChannelId]);
  const getDetailError = useCallback((channelID) => detailErrorByChannelId[channelID] || '', [detailErrorByChannelId]);

  return {
    previewSnapshotByChannelId,
    detailSnapshotByChannelId,
    prefetchPreviews,
    invalidateSnapshots,
    refreshDetail,
    getPreviewSnapshot,
    getDetailSnapshot,
    isDetailLoading,
    getDetailError
  };
}
