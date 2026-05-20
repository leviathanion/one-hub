import PropTypes from 'prop-types';
import { Divider, Grid, Skeleton, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import SubCard from 'ui-component/cards/SubCard';

function toNumber(value) {
  return Number(value || 0);
}

function formatInteger(value) {
  return toNumber(value).toLocaleString();
}

function formatPercent(value) {
  return `${(toNumber(value) * 100).toFixed(2)}%`;
}

function formatRatio(numerator, denominator) {
  return `${formatInteger(numerator)} / ${formatInteger(denominator)}`;
}

function getCacheTokenHitStats(tokenData) {
  const cacheHitTokens = toNumber(tokenData?.cacheTokens) + toNumber(tokenData?.cacheReadTokens);
  const inputTokens = toNumber(tokenData?.inputTokens);

  return {
    cacheHitTokens,
    inputTokens,
    hitRate: inputTokens > 0 ? cacheHitTokens / inputTokens : 0
  };
}

const CacheHitRateCard = ({ isLoading, data, tokenData, title }) => {
  const { t } = useTranslation();
  const tokenHitStats = getCacheTokenHitStats(tokenData);

  return (
    <SubCard title={title || t('dashboard_index.cache_hit_rate')} sx={{ height: '100%' }}>
      <Stack spacing={2.5}>
        <Grid container spacing={2}>
          <Grid item xs={12} sm={6}>
            <Stack spacing={0.75}>
              {isLoading ? (
                <Skeleton variant="text" width={140} height={42} />
              ) : (
                <Typography variant="h3">{formatPercent(data?.hitRate)}</Typography>
              )}
              <Typography variant="body2" color="text.secondary">
                {t('dashboard_index.cache_request_hit_rate')}
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {t('dashboard_index.cache_hits')}: {formatRatio(data?.cacheHitCount, data?.requestCount)}
              </Typography>
            </Stack>
          </Grid>

          <Grid item xs={12} sm={6}>
            <Stack spacing={0.75}>
              {isLoading ? (
                <Skeleton variant="text" width={140} height={42} />
              ) : (
                <Typography variant="h3">{formatPercent(tokenHitStats.hitRate)}</Typography>
              )}
              <Typography variant="body2" color="text.secondary">
                {t('dashboard_index.cache_token_hit_rate')}
              </Typography>
              <Typography variant="caption" color="text.secondary">
                {t('dashboard_index.cache_token_hits')}: {formatRatio(tokenHitStats.cacheHitTokens, tokenHitStats.inputTokens)}
              </Typography>
            </Stack>
          </Grid>
        </Grid>

        <Divider />

        <Grid container spacing={2}>
          <Grid item xs={6} sm={3}>
            <Typography variant="caption" color="text.secondary">
              {t('dashboard_index.cache_hit_count')}
            </Typography>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={28} />
            ) : (
              <Typography variant="h5">{formatInteger(data?.cacheHitCount)}</Typography>
            )}
          </Grid>
          <Grid item xs={6} sm={3}>
            <Typography variant="caption" color="text.secondary">
              {t('dashboard_index.request_count')}
            </Typography>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={28} />
            ) : (
              <Typography variant="h5">{formatInteger(data?.requestCount)}</Typography>
            )}
          </Grid>
          <Grid item xs={6} sm={3}>
            <Typography variant="caption" color="text.secondary">
              {t('dashboard_index.cache_tokens')}
            </Typography>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={28} />
            ) : (
              <Typography variant="h5">{formatInteger(tokenHitStats.cacheHitTokens)}</Typography>
            )}
          </Grid>
          <Grid item xs={6} sm={3}>
            <Typography variant="caption" color="text.secondary">
              {t('dashboard_index.input_tokens')}
            </Typography>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={28} />
            ) : (
              <Typography variant="h5">{formatInteger(tokenHitStats.inputTokens)}</Typography>
            )}
          </Grid>
        </Grid>
      </Stack>
    </SubCard>
  );
};

CacheHitRateCard.propTypes = {
  isLoading: PropTypes.bool,
  title: PropTypes.string,
  data: PropTypes.shape({
    requestCount: PropTypes.number,
    cacheHitCount: PropTypes.number,
    hitRate: PropTypes.number
  }),
  tokenData: PropTypes.shape({
    inputTokens: PropTypes.number,
    cacheTokens: PropTypes.number,
    cacheReadTokens: PropTypes.number,
    cacheWriteTokens: PropTypes.number
  })
};

export default CacheHitRateCard;
