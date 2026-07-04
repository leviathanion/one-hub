# Codex / PI OAuth 请求 Header 画像对照

## 文档状态

- 状态：当前诊断；one-hub 小节已按当前工作区的 Codex Official upstream 实现重新核对。
- 适用范围：Codex / PI 官方 OAuth 请求 header、body metadata、one-hub Codex provider 的 `/v1/responses` HTTP、`/v1/responses/compact` HTTP、`GET /v1/responses` ResponsesWS upstream parity 分析；`/v1/realtime` 兼容路径仅作为边界说明。
- 文档口径：本文是实测/源码画像与差异诊断，不是最终实现 contract；Codex provider 的目标协议边界以 [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md) 为准。

## 目标与边界

本文记录 Codex 官方客户端、PI 客户端在 ChatGPT Codex OAuth 路径下的 Responses HTTP / Responses WebSocket header 画像，并对照 one-hub 当前 Codex provider 中转后的差异。

目标是给 Codex Official upstream 边界提供 parity 依据，并明确 one-hub 当前实现不再按客户端 UA 在 Codex / PI / legacy 行为之间做 smart profile 推断。PI 画像仍用于判断差异，但 one-hub 当前 Codex provider 不实现 `pi_official` profile。

边界：

- 本文只讨论应用层可控 header、body metadata 相关差异。
- `Host`、`Connection`、`Upgrade`、`Sec-WebSocket-*`、`Content-Length`、HTTP/2 pseudo-header、TLS/ALPN、header 顺序和大小写由底层 runtime / transport 决定，one-hub 只要终止请求再用 Go 重新发，就无法做到线缆级完全一致。
- HTTP header 名大小写无语义差异；表格保留源码中常见写法，便于定位代码。
- PI WS 中 `OpenAI-Beta` 的最终 wire 行为需要实测确认：PI `buildWebSocketHeaders()` 先删除 `OpenAI-Beta` / `openai-beta`，随后重新 set `OpenAI-Beta: responses_websockets=2026-02-06`；建连前 `connectWebSocket()` 又通过 `headersToRecord()` 把 `Headers.entries()` 展开为普通对象，并只删除精确键 `OpenAI-Beta`。在常见 runtime 中 `Headers.entries()` 会输出小写 `openai-beta`，因此源码层面可能把 `openai-beta` 传给 WebSocket constructor，最终 wire 是否发送仍取决于 WebSocket runtime。
- Codex 的部分身份字段同时存在于 header compatibility projection 和 body `client_metadata`。二者不是同一层：`session-id` 是 header，`client_metadata.session_id` 是 body 字段；`x-codex-installation-id` 在普通 Responses 请求主要通过 body 发送，但 `/responses/compact` 会额外作为 HTTP header 发送。
- one-hub 当前 Codex channel 禁止使用 `model_headers` 作为 upstream header 注入口；保存校验和运行时 policy 解析都会拒绝非空 `model_headers`。Codex policy 只能通过结构化 `other.codex` 表达。
- 当前 `other.codex` 仅支持 `fedramp`、`residency`、`default_originator`、`trust_client_attestation`、`auto_generate`。`auto_generate` 是显式 opt-in 子对象，默认所有字段都不自动生成；没有 `responses_lite` channel 配置键。

## 源码依据

Codex 是 monorepo，不存在一个能同时代表所有子项目的仓库级版本。本文对 Codex 使用固定 commit 作为主复核口径；版本栏只记录本次 header 画像相关组件的版本来源。尤其要注意：Codex `User-Agent` 中的版本来自 Rust crate 的 `env!("CARGO_PKG_VERSION")`，在该源码快照中继承 `codex-rs/Cargo.toml` 的 `[workspace.package].version`；正式发布包可能由 release 流程改写为发布版本。

| 项目 | 项目地址 | 版本 / 快照 | Commit ID | 参考文件 |
| --- | --- | --- | --- | --- |
| Codex | <https://github.com/openai/codex> | 固定源码快照；相关组件：`@openai/codex` npm shim 为 `0.0.0-dev`，`codex-rs` workspace / `codex-cli` / `codex-login` / `codex-core` / `codex-api` crate 均继承 `0.0.0`；UA 版本来自 `codex-login` 的 `CARGO_PKG_VERSION` | `bdd282f3bbd55df3a869a5438519cd948c134d4d` | [`codex-cli/package.json`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-cli/package.json)<br>[`codex-rs/Cargo.toml`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/Cargo.toml)<br>[`codex-rs/core/src/client.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/core/src/client.rs)<br>[`codex-rs/login/src/auth/default_client.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/login/src/auth/default_client.rs)<br>[`codex-rs/core/src/responses_metadata.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/core/src/responses_metadata.rs)<br>[`codex-rs/codex-api/src/endpoint/responses.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/endpoint/responses.rs)<br>[`codex-rs/codex-api/src/endpoint/responses_websocket.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/endpoint/responses_websocket.rs)<br>[`codex-rs/codex-api/src/requests/headers.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/requests/headers.rs) |
| PI | <https://github.com/earendil-works/pi> | `@earendil-works/pi-ai@0.80.2`；monorepo `0.0.3`；`git describe`: `v0.80.2-70-g5a073885b` | `5a073885b5f23cd6125cda0927cf50acf2bf22fb` | [`packages/ai/src/api/openai-codex-responses.ts`](https://github.com/earendil-works/pi/blob/5a073885b5f23cd6125cda0927cf50acf2bf22fb/packages/ai/src/api/openai-codex-responses.ts)<br>[`packages/ai/test/openai-codex-stream.test.ts`](https://github.com/earendil-works/pi/blob/5a073885b5f23cd6125cda0927cf50acf2bf22fb/packages/ai/test/openai-codex-stream.test.ts) |
| one-hub | <https://github.com/leviathanion/one-hub> | 当前工作区；one-hub 代码结论以本次变更后的工作区为准 | 未提交工作区 | `common/codexpolicy/schema.go`<br>`common/jsonobject/object.go`<br>`common/requestctx/header_snapshot.go`<br>`common/responses/request.go`<br>`common/responsesws/raw_frame.go`<br>`common/responsesws/upstream.go`<br>`providers/base/interface.go`<br>`providers/codex/wire/*`<br>`providers/codex/base.go`<br>`providers/codex/chat.go`<br>`providers/codex/credential_policy.go`<br>`providers/codex/responses.go`<br>`providers/codex/responses_ws_upstream.go`<br>`providers/codex/realtime.go`<br>`relay/responses.go`<br>`relay/responses_ws_open.go`<br>`model/channel_validation.go` |

## Codex 自身 WS

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | `Bearer <Codex OAuth/API token>`，由 auth provider 注入 |  |
| `ChatGPT-Account-ID` | 有 account id 时由 auth provider 注入 |  |
| `X-OpenAI-Fedramp` | FedRAMP 账号时为 `true` |  |
| `originator` | 默认 `codex_cli_rs`；可由前端/环境覆盖 |  |
| `User-Agent` | `<originator>/<version> (<OS> <version>; <arch>) <terminal-info>` |  |
| `OpenAI-Beta` | 固定 `responses_websockets=2026-02-06` |  |
| `session-id` | `responses_metadata.session_id` |  |
| `thread-id` | `responses_metadata.thread_id` |  |
| `x-client-request-id` | `responses_metadata.thread_id` |  |
| `x-codex-window-id` | `responses_metadata.window_id` |  |
| `x-codex-turn-metadata` | 有 turn metadata 时发送，值为 ASCII JSON |  |
| `x-codex-parent-thread-id` | 子线程/派生线程时发送 |  |
| `x-openai-subagent` | subagent 时发送，如 `review`、`compact`、`memory_consolidation`、`collab_spawn` 或自定义 label |  |
| `x-openai-memgen-request` | memory consolidation 时为 `true` |  |
| `x-codex-beta-features` | 启用 beta feature 时发送 |  |
| `x-responsesapi-include-timing-metrics` | 开启 timing metrics 时为 `true` |  |
| `x-oai-attestation` | provider 支持且客户端能生成 attestation 时发送 |  |
| `Host` / `Connection` / `Upgrade` / `Sec-WebSocket-*` | WebSocket/TLS/HTTP 库自动生成 |  |

Codex Responses WS 的 `response.create` frame body 还会携带 `client_metadata`。其主要 Codex-owned keys：

| `client_metadata` key | 生成逻辑 |
| --- | --- |
| `x-codex-installation-id` | `responses_metadata.installation_id` |
| `session_id` | `responses_metadata.session_id`；注意 body 使用 underscore，不是 header 的 `session-id` |
| `thread_id` | `responses_metadata.thread_id` |
| `x-codex-window-id` | `responses_metadata.window_id` |
| `turn_id` | 有 turn id 时发送 |
| `x-openai-subagent` | subagent 时发送 |
| `x-codex-parent-thread-id` | 子线程/派生线程时发送 |
| `x-codex-turn-metadata` | 有 turn metadata 时发送，值为 ASCII JSON |
| `x-codex-turn-state` | WS streaming 请求后续 frame 若已拿到 sticky turn state，则放入 body `client_metadata`；WS handshake header 本身不发送该字段 |
| `ws_request_header_x_openai_internal_codex_responses_lite` | model 使用 responses-lite 时在 WS body metadata 中发送 |
| `ws_request_header_traceparent` | 当前 span 有 W3C traceparent 时由 WS body metadata 携带 |
| `ws_request_header_tracestate` | 当前 span 有 W3C tracestate 时由 WS body metadata 携带 |
| `x-codex-ws-stream-request-start-ms` | 每次 WS request 真正发送前由 Codex 动态 stamp 当前毫秒时间 |

## Codex 自身 HTTP

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | `Bearer <Codex OAuth/API token>`，由 auth provider 注入 |  |
| `ChatGPT-Account-ID` | 有 account id 时发送 |  |
| `X-OpenAI-Fedramp` | FedRAMP 账号时为 `true` |  |
| `originator` | 默认 `codex_cli_rs`；可由前端/环境覆盖 |  |
| `User-Agent` | 同 Codex WS |  |
| `Content-Type` | JSON body 时为 `application/json` |  |
| `Accept` | streaming 为 `text/event-stream` |  |
| `session-id` | `responses_metadata.session_id` |  |
| `thread-id` | `responses_metadata.thread_id` |  |
| `x-client-request-id` | 普通 `/responses` streaming 请求按 `responses_metadata.thread_id` 生成；`/responses/compact` 路径未在当前源码中额外补该字段 | 不应简单推广为所有 Codex HTTP 请求都有 |
| `x-codex-window-id` | `responses_metadata.window_id` |  |
| `x-codex-turn-metadata` | 有 turn metadata 时发送 |  |
| `x-codex-parent-thread-id` | 子线程/派生线程时发送 |  |
| `x-openai-subagent` | subagent 时发送 |  |
| `x-openai-memgen-request` | memory consolidation 时为 `true` |  |
| `x-codex-beta-features` | 启用 beta feature 时发送 |  |
| `x-codex-turn-state` | 后续请求若拿到 sticky turn state 时发送 |  |
| `x-openai-internal-codex-responses-lite` | model 使用 responses-lite 时为 `true` |  |
| `x-oai-attestation` | provider 支持且客户端能生成 attestation 时发送 |  |
| `x-codex-inference-call-id` | rollout trace 开启时发送 |  |
| `x-codex-installation-id` | 仅 `/responses/compact` 额外作为 header 发送；普通 `/responses` 通过 body `client_metadata` 发送 | compact-only，不应简单推广为所有 HTTP 请求的常规 header |
| `x-openai-internal-codex-residency` | residency requirement 为 US 时为 `us` |  |
| `Content-Encoding` | 部分 ChatGPT/Codex 请求可能为 `zstd` |  |
| `Host` / `Content-Length` / transport headers | HTTP 库自动生成 |  |

Codex HTTP `/responses` body 同样包含 `client_metadata`，主要 keys 为 `x-codex-installation-id`、`session_id`、`thread_id`、`x-codex-window-id`、可选 `turn_id`、`x-openai-subagent`、`x-codex-parent-thread-id`、`x-codex-turn-metadata`。HTTP sticky turn state 通过 `x-codex-turn-state` header 发送，不在 body `client_metadata` 中；responses-lite 在 HTTP 上投影为 `x-openai-internal-codex-responses-lite` header。`/responses/compact` 除 body metadata 外，还把 `x-codex-installation-id` 投影为 header。

## PI 自身 WS

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | `Bearer <PI OpenAI Codex OAuth token>` |  |
| `chatgpt-account-id` | 从 JWT claim `https://api.openai.com/auth.chatgpt_account_id` 提取 |  |
| `originator` | 固定 `pi` |  |
| `User-Agent` | Node/Bun 下 `pi (<platform> <release>; <arch>)`；浏览器为 `pi (browser)` |  |
| `OpenAI-Beta` / `openai-beta` | `buildWebSocketHeaders()` 构造为 `responses_websockets=2026-02-06`；`connectWebSocket()` 建连前通过 `headersToRecord()` 展开后只删除精确键 `OpenAI-Beta`，若 `Headers.entries()` 输出小写键则会以 `openai-beta` 传给 WebSocket constructor，实际 wire 是否保留需抓包确认 |  |
| `session-id` | `options.sessionId`；无 sessionId 时使用随机 request id |  |
| `x-client-request-id` | 同 `session-id` |  |
| `Host` / `Connection` / `Upgrade` / `Sec-WebSocket-*` | WebSocket runtime 自动生成 |  |

## PI 自身 HTTP

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | `Bearer <PI OpenAI Codex OAuth token>` |  |
| `chatgpt-account-id` | 从 JWT claim 提取 |  |
| `originator` | 固定 `pi` |  |
| `User-Agent` | Node/Bun 下 `pi (<platform> <release>; <arch>)`；浏览器为 `pi (browser)` |  |
| `OpenAI-Beta` | 固定 `responses=experimental` |  |
| `accept` | 固定 `text/event-stream` |  |
| `content-type` | 固定 `application/json` |  |
| `session-id` | 仅 `options.sessionId` 存在时发送 |  |
| `x-client-request-id` | 仅 `options.sessionId` 存在时发送 |  |
| `Host` / `Content-Length` / transport headers | HTTP / fetch runtime 自动生成 |  |

## one-hub ResponsesWS 对比 Codex

当前 one-hub 的 `GET /v1/responses` Codex upstream 已不再走 `getRealtimeHeaders()` / mutable header bag。provider-facing contract 是 `responsesws.OpenRequest{InboundHeaders, FirstFrame, Principal, ...}`，由 `providers/codex/wire` 在 open 阶段一次性规划 Codex Official WS handshake；`TransportModeResponsesHTTPBridge` 会被 Codex provider 直接拒绝。

| 字段 | 当前 one-hub 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | channel OAuth token：`Bearer <channel access_token>` | 形式一致；token 来源是 one-hub channel credential |
| `ChatGPT-Account-ID` | channel credential 有 AccountID 时发送 | 与 Codex 字段语义一致；值需与 token 账号一致 |
| `X-OpenAI-Fedramp` | `other.codex.fedramp=true` 时发送 `true` | 条件字段已支持；只来自 channel policy |
| `Content-Type` / `Accept` / `Connection` | Codex Official WS planner 不设置 | 与旧文档相反，不再由 `getRequestHeaderBag()` 给 WS 额外加 `Content-Type` |
| `User-Agent` | 下游有效 `User-Agent` 优先；缺失时默认 `codex_cli_rs/2026-06-29 (<goos>; <goarch>) one-hub` | 透传官方 UA 时可接近；默认 UA 仍不是 Codex 发布包的精确 UA |
| `originator` | 下游有效 `originator` 优先；缺失时用 `other.codex.default_originator`，默认 `codex_cli_rs` | 默认已从旧 `codex-tui` smart 推断改为 Codex Official 口径 |
| `OpenAI-Beta` | 固定 `responses_websockets=2026-02-06` | 与 Codex WS 一致 |
| `session-id` | inbound `session-id`，否则 first `response.create.client_metadata.session_id`；缺失时默认不发送，只有 `other.codex.auto_generate.session_id=true` 才生成 UUID | 已支持；不读取 legacy `session_id` / `x-session-id` 作为 upstream header |
| `thread-id` | inbound `thread-id`，否则 first frame `client_metadata.thread_id`；缺失时默认不发送，只有 `other.codex.auto_generate.thread_id=true` 才生成 UUID | 已支持 |
| `x-client-request-id` | inbound `x-client-request-id`；缺失时默认不发送，只有 `other.codex.auto_generate.client_request_id=true` 才按 resolved `thread-id` 或新 UUID 生成 | 已支持 |
| `session_id` / `x-session-id` | Codex Official WS planner 不发送 | 旧 one-hub execution-session 字段已从 ResponsesWS upstream header 中移除 |
| `x-codex-window-id` | inbound header 或 first frame `client_metadata.x-codex-window-id` | 有证据时发送；缺失时不伪造 |
| `x-codex-turn-metadata` | inbound header 或 first frame `client_metadata.x-codex-turn-metadata` | 有证据时发送；metadata 非法时 400 |
| `x-codex-parent-thread-id` | inbound header 或 first frame metadata | 有证据时发送 |
| `x-openai-subagent` | inbound header 或 first frame metadata | 有证据时发送 |
| `x-openai-memgen-request` | inbound header | 有证据时发送 |
| `x-codex-beta-features` | inbound header | 有证据时发送 |
| `x-responsesapi-include-timing-metrics` | inbound header | 有证据时发送 |
| `x-codex-turn-state` | 不放入 WS handshake；first frame metadata 若自带则原样保留 | 与 Codex 固定快照的“sticky state 在 body metadata”一致；不会再把 header 透传到 handshake |
| `x-codex-installation-id` | 不放入 WS handshake；first frame metadata 有则保留；缺失时默认不生成，只有 `other.codex.auto_generate.installation_id=true` 才按 channel/principal/session 生成并补入 frame metadata | Codex 官方客户端使用真实 installation id；one-hub 生成值是 proxy-scoped identity |
| `x-oai-attestation` | 仅 `other.codex.trust_client_attestation=true` 时允许透传；默认收到该 header 会 400 | one-hub 不自行生成 attestation |
| `traceparent` / `tracestate` | 不作为 WS handshake header 发送；`ws_request_header_traceparent` / `ws_request_header_tracestate` 若已在 frame metadata 中则保留 | one-hub 当前不主动从本地 span stamp WS trace metadata |
| `Host` / `Sec-WebSocket-*` | 由 Go WebSocket transport 处理 | 仍不追求线缆级完全一致 |

ResponsesWS frame body 现状：

- `response.create` 以 raw frame map 为序列化基底，未知字段和既有 `client_metadata` 会保留。
- 若 frame metadata 缺 `session_id`、`thread_id`、`x-codex-window-id`、`x-codex-installation-id`，one-hub 只用已经解析出的 resolved identity 补齐可补字段；缺失字段不会默认生成。`session_id`、`thread_id`、`x-codex-installation-id` 分别只有在对应 `other.codex.auto_generate.*` 显式为 `true` 时才会被合成。
- 若 inbound header `x-openai-internal-codex-responses-lite: true` 存在，one-hub 会补 `ws_request_header_x_openai_internal_codex_responses_lite=true`。当前没有 `other.codex.responses_lite` 配置，也没有未接入的内部 policy 分支。
- `x-codex-ws-stream-request-start-ms` 默认不 stamp；只有 `other.codex.auto_generate.ws_stream_request_start_ms=true` 时，才会在每次发送 `response.create` 前写入当前毫秒时间。
- 当前不会把 inbound header 里的 `x-codex-turn-state`、`x-openai-subagent` 等全部镜像到 frame metadata；已有 frame metadata 会保留，缺失则通常只影响 body parity，不影响 handshake parity。

`/v1/realtime` 兼容入口仍保留旧 execution-session header 行为，会在上游 WS header 中使用 `session_id` / `x-session-id` 等字段。该路径不是本文的 Codex Official ResponsesWS path。

## one-hub HTTP 对比 Codex

当前 one-hub 的 `/v1/responses` 和 `/v1/responses/compact` Codex upstream 已切到 `commonresponses.Request` raw envelope contract。typed `OpenAIResponsesRequest` 只作为本地 routing/accounting lens，普通 `/responses` 上游 body 不再由 typed struct 重组。

| 字段 | 当前 one-hub 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | channel OAuth token：`Bearer <channel access_token>` | 形式一致；token 来源是 one-hub channel credential |
| `ChatGPT-Account-ID` | channel credential 有 AccountID 时发送 | 与 Codex 字段语义一致；值需与 token 账号一致 |
| `X-OpenAI-Fedramp` | `other.codex.fedramp=true` 时发送 `true` | 条件字段已支持；只来自 channel policy |
| `x-openai-internal-codex-residency` | `other.codex.residency` 非空时发送 | 条件字段已支持；只来自 channel policy |
| `Content-Type` | `application/json` | 与 Codex HTTP 一致 |
| `Accept` | `/responses` 为 `text/event-stream`；`/responses/compact` 为 `application/json` | 与当前 Codex Official 目标路径一致 |
| `User-Agent` | 下游有效 `User-Agent` 优先；缺失时默认 `codex_cli_rs/2026-06-29 (<goos>; <goarch>) one-hub` | 默认 UA 仍不是 Codex 发布包精确 UA |
| `originator` | 下游有效 `originator` 优先；缺失时用 `other.codex.default_originator`，默认 `codex_cli_rs` | 默认已从旧 smart `codex-tui` / `pi` 推断改为 Codex Official 口径 |
| `Connection` / `Host` | Codex Official planner 不设置 | Go HTTP transport 仍可能写 transport 层字段；应用层不再手工注入 `Keep-Alive` / Host |
| `session-id` | inbound `session-id`，否则 body `client_metadata.session_id`；缺失时默认不发送，只有 `other.codex.auto_generate.session_id=true` 才生成 UUID | 已支持；不读取 legacy `session_id` / `x-session-id` |
| `thread-id` | inbound `thread-id`，否则 body `client_metadata.thread_id`；缺失时默认不发送，只有 `other.codex.auto_generate.thread_id=true` 才生成 UUID | 已支持 |
| `x-client-request-id` | inbound `x-client-request-id`；缺失时默认不发送，只有 `other.codex.auto_generate.client_request_id=true` 才按 resolved `thread-id` 或新 UUID 生成 | 已支持；显式生成时 `/responses/compact` 也会发送该字段，严格对照固定 Codex 快照时可能比实测画像多 |
| `session_id` / `x-session-id` / `Conversation_id` | Codex Official HTTP planner 不发送 | 旧 one-hub legacy identity / prompt-cache projection 已从 Codex Official HTTP upstream 移除 |
| `OpenAI-Beta` | HTTP Codex Official planner 不发送 | 与 Codex HTTP create/compact 目标一致；不同于 PI HTTP |
| `x-codex-window-id` | inbound header 或 body `client_metadata.x-codex-window-id` | 有证据时发送 |
| `x-codex-turn-metadata` | inbound header 或 body metadata | 有证据时发送；metadata 非法时 400 |
| `x-codex-parent-thread-id` | inbound header 或 body metadata | 有证据时发送 |
| `x-openai-subagent` | inbound header 或 body metadata | 有证据时发送 |
| `x-openai-memgen-request` | inbound header | 有证据时发送 |
| `x-codex-beta-features` | inbound header | 有证据时发送 |
| `x-responsesapi-include-timing-metrics` | inbound header | 有证据时发送 |
| `x-codex-turn-state` | inbound HTTP header | 与 Codex HTTP sticky state header 层一致；body metadata 中的同名 key 只参与校验，不会被提升为 header |
| `x-openai-internal-codex-responses-lite` | inbound header | 有证据时发送；当前没有对外的 `other.codex.responses_lite` 配置 |
| `x-oai-attestation` | 仅 `other.codex.trust_client_attestation=true` 时允许透传；默认收到该 header 会 400 | one-hub 不自行生成 attestation |
| `x-codex-inference-call-id` | inbound header | 有证据时发送 |
| `traceparent` / `tracestate` | inbound valid W3C tracing header | HTTP path 可透传；one-hub 当前不在 planner 内主动生成本地 trace header |
| `x-codex-installation-id` | `/responses`：body metadata 优先，其次 inbound header；`/responses/compact`：inbound header 优先，其次 body metadata；缺失时默认不生成，只有 `other.codex.auto_generate.installation_id=true` 才生成 proxy-scoped installation id | 普通 `/responses` 当前会把 resolved installation id 投影为 header；显式生成时也会作为 header 发送，严格对照本文固定 Codex 快照时比“主要通过 body”画像更强 |
| `Content-Encoding` | 不按 Codex zstd 策略生成 | Codex 部分请求可能 zstd；one-hub 仍使用普通 JSON body |

HTTP body 现状：

- 普通 `/responses` 使用 raw JSON object 为上游 body 基底，保留 `client_metadata`、未知字段、未知数值精度；只 patch `model`、`stream`、`store=false`、必要的 `prompt_cache_key`，并在存在 `reasoning` 时补 `include: reasoning.encrypted_content`。
- `/responses/compact` 使用 compact 专用 body，不保留 ordinary `/responses` 的 `client_metadata` 和未知字段；identity 通过 header 投影。
- `client_metadata` 若存在但不是 JSON object 会 400；reserved metadata keys 会按对应 grammar 校验。

## one-hub Codex policy

`other.codex.auto_generate` 是唯一的身份字段自动合成入口，默认等价于空对象：

```json
{
  "codex": {
    "fedramp": true,
    "residency": "us",
    "default_originator": "codex_cli_rs",
    "trust_client_attestation": false,
    "auto_generate": {
      "session_id": true,
      "thread_id": true,
      "client_request_id": true,
      "installation_id": true,
      "ws_stream_request_start_ms": true
    }
  }
}
```

未配置 `auto_generate` 或某个子字段为 `false` 时，对应字段不会由 one-hub 合成；若下游请求已经在 official header 或 `client_metadata` 中提供合法值，则仍按上文优先级透传/投影。`installation_id` 对 `/responses`、`/responses/compact` 和 ResponsesWS 使用同一语义：缺失时默认不生成，显式开启后生成 proxy-scoped installation id。`User-Agent` 和 `originator` 不属于该 opt-in 集合，继续沿用当前逻辑：下游合法值优先，缺失时使用 one-hub/Codex Official 默认值。

启用 `other.codex.auto_generate.installation_id=true` 时必须配置 `codex_identity_secret`。该 secret 是 proxy-scoped installation id 的 HMAC key；缺失时请求会以 channel config error 失败，不回退到 `session_secret`。

Trade-off：默认不合成身份字段会牺牲“空请求也尽量像 Codex 官方客户端”的表面 parity，但换来更干净的协议边界：one-hub 不伪造客户端并未表达的 session/thread/request/installation 语义。需要官方式 completion 的渠道可以显式打开对应 `auto_generate` key，生成行为也因此在 channel policy 中可审计。

## one-hub WS 对比 PI

当前 Codex provider 不实现 `pi_official` profile。PI 客户端如果通过 one-hub Codex channel 访问 `GET /v1/responses`，上游仍按 Codex Official WS dialect 规划。

| 字段 | 当前 one-hub 生成逻辑 | 对 PI 画像的差异 |
| --- | --- | --- |
| `Authorization` | channel OAuth token | 形式一致；token 来源仍是 channel credential |
| `ChatGPT-Account-ID` | channel credential AccountID | 值需与 PI token claim 对齐，否则画像不一致 |
| `originator` | 下游有效 `originator` 可透传；缺失默认 `codex_cli_rs` | PI 默认固定 `pi`；one-hub 不会因 UA 自动切到 PI profile |
| `User-Agent` | 下游有效 UA 可透传；缺失为 Codex Official one-hub UA | PI 默认 UA 不会自动生成 |
| `OpenAI-Beta` | 固定 `responses_websockets=2026-02-06` | 源码层面与 PI 构造值接近；PI runtime 是否 wire 发送仍需实测 |
| `Content-Type` / `Accept` | WS planner 不设置 | 与旧 one-hub 多余 `Content-Type` 不同；也不是 PI runtime 的完整行为模拟 |
| `session-id` | inbound `session-id` 或 frame metadata；缺失时默认不发送，除非显式开启 `auto_generate.session_id` | PI 的 session/request id 只有客户端按 `session-id` 提供时才可一致 |
| `x-client-request-id` | inbound header；缺失时默认不发送，除非显式开启 `auto_generate.client_request_id` | PI 默认与 session/request id 相同；one-hub 默认不是 PI 规则 |
| `session_id` / `x-session-id` | 不发送 | 与 PI 不发送这些 legacy header 一致 |

## one-hub HTTP 对比 PI

当前 `/v1/responses` HTTP 同样只实现 Codex Official dialect，不按 PI 行为自动补 header。

| 字段 | 当前 one-hub 生成逻辑 | 对 PI 画像的差异 |
| --- | --- | --- |
| `Authorization` | channel OAuth token | 形式一致；token 来源仍是 channel credential |
| `ChatGPT-Account-ID` | channel credential AccountID | PI 源码使用小写 `chatgpt-account-id`；HTTP 语义无大小写差异，值仍需一致 |
| `originator` | 下游有效 `originator` 可透传；缺失默认 `codex_cli_rs` | PI 默认固定 `pi`；one-hub 不自动切换 |
| `User-Agent` | 下游有效 UA 可透传；缺失为 Codex Official one-hub UA | PI 默认 UA 不会自动生成 |
| `OpenAI-Beta` | HTTP planner 不发送 | PI HTTP 固定 `responses=experimental`，当前 one-hub 缺失 |
| `Accept` / `Content-Type` | `/responses` streaming 为 `text/event-stream` / `application/json` | stream create 场景与 PI 近似 |
| `Connection` | planner 不设置 | PI fetch/runtime 也不显式设置；Go transport 仍可能管理连接层行为 |
| `session-id` | inbound header 或 body metadata；缺失时默认不发送，除非显式开启 `auto_generate.session_id` | PI 仅有 `options.sessionId` 时发送；one-hub 默认也不会凭空生成 |
| `x-client-request-id` | inbound header；缺失时默认不发送，除非显式开启 `auto_generate.client_request_id` | PI 有 sessionId 时与 sessionId 相同；one-hub 默认不是 PI 规则 |
| `session_id` / `x-session-id` / `Conversation_id` | 不发送 | 与 PI 不发送 legacy identity header 一致 |
| `prompt_cache_key` | raw body 优先；否则可由 one-hub policy 明确 patch | PI 使用 `clampOpenAIPromptCacheKey(options.sessionId)`；当前 one-hub 不实现该 PI 规则 |

## 当前结论与后续取舍

当前仓库已经完成 Codex Official path 的核心修正：

- Codex HTTP / ResponsesWS upstream header 由 `providers/codex/wire` 单一 planner 生成。
- `/v1/responses` 使用 raw envelope 保真，不再丢 `client_metadata` 和未知字段。
- Codex Official path 不再发送 `session_id`、`x-session-id`、`Conversation_id`。
- Codex Official ResponsesWS 不再走 HTTP bridge。
- Codex channel 不再允许 `model_headers` 作为第二个 header 作者。
- `other.codex` policy 现在是显式小 schema；除 `User-Agent` 和 `originator` 沿用“下游有效值优先，缺失则按当前默认值生成/修改”的逻辑外，身份字段默认全量透穿/省略。`session-id`、`thread-id`、`x-client-request-id`、proxy installation id、WS stream start timestamp 只有在 `other.codex.auto_generate.*` 对应字段显式为 `true` 时才自动生成。

仍需明确取舍的差异：

- `pi_official` 没有实现；如果 PI parity 是目标，应做独立 provider/profile，而不是恢复 UA smart fallback。
- `/v1/realtime` 兼容入口仍保留 legacy execution-session header 语义；不要把它和 `GET /v1/responses` Codex Official ResponsesWS 混为一条路径。
- 默认 UA 仍是 one-hub 合成的 Codex-like UA，不是 Codex release 包的精确 UA。
- 普通 `/responses` 当前会把 resolved `x-codex-installation-id` 投影为 HTTP header；若严格追随本文固定 Codex 快照的普通请求画像，需要重新评估是否只保留 body 层。
- `/responses/compact` 当前也会发送 `x-client-request-id`；固定 Codex 快照里 compact 路径未明确额外补该字段。
- responses-lite 当前只有 inbound header 会在真实请求中触发；没有公开 channel policy knob，也没有未接入的内部 policy 分支。
- one-hub 仍不实现 Codex zstd request body、客户端 attestation 生成、本地 W3C trace metadata 注入等更深层 parity。
