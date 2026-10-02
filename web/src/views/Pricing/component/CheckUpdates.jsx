import PropTypes from 'prop-types';
import { useCallback, useEffect, useRef, useState } from 'react';
import {
  Alert,
  Box,
  Button,
  ButtonGroup,
  Card,
  CardContent,
  Chip,
  Checkbox,
  MenuItem,
  Dialog,
  DialogActions,
  DialogContent,
  Divider,
  IconButton,
  Stack,
  TextField,
  Tooltip,
  Typography
} from '@mui/material';
import LoadingButton from '@mui/lab/LoadingButton';
import { Icon } from '@iconify/react';
import { useTranslation } from 'react-i18next';
import { API } from 'utils/api';
import { showError, showSuccess } from 'utils/common';
import { canAcceptDefaultPricingUrl, createPricingFetchController, resolveDefaultPricingUrl } from './pricingFetchState.mjs';

import { modelsDevSelectionKey, modelsDevSelectedSource, selectModelsDevCandidate, ordinaryPricingMode } from './modelsDevState.mjs';

const updateModes = ['add', 'update', 'overwrite'];

const actionColor = {
  add: 'success',
  update: 'warning',
  delete: 'error',
  locked: 'default'
};

const policyText = (policy) => (policy == null ? '—' : JSON.stringify(policy, null, 2));
const PRICE_UPDATE_URL_STORAGE_KEY = 'oneapi_price_update_url';

export const CheckUpdates = ({ open, onCancel, onOk }) => {
  const { t } = useTranslation();
  const [url, setUrl] = useState(() => localStorage.getItem(PRICE_UPDATE_URL_STORAGE_KEY) || '');
  const urlRef = useRef(url);
  const userEditedUrlRef = useRef(false);
  const [loading, setLoading] = useState(false);
  const [applyLoading, setApplyLoading] = useState(false);
  const [mode, setMode] = useState('overwrite');
  const [source, setSource] = useState([]);
  const [preview, setPreview] = useState(null);
  const [modelsDev, setModelsDev] = useState(null);
  const [selectedModelsDev, setSelectedModelsDev] = useState({});
  const [providerFilter, setProviderFilter] = useState('');
  const [modelFilter, setModelFilter] = useState('');
  const [candidatePage, setCandidatePage] = useState(0);
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
      setMode(ordinaryPricingMode);
      setModelsDev(null);
      setSelectedModelsDev({});
      setPreview(null);
      setLoading(false);
    }
  }, [open]);

  const requestPreview = useCallback(
    async (catalog, selectedMode, requestGeneration) => {
      const response = await API.post('/api/prices/sync/preview', { mode: selectedMode, source: catalog });
      if (!catalogRequestController.current.isCurrent(requestGeneration)) {
        return;
      }
      if (!response.data?.success) {
        throw new Error(response.data?.message || t('CheckUpdatesTable.dataFormatIncorrect'));
      }
      setPreview(response.data.data);
    },
    [t]
  );

  const handleUrlChange = (event) => {
    const nextUrl = event.target.value;
    userEditedUrlRef.current = true;
    urlRef.current = nextUrl;
    defaultUrlController.current.invalidate();
    catalogRequestController.current.invalidate();
    setModelsDev(null);
    setSelectedModelsDev({});
    setMode(ordinaryPricingMode);
    setUrl(nextUrl);
    setSource([]);
    setPreview(null);
    setLoading(false);
    localStorage.setItem(PRICE_UPDATE_URL_STORAGE_KEY, nextUrl);
  };

  const handleCheckUpdates = async () => {
    const selectedMode = ordinaryPricingMode(mode);
    setMode(selectedMode);
    const requestGeneration = catalogRequestController.current.begin();
    setModelsDev(null);
    setSelectedModelsDev({});
    setLoading(true);
    setSource([]);
    setPreview(null);
    try {
      const response = await API.get(url);
      const catalog = Array.isArray(response?.data) ? response.data : response?.data?.data;
      if (!Array.isArray(catalog) || catalog.length === 0) {
        throw new Error(t('CheckUpdatesTable.dataFormatIncorrect'));
      }
      if (!catalogRequestController.current.isCurrent(requestGeneration)) {
        return;
      }
      setSource(catalog);
      await requestPreview(catalog, selectedMode, requestGeneration);
    } catch (error) {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        showError(error.response?.data?.message || error.message);
      }
    } finally {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        setLoading(false);
      }
    }
  };

  const fetchModelsDev = async () => {
    const generation = catalogRequestController.current.begin();
    setLoading(true);
    setModelsDev(null);
    setSelectedModelsDev({});
    setSource([]);
    setPreview(null);
    setCandidatePage(0);
    setProviderFilter('');
    setModelFilter('');
    try {
      const response = await API.get('/api/prices/modelsdev');
      if (!catalogRequestController.current.isCurrent(generation)) return;
      if (!response.data?.success || !Array.isArray(response.data?.data?.candidates)) {
        throw new Error(response.data?.message || t('CheckUpdatesTable.dataFormatIncorrect'));
      }
      setModelsDev(response.data.data);
      setMode('merge');
    } catch (error) {
      if (catalogRequestController.current.isCurrent(generation)) showError(error.response?.data?.message || error.message);
    } finally {
      if (catalogRequestController.current.isCurrent(generation)) setLoading(false);
    }
  };

  const selectCandidate = (candidate, checked) => {
    const next = selectModelsDevCandidate(selectedModelsDev, candidate, checked);
    catalogRequestController.current.invalidate();
    setSelectedModelsDev(next);
    setSource(modelsDevSelectedSource(next));
    setPreview(null);
    setLoading(false);
  };

  const handleModeChange = async (selectedMode) => {
    setMode(selectedMode);
    setPreview(null);
    if (source.length === 0) {
      return;
    }
    const requestGeneration = catalogRequestController.current.begin();
    setLoading(true);
    try {
      await requestPreview(source, selectedMode, requestGeneration);
    } catch (error) {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        showError(error.response?.data?.message || error.message);
      }
    } finally {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        setLoading(false);
      }
    }
  };

  const applyPreview = async () => {
    if (!preview || source.length === 0) {
      showError(t('CheckUpdatesTable.pleaseFetchData'));
      return;
    }
    setApplyLoading(true);
    try {
      const response = await API.post('/api/prices/sync/apply', {
        mode,
        source,
        base_version: preview.base_version,
        digest: preview.digest
      });
      if (response.data?.success) {
        showSuccess(t('CheckUpdatesTable.operationCompleted'));
        onOk(true);
      } else {
        showError(response.data?.message);
      }
    } catch (error) {
      showError(error.response?.data?.message || error.message);
      setPreview(null);
    } finally {
      setApplyLoading(false);
    }
  };

  const refreshPreview = async () => {
    if (source.length === 0) return;
    const requestGeneration = catalogRequestController.current.begin();
    setLoading(true);
    try {
      await requestPreview(source, mode, requestGeneration);
    } catch (error) {
      if (catalogRequestController.current.isCurrent(requestGeneration)) {
        showError(error.response?.data?.message || error.message);
      }
    } finally {
      if (catalogRequestController.current.isCurrent(requestGeneration)) setLoading(false);
    }
  };

  const changes = preview?.plan?.changes || [];
  const providers = [...new Set((modelsDev?.candidates || []).map((candidate) => candidate.provider))].sort();
  const filteredCandidates = (modelsDev?.candidates || []).filter(
    (candidate) =>
      (!providerFilter || candidate.provider === providerFilter) && candidate.model.toLowerCase().includes(modelFilter.toLowerCase())
  );
  const visibleCandidates = filteredCandidates.slice(candidatePage * 100, (candidatePage + 1) * 100);

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
            placeholder={t('CheckUpdatesTable.url')}
            value={url}
            onChange={handleUrlChange}
            disabled={loading || applyLoading}
            InputProps={{
              endAdornment: (
                <Tooltip title={t('CheckUpdatesTable.fetchData')}>
                  <IconButton onClick={handleCheckUpdates} disabled={loading || applyLoading || !url} color="primary" size="small">
                    <Icon icon={loading ? 'svg-spinners:180-ring' : 'solar:refresh-bold'} fontSize="1.2rem" />
                  </IconButton>
                </Tooltip>
              )
            }}
          />

          <Button onClick={fetchModelsDev} disabled={loading || applyLoading} variant="outlined">
            {t('modelsDev.fetch')}
          </Button>
          {modelsDev && (
            <Stack spacing={1}>
              <Alert severity="info">{t('modelsDev.policy')}</Alert>
              <Typography variant="caption">
                {modelsDev.url} · {t('modelsDev.selected')}: {source.length}
              </Typography>
              <Stack direction="row" spacing={1}>
                <TextField
                  select
                  size="small"
                  label={t('modelsDev.provider')}
                  value={providerFilter}
                  onChange={(event) => {
                    setProviderFilter(event.target.value);
                    setCandidatePage(0);
                  }}
                  sx={{ minWidth: 180 }}
                >
                  <MenuItem value="">{t('common.all')}</MenuItem>
                  {providers.map((provider) => (
                    <MenuItem key={provider} value={provider}>
                      {provider}
                    </MenuItem>
                  ))}
                </TextField>
                <TextField
                  size="small"
                  label={t('common.search')}
                  value={modelFilter}
                  onChange={(event) => {
                    setModelFilter(event.target.value);
                    setCandidatePage(0);
                  }}
                  fullWidth
                />
              </Stack>
              {visibleCandidates.map((candidate) => (
                <Stack key={modelsDevSelectionKey(candidate)} direction="row" spacing={1} alignItems="center">
                  <Checkbox
                    checked={selectedModelsDev[candidate.model]?.provider === candidate.provider}
                    disabled={!candidate.price || Boolean(candidate.reason) || loading || applyLoading}
                    onChange={(event) => selectCandidate(candidate, event.target.checked)}
                    inputProps={{ 'aria-label': `${candidate.provider}: ${candidate.model}` }}
                  />
                  <Box sx={{ minWidth: 0, overflowWrap: 'anywhere' }}>
                    <Typography variant="body2">
                      {candidate.provider} / {candidate.model}
                    </Typography>
                    <Typography variant="caption" color={candidate.reason ? 'error' : 'text.secondary'}>
                      {candidate.reason || `input: ${candidate.price?.input}, output: ${candidate.price?.output}`}
                    </Typography>
                  </Box>
                  {candidate.conflict && <Chip size="small" color="warning" label={t('modelsDev.conflict')} />}
                </Stack>
              ))}
              <Stack direction="row" spacing={1} alignItems="center">
                <Button disabled={candidatePage === 0} onClick={() => setCandidatePage(candidatePage - 1)}>
                  ←
                </Button>
                <Typography variant="caption">
                  {candidatePage + 1} / {Math.max(1, Math.ceil(filteredCandidates.length / 100))} ({filteredCandidates.length})
                </Typography>
                <Button
                  disabled={(candidatePage + 1) * 100 >= filteredCandidates.length}
                  onClick={() => setCandidatePage(candidatePage + 1)}
                >
                  →
                </Button>
              </Stack>
            </Stack>
          )}

          {!modelsDev && (
            <ButtonGroup fullWidth aria-label={t('CheckUpdatesTable.updatePrices')}>
              {updateModes.map((item) => (
                <Button
                  key={item}
                  variant={mode === item ? 'contained' : 'outlined'}
                  onClick={() => handleModeChange(item)}
                  disabled={loading || applyLoading}
                >
                  {t(`CheckUpdatesTable.updateMode${item.charAt(0).toUpperCase()}${item.slice(1)}`)}
                </Button>
              ))}
            </ButtonGroup>
          )}

          {source.length > 0 && !preview && (
            <Button variant="outlined" onClick={refreshPreview} disabled={loading || applyLoading}>
              {t('CheckUpdatesTable.fetchData')}
            </Button>
          )}

          {preview && (
            <>
              <Card variant="outlined">
                <CardContent sx={{ p: '12px !important' }}>
                  <Stack direction="row" spacing={1} useFlexGap flexWrap="wrap">
                    <Chip label={`${t('CheckUpdatesTable.priceServerTotal')}: ${source.length}`} color="primary" size="small" />
                    <Chip label={`base_version: ${preview.base_version}`} size="small" />
                    {Object.entries(actionColor).map(([action, color]) => (
                      <Chip
                        key={action}
                        label={`${action}: ${changes.filter((change) => change.action === action).length}`}
                        color={color}
                        size="small"
                      />
                    ))}
                  </Stack>
                </CardContent>
              </Card>

              {changes.length === 0 ? (
                <Alert severity="success">{t('CheckUpdatesTable.noUpdates')}</Alert>
              ) : (
                <Stack spacing={1.5}>
                  {changes.map((change) => (
                    <Card key={`${change.action}:${change.model}`} variant="outlined">
                      <CardContent>
                        <Stack direction="row" spacing={1} alignItems="center" sx={{ mb: 1 }}>
                          <Chip label={change.action} color={actionColor[change.action] || 'default'} size="small" />
                          <Typography variant="subtitle2">{change.model}</Typography>
                        </Stack>
                        <Stack direction={{ xs: 'column', md: 'row' }} spacing={2}>
                          <Box sx={{ minWidth: 0, flex: 1 }}>
                            <Typography variant="caption" color="text.secondary">
                              before
                            </Typography>
                            <Box component="pre" sx={{ m: 0, p: 1, overflow: 'auto', bgcolor: 'action.hover', fontSize: 11 }}>
                              {policyText(change.before)}
                            </Box>
                          </Box>
                          <Box sx={{ minWidth: 0, flex: 1 }}>
                            <Typography variant="caption" color="text.secondary">
                              after
                            </Typography>
                            <Box component="pre" sx={{ m: 0, p: 1, overflow: 'auto', bgcolor: 'action.hover', fontSize: 11 }}>
                              {policyText(change.after)}
                            </Box>
                          </Box>
                        </Stack>
                      </CardContent>
                    </Card>
                  ))}
                </Stack>
              )}
            </>
          )}
        </Stack>
      </DialogContent>

      <DialogActions sx={{ px: 2, py: 1.5, justifyContent: 'space-between' }}>
        <Button onClick={onCancel} disabled={applyLoading} variant="outlined" color="inherit">
          {t('CheckUpdatesTable.cancel')}
        </Button>
        <LoadingButton
          variant="contained"
          onClick={applyPreview}
          loading={applyLoading}
          disabled={!preview || loading || changes.length === 0}
        >
          {t(`CheckUpdatesTable.updateMode${mode.charAt(0).toUpperCase()}${mode.slice(1)}`)}
        </LoadingButton>
      </DialogActions>
    </Dialog>
  );
};

CheckUpdates.propTypes = {
  open: PropTypes.bool,
  onCancel: PropTypes.func,
  onOk: PropTypes.func
};
