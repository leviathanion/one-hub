import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import { rememberOperation, restoreOperation } from './paymentFlow.mjs';
import { createPaymentRequestKey, isUsablePaymentRequestKey } from './paymentRequestKey.mjs';

const topupSource = await readFile(new URL('./TopupCard.jsx', import.meta.url), 'utf8');

const deterministicCrypto = (bytes) => ({
  getRandomValues(target) {
    target.set(bytes);
    return target;
  }
});

test('优先使用 randomUUID，并满足服务端 request_key 的长度与空白约束', () => {
  let fallbackCalls = 0;
  const cryptoSource = {
    randomUUID: () => 'uuid-from-randomUUID',
    getRandomValues: () => {
      fallbackCalls += 1;
    }
  };

  assert.equal(createPaymentRequestKey(cryptoSource), 'uuid-from-randomUUID');
  assert.equal(fallbackCalls, 0);
  assert.equal(isUsablePaymentRequestKey('uuid-from-randomUUID'), true);
  assert.equal(isUsablePaymentRequestKey(` ${'uuid-from-randomUUID'}`), false);
  assert.equal(isUsablePaymentRequestKey(''), false);
  assert.equal(isUsablePaymentRequestKey('x'.repeat(129)), false);
});

test('randomUUID 不可用时使用 getRandomValues 生成规范 UUID v4', () => {
  const key = createPaymentRequestKey(
    deterministicCrypto(Uint8Array.from([0x00, 0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff]))
  );

  assert.equal(key, '00112233-4455-4677-8899-aabbccddeeff');
  assert.equal(key[14], '4');
  assert.ok(['8', '9', 'a', 'b'].includes(key[19]));
  assert.equal(isUsablePaymentRequestKey(key), true);
});

test('randomUUID 抛错时仍可安全回退；全部随机能力不可用时明确失败', () => {
  const fallback = createPaymentRequestKey({
    randomUUID: () => {
      throw new Error('insecure context');
    },
    ...deterministicCrypto(Uint8Array.from({ length: 16 }, (_, index) => index))
  });
  assert.equal(fallback, '00010203-0405-4607-8809-0a0b0c0d0e0f');

  assert.throws(() => createPaymentRequestKey({}), /不支持安全随机数/);
  assert.throws(
    () =>
      createPaymentRequestKey({
        getRandomValues() {
          throw new Error('random failure');
        }
      }),
    /无法生成安全付款请求键/
  );
});

test('同一付款 operation 保存、恢复和重试复用同一 request_key，新 operation 才换键', () => {
  const stored = new Map();
  const storage = { setItem: (key, value) => stored.set(key, value), getItem: (key) => stored.get(key) };
  const first = { request_key: 'first-key', uuid: 'gateway-a', amount: 101 };
  const second = { request_key: 'second-key', uuid: 'gateway-b', amount: 202 };

  assert.equal(rememberOperation(storage, first), true);
  assert.deepEqual(restoreOperation(storage), first);
  assert.equal(restoreOperation(storage).request_key, first.request_key);
  assert.equal(rememberOperation(storage, second), true);
  assert.deepEqual(restoreOperation(storage), second);
  assert.notEqual(first.request_key, second.request_key);
});

test('随机或存储失败时不产生可恢复 operation，组件在打开付款前检查两者', () => {
  const failingStorage = {
    setItem() {
      throw new Error('storage disabled');
    },
    getItem() {
      return JSON.stringify({ uuid: 'gateway', amount: 101 });
    }
  };
  assert.equal(rememberOperation(failingStorage, { request_key: 'no-submit', uuid: 'gateway', amount: 101 }), false);
  assert.equal(restoreOperation(failingStorage), null);

  assert.doesNotMatch(topupSource, /crypto\.randomUUID/);
  assert.doesNotMatch(topupSource, /Math\.random/);
  assert.match(topupSource, /createPaymentRequestKey\(\)/);
  const creationBlock = topupSource.match(/let requestKey;[\s\S]*?setOpen\(true\);\n  \};/);
  assert.ok(creationBlock, '创建付款 operation 的代码块应存在');
  assert.match(creationBlock[0], /if \(!rememberOperation\(sessionStorage, nextOperation\)\)/);
  assert.ok(
    creationBlock[0].indexOf('if (!rememberOperation(sessionStorage, nextOperation))') <
      creationBlock[0].indexOf('setOperation(nextOperation)')
  );
  assert.ok(creationBlock[0].indexOf('setOperation(nextOperation)') < creationBlock[0].indexOf('setOpen(true)'));
});

test('恢复旧订单、普通重开和显式新订单保持明确的 key 生命周期', () => {
  assert.match(topupSource, /if \(operation && !newOrder\)/);
  assert.match(topupSource, /setOpen\(true\);\n      return;/);
  assert.match(topupSource, /onClick=\{\(\) => handlePay\(true\)\}/);
  assert.match(topupSource, /<PayDialog open=\{open\} onClose=\{onClosePayDialog\} operation=\{operation\} \/>/);
});
