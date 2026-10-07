import PropTypes from 'prop-types';
import {
  Alert,
  Box,
  Button,
  Card,
  Dialog,
  DialogActions,
  DialogContent,
  Divider,
  FormControlLabel,
  IconButton,
  LinearProgress,
  Radio,
  RadioGroup,
  Stack,
  Typography
} from '@mui/material';
import LoadingButton from '@mui/lab/LoadingButton';
import { Icon } from '@iconify/react';
import { useTranslation } from 'react-i18next';
import { showSuccess } from 'utils/common';
import PricingSyncChanges from './PricingSyncChanges';
import usePricingSync from './usePricingSync';
import ModelsDevSources from './ModelsDevSources';

const modes = ['add', 'update', 'overwrite'];

export const CheckUpdates = ({ open, onCancel, onOk }) => {
  const { t } = useTranslation();
  const sync = usePricingSync(open, t);
  const { session } = sync;
  const reviewing = session.catalog !== null;
  const applying = session.phase === 'apply';
  const fetching = session.phase === 'fetch';
  const calculating = session.phase === 'preview';
  const close = () => {
    if (!applying) {
      sync.reset();
      onCancel();
    }
  };
  const apply = async () => {
    if (await sync.apply()) {
      showSuccess(t('CheckUpdatesTable.operationCompleted'));
      onOk(true);
    }
  };

  return (
    <Dialog
      open={open}
      onClose={applying ? undefined : close}
      fullWidth
      maxWidth="md"
      aria-labelledby="pricing-sync-title"
      PaperProps={{
        sx: {
          borderRadius: 2,
          m: { xs: 1.5, sm: 4 },
          width: { xs: 'calc(100% - 24px)', sm: '100%' },
          maxHeight: { xs: 'calc(100% - 24px)', sm: 'calc(100% - 64px)' }
        }
      }}
    >
      <Stack direction="row" alignItems="center" spacing={2} sx={{ px: { xs: 2, sm: 3 }, py: 2.5 }}>
        <Box sx={{ bgcolor: 'primary.lighter', color: 'primary.main', display: 'flex', p: 1.25, borderRadius: 2 }}>
          <Icon icon="solar:refresh-bold" width={24} />
        </Box>
        <Box sx={{ flex: 1 }}>
          <Typography id="pricing-sync-title" variant="h5">
            {t('pricingSync.title')}
          </Typography>
          <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
            {t(reviewing ? 'pricingSync.reviewIntro' : 'pricingSync.sourceIntro')}
          </Typography>
        </Box>
        <IconButton onClick={close} disabled={applying} aria-label={t('common.close')}>
          <Icon icon="solar:close-circle-linear" width={24} />
        </IconButton>
      </Stack>
      <Divider />
      <DialogContent sx={{ p: { xs: 2, sm: 3 } }}>
        <Stack spacing={2.5}>
          {!reviewing ? (
            <>
              <Card variant="outlined" sx={{ p: 2, bgcolor: 'background.neutral' }}>
                <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} alignItems={{ sm: 'center' }}>
                  <Box sx={{ flex: 1 }}>
                    <Typography variant="subtitle1">models.dev</Typography>
                    <Typography variant="body2" color="text.secondary" sx={{ mt: 0.5 }}>
                      {t('pricingSync.modelsDevHelp')}
                    </Typography>
                  </Box>
                  <LoadingButton
                    variant="outlined"
                    loading={fetching}
                    disabled={fetching}
                    onClick={sync.fetchCatalog}
                    sx={{ flexShrink: 0 }}
                  >
                    {t('modelsDev.fetch')}
                  </LoadingButton>
                </Stack>
              </Card>
              <Typography variant="caption" color="text.secondary">
                {t('pricingSync.fetchHelp')}
              </Typography>
            </>
          ) : (
            <>
              <Stack direction="row" alignItems="center" justifyContent="space-between" spacing={1}>
                <Box sx={{ minWidth: 0 }}>
                  <Typography variant="caption" color="text.secondary">
                    {t('pricingSync.loadedSource')}
                  </Typography>
                  <Typography variant="body2" sx={{ fontWeight: 600, overflowWrap: 'anywhere' }}>
                    {session.source.label}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {t('pricingSync.total', { count: session.catalog.length })}
                    {session.source.skipped > 0 && ` · ${t('pricingSync.skippedShort', { count: session.source.skipped })}`}
                  </Typography>
                </Box>
                <Button onClick={sync.fetchCatalog} disabled={applying} size="small" sx={{ flexShrink: 0 }}>
                  {t('pricingSync.refetch')}
                </Button>
              </Stack>
              <ModelsDevSources candidates={session.source.candidates} />
              {session.catalog.length === 0 ? (
                <Alert severity="info">{t('pricingSync.noUsablePrices')}</Alert>
              ) : (
                <Box>
                  <Typography variant="subtitle1" sx={{ mb: 1.25 }}>
                    {t('pricingSync.modeLabel')}
                  </Typography>
                  <RadioGroup
                    value={session.mode}
                    onChange={(event) => sync.chooseMode(event.target.value)}
                    aria-label={t('pricingSync.modeLabel')}
                    sx={{ display: 'grid', gridTemplateColumns: { xs: '1fr', sm: 'repeat(3, minmax(0, 1fr))' }, gap: 1 }}
                  >
                    {modes.map((mode) => (
                      <FormControlLabel
                        key={mode}
                        value={mode}
                        disabled={applying}
                        control={<Radio size="small" />}
                        sx={{
                          m: 0,
                          p: 1,
                          alignItems: 'flex-start',
                          border: 1,
                          borderColor: session.mode === mode ? 'primary.main' : 'divider',
                          borderRadius: 1.5,
                          bgcolor: session.mode === mode ? 'action.selected' : 'transparent',
                          '& .MuiFormControlLabel-label': { minWidth: 0 }
                        }}
                        label={
                          <Box sx={{ py: 0.5 }}>
                            <Typography variant="subtitle2">
                              {t(`CheckUpdatesTable.updateMode${mode.charAt(0).toUpperCase()}${mode.slice(1)}`)}
                            </Typography>
                            <Typography variant="caption" color="text.secondary">
                              {t(`pricingSync.modeShort.${mode}`)}
                            </Typography>
                          </Box>
                        }
                      />
                    ))}
                  </RadioGroup>
                </Box>
              )}
              {session.mode === 'overwrite' && (
                <Alert severity="warning" variant="outlined">
                  {t('pricingSync.overwriteNotice')}
                </Alert>
              )}
            </>
          )}
          {(fetching || calculating) && (
            <Box role="status" aria-live="polite">
              <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
                {t(calculating ? 'pricingSync.calculating' : 'pricingSync.fetching')}
              </Typography>
              <LinearProgress />
            </Box>
          )}
          {session.error && (
            <Alert severity="error" sx={{ '& .MuiAlert-message': { minWidth: 0, overflowWrap: 'anywhere' } }}>
              <Typography variant="body2">{session.error.message}</Typography>
              {session.error.retry && reviewing && (
                <Button onClick={sync.retryPreview} size="small" color="inherit" sx={{ mt: 1 }}>
                  {t('pricingSync.retryPreview')}
                </Button>
              )}
            </Alert>
          )}
          {session.preview && (
            <>
              <PricingSyncChanges changes={session.preview.plan.changes} />
              {session.mode !== 'add' && (
                <Typography variant="caption" color="text.secondary">
                  {t('pricingSync.rulesNotice')}
                </Typography>
              )}
            </>
          )}
        </Stack>
      </DialogContent>
      <Divider />
      <DialogActions sx={{ px: { xs: 2, sm: 3 }, py: 2, justifyContent: 'space-between', gap: 1 }}>
        <Button onClick={close} disabled={applying} color="inherit">
          {t('CheckUpdatesTable.cancel')}
        </Button>
        {reviewing && session.catalog.length > 0 && (
          <LoadingButton
            variant="contained"
            onClick={apply}
            loading={applying}
            disabled={!session.preview || calculating || sync.changeCount === 0}
            sx={{ px: { xs: 2, sm: 3 } }}
          >
            {session.preview ? t('pricingSync.apply', { count: sync.changeCount }) : t('pricingSync.applyPending')}
          </LoadingButton>
        )}
      </DialogActions>
    </Dialog>
  );
};
CheckUpdates.propTypes = {
  open: PropTypes.bool,
  onCancel: PropTypes.func.isRequired,
  onOk: PropTypes.func.isRequired
};
