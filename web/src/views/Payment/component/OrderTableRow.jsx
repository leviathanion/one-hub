import PropTypes from 'prop-types';
import { useState } from 'react';
import { API } from 'utils/api';
import { TableRow, TableCell, Stack, Typography, Button } from '@mui/material';
import { timestamp2string, showError, showSuccess } from 'utils/common';
import Label from 'ui-component/Label';
import { formatOrderTotal } from '../../Topup/component/paymentFlow.mjs';

const StatusType = {
  pending: { name: '待支付', value: 'pending', color: 'primary' },
  success: { name: '额度已到账', value: 'success', color: 'success' },
  failed: { name: '准备失败', value: 'failed', color: 'error' },
  closed: { name: '入口已关闭', value: 'closed', color: 'default' }
};
const PaymentStates = {
  unconfirmed: ['未确认付款', 'primary'],
  processing: ['支付处理中', 'warning'],
  paid: ['额度已到账', 'success']
};
const PreparationStates = { new: '尚未准备', claimed: '正在准备', ready: '已准备', unknown: '创建结果待确认', rejected: '准备被拒绝' };
const WindowStates = { open: '入口开放', local_expired: '本地入口到期，已有付款仍核查', provider_closed: '上游已关闭' };
export { StatusType };

export default function OrderTableRow({ item, onRefresh, onGatewayCredentials }) {
  const [querying, setQuerying] = useState(false);
  const query = async () => {
    setQuerying(true);
    try {
      const { data } = await API.post(`/api/payment/order/${encodeURIComponent(item.trade_no)}/query`, {}, { timeout: 15000 });
      if (!data.success) throw new Error(data.message || '查单失败');
      showSuccess('已核查上游付款结果');
      onRefresh();
    } catch (err) {
      showError(err.message || '查单失败');
    } finally {
      setQuerying(false);
    }
  };
  const state = PaymentStates[item.payment_state] || ['未知', 'default'];
  return (
    <TableRow tabIndex={item.id}>
      <TableCell sx={{ minWidth: 180 }}>{timestamp2string(item.created_at)}</TableCell>
      <TableCell>
        <Button size="small" aria-label={`维护原网关 ${item.gateway_id} 凭证`} onClick={() => onGatewayCredentials(item.gateway_id)}>
          {item.gateway_id}
        </Button>
        <Typography variant="caption" display="block">
          {item.product_code}
        </Typography>
      </TableCell>
      <TableCell>{item.user_id}</TableCell>
      <TableCell>{item.trade_no}</TableCell>
      <TableCell sx={{ maxWidth: 260, overflowWrap: 'anywhere' }}>{item.transaction_namespace}</TableCell>
      <TableCell>{item.provider_resource_ref || '—'}</TableCell>
      <TableCell>{item.provider_transaction_id || '—'}</TableCell>
      <TableCell>
        {formatOrderTotal({ minor: item.expected_amount_minor, currency: item.order_currency, exponent: item.currency_exponent })}
      </TableCell>
      <TableCell>{item.quota}</TableCell>
      <TableCell>
        <Stack spacing={1}>
          <Label color={state[1]}>{state[0]}</Label>
          <Typography variant="caption">{PreparationStates[item.preparation_state] || item.preparation_state}</Typography>
          <Typography variant="caption">{WindowStates[item.window_state] || item.window_state}</Typography>
        </Stack>
      </TableCell>
      <TableCell>
        <Typography variant="body2">{item.last_provider_status || '尚无上游状态'}</Typography>
        <Typography variant="caption" color="error">
          {item.last_error_code}
        </Typography>
        <Typography variant="caption" display="block">
          查单次数：{item.query_count}
        </Typography>
        {item.payment_state !== 'paid' && (
          <Button size="small" disabled={querying} onClick={query}>
            {querying ? '正在查单…' : '认证查单'}
          </Button>
        )}
      </TableCell>
    </TableRow>
  );
}
OrderTableRow.propTypes = { item: PropTypes.object, onRefresh: PropTypes.func, onGatewayCredentials: PropTypes.func };
