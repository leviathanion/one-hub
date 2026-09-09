import PropTypes from 'prop-types';
import { useState, useEffect, useCallback, useRef } from 'react';
import { Alert, Button, Dialog, DialogContent, DialogTitle, IconButton, Stack, Typography } from '@mui/material';
import CloseIcon from '@mui/icons-material/Close';
import { useTheme } from '@mui/material/styles';
import { QRCode } from 'react-qrcode-logo';
import { API } from 'utils/api';
import { renderQuota } from 'utils/common';
import {
  canDeliver,
  confirmOrder,
  deliverAction,
  formatOrderTotal,
  orderMessage,
  remainingTime,
  rememberOperation,
  sameFrozenOrder
} from './paymentFlow.mjs';

const readStatus = async (tradeNo) => {
  const { data } = await API.get('/api/user/order/status', {
    params: { trade_no: tradeNo },
    timeout: 15000,
    headers: { 'Cache-Control': 'no-cache' }
  });
  if (!data.success) throw new Error(data.message || '读取订单失败');
  return data.data;
};

const PayDialog = ({ open, onClose, operation }) => {
  const theme = useTheme();
  const [snapshot, setSnapshot] = useState(null);
  const [action, setAction] = useState(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [now, setNow] = useState(performance.now());
  const current = useRef({ key: null, snapshot: null, creation: null, generation: 0, busy: false });

  const save = useCallback(
    (order, receivedAt) => {
      const next = { order, receivedAt };
      current.current.snapshot = next;
      rememberOperation(sessionStorage, { ...operation, trade_no: order.trade_no });
      setSnapshot(next);
      setNow(performance.now());
      return next;
    },
    [operation]
  );

  const refresh = useCallback(
    async ({ prepare = false, query = false, invalidate = false } = {}) => {
      const state = current.current;
      if (!operation || (state.busy && !invalidate)) return;
      if (invalidate) {
        state.generation += 1;
        setAction(null);
      }
      const generation = state.generation;
      state.busy = true;
      setBusy(true);
      setError('');
      try {
        const receivedAt = performance.now();
        let order;
        const tradeNo = state.snapshot?.order.trade_no || operation.trade_no;
        if (tradeNo && !prepare) {
          if (query) {
            const response = await API.post('/api/user/order/query', { trade_no: tradeNo }, { timeout: 15000 });
            if (!response.data.success) throw new Error(response.data.message || '上游查单失败');
          }
          order = await readStatus(tradeNo);
        } else {
          // 同一请求在 effect 重跑和网络重试时保持 request_key；普通 GET 不准备订单。
          if (!state.creation) {
            const input = { ...operation };
            delete input.trade_no;
            const request = API.post('/api/user/order', input, { timeout: 45000 })
              .then(({ data }) => {
                if (!data.success) throw new Error(data.message || '创建订单失败');
                return data.data;
              })
              .finally(() => {
                if (state.creation === request) state.creation = null;
              });
            state.creation = request;
          }
          order = await state.creation;
        }
        if (generation !== state.generation) return;
        if (tradeNo && order.trade_no !== tradeNo) throw new Error('订单号不一致，请重新刷新本单');
        if (state.snapshot && !sameFrozenOrder(state.snapshot.order, order)) {
          setAction(null);
          setError('订单确认信息发生变化，请重新核对金额和额度');
        }
        save(order, receivedAt);
        setAction((previous) =>
          canDeliver(order, receivedAt) && JSON.stringify(previous) === JSON.stringify(order.next_action) ? previous : null
        );
      } catch (err) {
        if (generation !== state.generation) return;
        setError(err.message || '读取订单失败，请重新刷新本单');
        setAction(null);
      } finally {
        if (generation === state.generation) {
          state.busy = false;
          setBusy(false);
        }
      }
    },
    [operation, save]
  );

  useEffect(() => {
    const state = current.current;
    if (state.key !== operation?.request_key) {
      state.generation += 1;
      state.key = operation?.request_key;
      state.snapshot = null;
      state.creation = null;
      state.busy = false;
      setSnapshot(null);
      setAction(null);
    }
    if (!open || !operation) return;
    refresh({ invalidate: true });
    const resume = () => {
      if (document.visibilityState === 'visible') refresh({ invalidate: true });
      else {
        state.generation += 1;
        state.busy = false;
        setBusy(false);
        setAction(null);
      }
    };
    window.addEventListener('pageshow', resume);
    document.addEventListener('visibilitychange', resume);
    const poll = window.setInterval(() => {
      if (document.visibilityState === 'visible' && state.snapshot && state.snapshot.order.payment_state !== 'paid') refresh();
    }, 3000);
    const tick = window.setInterval(() => {
      setNow(performance.now());
      if (state.snapshot && !canDeliver(state.snapshot.order, state.snapshot.receivedAt)) setAction(null);
    }, 500);
    return () => {
      state.generation += 1;
      state.busy = false;
      setAction(null);
      window.removeEventListener('pageshow', resume);
      document.removeEventListener('visibilitychange', resume);
      window.clearInterval(poll);
      window.clearInterval(tick);
    };
  }, [open, operation, refresh]);

  const confirm = async (launch = false) => {
    const state = current.current;
    if (state.busy || !state.snapshot) return;
    state.busy = true;
    setBusy(true);
    setAction(null);
    setError('');
    const generation = state.generation;
    try {
      const result = await confirmOrder(state.snapshot.order, readStatus);
      if (generation !== state.generation) return;
      if (result.order.trade_no !== state.snapshot.order.trade_no) throw new Error('订单号不一致，请重新刷新本单');
      save(result.order, result.receivedAt);
      if (result.changed) {
        setError('订单确认信息发生变化，请重新核对金额和额度');
        return;
      }
      if (!result.action || !canDeliver(result.order, result.receivedAt)) return;
      if (launch && result.action.kind === 'qr_code' && result.action.qr_code.optional_launch_url) {
        window.location.assign(result.action.qr_code.optional_launch_url);
      } else deliverAction(result.action, document, window);
      setAction(result.action);
    } catch (err) {
      if (generation === state.generation) setError(err.message || '确认失败，请重新刷新本单');
    } finally {
      if (generation === state.generation) {
        state.busy = false;
        setBusy(false);
      }
    }
  };

  const order = snapshot?.order;
  const eligible = snapshot && canDeliver(order, snapshot.receivedAt, now);
  const expired = order?.next_action?.kind !== 'none' && snapshot && remainingTime(order, snapshot.receivedAt, now) <= 0;
  const displayedAction = eligible ? action : null;
  return (
    <Dialog open={open} onClose={onClose} fullWidth maxWidth="sm">
      <DialogTitle>确认充值订单</DialogTitle>
      <IconButton aria-label="关闭支付确认" onClick={onClose} sx={{ position: 'absolute', right: 8, top: 8 }}>
        <CloseIcon />
      </IconButton>
      <DialogContent>
        <Stack spacing={2} alignItems="center">
          {error && (
            <Alert severity="error" role="alert">
              {error}
            </Alert>
          )}
          <Typography role="status">{order ? orderMessage(order, expired) : '正在获取冻结订单…'}</Typography>
          {order && (
            <>
              <Typography>订单号：{order.trade_no}</Typography>
              <Typography variant="h3">应付金额：{formatOrderTotal(order.order_total)}</Typography>
              <Typography>充值额度：{renderQuota(order.quota)}</Typography>
              <Typography variant="body2">本单付款入口截止：{new Date(order.local_display_until).toLocaleString()}</Typography>
              {eligible && (
                <Typography variant="body2">剩余 {Math.ceil(remainingTime(order, snapshot.receivedAt, now) / 1000)} 秒</Typography>
              )}
            </>
          )}
          {displayedAction?.kind === 'qr_code' && (
            <>
              <Typography>请扫码支付</Typography>
              <QRCode
                value={displayedAction.qr_code.content}
                size={256}
                fgColor={theme.palette.primary.main}
                bgColor={theme.palette.background.paper}
              />
            </>
          )}
          {displayedAction?.kind === 'qr_code' && displayedAction.qr_code.optional_launch_url && (
            <Button disabled={busy} onClick={() => confirm(true)}>
              打开付款页面
            </Button>
          )}
          <Button variant="contained" disabled={busy || !eligible || Boolean(error)} onClick={() => confirm()}>
            {busy ? '正在核对订单…' : '确认金额并支付'}
          </Button>
          <Button disabled={busy} onClick={() => refresh({ invalidate: true })}>
            刷新本单状态
          </Button>
          {order?.can_query && (
            <Button disabled={busy} onClick={() => refresh({ query: true, invalidate: true })}>
              向上游核查付款结果
            </Button>
          )}
          {order?.preparation_state === 'new' && (
            <Button disabled={busy} onClick={() => refresh({ prepare: true, invalidate: true })}>
              继续准备本单付款入口
            </Button>
          )}
          <Typography variant="caption">关闭窗口不会关闭上游订单。若已付款，请等待原订单确认，避免重复充值。</Typography>
        </Stack>
      </DialogContent>
    </Dialog>
  );
};

PayDialog.propTypes = { open: PropTypes.bool, onClose: PropTypes.func, operation: PropTypes.object };
export default PayDialog;
