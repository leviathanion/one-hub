import { useCallback, useEffect, useRef, useState } from 'react';
import { API } from 'utils/api';
import { pricingSyncFailure, pricingSyncRequestOptions } from './pricingSyncFeedback.mjs';

const emptySession = () => ({ catalog: null, source: null, mode: 'add', preview: null, phase: 'idle', error: null });
const responseError = (data) => Object.assign(new Error(data?.message), { data });

// One session owns its source, selected mode and matching preview. Source loading
// and previewing are cancellable reads; applying is an explicit, non-replayed write.
export default function usePricingSync(open, t) {
  const [session, setSession] = useState(emptySession);
  const readRequest = useRef(null);
  const applying = useRef(false);
  const mounted = useRef(false);

  const cancelRead = useCallback(() => {
    readRequest.current?.abort();
    readRequest.current = null;
  }, []);
  const reset = useCallback(() => {
    if (applying.current) return;
    cancelRead();
    setSession(emptySession());
  }, [cancelRead]);

  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
      cancelRead();
    };
  }, [cancelRead]);

  useEffect(() => {
    if (!open) reset();
  }, [open, reset]);

  const beginRead = () => {
    cancelRead();
    const request = new AbortController();
    readRequest.current = request;
    return request;
  };
  const current = (request) => mounted.current && readRequest.current === request && !request.signal.aborted;
  const options = (request) => ({ ...pricingSyncRequestOptions, signal: request.signal });

  const preview = async (catalog, source, mode, request) => {
    setSession({ catalog, source, mode, preview: null, phase: 'preview', error: null });
    try {
      const response = await API.post('/api/prices/sync/preview', { source: catalog, mode }, options(request));
      if (!current(request)) return;
      if (!response.data?.success) throw responseError(response.data);
      setSession({ catalog, source, mode, preview: response.data.data, phase: 'review', error: null });
    } catch (failure) {
      if (!current(request)) return;
      const error = pricingSyncFailure(failure, 'preview', t);
      setSession(
        error.invalidCatalog ? { ...emptySession(), source, error } : { catalog, source, mode, preview: null, phase: 'review', error }
      );
    }
  };

  const fetchCatalog = async () => {
    if (applying.current) return;
    const request = beginRead();
    const source = { label: 'models.dev', skipped: 0, candidates: [] };
    setSession({ ...emptySession(), phase: 'fetch', source });
    try {
      const response = await API.get('/api/prices/modelsdev', options(request));
      if (!current(request)) return;
      if (!response.data?.success) throw responseError(response.data);
      const catalog = response.data.data?.prices;
      if (!Array.isArray(catalog)) throw new Error(t('CheckUpdatesTable.dataFormatIncorrect'));
      source.skipped = response.data.data.skipped || 0;
      source.candidates = Array.isArray(response.data.data.candidates) ? response.data.data.candidates : [];
      if (!catalog.length) {
        setSession({ ...emptySession(), catalog, source, phase: 'review' });
        return;
      }
      await preview(catalog, source, 'add', request);
    } catch (failure) {
      if (current(request)) setSession({ ...emptySession(), source, error: pricingSyncFailure(failure, 'fetch', t) });
    }
  };

  const chooseMode = (mode) => {
    if (!session.catalog?.length || applying.current || mode === session.mode) return;
    return preview(session.catalog, session.source, mode, beginRead());
  };
  const retryPreview = () => {
    if (!session.catalog?.length || applying.current) return;
    return preview(session.catalog, session.source, session.mode, beginRead());
  };
  const changeCount = (session.preview?.plan?.changes || []).filter((change) => change.action !== 'locked').length;
  const apply = async () => {
    if (applying.current || session.phase !== 'review' || !session.preview || !changeCount) return false;
    const snapshot = session;
    applying.current = true;
    setSession({ ...snapshot, phase: 'apply', error: null });
    try {
      const response = await API.post(
        '/api/prices/sync/apply',
        {
          source: snapshot.catalog,
          mode: snapshot.mode,
          base_version: snapshot.preview.base_version,
          digest: snapshot.preview.digest
        },
        pricingSyncRequestOptions
      );
      if (!mounted.current) return false;
      if (!response.data?.success) throw responseError(response.data);
      setSession(emptySession());
      return true;
    } catch (failure) {
      if (mounted.current) {
        const error = pricingSyncFailure(failure, 'apply', t);
        setSession(
          error.invalidCatalog
            ? { ...emptySession(), source: snapshot.source, error }
            : { ...snapshot, phase: 'review', preview: null, error }
        );
      }
      return false;
    } finally {
      applying.current = false;
    }
  };

  return { session, changeCount, fetchCatalog, chooseMode, retryPreview, reset, apply };
}
