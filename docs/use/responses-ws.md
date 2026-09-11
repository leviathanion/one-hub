---
title: "Responses WebSocket"
layout: doc
outline: deep
lastUpdated: true
---

# Responses WebSocket

one-hub 通过 `GET /v1/responses` 的 WebSocket Upgrade 提供 Responses WebSocket。该入口只连接声明了 Native Responses WebSocket 能力的上游；不会把 WebSocket 请求降级为 HTTP/SSE，也不会把 Realtime 或其他协议转换成 Responses 事件。

如果候选渠道不支持 Native Responses WebSocket，请求会在上游工作开始前返回 `426 responses_ws_unsupported_for_channel`。普通 HTTP 请求不受 native WS 能力限制；HTTP Responses create 的工作后重放边界见[请求重试说明](../dev/responses-ws-attempt-replay-architecture.md)。

本文记录当前实现；交付、资源授权与账务的职责边界见[透明转发设计](../dev/responses-transparent-relay-design.md)。

## 协议边界

- 客户端事件支持 `response.create`、针对当前活动响应的 `response.steer`，以及针对已授权响应的 `response.inject`。inject 的 multi-agent 模式和工具输入 schema 由上游校验。
- `response.cancel` 属于 Realtime API，在 Responses WebSocket 上返回 `unsupported_client_event`。客户端断开连接会关闭当前上游 transport。
- 同一连接固定一个渠道。每个 `response.create` 独立读取其原始 `model`、`store`、continuation 和工具门禁；预扣与结算分别使用当时的当前价格，配置更新可能使两者不同。
- 渠道必须支持该 turn 的精确 model；连接内不切换渠道、不做 model mapping，也不改写请求 model。
- busy 时合法的 `response.create` 进入有界 FIFO，不返回 `session_busy`。父响应结束后可开始下一项，无需等待旧 inject 回执；可能创建自动后继的 steering 预扣仍会阻挡新 create。已发布的发送命令保持顺序。
- `response.completed`、`response.failed`、`response.incomplete` 为本地提供终结和用量事实。未知事件、缺省或非递增序号不阻止原帧交付；代理不会据此猜测上游已结束。新资源的归属屏障和已准入执行关联仍须成立。
- 同一已验证连接上的迟到控制回执和安全诊断仍会交付，不重开旧账务，也不计入新响应。供应商私有事件仅在 adapter 有明确语义映射时转换；公共事件不补造序号。
- provider terminal 先交付客户端，再执行日志和结算。结算失败不会用本地 error 替换已经交付的 terminal。

## 注入工具结果

`response.inject` 保留原始帧。目标可以是当前响应，也可以是仍有同连接资源证明或有效 Stored owner 的已完成响应；未知、跨用户或不属于当前渠道的目标在发送前拒绝。代理不保存完整工具调用历史，不替客户端检查 `call_id`，也不生成注入失败回执。

注入是否成功由上游的 `response.inject.created/failed` 告知。若目标已结束，客户端根据上游回执构造下一次 `response.create`。注入回执可能晚于下一响应的事件；需要先收齐回执的客户端应自行等待。单条 inject 不创建新的计费 attempt；发送结果歧义会停止连接，不自动重发。

## 执行中追加指令

收到 `response.created` 后，可用该响应 ID 发送 `response.steer`。代理保留原始事件；具体模型、执行模式和消息内容是否支持 steering，由真实上游判断。

```json
{"type":"response.steer","previous_response_id":"resp_1","input":"将范围缩小到两周内可完成。"}
```

代理在发送第一条 steering 前完成自动续接的权限、渠道、RPM 和余额准入，并预扣一次。一个尚未绑定的自动续接最多保留 64 条待观察提交，每个已接管 ID 最多 1024 字节；原始帧沿用现有队列字节限制，不累计已发送的历史流量。连接最多保留 64 个待续接父资源证明，ID 总量最多 4 MiB；状态只在当前连接存在，断线不会自动重放。

`response.steer.accepted` 表示上游已接管输入。原响应可能以 `response.incomplete`（`reason: "steered"`）或 `response.completed` 结束，随后上游自动发出新的 `response.created`。每个实际响应分别持久化资源归属、提取用量并结算，代理不发送额外的 `response.create`。

如果上游返回 `response.steer.pending`，用其 `required_input` 填入已保存的工具结果或批准决定，再发一个带同一 `previous_response_id` 的 `response.create`。该显式请求使用自己的参数重新准入；不要重跑工具或重复发送已被接管的 steering 输入。`response.steer.failed` 也会原样返回。全部提交明确失败，或父已完成、全部初始回执已消解且上游明确等待客户端输入时，释放尚未使用的自动续接预扣。未知 pending reason 原样交付，不作为提前退款依据。

同一已验证上游连接的迟到 steering 回执原样交付，不依赖当前候选或最近完成历史，不计入新响应用量。每条发送独立消费结果；已消费的重复结果不影响新响应，原发送首次报告的歧义仍会关闭连接。等待客户端续接所需的临时父资源证明独立保留，无关请求不会消费它；匹配显式请求收到新 Response 的 created 后才释放。无关联的泛化 `error` 只交付，不取消候选或推进 FIFO；连接关闭和既有超时负责有界收尾。明确关联的 create 拒绝可结束该准入，已知 workflow stop 则停止连接。

## Astra 请求与安全监控

原生 HTTP Responses 和 WebSocket `response.create` 保留工具的 `async: true`、`configuration_update` 输入项、`prompt_cache_options` 和 `prompt_cache_breakpoint`。工具由客户端执行，结果按原始 `call_id` 回传；异步工具与代理不支持的 `background:true` 是不同能力。推理更新的模型和压缩兼容性限制由上游校验；跨协议 Chat 适配器会在上游工作前拒绝无法表示的字段。

`cached_tokens`、`cache_write_tokens` 从上游用量进入结算。管理员仍需配置模型价格与缓存倍率，代理不会自动覆盖已有价格。

收到 `misalignment_policy_violation` 时，代理保留上游错误，不受可重试 HTTP 状态配置影响；WebSocket 会在交付该错误后关闭连接并停止排队工作。同一已验证上游连接的迟到停止信号也会生效，不受当前回合切换影响。

上游项目的 `safety.alert.created` Webhook 应直接配置到运营方接收端，并按 OpenAI 文档验证签名和处理重复通知。查询告警使用 `GET /v1/safety/alerts/:alert_id`，只允许管理员凭据显式指定渠道；它使用该渠道的上游项目凭据，普通用户不能查询项目级告警。代理不代管上游 Webhook 密钥或业务侧工具任务。

官方协议：[Steering](https://developers.openai.com/api/docs/guides/steering)、[异步工具](https://developers.openai.com/api/docs/guides/async-tool-calling)、[监控与告警](https://developers.openai.com/api/docs/guides/safety-checks/misalignment-monitoring)。

## 状态与路由

`store` 使用三态语义：

| 输入 | 行为 |
| --- | --- |
| 省略或 `true` | 只选择同时支持 create 和完整 Stored Response 生命周期的渠道；在首个携带 Response ID 的 provider 事件交付前持久化 owner |
| `false` | 使用普通调度；不创建 durable owner，同一 Native WS 连接内可使用 provider connection-local state |

Stored Response owner 绑定 `UserId` 和创建渠道。后续 retrieve/delete/input-items 或严格 continuation 固定回到创建渠道，不 fallback。owner miss、跨用户、tombstone 和过期对普通用户统一表现为资源不存在。

## 渠道配置

operation 支持由代理已有的 provider adapter 决定，不需要、也不能通过 `capabilities.operations` 配置。adapter 能表达某个 HTTP operation 时，代理会把请求发给真实上游；具体 endpoint 或账号不支持时，保留真实上游错误。模型是否可选仍只由渠道模型列表和模型映射决定。

官方 OpenAI adapter 默认支持原生 Responses WebSocket。Codex adapter 也支持原生 WS，但不支持完整 Stored Response 生命周期，因此通常配合 `store:false`。Azure adapter 支持原生 Responses WebSocket。

OpenAI Data Residency 的区域域名暂不支持。这个限制只针对 OpenAI Data Residency 产品，不根据 Azure、AWS 或 Google Cloud 的部署区域推断渠道能力。

自定义渠道，以及使用非官方 `BaseURL` 的 OpenAI 渠道，只有在上游确实实现原生 Responses WebSocket 协议时，才在渠道表单的 `Other(JSON)` 中显式配置：

```json
{
  "responses_ws_native": true
}
```

这个字段只允许 adapter 尝试原生 WS，不承诺上游一定支持；握手和协议错误会按真实结果返回。它不影响 HTTP Responses。官方 OpenAI 和 Codex 不需要配置此字段。

私有或本地自建的 Responses WS 上游可额外设置：

```json
{
  "responses_ws_native": true,
  "responses_ws_self_hosted": true
}
```

页面在检测到 `responses_ws_self_hosted` 时显示安全警告。该字段只影响 Responses WebSocket；`self_hosted` 属于 Realtime，两者不是别名。

WebSocket 入口不提供 HTTP 传输选择；需要 HTTP 的客户端应直接调用普通 HTTP Responses。

### Azure classic

Azure classic 的普通 HTTP Responses 仍需要 `api_version`，例如：

```json
{
  "api_version": "2024-10-01-preview"
}
```

Azure V1 使用 resource-level endpoint，不应包含 `/openai/deployments`，也不读取 `api_version`。两种 Azure adapter 都直接支持原生 Responses WebSocket；私有或本地地址仍需开启自建上游安全开关。

### Codex

Codex Responses WebSocket 直接连接原生上游 WebSocket，无传输模式配置。`/v1/realtime` 同样只使用上游 WebSocket，但仍拥有独立的协议和会话生命周期。

## 服务配置

```yaml
responses_ws:
  connect_per_credential_per_minute: 600
  pending_per_credential: 96
  pending_per_user: 96
  pending_per_group: 192
  pending_global: 512
  pending_bytes_per_credential: 67108864
  pending_bytes_per_user: 67108864
  pending_bytes_per_group: 134217728
  pending_bytes_global: 268435456
  active_per_credential: 16
  active_per_user: 16
  active_per_group: 32
  active_global: 128
  active_lease_redis_fail_open: true

  first_frame_timeout_ms: 30000
  idle_timeout_ms: 1800000
  active_turn_timeout_ms: 120000
  max_lifetime_ms: 3600000

  pending_provider_events_max_bytes: 2097152
  unsupported_scan_limit: 0
  allow_anonymous_capacity_bucket: false

responses_websocket_client_ping_interval_ms: 25000
responses_websocket_client_pong_miss_timeout_ms: 10000
responses_websocket_client_inbound_activity_timeout_ms: 60000
```

所有时间值单位为毫秒。显式 `0` 会禁用相应 timeout/liveness watchdog。

### 容量

| 配置 | 默认值 | 说明 |
| --- | ---: | --- |
| `connect_per_credential_per_minute` | 600 | 稳定 user 与 token 同时计数的建连尝试速率；`-1` 不限 |
| `pending_per_credential` | 96 | 单 token pending 槽位，只能收紧 user 上限；`-1` 不限 |
| `pending_per_user` | 96 | 稳定 user pending 槽位；`-1` 不限 |
| `pending_per_group` | 192 | 每分组 pending 槽位；`-1` 不限 |
| `pending_global` | 512 | 全局 pending 槽位；`-1` 不限 |
| `pending_bytes_per_credential` | 67108864 | 首帧 streaming read 的单 token 实际保留字节上限 |
| `pending_bytes_per_user` | 67108864 | 首帧 streaming read 的稳定 user 实际保留字节上限 |
| `pending_bytes_per_group` | 134217728 | 首帧 streaming read 的每分组实际保留字节上限 |
| `pending_bytes_global` | 268435456 | 首帧 streaming read 的全局实际保留字节上限 |
| `active_per_credential` | 16 | 单 token active 上限，只能收紧 user 上限；`-1` 不限 |
| `active_per_user` | 16 | 稳定 user active 连接上限；`-1` 不限 |
| `active_per_group` | 32 | 每分组 active 连接上限；`-1` 不限 |
| `active_global` | 128 | 全局 active 连接上限；`-1` 不限 |
| `active_lease_redis_fail_open` | true | Redis 故障时回退进程内计数；设为 false 则拒绝新 active lease |

`allow_anonymous_capacity_bucket` 只用于本地诊断。生产环境应保持 `false`，未认证请求必须在容量检查前被拒绝。

所有阶段同时占用 token、稳定 user、group 和 global 维度；多个自助 token 共享同一个 user 上限。`*_per_credential` 只提供更严格的 token 级限制，不能放宽 `*_per_user`。

### 超时与 liveness

| 配置 | 默认值 | 说明 |
| --- | ---: | --- |
| `first_frame_timeout_ms` | 30000 | Upgrade 后等待首个 `response.create`；`0` 禁用 |
| `idle_timeout_ms` | 1800000 | 仅清理没有 opening/pending/active turn 的业务空闲连接；`0` 禁用 |
| `active_turn_timeout_ms` | 120000 | provider inactivity 限制；`0` 禁用，只产生 one-hub timeout/close，不合成 provider terminal |
| `max_lifetime_ms` | 3600000 | 代理连接寿命上限；`0` 禁用 |
| `responses_websocket_client_ping_interval_ms` | 25000 | 服务端 ping 周期；`0` 禁用 |
| `responses_websocket_client_pong_miss_timeout_ms` | 10000 | pong 缺失判死；`0` 显式禁用并使 readiness 标记 degraded |
| `responses_websocket_client_inbound_activity_timeout_ms` | 60000 | 客户端入站活性限制；`0` 显式禁用并使 readiness 标记 degraded |

active watchdog 只按 provider inactivity 计时，provider 有活动时会续期；max lifetime 限制单连接最长占用时间。两者都属于 one-hub 自己的资源边界，不是 provider 已完成的证据，可显式设为 `0` 禁用。

### 缓冲与候选扫描

`pending_provider_events_max_bytes` 限制上游 send 结果确认前的 provider 事件缓冲，默认 2 MiB；另有固定事件条数上限。超限时 fail closed，并保留已存在的计费证据，不把不确定发送误判为未发送。首帧在完整物化前按实际读取 chunk 同时取得 token/user/group/global byte lease；额度不足时立即以资源错误关闭，不先复制完整 32 MiB 帧。actor mailbox 另有每连接 64 MiB 的 payload 总字节预算，避免固定事件条数与大帧相乘时放大内存占用。

busy `response.create` FIFO 同时限制帧数和 4 MiB payload bytes；上游 send queue 另有 32 MiB 总字节预算。队列超限返回对应的 backpressure error 并关闭连接。

`unsupported_scan_limit=0` 表示按当前已加载候选数扫描。显式上限在仍可能存在未扫描候选时返回 `503 responses_ws_unsupported_scan_limited`；只有已证明全部候选不支持 native WS 时才返回 426。

## 连接生命周期

```text
认证与 token 策略
  → user+token 建连速率与 pending slot/byte lease
  → Upgrade 并以 streaming reader 读取首个 response.create
  → capability / OpenAI Data Residency / exact-model gate
  → 获取 active lease 并建立 native upstream
  → response.create FIFO + 已授权 inject / steer 原帧发送
  → provider terminal 先交付，后结算
  → 释放 lease 并关闭 upstream
```

每个 turn 都执行 RPM 与基于 provider usage 的 TCC 结算。`/v1/responses/input_tokens` 是不同的 HTTP operation，不经过本页的 WebSocket 生命周期。

## Redis 与多实例

Redis 可用时，建连速率和 active lease 跨实例协调。每条 active 连接使用独立 lease id 和过期分数，心跳只续期自己的 member，释放为幂等删除；进程崩溃后该 member 可独立过期，不会把其他连接的 TTL 绑在共享计数器上。Redis 不可用且 `active_lease_redis_fail_open=true` 时，容量退化为进程内计数，多实例总容量可能按实例数放大；需要严格容量上限时设为 false。

Stored Response owner 持久化在数据库中，不依赖 Redis。`store:false` 的跨请求 ephemeral proof 和 prompt-cache affinity 可以使用内存/Redis，丢失时不得放宽 owner 授权或把 unknown ID 路由到任意渠道。

同一连接刚产生的 `store:false` Response ID 由连接本地状态直接证明，不依赖数据库或一小时 ephemeral proof。若 provider transport 在合法 lifecycle terminal 前结束，已有可归属 provider usage 则按 usage Confirm；否则 Cancel 并释放预扣。服务不会为缺失 usage 建立未决结算记录。

## 相关文档

- [Responses WebSocket 架构](/dev/responses-ws-architecture)
- [ADR-0013：要求 Native Responses WebSocket 上游](/adr/0013-require-native-responses-websocket-upstreams)
- [ADR-0014：保持 Native Responses WebSocket turn 语义](/adr/0014-preserve-native-responses-websocket-turn-semantics)
- [Codex 渠道配置](/use/Codex)
