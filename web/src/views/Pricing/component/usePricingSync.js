import { useCallback, useEffect, useRef, useState } from 'react';
import { API } from 'utils/api';
import { canAcceptDefaultPricingUrl, createPricingFetchController, resolveDefaultPricingUrl } from './pricingFetchState.mjs';
import { pricingSyncFailure, pricingSyncRequestOptions } from './pricingSyncFeedback.mjs';

const storageKey = 'oneapi_price_update_url';
const emptySession = () => ({ catalog: null, source: null, mode: 'add', preview: null, phase: 'idle', error: null });
const responseError = (data) => Object.assign(new Error(data?.message), { data });

// One session owns its source, selected mode and matching preview. Source loading
// and previewing are cancellable reads; applying is an explicit, non-replayed write.
export default function usePricingSync(open, t) {
  const [url, setUrl] = useState(() => localStorage.getItem(storageKey) || '');
  const [session, setSession] = useState(emptySession);
  const urlRef = useRef(url);
  const userEdited = useRef(false);
  const defaultRequest = useRef(createPricingFetchController());
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
    const controller = defaultRequest.current;
    const generation = controller.begin();
    const signal = new AbortController();
    if (!localStorage.getItem(storageKey)) {
      const initialize = async () => {
        let defaultUrl;
        try {
          const response = await API.get('/api/prices/updateService', { ...pricingSyncRequestOptions, signal: signal.signal });
          defaultUrl = response.data?.data;
        } catch {
          // The existing fallback also works if the optional settings call fails.
        }
        if (
          canAcceptDefaultPricingUrl({
            controller,
            generation,
            currentUrl: urlRef.current,
            cachedUrl: localStorage.getItem(storageKey),
            userEdited: userEdited.current
          })
        ) {
          const next = resolveDefaultPricingUrl(defaultUrl);
          urlRef.current = next;
          setUrl(next);
          localStorage.setItem(storageKey, next);
        }
      };
      initialize();
    }
    return () => {
      mounted.current = false;
      controller.invalidate();
      signal.abort();
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

  const editUrl = (value) => {
    if (applying.current) return;
    userEdited.current = true;
    defaultRequest.current.invalidate();
    urlRef.current = value;
    setUrl(value);
    localStorage.setItem(storageKey, value);
    reset();
  };

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

  const fetchCatalog = async (kind) => {
    if (applying.current) return;
    const request = beginRead();
    const source = { kind, label: kind === 'modelsdev' ? 'models.dev' : url, skipped: 0 };
    setSession({ ...emptySession(), phase: 'fetch', source });
    try {
      const response = await API.get(kind === 'modelsdev' ? '/api/prices/modelsdev' : url, options(request));
      if (!current(request)) return;
      if (response.data?.success === false) throw responseError(response.data);
      const catalog =
        kind === 'modelsdev' ? response.data?.data?.prices : Array.isArray(response.data) ? response.data : response.data?.data;
      if (!Array.isArray(catalog) || !catalog.length) throw new Error(t('CheckUpdatesTable.dataFormatIncorrect'));
      if (kind === 'modelsdev') {
        if (!response.data?.success) throw new Error(t('CheckUpdatesTable.dataFormatIncorrect'));
        source.skipped = response.data.data.skipped || 0;
      }
      await preview(catalog, source, 'add', request);
    } catch (failure) {
      if (current(request)) setSession({ ...emptySession(), source, error: pricingSyncFailure(failure, 'fetch', t) });
    }
  };

  const chooseMode = (mode) => {
    if (!session.catalog || applying.current || mode === session.mode) return;
    return preview(session.catalog, session.source, mode, beginRead());
  };
  const retryPreview = () => {
    if (!session.catalog || applying.current) return;
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

  return { url, session, changeCount, editUrl, fetchCatalog, chooseMode, retryPreview, reset, apply };
}
