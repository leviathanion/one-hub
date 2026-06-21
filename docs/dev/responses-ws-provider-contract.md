# ResponsesWS Provider Contract ADR

## Decision

ResponsesWS 的长期 provider contract 选择 `providers/base.ResponsesWSProvider.OpenResponsesWS(ctx, model, options)`，返回 `common/responsesws.Upstream`。

`responsesws.Upstream` 是 ResponsesWS 专用 provider-facing contract，不是 `/v1/realtime` 的 session contract。它只表达一条 downstream ResponsesWS 连接对应的一条 upstream Responses transport，能力边界为：

- `SendClientWithResult(ctx, responsesws.SendRequest)`：发送 downstream client frame，并返回显式 `responsesws.ResponsesWSTransportSendResult`；这是 actor accounting 的 send evidence input，不是 provider 已接受请求的证明。
- `Recv(ctx)`：接收 provider-originated frame、usage、provider close 或 proxy-local transport evidence。
- `Abort(reason)`：终止当前 ResponsesWS upstream transport。

`responsesws.SendRequest` 显式携带 `AttemptID`、`responsesws.Frame` 和 `DefaultPreviousResponseID`；`context.Context` 只承载 cancel、deadline 和 trace/log metadata。新 ResponsesWS provider contract 必须让 `responsesws.Upstream` 实现 `responsesws.TransportSendCapable`，relay actor 不再通过 hidden optional capability 或 legacy error mapper 推导 ResponsesWS accounting。

`Recv(ctx)` 中的 provider close 只表示 upstream peer close evidence：native helper 必须基于 `wsconn.CloseKindPeerClose` 才能生成 `ProviderClose`。本地 write/read/backpressure/abort/graceful shutdown 等 close lifecycle 必须作为 proxy-local transport evidence 返回，不能进入 provider evidence 或 terminal accounting。

relay-facing 状态机仍由 ResponsesWS actor 持有：`response.create` admission、quota reserve、RPM、affinity、provider evidence、terminal finalization、conservative floor settlement 和 downstream close flow 都不进入 provider adapter 或 transport helper。actor 执行账务时必须先经过 ResponsesWS settlement core；真实扣费金额来自 `SettlementDecision.ExpectedFinalQuota`，provider adapter 和 transport helper 不能根据 usage、close reason 或 send detail 自行 rollback/finalize。

## Rationale

第一性原理上，ResponsesWS 需要的是每条下游 WebSocket 连接独立打开 provider Responses transport；它不需要 Codex realtime execution session 的跨请求 resume、binding、revocation，也不需要 `/v1/realtime` 的 long-lived session ownership。

选择 `OpenResponsesWS` 的原因：

- OpenAI/Codex native ResponsesWS path 已经迁到 `common/responsesws` native helper，provider opener 只负责 URL/header/policy/dial，helper 只负责 transport mechanics。
- relay ResponsesWS open path 已经只断言 `ResponsesWSProvider`，不再通过 `OpenRealtimeSessionWithOptions(...ResponsesWS...)` 打开 provider。
- `runtime/session.RealtimeSession` 保留给 `/v1/realtime`，避免把 realtime resume/binding/revocation 语义泄露到 ResponsesWS。
- `responsesws.Upstream` 能直接表达 ResponsesWS 的实际 provider-facing surface，避免长期维持两套含义相近但状态语义不同的入口。

Trade-off：ResponsesWS 不再保留 `runtime/session` 兼容窗口；`runtime/session` 只服务 `/v1/realtime`。收益是 ResponsesWS 不再依赖 realtime session 的 turn state 或 execution session manager，provider adapter 只能产出 evidence，actor 仍是唯一 accounting owner。

## Compatibility Window

兼容期内允许 provider 继续实现 `OpenRealtimeSessionWithOptions`，但仅服务 `/v1/realtime` 和已有 realtime managed-session 行为。新 provider 不应通过 `OpenRealtimeSessionWithOptions(...TransportModeResponsesWS...)` 暴露 ResponsesWS。

删除条件：

- OpenAI/Codex native ResponsesWS path 均通过 `OpenResponsesWS`。
- HTTP bridge path 通过 `OpenResponsesWS` 或同一 ResponsesWS provider contract 接入。
- relay/config 测试覆盖 normalized `responses_ws_transport` 不进入 legacy realtime open path。

Owner：ResponsesWS relay/provider transport 迁移由 ResponsesWS actor 与 provider transport 边界维护者共同负责；`runtime/session.RealtimeSession` owner 仍负责 `/v1/realtime` execution session 语义。

## Provider Configuration Contract

所有渠道的 `channel.Other` 非空时都是 JSON object；旧 plain string 不再作为运行时配置读取。provider 可以读取已经解析过的 JSON 字段，但不能把 `Other` 当裸字符串解释。持久化入口和 DB migration 会做一次 canonicalization：OpenAI / Custom 历史 plain string 没有明确运行时语义，会保存为 `{"vendor_extra":{"legacy_other":"..."}}`，只保留审计信息；Azure/Gemini/Xunfei/Azure Speech/Ali/Vertex 这类有明确历史字段语义的 plain string 才做可无损映射；JSON-like malformed 值保持原样，由 runtime validation fail-closed。Codex 历史别名 `{"websocket_mode":"required"}` 会落库为 canonical `{"websocket_mode":"force"}`。Trade-off：迁移比直接拒绝旧数据更复杂，但避免把旧版本“OpenAI/Custom Other 对运行时无影响”的语义误解释成 provider 参数；同时不在运行时增加永久 fallback。

混合版本发布时必须先部署后端并确认 `migrateLegacyChannelOtherJSON` 完成，再发布依赖 strict JSON object contract 的新前端资源。Trade-off：发布顺序多一个约束，但避免新前端在旧 plain string `Other` 数据上隐藏或误展示配置；后端保持 fail-closed，不恢复运行时 legacy string fallback。

classic Azure (`ChannelTypeAzure`) 的 `channel.Other` 是 strict JSON object：

```json
{
  "api_version": "2024-10-01-preview",
  "responses_ws_transport": "native",
  "self_hosted": false,
  "responses_ws_self_hosted": true,
  "extra": {},
  "vendor_extra": {}
}
```

`api_version` 必须是非空 string；`responses_ws_transport` 可选且只允许 `native` / `http_bridge`；`self_hosted` 可选且只允许 boolean，只用于 Azure `/v1/realtime` 私有/本地上游 URL policy；`responses_ws_self_hosted` 可选且只允许 boolean，只用于 ResponsesWS native/HTTP bridge upstream URL policy；`extra` / `vendor_extra` 是 opaque namespace，运行时不解释，批量更新 `api_version` 时必须原样保留。除这些字段外的未知顶层 key 仍 fail-fast，避免拼写错误静默保存。旧 plain api-version 字符串是有意不兼容的配置格式收敛，URL 构造时不得 fallback 读取旧格式。升级迁移只允许把可无损识别的历史 provider `Other` 字符串一次性转换为 JSON object；无法无损判断的旧值保持原样并由运行时校验 fail-closed。Trade-off：解析/更新逻辑要保留 raw JSON object，复杂度略高于 struct round-trip；收益是 strict runtime contract 不放松，同时 opaque vendor data 不会在管理操作中丢失。

Azure ResponsesWS native 与 Azure HTTP Responses/bridge URL 不是同一条规则：

- ResponsesWS native：classic Azure 与 Azure V1 都使用官方 resource-level endpoint `/openai/v1/responses`，deployment name 通过 `response.create.model` 传给上游。
- classic Azure HTTP Responses 与 HTTP bridge：使用 resource-level HTTP endpoint `/openai/responses?api-version={api_version}`，compact 使用 `/openai/responses/compact?api-version={api_version}`；`model` 只进入 request body / custom parameter merge，不参与 URL deployment routing。
- Azure V1 HTTP Responses：使用 `/openai/v1/responses`，不追加 classic `api-version`；Azure V1 的 `Other` 只接受公共 runtime 字段与 `extra` / `vendor_extra`，顶层 `api_version` 会保存失败，避免被误以为生效。

classic Azure 与 Azure V1 都不得拼 `/openai/deployments/{deployment}/responses?api-version=...`。classic Azure 仍保留 `api_version` 必填约束，是为了让同一渠道在非 WS Azure HTTP relay mode 下保持单一 strict JSON schema。

第三方 OpenAI-compatible channel 的 native ResponsesWS capability 必须显式声明：

```json
{
  "responses_ws_native": true
}
```

`responses_ws_native` 只允许 boolean。`Config.Responses` 非空仅说明存在 HTTP Responses endpoint，不能自动推断 native WebSocket support；显式 `responses_ws_transport=http_bridge` 仍以 `Config.Responses` 作为 HTTP bridge 的必要条件。Trade-off：兼容 provider 多一个 opt-in 字段，但避免把所有 HTTP Responses-compatible provider 都误判为 native WS-compatible provider。

`model_headers` 不能覆盖 provider credential/routing headers、hop-by-hop headers 或 WebSocket handshake headers。受保护 credential family 包括 `Authorization`、`api-key`、`x-api-key`、`x-goog-api-key`；provider 自己设置的认证头仍由 provider 逻辑负责。Trade-off：少数依赖 `model_headers` 注入 `x-api-key` 的自定义网关需要改用非 credential 名称（例如 `X-Gateway-Auth`），收益是 channel 级 header 不会覆盖 provider credential 或造成双发凭据。

Realtime 与 ResponsesWS 的 self-hosted URL policy 必须按协议分域：`self_hosted` 只允许 `/v1/realtime` 使用私有/本地自建 websocket 上游；`responses_ws_self_hosted` 只允许 ResponsesWS 使用私有/本地自建 upstream。二者不互为兼容别名。Trade-off：旧配置可能需要显式补齐另一个 key，但可以防止 ResponsesWS 配置扩大 Realtime 攻击面，或 Realtime 配置意外放开 ResponsesWS。

Web 后台的 OpenAI-compatible 模型拉取模式会临时把请求 `type` 改成 OpenAI，但不能继承原渠道 provider-specific `Other` 字段，例如 Azure `api_version`、Gemini/Xunfei `api_version`、Azure Speech `region`、Vertex `project_id`。该模式只保留公共 runtime 字段（如 `responses_ws_transport`、`responses_ws_native`、`self_hosted`、`responses_ws_self_hosted`、`extra`、`vendor_extra`）。Trade-off：管理员在模型选择器里无法借 OpenAI 模式测试 provider-specific 扩展字段，但避免一个 Azure/Gemini 渠道因为原 `Other` JSON 被严格 OpenAI-compatible 校验拒绝而无法拉取自定义 base_url 模型。

Azure Speech 的 `base_url` 与 `Other.region` 都是一等配置入口，后端允许二选一；Web 表单必须展示 `base_url` 输入框，不能只在校验文案里提示。Trade-off：表单比只填 region 多一个可选输入，但与 API/runtime contract 保持一致，支持 region 无法表达的自定义 endpoint。

ResponsesWS HTTP bridge 的最终 HTTP request URL 也是 ResponsesWS upstream policy 的一部分，不是普通 `/v1/responses` REST relay 的宽松 URL。默认只允许 `https` 且 host 不得是 loopback/private/link-local/metadata；显式 `responses_ws_self_hosted=true` 后允许 `http` 与 private/local host，但 metadata host 与 metadata 解析结果仍然硬拒绝。使用显式 proxy 时仍必须先做本地 DNS fail-closed 校验；不用 proxy 时还必须把实际 HTTP dial pin 到校验得到的 IP。Trade-off：proxy 侧 split DNS/私网访问必须显式 self-hosted 且本进程也能解析该 host，牺牲一部分代理灵活性，换取携带 provider 凭据的 bridge 请求不绕过 SSRF 边界。

`OpenResponsesWS(ctx, model, options)` 的 `ctx` 是 upstream open / bridge base context 的唯一来源；`responsesws.OpenOptions` 不再携带第二个 context。Trade-off：调用方需要在函数参数上传递 context，不能把 context 偷藏在 options 里，但可以避免 retry/open worker 出现两个生命周期源导致 cancel、timeout 和测试替身语义分叉。

HTTP bridge 只产出 typed stream/open/cancel/error events；synthetic cancel 只是 downstream ACK，不是 provider terminal。actor control lane 必须把 `response.cancel` 绑定到发起取消时的 attempt，bridge 在锁内校验 target attempt；迟到 cancel 与当前 active/opening attempt 不匹配时只返回 stale no-op，不能跨 turn 关闭 stream。active stream 匹配时，bridge 接收到 `response.cancel` 就必须先关闭 HTTP stream，不能因为 recv queue 背压让 provider stream 继续生成 token 或占用资源。若 synthetic cancel 不能立即入队，bridge 仍关闭 active stream，并在队列恢复后按 `synthetic_bridge` cancel ACK、`bridge_stream_eof` 的顺序补投事件；但如果 pump 已经解析出 provider terminal，则 provider terminal 优先进入 actor，synthetic cancel fallback 不得覆盖 terminal usage、response id 或 continuation evidence。matching bridge EOF/expected close 后的 quota 结算、active turn 清理、provider terminal race 处理仍由 actor 串行完成。本地 `response.cancel` 触发的 HTTP request context cancel、closed body、closed network connection 等读错误属于 expected close，应在 bridge helper 内归一为 cancel-matching stream EOF；普通 read error 仍是 `bridge_stream_error`。Trade-off：背压场景下 cancel ACK 可能延迟，甚至在 session 已关闭时丢失，但资源释放优先；actor 仍通过有序事件观察本地取消意图，不由 bridge helper 做 quota/finalize，同时保留 provider terminal wins 的 exactly-once 裁决边界。

Web 后台 Azure api_version 批量搜索按 `Other` JSON object 语义解析过滤，而不是依赖 compact/pretty JSON 文本格式；无效 Azure `Other` 会按计数写 warning，避免静默跳过完全不可见。Trade-off：该筛选在跨数据库实现中需要先取出候选 Azure 渠道再分页，极大数据量下成本高于单个 SQL `LIKE`；收益是 batch UI 不会因为编辑表单 pretty-print JSON 而漏掉合法 Azure 渠道，且旧 plain string / malformed JSON 不会被误认为符合 strict JSON contract。

`response.cancel` 的状态判断属于 relay actor：opening/setup 阶段必须取消 setup/open worker 并拒绝 adopt 迟到 open result；no active turn / idle / already terminal 时是 proxy-local no-op，不得转发给 native provider，也不得产生 quota/RPM/affinity side effect。

ResponsesWS active attempt watchdog 属于 relay actor。provider adapter / HTTP bridge helper 只能上报 provider activity evidence，不能拥有 turn timeout、quota finalize 或 affinity side effect。active attempt 超时默认关闭当前 downstream session 并 abort upstream，而不是把同一 upstream 恢复为 idle；Trade-off 是牺牲单连接内恢复，换取迟到 provider terminal 不会污染下一次 turn。

## Non-goals

- 不把 `responsesws.Upstream` 扩展成 `/v1/realtime` session。
- 不把 Codex execution session binding/revocation 引入 ResponsesWS。
- 不让 native helper 或 HTTP bridge helper 调用 `TurnObserver.AdmitTurn`、`RollbackTurnAdmission` 或 `FinalizeTurn`。
- 不提供 native WS 到 HTTP bridge 的 automatic fallback；bridge 只能由显式 `responses_ws_transport=http_bridge` 启用。
