import PropTypes from 'prop-types';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  ButtonGroup,
  Dialog,
  DialogActions,
  DialogContent,
  Divider,
  IconButton,
  LinearProgress,
  Stack,
  TextField,
  Tooltip,
  Typography
} from '@mui/material';
import LoadingButton from '@mui/lab/LoadingButton';
import { Icon } from '@iconify/react';
import { useTranslation } from 'react-i18next';
import { API } from 'utils/api';
import { showSuccess } from 'utils/common';
import { canAcceptDefaultPricingUrl, createPricingFetchController, resolveDefaultPricingUrl } from './pricingFetchState.mjs';

import PricingSyncChanges from './PricingSyncChanges';
import { pricingSyncFailure, pricingSyncRequestOptions } from './pricingSyncFeedback.mjs';

const updateModes = ['add', 'update', 'overwrite'];

const PRICE_UPDATE_URL_STORAGE_KEY = 'oneapi_price_update_url';

export const CheckUpdates = ({ open, onCancel, onOk, ownedby = [] }) => {
  const { t } = useTranslation();
  const [url, setUrl] = useState(() => localStorage.getItem(PRICE_UPDATE_URL_STORAGE_KEY) || '');
  const urlRef = useRef(url);
  const userEditedUrlRef = useRef(false);
  const [stage, setStage] = useState(null);
  const loading = stage !== null;
  const [error, setError] = useState(null);
  const [sourceLabel, setSourceLabel] = useState('');
  const [applyLoading, setApplyLoading] = useState(false);
  const [mode, setMode] = useState('add');
  const [source, setSource] = useState([]);
  const [preview, setPreview] = useState(null);
  const [modelsDevSkipped, setModelsDevSkipped] = useState(0);
  const defaultUrlController = useRef(null);
  const catalogRequestController = useRef(null);
  if (defaultUrlController.current === null) {
    defaultUrlController.current = createPricingFetchController();
  }
  if (catalogRequestController.current === null) {
    catalogRequestController.current = createPricingFetchController();
  }

  const fetchDefaultUrl = useCallback(async (requestGeneration) => {
    try {
      const response = await API.get('/api/prices/updateService');
      const nextUrl = resolveDefaultPricingUrl(response.data?.data);
      if (
        canAcceptDefaultPricingUrl({
          controller: defaultUrlController.current,
          generation: requestGeneration,
          currentUrl: urlRef.current,
          cachedUrl: localStorage.getItem(PRICE_UPDATE_URL_STORAGE_KEY),
          userEdited: userEditedUrlRef.current
        })
      ) {
        urlRef.current = nextUrl;
        setUrl(nextUrl);
        localStorage.setItem(PRICE_UPDATE_URL_STORAGE_KEY, nextUrl);
      }
    } catch {
      const fallbackUrl = resolveDefaultPricingUrl();
      if (
        canAcceptDefaultPricingUrl({
          controller: defaultUrlController.current,
          generation: requestGeneration,
          currentUrl: urlRef.current,
          cachedUrl: localStorage.getItem(PRICE_UPDATE_URL_STORAGE_KEY),
          userEdited: userEditedUrlRef.current
        })
      ) {
        urlRef.current = fallbackUrl;
        setUrl(fallbackUrl);
        localStorage.setItem(PRICE_UPDATE_URL_STORAGE_KEY, fallbackUrl);
      }
    }
  }, []);

  useEffect(() => {
    if (!localStorage.getItem(PRICE_UPDATE_URL_STORAGE_KEY)) {
      fetchDefaultUrl(defaultUrlController.current.begin());
    }
    return () => {
      defaultUrlController.current.invalidate();
      catalogRequestController.current.invalidate();
    };
  }, [fetchDefaultUrl]);

  useEffect(() => {
    if (!open) {
      catalogRequestController.current.invalidate();
      setSource([]);
      setModelsDevSkipped(0);
      setPreview(null);
      setStage(null);
      setError(null);
      setMode('add');
    }
  }, [open]);

  const requestPreview = useCallback(
    async (catalog, selectedMode, requestGeneration) => {
      setStage('preview');
      setPreview(null);
      setError(null);
      try {
        const response = await API.post('/api/prices/sync/preview', { mode: selectedMode, source: catalog }, pricingSyncRequestOptions);
        if (!catalogRequestController.current.isCurrent(requestGeneration)) return;
        if (!response.data?.success) throw Object.assign(new Error(response.data?.message), { data: response.data });
        setPreview(response.data.data);
      } catch (failure) {
        if (!catalogRequestController.current.isCurrent(requestGeneration)) return;
        const feedback = pricingSyncFailure(failure, 'preview', t);
        setError(feedback);
        if (feedback.invalidCatalog) setSource([]);
      } finally {
        if (catalogRequestController.current.isCurrent(requestGeneration)) setStage(null);
      }
    },
    [t]
  );

  const handleUrlChange = (event) => {
    const nextUrl = event.target.value;
    userEditedUrlRef.current = true;
    urlRef.current = nextUrl;
    defaultUrlController.current.invalidate();
    catalogRequestController.current.invalidate();
    setModelsDevSkipped(0);
    setUrl(nextUrl);
    setSource([]);
    setPreview(null);
    setStage(null);
    setError(null);
    localStorage.setItem(PRICE_UPDATE_URL_STORAGE_KEY, nextUrl);
  };

  const handleCheckUpdates = async (fromModelsDev = false) => {
    const requestGeneration = catalogRequestController.current.begin();
    setModelsDevSkipped(0);
    setStage(fromModelsDev ? 'fetch-modelsdev' : 'fetch-url');
    setError(null);
    setSource([]);
    setPreview(null);
    try {
      const response = await API.get(fromModelsDev ? '/api/prices/modelsdev' : url, pricingSyncRequestOptions);
      if (!catalogRequestController.current.isCurrent(requestGeneration)) return;
      let catalog;
      if (fromModelsDev) {
        if (!response.data?.success || !Array.isArray(response.data?.data?.prices)) {
          throw new Error(response.data?.message || t('CheckUpdatesTable.dataFormatIncorrect'));
        }
        catalog = response.data.data.prices;
        setModelsDevSkipped(response.data.data.skipped || 0);
      } else {
        catalog = Array.isArray(response?.data) ? response.data : response?.data?.data;
      }
      if (!Array.isArray(catalog) || catalog.length === 0) {
        throw new Error(t('CheckUpdatesTable.dataFormatIncorrect'));
      }
      if (!catalogRequestController.current.isCurrent(requestGeneration)) {
        return;
      }
      setSourceLabel(fromModelsDev ? 'models.dev' : url);
      setSource(catalog);
      await requestPreview(catalog, mode, requestGeneration);
    } catch (error) {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        setError(pricingSyncFailure(error, 'fetch', t));
      }
    } finally {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        setStage(null);
      }
    }
  };

  const handleModeChange = async (selectedMode) => {
    if (selectedMode === mode || source.length === 0) return;
    setMode(selectedMode);
    await requestPreview(source, selectedMode, catalogRequestController.current.begin());
  };

  const applyPreview = async () => {
    if (!preview || source.length === 0 || loading || applyLoading) return;
    setApplyLoading(true);
    setError(null);
    try {
      const response = await API.post(
        '/api/prices/sync/apply',
        {
          mode,
          source,
          base_version: preview.base_version,
          digest: preview.digest
        },
        pricingSyncRequestOptions
      );
      if (!response.data?.success) throw Object.assign(new Error(response.data?.message), { data: response.data });
      showSuccess(t('CheckUpdatesTable.operationCompleted'));
      onOk(true);
    } catch (failure) {
      setPreview(null);
      const feedback = pricingSyncFailure(failure, 'apply', t);
      setError(feedback);
      if (feedback.invalidCatalog) setSource([]);
    } finally {
      setApplyLoading(false);
    }
  };

  const refreshPreview = () => requestPreview(source, mode, catalogRequestController.current.begin());

  const changes = preview?.plan?.changes || [];
  const changeCount = changes.filter((change) => change.action !== 'locked').length;

  return (
    <Dialog open={open} onClose={applyLoading ? undefined : onCancel} fullWidth maxWidth="md">
      <Box sx={{ display: 'flex', alignItems: 'center', px: 2.5, py: 2 }}>
        <Icon icon="solar:restart-bold" width={20} height={20} style={{ marginRight: 8 }} />
        <Typography variant="h6" sx={{ flexGrow: 1 }}>
          {t('CheckUpdatesTable.checkUpdates')}
        </Typography>
        <IconButton edge="end" onClick={onCancel} disabled={applyLoading} aria-label={t('common.close')} size="small">
          <Icon icon="solar:close-circle-bold" />
        </IconButton>
      </Box>
      <Divider />

      <DialogContent sx={{ p: 2 }}>
        <Stack spacing={2}>
          <TextField
            fullWidth
            size="small"
            label={t('CheckUpdatesTable.url')}
            placeholder={t('CheckUpdatesTable.url')}
            value={url}
            onChange={handleUrlChange}
            disabled={loading || applyLoading}
            InputProps={{
              endAdornment: (
                <Tooltip title={t('CheckUpdatesTable.fetchData')}>
                  <IconButton onClick={() => handleCheckUpdates()} disabled={loading || applyLoading || !url} color="primary" size="small">
                    <Icon icon={stage === 'fetch-url' ? 'svg-spinners:180-ring' : 'solar:refresh-bold'} fontSize="1.2rem" />
                  </IconButton>
                </Tooltip>
              )
            }}
          />

          <LoadingButton
            onClick={() => handleCheckUpdates(true)}
            loading={stage === 'fetch-modelsdev'}
            disabled={loading || applyLoading}
            variant="outlined"
          >
            {t('modelsDev.fetch')}
          </LoadingButton>
          {loading && (
            <Box role="status" aria-live="polite">
              <Typography variant="body2" sx={{ mb: 1 }}>
                {t(stage === 'preview' ? 'pricingSync.calculating' : 'pricingSync.fetching')}
              </Typography>
              <LinearProgress />
            </Box>
          )}
          {error && !loading && (
            <Alert
              severity="error"
              action={
                error.retry && source.length > 0 ? (
                  <Button color="inherit" size="small" onClick={refreshPreview}>
                    {t('pricingSync.retryPreview')}
                  </Button>
                ) : undefined
              }
            >
              {error.message}
            </Alert>
          )}
          {modelsDevSkipped > 0 && <Alert severity="info">{t('modelsDev.skipped', { count: modelsDevSkipped })}</Alert>}

          {source.length > 0 && (
            <>
              <ButtonGroup fullWidth aria-label={t('pricingSync.modeLabel')}>
                {updateModes.map((item) => (
                  <Button
                    key={item}
                    variant={mode === item ? 'contained' : 'outlined'}
                    aria-pressed={mode === item}
                    onClick={() => handleModeChange(item)}
                    disabled={loading || applyLoading}
                  >
                    {t(`CheckUpdatesTable.updateMode${item.charAt(0).toUpperCase()}${item.slice(1)}`)}
                  </Button>
                ))}
              </ButtonGroup>

              <Typography variant="body2" color={mode === 'overwrite' ? 'error' : 'text.secondary'}>
                {t(`pricingSync.modeHelp.${mode}`)}
              </Typography>
              {mode !== 'add' && <Alert severity="warning">{t('CheckUpdatesTable.contextTierPolicy')}</Alert>}
            </>
          )}

          {preview && <PricingSyncChanges changes={changes} sourceCount={source.length} sourceLabel={sourceLabel} ownedby={ownedby} />}
        </Stack>
      </DialogContent>

      <DialogActions sx={{ px: 2, py: 1.5, justifyContent: 'space-between' }}>
        <Button onClick={onCancel} disabled={applyLoading} variant="outlined" color="inherit">
          {t('CheckUpdatesTable.cancel')}
        </Button>
        {source.length > 0 && (
          <LoadingButton
            variant="contained"
            onClick={applyPreview}
            loading={applyLoading}
            disabled={!preview || loading || changeCount === 0}
          >
            {t('pricingSync.apply', { count: changeCount })}
          </LoadingButton>
        )}
      </DialogActions>
    </Dialog>
  );
};

CheckUpdates.propTypes = {
  open: PropTypes.bool,
  onCancel: PropTypes.func,
  onOk: PropTypes.func,
  ownedby: PropTypes.array
};
