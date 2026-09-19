---
title: "Responses 请求重试边界"
layout: doc
outline: deep
lastUpdated: true
---

# Responses 请求重试边界

## 文档状态

- 状态：当前实现。
- 适用范围：HTTP Responses create 的 submission 边界、Native ResponsesWS 的 open／turn，以及 `/v1/realtime` 共用的建连重试门禁。
- 文档口径：原 Responses attempt replay 方案已删除；本文记录 application no-replay 契约及建连候选选择的授权条件。
- 关联代码：`relay/main.go`、`relay/common.go`、`relay/responses_ws_open.go`、`relay/realtime.go`、`types/common.go`。

## 当前结论

HTTP 与 WebSocket 都不再拥有 application request replay actor。provider work 前可以继续做能力筛选和握手候选选择；Billing Attempt claim submission 后只允许一次 Application Submission。

### HTTP Responses create

普通 HTTP create 在调用 provider 前完成选路、请求可表示性和 owner 检查。进入 provider send 后，无论返回 HTTP error、普通 5xx、decode failure、timeout 还是 transport 歧义，都不换渠道、不重放。body 可重放不等于 provider work 可重放；收费另按 provider usage Confirm/Cancel。

### Native Responses WebSocket

Responses WS 没有 request-level replay：

1. native upstream 建立前，可以在既有 open 候选预算内跳过不支持的渠道；握手失败是否允许换候选，必须通过下述完整重试门禁；
2. 任何 `response.create` 一旦进入 upstream write，就不换渠道、不重放；
3. write 前失败仍结束本次 turn，不把 submission 权恢复给另一渠道；
4. create write 结果不确定且没有已关联的 provider 事件时关闭连接；已有事件可证明执行归属时，沿用该执行观察，不重新发送。inject/steer 的首次歧义发送结果停止连接。有 provider usage 才 Confirm，否则 Cancel；
5. 泛化 provider `error` 不结束当前执行；明确关联的 create 拒绝才结束该准入，connection fatal / workflow stop 关闭连接。原始错误与无关联诊断均保留，任何路径都不在另一渠道重放；
6. connection 内 FIFO 只是 turn 排队，不是 replay。

生命周期与交付边界见[Responses 透明转发与计费边界设计](./responses-transparent-relay-design.md)，不改变本文的 no-replay 契约。

删除 replay 后，不再需要 cross-channel replay command、zero-charge replay proof、candidate rebuild、replay metrics 或 pending rejection 重放分支。

## 错误标记与建连重试门禁

`types.OpenAIErrorWithStatusCode` 的以下字段是内部错误元数据，不输出到客户端 JSON。它们是独立标记，不构成一个严格的三态状态机；未设置标记也不能作为相反事实的证明。

| 字段 | 含义与边界 |
| --- | --- |
| `UpstreamNotAttempted` | provider 声明本次目标请求尚未尝试。WS open 失败时指尚未提交应用请求，不表示上游没看见握手，也不证明 OAuth 刷新等辅助操作没有副作用。false 不能反推请求已发送。 |
| `ProviderOpenRetrySafe` | provider 明确允许在当前 open 失败后选择另一候选；必须把凭据刷新等 open 阶段副作用纳入判断。false 表示没有该授权，不能从状态码或 `UpstreamNotAttempted` 推导为 true。 |
| `UpstreamAccepted` | 已有上游接受／执行证据，禁止据此在另一渠道重放。 |
| `UpstreamAmbiguous` | 上游执行结果不确定，禁止把不确定性当作未执行。 |
| `LocalError` | 本地错误分类；它不是“可以安全重试”的同义词。 |

ResponsesWS 和 realtime 的 provider open 错误，只有满足以下条件才能进入后续选路判断（`providerOpenCanRetry`）：

```text
ProviderOpenRetrySafe
&& UpstreamNotAttempted
&& !UpstreamAccepted
&& !UpstreamAmbiguous
```

随后还必须通过 `shouldRetry`，并满足入口的候选预算等约束。`shouldRetry` 先检查 workflow stop、请求取消和显式渠道 pin；这些条件均未阻断时，`UpstreamNotAttempted=true` 才使该函数返回 true。否则 `LocalError=true` 返回 false，其他错误再按状态码等既有规则判断。因此 `shouldRetry` 不是最终重试授权，也不能概括为“只要未尝试就换渠道”。不支持渠道的能力筛选是独立分支，不授予已经提交的请求重放权。

HTTP create 还受外层 `done`／submission 边界约束：`RelayHandler` 在 `ClaimSubmission` 成功后、调用 provider 前设置 `done=true`。即使后续 provider 错误带有 `UpstreamNotAttempted`，外层也不重新分配这次 Application Submission。WS open 候选选择同样不能绕过 turn 已提交后的 no-replay 规则。

### Codex open 错误的当前处置

| 错误来源 | `UpstreamNotAttempted` | `ProviderOpenRetrySafe` | ResponsesWS／realtime 建连是否换渠道 |
| --- | --- | --- | --- |
| 客户端身份校验或渠道配置错误 | false | false | 否；返回对应本地错误，配置问题需管理员处理 |
| 普通 token 获取失败，例如缺少 access token | true | false | 否；未获 open 重试授权 |
| OAuth 刷新结果歧义或需要重新授权 | false | false | 否；保留 credential fence 的失败语义 |
| WS 握手失败 | true | 取建连前保存的凭据副作用判断 | 仅标记为 true、完整门禁通过且候选预算允许时可以换候选 |

Codex 在建连前通过 `codexOpenMayMutateCredentials` 检查 refresh token 或待持久化凭据；存在这些情况时，不给握手失败授予 `ProviderOpenRetrySafe`。这解释了为什么“尚未提交推理请求”不足以证明整个 open 流程可以换渠道重试。具体状态码和错误映射以 [Codex 错误语义](./codex-official-upstream-architecture.md#错误语义)为准。

## 为什么统一为 no-replay

代理无法从一般 HTTP/WS transport 事实证明 provider 没有观察请求、产生费用、创建资源或触发工具。把重试限制在 provider work 前，使 submission owner 与计费 owner 使用同一边界，也避免为少量可用性收益维护第二套 replay reducer。若未来某个 operation 具备明确 provider 幂等契约，应单独记录可重放条件、幂等键作用域与收费影响。

相关长期决策见 [ADR-0019](../adr/0019-do-not-replay-ambiguous-http-creates.md) 和 [ADR-0013](../adr/0013-require-native-responses-websocket-upstreams.md)。
