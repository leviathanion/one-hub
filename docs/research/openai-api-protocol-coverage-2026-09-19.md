# OpenAI 与兼容渠道 API 协议覆盖审计

初始审计及官方协议复核日期：2026-09-19；文档修订日期：2026-09-20。原审计代码基线：`b986504166b94e4bfdf53ec48740a7dcf94985c3`；以下当前状态已同步本次实施工作树，未提交版本不是该基线的原始状态。主范围是当前仓库的 OpenAI/兼容公共入口、relay 准入与 provider；本次另补 Claude/Gemini 原生入口的范围检查及 Gemini 错派问题。对照 OpenAI Docs 和对应厂商官方协议；本文记录代理功能覆盖和仍存在的限制；本地回归不等于真实上游支持验证。源码路径以仓库根目录为起点；保留的行号仅用于原审计基线定位。功能取舍、模块改造与交付步骤见 [OpenAI 协议透明透传扩展实现方案](../dev/openai-transparent-relay-implementation-plan.md)。

结论：本轮已补齐 A0/A1/A2/A3、W、Files/Uploads/Conversations、后台 Responses、Stored Chat 与历史音频的本地实现。原工具 schema 误拒、Gemini 错派、JSON 图片编辑映射、音频终态截流和 Stored Responses 墓碑门禁已修复。首批 Batch 已实现并通过定向/race；Realtime sideband/并行 out-of-band、声音管理与托管资源等专项仍未实现，不能据此宣称全部 OpenAI API 已覆盖。

“管理员透传”“普通用户可用”“渠道启用”“真实上游支持”继续分别记录。[机器可读账本](./protocol-inventory-2026-09-19.json) 保留 62 个稳定家族/能力条目、12 个子操作，不是 bug 数、端点数或穷尽证明。Claude/Gemini 是扩展范围，其他原生供应商尚未逐项核查。

代理只维护认证、资源归属/固定渠道、安全、真实本地容量和可信用量的一次结算。上游参数合法性、资源可用性、声线切换及业务先后由客户端/上游处理；计费观察不足不阻断原始交付。主动保留的权限和产品支持限制仍计入覆盖限制。

## 1. 当前基础能力与新增支持

| API / 传输 | 本地实现与剩余限制 |
| --- | --- |
| Chat Completions、旧 Completions | JSON/SSE；原生 OpenAI 方言保留完整 SSE framing 和扩展事件。Chat voice 支持联合值，但已知自定义 voice.id 仍受未实现声音 owner 限制 |
| Responses | JSON/SSE、compact、input_tokens、原生 WS 并行 stream_id；HTTP background/cancel/retrieve 恢复使用同一持久 Task。未知事件及观察歧义不构成交付门禁 |
| Stored Responses | retrieve/delete/input_items 与 continuation 固定 owner 渠道；墓碑留存期间仍授权访问，上游决定是否存在 |
| Images | generations/edits 的 JSON/SSE 与 variations；JSON edits 映射只 patch model，multipart 保留原始体；流式能力按 adapter 声明 |
| Audio | Speech 二进制/SSE；转录 JSON/verbose_json/diarized_json/SSE；translations。done/error 后仍读至真实传输结束；text/srt/vtt 付费门控保留 |
| Files / Uploads | 普通用户创建、读取、删除、内容下载及分片/完成/取消，真实 ID 交付前登记 owner；共享账号顶层列表不开放 |
| Conversations | 根资源及父级 items 操作、Responses conversation 引用；用户与渠道归属验证，不镜像会话状态 |
| Stored Chat | store=true 创建与 get/update/delete/messages；普通用户顶层 list 仍不开放 |
| 历史 Chat 音频 | JSON/SSE 真实 audio ID 登记 owner 后立即交付，后续 messages[].audio.id 固定渠道；不等待 expires_at，不据期限拒绝已授权引用 |
| Embeddings / Moderations | 原有接口保留；联合输入、扩展字段和开放结果不因本轮改动封闭 |
| Realtime | 原有 model 新会话 WS；session.update 与 response.create 声音引用均检查共享资源 policy。call_id attach、并行 out-of-band 仍缺失 |
| Models | list/retrieve 是代理的可用模型视图，不是上游账号目录完整镜像 |

入口与当前源码：路由（`router/relay-router.go`）、资源 HTTP（`relay/resource_http.go`）、后台 Responses（`relay/responses_background.go`）、图片交付（`relay/image_response.go`）、历史音频（`relay/chat_audio_owner.go`）。

本地“已有实现”仍受渠道 endpoint、账号、模型、代理授权/计费支持范围影响，不保证所有兼容上游均支持对应操作。验证范围见第 8 节。

## 2. 已有接口的限制及本轮修复结果

| 条目 | 当前状态 | 证据 / 官方依据 |
| --- | --- | --- |
| O01 Responses WS lanes | 已实现有界多 work 观察；关联歧义可能少计费，不暂停原帧。每连接 64 个候选及 32 MiB 请求原文预算，不是上游业务并发限制 | 观察实现（`relay/responses_ws_observation.go`）、回归（`relay/responses_ws_observation_test.go`）；[WebSocket mode](https://developers.openai.com/api/docs/guides/websocket-mode) |
| O03–O05 后台、取消、恢复 | 已接持久 Task；创建、查询、取消、恢复和轮询不重复收费，恢复请求原样转发，不重新 POST | 生命周期（`relay/responses_background_lifecycle.go`）、回归（`relay/responses_background_test.go`）；[Background mode](https://developers.openai.com/api/docs/guides/background) |
| O08 Chat Search | web_search_options 及识别出的 search 模型仍受付费支持门控；Responses Web Search 另有已实现路径 | capability（`providers/openai/capability.go`） |
| O09/O10 JSON edits 与图片 SSE | JSON 模型映射与原生图片流已实现；跨协议 adapter 仍需真实可表示性支持 | images（`providers/openai/image_response.go`）；[Image generation](https://developers.openai.com/api/docs/guides/image-generation) |
| O11 text/srt/vtt | 付费路径仍要求可信用量；这是产品支持限制，不是技术上无法透传 | 转录能力（`providers/openai/capability.go`）；[Speech to text](https://developers.openai.com/api/docs/guides/speech-to-text) |
| O12 保存 prompt | 仍拒绝账号 prompt 引用，需内联 instructions/input；不新建弃用产品生命周期 | 准入（`relay/capability_gate.go`）；[弃用计划](https://developers.openai.com/api/docs/deprecations) |
| O47 Realtime 并行 out-of-band | 单 inflight 限制仍存在；不代表单个串行 conversation:none 完全不可用 | Realtime 实现（`providers/openai/realtime_session.go`）；[Realtime client events](https://developers.openai.com/api/reference/resources/realtime/client-events) |

WS 对唯一可证明的普通 create 请求级拒绝只释放该候选，不永久放弃同 lane 后续用量观察；无候选的空闲错误不污染后续请求。多个候选或控制命令来源不明仍保守放弃受影响观察，已收尾 ID 的有界去重避免迟到回执消费新候选。

本地 `/responses/:id/resume` 拒绝占位不算官方缺失端点；官方恢复使用 retrieve 的查询。Responses image_generation 工具与 Images 原生 SSE 分属不同能力。

### 2.1 工具 schema 与真实引用

原扫描器将 `input[].tool_search_output.tools[].parameters.properties.file_id` 当文件引用；本轮已改为按协议语义位置提取。Chat、HTTP/WS Responses 的初始/最终有效请求、injection、steering 与 JSON Images 共用资源边界，函数 schema、业务数据及无关未知字段不触发同名键黑名单。语义回归（`common/responses/resource_semantics_test.go`）、[Tool search](https://developers.openai.com/api/docs/guides/tools-tool-search)

`function_call_output.output` 若为协议内容数组，其中 `input_file` / `input_image` 的文件引用同样授权；普通字符串和业务对象仍不按同名属性递归扫描。初始准入与 provider 最终有效请求均已补对应回归。

已授权 file/conversation/chat_audio 引用决定固定渠道；跨用户、未知 owner、跨渠道冲突仍在发送前拒绝。Vector Store、container、Skill 等未实施家族仍明确受限，这与资源当前是否删除/过期的上游业务判断分开。

### 2.2 声音与历史音频的支持边界

| 消费位置 | 当前支持 |
| --- | --- |
| Speech voice.id | 已接共享 policy；声音 owner/管理未实现，已知自定义引用仍拒绝，内置字符串透传 |
| Realtime session.update.session.audio.output.voice.id | 最终设置帧发送前执行同一授权边界，不检查声线业务状态 |
| Realtime response.create.response.audio.output.voice.id | 首次或后续 create 的覆盖值同样检查，不依赖之前是否更新 session |
| Chat audio.voice | 联合类型投影已修复；不能由支持对象语法推断自定义声音可用 |
| Chat messages[].audio.id | 已识别并使用真实输出 owner 授权；上游判断句柄是否仍有效 |

媒体授权回归（`providers/openai/media_resource_policy_test.go`）、历史音频回归（`relay/chat_audio_owner_test.go`）。真实 audio ID 首次交付仍有必要 owner 屏障，expires_at 只作可选观察，不积压后续音频帧、不成为后续引用前置条件；不保存音频正文或新增生成 Task。归属元数据本地清理后缺少授权事实仍不能放行未知 ID。

官方依据：[Speech](https://developers.openai.com/api/reference/resources/audio/subresources/speech/methods/create)、[Custom voices](https://developers.openai.com/api/docs/guides/custom-voices)、[Realtime](https://developers.openai.com/api/reference/resources/realtime/client-events)、[Chat](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create)。上述本地边界修复不是上游越权联调证明。

后台 Responses 的完整 JSON 用量只提取一次，工具调用次数不再重复累加。后台 Responses/Batch 在首次 Task 关闭明确提交后尽力补齐消费日志、渠道用量和用户请求次数，不再次修改余额；重复收尾不补写，提交确认丢失或提交后崩溃可能漏投影。

### 2.3 原生 SSE 与墓碑的修复状态

C05 已从“终态依赖交付限制”改为已修复：音频 done/error 只用于观察，之后安全事件继续交付；干净 EOF 不因缺少本地已知终态补造 error，done 也不掩盖后续真实读取失败。音频实现（`relay/audio_sse.go`）、回归（`relay/audio_sse_test.go`）。兼容 OpenAI 方言的 Chat/Completion 同步改为完整原生 framing；其他跨协议 adapter 不因此自动成为 exact-wire。兼容 SSE 回归（`providers/openai/native_sse_test.go`）

Stored Responses 的 GET/DELETE、previous_response_id 与 WS injection 持久 owner 回退已去除 active/tombstone 业务门禁。真实用户、渠道和本地授权留存继续检查；model 创建/绑定冲突约束不因此删除。HTTP 回归（`relay/responses_background_test.go`）、WS 回归（`relay/responses_ws_observation_test.go`）。资源授权失效与上游资源可能不可用是不同事实。

## 3. 资源型能力与管理员透传

| API 家族 | 普通用户当前状态 | 仍保留的覆盖限制 |
| --- | --- | --- |
| Files | create/get/delete/content 与引用已实现 | 顶层账号 list 仍仅管理员指定渠道 raw |
| Uploads | create/parts/complete/cancel 已实现 | 上游配额、过期与阶段由上游判断；有界请求容量仍生效 |
| Conversations | 资源/items 操作与引用已实现 | 普通顶层 list 不开放，不合并多渠道账号目录 |
| Stored Chat | store 创建及 get/update/delete/messages 已实现 | 普通顶层 list 不开放 |
| Batch | create/get/cancel、单 Task 结算与已知派生结果 owner 已实现 | 七类 endpoint，不含 Videos；账号 list、首次未完整观察的派生资源 Range/压缩及未知资源扇出仍受限 |
| Fine-tuning | 管理员指定渠道 raw | 普通用户完整生命周期未实现 |
| Assistants / Threads / Runs | 管理员指定渠道 raw | 官方已关闭，不新建普通用户生命周期 |
| Vector Stores / File Search | 管理员指定渠道 raw | 普通用户资源授权及存储/检索收费未闭合 |
| Containers / hosted shell / Skills | 未实现普通用户入口及引用 | 派生资源与独立费用专项后置 |

Custom 已能按显式启用的资源 endpoint 进行管理员 raw，缺失/关闭配置不自动启用。Files/Uploads/Conversations 等普通入口独立使用 owner；普通用户 owner miss 不回退管理员 raw。Raw 的存在仍不证明有任务结算或用户隔离。endpoint 目录（`common/providerendpoint/endpoints.go`）、资源入口回归（`relay/resource_http_test.go`）

共享账号的顶层 list 没有 one-hub 用户作用域，不能通过仅过滤一页伪造透明分页。已授权 DELETE 原样发上游，不因活跃 Task、依赖关系或保住计费证据拒绝；本地记录删除观察，不模拟上游 404。

Batch 的结果 File owner 不等于 JSONL 内部 Response、Stored Chat、音频和工具资源的 owner。内部资源只在真实协议产出位置观察，并结合 custom_id 关联；不能递归认领业务 id 或把未知参数做成封闭白名单。计费证据缺失只影响对应收费，不能串账、改写 JSONL 或为每 item 建第二套账务。[Batch API](https://developers.openai.com/api/docs/guides/batch)

已知结果 File 的派生归属暂未观察完成时，Batch 保留原 Task 与槽位，在既有 48 小时窗口内只读重试；持久最小证据可跨重启恢复，重复扫描不重复累加。客户端完成资源观察或窗口到期后收尾；仅缺 usage、或上游终态没有结果 File 不额外延后，不重发 Batch、不另建关闭后状态机。

结果 File 的完整观察事实仅在所有已知派生 owner 屏障通过且读至真实 EOF 后成立。此后已授权重读及 Range/压缩不依赖已清理的 Task/子 owner，也不重新认领或延长它们；首次未完整观察仍执行资源屏障并保留交付模式限制。Batch 根 owner 原子保存最多两个既有 output/error File ID，子 File 删除留存结束不使根 GET 提前拒绝；陌生新 ID 仍需真实归属或预留证明。回归：`relay/batch_retention_test.go`。

## 4. 完全没有入口或未覆盖的新协议家族

| API / 传输家族 | 具体缺口 |
| --- | --- |
| Realtime sideband | 同一路径 `GET /v1/realtime?call_id=...` 未实现；现有入口要求 model，上游 URL 只处理模型，不能表示附着已有 call |
| Realtime WebRTC / SIP | client_secrets、calls、accept/reject/hangup/refer 等建连与控制入口缺失；旧 sessions/transcription_sessions HTTP 入口也没有 |
| Realtime Translation | 独立 `/realtime/translations` WS、client_secrets、calls 入口缺失 |
| GPT-Live | `/live/sessions` 及会话控制、音频连接入口缺失；不能与已有 Realtime WS 混算 |
| Videos / Sora | `/videos` 及扩展操作无路由；官方 API 将于 2026-09-24 关闭，不默认新建。Kling 不是该协议，兼容厂商是否继续服务须另证 |
| 自定义声音管理 | `/audio/voices`、`/audio/voice_consents`、`GET /audio/consent_phrases` 无路由；speech 能传结构化 voice 不等于具备创建声音能力 |
| Agents API / ChatKit | agents、sessions、环境、artifacts 等及 chatkit sessions/threads 无路由；支持 Responses 内的 multi_agent 不等于实现 Agents API |
| Evals | evals、runs、output_items 无路由 |
| 平台管理与事件 | organization/project、上游 usage/costs、webhook_endpoints/event_types 等无路由；本地用量统计不是这些上游 API 的实现 |
| Webhook 接收与交付 | 未找到 OpenAI 回调接收、验签、反重放/去重及 owner 关联的完整实现；与 webhook 管理接口分开，Task 轮询不代表支持回调 |
| Secure MCP Tunnel | 未找到 `/v1/tunnel/*` 长轮询与结果回传入口；Responses 远端 MCP tool 透传不代表实现 tunnel-client |
| 渠道认证/部署 | 未识别 OpenAI workload federation 或 mTLS 专用 adapter；已有 Codex OAuth 不等于这些能力，未检查部署侧外置设施 |
| 其他专用资源 | vaults、content_provenance_checks、safety/cases 无路由；仅 safety/alerts 的 GET 有管理员指定渠道透传 |

完整入口以 `router/relay-router.go:27` 为准。这里的“无路由”不承诺一律返回 404：独立前端配置可能影响 NoRoute 行为。

官方依据：[Realtime](https://developers.openai.com/api/docs/guides/realtime)、[Realtime Translation](https://developers.openai.com/api/docs/guides/realtime-translation)、[GPT-Live](https://developers.openai.com/api/docs/guides/live)、[Video generation](https://developers.openai.com/api/docs/guides/video-generation)、[Audio API](https://developers.openai.com/api/reference/resources/audio)、[Agents API](https://developers.openai.com/api/docs/guides/agents-api/configuration)、[ChatKit](https://developers.openai.com/api/docs/guides/chatkit)、[API Reference](https://developers.openai.com/api/reference)。

Realtime attach 的代码证据为 强制 model 的入口（`relay/realtime.go:183`） 与 URL 构造（`providers/openai/realtime_session.go:222`）。官方 `call_id` 可来自创建回执的 Location 或 SIP 事件；attach 与新建 model session 是不同 operation。[官方 sideband](https://developers.openai.com/api/docs/guides/voice-server-controls)

新增独立维度的官方依据：[Secure MCP Tunnel](https://developers.openai.com/api/docs/guides/secure-mcp-tunnels)、[Webhook](https://developers.openai.com/api/docs/guides/webhooks)、[Workload federation](https://developers.openai.com/api/docs/guides/workload-identity-federation)、[mTLS](https://developers.openai.com/api/docs/guides/mutual-tls)。它们是覆盖清单条目，不自动成为普通用户推理代理的建设范围。

独立 Realtime transcription 的完整兼容性需要另测：本地强制 `?model=`，且不会传播客户端 `intent` query；现有代码已经处理独立转录 usage 与 GA 音频设置。当前官方指南使用 `session.type=transcription`，因此本次不把整套独立转录武断归为“完全未实现”。本地入口（`relay/realtime.go:183`）、URL 构造（`providers/openai/realtime_session.go:222`）、[官方转录指南](https://developers.openai.com/api/docs/guides/realtime-transcription)

## 5. OpenAI 与兼容渠道的差异

| 项目 | 官方 OpenAI 根端点 | OpenAI 兼容 / Custom |
| --- | --- | --- |
| Responses create/compact/input_tokens | 默认具备 adapter 支持 | 有对应 endpoint 时具备原生路径；上游是否接受仍由上游决定 |
| Stored Responses | 默认声明完整生命周期 | 需要 `other.responses_stored_lifecycle=true`；未启用时普通 create 需注意默认 store 语义，可用 `store:false` |
| Responses WebSocket | 默认声明支持 | 需要 `other.responses_ws_native=true`；私网/明文目标另有现存安全要求 |
| Responses→Chat | 非核心依赖 | `CompatibleResponse` 仅开启显式转换，要求 `store:false`，不能提供 `previous_response_id` 等原生状态能力 |
| API 路径 | 默认 OpenAI 路径 | Custom 可通过 `plugin.endpoints` 按操作启用、禁用或改 URL |
| SSE wire 保真 | 原生事件透传路径 | OpenAI 方言 Chat/Completion 已保留完整 framing；其他跨协议 adapter 仍按转换契约 |
| 资源 RawRelay | 管理员指定渠道可用 | Custom 显式启用目录资源接口后可用；不自动扩张普通用户支持 |

证据：factory 与兼容回落（`providers/providers.go:174`）、能力推导（`providers/openai/capability.go:206`）、Custom endpoints（`model/channel_endpoints.go:29`）、跨协议限制（`relay/responses.go:618`）、SSE 处理（`providers/openai/chat.go:212`）。

Responses WS 还不允许实际改变 model 的映射或改变请求体的 custom parameters；这是已实现协议的使用限制。WS 门控（`relay/capability_gate.go:949`）

OpenAI 区域数据驻留域名另被明确拒绝。这是部署/处理区域支持缺口，不是一个新的 API 家族。区域检查（`model/channel_validation.go:169`）

## 6. 原生协议扩展检查

此节扩大原先 OpenAI/兼容审计的范围，只陈述已核实边界，不宣称完成整个项目的所有原生协议对照。

| 范围 | 当前事实与验证程度 |
| --- | --- |
| Claude | 路由（`router/relay-router.go:158`） 主要为 Messages 和模型列表；未登记原生 count_tokens、模型详情、Message Batches、Files、Skills 等对应入口。其他管理/托管 agent 能力仍需逐操作专项核查 |
| Gemini action | 未实现 action 已明确拒绝，不再把 countTokens/embedding/batch 等默认为 generateContent；两种生成 action 保留。真正计数/embedding/batch 接口仍未实现 |
| Gemini 其他原生面 | Live、Interactions、Files 和模型详情未形成对应入口；embedContent/batchGenerateContent 等命中动态 action 路由不等于正确实现，需独立分发 |
| Recraft、Kling、Suno、Midjourney | 仅有路由/已有适配器盘点，未逐项核对实际供应商协议版本，剩余覆盖与上游生命周期均保留未验证 |

Gemini 计数是独立操作，原错派已由定向回归确认修复；不能将“未实现动作不再生成”写成“计数已经实现”。[Gemini countTokens](https://ai.google.dev/api/tokens)、[Gemini API](https://ai.google.dev/api)、[Claude API](https://platform.claude.com/docs/en/api/overview)

## 7. 上游生命周期与实施取舍

| 官方协议/产品 | 截至 2026-09-19 的生命周期 | 本地覆盖与取舍 |
| --- | --- | --- |
| Videos API / Sora 2 | 将于 2026-09-24 关闭，包含 API 本身 | 仍记未实现，撤下官方默认建设项；有实际需求的兼容厂商另行核查 |
| Assistants | 2026-08-26 已关闭 | 本地管理员 raw 仍存在，不新建完整普通用户生命周期 |
| Realtime beta | 2026-05-12 已关闭 | 不把旧 sessions/transcription_sessions 当现行官方建设目标；不等于 GA Realtime 被关闭 |
| Saved Prompts | 2026-11-30 计划关闭 | 继续记录引用/生命周期缺口，不新建 |
| Evals | 2026-10-31 转只读，2026-11-30 计划关闭 | 不列默认建设项 |
| 自助 Fine-tuning | 开放面收缩 | 管理员 raw 存在；具体账号和兼容供应商另核查，不能由路由推断可训练 |

依据：[官方视频网页](https://developers.openai.com/api/docs/guides/video-generation)、[官方弃用时间表](https://developers.openai.com/api/docs/deprecations)。视频页顶部退役告警曾被文档提取工具遗漏，本次以实际 HTML 与弃用页交叉核对；原研究的默认视频推进建议据此撤回。官方退役日期不自动适用于兼容供应商。

本轮按 A0/A1/A2/A3、独立 W、资源 B 与后台 C 落地，并扩展 Stored Chat/历史音频；首批 Batch 已完成定向/race 验收，其余 D/N 按需求和开放条件逐项选择。具体模块、预算、迁移和验收以 [实现方案](../dev/openai-transparent-relay-implementation-plan.md) 为准，本调查不重复维护第二份实施步骤。

## 8. 证据、账本与验证限制

复核输入包括首轮审计材料、方案修订稿、原 56 项 JSON 清单及二/三/四轮复核、源码摘录和增量清单；结论以当前源码、实际行为观察和官方原文核验为依据。账本的实现状态、授权范围、上游生命周期、验证程度和建议阶段分别记录；按已有证据填写代码依据、官方来源索引、测试引用及核查日期；没有的证据不以已验证补齐。未完成的原生专项不填成 active/已验证；账本不是动态 capability 配置或覆盖率计算工具。现已增加薄的 suboperations 索引，分别记录 method/path/action、关键模式、资源引用/产出位置、当前状态、目标取舍和来源/测试；覆盖新增音频模式及普通用户顶层 list、Batch 内部派生产出、已授权 DELETE 等易混淆子项。这只是已选定范围的初始比较索引，不是所有供应商的完整全集。

初始调研曾运行 15 个已有顶层测试，全部通过：router 拒绝路由注册 1 个；OpenAI provider 的音频/图片 multipart、未知字段、Responses body/compact 等 7 个；relay 的 Stored Chat、资源准入、工具 schema、恢复流和图片 stream 等 7 个。这些测试证明其所断言的当前行为，并未覆盖所有协议模式。

复核在当前 Go 1.27.1 工程环境额外完成：

| 验证 | 结果及含义 |
| --- | --- |
| 临时 overlay：TestAuditResourceScannerObservations（4 子例） | 顶层 function schema 通过；同 schema 在 tool_search_output 被拒；真实 input_file 引用被拒；内联 file_data 通过。确认误拒仍存在 |
| 临时 overlay：TestAuditGeminiCountTokensDispatchObservation | countTokens 被接收为非流式 GeminiChatRequest，action 未保留；结合 provider URL 构造确认错派控制流 |
| 既有 TestValidateNoAccountScopedResourcesChecksEffectiveManifest | 通过；既有 manifest 边界，不代表新回放场景正确 |
| 既有 TestOpenAISpeechPreservesCurrentFieldsAndStructuredVoice | 通过；证明结构化 voice 透传，不证明资源授权 |
| 既有 TestResponsesWSRejectsStreamIDBeforeChannelSelection | 通过；证明 lanes 当前主动拒绝，不是 lanes 已实现 |

临时 overlay 是复核中的行为观察工具，没有作为修复或永久回归测试提交；实现 A0/W 时须把相应正确行为写为仓库内回归。当前材料引用的原压缩包 hash、未提供的 patch/路由脚本及其验证输出没有被当作本次已复验产物；使用当前 Git 代码基线与实际测试记录。外部审计环境的 Go 1.23.2 限制不适用于当前工程环境。

二轮工程观察已运行 TestReview2ChatVoiceProjectionObservation、TestReview2HistoryAudioManifestObservation、TestReview2RealtimeCustomVoicePreparationObservation，确认 Chat voice 对象解码拒绝、历史音频漏提取及 Realtime session voice 对象保留；临时源码仍位于 `/tmp/onehub-second-protocol-review-t9xmki3o`，这些记录不代表本次重新执行。前一轮分析还运行了 TestOpenAIRealtimeSessionRejectsConcurrentResponseCreate，确认当前一般并发拒绝，不是 OOB 专项联调。三轮外部复核只有静态检查，未运行工程测试；当次文档修订仅作 JSON/引用/一致性检查；随后实施的修复验证在下文单列。

上一轮音频交付复核实际运行 `go test ./relay -run '^TestAudioSSE' -count=1`、`go test ./common/requester -run '^TestRequestStreamRequiredTerminal' -count=1`，以及临时 overlay 的 `TestAuditAudioSSECurrentDeliveryBoundaries`，全部通过。overlay 位于 `/tmp/onehub-audio-sse-audit-cnoj0qhd/overlay.json`；四个合成夹具为 done + extension、unknown + EOF、unknown + UnexpectedEOF、done + UnexpectedEOF。结果确认当前提前停读与缺终态 EOF 的交付限制，不代表修复通过或真实上游轨迹，本次文档同步没有重跑这些测试。

本轮五项缺陷修复后，冻结代码执行 `go test ./...` 全量通过；`go test -race ./common/responses ./providers/openai ./model ./internal/billing ./relay ./relay/task/...` 的 10 个相关完整包全部通过。覆盖工具结果协议数组授权、后台工具次数去重、Batch 48 小时内派生观察恢复、WS 唯一请求拒绝不污染后续关联，以及首次 Task 关闭后的消费投影。前两项先复现失败再修复通过，其余经调用链确认与回归验证。

前轮实施的 Batch 91 天正式清理后重读、6 个独立 HTTP 观察回归、前端 114 通过/1 跳过及前端/文档构建保留为历史验证。本轮未改前端，未重跑前端测试。

没有调用真实上游、验证声音越权或实际生成费用；SQL fixtures 使用 SQLite，未验证部署 MySQL/PostgreSQL、真实渠道配置与迁移。独立实时转录完整握手、全部供应商组合和其他原生 API 仍未完成验证。账本 T01–T10 保留历史基线，T11 以后记录修复回归，不把旧的“拒绝正确”测试当新支持证明。
