import PropTypes from 'prop-types';
import { Alert, Box, Card, CardContent, Chip, Divider, List, ListItem, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { pricingSyncRows } from './pricingSyncComparison.mjs';

const groups = { add: 'success', update: 'warning', delete: 'error', locked: 'default' };

export default function PricingSyncChanges({ changes, sourceCount, sourceLabel, ownedby = [] }) {
  const { t } = useTranslation();
  const channelName = (value) =>
    ownedby.find((channel) => channel.value === value)?.label || `${t('pricingSync.field.channel_type')} ${value}`;
  const mutableCount = changes.filter((change) => change.action !== 'locked').length;
  return (
    <Stack spacing={2}>
      <Card variant="outlined">
        <CardContent sx={{ p: '12px !important' }}>
          <Typography variant="caption" component="p" sx={{ mb: 1, overflowWrap: 'anywhere' }}>
            {t('pricingSync.source', { source: sourceLabel })}
          </Typography>
          <Stack direction="row" spacing={1} useFlexGap flexWrap="wrap">
            <Chip label={t('pricingSync.total', { count: sourceCount })} color="primary" size="small" />
            {Object.entries(groups).map(([action, color]) => (
              <Chip
                key={action}
                label={`${t(`pricingSync.action.${action}`)}: ${changes.filter((change) => change.action === action).length}`}
                color={color}
                size="small"
              />
            ))}
          </Stack>
        </CardContent>
      </Card>
      {mutableCount === 0 && <Alert severity="success">{t('pricingSync.noChanges')}</Alert>}
      {Object.entries(groups).map(([action, color]) => {
        const entries = changes.filter((change) => change.action === action);
        if (!entries.length) return null;
        return (
          <Card key={action} variant="outlined" component="section" aria-label={t(`pricingSync.action.${action}`)}>
            <Box sx={{ px: 2, py: 1.5, borderBottom: 1, borderColor: 'divider' }}>
              <Typography variant="subtitle2">
                {t(`pricingSync.action.${action}`)} ({entries.length})
              </Typography>
              {action === 'delete' && (
                <Typography variant="caption" color="error">
                  {t('pricingSync.deletionHelp')}
                </Typography>
              )}
            </Box>
            <List disablePadding sx={{ maxHeight: 320, overflow: 'auto' }}>
              {entries.map((change, index) => (
                <Box component="li" key={change.model}>
                  {index > 0 && <Divider />}
                  <ListItem component="div" sx={{ display: 'block', px: 2, py: 1.5 }}>
                    <Chip
                      label={change.model}
                      variant="outlined"
                      color={color}
                      size="small"
                      sx={{
                        mb: 1,
                        maxWidth: '100%',
                        height: 'auto',
                        '& .MuiChip-label': { whiteSpace: 'normal', overflowWrap: 'anywhere' }
                      }}
                    />
                    {action === 'locked' ? (
                      <Typography variant="body2">{t('pricingSync.lockedHelp')}</Typography>
                    ) : (
                      <Stack spacing={0.5}>
                        {pricingSyncRows(change.before, change.after, t, channelName).map((row) => (
                          <Box
                            key={row.key}
                            sx={{
                              display: 'grid',
                              gridTemplateColumns: { xs: '1fr', sm: '140px minmax(0, 1fr)' },
                              gap: { xs: 0, sm: 1 },
                              fontSize: 13
                            }}
                          >
                            <Typography variant="body2" color="text.secondary">
                              {row.label}
                            </Typography>
                            <Typography component="div" variant="body2" sx={{ overflowWrap: 'anywhere', whiteSpace: 'pre-line' }}>
                              {action === 'add' ? (
                                row.after || t('pricingSync.notSet')
                              ) : action === 'delete' ? (
                                row.before || t('pricingSync.notSet')
                              ) : (
                                <>
                                  {row.before || t('pricingSync.notSet')}{' '}
                                  <Box component="span" sx={{ color: 'text.secondary', mx: 0.5 }}>
                                    →
                                  </Box>{' '}
                                  {row.after || t('pricingSync.notSet')}
                                </>
                              )}
                            </Typography>
                          </Box>
                        ))}
                      </Stack>
                    )}
                  </ListItem>
                </Box>
              ))}
            </List>
          </Card>
        );
      })}
    </Stack>
  );
}
PricingSyncChanges.propTypes = {
  changes: PropTypes.array.isRequired,
  sourceCount: PropTypes.number.isRequired,
  sourceLabel: PropTypes.string.isRequired,
  ownedby: PropTypes.array
};
