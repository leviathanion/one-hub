import Decimal from 'decimal.js';
import { isUsablePaymentRequestKey } from './paymentRequestKey.mjs';

// 页面只展示预估；最终确认使用服务端冻结的整数金额。
export function estimateTotal(amount, discount, payment, usdRate) {
  const base = new Decimal(amount || 0).mul(discount || 1);
  const fee = Number(payment?.fixed_fee) > 0 ? new Decimal(payment.fixed_fee) : base.mul(payment?.percent_fee || 0);
  return base
    .add(fee)
    .mul(payment?.currency === 'CNY' ? usdRate || 1 : 1)
    .toFixed(2, Decimal.ROUND_HALF_UP);
}

export function formatOrderTotal(money) {
  if (!money) return '—';
  return `${new Decimal(money.minor).div(new Decimal(10).pow(money.exponent)).toFixed(money.exponent)} ${money.currency}`;
}

export function sameFrozenOrder(a, b) {
  return (
    a?.trade_no === b?.trade_no &&
    a?.quota === b?.quota &&
    a?.order_total?.minor === b?.order_total?.minor &&
    a?.order_total?.currency === b?.order_total?.currency &&
    a?.order_total?.exponent === b?.order_total?.exponent &&
    a?.local_display_until === b?.local_display_until
  );
}

export function remainingTime(order, receivedAt, now = performance.now()) {
  const serverNow = Date.parse(order?.server_now);
  const localUntil = Date.parse(order?.local_display_until);
  const actionUntil = Date.parse(order?.next_action?.valid_until);
  if (![serverNow, localUntil, actionUntil, receivedAt, now].every(Number.isFinite)) return 0;
  return Math.max(0, Math.min(localUntil, actionUntil) - serverNow - Math.max(0, now - receivedAt));
}

function webURL(value) {
  const url = new URL(value);
  if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password) throw new Error('付款地址无效');
  return url.href;
}

export function validateAction(action) {
  if (action?.kind === 'redirect') webURL(action.redirect?.url);
  else if (action?.kind === 'form') {
    webURL(action.form?.action_url);
    if (
      !['GET', 'POST'].includes(action.form?.method) ||
      !action.form.fields ||
      Object.values(action.form.fields).some((value) => typeof value !== 'string')
    )
      throw new Error('付款表单无效');
  } else if (action?.kind === 'qr_code') {
    if (typeof action.qr_code?.content !== 'string' || !action.qr_code.content) throw new Error('付款码无效');
    if (action.qr_code.optional_launch_url) webURL(action.qr_code.optional_launch_url);
  } else throw new Error('当前订单没有可执行的付款入口');
  return action;
}

export function canDeliver(order, receivedAt, now = performance.now()) {
  return (
    order?.preparation_state === 'ready' &&
    order.payment_state === 'unconfirmed' &&
    order.window_state === 'open' &&
    remainingTime(order, receivedAt, now) > 0
  );
}

// 确认只读原单，失败直接向调用者报错，没有保存动作的回退路径。
export async function confirmOrder(original, readStatus, clock = () => performance.now()) {
  const receivedAt = clock();
  const order = await readStatus(original.trade_no);
  if (!sameFrozenOrder(original, order)) return { order, receivedAt, changed: true, action: null };
  const action = canDeliver(order, receivedAt, clock()) ? validateAction(order.next_action) : null;
  return { order, receivedAt, changed: false, action };
}

export function restoreOperation(storage) {
  try {
    const operation = JSON.parse(storage.getItem('topup-current-order'));
    return isUsableStoredOperation(operation) ? operation : null;
  } catch {
    return null;
  }
}

export function rememberOperation(storage, operation) {
  try {
    if (!isUsableStoredOperation(operation)) return false;
    storage.setItem('topup-current-order', JSON.stringify(operation));
    return true;
  } catch {
    return false;
  }
}

function isUsableStoredOperation(operation) {
  return (
    operation &&
    typeof operation === 'object' &&
    isUsablePaymentRequestKey(operation.request_key) &&
    typeof operation.uuid === 'string' &&
    operation.uuid.trim() === operation.uuid &&
    operation.uuid.length > 0 &&
    Number.isSafeInteger(operation.amount) &&
    operation.amount > 0
  );
}

export function deliverAction(action, document, browserWindow) {
  validateAction(action);
  if (action.kind === 'redirect') browserWindow.location.assign(action.redirect.url);
  if (action.kind === 'form') {
    const form = document.createElement('form');
    form.method = action.form.method;
    form.action = action.form.action_url;
    // 原生控件赋值，不能插入网关返回的 HTML；字段名也可能叫 submit。
    for (const [name, value] of Object.entries(action.form.fields)) {
      const input = document.createElement('input');
      input.type = 'hidden';
      input.name = name;
      input.value = value;
      form.appendChild(input);
    }
    document.body.appendChild(form);
    try {
      browserWindow.HTMLFormElement.prototype.submit.call(form);
    } finally {
      form.remove();
    }
  }
}

export function orderMessage(order, expired = false) {
  if (order?.payment_state === 'paid') return '支付成功，额度已到账';
  if (order?.payment_state === 'processing') return '支付处理中，请勿重复付款';
  if (order?.window_state === 'provider_closed') return '上游已关闭本单付款入口';
  if (order?.window_state === 'local_expired' || expired) return '本单付款入口已过期，已有付款仍在核查';
  if (order?.preparation_state === 'unknown' || order?.preparation_state === 'claimed') return '支付创建结果待确认';
  if (order?.preparation_state === 'rejected') return '付款入口准备失败';
  if (order?.preparation_state === 'new') return '订单已保存，付款入口尚未准备';
  return '请核对本订单的冻结金额和充值额度';
}
