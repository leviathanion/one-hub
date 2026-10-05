---
title: "Relay 性能优化方案：请求处理、代理连接隔离与账号负载"
layout: doc
outline: deep
lastUpdated: true
---

# Relay 性能优化方案：请求处理、代理连接隔离与账号负载

## 文档状态

- 状态：P0 出口隔离与 P1 两处复制优化已实现，并按功能拆分提交；包含 2026-10-04 的源码核对、消融实验和实现回归。账号调度等候选仍暂缓，未部署到真实上游。
- 适用范围：优先覆盖 Codex 订阅凭证到 HTTP Responses 的请求链路；共享机制兼顾 OpenAI same-dialect / exact-wire。WS 只沿用现有 native 传输和生命周期边界。
- 文档口径：区分已确认问题、预期收益和条件性扩展；不承诺线上提升比例，不把局部微基准当作真实上游压测。
- 决策记录：遵守当前 `AGENTS.md`、ADR-0003、ADR-0019、ADR-0030、ADR-0034、ADR-0035，以及当前 Responses、Codex wire、credential fence、channel affinity 与 shutdown 架构。

## 1. 结论与基线

原分析总体合理，但实施范围应缩小：当前确定实施的是代理出口隔离和有直接消融证据的请求处理改动；账号调度、完整 raw visitor/偏移编辑及到期堆先作为观测候选，不列入已确定的实施批次。连接复用、prompt-cache 软亲和、严格 continuation/owner 绑定、native WS、事件级 flush、凭据刷新并发控制均已有实现，不列为新增功能。

本次实际核对的版本：

| 项目 | 提交 | 说明 |
| --- | --- | --- |
| one-hub | `34a3099e6fa77388de168132d81ab061d0582be8` | 本次实现前的基线；原分析基于 `48d78274`。相关解析、SOCKS 拨号、静态权重和刷新主逻辑仍存在 |
| CLIProxyAPI | `8ef43e4df3b216a42493105d31c2873b69191473` | 克隆后固定到原分析提交，未用滚动 main 代替 |
| sub2api | `b8dece9000c68815a5b867ca5a1e6f236e173905` | 克隆后固定到原分析提交 |

当前 one-hub 相比原基线还包含业务优雅停机与 Responses compact/input_tokens raw-wire 修正。方案必须覆盖这些新边界，不能按旧提交实现后覆盖掉它们。

| 原判断 | 本次结论 | 必要修正 |
| --- | --- | --- |
| 大请求存在重复解析、整包对象化和复制 | 成立；微基准显示明显累计分配 | `jsonobject.Parse` 自身还先用 Token 完整扫描深度，再解析顶层字段；typed projection 失败会再次 core decode。不能只优化最后一次 Clone |
| SOCKS 共享池可能串出口 | 已在本地真实 TCP/TLS 链路复现 | P0 正确性修复；复核还确认当前 Go 1.27.1 的 HTTP CONNECT/H2 共享池也串出口，HTTP/1.1 CONNECT 对照正常 |
| 亲和上补账号负载感知 | 有条件成立 | 属于选路/准入策略变更，不是无语义影响的性能补丁；先定义身份、容量、生命周期和单机范围 |
| 保留缓存、Redis 锁、SQL rotation fence | 表述不准确 | 持久化 Codex 渠道的唯一刷新正确性来源是 SQL durable fence/revision；Redis 锁和旧 journal 不参与该路径，不能重新引入为必要条件 |
| 到期堆减少刷新扫描 | 条件性成立 | 当前已有 8 个共享后台 worker 槽位、15 分钟调度与扫描游标；仅在扫描成本被测出后替换调度机制 |
| sub2api 高级调度、CPA uTLS 可直接作为整体模板 | 不适合整体移植 | 只参考处理机制；前者高级调度由设置启用，后者该 Codex uTLS 路径每请求独立建连并随 Body 关闭 |

Claude OAuth 字段政策、工具别名、请求清洗、TLS 指纹和伪装策略均不进入本方案。one-hub 的 Claude provider 当前使用 API Key；可借鉴的是底层处理算法。

## 2. 已执行的验证及其边界

### 2.1 代理出口隔离复现与最小方案反证

实验使用当前 one-hub 的 `requester.InitHttpClient`、`NewHTTPRequester` 和实际 SOCKS 拨号逻辑，设置两个本地 SOCKS5 服务 A/B，以及一个受信任证书的 `httptest` TLS 上游。请求使用非空 POST body，读取完整响应并关闭 Body；上游通过 TCP 对端识别实际出口。没有调用真实供应商，也没有使用生产凭据。

只为测试证书信任克隆 Transport；共享池组仍让所有 requester 共用同一 Transport。分别确认协商为 HTTP/1.1 与 HTTP/2.0。

| 连接配置 | 依次指定的出口 | 上游实际观察 | 结果 |
| --- | --- | --- | --- |
| 共享 Transport | A → A → B → 直连 | A → A → A → A | 两种 HTTP 协议都复现串出口；B 没有收到 SOCKS CONNECT |
| 共享 Transport | 直连 → A → B | 直连 → 直连 → 直连 | SOCKS 配置被已有直连绕过 |
| 按出口独立 Transport 的对照组 | A → A → B → 直连 → B | 与指定出口一致 | 同出口重复请求仍有 `GotConn.Reused=true` |

SOCKS 的一条直接原因是地址只在 `DialContext` 中读取 context，命中已有连接时不会拨号。复核又测试了两个更小的候选：让标准 `Transport.Proxy` 直接返回 SOCKS URL，以及当前已有的标准 HTTP CONNECT 路径。结果如下：

| 模式 | HTTP/1.1 | HTTP/2.0 | 结论 |
| --- | --- | --- | --- |
| 当前共享池 + context SOCKS | 串出口 | 串出口 | 需要修复 |
| 共享池 + 标准 Proxy SOCKS + 普通 TCP dialer | 隔离正确 | 仍串出口 | 单改 Proxy 不是当前工具链上的完整修复 |
| 当前共享池 + HTTP CONNECT | 隔离正确 | 串出口 | HTTP/1 的 proxy key 不能证明 HTTP/2 安全 |
| 按实际出口固定 Transport | 隔离正确且同出口复用 | 隔离正确且同出口复用 | 保留为最小正确性方向 |

CONNECT/H2 中 A → A → B → 直连实际仍走 A；直连 → A → B 也全部复用直连。正确性 review 独立重跑了该矩阵。新的矩阵脚本对已知缺陷、实际协商协议及正确控制组均有断言，条件不满足会非零退出，不只打印结果。

Go 1.27.1 的 HTTP/1 `connectMethod` 包含 proxy URL，但 H2 快路径在普通建连之前按目标 authority 查询连接池，没有把此次 proxy 配置作为相同作用域的 key，因此可能连 B 的 CONNECT 都不发生。这是当前工具链和 fixture 的实测结论，不泛化到所有 Go 版本。它证明需要出口隔离，不证明需要原方案的独立退休状态机；第 4 节已收敛为单一有界缓存。

### 2.2 大请求分配基线

使用 Go 1.27.1、linux/amd64，在当前代码上通过 Go overlay 加载临时 benchmark；没有更改运行时代码。输入为约 256 KiB、1 MiB、4 MiB 的 ASCII 长文本 `input`，并带 model/stream/store 和一个含大整数的未知字段。每组运行三次，`-benchmem -benchtime=100ms`。以下为 1 MiB 场景中位数的近似值：

| 测量阶段 | 累计分配 B/op | 耗时 ns/op 换算 | 范围 |
| --- | --- | --- | --- |
| `jsonobject.Parse` | 12.03 MiB | 7.69 ms | 深度扫描、字段解析、原文与字段复制 |
| `ParseRawEnvelope` | 17.03 MiB | 10.33 ms | **已包含上一行**，不能相加 |
| 资源引用提取 | 5.00 MiB | 3.19 ms | 复现当前整包 map decode，再调用 `ProtocolResourceReferences`；不含授权 SQL |
| Codex body planner | 3.03 MiB | 1.19 ms | 已解析 Object 作为输入；测 Clone、patch 与 marshal |

后面三个独立阶段的累计分配约 25 MiB，只用于说明这条局部处理链的分配放大。它不是峰值常驻内存、完整请求总成本，也不能用来推断并发吞吐或线上 TTFT；其中还没有计算 ingress、分词、SQL、网络和结算开销。短运行中的时间数据仅作诊断，正式比较需要更长、多轮成对 benchmark。

### 2.3 方案评估阶段的回归验证

在实现前运行并通过：

- `go test -count=1 ./common/jsonobject ./common/responses ./common/requester ./providers/codex/wire`。
- relay 中选取的资源授权、用户隔离、opaque ID、数值词法保留、continuation、owner 优先级、SSE 尾部与 WS terminal 后关闭交付测试。
- 上述 SOCKS/TLS 诊断与局部 benchmark。

该阶段未运行完整项目测试、生产实现的优化前后对照、真实供应商并发压测或分布式调度测试。下一节记录实现前的候选消融，实际实现结果见 §2.5。

### 2.4 消融证据与收益边界

复核用 Go overlay 分别移除两个明确的成本：A = RawMessage decode 后的第二次字段复制；B = Codex body planner 对马上失效的整包 Raw 的复制。B 仍深拷贝 Fields/Order，不改变通用 Clone 契约；A 继续使用 decoder 已拥有的 RawMessage，不借用调用方输入。两项均保留完整 JSON、深度、未知字段和字段验证。

下表是每组 200ms × 3 次中位数，单位为累计 MiB/op。Chain 是实际组合执行 `ParseRawEnvelope → 资源提取 → Codex body planner`，不含网络、SQL 授权、分词、计费等其他环节。

| 约 1 MiB 的输入形状 | 原 Chain | 仅 A | 仅 B | A+B |
| --- | --- | --- | --- | --- |
| 长文本 input | 25.059 | 24.052 | 24.053 | 23.044 |
| 图片 data URL + 文件引用 | 25.087 | 24.079 | 24.079 | 23.071 |
| 大工具 schema | 26.084 | 25.076 | 25.076 | 24.066 |
| 未知工具联合形状，触发 core projection fallback | 28.071 | 27.063 | 27.063 | 26.054 |

组合 overlay 上现有 jsonobject/responses/wire 三包测试通过，额外检查 planner 不改原 Object、Parse 不借用调用方原文。它们没有接入新的授权路径，也不代表生产实现已完成。

另一个候选曾直接复用 `Projection.Input/Conversation`，只单独 decode raw tools。它在普通文本 fixture 上得到约 18.041 MiB/op，但已被授权反例否决，不能列作可采用的收益。合法输入 `{"model":"gpt-5","input":[{"type":"input_file","file_id":"file_restricted"}],"INPUT":"opaque extension"}` 中，现有 full-map 资源提取仍会发现 `file_restricted`，typed projection 却被大小写宽松匹配覆盖，复用后没有资源引用。`conversation/Conversation` 也会覆盖身份；只有大写 `INPUT` 的未知扩展还可能被错误解释为资源。

根因是 struct JSON 解码的大小写宽松匹配与原资源 visitor 的精确字段名规则不同。原本几个 fixture 的等价测试漏掉了这个差异；复核新增 11 个差分 case，包括 8 个授权差异和 3 个反序对照，覆盖 Unicode 大小写折叠、fallback 与仅有未知扩展的输入。测试通过的含义是成功证实候选不等价，并确认未知字段仍被接受/保留，不是候选获得准入。这项收益已撤回，不能靠拒绝大写未知扩展修复，也不能用“性能更快”放宽授权。未来如继续研究，应从当前原始请求的精确协议槽位获得授权输入，先证明字段名/重复嵌套键/顺序等语义等价，再独立测量。

短请求额外按 baseline→combined→combined→baseline→baseline→combined 顺序、每次 1 秒交替测量。Chain 时间区间约 31.24–33.73µs 与 31.50–33.42µs，范围重叠；分配则稳定从约 32,436 B/168 次降至 30,056 B/162 次。可以确认这些局部候选减少累计分配，不能据此确认 CPU、TTFT 或短请求性能已通过正式门槛。

另用容量为 1、直接淘汰活跃 entry 的玩具缓存验证资源口径：A→B→A 可以留下 3 个同时活跃的 Transport，HTTP/1 和 HTTP/2 SSE 尾部都能继续交付。`CloseIdleConnections` 不打断这些活跃流，但 map 容量不等于总池数量。这支持保留最小在途引用和零引用淘汰，不支持再加独立 retired 队列、reaper 或渠道编辑同步管道。

### 2.5 实际实现与复核结果

运行时代码现已采用固定代理的共享有界缓存。`HTTPRequester.Do` 与已有 Send 路径共用选池和 I/O；Codex usage/reset-credit 保留调用方自己的状态、body 限长、脱敏和已提交结果判断。请求解析只应用 A/B，资源授权仍走原 full-map 提取。

源码基线 `34a3099e` 与当前实现使用同一份仓库内 `BenchmarkRequestProcessing`（Go 1.27.1，500ms × 3）比较；基线通过两份原源文件 overlay 恢复，未替换授权逻辑。中位累计 MiB/op：

| 约 1 MiB 输入 | 原局部 create 链 | 实现后 |
| --- | --- | --- |
| 长文本 | 25.060 | 23.044 |
| 图片 data URL + 文件引用 | 25.087 | 23.070 |
| 大工具 schema | 26.076 | 24.060 |

长文本的 create/compact planner 分别从约 3.025 降到 2.017 MiB/op。交替 1 秒短请求样本，原链 31.369/33.099/33.495µs，实现后 31.024/31.166/31.493µs；本次样本没有短请求退化证据，仍不能推断真实上游 CPU、GC、P95/P99 TTFT 或端到端收益。这里的局部链不包含网络、SQL、分词和计费；新增连接池的真实本地网络成本另见 §2.6。

两位 reviewer 分别检查正确性和消融：保留拨号时读取当前 `connect_timeout`；配置错误/池满时关闭 request body；删去配置模板上不再使用的 noKeepAlive 副本。HTTP 隐式重放回归测试改为通过同一 requester 预热，并增加非空工作请求确实复用连接的断言，避免“换池后测试仍绿但不再覆盖旧连接”的假通过。

新增自动回归覆盖 SOCKS/CONNECT × H1/H2、认证变化、继承 context 后显式直连、同出口复用、池满前置拒绝、32 个池的并发容量竞争、SSE 尾部与淘汰、取消/超时/拨号失败和并发 Body.Close。JSON 回归覆盖 Parse 输入所有权、Clone 深拷贝、create/compact 成功及中途失败均不修改源对象。

当前实现的可复现命令：

```sh
go test -count=1 ./...
go test -race -count=1 ./common/requester
go test ./providers/codex/wire -run '^$' -bench '^BenchmarkRequestProcessing$' -benchmem -count=3 -benchtime=500ms
```

基线对照需把 `34a3099e` 的 `common/jsonobject/object.go` 与 `providers/codex/wire/body.go` 放入 Go overlay，保留相同 benchmark；不要在运行中的工作区切换或覆盖原文件。

最终 `go test -count=1 ./...` 与 `go test -race -count=1 ./common/requester` 通过；stored lifecycle deadline 的定向三轮及 race 检查也通过。OpenAI unary timeout 测试改为 `errors.Is(err, context.DeadlineExceeded)`，验证标准错误语义；stored deadline 测试直接观察 RoundTrip 的 request context，避免把被固定出口替换的 Proxy 回调当探针。

中途一次全量运行中，`TestBackgroundCancelAndRecoveryObserveSameSettlement` 在 SQLite `database table is locked` 后失败。原始基线和当前实现分别定向运行 30 次均通过，原始基线 relay 整包及当前实现最终全量复测均通过；目前未复现或确认其因果来源。本次未修改结算业务逻辑，也没有通过增加重试或降低断言掩盖该异常。


本次没有引入账号负载调度、到期堆、COW、字段偏移、自定义 scanner、资源投影复用或隐式重试。响应时间能否明显改善仍需受控上游压测。

### 2.6 后续双 agent 复核：请求范围的引用与传输成本

正确性复核发现，最初每个 `RoundTrip` 获取和释放出口引用，会在允许的 GET 重定向两跳之间留下空档：首跳 Body 关闭后，其他出口能占满缓存，使第二跳返回池满错误，原错误映射还会把已发送首跳误标为未尝试上游。默认工作请求不跟随重定向，这个反例没有赋予工作请求重放权限。

最终实现改为在整次 `http.Client.Do` 前获取一个引用，所有允许的重定向使用同一固定出口；最终响应 Body 关闭或调用失败时释放。回归通过真实 requester 和标准 CookieJar 在两跳之间制造确定性交错，检查其他出口被容量约束拒绝、原请求仍能完成且引用不会提前释放。取消观察绑定 `resp.Request.Context()`，因此包含 `Client.Timeout` 加入的期限；收到响应头后不 Read、不主动 Close，也必须在超时后关闭传输 Body 并释放引用。第二轮又补上引用移交前的 `defer` 清理：请求观察回调触发 panic 时仍释放槽位，panic 继续交给原有外层处理，未引入重试。

消融复核同时确认，旧的自定义 `RoundTripper` 隐藏了具体 `*http.Transport`，使 `net/http` 对带总超时的请求启用额外的兼容取消 channel、timer 和 goroutine。移除这个包装层后，仍保留同一缓存、原子获取、在途引用和取消观察。pprof 验证进入原生 context deadline 路径。没有增加无锁 LRU、代理解析缓存或后台清理状态机。

实验使用真实 TCP/TLS 的 HTTP/1.1、HTTP/2，分别测直连和 HTTP CONNECT、串行和并发，以及 work、observation、long-stream 三种既有策略。固定单出口 Transport 是成本对照，缺少多出口隔离所需的缓存和引用管理，不能替换生产方案。每组先断言实际 TLS 协议与同出口复用；串行热请求的 `newconns/op=0`。B/op 包含同进程 mock 服务端与代理，并发 ns/op 是吞吐倒数，不是每个请求的响应延迟。

初轮 400ms × 3 候选消融中，带总超时的 observation 请求较原包装实现稳定减少 H1 的 14 次、H2 的 13 次分配，每请求约 0.8–0.9 KiB；直连/CONNECT、串行/并发均成立。耗时有升有降，不能据此宣称稳定的吞吐或供应商 TTFT 改善。锁命中获取/释放的独立测量约 28ns（串行）、91ns（并发摊销），均零分配；取消观察约增加 144B/3 次分配，但承担无人读取时的资源释放，不能为了数字移除它。

最终复测冻结了修改前后源码，新快照已包含 panic 清理，使用仓库内 `BenchmarkHTTPEgressWarmReuse`。Go 1.27.1、linux/amd64、AMD EPYC 7763、`GOMAXPROCS=5`；每组 200ms × 3，依次 before→after、after→before、before→after，计时期间不并行运行测试。48 个场景的两套三轮共通过 288 个样本，144 个串行样本均无新增连接。以下为 observation 串行中位数；“修改前”指本轮简化前的固定出口包装实现，非存在串出口问题的原始代码：

| 场景 | 分配次数：修改前 → 后 | B/op：修改前 → 后 | µs/op：修改前 → 后 |
| --- | --- | --- | --- |
| H1 直连 | 105 → 91 | 8,710 → 7,862 | 115.09 → 102.83 |
| H1 CONNECT | 113 → 99 | 9,172 → 8,254 | 154.97 → 157.89 |
| H2 直连 | 102 → 89 | 9,802 → 8,950 | 145.54 → 150.36 |
| H2 CONNECT | 109 → 96 | 10,234 → 9,416 | 236.77 → 206.69 |

observation 全矩阵稳定减少 13–14 次分配、818–918 B/op；work/long-stream 多数减少 2 次分配（1 组减少 1 次）、130–252 B/op。实现后相对固定单出口对照仍多 5–9 次分配，用于解析、受限缓存和生命周期管理。耗时结果不能统一宣称改善：例如短样本中 H1 long-stream 并发摊销值从 30.48 增到 33.88µs，三轮均上升，控制组只能解释部分变化，因此继续定向追查。

随后仅对 H1 直连 long-stream 并发、H2 直连 long-stream 串行做独占 1s × 5 交替复测，并保留 Fixed 控制组：

- H1 并发的 Cached 中位数为 32.347 → 32.311µs，五轮两次变慢，原来三轮同向变慢没有持续复现。
- H2 串行的 Cached 中位数为 192.088 → 194.186µs，五轮四次变慢；逐轮变化中位数 +1.95%，范围 −1.83% 至 +4.94%。逐轮 Cached/Fixed 比值变化中位数 +6.10%，但 Fixed 是分开测量的控制组，不能把该比值当作已隔离出的请求器开销。保留小幅退化信号，不宣称已通过“流式耗时无回退”验证。
- 进一步采集 H2 串行的 CPU/block/mutex：前后 body 关闭相关的 `sync.Once` 阻塞累计约 7.85 → 8.49ms，body 相关 mutex 累计约 5.01 → 5.08ms，采样进程分别运行约 5.76/5.63s；没有定位到足以解释上述比值差异的锁热点。新旧实现正常 EOF 都会触发取消回调，新的内外层 Close 可能有不同竞争窗口，但尚未证实是耗时差异原因。带全量阻塞采样的运行本身改变时序，不用于吞吐验收。

目前可确认的是分配减少、重定向引用及异常清理正确性；响应时间收益仍未证实。没有依据为这点未归因的差异新增关闭状态机、交换原有先取消后关闭的顺序，或移除未读取响应时所需的取消观察。

work/long-stream 使用 31 字节 POST，observation 使用无 body GET；普通响应 46 字节，long-stream fixture 立即 flush 并返回 42 字节 SSE。因此后者测的是流式策略和关闭开销，长流正确性另由 SSE 生命周期测试覆盖。该实验不是长上下文压测，也没有供应商、账号池、真实首 token 或 P95/P99 数据。

可复测当前实现与固定单出口成本对照：

```sh
GOMAXPROCS=5 go test ./common/requester -run '^$' -bench '^BenchmarkHTTPEgressWarmReuse$' -benchmem -count=3 -benchtime=200ms
```

本轮两份源码快照、overlay、交替执行脚本、原始日志和 pprof 保存在本次开发环境的 `/workspace/work/review-cycle-ablation/`，未作为仓库文件提交。以上表格记录该次实验结果；仓库内 benchmark 可复测当前实现与固定单出口的成本。旧实现只作为实验快照保留，未留在生产分支或增加运行时开关。

本轮最终 `go test -count=1 ./...` 与 `go test -race -count=1 ./common/requester` 均通过。新增用例、辅助函数和 benchmark 按出口隔离、重定向、取消释放、异常清理及连接复用命名，不使用问题单编号。

## 3. 必须保持的项目契约

1. **按数据路径处理。** exact-wire 没有代理修改时保留原始字节；same-dialect 只改代理拥有的字段；cross-protocol 在 provider work 前完成必要解码和可表示性检查。未知字段和未来联合类型不因 DTO 不支持而在透传路径失败。
2. **只保留必要校验，但不能少做本地边界。** 保留完整 JSON 合法性、顶层对象、尾随 JSON 拒绝、顶层重复 key 检测、64 层深度限制、当前请求体与解压上界。现实现只拒绝顶层重复 key，不能借优化顺便扩大成新的嵌套字段校验政策。
3. **资源授权是强边界。** 只访问当前协议定义的资源位置；function arguments、工具输出中的普通业务对象、JSON schema 与未知扩展保持 opaque。资源 ID 不 trim/case-fold，不跳过跨用户或 owner 冲突检查。
4. **规则保持唯一负责层。** relay/shared policy 负责认证、选路、准入、资源 owner、计费；provider/wire planner 负责协议构造和证据提取；requester/transport 只负责 I/O、连接资源和交付事实。
5. **提交后不换渠道、不重放。** 新候选选择必须发生在现有 submission/provider work 边界之前。首 token 尚未到达、客户端未收到字节、返回 429/5xx，都不是再次执行的授权。
6. **传输资源与业务生命周期分开。** HTTP SSE 不因 `response.completed` 截断上游尾部；WS 不把空闲连接视为正在执行的 turn，也不因降负载重建已有会话或转成 HTTP bridge。
7. **计费契约不变。** 保留当前 Try/ClaimSubmission/Confirm/Cancel；仅合格 provider usage 收费。负载统计、请求体可复用性、Body.Close 都不成为新的计费事实。
8. **新增状态必须有界且能排空。** 明确唯一事实来源、作用域、释放及失败语义；接入已有 shutdown 顺序和共同预算。采用单一路径，不为性能开关引入长期 legacy/optimized 双实现。

## 4. P0：按连接配置隔离 Transport

### 4.1 落点与 key

在 `common/requester` 内让一个有界缓存负责固定出口的 TransportSet；代理解析和固定 dialer 尽量复用现有工具。requester 从明确的有效连接配置取得 transport，不能继续依赖请求 context 动态改变共享池的实际出口。不为此增加配置发布者、渠道到池的引用表或独立退休管理器。

连接选择边界必须覆盖 `providers/codex/usage_snapshot.go` 的用量 GET 和 reset-credit consume POST：它们通过 requester 构造带代理的请求，却直接调用全局 `HTTPClient.Do`，仅修改 `Send*` 会漏修。迁移只共享选池/I/O，不机械改成 `SendRequestRaw`，因为后者会提前处理并关闭非 2xx body；现有调用方自己的 status、结构化 body、限长、脱敏和已提交结果判定必须保留，也不能新增操作重试。

Transport key 使用影响建连的配置：

- 直连显式 sentinel，区别于任意代理。
- 代理 scheme、主机、端口、认证身份/密码指纹，以及真实影响解析或路由的配置；规范化必须语义等价，不能把用户名密码转小写、丢失认证差异或混同不同 SOCKS scheme。
- 如果存在不同的 TLS 信任、客户端证书、协议模式或拨号配置，纳入对应配置身份；普通目标 host/port 仍由 Go Transport 的内部 key 区分。
- 不把 channel ID、model、cache key 或普通 HTTP Authorization 自动加入 key。HTTP header 身份通常无需拆 TCP 连接；确有连接级身份约束时再分池。

Transport 创建后固定配置，不原地修改已使用的 TLS、Proxy、DialContext 等字段。缓存 key 和真正拨号/CONNECT 必须使用同一份有效配置；当前 `SetProxy` 空串不清空继承值，HTTP/SOCKS context key 也可能同时存在，所以需覆盖旧代理 context 下的切换及显式直连，不能只修缓存 key 而留下动态拨号入口。无效代理配置本地失败，不回退直连。key 和日志不包含可输出的代理密码或 OAuth token。不得无差别修改 WS 等其他调用方的代理语义。

### 4.2 生命周期

- 缓存命中和引用获取必须是一个原子动作，避免取得 entry 后被并发淘汰。
- 每个 entry 继续拥有现有 `normal` 与 `noKeepAlive` 两个 sibling；后者保留非安全空 body 请求的 HTTP/1-only 防隐式重放语义。非空工作请求继续清除自动 `GetBody` 重放能力，保留客户端幂等头及原有重定向政策。
- 在整次 `http.Client.Do` 前获取一个引用，允许的 GET 重定向期间不重新抢槽；返回响应头不释放；在最终 Body.Close 或确定无 Body/调用失败时释放一次。取消观察绑定最终响应的实际请求 context，覆盖 `Client.Timeout`；关闭传输 Body 后才释放引用，不能只在 context cancel 时把仍使用 entry 的请求记成空闲。
- 只有 `inFlight=0` 的缓存 entry 能作为闲置对象淘汰；到达容量且全部忙时返回明确本地资源错误，不切出口、不无界创建备用 Transport。
- 配置变化后新请求选新 key；旧 entry 仍留在同一缓存中计入容量，在引用归零后按闲置/LRU 规则淘汰并关闭闲置连接。因为不移出仍在使用的 entry，不需要 map 外 retired entry、第二次清理协议或渠道配置引用跟踪。
- 不因单渠道改配置就主动关闭共享池。缓存访问时即可回收零引用项，连接本身继续使用既有 `IdleConnTimeout`；初版不新增定时 reaper 或独立缓存 TTL。预算满载且无可淘汰项的明确本地失败，是新增资源约束，PR 必须给出容量数值、错误语义和容量耗尽指标。
- shutdown 关闭新请求准入后等待现有请求清理，再关闭剩余空闲连接。这里的 `inFlight` 是传输引用，不复用作账号执行槽位。

当前实现的缓存上限为 **32 个出口配置，包含直连**；每个配置有 normal/noKeepAlive 两个 sibling。来源仅为 requester 明确代理 URL 与进程级 Transport 配置；URL（包括认证）经 SHA-256 作为缓存 key，不进入日志或 metric label。相同账号、模型或渠道不额外拆池；直连或代理配置变化只改变请求使用的 key。

保留每个 normal Transport 的 `MaxIdleConns=256`、`MaxIdleConnsPerHost=64`、`IdleConnTimeout=90s`，noKeepAlive 不保留闲置连接。32 个池的 HTTP/1 闲置连接配置上界合计 8,192、同一目标合计 2,048；这不是预分配量，也不是包含 HTTP/2 或活跃连接的 FD 硬上限。已测真实网络场景使用 2–3 个出口/认证身份，并测试 32 个 entry 的并发争抢；32 是初版代码资源预算，不是已压测出的最佳值。部署时需把实际 FD/连接数纳入观测。

所有池都忙且新 key 无可用槽时，Send 路径返回本地 HTTP 503、code `provider_transport_capacity`、`UpstreamNotAttempted=true`，不拨号、不换出口。原始 Do 调用方获得传输错误并继续其既有错误呈现合同。无标签计数器 `provider_http_egress_capacity_exhausted_total` 记录容量拒绝次数。池内只保存 entry、引用数与 LRU 顺序，不增加 retired 队列、后台清理器或账号并发预算；只有连接资源清理接入现有业务 drain 后的 shutdown。

缓存有界不等于所有活跃连接数有界，`MaxConnsPerHost` 也不能代表 HTTP/2 并发请求数，不能拿它替代账号准入。

### 4.3 验收

- 将本次双出口复现收敛成自动回归，覆盖 HTTP/1.1、HTTP/2、直连/A/B、同地址不同代理认证和配置更新；错出口次数为 0。
- 热请求在相同出口有真实连接复用；不能通过全局禁用 keep-alive 修复隔离。
- 长 SSE 在其他出口反复建连/回收时仍持续交付；缓存满载、取消、拨号失败、错误 body、重复 Close、配置变化/停机均无引用泄漏。旧 key 无需主动退休也能在归零后被回收。
- 现有 requester no-replay、重定向、HTTP/2 empty-work 回归全部通过；歧义失败不增加 application submission。
- HTTP CONNECT 按协议独立验证：当前 H1 对照正确，H2 缺陷已确认，必须同时覆盖。不得只用 H1 connectMethod 的源码形状推导 H2 结论。

## 5. P1：大请求低分配处理

### 5.1 先减少明确冗余

在共享 `jsonobject` / Responses envelope 内先做两项局部改动，不在 Codex provider 另建一套 JSON 处理器。资源 projection 复用候选因授权反例被取消，现有授权解析保持不变。

第一批：

- 去除 `json.RawMessage` decode 后立即再次复制字段的冗余，先验证字段所有权；不将可变输入切片直接暴露给并发读者。
- Codex planner 的现路径是 `Clone → SetJSON → Raw=nil → MarshalJSON`。为编辑操作避免复制马上失效的整包 Raw；保留通用 `Clone` 的深拷贝契约，不偷偷改成浅拷贝。
- 现有 SetJSON 已经积累字段、最后统一 Marshal，保留这一实现；不把已经存在的一次组装列成新优化，也不为此新建 patch planner。
- 保留现有完整 JSON/depth/字段验证。`Object.Raw/Fields/Order` 仍是公开可变字段，不能仅因曾经 Parse 成功就去掉后续验证；将结构封装为不可变对象是另一个改动，当前没有证据要求它。

资源解析的后续研究门槛：

- 当前 `Projection.Input/Conversation` 不能直接作为精确协议槽位的授权事实；typed Tools 也不能代替原始 tools。§2.4 的不合格候选不进入实施。
- 如果局部复制优化后的 profile 仍指向资源解码，才尝试精确 raw 字段读取等更小替代；继续调用唯一资源规则，不把 raw visitor、COW 或新投影缓存作为前提。未证明等价前保留现有 `prepareResourceRequest`。
- 当前 `getPromptTokens` 依赖 `Input`，本次继续保留完整 projection 行为。减少完整 DTO 解码属于另一候选，不能为消除授权 decode 顺带改变计数/预扣。
- 未来候选必须验证精确字段与大小写别名的不同次序、未知扩展、fallback、opaque ID、原始 body 变更、Chat pre-mapping/Chat→Responses 和 WS 接入；解析失败不能静默跳过权限检查。测试通过后仍需重新测量，不能沿用已否决候选的 18 MiB 数字。

### 5.2 仅在测量支持时引入偏移编辑

CPA 的批量偏移算法解决的是重复工具引用导致的多次整包复制；one-hub 当前顶层 SetJSON 并非每次都 marshal 整包，因此不能宣称能直接获得 CPA 同量级收益。

本轮不采用 COW、偏移索引、自写 scanner、raw 资源 visitor、buffer pool 或 no-op setter 重构。先验证上述小改动。只有它们落地后的 profile 仍指向相应成本，才逐个做消融，对比维护代价与增量收益；不能把这些技术打包成下一阶段必做重构。若届时采用偏移编辑，需要预先计算输出长度，检查溢出、重叠、重复修改和偏移边界，并与现有解析行为差分测试。

无修改路径可复用原文，但必须明确生命周期：发送、异步 observer、日志及 WS reader 仍引用时不得归还或复用 backing buffer。不能为了零分配改用 unsafe string/bytes 转换。小字段如要长期保存应独立复制，防止保留一个小 slice 却挂住数 MiB 的原 body。初版不增加大 buffer `sync.Pool`。

资源树解析只解释代理负责的节点。未知大对象仍必须通过完整 JSON 合法性和深度检查，但不必建立 Go 对象树。保留数值原始词法、未知字段内部顺序和联合类型；exact-wire 无修改时逐字节一致，same-dialect 的差异只能来自批准的代理字段。

### 5.3 验收

- 覆盖 create、compact、input_tokens、Chat→Responses，以及已共享 Object 的路径；不能退化当前 compact/input_tokens raw-wire 修正。
- fixtures 包括 1/16/256 KiB、1/4 MiB 长文本、大工具 schema、图片 data URL、合法未知联合类型、Unicode/转义、大整数、顶层重复 key、64/65 层、尾随 JSON、畸形 JSON。
- 资源用例包括跨用户文件、owner 冲突、opaque ID、`function_call_output` 内容数组、namespace 工具、普通 schema 内同名 `file_id`。拒绝必须在 provider work 前发生。
- 未修改请求字节等价；修改请求差异受限；错误状态/body、尾部 SSE、owner 屏障和 Confirm/Cancel 不变。
- 原始 fixture 上 `B/op`、`allocs/op` 和 CPU 成对比较；大请求分配持续下降，短请求吞吐/延迟不出现可重复的退化。偏移编辑不能仅因“理论更快”通过验收。
- 两处复制删除优先复用现有回归，补充输入所有权和 planner 不修改原 Object 的断言；后续资源优化必须先通过协议槽位与授权等价测试。只有实际引入共享内存才增加对应 race 检查，未来真的替换 scanner/offset editor 时才要求相应差分 fuzz；不为局部复制优化先造一套 parser 测试系统。不能用去掉深度检查或跳过授权取得基准改进。

## 6. 暂缓：账号负载感知

目前只证明存在静态权重选路，没有实际账号池负载分布、上游限制或调度消融数据。因此本轮不新建账号准入管理器、容量配置、等待队列或分布式状态，不安排确定的调度实施 PR。先区分本地并发倾斜、连接等待、token/RPM/套餐限额等成因；429 或 TTFT 变差本身不能证明增加并发槽位有效。

只有受控账号池实验显示收益时，再评审“在既有候选与权重中排除饱和账号 + 原子准入”的最小方案。首个实验不新增 HTTP 队列，不引入 EWMA/Top-K/Redis 负载缓存；统计筛选、实际抢槽、完成/拒绝率和 TTFT，不能只比较成功请求。上线前至少闭合以下约束：

| 必须闭合的问题 | 最低要求 |
| --- | --- |
| 账号事实与容量 | 来自可信稳定身份和明确容量政策；相同实际账号的不同渠道归并预算。缺少稳定身份不能宣称账号级限制；proxy/channel/rotating token 的变化不能任意重建预算 |
| 强 owner 与亲和 | 继续绑定既有精确 channel 和资源身份。账号只合并预算，不能把同账号其他 channel 加入不允许的候选；只有软亲和能在既有提交前边界选择其他合格渠道 |
| 最终发送身份 | 取得 A 的 lease 后，管理员可能原地改为 B；`GetToken → rotateOnce → loadLatestCredentialsFromDatabase` 会加载新凭据。业务发送前共享准入规则必须核对实际要发送的身份，不匹配就本地终止并释放，不迁移 lease、不跨账号重放、不新增跨阶段配置 revision pin |
| 状态和生命周期 | 原子 check-and-increment 与一次释放；一个负责层；定义容量、失败和清理，不重复扣 RPM/预扣。HTTP/WS 各自沿现有 owner 生命周期，不把 terminal、socket 关闭或本地取消都当作上游结束 |
| 覆盖与承诺 | 本地计数只保护已接入入口和本进程；不能称为跨节点总并发或供应商真实执行数。background/task 要纳入必须使用现有 durable owner，不能在 HTTP 返回时假定完成 |

新准入在可能产生副作用的 provider work 和 `ClaimSubmission` 前完成，但不能把这一次核对误认为最终凭据永远不变。进入刷新/open/submission 后，不因身份或传输失败获得额外候选重试权。未来接入需要专门的“同账号不同渠道强绑定”和“A lease/B 实际凭据”回归；本轮不为这些未实施的功能扩展生产 API。

## 7. 暂缓：到期刷新调度

当前已有 15 分钟周期、提前刷新窗口、8 个共享后台槽位、公平扫描游标和 SQL durable fence。本次没有大账号池扫描消融，不能据其他项目使用最小堆就安排堆、dirty 集合与跨节点修复管道。

先测 DB 读取、凭据解码、到期判断、扫描/到期比例、worker 饱和和刷新滞后，定位真实成本。若是查询/读取问题，先评估现有查询的裁剪或分页；若是时间调度问题，再对比当前扫描与到期索引。任何候选只有证明增量收益才进入独立方案。

若最终选择到期索引，它只能是可重建的 channel ID/revision/检查时间索引，不复制 token，不成为刷新权威；SQL row/fence/revision 与现有 `rotateOnce` 不变。busy/ambiguous/orphan 不因计时到期清 fence，DB 无法 Claim 时 OAuth 调用次数为 0。届时必须覆盖启动、管理员编辑、跨节点变动、重建以及现有 shutdown/worker 上限，但本轮不先实现这些配套系统。

## 8. 实施批次与退出条件

| 批次 | 交付 | 合并条件 |
| --- | --- | --- |
| P0，已实现 | 固化出口复现，修复 Transport 隔离和有界生命周期 | 错出口为 0；同出口复用；no-replay、SSE、配置变更与 shutdown 回归通过 |
| P1，已实现 | 固定可重复 benchmark；消除 RawMessage 二次复制和 planner 的无效 Raw 深拷贝 | 协议/资源语义不变；大请求累计分配下降；短请求无可重复退化 |
| 观察项，不预排 PR | 资源解析成本及精确 raw 槽位候选 | 无条件 typed projection 复用已否决；替代方案先通过大小写/顺序/授权差分，再重新消融。不带入完整 parser 重构 |
| 观察项 A，不预排 PR | 账号负载成因与受控调度消融 | 第 6 节证据和身份/owner 门槛成立后，另行确定最小实施范围 |
| 观察项 B，不预排 PR | 刷新扫描成本分解 | 第 7 节确认瓶颈并比较简单替代后，再决定是否需要到期索引 |

本次在独立分支实现 P0/P1，按测试命名、出口隔离、JSON 字段复制、Codex 请求复制和方案记录五个功能维度提交审查，尚未部署。对应内部路径已完成替换，未保留长期 legacy 分支。灰度通过部署批次完成，异常时回退部署版本。已经使用不同出口的部署若必须回退出口隔离版本，需同时限制为不会跨出口混用的部署拓扑，不能把已复现的缺陷重新暴露给同一工作负载。任何候选回退都不重放已经发送的业务请求。

## 9. 正式性能验收方法

分三层测量，避免用一类工具证明另一类收益：

| 层级 | 工具与场景 | 主要指标 |
| --- | --- | --- |
| 局部 CPU/分配 | `go test -bench -benchmem`、benchstat、CPU/alloc/heap pprof；按 body 大小和结构分层 | ns/op、B/op、allocs/op、GC CPU/暂停、存活 heap |
| 真实网络 mock | 实际 Codex provider → TCP/TLS HTTP/1.1/HTTP/2 mock；直连、双 SOCKS、HTTP CONNECT；长 SSE、取消、错误、热更新 | GotConn 复用、建连/握手数、真实出口、FD、缓存 entry/在用 entry 数量、泄漏、尾部交付 |
| 受控上游与负载 | 固定模型、输入/output token 档位、账号、出口、并发和到达率，区分冷/热连接及 cache 命中 | 排队时间、首字节、首 token、总耗时、吞吐、拒绝率、429、P50/P95/P99 |

长 SSE 用慢客户端覆盖背压；并发至少覆盖低负载、接近预算和超预算三档。P99 需要足够样本并报告样本量/置信区间；不直接照搬旧工具的超高并发参数。指标增加到现有 metrics 体系，使用 protocol/proxy 类型/path/result 等有界维度；不得导出完整账号、token、proxy URL、request/session ID。

现有 `hack/bench/self_hosted.go` 使用 in-memory Transport、`httptest.ResponseRecorder` 与 OpenAI provider，可以测局部热路径，不能验证 Codex 真实 TLS、出口隔离或真实网络流式 TTFT。正式网络验收要新增真实服务 fixture 或以 `-self-hosted=false` 指向受控环境；不要通过修改指标名称把自举结果当作端到端结果。

性能通过的前提是所有语义回归通过；收益必须来自同环境、同 fixture、同成功/拒绝口径的成对比较。首次基线不设虚构的提升百分比，后续以 profile 证明优化命中了目标成本。

## 10. 明确不纳入的优化

- 为连接复用绕过 requester 的 no-replay、redirect 或超时策略。
- 在 SSE terminal 后提前断流、整段缓冲合并 flush、丢弃未知事件。
- 普通 HTTP 自动改为 WS，native WS 自动 fallback 到 HTTP，或在已有会话中热切账号。
- 首包前失败自动重放、401/429 后额外发送业务请求、删除 previous_response_id 重新尝试。
- 强行统一 cache key、扩大亲和作用域、降低租户/资源隔离。
- 直接复制 sub2api 的大连接数、Redis 往返、全套调度状态或字段清洗规则。
- 引入 CPA 的 Claude OAuth 工具别名、legacy fallback、uTLS 指纹路径。
- 为性能去掉 JSON 深度/体积限制、资源授权、错误脱敏、SQL fence 或 usage 结算。

## 11. 主要源码与规范证据

以下链接固定到本次提交，可独立复核；开发时仍以当前项目规范为准。

| 主题 | 证据 |
| --- | --- |
| 项目规则与文档口径 | [AGENTS.md](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/AGENTS.md)、[开发文档索引](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/index.md) |
| JSON 重复扫描/复制 | [jsonobject](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/jsonobject/object.go#L22)、[envelope](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/responses/request.go#L97)、[body planner](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/wire/body.go#L18) |
| 资源解析与 opaque 边界 | [resource_request](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/relay/resource_request.go#L14)、[protocol_resources](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/responses/protocol_resources.go)、[resource_manifest](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/responses/resource_manifest.go#L25) |
| 连接池、代理与传输防重放 | [HTTP client](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/requester/http_client.go#L27)、[proxy](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/utils/proxy.go#L21)、[requester](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/requester/http_requester.go#L150)、[绕过 Send 路径的用量/consume 调用](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/usage_snapshot.go#L338) |
| 当前选路与亲和 | [balancer](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/model/balancer.go#L197)、[affinity defaults](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/common/config/channel_affinity.go#L79) |
| 账号预算不能放宽渠道/最终凭据边界 | [owner 精确渠道](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/relay/responses_ownership.go#L102)、[WS 渠道冲突](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/relay/responses_ws_affinity.go#L127)、[凭据重新加载](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/base.go#L1390)、[最终发送凭据](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/responses.go#L441) |
| SQL refresh 权威来源 | [rotateOnce](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/base.go#L737)、[fence 架构](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/codex-credential-refresh-fence-architecture.md)、[auto_refresh](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/providers/codex/auto_refresh.go#L142) |
| 协议、重试、计费与停机边界 | [Codex official](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/codex-official-upstream-architecture.md)、[透明转发](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/responses-transparent-relay-design.md)、[no-replay](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/responses-ws-attempt-replay-architecture.md)、[usage TCC](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/usage-confirmed-tcc-billing-architecture.md)、[shutdown](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/docs/dev/batch-shutdown.md) |
| 自举压测边界 | [self_hosted.go](https://github.com/leviathanion/one-hub/blob/34a3099e6fa77388de168132d81ab061d0582be8/hack/bench/self_hosted.go#L48) |
| CPA 批量修改 | [偏移收集](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/runtime/executor/claude_executor_request.go#L1700)、[一次组装](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/runtime/executor/claude_executor_request.go#L1970) |
| CPA uTLS 与刷新堆 | [uTLS](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/internal/runtime/executor/helps/utls_client.go#L96)、[auto refresh loop](https://github.com/router-for-me/CLIProxyAPI/blob/8ef43e4df3b216a42493105d31c2873b69191473/sdk/cliproxy/auth/auto_refresh_loop.go#L19) |
| sub2api raw body 与连接缓存 | [passthrough](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/service/openai_gateway_passthrough.go#L143)、[缓存获取](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/repository/http_upstream.go#L715)、[cache key](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/repository/http_upstream.go#L1020) |
| sub2api 负载读取与调度门控 | [短缓存与 singleflight](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/service/concurrency_service.go#L581)、[刷新负载再抢槽](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/service/openai_account_scheduler.go#L1591)、[高级调度设置](https://github.com/Wei-Shaw/sub2api/blob/b8dece9000c68815a5b867ca5a1e6f236e173905/backend/internal/service/openai_account_scheduler.go#L1886) |
