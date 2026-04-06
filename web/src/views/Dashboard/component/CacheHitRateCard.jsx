import PropTypes from 'prop-types';
import { useEffect, useState } from 'react';
import { Box, FormControl, MenuItem, Select, Skeleton, Stack, Typography } from '@mui/material';
import { useTranslation } from 'react-i18next';
import SubCard from 'ui-component/cards/SubCard';

const ALL_MODELS = '__all_models__';

function formatInteger(value) {
  return Number(value || 0).toLocaleString();
}

function formatPercent(value) {
  return `${(Number(value || 0) * 100).toFixed(2)}%`;
}

const CacheHitRateCard = ({ isLoading, data }) => {
  const { t } = useTranslation();
  const [selectedModel, setSelectedModel] = useState(ALL_MODELS);
  const modelItems = data?.models || [];

  useEffect(() => {
    if (selectedModel === ALL_MODELS) {
      return;
    }
    if (!modelItems.some((item) => item.modelName === selectedModel)) {
      setSelectedModel(ALL_MODELS);
    }
  }, [modelItems, selectedModel]);

  const currentData =
    selectedModel === ALL_MODELS
      ? data
      : modelItems.find((item) => item.modelName === selectedModel) || {
          requestCount: 0,
          cacheHitCount: 0,
          hitRate: 0
        };

  return (
    <SubCard
      title={t('dashboard_index.today_cache_hit_rate')}
      sx={{ height: '100%' }}
      secondary={
        <FormControl size="small" sx={{ minWidth: 150 }}>
          <Select
            value={selectedModel}
            onChange={(event) => setSelectedModel(event.target.value)}
            displayEmpty
            disabled={isLoading || modelItems.length === 0}
          >
            <MenuItem value={ALL_MODELS}>{t('dashboard_index.all_models')}</MenuItem>
            {modelItems.map((item) => (
              <MenuItem key={item.modelName} value={item.modelName}>
                {item.modelName}
              </MenuItem>
            ))}
          </Select>
        </FormControl>
      }
    >
      <Stack spacing={2.5}>
        <Box>
          {isLoading ? (
            <Skeleton variant="text" width={160} height={44} />
          ) : (
            <Typography variant="h2">{formatPercent(currentData?.hitRate)}</Typography>
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
              <Typography variant="h5">{formatInteger(currentData?.cacheHitCount)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.cache_hit_count')}
            </Typography>
          </Box>
          <Box>
            {isLoading ? (
              <Skeleton variant="text" width={72} height={30} />
            ) : (
              <Typography variant="h5">{formatInteger(currentData?.requestCount)}</Typography>
            )}
            <Typography variant="body2" color="text.secondary">
              {t('dashboard_index.request_count')}
            </Typography>
          </Box>
        </Stack>

        <Typography variant="body2" color="text.secondary">
          {t('dashboard_index.cache_hits')}: {formatInteger(currentData?.cacheHitCount)} / {formatInteger(currentData?.requestCount)}
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
    hitRate: PropTypes.number,
    models: PropTypes.arrayOf(
      PropTypes.shape({
        modelName: PropTypes.string,
        requestCount: PropTypes.number,
        cacheHitCount: PropTypes.number,
        hitRate: PropTypes.number
      })
    )
  })
};

export default CacheHitRateCard;
