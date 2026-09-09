const PaymentType = { epay: '易支付', alipay: '支付宝', wxpay: '微信支付', stripe: 'Stripe' };
const CurrencyType = { CNY: '人民币', USD: '美元' };
const PaymentProducts = {
  epay: { 'epay.checkout': '签名收银台' },
  alipay: { 'alipay.facepay': '当面付', 'alipay.pagepay': '电脑网站支付', 'alipay.wappay': '手机网站支付' },
  wxpay: { 'wxpay.native': 'Native 支付' },
  stripe: { 'stripe.checkout': 'Checkout' }
};
const text = (name, identity = false, description = '') => ({ name, type: 'text', identity, description, value: '' });
const select = (name, options, value, identity = false) => ({ name, type: 'select', options, value, identity });
const PaymentConfig = {
  epay: {
    pay_domain: text('支付域名', true),
    partner_id: text('商户号', true),
    protocol_profile: select(
      '协议方言',
      [{ name: '现有 MD5 签名表单（CNY，仅通知）', value: 'epay.form-md5.v1' }],
      'epay.form-md5.v1',
      true
    ),
    key: text('密钥'),
    pay_type: select(
      '下游支付方式',
      [
        { name: '收银台', value: '' },
        { name: '支付宝', value: 'alipay' },
        { name: '微信', value: 'wxpay' },
        { name: 'QQ', value: 'qqpay' },
        { name: '京东', value: 'jdpay' },
        { name: '银联', value: 'bank' },
        { name: 'PayPal', value: 'paypal' },
        { name: 'USDT 钱包（订单仍以 CNY 计价）', value: 'usdt' }
      ],
      '',
      true
    )
  },
  alipay: {
    app_id: text('应用 ID', true),
    seller_id: text('预期收款商户号', true),
    environment: select(
      '环境',
      [
        { name: '生产', value: 'production' },
        { name: '沙箱', value: 'sandbox' }
      ],
      'production',
      true
    ),
    private_key: text('应用私钥'),
    public_key: text('支付宝公钥')
  },
  wxpay: {
    app_id: text('AppID', true),
    mch_id: text('商户号', true),
    mch_certificate_serial_number: text('商户证书序列号'),
    mch_apiv3_key: text('商户 APIv3 密钥'),
    mch_private_key: text('商户私钥')
  },
  stripe: {
    secret_key: text('API 私钥'),
    webhook_secret: text('Webhook 验证密钥', false, '自动配置时由服务端获取；已有 endpoint 须保留相应验证密钥。'),
    account_id: { ...text('Stripe 账户 ID', true), generated: true },
    environment: { ...text('环境', true), generated: true },
    webhook_endpoint_id: text('Webhook Endpoint ID', false, '已有通知端点 ID；留空时按本网关通知地址查找或创建。')
  }
};
const defaultConfig = (kind) =>
  Object.fromEntries(
    Object.entries(PaymentConfig[kind] || {})
      .filter(([, field]) => !field.generated)
      .map(([key, field]) => [key, field.value])
  );
export { PaymentConfig, PaymentType, CurrencyType, PaymentProducts, defaultConfig };
