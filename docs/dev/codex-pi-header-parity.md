# Codex / PI OAuth 请求 Header 画像对照

## 目标与边界

本文记录 Codex 官方客户端、PI 客户端在 ChatGPT Codex OAuth 路径下的 Responses HTTP / Responses WebSocket header 画像，并对照 one-hub 当前 Codex provider 中转后的差异。

目标是服务后续 header parity 修复：当客户端为 Codex 时，上游请求尽量贴近 Codex 自己 OAuth；当客户端为其他时，上游请求尽量贴近 PI 自己 OAuth。

边界：

- 本文只讨论应用层可控 header、body metadata 相关差异。
- `Host`、`Connection`、`Upgrade`、`Sec-WebSocket-*`、`Content-Length`、HTTP/2 pseudo-header、TLS/ALPN、header 顺序和大小写由底层 runtime / transport 决定，one-hub 只要终止请求再用 Go 重新发，就无法做到线缆级完全一致。
- HTTP header 名大小写无语义差异；表格保留源码中常见写法，便于定位代码。
- PI WS 中 `OpenAI-Beta` 的最终 wire 行为需要实测确认：PI `buildWebSocketHeaders()` 先删除 `OpenAI-Beta` / `openai-beta`，随后重新 set `OpenAI-Beta: responses_websockets=2026-02-06`；建连前 `connectWebSocket()` 又通过 `headersToRecord()` 把 `Headers.entries()` 展开为普通对象，并只删除精确键 `OpenAI-Beta`。在常见 runtime 中 `Headers.entries()` 会输出小写 `openai-beta`，因此源码层面可能把 `openai-beta` 传给 WebSocket constructor，最终 wire 是否发送仍取决于 WebSocket runtime。
- Codex 的部分身份字段同时存在于 header compatibility projection 和 body `client_metadata`。二者不是同一层：`session-id` 是 header，`client_metadata.session_id` 是 body 字段；`x-codex-installation-id` 在普通 Responses 请求主要通过 body 发送，但 `/responses/compact` 会额外作为 HTTP header 发送。
- one-hub 的 `model_headers` 可以静态注入未受保护的业务 header，因此“当前不透传/不生成”表示默认动态路径不能按请求生成这些字段；不排除管理员手工写死 header。静态注入无法表达 Codex/PI 的 per-session / per-thread / per-turn 身份，不应视为官方 parity。

## 源码依据

Codex 是 monorepo，不存在一个能同时代表所有子项目的仓库级版本。本文对 Codex 使用固定 commit 作为主复核口径；版本栏只记录本次 header 画像相关组件的版本来源。尤其要注意：Codex `User-Agent` 中的版本来自 Rust crate 的 `env!("CARGO_PKG_VERSION")`，在该源码快照中继承 `codex-rs/Cargo.toml` 的 `[workspace.package].version`；正式发布包可能由 release 流程改写为发布版本。

| 项目 | 项目地址 | 版本 / 快照 | Commit ID | 参考文件 |
| --- | --- | --- | --- | --- |
| Codex | <https://github.com/openai/codex> | 固定源码快照；相关组件：`@openai/codex` npm shim 为 `0.0.0-dev`，`codex-rs` workspace / `codex-cli` / `codex-login` / `codex-core` / `codex-api` crate 均继承 `0.0.0`；UA 版本来自 `codex-login` 的 `CARGO_PKG_VERSION` | `bdd282f3bbd55df3a869a5438519cd948c134d4d` | [`codex-cli/package.json`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-cli/package.json)<br>[`codex-rs/Cargo.toml`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/Cargo.toml)<br>[`codex-rs/core/src/client.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/core/src/client.rs)<br>[`codex-rs/login/src/auth/default_client.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/login/src/auth/default_client.rs)<br>[`codex-rs/core/src/responses_metadata.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/core/src/responses_metadata.rs)<br>[`codex-rs/codex-api/src/endpoint/responses.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/endpoint/responses.rs)<br>[`codex-rs/codex-api/src/endpoint/responses_websocket.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/endpoint/responses_websocket.rs)<br>[`codex-rs/codex-api/src/requests/headers.rs`](https://github.com/openai/codex/blob/bdd282f3bbd55df3a869a5438519cd948c134d4d/codex-rs/codex-api/src/requests/headers.rs) |
| PI | <https://github.com/earendil-works/pi> | `@earendil-works/pi-ai@0.80.2`；monorepo `0.0.3`；`git describe`: `v0.80.2-70-g5a073885b` | `5a073885b5f23cd6125cda0927cf50acf2bf22fb` | [`packages/ai/src/api/openai-codex-responses.ts`](https://github.com/earendil-works/pi/blob/5a073885b5f23cd6125cda0927cf50acf2bf22fb/packages/ai/src/api/openai-codex-responses.ts)<br>[`packages/ai/test/openai-codex-stream.test.ts`](https://github.com/earendil-works/pi/blob/5a073885b5f23cd6125cda0927cf50acf2bf22fb/packages/ai/test/openai-codex-stream.test.ts) |
| one-hub | <https://github.com/leviathanion/one-hub> | 当前工作副本；本次复核时工作区为 dirty，本文 one-hub 结论以本地文件内容为准，不仅是干净 HEAD | `21fd5b8b00be3d1b1d2ad761628804e8bb6d383e` | `providers/codex/base.go`<br>`providers/codex/chat.go`<br>`providers/codex/realtime.go`<br>`providers/codex/responses.go`<br>`providers/codex/responses_ws_upstream.go`<br>`relay/responses_ws_open.go`<br>`runtime/session/request.go`<br>`types/responses.go` |

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

## one-hub WS 对比 Codex

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | one-hub channel token：`Bearer <channel access_token>` | 形式一致，但 token 来源不同；需同账号 channel 才接近 Codex OAuth 画像 |
| `chatgpt-account-id` | one-hub channel credential 有 AccountID 时发送 | 大小写无语义差异；值需与 Codex 账号一致 |
| `X-OpenAI-Fedramp` | 当前不透传/不生成 | FedRAMP 账号条件字段缺失 |
| `Content-Type` | `getRequestHeaderBag()` 固定 `application/json` | Codex WS handshake 不显式需要该字段，额外字段 |
| `User-Agent` | 下游 allowlist 透传；缺失时 one-hub 默认 `codex-tui/0.135.0 ...` | 透传 Codex 官方 UA 时可一致；未透传时默认值不一致，Codex 当前默认是 `codex_cli_rs/<version> ...` |
| `originator` | 下游透传；缺失时按有效 UA 推断，默认常变成 `codex-tui` | 透传 Codex 官方 originator 时可一致；未透传时默认逻辑不一致，Codex 当前默认 `codex_cli_rs` |
| `OpenAI-Beta` | one-hub WS 固定 `responses_websockets=2026-02-06` | 与 Codex WS 一致 |
| `session_id` | 下游透传或 one-hub 生成 UUID；若仅有 `session-id` / `session_id`，内部 execution session 逻辑会把其用于回填 `x-session-id` | Codex Responses WS handshake 使用 `session-id`，不使用 `session_id`；额外且语义不一致 |
| `x-session-id` | 下游透传或 one-hub 生成 UUID；优先作为 one-hub upstream execution-session 隔离 key | Codex Responses WS 不使用该字段，额外字段 |
| `session-id` | one-hub runtime 可读取该字段作为客户端 session id，但 Codex provider 上游 header 默认不生成/不转发该官方字段 | Codex 常规字段缺失 |
| `thread-id` | 当前不在 one-hub allowlist，通常不发送 | Codex 常规字段缺失 |
| `x-client-request-id` | 仅下游透传 | 透传 Codex 官方值时可一致；未透传时 Codex 自身按 `thread_id` 生成，one-hub 不生成 |
| `x-codex-turn-metadata` | 仅下游透传 | 透传 Codex 官方 turn metadata 时可一致；未透传时 one-hub 不生成，若 body / 下游缺失则缺失 |
| `x-codex-turn-state` | native WS 会从下游透传到 handshake；WS HTTP bridge 主动删除 | Codex 固定快照中 WS sticky state 放在 `client_metadata`，不是 handshake header；one-hub native 透传 handshake 与官方画像不一致，bridge 则不透传 |
| `x-codex-window-id` | 当前不透传/不生成 | Codex 字段缺失 |
| `x-codex-installation-id` | 当前不透传/不生成；WS body 若由下游 frame 自带则 native/bridge body 可保留 | Codex WS 不使用 handshake header；差异主要在 body metadata 是否保真 |
| `x-codex-parent-thread-id` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-openai-subagent` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-openai-memgen-request` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-codex-beta-features` | 下游透传或 realtime override | 透传 Codex 官方值时可一致；未透传时 one-hub 不生成 |
| `x-responsesapi-include-timing-metrics` | 下游透传或 realtime override | 透传 Codex 官方值时可一致；未透传时 one-hub 不生成 |
| `x-oai-attestation` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `Host` | one-hub 手动设置为 upstream host；实际 Go wire Host 可能由 URL 控制 | Codex 由 WS 库自动生成；实现方式不一致 |

路径差异：

- Native Responses WS：上游 handshake 由 `getRealtimeHeaders()` 生成，`x-codex-beta-features`、`x-responsesapi-include-timing-metrics`、`x-codex-turn-state` 等 realtime override 字段可透传；`x-codex-turn-metadata` 在当前 allowlist 中只会经 `getRequestHeaderBag()` 进入，随后 `getRealtimeHeaders()` 不主动覆盖。Codex 固定快照的官方 WS sticky state 在 `response.create.client_metadata`，不是 handshake header；one-hub native 把 `x-codex-turn-state` 放在 handshake 与官方画像不一致。首个/后续 `response.create` frame 以 raw frame 为序列化来源，未知 body 字段和 `client_metadata` 可保留。但 one-hub 只保留下游 frame 已有的 metadata，不会像 Codex 官方客户端一样主动 stamp `x-codex-ws-stream-request-start-ms`，也不会主动补 `ws_request_header_traceparent` / `ws_request_header_tracestate`。
- Responses WS HTTP bridge：上游改走 HTTP `/responses`，`codexResponsesWSBridgeBlockedHeaders` 主动删除 `session_id`、`session-id`、`x-session-id`、`x-codex-turn-metadata`、`x-codex-turn-state`。body 由 raw frame map 转换，通常仍可保留 frame 里的 `client_metadata` 与未知 body 字段。

## one-hub HTTP 对比 Codex

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | one-hub channel token：`Bearer <channel access_token>` | 形式一致，token 来源不同 |
| `chatgpt-account-id` | channel AccountID | 值需与 Codex 账号一致 |
| `X-OpenAI-Fedramp` | 当前不透传/不生成 | FedRAMP 账号条件字段缺失 |
| `Content-Type` | 固定 `application/json` | 与 Codex HTTP 一致 |
| `Accept` | stream 为 `text/event-stream`，非 stream 为 `application/json` | stream 场景一致 |
| `User-Agent` | 透传或 one-hub 默认 Codex TUI UA | 透传 Codex 官方 UA 时可一致；未透传时默认不等于 Codex 当前默认 `codex_cli_rs/...` |
| `originator` | 透传或按 UA 推断 | 透传 Codex 官方 originator 时可一致；未透传时默认不等于 Codex 当前默认 |
| `Connection` | one-hub 默认 `Keep-Alive` | Codex 不显式设置，额外字段 |
| `session_id` | 透传/生成；`PromptCacheKey` 存在时会覆盖成 prompt cache key | Codex HTTP header 用 `session-id`，body metadata 才用 `session_id`；one-hub 把该 header 同时当作 legacy session / conversation identity，额外且语义不一致 |
| `x-session-id` | 透传/生成；若只收到 `session-id` 或 `session_id`，会用于回填 execution session | Codex HTTP 不使用，额外字段 |
| `Conversation_id` | `PromptCacheKey` 非空时设置 | Codex 官方 HTTP 不这样投影，额外字段 |
| `session-id` | one-hub runtime 可读取该字段作为客户端 session id，但 Codex provider 上游 header 默认不生成/不转发该官方字段 | Codex 字段缺失 |
| `thread-id` | 当前通常不发送 | Codex 字段缺失 |
| `x-client-request-id` | 仅下游透传 | 透传 Codex 官方值时可一致；未透传时 Codex 普通 `/responses` 按 `thread_id` 生成，one-hub 不生成 |
| `x-codex-window-id` | 当前不透传/不生成 | Codex 字段缺失 |
| `x-codex-turn-metadata` | 仅下游透传 | 透传 Codex 官方 turn metadata 时可一致；未透传时 one-hub 不生成，结构体 body 还可能丢 `client_metadata` |
| `x-codex-installation-id` | 当前不透传/不生成；`/responses/compact` 也不生成 | Codex compact HTTP header 缺失；普通 HTTP 的同名身份主要应来自 body `client_metadata` |
| `x-codex-parent-thread-id` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-openai-subagent` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-openai-memgen-request` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-codex-beta-features` | 下游透传 | 透传 Codex 官方值时可一致；未透传时 one-hub 不生成 |
| `x-codex-turn-state` | 下游透传 | 透传 Codex 当前 turn state 时可一致；未透传时 one-hub 不生成，Codex 是从 turn state 机制生成 |
| `x-openai-internal-codex-responses-lite` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-oai-attestation` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `x-codex-inference-call-id` | 当前不透传/不生成 | Codex trace 字段缺失 |
| `x-openai-internal-codex-residency` | 当前不透传/不生成 | Codex 条件字段缺失 |
| `Content-Encoding` | 当前不按 Codex zstd 策略生成 | Codex 部分请求可能 `zstd`，逻辑不一致 |
| `client_metadata` body 字段 | `OpenAIResponsesRequest` 无该字段，普通 HTTP marshal 可能丢失 | Codex 重要 body metadata 可能丢失 |

one-hub 普通 HTTP Responses 当前用 typed `OpenAIResponsesRequest` 重组 body，该结构没有 `client_metadata` 字段，因此普通 HTTP `/responses` 和 `/responses/compact` 都存在 body metadata 丢失风险。需要保留的 Codex-owned keys 至少包括：

| `client_metadata` key | Codex 语义 |
| --- | --- |
| `x-codex-installation-id` | installation identity |
| `session_id` | body 层 session identity，合法且不同于 header `session-id` |
| `thread_id` | thread identity |
| `x-codex-window-id` | window identity |
| `turn_id` | turn identity，可选 |
| `x-openai-subagent` | subagent label，可选 |
| `x-codex-parent-thread-id` | parent thread，可选 |
| `x-codex-turn-metadata` | Codex turn metadata ASCII JSON，可选 |
| `ws_request_header_traceparent` / `ws_request_header_tracestate` | WS body trace metadata，可选；普通 HTTP 不使用 |

WS native/bridge 路径若追求 body parity，还应注意 `x-codex-ws-stream-request-start-ms`：它是 Codex 官方 WS 发送前动态写入的 timing metadata。one-hub 当前 raw-frame 策略能保留下游自带值，但不会自行生成；普通 HTTP `/responses` 不使用这个 key。WS 的 `ws_request_header_traceparent` / `ws_request_header_tracestate` 同样属于 body metadata parity 范畴，不应误当成握手 header。

## one-hub WS 对比 PI

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | one-hub channel token：`Bearer <channel access_token>` | 形式一致，token 来源不同 |
| `chatgpt-account-id` | channel AccountID | 值需与 PI token claim 一致 |
| `originator` | 透传或按 UA 推断 | 透传 `pi` 时可一致；未透传时 PI 固定 `pi`，one-hub 默认可能为 `codex-tui` |
| `User-Agent` | 透传或默认 Codex TUI UA | 透传 PI UA 时可一致；未透传时 PI 应为 `pi (<platform> <release>; <arch>)` 或 `pi (browser)` |
| `OpenAI-Beta` | one-hub 固定 `responses_websockets=2026-02-06` | PI 源码构造该值，但 `connectWebSocket()` 可能以小写 `openai-beta` 传入 WebSocket constructor；若目标 runtime 最终不发该 header，则 one-hub 额外 |
| `Content-Type` | one-hub 固定 `application/json` | PI WS 建连前删除 `content-type`，one-hub 额外 |
| `Accept` | one-hub WS 会删除 | 与 PI WS 删除 accept 的意图一致 |
| `session_id` | one-hub 透传/生成 UUID；`session-id` / `session_id` 也可能被内部逻辑用于回填 `x-session-id` | PI 不使用，额外字段 |
| `x-session-id` | one-hub 透传/生成 UUID，作为 execution-session 隔离 key | PI 不使用，额外字段 |
| `session-id` | one-hub runtime 可读取该字段，但 Codex provider 上游 header 默认不生成/不转发该官方字段 | PI WS 使用该字段，缺失 |
| `x-client-request-id` | 仅下游透传 | 透传 PI session/request id 时可一致；未透传时 PI 总是设置为 session/request id，one-hub 不生成 |
| `Host` | one-hub 手动设置/Go transport 处理 | PI 由 WebSocket runtime 处理，实现方式不一致 |

## one-hub HTTP 对比 PI

| 字段 | 生成逻辑 | 不一致信息 |
| --- | --- | --- |
| `Authorization` | one-hub channel token：`Bearer <channel access_token>` | 形式一致，token 来源不同 |
| `chatgpt-account-id` | channel AccountID | 值需与 PI token claim 一致 |
| `originator` | 透传或按 UA 推断 | 透传 `pi` 时可一致；未透传时 PI 固定 `pi`，one-hub 默认可能为 `codex-tui` |
| `User-Agent` | 透传或默认 Codex TUI UA | 透传 PI UA 时可一致；未透传时 PI 应为 `pi (<platform> <release>; <arch>)` 或 `pi (browser)` |
| `OpenAI-Beta` | 仅下游透传；one-hub 不为 PI 自动补 `responses=experimental` | 透传 `responses=experimental` 时可一致；未透传时 PI HTTP 固定 `responses=experimental`，one-hub 缺失/逻辑不一致 |
| `Accept` / `accept` | stream 为 `text/event-stream` | 与 PI HTTP 一致 |
| `Content-Type` / `content-type` | `application/json` | 与 PI HTTP 一致 |
| `Connection` | one-hub 默认 `Keep-Alive` | PI fetch 不显式设置，额外字段 |
| `session_id` | one-hub 透传/生成；`PromptCacheKey` 存在时覆盖 | PI 明确不发 `session_id`，额外且语义不一致 |
| `x-session-id` | one-hub 透传/生成；`session-id` / `session_id` 输入可能被回填到该字段 | PI 不发，额外字段 |
| `Conversation_id` | `PromptCacheKey` 非空时设置 | PI 不发，额外字段 |
| `session-id` | one-hub runtime 可读取该字段，但 Codex provider 上游 header 默认不生成/不转发该官方字段 | PI 有 `sessionId` 时发送；缺失 |
| `x-client-request-id` | 仅下游透传 | 透传 PI session id 时可一致；未透传时 PI 有 `sessionId` 时发送，one-hub 不生成 |
| `PromptCacheKey` / body `prompt_cache_key` | one-hub 可能由策略生成或复用 | PI 使用 `clampOpenAIPromptCacheKey(options.sessionId)`；逻辑不一致 |
| `Host` | one-hub 手动设置/Go transport 处理 | PI fetch/runtime 自动处理，实现方式不一致 |

## 修复方向

为避免把 Codex 和 PI 的画像继续混在一套 smart fallback 中，后续应把上游 header 构造拆成显式 profile：

- `codex_official`：使用 `session-id` / `thread-id`，保留 Codex metadata/window/subagent/attestation/lite/inference 头，不补 `session_id` / `x-session-id`。
- `pi_official`：固定 `originator=pi`，UA 按 PI 规则，HTTP 加 `OpenAI-Beta: responses=experimental`，WS 按 PI 行为处理 `OpenAI-Beta` / `Accept` / `Content-Type`，session 只用 `session-id`。
- `legacy_onehub`：保留当前 `session_id` / `x-session-id` 兼容逻辑，避免影响已有非目标客户端。

同时，普通 HTTP Responses 路径需要解决 raw body 保真问题，至少保留 `client_metadata` 与未知字段；否则即使 header 对齐，Codex 官方请求画像仍可能在 body metadata 层面偏离。`/responses/compact` 还需要单独补齐 Codex compact-only 的 `x-codex-installation-id` header。WS HTTP bridge 的 header blocklist 是有意隔离客户端 session / turn sticky state 的设计，若后续追求 Codex parity，需要明确取舍：获得 bridge stateless 隔离，牺牲 native Codex turn metadata/state header 画像；或放开这些 header，获得更高 parity，但承担跨 transport sticky state 语义混用风险。
