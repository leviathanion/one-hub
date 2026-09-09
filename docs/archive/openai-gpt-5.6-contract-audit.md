---
title: "OpenAI GPT-5.6 契约审计归档"
layout: doc
outline: deep
lastUpdated: true
---

# OpenAI GPT-5.6 契约审计归档

## 文档状态

- 状态：历史诊断。
- 审计基线：`main@1f4e6697`。
- 初始审计日期：2026-07-21；最终处置日期：2026-08-24。
- 用途：保留 finding 编号、最终处置和已接受偏差的追踪关系。
- 非用途：本文不描述当前实现，也不作为新增实现的 contract。

2026-08-26 后续整改已用当前 ADR-0019 撤销 G56-018 的 accepted deviation：HTTP create 的 ambiguous transport 结果不再自动重试。本归档下方矩阵保留 2026-08-24 当时的处置历史。

当前行为分别以仓库根目录的 `CONTEXT.md`、[Responses WebSocket 架构](/dev/responses-ws-architecture)、[基于下游 Usage 的 TCC 计费](/dev/usage-confirmed-tcc-billing-architecture)、[Responses 请求重试边界](/dev/responses-ws-attempt-replay-architecture)、ADR、用户配置和代码测试为准。

## 审计结论

原审计共记录 G56-001～G56-066。整改最终没有继续扩展封闭的 OpenAI DTO、observe-only 账务、可丢失资源 affinity 或 Responses WebSocket HTTP bridge，而是收敛为以下边界：

- exact-wire 路径以原始 wire 为事实源，typed projection 只读取代理拥有的策略字段；
- operation support 由 adapter 提供，模型配置只参与普通模型准入和映射；
- 无法忠实表示的输入在 provider work 前 fail closed；
- Stored Response 使用持久 owner 和 delivery barrier，不以 affinity 代替授权；
- Responses WebSocket 只使用 native upstream，并保持 per-turn model、FIFO、inject acknowledgement 和 provider terminal 语义；
- 条件价格由显式 Price Policy 和 provider terminal evidence 决定；
- 保留既有 Legacy Quota Settlement 及其已接受偏差，不宣称财务级 exactly-once。

## 最终处置矩阵

| Findings | 最终处置 | 当前语义所有者 |
| --- | --- | --- |
| G56-001、G56-028、G56-047 | 实现修复 | cache write/read 与 usage presence 测试 |
| G56-002、G56-033、G56-038、G56-057 | 实现修复 | raw response/error/header/union 边界 |
| G56-003、G56-043、G56-051、G56-052、G56-054、G56-059、G56-060、G56-062 | 实现修复 | Native Responses WebSocket FIFO、inject、terminal、liveness、per-turn contract |
| G56-004、G56-009、G56-012、G56-015 | 实现修复 | 模型目录、tokenizer、long-context 与 actual tier 定价 |
| G56-005、G56-008、G56-011、G56-014、G56-039、G56-040 | 实现修复 | raw 请求字段、beta/safety、cache 模板与显式 false |
| G56-006、G56-007、G56-050 | 实现修复或 adapter gate | Chat/Responses exact-wire 与 representability |
| G56-010 | 实现修复 | image/PDF detail 保真与 admission 估算 |
| G56-013、G56-020 | 实现修复 | compact/input_tokens 独立 operation |
| G56-016 | 能力拒绝 | background 在 provider work 前失败 |
| G56-017 | 能力拒绝 | 普通 Batch 关闭；管理员 pin 保留 |
| G56-018 | 接受偏差 | ADR-0019：replayable HTTP create 保留历史重试 |
| G56-019 | 能力拒绝 | ADR-0020：OpenAI Data Residency 区域端点关闭 |
| G56-021、G56-035、G56-044、G56-055 | 删除错误能力 | 删除 Responses WebSocket HTTP bridge |
| G56-022、G56-023、G56-024、G56-030、G56-031、G56-041 | 实现修复并限制支持面 | raw local/inline 形态保真；独立资源和 hosted container 拒绝 |
| G56-025、G56-032、G56-049 | 能力拒绝 | Conversation、Stored Chat、saved prompt 关闭 |
| G56-026 | 实现修复 | durable owner、delivery barrier、37 天保留与 tombstone |
| G56-027、G56-034、G56-064 | 实现修复 | Image/Web Search/Chat Search 系统工具费 |
| G56-029、G56-058 | 接受偏差 | ADR-0012：保留历史 reservation 与高余额 fast path |
| G56-036、G56-037、G56-042、G56-045、G56-046 | 实现修复；账务部分保留偏差 | 流终态、usage chunk、heartbeat、取消传播 |
| G56-048 | 实现修复 | Chat/Responses soft Prompt Cache Affinity |
| G56-053、G56-056 | 实现修复 | 保留真实 status/error 与最后 provider response |
| G56-061、G56-063 | 接受偏差 | ADR-0012：不建设 durable unary settlement/outbox |
| G56-065、G56-066 | 实现修复 | 管理员 raw query 与 Azure v1 URL capability |

“能力拒绝”表示代理不再接受自己无法履行完整 wire、授权、路由、生命周期或扣费契约的请求；它不是对应上游能力已经实现。

## 勘误

- 原 G56-006 关于“GPT-5.6 Chat function tools 只兼容有效 reasoning effort=none”的前提没有官方依据，由此推导的本地拒绝和健康检查改动作废。reasoning 与 tools 参数由真实上游校验。
- 原价格表使用了 2026-07-30 调价前数值；当前用户配置必须以[价格更新](/use/prices_update)的核验日期和发布当天官方页面为准。
- Fast/Priority 的请求与回显名称存在兼容语义；未知请求值透传上游，未知回显 tier 使用基础价并记录 `billing_tier_unknown`，不由代理伪造上游参数校验。

## 已接受偏差

| 偏差 | Findings | 当前边界 | 升级触发条件 |
| --- | --- | --- | --- |
| replayable HTTP create 的歧义传输失败可重试 | G56-018 | 下游未提交且 body 可重放；可能重复生成、工具副作用和上游成本 | 需要强幂等或外部工具副作用不可接受 |
| 保留历史小额预扣和高余额 fast path | G56-029、G56-058 | 最终差额可能扣款失败或少扣，不建设足额 reservation | 坏账成为重要业务指标 |
| unary settlement 不建设 durable identity/outbox | G56-061、G56-063 | 进程崩溃、Redis/DB 边界仍可能漏扣或重复扣 | 账务要求 exactly-once 或需要审计追偿 |
| 完整流缺 usage 时保留既有估算结算 | G56-042 的账务部分 | 截断流仍视为中继失败；只有已确认协议终态但缺 usage 时才估算 | provider 稳定返回 usage，或账务要求只认权威证据 |
| Native Responses WebSocket 关闭只短暂等待 send result | 本次整改裁决 | 超时后仍未返回时按歧义发送保留预扣；极端迟到的 `NotAttempted` 可能形成小幅多收 | 监控证明该偏差不可继续忽略 |

## 验证边界

本地整改验收覆盖正常路径、未知字段、不可表示输入、上游错误、Stored Response ownership、Native Responses WebSocket 生命周期和歧义执行后的无未授权重试。真实生产 migration、实际 OpenAI 账单和生产 smoke 不由历史审计证明，仍按 [GPT-5.6 发布手册](/deployment/gpt-5.6-rollout)执行。
