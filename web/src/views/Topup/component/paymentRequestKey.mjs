const REQUEST_KEY_MAX_LENGTH = 128;

const isUsableRequestKey = (value) =>
  typeof value === 'string' && value.length > 0 && value.length <= REQUEST_KEY_MAX_LENGTH && value.trim() === value;

const formatUUIDv4 = (bytes) => {
  const hex = Array.from(bytes, (byte) => byte.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
};

export const createPaymentRequestKey = (cryptoSource = globalThis.crypto) => {
  if (typeof cryptoSource?.randomUUID === 'function') {
    try {
      const key = cryptoSource.randomUUID();
      if (isUsableRequestKey(key)) return key;
    } catch {
      // 在不安全上下文中 randomUUID 可能不可用，继续使用 getRandomValues 回退。
    }
  }

  if (typeof cryptoSource?.getRandomValues !== 'function') {
    throw new Error('当前浏览器不支持安全随机数，请更新浏览器后重试');
  }

  const bytes = new Uint8Array(16);
  try {
    cryptoSource.getRandomValues(bytes);
  } catch {
    throw new Error('无法生成安全付款请求键，请重试');
  }
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  return formatUUIDv4(bytes);
};

export const isUsablePaymentRequestKey = isUsableRequestKey;
