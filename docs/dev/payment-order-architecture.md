---
title: "Payment Order 协议与原子入账"
layout: doc
outline: deep
lastUpdated: true
---

# Payment Order 协议与原子入账

## 文档状态

- 状态：当前实现。
- 适用范围：Order 创建与恢复、网关 adapter 与配置资源生命周期、通知/认证查询观察、原子入账、到期查单与关单。
- 文档口径：本文是实现 contract；真实商户与运营商联调、部署签收边界见文末“验收与发布边界”。

本实现覆盖易支付、支付宝当面付/PC/WAP、微信 Native 和 Stripe Checkout。资金归属使用已有 Order；`payment/types` 是只依赖标准库的中立契约，`payment` 注册 factory 并编排，adapter 只负责供应商协议。行为接口由中立包定义并由 `payment` 重导出，避免 adapter 反向依赖 service。决策见 [ADR-0029](../adr/0029-make-payment-order-the-credit-owner.md)。

## 创建与恢复

`POST /api/user/order` 接受 `uuid`、正额 `amount`、必需 `request_key` 以及可选 `product`、`method`。服务先按用户和请求键寻找原订单，对照规范化原始业务输入；旧单不使用最新费率重新报价。

新单使用 decimal 按折扣、手续费、汇率顺序计算，在最终现金金额落单时一次 HALF_UP。当前 CNY/USD 精度均为 2；示例 `101 × 0.85 × 1.03 × 7` 冻结为 `61898` 分 CNY。额度、金额、身份、商品描述、通知/返回地址和本地截止一起保存。

```text
保存 new Order 并确认提交
→ CAS new → claimed，写入 claim ID 与 30 秒准备 deadline
→ 锁外执行一次 PreparePayment
→ 按原 claim 条件保存 ready/rejected/unknown 及动作/资源
→ 返回原单冻结金额和当前可交付动作
```

`new` 可以经同键请求恢复首次准备。未过 deadline 的 `claimed` 仍准备中；到期远端创建只转 `unknown`，查询未命中不会触发第二次创建。纯本地签名表单可以重新生成原冻结动作。迟到准备响应不能覆盖已入账事实或已绑定的其他资源。INSERT/claim 提交不明时，未确认执行权就不调用 SDK。

## 身份、金额与 SQL 约束

网关种类、支付产品、用户付款工具和前端动作分别使用 Kind、ProductCode、MethodCode 和 NextAction.Kind。

| 网关 | 固定产品 | 交易命名空间 | 稳定付款引用 |
| --- | --- | --- | --- |
| 易支付 | epay.checkout | 已固定运营端点/profile + pid | 平台 trade_no |
| 支付宝 | alipay.facepay/pagepay/wappay | 支付宝环境 + seller_id | trade_no |
| 微信 | wxpay.native | 微信环境 + mchid | transaction_id |
| Stripe | stripe.checkout | 认证账户 + test/live | PaymentIntent ID |

namespace 用规范 JSON 数组编码。产品、付款方式和凭证 revision 不拆分交易空间；Order 仍另外严格匹配自己的 GatewayID、应用绑定与商户身份。

数据库建立 `trade_no` 唯一、`(user_id, request_key)` 唯一、`(transaction_namespace, provider_transaction_id)` 唯一。尚未取得交易引用时保存 NULL，已有真实非空引用不因未付款或未确认而清除；允许同商户多张未付款订单。paid CHECK 要求正额、非空交易及完整且相等的确认金额/币种。

入账核对字段是订单应付总额：易支付 signed money、支付宝 total_amount、微信 amount.total/currency、Stripe Session amount_total/currency。优惠后 payer_total/buyer_pay_amount、receipt_amount 和供应商净额不是等额核对值。严格 decimal string 解析不接受超精度截断或浮点容差。

## 统一观察与完整信用事务

通知原始 method/query/headers/body 由绑定 adapter 验签、解密、分类；需要补全成功事实时使用服务端认证查询。浏览器返回参数不能构造证据。`VerifyNotification` 没有 ResponseWriter，供应商 SDK 无权提前 ACK。

```text
已验签通知 / 认证查询
→ ApplyPaymentObservation
→ 成功：CompleteOrderPayment
  锁定原 Order，核对必要事实
  已 paid 且同一事实：不再增加额度
  认领 namespace + transaction
  同事务 CreditUserRecharge：锁原用户，更新余额和晋级组
  保存 Order.paid 并提交
→ 提交后缓存失效及展示日志
→ 按 adapter 输出成功/重复/忽略/拒绝/临时失败 ACK
```

可选事件 ID、资源 ID、付款时间不参与同单重复事实比较；已有资源必须保持归属。失败回滚全部资金字段。提交不明只重读同单必要事实，不重放余额命令。跨订单重复交易明确拒绝；普通 SQL 或补证据失败不能伪装成功。

历史 soft-deleted 用户按原 ID 入账；物理删除且无法证明归属时拒绝。API/Telegram 兑换统一进入 `model.Redeem`，在自己的事务里记录实际兑换者、兑换状态、余额和组；兑换码不冒充支付网关。

晋级在充值、兑换、注册/邀请奖励和管理员增减余额的事务内检查。按变动后的 `Quota + UsedQuota` 匹配已启用的晋级组，等价于变动前余额加已消费再加本次增减，不重复加本次额度，也不新增累计字段。沿用 `min <= 总额 < max`、`max=0` 无上限和较高 min 优先的规则；扣减后可以匹配较低区间，没有匹配项则保留原组。管理员手动指定组仍走原编辑入口。

消费预扣不重算分组，也不计入已消费；Confirm 将最终消费与余额差额同事务写入后重算分组，Cancel 退回预扣后也重算。多请求在途时仍可能暂时匹配较低区间，最后一笔正常结算后恢复一致。异常情况下管理员可提交零额度增减，只重算分组；详见[用户自动分组最终一致性方案](./user-group-quota-consistency-plan.md)。

## 到期、查单与关单

准备状态为 `new/claimed/ready/rejected/unknown`，付款状态为 `unconfirmed/processing/paid`，窗口状态为 `open/local_expired/provider_closed`。paid 不因后到未付、失败或关闭观察倒退。

本地窗口冻结且必需：易支付、微信、Stripe 默认三小时；支付宝十五分钟。可执行动作截止取本地截止与已知更早的供应商/动作截止。支付宝 WAP 绝对期限按锁定 SDK 的分钟精度向下取整；其他对应产品按其精度编码。易支付固定 profile 不声明远端期限或关单能力。

`GET /api/user/order/status` 从 SQL 读取用户自己的原单并设置 `Cache-Control: no-store`，不调用 Prepare 或上游查询。未 ready、processing、paid 或截止到达均返回 `next_action.kind=none`。

前端收到创建结果先展示冻结金额、币种、额度和订单号；用户确认时再读取该单状态，核对冻结事实和必需截止，紧邻执行时按 `server_now + 已过时长` 检查。重新打开、pageshow、恢复可见会先撤下旧动作再刷新；读取失败不回退旧表单、链接或二维码。已经交付的动作无法保证远端撤回，合法迟到成功仍可入账。

用户主动查询为 `POST /api/user/order/query`，管理查询为 `POST /api/payment/order/:trade_no/query`。SQL `NextQueryAt` 领取全实例共享查询机会：前十分钟至少间隔三十秒，其后至少五分钟。定时 reconciliation 每三十秒小批观察，自动观察范围为二十四小时，缺少可用查询引用的订单不占据查询扫描；超出自动范围保留待人工核查与历史通知入口。查询不会重新创建。

关单先取得新的认证查询结果。已付款/处理中不关单；只允许一次持久声明后的 close 调用。明确关闭才记录 provider_closed；已付结果继续认证查询并走相同入账入口，未知结果仅观察，不重放写动作。

## 配置与资源生命周期

普通管理编辑只更新显式业务白名单，支持部分更新；身份、配置串及凭证版本不会被旧表单覆盖。`PUT /api/payment/:id/credentials` 使用 `expected_revision` 和同主体配置完成轮换，历史 soft-deleted 网关也保留这一维护入口。

新增配置先保存 configuring 身份，再初始化候选并完成可选 webhook setup；失败保留 failed 身份并释放候选资源。只有校验、setup、SQL 和资源发布完成才开放新单。Stripe 配置所有 required events，读取已有 endpoint 不清空 secret。

每个 `GatewayID + CredentialRevision + ProtocolProfile` 复用资源组。候选失败和 CAS 失败同步清理；轮换后拒绝重新取得旧 snapshot。旧组的在途句柄归零后取消 context，同步等待 CloseResources 结束；服务关闭也等待已 draining 的旧组。

微信使用官方 v0.2.20 的独立 `CertificateDownloader`；业务 client 和通知处理器共享本 revision 的证书访问器。首次下载同步完成，完整初始化后才启动适配器拥有的刷新任务。刷新串行执行，每六小时调用一次，每次下载显式设置十五秒超时；周期按[官方小于十二小时的要求](https://pay.wechatpay.cn/doc/v3/merchant/4012551764)选定。后续刷新失败保留上次成功证书，记录本地网关 ID 和 revision，下一周期继续尝试；未知证书或无效签名不放行。退役取消下载，并在资源池锁外等待任务实际退出；不使用 SDK manager 的停止机制。

历史订单查询使用原网关当前同主体凭据，旧资源只保留到在途句柄结束。APIv3 密钥轮换需按[官方说明](https://pay.wechatpay.cn/doc/v3/merchant/4012072195)协调相关服务，不能保证旧通知密文始终能被新密钥解密；无法验证的通知返回失败，由认证查单或人工核查恢复原单事实。

## 验收与发布边界

公共第五 fake 网关分别覆盖仅通知、仅认证查询和没有可信完成路径的能力组合；controller/model 不包含第五网关特判。真实适配器 fixture 覆盖签名、SDK HTTP、金额及一次 SQL 入账；微信还验证新凭证下载新平台证书与实际 worker 退役。

这些测试不代表真实支付宝沙箱、微信商户、Stripe 商户或易支付运营商联调通过。`epay.form-md5.v1` 是本仓库 GET 通知、MD5 表单和 CNY 元报价的固定契约，实际运营商仍需核实通知重投和金额语义。

部署前执行 U1 停机升级签收：核实兑换归属、旧单商户/交易事实、异常清单及最终约束。存在历史支付数据而缺身份字段时迁移前阻断，不能用 AutoMigrate 默认值充当历史事实。晋级直接沿用已有余额和已消费字段，无需回填充值累计；历史成功订单和兑换不能再次增加余额；没有签收则保持维护，不恢复旧 writer。
