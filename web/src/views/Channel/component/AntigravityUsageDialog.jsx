import PropTypes from 'prop-types';
import { useCallback, useEffect, useRef, useState } from 'react';
import { useTranslation } from 'react-i18next';
import {
  Alert,
  Button,
  CircularProgress,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography
} from '@mui/material';
import { API } from 'utils/api';

function formatDate(value, unknown) {
  const date = new Date(value);
  return Number.isFinite(date.getTime()) ? date.toLocaleString() : unknown;
}

export default function AntigravityUsageDialog({ record, onClose }) {
  const { t } = useTranslation();
  const channelID = record.id;
  const request = useRef(null);
  const [state, setState] = useState({ loading: true, snapshot: null, error: '' });

  const refresh = useCallback(async () => {
    request.current?.abort();
    const controller = new AbortController();
    request.current = controller;
    setState({ loading: true, snapshot: null, error: '' });
    try {
      const token = localStorage.getItem('token');
      const response = await API.get(`/api/channel/${channelID}/antigravity/usage`, {
        signal: controller.signal,
        skipErrorNotification: true,
        ...(token ? { headers: { Authorization: token } } : {})
      });
      if (controller.signal.aborted) return;
      const result = response.data;
      if (!result?.success) {
        setState({ loading: false, snapshot: null, error: result?.message || null });
        return;
      }
      if (Number(result.data?.channel_id) !== Number(channelID) || !Array.isArray(result.data?.models)) {
        setState({ loading: false, snapshot: null, error: null });
        return;
      }
      setState({ loading: false, snapshot: result.data, error: '' });
    } catch (error) {
      if (!controller.signal.aborted) {
        setState({ loading: false, snapshot: null, error: error.response?.data?.message || null });
      }
    }
  }, [channelID]);

  useEffect(() => {
    refresh();
    return () => request.current?.abort();
  }, [refresh]);

  const { loading, snapshot, error } = state;
  const unknown = t('common.unknown');
  const fetchedAt = snapshot?.fetched_at;

  return (
    <Dialog open onClose={onClose} fullWidth maxWidth="md" aria-labelledby="antigravity-usage-title">
      <DialogTitle id="antigravity-usage-title">{t('channel_row.antigravityUsageDetail')}</DialogTitle>
      <DialogContent aria-busy={loading}>
        <Stack spacing={2}>
          <Typography variant="subtitle2">{`${record.name || ''} (#${channelID})`}</Typography>
          <Typography variant="body2" color="text.secondary">
            {t('channel_row.antigravityQuotaNote')}
          </Typography>
          {loading ? (
            <Stack direction="row" spacing={1} alignItems="center" role="status">
              <CircularProgress size={20} aria-label={t('channel_row.antigravityQuotaLoading')} />
              <Typography>{t('channel_row.antigravityQuotaLoading')}</Typography>
            </Stack>
          ) : snapshot ? (
            <>
              <Typography variant="caption" color="text.secondary">
                {t('channel_row.antigravityFetchedAt', {
                  time: typeof fetchedAt === 'number' && fetchedAt > 0 ? formatDate(fetchedAt * 1000, unknown) : unknown
                })}
              </Typography>
              {snapshot.models.length === 0 ? (
                <Alert severity="info">{t('channel_row.antigravityQuotaEmpty')}</Alert>
              ) : (
                <Table size="small" aria-label={t('channel_row.antigravityUsageDetail')}>
                  <TableHead>
                    <TableRow>
                      <TableCell>{t('channel_row.antigravityQuotaModel')}</TableCell>
                      <TableCell>{t('channel_row.antigravityQuotaRemaining')}</TableCell>
                      <TableCell>{t('channel_row.antigravityQuotaReset')}</TableCell>
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {snapshot.models.map((model) => {
                      const remaining = model.remaining_fraction;
                      const valid = typeof remaining === 'number' && Number.isFinite(remaining) && remaining >= 0 && remaining <= 1;
                      return (
                        <TableRow key={model.model_id}>
                          <TableCell component="th" scope="row" sx={{ overflowWrap: 'anywhere' }}>
                            {model.display_name && model.display_name !== model.model_id ? (
                              <Typography variant="body2">{model.display_name}</Typography>
                            ) : null}
                            <Typography variant="body2">{model.model_id}</Typography>
                          </TableCell>
                          <TableCell>{valid ? `${Number((remaining * 100).toFixed(2))}%` : unknown}</TableCell>
                          <TableCell>{model.reset_time ? formatDate(model.reset_time, unknown) : unknown}</TableCell>
                        </TableRow>
                      );
                    })}
                  </TableBody>
                </Table>
              )}
            </>
          ) : (
            <Alert severity="warning">{error || t('channel_row.antigravityQuotaUnavailable')}</Alert>
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={refresh} disabled={loading}>
          {t('channel_row.antigravityQuotaRefresh')}
        </Button>
        <Button onClick={onClose}>{t('common.close')}</Button>
      </DialogActions>
    </Dialog>
  );
}

AntigravityUsageDialog.propTypes = {
  record: PropTypes.object.isRequired,
  onClose: PropTypes.func.isRequired
};
