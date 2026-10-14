---
title: "Antigravity OAuth 渠道接入方案"
layout: doc
outline: deep
lastUpdated: true
---

# Antigravity OAuth 渠道接入方案

## 文档状态

- 状态：渠道类型 102、管理授权与四入口适配已实现，已使用本地模拟上游验证；真实 Google 账号及具名客户端的联调尚未完成。
- 适用范围：Google OAuth 授权、凭据刷新、project 初始化，以及现有 Gemini、Claude Messages、OpenAI Chat Completions 和 Responses HTTP 入口的 Antigravity 适配。
- 文档口径：当前架构依据 one-hub `3ee17687`；参考本地 new-api `4013729f6`、done-hub `42dca4a`、sub2api `9c2cca9e6`、CLIProxyAPI `0d18d3b3`。源码与定向测试用于确定实现差异，真实 Google 授权、内部接口、模型和签名行为仍需联调。
- 设计取向：透明转发优先，只做必要的正确性校验、协议转换和接口请求修复；复用现有渠道、凭据 fence、选路、传输和结算。
- 关联文档：[渠道编辑确认方案](./immutable-channel-identity-architecture.md)、[凭据刷新 fence](./codex-credential-refresh-fence-architecture.md)、[Codex 上游协议边界](./codex-official-upstream-architecture.md)、[请求重试边界](./responses-ws-attempt-replay-architecture.md)、[TCC 计费](./usage-confirmed-tcc-billing-architecture.md)、[Channel Affinity](./channel-affinity-architecture.md)。

## 问题与目标

Antigravity OAuth 提供访问 Google 上游的身份。one-hub 仍使用自己的用户、Token、渠道、分组与售价进行鉴权、准入和计费，两套凭据不互换。

该上游通过 Cloud Code 的 `v1internal` 接口接收 Gemini 风格的请求，并在外层携带 `project`、模型和客户端元数据。即使实际模型是 Claude，也需要按此协议调用。provider 负责 OAuth 凭据、内部 envelope 及必要的方言适配；可以原样交给上游的内容保持原样。

目标是让管理员通过 OAuth 建立一个普通 one-hub 渠道，并让已支持的公开入口按明确的转换契约使用它。调用结果、工具语义、流式结束和用量必须可解释；其他渠道的行为不因接入而改变。

### 透明转发与必要校验

先判断请求能否直接表达，再决定需要读取和转换哪些字段。透明转发保留客户端的参数、历史、顺序及上游错误；跨协议转换保留这些内容的语义。接口请求修复用于处理已确认的表示差异，不把请求改造成另一项任务。

| 边界 | 本地处理 | 交给上游的判断 |
| --- | --- | --- |
| 代理自己的契约 | JSON envelope、路由控制、权限、凭据、资源归属、生命周期与容量；复用现有策略 | 模型是否存在、账号是否有模型额度或可用权限 |
| 可直接传递的参数 | 保留 raw 字段、联合类型、schema、原生签名；只修改代理拥有的控制字段 | sampling 范围、模型参数组合、schema 约束有效性、签名有效性及历史能否被模型接受 |
| 必须转换的内容 | 只检查生成目标表示所需的结构、关联和语义；无法表达时返回带参数路径的能力错误 | 已保留下来的目标字段值是否符合该模型当前规则 |
| 已知接口差异 | 在 provider 的纯转换函数中按明确条件做最小修复，保留未涉及字段 | 未识别的新增字段与上游规则变化，不通过扩大清洗范围猜测兼容 |

字段未知、模型未收录或 typed 投影失败，本身都不是拒绝理由。跨协议中能落入明确等价的 raw 子树的内容继续保留；只有缺少目标表达、会丢失语义或触及代理边界时才拒绝。这里不新增上游参数校验器，也不执行用户 schema 来验证工具参数或模型输出。

## 参考实现的取舍

| 来源 | 吸收的优点 | 在 one-hub 中的落点 | 不采用的行为 |
| --- | --- | --- | --- |
| new-api | 复杂 schema 保留、文本与工具调用并存、同帧 reasoning 与正文处理 | 补充转换与流式样本；该项目没有独立 Antigravity 实现 | 完整协议转换注册框架；有损转换默认放行；将公共 Gemini API 能力视为内部接口能力 |
| done-hub | OAuth 结果作为渠道凭据；复用多渠道管理和计费；清晰的 Antigravity envelope | 新增渠道类型和 provider，接入现有管理表单与 relay | 多级 provider 继承及 Claude→Chat→Gemini 转换链；随机 project；无 fence 覆写凭据 |
| sub2api | 授权、project onboarding、token 获取与业务转发分开；PKCE；跨协议流式与非流式统一处理 | 独立 OAuth 服务、现有凭据服务、一个上游 SSE decoder 与多个下游 codec | 另建账号池；Redis 锁作为刷新授权；签名失败降级；网络错误与空响应自动重试 |
| CLIProxyAPI | executor/translator 分工；不透明签名往返；账号模型发现；细分流式终态 | provider/wire 分层、原生签名 carrier、模型发现管理操作 | 整套账号调度与会话 replay 系统；未经验证的强制 HTTP/1.1；静默删字段 |

参考实现中的相关变更：

| 更新证据 | 本方案的落点 |
| --- | --- |
| new-api `c10780e05`、`ba0e478e0`、`5ac756284`：schema 与混合内容保真 | 原样传递可表达的 schema；覆盖 text + tool_use、reasoning + text 样本 |
| done-hub `b2d3e26`：OAuth callback 输出编码与 CSP | 保留现有手动 callback 方案；管理 UI 使用安全文本呈现，不记录或跨窗口广播凭据 |
| sub2api `cfbbfd6b3`：图片限流与文本隔离 | 复用现有渠道加实际模型的冷却作用域，不引入 Gemini 公共桶和图片例外 |
| CLIProxyAPI `dfe0a0ef`、`ab151f5d`：Chat 可选字段与 thinking 用量 | 未产生工具调用时省略 tool_calls；同一原始 usage 派生对客字段和计费证据 |
| CLIProxyAPI `a0fd8448`、`a2718314`、`15d2148b`：schema union、尾部 thinking、工具结果媒体 | 吸收失败样本；不采用 oneOf 改 anyOf、删除历史、补空 user 等语义改写 |
| CLIProxyAPI `091ff27f`、`77fd91e8`：目录提示持久化与 tool_search | 现有模型配置已持久化；client tool_search 确有请求级映射，但仅在下文扩展条件成立时纳入 |

参考定位：new-api 的 `relaykit/relayconvert/`；done-hub 的 `controller/antigravity_oauth.go`、`providers/antigravity/`；sub2api 的 `backend/internal/pkg/antigravity/`、`backend/internal/service/antigravity_*`；CLIProxyAPI 的 `internal/auth/antigravity/`、`internal/runtime/executor/antigravity_*`、`internal/translator/antigravity/`。这些实现不作为当前上游契约的证明。

## 当前架构与必要改造

| 边界 | 当前实现 | 本次变化 |
| --- | --- | --- |
| 渠道与 provider | `providers/providers.go` 的 factory 注册；`model.Channel` 持有连接配置 | 新增 `ChannelTypeAntigravity` 和独立 factory；数字 ID 从本项目未占用值分配，不照搬 done-hub 的 60 |
| 凭据 | `common/credentials.Service`、`model.ChannelCredentialStore`、`internal_state + version` | 复用 Claim/Commit/Cancel/Recover；提取 Codex 中可共享的刷新互斥，Google 交换协议留在 provider |
| Chat 候选评估 | `providers/capabilities.go` 的 factory assessment；`relay/capability_gate.go` 评估映射模型与有效请求 | 评估与执行共用纯转换函数，不增加跨层 Plan 契约；扩展保留使用通用 raw 能力 |
| Claude/Gemini 候选资格 | `relay/claude.go`、`relay/gemini.go`、`relay/common.go` 尚有渠道类型列表；请求评估不完整 | 改为 factory 声明资格与请求可表示性；既有 provider 同步声明当前支持面 |
| Responses 执行 | `DataPathCrossProtocol` 同时触发固定 Responses→Chat 路径 | resolver 从既有声明派生执行目标，与 DataPath 一并返回，不增加独立配置 |
| 请求真相 | 原始/规范化 body 已缓存；Gemini ingress 仍先强制 typed 解码；Responses 已有 `RawEnvelope + Control + Policy` | Gemini 从 ingress 到 provider 保持 raw；typed 值仅作投影，媒体处理产生显式 patch |
| 推理与结算 | `RelayHandler` 认领一次 submission，requester 记录发送事实，`AttemptQuota` 统一关闭 | 沿用现有链路，provider 不获得重试或结算权 |

`CONTEXT.md` 中旧的 Channel Account Incarnation 定义仍提到身份不可变；本文以现行渠道编辑文档、AGENTS.md 和管理实现为准：渠道 ID 是可编辑的管理记录。接入不引入换账号自动 clone、资源身份迁移或永久账号绑定。

### Claude/Gemini 的共享能力边界

沿用 Chat 的工厂评估模式，在 `providers/base` 和 `providers/capabilities.go` 增加 Claude/Gemini 请求支持声明与纯本地 assessment。输入是候选渠道快照、映射后的模型和应用 `pre_add` 后的有效 raw 请求。评估与执行调用同一纯转换函数，以能否构造目标表示判断可表示性；不另建验证白名单，不进行 OAuth、project discovery、媒体下载或上游试探。共享的是调用约定、请求事实与错误类型，不要求既有 provider 都实现同构 planner 类。

移除相关 ingress 与候选过滤中的渠道类型枚举，让既有 Anthropic、Bedrock、Vertex、Gemini、Custom factory 声明自身原有资格。Claude 的远程媒体评估并入同一次候选评估流程，保留原有权限与资源约束。公开模型列表也读取相同资格事实，避免列表可见而请求无法选路。

候选不可表示沿用 Candidate Representability Mismatch：普通调度可在 provider work 前考虑其他合格候选；所有候选都失败才形成公开 unsupported 错误。显式 pin、已有资源 owner 和已认领 submission 不获得候选切换权。

### Gemini raw 入口闭环

`relay/gemini.go:setRequest` 当前在选渠道前将 body 强制解码成 `GeminiChatRequest`；即使 provider 读取缓存 raw，未来联合类型也可能已经在 ingress 被拒绝。本次将 Gemini 请求真相改为规范化 raw object，加上 URL 中的模型/操作和本地 stream 控制；只校验 JSON envelope 与代理实际拥有的控制边界。

typed 内容解码降为观察、内容策略或预估所需的投影。投影失败不能直接否决所有 same-dialect 候选；依赖该形态的既有 provider 在自身 assessment 中判断可表示性，Antigravity raw 路径不受无关 typed 字段约束。适用的内容安全策略若需要理解某字段，仍由该策略明确拒绝或允许，不能以投影失败静默绕过。token 预估不因未知字段失败而变成真实计费证据。

同步调整 Gemini provider-facing 请求契约及现有 Gemini/Vertex 实现，让 raw 与路由控制始终作为一个请求对象传递，不保留“typed 失败后可选 raw”的双接口。响应复用现有 Gemini raw capture/replay。Claude 同样先保留 canonical raw；cross-protocol 的不可表示判断由候选 assessment 给出，而非在评估前丢掉未知字段。验收必须经过真实 HTTP ingress，不能只直接调用 provider 验证未知字段。

### 通用扩展字段保留

Chat message、stream delta 及转换依赖的工具对象需要保留未知扩展，这是通用协议能力。复用 `common/jsonobject`、`json.RawMessage` 和现有 Responses raw 保留方式，在对应通用 wire 类型或原始交付路径解决；不向通用 DTO 增加 AntigravityState 等 provider 专属成员，也不在 relay 中拼接专属字段。

原样交付使用原始表示；必须重编码时，未知字段保留其值、显式 null 和数字表示，只有明确修改的已知字段覆盖原值。转换器生成的扩展通过同一机制输出，恢复后的请求仍受既有控制字段、内容和资源策略约束。raw 保留只防止数据丢失，不自动赋予跨协议字段等价性。

### Responses 的最小契约调整

在现有 `ResolveAdapterSupport` 边界解析 Responses create 的执行目标，与其 DataPath 一并返回。目标只由 provider 的操作声明和既有 `CompatibleResponse` 派生，不由管理员或 provider 再维护一份独立开关：

- factory 声明实现 Responses create：目标为 `provider`，DataPath 使用该声明；Antigravity 因此得到 `provider + cross-protocol`。
- factory 未声明 create，但已启用 `CompatibleResponse` 且具备 Chat 支持：目标为 `chat_adapter`，DataPath 必须为 `cross-protocol`。
- 两者都不满足：create 不受支持，不产生执行目标。

`relay/capability_gate.go` 与 `relay/responses.go` 的候选评估、选中后请求准备、媒体准备和 send 统一根据执行目标选择路径；DataPath 只表示协议关系。特别是 `validateSelectedProviderRequest` 也要退出按 cross 自动执行 Responses→Chat 校验与转换的逻辑。`provider` 路径保留 Responses raw envelope，由自身 assessment 判断可表示性，不能因共享 typed projection 失败或进入 Chat preparation 而提前拒绝。不能以类型断言替代 factory 声明：部分 provider 会通过嵌入 OpenAIProvider 继承 Responses 方法，但实际并未声明对应上游能力。

这只是现有两条执行路径的解析结果，不新增全操作 adapter 注册框架或持久化字段。原有 direct 与 Chat fallback 同批迁移并验证，不能保留按 DataPath 猜测执行目标的第二套逻辑。

## 支持面与协议分类

四类入口都复用现有路由，不另开 `/antigravity/*` 推理入口。下表是目标支持面；按后文实施批次通过测试后才注册能力。

| 入口/操作 | 协议分类 | 本方案承诺 | 明确边界 |
| --- | --- | --- | --- |
| `/gemini/:version/models/:model` 的 generateContent/streamGenerateContent | same-dialect，外加内部 envelope | 保留内层 Gemini raw 请求及未知扩展，解包响应；由上游判断模型与参数支持 | 外层包装使整个操作不属于 exact-wire；仅对已知且不能等价表达的方言差异在发送前拒绝 |
| `/claude/v1/messages` | cross-protocol | 直接 Claude→Antigravity；文本、可表示的内联图像、function tools、thinking 与签名；工具签名使用下文明确的 carrier | 不经过 Chat；工具签名扩展需目标客户端保留；服务端工具、远程资源引用、缓存指令等只在有明确等价映射时支持 |
| `/v1/chat/completions` | cross-protocol | 文本与可表示的 function tools，覆盖流式和非流式；实际产生签名状态时通过响应扩展携回 | 不支持 Stored Chat；生成图片的输出转换不属首期承诺；签名多轮要求客户端保留必要状态 |
| `/v1/responses` create | cross-protocol，执行目标为 provider | 显式 `store:false` 的无状态 create；消息、function items、内联图像与可往返的复合签名 carrier | `store` 省略或为 true、`previous_response_id`、conversation、`background:true` 不支持；远程图像 URL 尚无等价准备路径，发送前拒绝 |
| Responses retrieve/delete/cancel/input_items/compact/input_tokens/WS | 首期不注册 | 无 | create 外形适配不等于拥有资源、压缩、计数或 WebSocket 契约 |
| Gemini countTokens、Claude count_tokens、Interactions、Files、Batch 及独立图像 API | 首期不注册 | 无 | 属于本次范围取舍，不表示真实上游一定不存在相应能力；generateContent 内可直接表达的媒体字段继续保留 |

同方言路径不做上游参数白名单。跨协议只对转换实际依赖的字段解码；目标能够原样接收的嵌套数据保留 raw，无法表达的内容明确失败。媒体复用现有能力、访问策略与准备流程：已批准的转换结果照常使用，目标可直接消费的 URI 在满足现有策略后传递；不因来源曾是 URL 再次拒绝，也不让新 provider 自行下载或绕过资源授权。

模型名称按既有 Models/ModelMapping 选路后交给上游，不另建“已验证模型”准入名单。可表示性由入口、目标协议和实际请求内容决定；仅当具体转换确实依赖某项已知方言差异时，才在 provider 内声明该差异。缺少某项必要映射时只拒绝依赖它的请求，不扩大为整个新模型不可用。具名模型与客户端测试用于发布验收，不成为运行时模型目录。

### 首期范围与扩展条件

首期完成上述四入口的 OAuth 接入、已声明生成与工具请求、内容和签名往返、流式与错误交付，以及现有计费闭环。原生 Gemini 中可直接表达的媒体、schema、签名等数据保持透传；必要 envelope 修正与已承诺 thinking 字段的正确映射属于该闭环。跨协议支持以表中声明为准，不因上游提供同名能力就自动承诺等价转换。

新增能力需满足实际需求：它解决已承诺请求的正确性问题，或指定必需客户端确实依赖它，并且能定义清楚转换、错误、资源和费用契约。参考项目有实现、不需要数据库或只涉及几个字段，都不足以单独成立实施理由。以下是范围决策和重新评估条件，不是默认排期或预留 feature flag：

| 能力 | 当前取舍 | 重新评估条件 |
| --- | --- | --- |
| CountTokens | 暂不实现。CLIProxyAPI 确实调用上游计数接口，但 one-hub 当前 Gemini 仅接受生成操作，Claude 也没有 count_tokens 入口 | 必需客户端实际调用并因缺失无法正常使用；随后明确独立计数请求、权限、错误及资源边界。不能直接将该结果当作生成用量或 Responses input_tokens 的等价实现 |
| Responses client tool_search | 暂不实现；它是客户端执行的工具发现，不需要服务端搜索状态，但仍有命名空间、历史配对与 item 生命周期 | 必需客户端确实发送 client tool_search，普通 function tools 不能满足；只扩展能够完整表达的 client execution，不顺带支持 server execution |
| 跨协议 web_search 与引用转换 | 暂不实现。原生路径可表达的搜索字段不因此被清除；CLIProxyAPI 的搜索能力不证明其历史改写和参数映射等价 | 明确的客户端搜索需求，并能保留查询、工具约束、引用及用量语义；不可表示的混用不能通过删工具、压缩历史或强制 prompt 放行 |
| Chat 图片输出转换 | 可后置。原生 Gemini 图片保留仍属首期；通用 Chat 类型已有 images 字段不能证明整个图片转换链已支持 | 明确要求在 Chat 入口获得生成图片，并验证 MIME、文本与图片顺序、流式/非流式及用量；不顺带扩展独立 Images API |
| Interactions | 本轮不做；CLIProxyAPI 有生成桥接，但新增第五种入口并非当前四入口交付的前提 | 新的客户端或产品要求；分别定义无状态生成与存储、续接等生命周期，不能用桥接成功代替后者 |
| 服务端签名恢复 | 默认不做。这是有兼容性收益的状态能力，具体触发条件见下文客户端验收 | 指定必需客户端确实丢弃必要签名材料，且无法通过现有原生字段或客户端修复完成往返 |
| 模拟 compact、credits fallback | 本轮不做；前者增加有损摘要生成，后者增加付费选择及重放策略 | 独立的产品需求及执行、费用和状态契约，不能作为 OAuth 反代的隐式能力加入 |

### 最低交付用例

四个入口必须有非空的实际可用范围。发布验收记录要写明账号实际可用的上游模型 ID、客户端名称/版本和配置；不能以一个接口注册成功或临时删掉难例替代功能完成。

| 路径 | 完成交付的最低证据 |
| --- | --- |
| Gemini | 至少一个 Gemini 模型完成原生文本、functionCall→functionResponse 两轮，以及签名原样往返；流式和非流式均通过 |
| Claude | 至少一个 Antigravity 托管 Claude 模型，在指定 Claude 客户端完成文本、thinking、单工具和并行工具的两轮；工具 part 签名不能丢失 |
| Responses | 至少一个签名模型完成无状态文本与工具两轮，覆盖真实思考、工具附着及独立签名 carrier；目标客户端实际回传 encrypted_content |
| Chat | 普通文本完成流式与非流式；至少一个签名模型由保留响应扩展的客户端完成 function tools 两轮，同时覆盖流式、非流式及并行工具 |

不要求所有账号有相同模型，但上述用例不能全部以“不支持”跳过。实施前确定各入口用于验收的客户端、版本与操作，先验证文本和两轮工具调用；不能只选择容易通过的客户端来代替已经明确的必需客户端。若必需客户端丢弃必要签名材料，该交付项保持未完成，先检查原生字段及客户端可修复性，再按扩展条件评估最小服务端恢复。

缓存命中补回真实材料与缓存未命中时伪造签名是不同能力。确需增加恢复时，必须先定义用户与连接身份隔离、可靠历史关联、容量、淘汰/重启、多节点和失败语义；这仍是功能取舍，不在本方案中预建缓存。不存在该需求时，按已验证的客户端往返范围发布。可选能力不自动进入最低交付矩阵。

## 责任与目录

```text
现有 ingress / canonical raw body
    → providers 请求评估（本地扩展归属、factory 能力、纯转换）
    → relay 选定候选并完成既有媒体准备
    → relay 准入与一次 submission
    → Antigravity provider 复用纯函数构造 body、获取凭据、填充 envelope
    → requester HTTP / long stream
    → Antigravity JSON 解包或 SSE decoder + usage evidence
    → 对应下游 codec / 有界非流式聚合
    → 现有交付与 AttemptQuota 关闭
```

实现的主要位置如下；协议样本位于相应的 `*_test.go` 中，均为本地构造样本：

```text
controller/antigravity_oauth.go          管理权限、输入、OAuth 会话和管理结果
providers/antigravity/
  base.go                              factory、配置、provider 接口
  capability.go                        操作声明、扩展格式声明与纯转换函数接入
  oauth.go                             Google code/refresh/userinfo/project 协议
  credentials.go                       对接现有 credentials.Service
  models.go                            管理端模型发现
  protocol.go                          四入口执行、envelope 和 usage 接线
  media.go                             已准备媒体的局部 raw patch
  stream.go / errors.go                流生命周期与对客错误映射
  wire/
    request.go                         纯转换入口及最终响应支持面检查
    chat.go / claude.go / responses.go  直接跨协议转换
    signature.go                       不透明签名载体与关联
    response.go / response_codec.go    请求内状态与三种输出编码
    stream.go / usage.go                raw 解包、终态与用量字段提取
```

`wire` 不 import Gin、数据库或 requester，不刷新凭据，不选渠道。provider 只执行已确定的候选；relay 继续拥有鉴权、准入、选路、健康观察与结算。OAuth 控制操作与推理共享连接安全基础，但不共享 Work Action 的业务生命周期。

评估与执行复用纯转换函数，不强制把 factory 产出的计划传给 provider。同一有效请求可以在选定后重新构造目标 body；确有必要复用的已准备数据沿用现有请求内准备流程，不新增通用 Plan 接口、Gin 隐式计划槽位、计划缓存或消息 IR。最终媒体物化与转换仍在刷新凭据前完成，已确认不可表示的请求必须保持 OAuth refresh 和推理 dispatch 均为零。

共享代码只承担已有多个使用方的职责：raw 字段保留、候选评估、凭据输入形态、PKCE 与刷新互斥。Antigravity 的 envelope、project 初始化、字段修复及签名关联保留在 provider。目录中的 planner/wire 表示纯函数分工，不是新的执行框架。

不整体继承 `GeminiProvider → OpenAIProvider`。当前 Gemini Chat converter 带有默认 safety、工具名称解释、schema 清理和模型字符串策略，不能作为 Antigravity 的隐式规则来源。只有经样本验证适配的 JSON、SSE、媒体和 usage 叶子能力可以复用。

## 渠道与凭据数据

沿用现有 `key + other + internal_state + version`：

| 位置 | 内容与事实来源 |
| --- | --- |
| `Channel.Key` | 唯一持久化凭据：access token、refresh token、明确的 expires_at、Google 稳定 subject、project_id；email 仅作管理展示 |
| `BaseURL`、`Proxy` | 复用现有连接地址与代理配置；空 BaseURL 使用 provider 的单一默认端点 |
| `internal_state.credentials.refresh` | 现有无 TTL、无 secret 的刷新 fence，由凭据服务写入 |
| `version` | 现有管理配置、凭据和生命周期并发版本；不作为资源 owner 或稳定会话身份 |

Antigravity 凭据使用一个明确的 JSON 格式，不引入 `project|token`、插件旧字段或其他项目私有 auth 文件的长期兼容分支。首次保存要求完整 token、过期时间、subject 和 project。首期通过管理授权取得完整凭据，也允许导入同格式的完整 JSON；不提供仅 refresh token 的导入接口，不能把未完成凭据加入可调度渠道。

`controller/channel.go` 当前仅对 Codex JSON 跳过按换行拆 key，`controller/channel-model.go` 也会直接取 key 的第一行。将“整体凭据或按行列表”的最小声明放在现有 factory 注册边界，由共享输入函数消费；Codex、Antigravity 声明整体凭据，其他渠道保持原有解释方式。创建与模型发现都调用该函数，不各自增加渠道类型分支，也不通过 json.Valid 猜测格式。pretty JSON 始终作为一份凭据；新建、标签新增及批量导入共用验证与原子写入边界。

首期不创建 `Other.antigravity` 空配置命名空间。已有 BaseURL、Proxy 和服务端 OAuth client/UA 默认值覆盖当前实际设置，只有新增了明确的渠道策略字段才扩展 Other。

OAuth client 身份由服务端统一 provider 配置拥有，不重复写入每个渠道 key。授权端点与 token endpoint 是服务端固定的 Google 地址，不随渠道 BaseURL 变化；client secret 不进入文档、普通响应、日志或前端持久存储。

使用时在渠道表单选择 Antigravity，打开授权链接，完成 Google 授权后把浏览器中的完整回调 URL 粘回表单；回调页无法连接本地端口不影响复制 URL。普通授权产生草稿，保存渠道后才持久化。服务端可用 `ANTIGRAVITY_OAUTH_CLIENT_ID`、`ANTIGRAVITY_OAUTH_CLIENT_SECRET`、`ANTIGRAVITY_OAUTH_REDIRECT_URI`、`ANTIGRAVITY_USER_AGENT` 覆盖默认客户端配置；默认回调为 `http://localhost:51121/oauth-callback`。真实 Google 客户端配置是否接受该回调和 PKCE 尚需账号联调。

代理中的 `%s` 会话占位符在创建 OAuth 连接快照时复用 `Channel.SetProxy()` 展开。已有渠道使用数据库中的 Key；新建草稿尚无 Key，沿用空 Key 的展开语义。授权交换固定使用该快照，表单和数据库保留代理模板。

账号 subject 从可信 Google userinfo 响应取得，不把 access token 当成 JWT 解码，也不用 email 证明账号身份。project 是上游连接上下文，不表示本地用户、计费 owner 或客户端可覆盖的请求参数。

### 数据库变更边界

以当前仓库声明并完成既有迁移的 Schema 为基线，本接入不新增表、列、索引或 Antigravity 专属迁移。`model/channel.go` 已包含整数 Type、TEXT Key、Version 和 JSON InternalState；`model/channel_snapshot.go` 的通用 CAS 已能原子更新 key、version 和内部状态。

| 数据或能力 | 使用现有载体 | 不新增的结构 |
| --- | --- | --- |
| token、expires_at、subject、project | Channel.Key 中一份 provider JSON | 独立 token/账号表、重复凭据列或 project 列 |
| 刷新 claim 与并发版本 | internal_state.credentials.refresh 与 version | Google 专属 fence、刷新表、额外版本列或 JSON 查询索引 |
| OAuth 临时会话 | 现有一次性缓存和 TTL | 关系库 session/消费结果表及清理任务 |
| 模型目录与连接配置 | Models、ModelMapping、BaseURL、Proxy 等现有字段 | discovery 副本表或 Other.antigravity 空配置 |
| 扩展格式、凭据输入形态、Responses 执行目标 | 代码声明及本次请求的解析结果 | 渠道配置列、功能开关表或能力注册表 |
| 签名 carrier 与已准备请求 | 客户端材料及请求内存 | 会话历史、签名恢复或请求计划存储 |

已有迁移 `202610030001` 负责 version/internal_state 和旧 fence 的转换，旧部署仍按项目正常升级路径执行；本接入不重复该迁移，不回填、重编码或清空既有渠道数据。实际部署是否完成既有迁移需上线检查，源码定义本身不能证明部署状态。

无新增 DDL 不等于无持久化契约变化：新增的渠道类型值与该类型的 key JSON 需要新代码识别。类型只使用未占用值，保持既有编号；运行时通过现有渠道 API 和凭据服务正常创建、更新记录。兼容性处理见上线与回滚，不为尚未上线的格式增加双读迁移。

## OAuth 管理流程

采用 sub2api 的 PKCE 思路和现有 Codex 管理端的手动回调输入方式：浏览器访问 Google 授权页，用户将完整 callback URL 粘贴回管理页面。one-hub 不新增公网匿名 callback 或本地监听器。redirect URI 必须使用该 OAuth client 已获准的精确值；参考项目的 localhost 地址需要联调确认，不能把任意部署 URL 当成已注册回调。

把 `controller/codex_oauth.go` 中的 PKCE verifier/challenge 生成和完整回调 URL 的 query 唯一性解析提取为小型公共函数，例如放入现有 `common/authutil`。提供方传入所需参数并保留各自回调策略；提取不能改变 Codex 现有 raw code 等输入契约。Google token/userinfo/project 协议与身份恢复核验留在 Antigravity，不引入统一 OAuth 流程框架。

新增管理路由建议置于 `/api/antigravity/oauth`，全部使用现有 AdminAuth 与适用的管理请求保护：

1. `start`：生成随机 state、PKCE verifier 和 S256 challenge；记录发起管理员、操作类型、redirect URI、代理快照，以及已有渠道的 ID/type/version/subject/project/fence attempt。返回授权 URL 和 session ID。
2. `exchange`：检查管理员与会话绑定，原子消费 state，再验证完整 callback URL 的 scheme/host/path、唯一 code/state 及 state 一致性。使用保存的 verifier 与 redirect URI 交换 code，不接受客户端覆盖这些字段；歧义失败要求重新授权，不重复 exchange。
3. 获取 userinfo，执行 `loadCodeAssist`；缺 project 时，仅在这次管理员授权操作中进行有界 `onboardUser`。按上游返回的 operation/完成状态继续观察，不能因超时重新发起无法证明幂等的 onboarding。
4. project、subject 与 token 资料完整后，按会话中固定的操作类型形成草稿或直接恢复凭据。失败不生成随机 project，也不把未完成结果保存为可用渠道。

state 直接复用现有 `common/cache.ConsumeCacheContext`。key 为 provider 命名空间、当前管理员 ID 与随机 state 的组合，TTL 为 10 分钟；管理员绑定还保存在值中并在消费后复核。每次 start 使用独立随机 key，允许同时授权不同渠道；旧 callback 不会消费新会话，无需单管理员槽位或新的 compare-delete 原语。消费回执不确定时不交换 code，也不重放消费操作。

单个会话序列化后最多 4 KiB，只保存 PKCE 和必要操作快照；超限在 start 时拒绝。start 复用 CriticalRateLimit 和管理鉴权，其限速按 IP 生效，不宣称每管理员有固定会话上限。本地缓存受现有 16 MiB 总容量限制；Redis 使用部署配置的总内存预算和淘汰/拒写策略，发布检查须确认该预算，不能把 TTL 当作全局硬容量。会话被淘汰或写入失败只导致重新授权，不允许跳过 state。

单机使用现有本地缓存；多实例使用共享 Redis，或明确将整个流程固定到同一节点，缓存缺失一律重新开始。会话不缓存 token 结果；草稿模式的完整凭据只在受保护的管理响应和当前表单内存中短暂存在，不写 localStorage；恢复模式仅返回提交结果和新版本。过期或被消费后由现有缓存清理，不新增状态表、索引或独立 janitor。

### 授权结果的三种提交语义

| 操作 | 提交方式 | 并发与身份约束 |
| --- | --- | --- |
| 新建渠道 | exchange 返回凭据草稿；使用现有渠道创建 API 保存 | 创建前不自动刷新、不进入路由 |
| 现有渠道独立授权或换账号，且无未决 fence | 更新表单 key，通过现有编辑 API 原 ID 保存 | 统一影响确认；携带 expected_version；版本冲突重新读取，不自动覆盖 |
| 有未决 fence 的同账号恢复 | start 固定为恢复模式；exchange 内核验新凭据后直接调用 `credentials.Service.Recover` | 使用已原子取出的服务端授权快照、fence attempt 和可信 subject/project；不接受浏览器提交的新 key 或恢复快照 |

恢复模式在用户提交 callback URL、发送 exchange 前展示影响确认；确认后这次受保护的 exchange 同时拥有独立授权与恢复提交，不增加“返回草稿后再次 recover”的中间步骤。这样固定快照和新凭据来源始终留在同一服务端操作中，无需签名结果票据或额外结果缓存。恢复不顺便保存表单中的其他未提交配置；成功后刷新版本，保留尚未保存的编辑。账号不同、project 不同、类型/版本/attempt 已改变均拒绝恢复；管理员换身份属于普通编辑，未解决 fence 时仍遵守现有阻断规则。草稿模式的后续保存始终携带授权开始时的 expected_version，不自动套用较新的表单版本。

下面是本地浏览器测试中的恢复确认界面，使用模拟上游和虚构渠道数据：

![恢复渠道凭据前展示影响说明与取消、确认按钮](/images/antigravity-credential-recovery.png)

单渠道和标签编辑继续执行现有保存确认，不查询任务占用，不自动 clone。标签编辑不提供批量替换 OAuth key。已有任务、文件、响应和会话不因编辑获得迁移或继续可用的保证。

## 刷新协议

Google refresh token 是否轮换、何时可重用不能从 Codex 推断。本方案仍选择复用现有保守 fence：一旦 refresh 可能 dispatch，未知结果不自动重试。代价是网络歧义或进程崩溃可能要求重新授权，即使 Google 在某些情况下允许旧 refresh token 重用。

请求准备阶段先使用仍有效的 access token；距离明确的 expires_at 不足 5 分钟时进入刷新。首期不新增定时扫描 worker 或独立 token cache。刷新发生在推理 HTTP dispatch 前；若当前 relay 已认领 submission 后准备失败，本次正常关闭，不倒退到候选切换。

```text
读取数据库最新渠道快照
    → 判断 access token / 已有 fence
    → Service.Claim（数据库确认后才允许 OAuth dispatch）
    → 一次 Google refresh exchange
    → 成功：合并凭据 → Service.Commit → 发布新 access token
    → 明确未 dispatch：Service.Cancel
    → dispatch 结果不确定或提交失败：保留 fence
```

将 `providers/codex/base.go` 中按渠道加锁、可取消等待和引用回收的互斥实现提取到 `common/credentials`，由 Codex 与 Antigravity 使用同一实现；不复制第二个 provider 锁池，也不直接依赖 Codex 包。等待后重读凭据，数据库 claim 仍是跨实例授权事实，不增加 Redis 刷新锁或独立 token cache。

成功响应必须有有效 access token 与可计算的过期时间。未返回新 refresh token 时保留旧值；返回新值时与 access token 原子提交。subject/project 沿用同账号快照，不在刷新中自动重做 onboarding 或切换项目。不得先把尚未 durable commit 的新 token 放入路由快照。

OAuth 成功后的 DB commit 使用独立于客户端取消的有界收尾上下文，复用现有 5 秒口径；可以重试数据库 CAS，不能再次调用 OAuth。存在 fence 时仍可使用有效旧 access token；需要刷新而无法继续时返回可诊断的凭据错误，由管理员恢复。provider 健康状态可影响后续请求，不能触发当前推理的 401 refresh-and-replay。

OAuth code/refresh、userinfo、project/onboarding 和模型发现均显式使用有界 JSON 读取：认证与身份响应最多 1 MiB，project/模型发现响应最多 4 MiB；不能依赖普通 `SendRequest` 的 JSON decoder 自带总量限制。超限终止解析；refresh 已可能 dispatch 时保持 fence，不能将读取失败归为未发送。控制调用设置有界 deadline，onboarding 整个管理操作还需总时限，不通过无限轮询占住管理员会话。

## 上游请求与协议转换

### 单一 envelope 与连接策略

Gemini 同方言入口保持原有响应形态：非流式调用 `POST /v1internal:generateContent`，对外层 response 做 raw 解包；流式调用 `POST /v1internal:streamGenerateContent?alt=sse`。非流式不从 SSE 重新合成，避免无法为未知扩展字段定义聚合规则。该模型若不能提供等价的非流式结果，则不声明这个操作支持。

Claude、Chat、Responses 跨协议入口统一使用上游 SSE，非流式客户端由同一个 decoder 配合有界聚合器生成目标响应；聚合与流式 codec 共享字段映射和签名关联。这样保留 sub2api 的转换复用，同时采用 done-hub/CLIProxyAPI 按实际协议需要选择调用形态的做法。代价是多保留一个原生 JSON 解包路径，收益是 Gemini 未知字段不依赖代理的聚合知识。

默认推理端点选用参考项目共同覆盖的 `https://daily-cloudcode-pa.googleapis.com`；管理员可通过现有 BaseURL 明确选择另一个受信端点。一次请求固定一个端点，不依据 tier 猜测路由，也不做 daily/prod 回退。重定向不能携带 OAuth Bearer 到另一个 origin；复用并验证现有出站地址、代理和认证头边界。

下面是普通生成的 envelope 示例，不表示所有已接入操作都固定为 agent。CLIProxyAPI 存在 image_gen 与 web_search 请求类型；只有本次已支持的请求确需对应类型且接口证据成立时，才在 provider 中补充必要映射。保留内层字段不能替代必要的外层适配，也不能反过来因发现一种 requestType 就扩大接口支持面。类型选择依据实际请求与明确的方言差异，不照搬模型名包含 image 的猜测。

```json
{
  "project": "<channel credential project>",
  "model": "<mapped upstream model>",
  "userAgent": "antigravity",
  "requestType": "agent",
  "requestId": "agent-<unique id>",
  "request": { "contents": [], "generationConfig": {} }
}
```

provider 在目标 body 构造成功后，根据凭据、渠道与选路事实填充 Authorization、project、外层模型、requestId 和客户端元数据；客户端 one-hub Token 不向上游转发。requestId 仅用于关联，不被当作幂等保证。UA 由一个服务端默认值拥有，版本调整需要样本验证；不靠 TLS 指纹或传输协议伪装弥补应用协议错误。

首期不新增 sessionId 派生规则或跨请求会话状态。客户端能够原样传递的原生会话字段遵循现有控制与资源策略；只有实际接口证据证明必须由代理补值时，才在 provider 中加入最小生成规则，不赋予它选路或资源 owner 语义。

### raw、转换与资源准备

Gemini 内层请求以规范化 raw object 为基底，仅修改路由或已明确的方言映射字段；未知字段、联合类型、数字表示和原生 signature 保留。不得整体 typed marshal；safetySettings、maxOutputTokens 等仍交给上游解释，不能为了发送成功而删除。

Chat、Claude、Responses 分别直接转换到 Antigravity 请求。共享明确的叶子映射，例如 role、text、inlineData、functionCall/functionResponse；不增加覆盖所有协议的通用消息 IR。`pre_add` 在既有位置应用，同一纯转换函数处理有效请求并生成修复结果，assessment 不重复实现其规则。

Claude 入口当前媒体准备可能产生 typed 副本。实施时须把已批准的媒体转换表示为受控 raw patch，让最终转换消费同一有效请求；不得忽略准备结果，也不得以 typed 重组覆盖未知字段。

工具名称、ID、调用结果对应关系由请求级转换上下文维护；不按函数名称偷偷改为内置搜索。同一消息的文本、thinking 和工具调用都必须保留；工具结果的图片、文本、空值及并行调用归属不能被扁平化为一段成功文本。只在无法保持目标协议所需关联时拒绝。

thinking 的计算强度与可见摘要分别处理，保留客户端显式选择；原生字段直接传递，跨协议只实现已经承诺且有依据的映射。不能因为设置了 effort 就默认打开摘要，也不把可见 reasoning 当作签名。这里不新增独立思考策略系统，无法等价表达的选项按既有可表示性边界处理。

schema 作为请求数据传递，不预先引入通用验证、递归解引用或关键词清洗机制。目标支持原样 JSON schema 时直接保留；必须改用另一种表示时，只实现该差异所需的等价转换。公共 Gemini 的 parametersJsonSchema/responseJsonSchema 支持不能直接套用到内部接口。oneOf 改 anyOf、删除 additionalProperties、把约束移到 description 均不能视为等价修复。

### 接口请求修复

修复与转换共同放在 provider 的纯函数中，不新增修复框架、规则配置层或失败重试链。每条规则只保留触发条件、最小字段变化及前后样本；不匹配的请求保持原样。规则需幂等，并与协议测试一同维护。

可以修复的是有明确语义依据的表示差异，例如将已确认等价的字段别名规范化为目标拼写、调整工具调用与结果的协议角色，或通过可逆映射满足目标工具名称格式。修复必须保留参数值、内容、顺序和调用关联；字段冲突或关系不明时不猜测。

不通过注入身份 prompt、强制 sampling/tool choice、删除 thinking 历史、补空 user、伪造签名或换模型来消除上游报错。只有目标协议真实需要的缺省值才在对应字段缺失时补入，不能覆盖客户端显式值。上游已收到请求后，修复不能触发第二次推理；新的接口差异通过后续代码与样本修正。

输出 codec 同样只生成实际存在的字段：无工具调用时省略 Chat delta.tool_calls；同一 chunk 的 reasoning 和正文分别按顺序编码，不用互斥分支丢掉其中一项。

### 签名往返与会话限制

thoughtSignature 是上游不透明数据。优先使用协议已有字段往返，保留工具 part、独立 part 的签名与位置；本地不检查其算法、可信性或跨模型有效性，也不伪造 sentinel。

只有目标协议不能直接保留必要的内容顺序、part 边界或签名关联时，才使用一个版本化 opaque carrier，例如 `ag1.<base64url payload>`。这是 one-hub 新增的客户端可见扩展，使用它的跨协议路径不能宣称完全透明或兼容所有标准客户端。它只携带实际存在的原生签名和对应 item/call/part 的必要关联；无签名时也可只携带位置、文本长度和调用关联以恢复布局。实际样本证明仍无法无损重建时，才附带最小原始片段，不预存整轮历史或可从下游重建的内容。carrier 不含 token、subject、project 等渠道秘密，也不作为身份、资源 owner 或选路授权。

codec 只校验代理自有格式的版本、大小和关联结构。无法关联、多个表示相互冲突或必需的转换材料缺失时明确失败；原生签名与内容是否被上游接受仍由上游判断。carrier 编码、解码及恢复内容计入同一转换预算，不另设与实际资源消耗无关的签名长度或 parts 限额。

carrier 按不可信客户端输入处理，由对应 adapter 的纯转换函数在评估时解码。若确需恢复内容，它与外层内容共同接受现有媒体、内容策略和容量检查；载荷只用于内容与关联，不能覆盖 model、project、认证或路由控制。无需新增签名密钥、结果票据或服务端签名副本。

| 客户端协议 | 允许的往返方式 |
| --- | --- |
| Gemini | 原生 thoughtSignature 与对应 parts 原样携回 |
| Claude | 真实 thinking 使用 signature/signature_delta；工具签名通过明确的 `tool_use.signature` 扩展携带同一 carrier，以 tool_use ID 关联。原生独立签名若能关联现有真实 block，一并编码其原始位置；不得凭空生成空 thinking |
| Responses | 在 reasoning item 的 encrypted_content 中携带同一 carrier，可包含多个 part 的签名及关联。无可见 thinking 但存在真实上游签名时，明确将该 item 作为 provider continuation state；summary 为空仅表示没有可见摘要，不伪造推理文本，也不声称是 OpenAI 原生密文 |
| Chat | 实际需要额外签名或布局状态时，在 assistant message 的 `extra_content.antigravity_state` 携带同一 carrier；下一轮客户端保留它和对应历史。普通字段可完整重建时省略，不把 reasoning_content 当成签名替代物 |

Chat 不新增模式开关或协商请求头。非流式通过 message 返回实际产生的 carrier；流式在相关 turn 完成前通过 `delta.extra_content.antigravity_state` 返回完整载荷。响应和下一轮请求通过前述通用扩展保留机制传递这些字段，指定客户端须通过实际回传测试；仅能读取普通 Chat 字段的客户端可能无法完整恢复需要签名或布局信息的历史。

本地扩展识别由 `providers` 请求评估入口统一执行，先于候选自己的字段校验，消费与该候选相同的有效 raw 请求。在既有 factory 注册边界增加最小的纯识别函数及支持格式声明；共享层只根据识别结果检查候选能否接收，具体位置、编码、签名和 part 关联由格式所属 adapter 解释。这样普通 OpenAI 等候选也会排除无法处理的本地材料，relay 和各个其他 factory 无需认识 ag1 或 Antigravity 字段。不增加运行时插件系统、模型目录或状态存储。

Antigravity 的识别规则是：Chat 专属 antigravity_state 字段一旦出现即为本地材料；Claude/Responses 只在上表指定位置，以完整的保留前缀 `ag<十进制版本号>.` 识别，不能按任意位置的子串猜测。只有 v1 可解码，未知版本、损坏载荷明确失败，不能进入不支持的候选，包括 exact-wire。普通原生 signature 不受本地编码约束。权限、分组、显式 pin 与资源 owner 规则保持原有优先级。

Claude 工具的签名如果晚于工具片段到达，转换器在关联确定前有界缓冲该 block，再输出包含 signature 的 tool_use。Responses/Chat 也须先输出完整 carrier 再完成该 turn。本地无法重建必要目标内容时明确失败；仅因请求未携带 carrier，不能推断客户端丢失签名或整个模型不可用。可以完整表达的历史照常发送，由上游判断有效性。完全无法关联真实 Claude block 的签名形态显式不支持，不生成虚构 block。

`providers/claude/type.go` 已有 Signature 字段，CLIProxyAPI 也输出 tool_use.signature，这些只证明有可参考的本地表达方式。必须通过指定客户端实际回传测试，才能声明该客户端支持相应扩展；严格丢弃未知字段的 SDK 不在此保证内。

默认使用客户端携回材料，不建立签名缓存或 reasoning replay ledger；必需客户端的例外按前述验收条件单独决策。现有 affinity 只能提高同渠道命中率，不能证明签名可跨账号、项目或模型使用；客户端需要固定渠道时使用既有显式 pin。管理员编辑身份后历史可能失效，上游签名拒绝原样映射为错误，不自动换账号或清历史。上游返回超出已声明转换范围时，按真实 usage 关闭并报告已执行后的适配失败，不能借此返回不完整的成功响应。

## 流式交付、错误与计费

上游流复用 `common/requester` 的 LongStream profile、SSE framing、有界行读取及取消机制。原生非流式响应复用现有有界 raw JSON 读取，仅用只读投影提取证据，不以 typed 对象重写内层 response。Antigravity decoder 只解释自身外层与 Gemini 响应；不能把 Responses/Realtime 的事件名或 terminal alias 引入这里。

一个 candidate 的 finishReason 不代表全部 candidate 已完成。观察预期候选完成情况，并继续读取合法尾随 usage/扩展；正常 EOF 与满足操作终态一起才能构成成功。`promptFeedback.blockReason` 等明确的整请求拒绝也可以在零 candidates 时构成合法上游终态，不能误判为空流或网关 502；Gemini 保留原始拒绝，跨协议采用明确的 refusal/finish_reason 映射并保留 usage，不伪造拒绝文本。空流、截断、尾随 error、超时和客户端写入失败保留其真实失败语义，不能合成成功完成事件。

现有行上限不等于完整 SSE event 上限：decoder 使用已有 `NewSSEEventFramer(16 << 20)` 限制多行 data 的完整事件。请求沿用既有 body 上限；已准备请求、carrier 展开与非流式聚合各自受 16 MiB 转换保留预算约束，内容副本与关联索引均计入各阶段预算，禁止解码放大或重复复制绕过限制。Chat/Claude 流式只保留未完成片段和必要关联；Responses 为生成标准完成事件中的完整 output 快照，须保留本次输出，受同一 16 MiB 聚合预算约束。两者都不产生跨请求历史状态。

不预设 8 candidates 或 4096 parts 等协议准入上限；只有具体分配或算法无法被字节预算约束时，才增加针对该资源的数量限制。原生 raw 路径不承担跨协议聚合器的限制。达到实际资源上限后关闭上游并报告交付/适配错误，保留此前合法 usage，不启动补偿请求；发送前已知超限则在本地失败。

上述 16 MiB 是事件字节和转换保留数据的边界，不是 Go 进程的硬堆配额。标准 JSON 解码的临时对象、slice/map 和序列化副本另有有界开销；尚未进行高并发、极密小对象输入的内存压测，不能据此承诺每个请求只占 16 MiB 内存。若该类真实负载成为容量瓶颈，应优先逐项解码，避免增加与协议无关的 parts 数量白名单。

非 2xx 与 SSE error 由 provider 提取原始 status、Google code、message、retry metadata 和执行事实。Gemini 同方言错误保留可表达的原始 Google 错误内容；Claude/OpenAI 按显式规则映射，保留可公开的关联 ID。成功内部 envelope 不向客户端泄露代理填入的账号配置。

错误观察先于对客脱敏，使用实际请求的凭据快照接入现有 `common/providerresponse`；只在现有诊断字段范围处理 token、subject/project 等，不扩大全正文扫描，不因脱敏失败修改执行事实或触发重试。HTTP 响应头继续按现有公开策略过滤，详见[上游响应脱敏与统一交付](./provider-response-redaction-design.md)。

### 一次提交

`RelayHandler` 的现有顺序是 `ApplyReserve → ClaimSubmission → relay.send → CloseFromProviderResult`。Claim 成功后本次不再进入外层候选重试，哪怕随后得到 UpstreamNotAttempted；HTTP 写出后断连为 Ambiguous，成功状态后的解析/转换失败仍为 Accepted。requester 已承担禁隐式 POST replay 的传输规则，Antigravity 必须沿用，不能自建自动重试 HTTP 客户端。

401、429、5xx、签名错误、空响应以及客户端尚未收到首段内容，都不构成重放授权。健康/限流证据仅影响后续独立请求。project onboarding 也不是只读 observation；fetchAvailableModels/loadCodeAssist 只有在控制调用契约成立时才使用相应传输策略，不能因使用 POST 或名称含 load 就推断无副作用。

限流继续由现有 relay 观察并写入渠道加实际映射模型的冷却状态；provider 只提取状态与 retry metadata。不因模型共有 Gemini 前缀扩大冻结范围，不新增图片例外桶、账号配额 owner 或 credits 自动回退。

### 用量证据

保留 upstream usageMetadata 的字段 presence，再生成既有 Billing Evidence。候选的 usage 语义是：input 为 promptTokenCount 加明确报告的 toolUsePromptTokenCount，output 为 candidatesTokenCount 加 thoughtsTokenCount；reasoning、cache 与媒体是对应分区，不再次叠加收费。cachedContentTokenCount 不应在生成统一总 input 时无条件扣除。

实现依据参考项目共同使用的 Gemini usage 字段约定，复用既有 Gemini usage 叶子转换；模拟测试覆盖 presence、缓存/思考分区、累计快照和冲突失效。真实 Antigravity 样本，尤其是托管 Claude 的计量口径，仍属于发布前联调项目；模型品牌不能证明字段语义。若真实证据不同，应在 provider 内解释该差异，不能修改通用计费以适配一个供应商。

对客 usage 与内部 Billing Evidence 从同一原始证据派生，分别遵循目标协议的字段口径；例如 Claude 的非缓存 input 与 OpenAI 总 input 不能直接互换。未报告的用量不能因输出字段需要数字而升级为精确零。

原生响应的 usage 投影只服务于观察和结算；字段未知或不能形成计价证据时，按现有 reducer 处理，不能仅因此改写响应或中止本来可透明交付的数据。

累计 usage 采用有来源的快照更新，不逐 chunk 相加。缺失字段、冲突总量和部分价格组件沿用现有 reducer，不把本地 token 估算、成功状态、文本长度或零值补齐标为 ProviderReported。正常、错误和断连均由同一个 AttemptQuota 关闭：有可计价组件则 Confirm，无合格 usage 则 Cancel；适配器不自行补扣或退款。尾随证据只在既有 closure cut 前参与结算。

## 模型发现与状态规模

吸收 CLIProxyAPI 的账号模型发现能力，Antigravity 实现现有 `providers/base.ModelListInterface`，复用 `/api/channel/provider_models_list`、`controller/channel-model.go` 与 ModelSelectorModal 的获取、选择、保存流程，不另建发现 API、界面或能力目录。使用 fetchAvailableModels 返回候选模型名，管理员通过现有渠道编辑更新 Models/ModelMapping；不在推理热路径查询，不自动发布售价或修改模型归属。失败保留已有配置，返回本次发现失败，不伪装成空目录。

模型发现按凭据是否已保存区分：省略 key、按渠道 ID 查询时读取数据库最新凭据并遵守 fence；显式提交整份凭据时，先用同一工厂规则规范化并与数据库比较。相同凭据仍走持久化刷新，不同凭据清除运行时渠道 ID、作为未保存草稿，不能借已有 ID 刷新或写回。草稿仅使用其仍有效的 access token，过期时提示保存后刷新或重新授权；发现操作不隐式保存表单中的连接配置。模型发现入口与创建入口共用 JSON 凭据形态识别，不能只取 pretty JSON 的第一行。

首期不保存 discovery 缓存或新 tier/quota 状态：返回结果可供本次管理操作展示，账号层级不自动决定端点、客户价格或当前请求的切换策略。公开模型可见性仍由既有渠道分组、协议资格和模型配置决定，目录不成为上游模型白名单。

| 状态 | 唯一事实来源与作用域 | 生命周期、容量与失败 |
| --- | --- | --- |
| 凭据与刷新 fence | 渠道数据库行 | 每渠道一份 key、至多一个未决 fence；按既有提交/恢复终止，无 TTL 解锁 |
| OAuth 会话 | 现有一次性缓存，key 按管理员和随机 state 隔离 | 10 分钟、单条 4 KiB，复用现有缓存总容量及 start 的 IP 限速；独立消费，丢失重新授权 |
| 本地刷新协调 | 进程内渠道互斥 | 仅减少竞争，有界回收；进程丢失不改变 DB fence |
| 路由快照/affinity | 既有路由缓存和 affinity manager | 沿用现有容量、TTL、失效和权限作用域，不升级为账号/签名 owner |
| 已准备请求、流式/非流式转换与 carrier codec | 当前请求；跨请求材料由客户端携回 | 按上述字节预算有界保留，请求结束释放，无服务端签名副本 |

### 消融后的最小设计

| 机制 | 移除后的实际影响 | 取舍 |
| --- | --- | --- |
| 模型验证名单、Chat 模式协商、额外 sessionId 派生 | 不再提前预测某模型或客户端是否接受请求 | 删除；按实际转换需要判断，模型参数与签名有效性交给上游 |
| 匿名 callback、OAuth 结果缓存、自动模型目录持久化 | 少自动回填和后台探测便利 | 不引入；现有管理操作与渠道配置已覆盖目标 |
| 签名缓存与 replay ledger | 部分丢弃签名材料的客户端无法继续多轮请求，这是实际兼容性损失 | 默认不引入；必需客户端确有阻断时按扩展条件评估最小恢复，不能声称无损消融 |
| 账号池、credits fallback | 不自动换账号或切换付费路径 | 本轮不引入；这些功能需要额外产品契约与状态 owner |
| 多跳转换、重复验证、schema 通用清洗 | 失去复用旧转换链和通过删字段提高成功率的便利 | 共用直接转换函数，修复只针对有依据的表示差异 |
| 跨层 Plan 契约与缓存 | 选定后可能重复一次纯构造计算 | 不引入；只有有必要复用的数据进入已有请求准备流程 |
| 原生 JSON 路径或 raw ingress | 未知响应字段无法可靠聚合，未来请求联合类型可能提前失败 | 保留；透明转发依赖这两个边界 |
| 凭据 fence、一次提交、流式终态及 usage presence | 可能覆盖新凭据、重复执行、截断伪成功或错误结算 | 保留并复用现有实现 |

消融检查以同一输入比较字段语义、调用顺序、上游 dispatch 次数和保留状态，不以“返回 200”单独判断修复有效。例如 oneOf 的两个数字分支分别要求值 ≥0、≤10 时，输入 5 应被拒绝，改成 anyOf 后却会通过；删除 additionalProperties:false 同样会放行原本被拒绝的额外属性。这些规则改变了请求语义，不能用兼容性修复解释。

## 实施批次与完成条件

| 批次 | 主要文件/职责 | 完成条件 |
| --- | --- | --- |
| 1. 固定协议证据与转换边界 | `providers/antigravity/wire/testdata`、纯转换函数、必要方言差异 | 确定验收客户端及必需操作，先验证两轮工具与签名；固定 OAuth/project/生成/usage 样本及必要 envelope 映射；不生成模型准入名单 |
| 2. 渠道与凭据控制面 | factory、OAuth controller/provider、`common/{authutil,credentials}`、渠道管理与前端 | 凭据输入形态、PKCE/回调解析、刷新互斥由 Codex 与 Antigravity 共享；复用 state 消费与 DB fence；授权/project 初始化通过，无新增 DDL |
| 3. 共享资格边界与 Gemini | `providers/base`、`providers/capabilities.go`、通用 raw/wire 类型、Claude/Gemini relay 与既有 provider | 既有渠道资格不回退；真实 ingress 的 future union/raw 通过；扩展字段保留不依赖 Antigravity DTO；错误、流式与计费闭环通过 |
| 4. Claude 与 Chat 直接转换 | Antigravity 纯转换、最小 carrier codec、providers 统一扩展识别 | 四入口矩阵中的对应项目通过；其他候选不会转发本地 carrier；混合内容、工具媒体与签名保持；修复不产生额外上游调用 |
| 5. Responses 执行边界与直接转换 | `providers/base/capability.go`、`providers/capabilities.go`、`relay/{capability_gate,responses}.go`、Antigravity Responses | direct/chat_adapter 选择唯一；原有 OpenAI/Codex/fallback 行为保持；仅无状态支持面注册 |
| 6. 模型发现与发布验证 | 现有 ModelListInterface/模型选择流程、集成测试与操作说明 | 目录/路由/售价分离；具名模型×客户端交付矩阵、故障注入、真实上游和部署验证完成 |

批次用于组织四入口目标的实现依赖，不引入长期 feature flag 或新旧双实现。可选能力在扩展条件未成立前不进入批次，也不作为接入完成的前置任务。每批只声明已经满足契约的操作；测试中的具名模型不限制其他可直接表达的请求。完整交付必须通过最低交付用例，不能通过删除难例把四入口目标改写成已完成。

## 验收矩阵

| 边界 | 必须验证的可观察结果 |
| --- | --- |
| OAuth | 公共 PKCE/回调解析不改变 Codex 输入契约；错误管理员/state/redirect/重复参数/过期或二次消费不发 token 请求；并行授权互不消费；交换歧义不自动重放；恢复使用服务端快照；project 失败不能发布渠道 |
| 刷新 | 公共互斥可取消、引用回收且不替代 DB claim；DB 不可写时 OAuth dispatch 为零；两实例只有一个 claim 胜者；无新 refresh token 时保留旧值；超时、解析失败、崩溃、commit 回执丢失保持 fence |
| 渠道编辑 | 保存确认/取消行为符合现行产品；原 ID 修改；旧 callback、旧 refresh ticket、删除/类型变更/版本冲突不能覆盖新配置；同账号恢复不能变成换账号 |
| 候选能力 | 评估与执行调用同一纯转换函数且使用相同有效请求；不可表示且 token 已过期时，refresh 和推理 dispatch 均为零；统一识别覆盖非 Antigravity 和 exact-wire 候选、未知版本与损坏载荷；pin/owner 规则保持 |
| 共享数据契约 | 原样交付或等价 raw 子树中的未知扩展在 Chat message/delta 及工具对象往返保留；修改已知字段不吞未知值或显式 null；整体凭据在创建/模型发现中不按行拆分；Responses 执行目标由同一 resolver 派生且无独立配置 |
| 原生 Gemini | 经真实 ingress 验证未知字段、未来联合类型、大整数、schema、signature 和扩展事件保留；未收录但已由管理员配置的新模型可进入上游；typed 投影失败或未经本地校验的上游参数不造成额外拒绝 |
| 跨协议 | 可直接承载的 raw 子树不因未知嵌套字段被拒绝；确实不可表示的字段、工具、媒体和历史在 provider work 前失败；Response 存储默认语义不被改写 |
| 范围与必要映射 | 对已支持请求验证真实所需的 requestType；不因未做跨协议搜索或 Chat 图片输出而清除原生 Gemini 可表达字段；未接入的操作明确失败，不伪造计数、压缩或生命周期结果 |
| schema 与修复 | oneOf/anyOf 重叠分支、additionalProperties:false、const、非字符串 enum、$ref/$defs 保留或明确不可表示；不校验用户参数是否满足 schema；修复条件外请求不变、重复执行幂等、没有修复后重试 |
| 混合内容与工具 | text + tool_use、同帧 reasoning + text 均保留；纯图/图文/空工具结果不伪造成功文本；并行结果保持归属；尾部 reasoning-only 不静默删除或补空轮次；无 tool_calls 时省略字段 |
| thinking | 已承诺的计算强度与摘要可见性分别映射；保留显式关闭/开启及省略语义；不因 effort 自动启用摘要，不将可见文本作为签名替代物 |
| 签名 | 指定客户端完成单轮、多轮、并行工具、迟到和独立签名；仅检查本地 carrier 结构、关联与预算；原生签名有效性由上游判定；恢复不能绕过现有策略或覆盖控制字段；缺少必要转换材料时明确失败 |
| 流式 | 多行 SSE、CR/LF、分段 UTF-8、多 candidates、零 candidate 的 prompt block、尾随 usage/error、空流、截断、超限和客户端断连；完整 carrier 先于成功终态；预算内不因固定候选或 parts 数量拒绝 |
| 执行事实 | 写前失败、写后断连、已接受后解析失败、401/429/5xx/空内容均不产生第二次推理；保留原始错误观察与对客映射 |
| 计费与冷却 | input/cache/thinking 不双算；对客字段与计费来源一致；累计快照不累加，缺失/显式零/冲突区分；有 usage 的失败仍结算，无 usage 的成功取消预扣；模型 A 的 429 不无依据冻结模型 B |
| 安全与容量 | OAuth Bearer 不跨 origin 重定向；管理 UI 不记录或以 HTML/跨窗口消息暴露凭据；错误脱敏沿用既有范围；多行 event、carrier 展开、索引、聚合、锁与 state 均有界 |
| 既有系统 | Responses direct/Chat fallback、Claude/Vertex/Bedrock/Gemini/Custom 资格、渠道编辑 fence 和 TCC 回归测试通过 |
| 持久化 | 已有 Schema 可完成新建/刷新/恢复；不新增表、列、索引或专属迁移，不重写旧渠道；新类型编号不改变既有值，旧节点不能误读新 key 格式 |

实现验证以相关 provider、relay、requester、credentials、model/controller 的行为测试为主；共享 contract 修改需运行覆盖已有 provider 的回归测试和适用 race 检查。前端需验证新建、编辑、恢复与确认取消，文档/纯格式修改不新增镜像实现的单测。

## 上线、回滚与尚未验证的事实

本方案无新增 DDL，只有现有 Type 与 Key 的新持久化契约；启动及部署沿用前述数据库变更边界。新增公共辅助函数、raw 保留和执行解析均为代码变更，不要求数据迁移或全库重写。

前后端及所有服务节点使用同一支持版本后，才允许创建和启用 Antigravity 渠道。旧二进制不认识新渠道类型，不能在仍有此类渠道可被选中时滚动回退。上线先使用明确 pin 的验证渠道，完成真实授权、普通/流式生成、工具历史和结算检查后再进入普通分组。

回滚前先停用 Antigravity、停止其管理 OAuth 操作并排空在途请求与结算，保持已产生的凭据和 fence 原样；确认旧版本的未知渠道加载行为，必要时隔离这些记录再恢复旧节点。不能通过清 fence、重用备份 refresh token 或恢复整库来伪装回滚；恢复服务后的账务与凭据变化不能被旧备份覆盖。

当前仍需验证：OAuth client/redirect/PKCE 的实际接受情况、稳定 subject 与 project 响应、onboarding 的 operation 观察契约、所选端点的原生非流式支持与 SSE 转换覆盖、不同模型的签名关联和 usageMetadata 含义，以及指定客户端是否实际保存并回传 carrier。只有源码参考和 mock 测试不能证明这些外部事实；未通过验证的交付用例保持未完成，不能宣称全模型、全协议或所有客户端兼容。
