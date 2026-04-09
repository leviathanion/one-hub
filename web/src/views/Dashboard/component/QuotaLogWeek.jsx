import { Table, TableBody, TableCell, TableContainer, TableHead, TableRow, Paper, Typography } from '@mui/material';
import PropTypes from 'prop-types';
import { useEffect, useState } from 'react';
import { calculateQuota } from 'utils/common';
import { getLastSevenDays } from 'utils/chart';
import { useTranslation } from 'react-i18next';
import SubCard from 'ui-component/cards/SubCard';

const normalizeInputTokens = (promptTokens, cacheTokens, cacheReadTokens, cacheWriteTokens) => {
  return Math.max(Number(promptTokens || 0) - Number(cacheTokens || 0) - Number(cacheReadTokens || 0) - Number(cacheWriteTokens || 0), 0);
};

const QuotaLogWeek = ({ data, dateRange }) => {
  const [logData, setLogData] = useState([]);
  const [isLoading, setIsLoading] = useState(true);
  const { t } = useTranslation();
  const [isHasData, setIsHasData] = useState(false);

  useEffect(() => {
    if (Array.isArray(data) && data.length > 0) {
      setIsHasData(true);
      // 处理数据，按日期分组
      const lastSevenDays = getLastSevenDays(dateRange?.today);
      const processedData = lastSevenDays
        .map((date) => {
          const dayData = data.filter((item) => item.Date === date);

          // 计算当天的总和
          const totalRequests = dayData.reduce((sum, item) => sum + item.RequestCount, 0);
          const totalAmount = dayData.reduce((sum, item) => sum + item.Quota, 0).toFixed(6);
          const totalInputTokens = dayData.reduce(
            (sum, item) => sum + normalizeInputTokens(item.PromptTokens, item.CacheTokens, item.CacheReadTokens, item.CacheWriteTokens),
            0
          );
          const totalOutputTokens = dayData.reduce((sum, item) => sum + item.CompletionTokens, 0);
          const totalCacheTokens = dayData.reduce((sum, item) => sum + (item.CacheTokens || 0), 0);
          const totalCacheReadTokens = dayData.reduce((sum, item) => sum + (item.CacheReadTokens || 0), 0);
          const totalCacheWriteTokens = dayData.reduce((sum, item) => sum + (item.CacheWriteTokens || 0), 0);
          const totalDuration = dayData.reduce((sum, item) => sum + item.RequestTime, 0);

          return {
            date,
            requests: totalRequests,
            amount: calculateQuota(totalAmount, 6),
            tokens: `${totalInputTokens}/${totalOutputTokens}/${totalCacheTokens}/${totalCacheReadTokens}/${totalCacheWriteTokens}`,
            duration: (totalDuration / 1000).toFixed(3)
          };
        })
        .reverse();

      setLogData(processedData);
    } else {
      setIsHasData(false);
      setLogData([]);
    }
    setIsLoading(false);
  }, [data, dateRange?.today]);

  return (
    <SubCard title={t('dashboard_index.week_consumption_log')} contentSX={{ p: 0 }}>
      <TableContainer component={Paper} sx={{ boxShadow: 'none', p: 2 }}>
        <Table sx={{ minWidth: 650 }} aria-label="consumption log table">
          <TableHead>
            <TableRow>
              <TableCell>{t('dashboard_index.date')}</TableCell>
              <TableCell align="right">{t('dashboard_index.request_count')}</TableCell>
              <TableCell align="right">{t('dashboard_index.amount')}</TableCell>
              <TableCell align="right">{t('dashboard_index.tokens_breakdown')}</TableCell>
              <TableCell align="right">{t('dashboard_index.request_time')}</TableCell>
            </TableRow>
          </TableHead>
          <TableBody>
            {isLoading ? (
              <TableRow>
                <TableCell colSpan={5} align="center">
                  <Typography variant="body2">{t('dashboard_index.loading')}</Typography>
                </TableCell>
              </TableRow>
            ) : !isHasData ? (
              <TableRow>
                <TableCell colSpan={5} align="center">
                  <Typography variant="h4" color={'#697586'}>
                    {t('dashboard_index.no_data')}
                  </Typography>
                </TableCell>
              </TableRow>
            ) : (
              logData.map((row) => (
                <TableRow key={row.date} sx={{ '&:last-child td, &:last-child th': { border: 0 } }}>
                  <TableCell component="th" scope="row">
                    {row.date}
                  </TableCell>
                  <TableCell align="right">{row.requests}</TableCell>
                  <TableCell align="right">${row.amount}</TableCell>
                  <TableCell align="right">{row.tokens}</TableCell>
                  <TableCell align="right">{row.duration}</TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </TableContainer>
    </SubCard>
  );
};

export default QuotaLogWeek;

QuotaLogWeek.propTypes = {
  data: PropTypes.array,
  dateRange: PropTypes.shape({
    start: PropTypes.string,
    end: PropTypes.string,
    today: PropTypes.string
  })
};
