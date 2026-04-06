import PropTypes from 'prop-types';
import { Box, Divider, Grid, Skeleton, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import SubCard from 'ui-component/cards/SubCard';

const tokenBreakdownItems = [
  { key: 'inputTokens', labelKey: 'dashboard_index.input_tokens' },
  { key: 'outputTokens', labelKey: 'dashboard_index.output_tokens' },
  { key: 'cacheTokens', labelKey: 'dashboard_index.cache_tokens' },
  { key: 'cacheReadTokens', labelKey: 'dashboard_index.cache_read_tokens' },
  { key: 'cacheWriteTokens', labelKey: 'dashboard_index.cache_write_tokens' }
];

function formatInteger(value) {
  return Number(value || 0).toLocaleString();
}

const TodayTokenBreakdownCard = ({ isLoading, data }) => {
  const { t } = useTranslation();

  return (
    <SubCard title={t('dashboard_index.today_token_breakdown')} sx={{ height: '100%' }}>
      <Stack spacing={2.5}>
        <Stack direction="row" alignItems="flex-start" justifyContent="space-between" spacing={2}>
          <Box>
            {isLoading ? (
              <Skeleton variant="text" width={160} height={44} />
            ) : (
              <Typography variant="h2">{formatInteger(data?.totalTokens)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.total_tokens')}
            </Typography>
          </Box>
          <Box textAlign="right">
            {isLoading ? (
              <Skeleton variant="text" width={72} height={32} />
            ) : (
              <Typography variant="h4">{formatInteger(data?.requestCount)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.request_count')}
            </Typography>
          </Box>
        </Stack>

        <Divider />

        <Grid container spacing={2}>
          {tokenBreakdownItems.map((item) => (
            <Grid item xs={6} sm={4} key={item.key}>
              <Stack spacing={0.5}>
                <Typography variant="caption" color="text.secondary">
                  {t(item.labelKey)}
                </Typography>
                {isLoading ? (
                  <Skeleton variant="text" width={96} height={30} />
                ) : (
                  <Typography variant="h5">{formatInteger(data?.[item.key])}</Typography>
                )}
              </Stack>
            </Grid>
          ))}
        </Grid>
      </Stack>
    </SubCard>
  );
};

TodayTokenBreakdownCard.propTypes = {
  isLoading: PropTypes.bool,
  data: PropTypes.shape({
    requestCount: PropTypes.number,
    inputTokens: PropTypes.number,
    outputTokens: PropTypes.number,
    cacheTokens: PropTypes.number,
    cacheReadTokens: PropTypes.number,
    cacheWriteTokens: PropTypes.number,
    totalTokens: PropTypes.number
  })
};

export default TodayTokenBreakdownCard;
