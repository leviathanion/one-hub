import PropTypes from 'prop-types';
import { Alert, Box, Chip, Divider, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import { pricingSyncRows } from './pricingSyncComparison.mjs';

const groups = { add: 'success', update: 'warning', delete: 'error', locked: 'default' };

export default function PricingSyncChanges({ changes }) {
  const { t } = useTranslation();
  const count = (action) => changes.filter((change) => change.action === action).length;
  return (
    <Stack component="section" aria-label={t('pricingSync.changeSummary')} spacing={2}>
      <Stack direction="row" spacing={1} flexWrap="wrap" useFlexGap>
        {Object.entries(groups).map(([action, color]) => (
          <Chip
            key={action}
            variant="outlined"
            size="small"
            color={color}
            label={`${t(`pricingSync.action.${action}`)} ${count(action)}`}
          />
        ))}
      </Stack>
      {!changes.some((change) => change.action !== 'locked') && <Alert severity="info">{t('pricingSync.noChanges')}</Alert>}
      {Object.entries(groups).map(([action, color]) => {
        const entries = changes.filter((change) => change.action === action);
        if (!entries.length) return null;
        return (
          <Box
            key={action}
            component="section"
            aria-label={t(`pricingSync.action.${action}`)}
            sx={{ border: 1, borderColor: 'divider', borderRadius: 1.5, overflow: 'hidden' }}
          >
            <Stack
              direction="row"
              alignItems="center"
              spacing={1}
              sx={{ px: 2, py: 1.25, bgcolor: 'background.neutral', borderBottom: 1, borderColor: 'divider' }}
            >
              <Box sx={{ width: 7, height: 7, borderRadius: '50%', bgcolor: color === 'default' ? 'text.disabled' : `${color}.main` }} />
              <Typography variant="subtitle2" sx={{ flex: 1 }}>
                {t(`pricingSync.action.${action}`)}
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {entries.length}
              </Typography>
            </Stack>
            {entries.map((change, index) => (
              <Box key={change.model}>
                {index > 0 && <Divider />}
                <Box sx={{ p: 2 }}>
                  <Typography variant="subtitle2" sx={{ mb: action === 'locked' ? 0.5 : 1.5, overflowWrap: 'anywhere' }}>
                    {change.model}
                  </Typography>
                  {action === 'locked' ? (
                    <Typography variant="body2" color="text.secondary">
                      {t('pricingSync.lockedHelp')}
                    </Typography>
                  ) : (
                    <Box role="table" aria-label={t('pricingSync.modelChanges', { model: change.model })}>
                      <Box
                        role="row"
                        sx={{
                          display: 'grid',
                          gridTemplateColumns:
                            action === 'update' ? 'minmax(0, 1fr) minmax(0, 1fr) 18px minmax(0, 1fr)' : 'minmax(0, 1fr) minmax(0, 2fr)',
                          columnGap: 1,
                          mb: 0.75
                        }}
                      >
                        <Typography role="columnheader" variant="caption" color="text.secondary">
                          {t('pricingSync.item')}
                        </Typography>
                        {action === 'update' ? (
                          <>
                            <Typography role="columnheader" variant="caption" color="text.secondary">
                              {t('pricingSync.current')}
                            </Typography>
                            <span aria-hidden="true" />
                            <Typography role="columnheader" variant="caption" color="text.secondary">
                              {t('pricingSync.incoming')}
                            </Typography>
                          </>
                        ) : (
                          <Typography role="columnheader" variant="caption" color="text.secondary">
                            {t(action === 'add' ? 'pricingSync.incoming' : 'pricingSync.current')}
                          </Typography>
                        )}
                      </Box>
                      <Stack spacing={1}>
                        {pricingSyncRows(change.before, change.after, t).map((row) => {
                          const isRule = row.key.startsWith('rule:') || row.key === 'rules';
                          const columns =
                            action === 'update' ? 'minmax(0, 1fr) minmax(0, 1fr) 18px minmax(0, 1fr)' : 'minmax(0, 1fr) minmax(0, 2fr)';
                          return (
                            <Box
                              key={row.key}
                              role="row"
                              sx={{
                                display: 'grid',
                                gridTemplateColumns: isRule ? { xs: 'minmax(0, 1fr)', sm: columns } : columns,
                                columnGap: 1,
                                ...(isRule && {
                                  rowGap: 0.75,
                                  borderTop: { xs: 1, sm: 0 },
                                  borderColor: { xs: 'divider' },
                                  pt: { xs: 1.25, sm: 0 }
                                }),
                                '& > *': { minWidth: 0, overflowWrap: 'anywhere', whiteSpace: 'pre-line' }
                              }}
                            >
                              <Typography role="rowheader" variant="body2" color="text.secondary">
                                {row.label}
                              </Typography>
                              {action === 'update' ? (
                                <>
                                  <Typography role="cell" variant="body2" color="text.secondary">
                                    {isRule && (
                                      <Typography component="span" variant="caption" sx={{ display: { xs: 'block', sm: 'none' } }}>
                                        {t('pricingSync.current')}
                                      </Typography>
                                    )}
                                    {row.before || t('pricingSync.notSet')}
                                  </Typography>
                                  <Typography
                                    aria-hidden="true"
                                    variant="body2"
                                    color="text.disabled"
                                    sx={isRule ? { display: { xs: 'none', sm: 'block' } } : undefined}
                                  >
                                    →
                                  </Typography>
                                  <Typography role="cell" variant="body2" sx={{ fontWeight: 500 }}>
                                    {isRule && (
                                      <Typography
                                        component="span"
                                        variant="caption"
                                        color="text.secondary"
                                        sx={{ display: { xs: 'block', sm: 'none' } }}
                                      >
                                        {t('pricingSync.incoming')}
                                      </Typography>
                                    )}
                                    {row.after || t('pricingSync.notSet')}
                                  </Typography>
                                </>
                              ) : (
                                <Typography role="cell" variant="body2">
                                  {(action === 'add' ? row.after : row.before) || t('pricingSync.notSet')}
                                </Typography>
                              )}
                            </Box>
                          );
                        })}
                      </Stack>
                    </Box>
                  )}
                </Box>
              </Box>
            ))}
          </Box>
        );
      })}
    </Stack>
  );
}

PricingSyncChanges.propTypes = { changes: PropTypes.array.isRequired };
