import PropTypes from 'prop-types';
import { useCallback, useEffect, useRef, useState } from 'react';
import { Grid, Box, Stack, Typography, Button, FormControl, InputLabel, MenuItem, Select } from '@mui/material';
import { gridSpacing } from 'store/constant';
import StatisticalLineChartCard from './component/StatisticalLineChartCard';
import ApexCharts from 'ui-component/chart/ApexCharts';
import SupportModels from './component/SupportModels';
import { getLastSevenDays, generateBarChartOptions, renderChartNumber } from 'utils/chart';
import { API } from 'utils/api';
import { showError, calculateQuota } from 'utils/common';
import ModelUsagePieChart from './component/ModelUsagePieChart';
import { useTranslation } from 'react-i18next';
import InviteCard from './component/InviteCard';
import QuotaLogWeek from './component/QuotaLogWeek';
import QuickStartCard from './component/QuickStartCard';
import RPM from './component/RPM';
import StatusPanel from './component/StatusPanel';
import TodayTokenBreakdownCard from './component/TodayTokenBreakdownCard';
import CacheHitRateCard from './component/CacheHitRateCard';
import { useSelector } from 'react-redux';

const initialCacheFilters = {
  model_name: '',
  channel_id: ''
};

const emptyCacheFilterOptions = {
  models: [],
  channels: []
};

const dashboardSnapshotMismatchCode = 'DASHBOARD_SNAPSHOT_MISMATCH';

// TabPanel component for tab content
function TabPanel(props) {
  const { children, value, index, ...other } = props;

  return (
    <div role="tabpanel" hidden={value !== index} id={`dashboard-tabpanel-${index}`} aria-labelledby={`dashboard-tab-${index}`} {...other}>
      {value === index && <Box sx={{ pt: 3 }}>{children}</Box>}
    </div>
  );
}

TabPanel.propTypes = {
  children: PropTypes.node,
  value: PropTypes.number,
  index: PropTypes.number
};

const Dashboard = () => {
  const [isLoading, setLoading] = useState(true);
  const [statisticalData, setStatisticalData] = useState([]);
  const [requestChart, setRequestChart] = useState(null);
  const [quotaChart, setQuotaChart] = useState(null);
  const [tokenChart, setTokenChart] = useState(null);
  const { t } = useTranslation();
  const [modelUsageData, setModelUsageData] = useState([]);
  const [currentTab, setCurrentTab] = useState(0);

  const [dashboardData, setDashboardData] = useState(null);
  const [dashboardDateRange, setDashboardDateRange] = useState(null);
  const [cacheOverviewData, setCacheOverviewData] = useState(null);
  const [cacheFilterOptions, setCacheFilterOptions] = useState(emptyCacheFilterOptions);
  const [cacheOverviewLoading, setCacheOverviewLoading] = useState(false);
  const [cacheFilters, setCacheFilters] = useState(initialCacheFilters);
  const [selectedCacheDate, setSelectedCacheDate] = useState('');
  const cacheOverviewRequestIdRef = useRef(0);
  const cacheOverviewResolvedQueryRef = useRef(null);
  const [cacheOverviewResolvedQuery, setCacheOverviewResolvedQuery] = useState(null);
  const siteInfo = useSelector((state) => state.siteInfo);

  const handleTabChange = (newValue) => {
    setCurrentTab(newValue);
  };

  const userDashboard = useCallback(async ({ resetCacheFilters = false, showLoading = false } = {}) => {
    if (showLoading) {
      setLoading(true);
    }

    try {
      const res = await API.get('/api/user/dashboard');
      const { success, message, data } = res.data;
      if (success) {
        if (data) {
          const nextDashboardDateRange = normalizeDashboardDateRange(data?.dateRange);
          const nextResolvedQuery = buildCacheOverviewQuery(initialCacheFilters, nextDashboardDateRange);
          const series = data.series || [];
          let lineData = getLineDataGroup(series, nextDashboardDateRange);

          setDashboardData(data);
          setDashboardDateRange(nextDashboardDateRange);
          setCacheFilterOptions(normalizeCacheFilterOptions(data?.cacheOverviewFilterOptions));
          setCacheOverviewData(buildCacheOverviewFallbackData(data));
          setCacheOverviewResolvedQuery(nextResolvedQuery);
          cacheOverviewResolvedQueryRef.current = nextResolvedQuery;

          if (resetCacheFilters) {
            setCacheFilters(cloneCacheFilters(initialCacheFilters));
          }

          setRequestChart(getLineCardOption(lineData, 'RequestCount'));
          setQuotaChart(getLineCardOption(lineData, 'Quota'));
          setTokenChart(getLineCardOption(lineData, 'PromptTokens'));
          setStatisticalData(getBarDataGroup(series, nextDashboardDateRange));
          setModelUsageData(getModelUsageData(series));
        }
      } else {
        showError(message);
      }
      setLoading(false);
    } catch (error) {
      setLoading(false);
      return;
    }
  }, []);

  const fetchCacheOverview = useCallback(
    async (filters, dateRange) => {
      const requestId = cacheOverviewRequestIdRef.current + 1;
      const requestedQuery = buildCacheOverviewQuery(filters, dateRange);
      cacheOverviewRequestIdRef.current = requestId;
      setCacheOverviewLoading(true);
      let shouldRollbackToResolvedQuery = false;
      let shouldRefreshDashboard = false;

      try {
        // Cache cards must stay anchored to the same snapshot as the base dashboard.
        const res = await API.post('/api/user/dashboard/modules/query', {
          dateRange,
          modules: [
            {
              name: 'cache_overview',
              filters: buildCacheOverviewFilters(filters)
            }
          ]
        });
        const { success, message, data, code } = res.data;
        const nextCacheOverview = data?.modules?.cache_overview;

        if (requestId !== cacheOverviewRequestIdRef.current) {
          return;
        }

        if (success && nextCacheOverview) {
          setCacheOverviewData(normalizeCacheOverviewData(nextCacheOverview, dateRange));
          setCacheOverviewResolvedQuery(requestedQuery);
          cacheOverviewResolvedQueryRef.current = requestedQuery;
          return;
        }

        if (!success && code === dashboardSnapshotMismatchCode) {
          shouldRefreshDashboard = true;
          return;
        }

        if (!success && message) {
          showError(message);
        }
        shouldRollbackToResolvedQuery = true;
      } catch (error) {
        if (requestId !== cacheOverviewRequestIdRef.current) {
          return;
        }
        if (error?.message) {
          showError(error.message);
        }
        shouldRollbackToResolvedQuery = true;
      } finally {
        if (requestId === cacheOverviewRequestIdRef.current) {
          if (shouldRollbackToResolvedQuery && cacheOverviewResolvedQueryRef.current?.filters) {
            setCacheFilters((current) => {
              const resolvedFilters = cacheOverviewResolvedQueryRef.current.filters;
              if (isSameCacheFilters(current, resolvedFilters)) {
                return current;
              }
              return cloneCacheFilters(resolvedFilters);
            });
          }
          setCacheOverviewLoading(false);
        }

        if (shouldRefreshDashboard) {
          void userDashboard({ resetCacheFilters: true, showLoading: true });
        }
      }
    },
    [userDashboard]
  );

  useEffect(() => {
    userDashboard({ resetCacheFilters: true });
  }, [userDashboard]);

  useEffect(() => {
    const nextFilters = {
      model_name: cacheFilters.model_name,
      channel_id: cacheFilters.channel_id
    };
    const nextQuery = buildCacheOverviewQuery(nextFilters, dashboardDateRange);
    if (!nextQuery || isSameCacheOverviewQuery(nextQuery, cacheOverviewResolvedQuery)) {
      return;
    }
    fetchCacheOverview(nextFilters, nextQuery.dateRange);
  }, [cacheFilters.channel_id, cacheFilters.model_name, cacheOverviewResolvedQuery, dashboardDateRange, fetchCacheOverview]);

  const selectedCacheOverviewQuery = buildCacheOverviewQuery(cacheFilters, dashboardDateRange);
  const isCacheOverviewFresh = isSameCacheOverviewQuery(selectedCacheOverviewQuery, cacheOverviewResolvedQuery);
  const isCacheOverviewLoading = (isLoading && !cacheOverviewData) || Boolean(selectedCacheOverviewQuery && !isCacheOverviewFresh);
  const areCacheFiltersDisabled = isLoading || !dashboardDateRange || (cacheOverviewLoading && !cacheOverviewData);
  const cacheOverviewAvailableDates = getCacheOverviewAvailableDates(cacheOverviewData, dashboardDateRange);
  const resolvedSelectedCacheDate = resolveSelectedCacheDate(selectedCacheDate, cacheOverviewAvailableDates, dashboardDateRange?.today);
  const selectedTokenBreakdown = getCacheOverviewTokenBreakdown(cacheOverviewData, resolvedSelectedCacheDate);
  const selectedCacheHitRate = getCacheOverviewCacheHitRate(cacheOverviewData, resolvedSelectedCacheDate);

  // Dashboard content
  const dashboardContent = (
    <Grid container spacing={gridSpacing}>
      {/* 支持的模型   */}
      <Grid item xs={12}>
        <SupportModels />
      </Grid>
      {/* 今日请求、消费、token */}
      <Grid item xs={12}>
        <Grid container spacing={gridSpacing}>
          <Grid item lg={3} xs={12} sx={{ height: '160' }}>
            <StatisticalLineChartCard
              isLoading={isLoading}
              title={t('dashboard_index.today_requests')}
              type="request"
              chartData={requestChart?.chartData}
              todayValue={requestChart?.todayValue}
              lastDayValue={requestChart?.lastDayValue}
            />
          </Grid>
          <Grid item lg={3} xs={12} sx={{ height: '160' }}>
            <StatisticalLineChartCard
              isLoading={isLoading}
              title={t('dashboard_index.today_consumption')}
              type="quota"
              chartData={quotaChart?.chartData}
              todayValue={quotaChart?.todayValue}
              lastDayValue={quotaChart?.lastDayValue}
            />
          </Grid>
          <Grid item lg={3} xs={12} sx={{ height: '160' }}>
            <StatisticalLineChartCard
              isLoading={isLoading}
              title={t('dashboard_index.today_tokens')}
              type="token"
              chartData={tokenChart?.chartData}
              todayValue={tokenChart?.todayValue}
              lastDayValue={tokenChart?.lastDayValue}
            />
          </Grid>
          <Grid item lg={3} xs={12} sx={{ height: '160' }}>
            <RPM />
          </Grid>
        </Grid>
      </Grid>

      <Grid item xs={12}>
        <Stack direction={{ xs: 'column', sm: 'row' }} spacing={2} justifyContent="flex-end" mb={2}>
          <FormControl size="small" sx={{ minWidth: { xs: '100%', sm: 180 } }}>
            <InputLabel id="dashboard-date-filter-label">{t('dashboard_index.date')}</InputLabel>
            <Select
              labelId="dashboard-date-filter-label"
              label={t('dashboard_index.date')}
              value={resolvedSelectedCacheDate}
              disabled={areCacheFiltersDisabled || cacheOverviewAvailableDates.length === 0}
              onChange={(event) => {
                setSelectedCacheDate(event.target.value);
              }}
            >
              {cacheOverviewAvailableDates.map((date) => (
                <MenuItem key={date} value={date}>
                  {date}
                </MenuItem>
              ))}
            </Select>
          </FormControl>

          <FormControl size="small" sx={{ minWidth: { xs: '100%', sm: 180 } }}>
            <InputLabel id="dashboard-model-filter-label">{t('dashboard_index.model')}</InputLabel>
            <Select
              labelId="dashboard-model-filter-label"
              label={t('dashboard_index.model')}
              displayEmpty
              value={cacheFilters.model_name}
              disabled={areCacheFiltersDisabled}
              onChange={(event) => {
                setCacheFilters((current) => ({
                  ...current,
                  model_name: event.target.value
                }));
              }}
            >
              <MenuItem value="">{t('dashboard_index.all_models')}</MenuItem>
              {cacheFilterOptions.models.map((modelName) => (
                <MenuItem key={modelName} value={modelName}>
                  {modelName}
                </MenuItem>
              ))}
            </Select>
          </FormControl>

          <FormControl size="small" sx={{ minWidth: { xs: '100%', sm: 220 } }}>
            <InputLabel id="dashboard-channel-filter-label">{t('dashboard_index.channel')}</InputLabel>
            <Select
              labelId="dashboard-channel-filter-label"
              label={t('dashboard_index.channel')}
              displayEmpty
              value={cacheFilters.channel_id}
              disabled={areCacheFiltersDisabled}
              onChange={(event) => {
                const value = event.target.value;
                setCacheFilters((current) => ({
                  ...current,
                  channel_id: value === '' ? '' : Number(value)
                }));
              }}
            >
              <MenuItem value="">{t('dashboard_index.all_channels')}</MenuItem>
              {cacheFilterOptions.channels.map((channel) => (
                <MenuItem key={channel.id} value={channel.id}>
                  {renderChannelOption(channel)}
                </MenuItem>
              ))}
            </Select>
          </FormControl>
        </Stack>

        <Grid container spacing={gridSpacing}>
          <Grid item lg={6} xs={12}>
            <TodayTokenBreakdownCard isLoading={isCacheOverviewLoading} data={selectedTokenBreakdown} />
          </Grid>
          <Grid item lg={6} xs={12}>
            <CacheHitRateCard isLoading={isCacheOverviewLoading} data={selectedCacheHitRate} />
          </Grid>
        </Grid>
      </Grid>

      <Grid item xs={12}>
        <Grid container spacing={gridSpacing}>
          <Grid item lg={8} xs={12}>
            {/* 7日模型消费统计 */}
            <ApexCharts isLoading={isLoading} chartDatas={statisticalData} title={t('dashboard_index.week_model_statistics')} />
            <Box mt={2}>
              {/* 7日消费统计 */}
              <QuotaLogWeek data={dashboardData?.series || []} dateRange={dashboardDateRange} />
            </Box>
          </Grid>

          <Grid item lg={4} xs={12}>
            {/* 用户信息 */}
            <ModelUsagePieChart isLoading={isLoading} data={modelUsageData} />
            <Box mt={2}>
              <QuickStartCard />
            </Box>
            {/* 邀请 */}
            <Box mt={2}>
              <InviteCard />
            </Box>
          </Grid>
        </Grid>
      </Grid>
    </Grid>
  );

  return (
    <>
      <Stack direction="row" alignItems="center" justifyContent="space-between" mb={1}>
        <Stack direction="row" alignItems="center" spacing={3}>
          <Stack direction="column" spacing={1}>
            <Typography variant="h2">{t('dashboard_index.title')}</Typography>
            <Typography variant="subtitle1" color="text.secondary">
              Dashboard
            </Typography>
          </Stack>

          {siteInfo.UptimeEnabled && (
            <Stack direction="row" spacing={1}>
              <Button
                onClick={() => handleTabChange(0)}
                variant={currentTab === 0 ? 'contained' : 'text'}
                size="small"
                disableElevation
                sx={{
                  padding: '6px 16px',
                  borderRadius: '4px',
                  backgroundColor: currentTab === 0 ? 'primary.main' : 'transparent',
                  color: currentTab === 0 ? 'white' : 'text.primary',
                  '&:hover': {
                    backgroundColor: currentTab === 0 ? 'primary.dark' : 'action.hover'
                  }
                }}
              >
                {t('dashboard_index.tab_dashboard')}
              </Button>
              <Button
                onClick={() => handleTabChange(1)}
                variant={currentTab === 1 ? 'contained' : 'text'}
                size="small"
                disableElevation
                sx={{
                  padding: '6px 16px',
                  borderRadius: '4px',
                  backgroundColor: currentTab === 1 ? 'primary.main' : 'transparent',
                  color: currentTab === 1 ? 'white' : 'text.primary',
                  '&:hover': {
                    backgroundColor: currentTab === 1 ? 'primary.dark' : 'action.hover'
                  }
                }}
              >
                {t('dashboard_index.tab_status')}
              </Button>
            </Stack>
          )}
        </Stack>
      </Stack>

      {siteInfo.UptimeEnabled ? (
        <>
          <TabPanel value={currentTab} index={0}>
            {dashboardContent}
          </TabPanel>
          <TabPanel value={currentTab} index={1}>
            <StatusPanel />
          </TabPanel>
        </>
      ) : (
        dashboardContent
      )}
    </>
  );
};

// 新增函数来处理模型使用数据
function getModelUsageData(data) {
  if (!Array.isArray(data)) {
    return [];
  }
  const modelUsage = {};
  data.forEach((item) => {
    if (!modelUsage[item.ModelName]) {
      modelUsage[item.ModelName] = 0;
    }
    modelUsage[item.ModelName] += item.RequestCount;
  });

  return Object.entries(modelUsage).map(([name, count]) => ({
    name,
    value: count
  }));
}
export default Dashboard;

function buildCacheOverviewFallbackData(dashboardData) {
  if (!dashboardData) {
    return null;
  }

  return buildCacheOverviewDataFromSeries(dashboardData?.series, normalizeDashboardDateRange(dashboardData?.dateRange));
}

function normalizeDashboardDateRange(dateRange) {
  if (!dateRange?.start || !dateRange?.end || !dateRange?.today) {
    return null;
  }

  return {
    start: dateRange.start,
    end: dateRange.end,
    today: dateRange.today
  };
}

function normalizeCacheFilterOptions(filterOptions) {
  return {
    models: Array.isArray(filterOptions?.models) ? filterOptions.models : emptyCacheFilterOptions.models,
    channels: Array.isArray(filterOptions?.channels) ? filterOptions.channels : emptyCacheFilterOptions.channels
  };
}

function buildCacheOverviewFilters(filters) {
  const payload = {};
  if (filters.model_name) {
    payload.model_name = filters.model_name;
  }
  if (filters.channel_id !== '') {
    payload.channel_id = filters.channel_id;
  }
  return payload;
}

function buildCacheOverviewQuery(filters, dateRange) {
  if (!dateRange) {
    return null;
  }

  return {
    filters: cloneCacheFilters(filters),
    dateRange: {
      start: dateRange.start,
      end: dateRange.end,
      today: dateRange.today
    }
  };
}

function isSameCacheFilters(left, right) {
  return left?.model_name === right?.model_name && left?.channel_id === right?.channel_id;
}

function isSameDashboardDateRange(left, right) {
  return left?.start === right?.start && left?.end === right?.end && left?.today === right?.today;
}

function isSameCacheOverviewQuery(left, right) {
  if (!left || !right) {
    return false;
  }

  return isSameCacheFilters(left.filters, right.filters) && isSameDashboardDateRange(left.dateRange, right.dateRange);
}

function cloneCacheFilters(filters) {
  return {
    model_name: filters?.model_name || '',
    channel_id: filters?.channel_id ?? ''
  };
}

function renderChannelOption(channel) {
  if (!channel?.name) {
    return `${channel?.id || ''}`;
  }
  return `${channel.id} (${channel.name})`;
}

function buildCacheOverviewDataFromSeries(series, dateRange) {
  const availableDates = getLastSevenDays(dateRange?.today);
  const tokenRows = availableDates.map((date) => ({
    date,
    requestCount: 0,
    inputTokens: 0,
    outputTokens: 0,
    cacheTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    totalTokens: 0
  }));
  const cacheRows = availableDates.map((date) => ({
    date,
    requestCount: 0,
    cacheHitCount: 0,
    hitRate: 0
  }));
  const tokenRowsByDate = new Map(tokenRows.map((row) => [row.date, row]));
  const cacheRowsByDate = new Map(cacheRows.map((row) => [row.date, row]));

  if (Array.isArray(series)) {
    series.forEach((item) => {
      const tokenRow = tokenRowsByDate.get(item?.Date);
      const cacheRow = cacheRowsByDate.get(item?.Date);
      if (!tokenRow || !cacheRow) {
        return;
      }

      const cacheTokens = Number(item?.CacheTokens || 0);
      const cacheReadTokens = Number(item?.CacheReadTokens || 0);
      const cacheWriteTokens = Number(item?.CacheWriteTokens || 0);
      const promptTokens = Number(item?.PromptTokens || 0);
      const completionTokens = Number(item?.CompletionTokens || 0);
      const inputTokens = Math.max(0, promptTokens - cacheTokens - cacheReadTokens - cacheWriteTokens);

      tokenRow.requestCount += Number(item?.RequestCount || 0);
      tokenRow.inputTokens += inputTokens;
      tokenRow.outputTokens += completionTokens;
      tokenRow.cacheTokens += cacheTokens;
      tokenRow.cacheReadTokens += cacheReadTokens;
      tokenRow.cacheWriteTokens += cacheWriteTokens;

      cacheRow.requestCount += Number(item?.RequestCount || 0);
      cacheRow.cacheHitCount += Number(item?.CacheHitCount || 0);
    });
  }

  tokenRows.forEach((row) => {
    row.totalTokens = row.inputTokens + row.outputTokens + row.cacheTokens + row.cacheReadTokens + row.cacheWriteTokens;
  });
  cacheRows.forEach((row) => {
    row.hitRate = row.requestCount > 0 ? row.cacheHitCount / row.requestCount : 0;
  });

  return {
    dateRange,
    availableDates,
    tokenBreakdownByDay: tokenRows,
    cacheHitRateByDay: cacheRows
  };
}

function normalizeCacheOverviewData(cacheOverviewData, dateRange) {
  if (!cacheOverviewData) {
    return null;
  }

  const normalizedDateRange = normalizeDashboardDateRange(cacheOverviewData?.dateRange) || dateRange;
  const fallbackData = buildCacheOverviewDataFromSeries([], normalizedDateRange);
  const availableDates =
    Array.isArray(cacheOverviewData?.availableDates) && cacheOverviewData.availableDates.length > 0
      ? cacheOverviewData.availableDates
      : fallbackData.availableDates;
  const tokenBreakdownByDay = mergeCacheOverviewRows(cacheOverviewData?.tokenBreakdownByDay, availableDates, createEmptyTokenBreakdownRow);
  const cacheHitRateByDay = mergeCacheOverviewRows(cacheOverviewData?.cacheHitRateByDay, availableDates, createEmptyCacheHitRateRow);

  return {
    dateRange: normalizedDateRange,
    availableDates,
    tokenBreakdownByDay,
    cacheHitRateByDay
  };
}

function mergeCacheOverviewRows(rows, availableDates, createEmptyRow) {
  const normalizedRows = Array.isArray(rows) ? rows : [];
  const rowsByDate = new Map(normalizedRows.filter((row) => row?.date).map((row) => [row.date, row]));
  return availableDates.map((date) => ({
    ...createEmptyRow(date),
    ...(rowsByDate.get(date) || {})
  }));
}

function createEmptyTokenBreakdownRow(date) {
  return {
    date,
    requestCount: 0,
    inputTokens: 0,
    outputTokens: 0,
    cacheTokens: 0,
    cacheReadTokens: 0,
    cacheWriteTokens: 0,
    totalTokens: 0
  };
}

function createEmptyCacheHitRateRow(date) {
  return {
    date,
    requestCount: 0,
    cacheHitCount: 0,
    hitRate: 0
  };
}

function getCacheOverviewAvailableDates(cacheOverviewData, dateRange) {
  if (Array.isArray(cacheOverviewData?.availableDates) && cacheOverviewData.availableDates.length > 0) {
    return cacheOverviewData.availableDates;
  }
  return getLastSevenDays(dateRange?.today);
}

function resolveSelectedCacheDate(selectedDate, availableDates, fallbackDate) {
  if (selectedDate && availableDates.includes(selectedDate)) {
    return selectedDate;
  }
  if (fallbackDate && availableDates.includes(fallbackDate)) {
    return fallbackDate;
  }
  return availableDates[availableDates.length - 1] || '';
}

function getCacheOverviewTokenBreakdown(cacheOverviewData, selectedDate) {
  const row = cacheOverviewData?.tokenBreakdownByDay?.find((item) => item?.date === selectedDate);
  return row || createEmptyTokenBreakdownRow(selectedDate);
}

function getCacheOverviewCacheHitRate(cacheOverviewData, selectedDate) {
  const row = cacheOverviewData?.cacheHitRateByDay?.find((item) => item?.date === selectedDate);
  return row || createEmptyCacheHitRateRow(selectedDate);
}

function getLineDataGroup(statisticalData, dateRange) {
  if (!Array.isArray(statisticalData)) {
    return [];
  }
  let groupedData = statisticalData.reduce((acc, cur) => {
    if (!acc[cur.Date]) {
      acc[cur.Date] = {
        date: cur.Date,
        RequestCount: 0,
        Quota: 0,
        PromptTokens: 0,
        CompletionTokens: 0
      };
    }
    acc[cur.Date].RequestCount += cur.RequestCount;
    acc[cur.Date].Quota += cur.Quota;
    acc[cur.Date].PromptTokens += cur.PromptTokens;
    acc[cur.Date].CompletionTokens += cur.CompletionTokens;
    return acc;
  }, {});
  let lastSevenDays = getLastSevenDays(dateRange?.today);
  return lastSevenDays.map((Date) => {
    if (!groupedData[Date]) {
      return {
        date: Date,
        RequestCount: 0,
        Quota: 0,
        PromptTokens: 0,
        CompletionTokens: 0
      };
    } else {
      return groupedData[Date];
    }
  });
}

function getBarDataGroup(data, dateRange) {
  if (!Array.isArray(data)) {
    return null;
  }
  const lastSevenDays = getLastSevenDays(dateRange?.today);
  const result = [];
  const map = new Map();
  let totalCosts = 0;

  for (const item of data) {
    if (!map.has(item.ModelName)) {
      const newData = { name: item.ModelName, data: new Array(7).fill(0) };
      map.set(item.ModelName, newData);
      result.push(newData);
    }
    const index = lastSevenDays.indexOf(item.Date);
    if (index !== -1) {
      let costs = Number(calculateQuota(item.Quota, 3));
      map.get(item.ModelName).data[index] = costs;
      totalCosts += parseFloat(costs.toFixed(3));
    }
  }

  let chartData = generateBarChartOptions(lastSevenDays, result, 'USD', 3);
  chartData.options.title.text = 'Total：$' + renderChartNumber(totalCosts, 3);

  return chartData;
}

function getLineCardOption(lineDataGroup, field) {
  let todayValue = 0;
  let lastDayValue = 0;
  let chartData = null;

  let lineData = lineDataGroup.map((item) => {
    let tmp = {
      x: item.date,
      y: item[field]
    };
    switch (field) {
      case 'Quota':
        tmp.y = calculateQuota(item.Quota, 3);
        break;
      case 'PromptTokens':
        tmp.y += item.CompletionTokens;
        break;
    }

    return tmp;
  });

  // 获取今天和昨天的数据
  if (lineData.length > 1) {
    todayValue = lineData[lineData.length - 1].y;
    if (lineData.length > 2) {
      lastDayValue = lineData[lineData.length - 2].y;
    }
  }

  switch (field) {
    case 'RequestCount':
      // chartData = generateLineChartOptions(lineData, '次');
      lastDayValue = parseFloat(lastDayValue);
      todayValue = parseFloat(todayValue);
      break;
    case 'Quota':
      // chartData = generateLineChartOptions(lineData, '美元');
      lastDayValue = parseFloat(lastDayValue);
      todayValue = '$' + parseFloat(todayValue);
      break;
    case 'PromptTokens':
      // chartData = generateLineChartOptions(lineData, '');
      lastDayValue = parseFloat(lastDayValue);
      todayValue = parseFloat(todayValue);
      break;
  }

  chartData = {
    series: [
      {
        data: lineData
      }
    ]
  };

  return { chartData: chartData, todayValue: todayValue, lastDayValue: lastDayValue };
}
