---
title: "Responses 请求重试边界"
layout: doc
outline: deep
lastUpdated: true
---

# Responses 请求重试边界

## 文档状态

- 状态：当前实现。
- 说明：原 Responses attempt replay 方案已删除；本文记录当前统一的 application no-replay 边界。

## 当前结论

HTTP 与 WebSocket 都不再拥有 application request replay actor。provider work 前可以继续做能力筛选和握手候选选择；Billing Attempt claim submission 后只允许一次 Application Submission。

### HTTP Responses create

普通 HTTP create 在调用 provider 前完成选路、请求可表示性和 owner 检查。进入 provider send 后，无论返回 HTTP error、普通 5xx、decode failure、timeout 还是 transport 歧义，都不换渠道、不重放。body 可重放不等于 provider work 可重放；收费另按 provider usage Confirm/Cancel。

### Native Responses WebSocket

Responses WS 没有 request-level replay：

1. native upstream 建立前，可以在既有 open 候选预算内跳过不支持或握手失败的候选；
2. 任何 `response.create` 一旦进入 upstream write，就不换渠道、不重放；
3. write 前失败仍结束本次 turn，不把 submission 权恢复给另一渠道；
4. create write 结果不确定且没有已关联的 provider 事件时关闭连接；已有事件可证明执行归属时，沿用该执行观察，不重新发送。inject/steer 的首次歧义发送结果停止连接。有 provider usage 才 Confirm，否则 Cancel；
5. 泛化 provider `error` 不结束当前执行；明确关联的 create 拒绝才结束该准入，connection fatal / workflow stop 关闭连接。原始错误与无关联诊断均保留，任何路径都不在另一渠道重放；
6. connection 内 FIFO 只是 turn 排队，不是 replay。

生命周期与交付边界见[Responses 透明转发与计费边界设计](./responses-transparent-relay-design.md)，不改变本文的 no-replay 契约。

删除 replay 后，不再需要 cross-channel replay command、zero-charge replay proof、candidate rebuild、replay metrics 或 pending rejection 重放分支。

## 为什么统一为 no-replay

代理无法从一般 HTTP/WS transport 事实证明 provider 没有观察请求、产生费用、创建资源或触发工具。把重试限制在 provider work 前，使 submission owner 与计费 owner 使用同一边界，也避免为少量可用性收益维护第二套 replay reducer。若未来某个 operation 具备明确 provider 幂等契约，应单独记录可重放条件、幂等键作用域与收费影响。

相关长期决策见 [ADR-0019](../adr/0019-do-not-replay-ambiguous-http-creates.md) 和 [ADR-0013](../adr/0013-require-native-responses-websocket-upstreams.md)。
