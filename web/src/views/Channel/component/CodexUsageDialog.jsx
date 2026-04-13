import PropTypes from 'prop-types';
import { useEffect, useState } from 'react';
import { useTranslation } from 'react-i18next';
import { Icon } from '@iconify/react';

import {
  Alert,
  Box,
  Button,
  Chip,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  Grid,
  LinearProgress,
  Paper,
  Stack,
  Typography,
  Accordion,
  AccordionDetails,
  AccordionSummary,
  CircularProgress
} from '@mui/material';

import {
  formatCodexFetchedAt,
  formatCodexPlanType,
  formatCodexResetAt,
  formatCodexResetCountdown,
  formatCodexWindowSummary,
  getCodexUsageWindow,
  resolveCodexUsageRatio
} from './codexUsage';

function usageStatusMeta(t, snapshot) {
  if (snapshot?.allowed === true && snapshot?.limit_reached !== true) {
    return { label: t('channel_row.codexAvailable'), color: 'success' };
  }
  if (snapshot?.limit_reached === true || snapshot?.allowed === false) {
    return { label: t('channel_row.codexLimited'), color: 'error' };
  }
  return { label: t('channel_row.codexPending'), color: 'default' };
}

function usageToneColor(theme, ratio) {
  if (ratio == null) {
    return theme.palette.text.disabled;
  }
  if (ratio >= 0.9) {
    return theme.palette.error.main;
  }
  if (ratio >= 0.7) {
    return theme.palette.warning.main;
  }
  return theme.palette.success.main;
}

function UsageWindowCard({ t, title, windowData, nowSeconds }) {
  const ratio = resolveCodexUsageRatio(windowData);
  const progressValue = ratio == null ? 0 : Math.min(100, Math.max(0, ratio * 100));
  const ratioLabel = ratio == null ? '--' : `${Math.round(ratio * 100)}%`;

  return (
    <Paper
      variant="outlined"
      sx={{
        p: 2,
        height: '100%'
      }}
    >
      <Stack spacing={1.5}>
        <Typography variant="subtitle2" color="text.secondary">
          {title}
        </Typography>
        <Typography variant="h4">{formatCodexWindowSummary(windowData)}</Typography>
        <LinearProgress
          variant="determinate"
          value={progressValue}
          sx={{
            height: 8,
            borderRadius: 999,
            '& .MuiLinearProgress-bar': {
              backgroundColor: (theme) => usageToneColor(theme, ratio)
            }
          }}
        />
        <Stack spacing={0.5}>
          <Typography variant="caption" color="text.secondary">
            {ratioLabel}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {windowData ? `${windowData.window_seconds || '--'}s` : '--'}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {`${t('channel_row.codexResetTime')}: ${formatCodexResetAt(windowData)}`}
          </Typography>
          <Typography variant="caption" color="text.secondary">
            {`${t('channel_row.codexResetIn')}: ${formatCodexResetCountdown(windowData, t, nowSeconds)}`}
          </Typography>
        </Stack>
      </Stack>
    </Paper>
  );
}

export default function CodexUsageDialog({ open, record, snapshot, loading, errorText, onRefresh, onClose }) {
  const { t } = useTranslation();
  const [nowSeconds, setNowSeconds] = useState(() => Math.floor(Date.now() / 1000));
  const statusMeta = usageStatusMeta(t, snapshot);
  const fiveHourWindow = getCodexUsageWindow(snapshot, 'five_hour');
  const weeklyWindow = getCodexUsageWindow(snapshot, 'weekly');
  const rawPayload = snapshot?.raw == null ? '' : typeof snapshot.raw === 'string' ? snapshot.raw : JSON.stringify(snapshot.raw, null, 2);

  useEffect(() => {
    if (!open) {
      return undefined;
    }

    setNowSeconds(Math.floor(Date.now() / 1000));
    const timer = window.setInterval(() => {
      setNowSeconds(Math.floor(Date.now() / 1000));
    }, 1000);

    return () => {
      window.clearInterval(timer);
    };
  }, [open]);

  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="md">
      <DialogTitle>{t('channel_row.codexUsageDetail')}</DialogTitle>
      <DialogContent dividers>
        {loading && !snapshot ? (
          <Box sx={{ display: 'flex', justifyContent: 'center', py: 6 }}>
            <CircularProgress size={28} />
          </Box>
        ) : (
          <Stack spacing={2}>
            {errorText && <Alert severity="warning">{errorText || t('channel_row.codexUsageUnavailable')}</Alert>}

            <Paper variant="outlined" sx={{ p: 2 }}>
              <Stack direction={{ xs: 'column', sm: 'row' }} spacing={1.5} justifyContent="space-between" alignItems={{ sm: 'center' }}>
                <Stack direction="row" spacing={1} alignItems="center" flexWrap="wrap">
                  <Typography variant="subtitle1" sx={{ fontWeight: 700 }}>
                    {record?.name || '--'}
                  </Typography>
                  <Chip label={formatCodexPlanType(snapshot?.plan_type)} size="small" color="primary" variant="outlined" />
                  <Chip label={statusMeta.label} size="small" color={statusMeta.color} />
                  <Chip
                    label={`${t('channel_row.codexUpstreamStatus')}: ${snapshot?.upstream_status ?? '--'}`}
                    size="small"
                    variant="outlined"
                  />
                </Stack>
                <Button size="small" onClick={onRefresh} disabled={loading} startIcon={<Icon icon="solar:refresh-bold-duotone" />}>
                  {t('channel_row.codexRefresh')}
                </Button>
              </Stack>

              <Divider sx={{ my: 2 }} />

              <Grid container spacing={2}>
                <Grid item xs={12} sm={4}>
                  <Typography variant="caption" color="text.secondary">
                    {t('channel_row.codexUserId')}
                  </Typography>
                  <Typography variant="body2" sx={{ wordBreak: 'break-all' }}>
                    {snapshot?.account?.user_id || '--'}
                  </Typography>
                </Grid>
                <Grid item xs={12} sm={4}>
                  <Typography variant="caption" color="text.secondary">
                    {t('channel_row.codexEmail')}
                  </Typography>
                  <Typography variant="body2" sx={{ wordBreak: 'break-all' }}>
                    {snapshot?.account?.email || '--'}
                  </Typography>
                </Grid>
                <Grid item xs={12} sm={4}>
                  <Typography variant="caption" color="text.secondary">
                    {t('channel_row.codexAccountId')}
                  </Typography>
                  <Typography variant="body2" sx={{ wordBreak: 'break-all' }}>
                    {snapshot?.account?.account_id || '--'}
                  </Typography>
                </Grid>
                <Grid item xs={12}>
                  <Typography variant="caption" color="text.secondary">
                    {t('channel_row.codexFetchedAt')}
                  </Typography>
                  <Typography variant="body2">{formatCodexFetchedAt(snapshot)}</Typography>
                </Grid>
              </Grid>
            </Paper>

            <Grid container spacing={2}>
              <Grid item xs={12} md={6}>
                <UsageWindowCard t={t} title={t('channel_row.codex5hWindow')} windowData={fiveHourWindow} nowSeconds={nowSeconds} />
              </Grid>
              <Grid item xs={12} md={6}>
                <UsageWindowCard t={t} title={t('channel_row.codexWeeklyWindow')} windowData={weeklyWindow} nowSeconds={nowSeconds} />
              </Grid>
            </Grid>

            <Accordion disableGutters>
              <AccordionSummary expandIcon={<Icon icon="solar:alt-arrow-down-bold" />}>
                <Typography variant="subtitle2">{t('channel_row.codexRawPayload')}</Typography>
              </AccordionSummary>
              <AccordionDetails>
                <Box
                  component="pre"
                  sx={{
                    m: 0,
                    p: 2,
                    borderRadius: 1,
                    bgcolor: 'grey.100',
                    overflow: 'auto',
                    fontSize: '0.75rem',
                    maxHeight: 360
                  }}
                >
                  {rawPayload || '--'}
                </Box>
              </AccordionDetails>
            </Accordion>
          </Stack>
        )}
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose}>{t('common.close')}</Button>
      </DialogActions>
    </Dialog>
  );
}

CodexUsageDialog.propTypes = {
  open: PropTypes.bool,
  record: PropTypes.object,
  snapshot: PropTypes.object,
  loading: PropTypes.bool,
  errorText: PropTypes.string,
  onRefresh: PropTypes.func,
  onClose: PropTypes.func
};

UsageWindowCard.propTypes = {
  t: PropTypes.func,
  title: PropTypes.string,
  windowData: PropTypes.object,
  nowSeconds: PropTypes.number
};
