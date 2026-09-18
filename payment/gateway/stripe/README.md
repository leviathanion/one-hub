# Stripe Checkout

本适配器锁定 `stripe-go/v80 v80.2.1` 和 API 头 `2024-09-30.acacia`，只接受冻结的 CNY/USD 订单应付总额，收银台自动换币（Adaptive Pricing）不在支持范围内。

`adaptive_pricing` 逐 Session 参数到 API 头 `2024-11-20.acacia`、Go SDK `v81.1.0` 才提供；锁定版本的创建参数不包含该字段，`Params.AddExtra` 只能添加请求字段，不能让服务端支持未承诺的参数，因此不作为锁定协议保证。

启用 Stripe 新单前，部署负责人须在该商户实际使用的 sandbox/test 与 live 环境关闭 Dashboard 的 Adaptive Pricing，并保存配置与新测试订单的币种/总额核验记录。`SetupStatus=ready` 只表示本地配置与 webhook 配置完成，不证明 Dashboard 的这项设置；缺少前置时收银台换币可能使付款金额/币种不符合原订单，无法自动入账。

若需由代码逐单关闭该功能，应单独升级到支持该参数的 API/SDK，加入显式 `enabled=false`，并验证请求字段与 webhook 版本后再移除 Dashboard 前置。
