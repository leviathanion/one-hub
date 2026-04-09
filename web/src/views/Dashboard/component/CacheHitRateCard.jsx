import PropTypes from 'prop-types';
import { Box, Skeleton, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import SubCard from 'ui-component/cards/SubCard';

function formatInteger(value) {
  return Number(value || 0).toLocaleString();
}

function formatPercent(value) {
  return `${(Number(value || 0) * 100).toFixed(2)}%`;
}

const CacheHitRateCard = ({ isLoading, data }) => {
  const { t } = useTranslation();

  return (
    <SubCard title={t('dashboard_index.today_cache_hit_rate')} sx={{ height: '100%' }}>
      <Stack spacing={2.5}>
        <Box>
          {isLoading ? (
            <Skeleton variant="text" width={160} height={44} />
          ) : (
            <Typography variant="h2">{formatPercent(data?.hitRate)}</Typography>
          )}
          <Typography variant="body2" color="text.secondary">
            {t('dashboard_index.cache_hit_rate')}
          </Typography>
        </Box>

        <Stack direction="row" spacing={4}>
          <Box>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={30} />
            ) : (
              <Typography variant="h5">{formatInteger(data?.cacheHitCount)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.cache_hit_count')}
            </Typography>
          </Box>
          <Box>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={30} />
            ) : (
              <Typography variant="h5">{formatInteger(data?.requestCount)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.request_count')}
            </Typography>
          </Box>
        </Stack>

        <Typography variant="body2" color="text.secondary">
          {t('dashboard_index.cache_hits')}: {formatInteger(data?.cacheHitCount)} / {formatInteger(data?.requestCount)}
        </Typography>
      </Stack>
    </SubCard>
  );
};

CacheHitRateCard.propTypes = {
  isLoading: PropTypes.bool,
  data: PropTypes.shape({
    requestCount: PropTypes.number,
    cacheHitCount: PropTypes.number,
    hitRate: PropTypes.number
  })
};

export default CacheHitRateCard;
