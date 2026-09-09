---
title: "GPT-5.6 发布手册"
layout: doc
outline: deep
lastUpdated: true
---

# GPT-5.6 发布手册

## 文档状态

- 状态：临时发布手册。
- 更新日期：2026-09-05。
- 发布方式：停机更新，不提供新旧 transport 并行或废弃配置的回滚兼容。
- 验收边界：本地测试不替代生产配置、数据库结构和生产 smoke 核验。
- 退出条件：生产发布完成后归档本文；可复用的用户配置继续由[价格更新](/use/prices_update)和[Responses WebSocket](/use/responses-ws)维护。

## 发布前条件

1. 数据库已具备当前版本要求的 `response_owners` 和 typed `rate_rules` 字段；按[价格更新](/use/prices_update)配置并核验四个 GPT-5.6 Price 行。仓库不提供运行时价格种子。
2. readiness 不报告 owner schema 或 Price 目录错误。
3. operation 支持由 adapter 统一提供，模型可用性独立由渠道模型配置表达；未知兼容端点只有原生 Responses WebSocket 需要管理员显式开启尝试。
4. 从 `channel.other` 移除废弃的 `capabilities` 和 `responses_ws_transport`；保留实际需要的 `responses_ws_native` 与 `responses_ws_self_hosted`，不维护镜像配置。
5. 发布记录附有本版本的全量 Go、race、vet、frontend test/build 结果及生产数据库 smoke 结果。

## 停机更新与验收

1. 停止新流量并备份数据库，再更新代码、数据库结构和价格目录；验收前不恢复流量。
2. 验证 HTTP Chat、`store:false` Responses、compact、input_tokens 的正常与拒绝路径。
3. 开放 Stored Response create，再验证 retrieve、input-items、delete/tombstone 和跨用户 404 等价性。
4. 只对 native/exact-model 渠道开放 Responses WebSocket，并排除 OpenAI Data Residency 区域端点。
5. 最后开放 GPT-5.6 flex/fast（priority 兼容值）/long-context 价格，并核对 actual tier、272000/272001、cache read/write、Web Search 和 Image Generation 费用样本。

请求能够透传不代表能力已经开放。普通用户的 background、Stored Chat、Conversations、saved prompt、独立资源引用、OpenAI Data Residency 区域端点和 WS 到 HTTP bridge 必须在 provider work 前稳定拒绝。

## 生产 smoke

### 认证与扣费

- 未认证 `/v1` 请求在路由与容量 admission 前失败。
- 普通 HTTP 使用统一额度准入与结算；Work Action claim 后不自动重试或换渠道。`/v1/responses/input_tokens` 只计认证、RPM、审计，不扣生成 quota。
- 使用 default/flex/fast/priority/unknown tier 与 272000/272001 input token 样本核对最终 quota；以 provider 回显的实际 tier 为结算证据。
- cache write 只应用 1.25 倍一次；cache read 使用有效 input 价的 0.1 倍。
- 使用 GPT Image 2 的 low/medium/high、合法尺寸和 partial image 样本核对 output-token 公式；模型、quality 或 size 省略/未知时必须命中系统目录保守上界，失败 image call 不扣费。

### Stored Response

- `store` 省略或为 true 时，创建后的 owner 包含正确的 `UserId`、token、channel 和 30+7 天期限。
- owner 提交失败时，JSON body 或首个带 Response ID 的 SSE/WS event 不得交付。
- owner channel 禁用或 capability 丢失时返回 503，不 fallback。
- owner miss、跨用户、tombstone、过期对普通用户表现一致；只有管理员显式 pin 才能尝试 unknown ID。
- DELETE provider 成功后先写 tombstone，再向客户端交付成功。

### Native Responses WebSocket

- HTTP-only 渠道在 WS 能力检查时返回 426，且没有 provider HTTP create；Codex 旧 bridge 配置在管理写入时直接拒绝。
- 同一连接的多个 create 按 FIFO 完成；每个 turn 使用原始 model，连接不换渠道、不 rewrite model。
- `response.inject` 只在当前 turn 的 `multi_agent.enabled=true` 时可用并原样转发；terminal 前已经接受的 inject 必须等 `response.inject.created/failed` 回执到齐后才能推进下一 turn。
- terminal 后 inject acknowledgement 超时会关闭停滞连接；不得合成第二个 provider terminal，也不得用代理错误覆盖已经交付的 terminal。
- 只接受 completed/failed/incomplete terminal；缺 response ID/sequence、重复或倒序 sequence、Realtime terminal 均 fail closed。
- terminal 已交付后注入 settlement failure，客户端仍只能看到 provider terminal。
- 客户端断开时关闭 upstream，不发送 `response.cancel`；provider close/timeout 不合成 completed。

## 观察与告警

重点观察：

- owner 写入失败、owner channel unavailable、tombstone/expiry cleanup 失败；
- capability reject 按 operation/channel/scope 的分布；
- unknown billing tier、缺失或冲突 usage、组件不收费诊断、结算失败；
- Responses WebSocket malformed terminal、out-of-order sequence、ambiguous write、queue/deferred buffer overflow、abnormal close；
- prompt-cache affinity hit/miss 与 Redis fallback；
- HTTP retry 导致的重复成本或工具副作用信号。

Redis 故障时，Stored Response owner correctness 不受影响；Responses WebSocket 容量和 ephemeral/soft affinity 会按配置 fail open 或退化到本机。需要严格跨实例容量时关闭 `active_lease_redis_fail_open`。

## 故障处理

- 数据库结构或 readiness 异常时保持停机，不能假设旧二进制可直接读取新版本数据。
- owner 异常时，关闭 store 省略/true、Stored lifecycle 与 Responses WebSocket，不降级为普通 affinity。
- tier/long-context 计价异常时，关闭 GPT-5.6 对应价格能力，不回落到未校验的基础价。
- Native Responses WebSocket 生命周期异常时，只关闭 Responses WebSocket；HTTP Chat/Responses 不启用 WS bridge。
- Prompt-cache backend 异常时允许 soft affinity miss，不得放宽 owner 校验。

需要恢复旧版本时，由管理员在停机状态下决定是否恢复与其匹配的备份，并评估备份后数据的损失；不能依靠保留废弃字段恢复旧 transport。

## 完成与归档

发布完成需要同时满足：

1. 支持操作具备 wire、授权、路由、生命周期和扣费的生产边界证据。
2. 不支持能力均在 provider work 前 fail closed。
3. 数据库结构、跨用户安全和生产 smoke 通过。
4. 全量 Go、race、vet、frontend test/build 通过，证据附在发布单。
5. 已接受偏差在 ADR、日志/指标和测试中可见。

历史审计的最终处置见 [OpenAI GPT-5.6 契约审计归档](/archive/openai-gpt-5.6-contract-audit)。
