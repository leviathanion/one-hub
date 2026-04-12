import PropTypes from 'prop-types';
import { useMemo, useState } from 'react';
import { ArrowForward } from '@mui/icons-material';

import Badge from '@mui/material/Badge';

import { TableRow, TableCell, Stack, Collapse, Tooltip, Typography } from '@mui/material';

import { copy, timestamp2string, renderQuota } from 'utils/common';
import Label from 'ui-component/Label';
import { useLogType } from '../type/LogType';
import { useTranslation } from 'react-i18next';
import QuotaWithDetailRow from './QuotaWithDetailRow';
import QuotaWithDetailContent from './QuotaWithDetailContent';
import { calculatePrice } from './QuotaWithDetailContent';
import { calculateTokenBreakdown } from './quotaDetail';
import { styled } from '@mui/material/styles';

function renderType(type, logTypes, t) {
  const typeOption = logTypes[type];
  if (typeOption) {
    return (
      <Label variant="filled" color={typeOption.color}>
        {' '}
        {typeOption.text}{' '}
      </Label>
    );
  } else {
    return (
      <Label variant="filled" color="error">
        {' '}
        {t('logPage.unknown')}{' '}
      </Label>
    );
  }
}

function requestTimeLabelOptions(request_time) {
  let color = 'error';
  if (request_time === 0) {
    color = 'default';
  } else if (request_time <= 10) {
    color = 'success';
  } else if (request_time <= 50) {
    color = 'primary';
  } else if (request_time <= 100) {
    color = 'secondary';
  }

  return color;
}

function requestTSLabelOptions(request_ts) {
  let color = 'success';
  if (request_ts === 0) {
    color = 'default';
  } else if (request_ts <= 10) {
    color = 'error';
  } else if (request_ts <= 15) {
    color = 'secondary';
  } else if (request_ts <= 20) {
    color = 'primary';
  }

  return color;
}

export default function LogTableRow({ item, userIsAdmin, userGroup, columnVisibility }) {
  const { t } = useTranslation();
  const LogType = useLogType();
  let request_time = item.request_time / 1000;
  let request_time_str = request_time.toFixed(2) + ' S';

  let first_time = item.metadata?.first_response ? item.metadata.first_response / 1000 : 0;
  let first_time_str = first_time ? `${first_time.toFixed(2)} S` : '';

  const stream_time = request_time - first_time;

  let request_ts = 0;
  let request_ts_str = '';
  if (first_time > 0 && item.completion_tokens > 0) {
    // Using the completion_tokens directly since we already checked it's > 0
    request_ts = item.completion_tokens / stream_time;
    request_ts_str = `${request_ts.toFixed(2)} t/s`;
  }

  const { totalInputTokens, totalOutputTokens, show, tokenDetails } = useMemo(() => calculateTokenBreakdown(item), [item]);

  // 计算当前显示的列数
  const colCount = Object.values(columnVisibility).filter(Boolean).length;

  // 展开状态（仅type=2时才有展开）
  const [open, setOpen] = useState(false);
  const showExpand = item.type === 2 && columnVisibility.quota;

  return (
    <>
      <TableRow tabIndex={item.id}>
        {columnVisibility.created_at && <TableCell sx={{ p: '10px 8px' }}>{timestamp2string(item.created_at)}</TableCell>}

        {userIsAdmin && columnVisibility.channel_id && (
          <TableCell sx={{ p: '10px 8px' }}>
            {(item.channel_id || '') + ' ' + (item.channel?.name ? '(' + item.channel.name + ')' : '')}
          </TableCell>
        )}
        {userIsAdmin && columnVisibility.user_id && (
          <TableCell sx={{ p: '10px 8px' }}>
            <Label color="default" variant="outlined" copyText={item.username}>
              {item.username}
            </Label>
          </TableCell>
        )}

        {columnVisibility.group && (
          <TableCell sx={{ p: '10px 8px' }}>
            {item?.metadata?.is_backup_group ? (
              // 显示分组重定向：原始分组 → 备份分组
              <Stack direction="row" spacing={1} alignItems="center">
                <Label color="default" variant="soft">
                  {userGroup[item.metadata.group_name]?.name || '跟随用户'}
                </Label>
                <ArrowForward sx={{ fontSize: 16, color: 'text.secondary' }} />
                <Label color="warning" variant="soft">
                  {userGroup[item.metadata.backup_group_name]?.name || '备份分组'}
                </Label>
              </Stack>
            ) : // 正常显示分组
            item?.metadata?.group_name || item?.metadata?.backup_group_name ? (
              <Label color="default" variant="soft">
                {userGroup[item.metadata.group_name || item.metadata.backup_group_name]?.name || '跟随用户'}
              </Label>
            ) : (
              ''
            )}
          </TableCell>
        )}
        {columnVisibility.token_name && (
          <TableCell sx={{ p: '10px 8px' }}>
            {item.token_name && (
              <Label color="default" variant="soft" copyText={item.token_name}>
                {item.token_name}
              </Label>
            )}
          </TableCell>
        )}
        {columnVisibility.type && <TableCell sx={{ p: '10px 8px' }}>{renderType(item.type, LogType, t)}</TableCell>}
        {columnVisibility.model_name && <TableCell sx={{ p: '10px 8px' }}>{viewModelName(item.model_name, item.is_stream)}</TableCell>}

        {columnVisibility.duration && (
          <TableCell sx={{ p: '10px 8px' }}>
            <Stack direction="column" spacing={0.5}>
              <Label color={requestTimeLabelOptions(request_time)}>
                {item.request_time === 0 ? '无' : request_time_str} {first_time_str ? ' / ' + first_time_str : ''}
              </Label>

              {request_ts_str && <Label color={requestTSLabelOptions(request_ts)}>{request_ts_str}</Label>}
            </Stack>
          </TableCell>
        )}
        {columnVisibility.message && (
          <TableCell sx={{ p: '10px 8px' }}>{viewInput(item, t, totalInputTokens, totalOutputTokens, show, tokenDetails)}</TableCell>
        )}
        {columnVisibility.completion && <TableCell sx={{ p: '10px 8px' }}>{item.completion_tokens || ''}</TableCell>}
        {columnVisibility.quota && (
          <TableCell sx={{ p: '10px 8px' }}>
            {item.type === 2 ? (
              <QuotaWithDetailRow item={item} open={open} setOpen={setOpen} />
            ) : item.quota ? (
              renderQuota(item.quota, 6)
            ) : (
              '$0'
            )}
          </TableCell>
        )}
        {columnVisibility.source_ip && <TableCell sx={{ p: '10px 8px' }}>{item.source_ip || ''}</TableCell>}
        {columnVisibility.user_agent && (
          <TableCell sx={{ p: '10px 8px' }}>{viewUserAgent(item.metadata?.user_agent, t('logPage.userAgent'))}</TableCell>
        )}
        {columnVisibility.detail && (
          <TableCell sx={{ p: '10px 8px' }}>{viewLogContent(item, t, totalInputTokens, totalOutputTokens)}</TableCell>
        )}
      </TableRow>
      {/* 展开行 */}
      {showExpand && (
        <TableRow>
          <TableCell colSpan={colCount} sx={{ p: 0, border: 0, bgcolor: 'transparent' }}>
            <Collapse in={open} timeout="auto" unmountOnExit>
              <QuotaWithDetailContent
                item={item}
                userGroup={userGroup}
                t={t}
                totalInputTokens={totalInputTokens}
                totalOutputTokens={totalOutputTokens}
              />
            </Collapse>
          </TableCell>
        </TableRow>
      )}
    </>
  );
}

LogTableRow.propTypes = {
  item: PropTypes.object,
  userIsAdmin: PropTypes.bool,
  userGroup: PropTypes.object,
  columnVisibility: PropTypes.object
};

function viewModelName(model_name, isStream) {
  if (!model_name) {
    return '';
  }

  if (isStream) {
    return (
      <Badge
        badgeContent="Stream"
        color="primary"
        sx={{
          '& .MuiBadge-badge': {
            fontSize: '0.55rem',
            height: '16px',
            minWidth: '16px',
            padding: '0 4px',
            top: '-3px'
          }
        }}
      >
        <Label color="primary" variant="outlined" copyText={model_name}>
          {model_name}
        </Label>
      </Badge>
    );
  }

  return (
    <Label color="primary" variant="outlined" copyText={model_name}>
      {model_name}
    </Label>
  );
}

function viewUserAgent(userAgent, copyName) {
  if (!userAgent) {
    return '';
  }

  return (
    <Tooltip title={userAgent} placement="top" arrow>
      <Typography
        variant="body2"
        noWrap
        sx={{ maxWidth: 280, display: 'block', cursor: 'pointer' }}
        onClick={() => copy(userAgent, copyName)}
      >
        {userAgent}
      </Typography>
    </Tooltip>
  );
}

const MetadataTypography = styled(Typography)(({ theme }) => ({
  fontSize: 12,
  color: theme.palette.grey[300],
  '&:not(:last-child)': {
    marginBottom: theme.spacing(0.5)
  }
}));

function viewInput(item, t, totalInputTokens, totalOutputTokens, show, tokenDetails) {
  const { prompt_tokens } = item;

  if (!prompt_tokens) return '';
  if (!show) return prompt_tokens;

  const tooltipContent = tokenDetails.map(({ key, label, tokens, value, rate, labelParams }) => (
    <MetadataTypography key={key}>{`${t(label, labelParams)}: ${value} *  (${rate} - 1) = ${tokens}`}</MetadataTypography>
  ));

  return (
    <Badge variant="dot" color="primary">
      <Tooltip
        title={
          <>
            {tooltipContent}
            <MetadataTypography>
              {t('logPage.totalInputTokens')}: {totalInputTokens}
            </MetadataTypography>
            <MetadataTypography>
              {t('logPage.totalOutputTokens')}: {totalOutputTokens}
            </MetadataTypography>
          </>
        }
        placement="top"
        arrow
      >
        <span style={{ cursor: 'help' }}>{prompt_tokens}</span>
      </Tooltip>
    </Badge>
  );
}

function viewLogContent(item, t) {
  // totalOutputTokens is passed but not used in this function
  // Check if we have the necessary data to calculate prices
  if (!item?.metadata?.input_ratio) {
    const free = (item.quota === 0 || item.quota === undefined) && item.type === 2;
    return free ? (
      <Stack direction="column" spacing={0.3}>
        <Label color={free ? 'success' : 'secondary'} variant="soft">
          {t('logPage.content.free')}
        </Label>
      </Stack>
    ) : (
      <>{item.content || ''}</>
    );
  }

  // Ensure we have valid values with appropriate defaults
  const groupDiscount = item?.metadata?.group_ratio ?? 1;
  const priceType = item?.metadata?.price_type || '';
  const originalCompletionRatio = item?.metadata?.output_ratio || 0;
  const originalInputRatio = item?.metadata?.input_ratio || 0;

  let inputPriceInfo;
  let outputPriceInfo = '';
  if (priceType === 'times') {
    // Calculate prices for 'times' price type
    const inputPrice = calculatePrice(originalInputRatio, groupDiscount, true);

    inputPriceInfo = t('logPage.content.times_price', {
      times: inputPrice
    });
  } else {
    // Calculate prices for a standard price type
    const inputPrice = calculatePrice(originalInputRatio, groupDiscount, false);
    const outputPrice = calculatePrice(originalCompletionRatio, groupDiscount, false);

    inputPriceInfo = t('logPage.content.input_price', {
      price: inputPrice
    });
    outputPriceInfo = t('logPage.content.output_price', {
      price: outputPrice
    });
  }

  return (
    <Stack direction="column" spacing={0.3}>
      {inputPriceInfo && (
        <Label color="info" variant="soft">
          {inputPriceInfo}
        </Label>
      )}
      {outputPriceInfo && (
        <Label color="info" variant="soft">
          {outputPriceInfo}
        </Label>
      )}
    </Stack>
  );
}
