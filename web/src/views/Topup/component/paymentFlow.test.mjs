import test from 'node:test';
import assert from 'node:assert/strict';
import {
  estimateTotal,
  formatOrderTotal,
  confirmOrder,
  canDeliver,
  deliverAction,
  validateAction,
  remainingTime,
  restoreOperation,
  rememberOperation
} from './paymentFlow.mjs';

const order = (changes = {}) => ({
  trade_no: 'frozen-order',
  preparation_state: 'ready',
  payment_state: 'unconfirmed',
  window_state: 'open',
  server_now: '2026-09-06T02:00:00Z',
  local_display_until: '2026-09-06T02:15:00Z',
  order_total: { minor: 61898, currency: 'CNY', exponent: 2 },
  quota: 101,
  next_action: { kind: 'qr_code', valid_until: '2026-09-06T02:15:00Z', qr_code: { content: 'weixin://native-pay' } },
  ...changes
});

test('G28：预估最终只舍入一次，确认显示冻结整数金额', () => {
  assert.equal(estimateTotal(101, 0.85, { percent_fee: 0.03, fixed_fee: 0, currency: 'CNY' }, 7), '618.98');
  assert.equal(formatOrderTotal(order().order_total), '618.98 CNY');
  assert.equal(estimateTotal(101, 0.85, { percent_fee: 0.03, fixed_fee: 2, currency: 'CNY' }, 7), '614.95');
});

test('G31：本地与动作截止均必需，以服务器时间和经过时间检查', () => {
  const frozen = order();
  assert.equal(remainingTime(frozen, 100, 200), 899900);
  assert.equal(canDeliver(frozen, 0, 900000), false);
  assert.equal(canDeliver(order({ local_display_until: null }), 0, 0), false);
  assert.equal(canDeliver(order({ next_action: { ...frozen.next_action, valid_until: null } }), 0, 0), false);
  assert.equal(canDeliver(order({ next_action: { ...frozen.next_action, valid_until: '2026-09-06T02:01:00Z' } }), 0, 60000), false);
});

test('G32：确认读取同一订单的最新动作，不使用打开时保存的内容', async () => {
  const requested = [];
  const latest = order({ next_action: { ...order().next_action, qr_code: { content: 'latest-content' } } });
  const result = await confirmOrder(
    order(),
    async (tradeNo) => {
      requested.push(tradeNo);
      return latest;
    },
    () => 100
  );
  assert.deepEqual(requested, ['frozen-order']);
  assert.equal(result.action.qr_code.content, 'latest-content');
});

test('G31/G32：已到账、处理中、非ready、窗口到期与无动作都不交付', async () => {
  for (const changes of [
    { payment_state: 'paid' },
    { payment_state: 'processing' },
    { preparation_state: 'unknown' },
    { window_state: 'local_expired' },
    { window_state: 'provider_closed' },
    { next_action: { kind: 'none' } }
  ]) {
    const result = await confirmOrder(
      order(),
      async () => order(changes),
      () => 0
    );
    assert.equal(result.action, null, JSON.stringify(changes));
  }
});

test('G32：网络失败不能回退旧动作，读取期间跨过截止也不交付', async () => {
  await assert.rejects(
    confirmOrder(order(), async () => {
      throw new Error('network failed');
    }),
    /network failed/
  );
  let elapsed = 0;
  const result = await confirmOrder(
    order(),
    async () => {
      elapsed = 900000;
      return order();
    },
    () => elapsed
  );
  assert.equal(result.action, null);
});

test('G32：订单号、金额、币种、额度、截止不一致要求重新确认', async () => {
  for (const changes of [
    { trade_no: 'another-order' },
    { quota: 102 },
    { local_display_until: '2026-09-06T02:20:00Z' },
    { order_total: { minor: 61901, currency: 'CNY', exponent: 2 } },
    { order_total: { minor: 61898, currency: 'USD', exponent: 2 } }
  ]) {
    const result = await confirmOrder(
      order(),
      async () => order(changes),
      () => 0
    );
    assert.equal(result.changed, true);
    assert.equal(result.action, null);
  }
});

test('G23：付款表单通过原生字段赋值，特殊内容与submit字段不成为HTML/脚本', () => {
  const elements = [];
  let submitted;
  let removed = false;
  const document = {
    body: { appendChild: (form) => elements.push(form) },
    createElement: (tag) => ({
      tag,
      children: [],
      appendChild(child) {
        this.children.push(child);
      },
      remove() {
        removed = true;
      }
    })
  };
  const browserWindow = {
    HTMLFormElement: {
      prototype: {
        submit() {
          submitted = this;
        }
      }
    }
  };
  deliverAction(
    {
      kind: 'form',
      form: {
        method: 'POST',
        action_url: 'https://pay.example/submit',
        fields: { submit: 'normal-field', content: '<script>alert(1)</script>' }
      }
    },
    document,
    browserWindow
  );
  assert.equal(submitted, elements[0]);
  assert.equal(submitted.children[1].value, '<script>alert(1)</script>');
  assert.ok(submitted.children.every((input) => input.type === 'hidden'));
  assert.equal(removed, true);
  assert.throws(() => validateAction({ kind: 'redirect', redirect: { url: 'javascript:alert(1)' } }));
  assert.throws(() => validateAction({ kind: 'form', form: { method: 'PUT', action_url: 'https://pay.example', fields: {} } }));
  assert.throws(() =>
    validateAction({ kind: 'qr_code', qr_code: { content: 'weixin://native', optional_launch_url: 'javascript:alert(1)' } })
  );
});

test('同一标签恢复原请求键和已知订单号，恢复无需重新报价', () => {
  const stored = new Map();
  const storage = { setItem: (key, value) => stored.set(key, value), getItem: (key) => stored.get(key) };
  const operation = { request_key: 'stable-key', uuid: 'gateway', amount: 101, trade_no: 'original-order' };
  rememberOperation(storage, operation);
  assert.deepEqual(restoreOperation(storage), operation);
});
