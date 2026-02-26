# 性能优化台账（中继与格式转换）

## 范围

- 主要流量路径：`/v1` OpenAI 兼容中继。
- 次要流量路径：`/api` 管理接口（低流量，不作为首要优化目标）。

## P0（优先优化）

1. 请求转换存在多次 JSON 编解码与对象复制
- 问题：在 `AllowExtraBody`/`CustomParameter` 路径里，请求体会经历 `struct -> json -> map -> json`，并且上游预处理还会先读取并重写 body。
- 影响：增加 CPU 与内存分配，长消息/高并发下明显放大。
- 证据：
  - `providers/base/common.go:287`
  - `providers/base/common.go:292`
  - `common/utils/request_builder.go:26`
  - `relay/main.go:170`
  - `relay/main.go:213`
- 建议：收敛为单次反序列化路径；仅在确实需要参数合并时走 map；其余走结构体直传。

2. 协议兼容路径存在“双向重复转换”
- 问题：部分请求会 `chat -> responses -> chat` 或 `responses -> chat -> responses`。
- 影响：多一层对象构建与 JSON 处理，提升尾延迟。
- 证据：
  - `relay/chat.go:185`
  - `relay/chat.go:216`
  - `relay/responses.go:140`
  - `relay/responses.go:160`
- 建议：按 provider 能力做“就近路由”，避免可直连时走兼容转换。

3. 流式转换为逐帧 decode + encode
- 问题：SSE 每个事件都 `Unmarshal` 后再 `Marshal` 输出。
- 影响：高 token/s 输出时 CPU 开销显著。
- 证据：
  - `providers/openai/responses.go:211`
  - `providers/openai/responses.go:373`
  - `relay/relay_util/responses_stream.go:81`
  - `relay/relay_util/responses_stream.go:405`
- 建议：保留透传场景，减少非必要重编码；结构化转换仅用于必要字段重写。

4. Tool 参数转换热路径频繁 JSON 解析
- 问题：工具调用参数以字符串 JSON 反复反序列化。
- 影响：tool-heavy 请求会出现额外 CPU 开销。
- 证据：
  - `providers/claude/chat.go:259`
  - `providers/gemini/type.go:444`
- 建议：优先使用 `json.RawMessage`/懒解析；仅在目标 provider 需要结构化时解析。

## P1（高价值优化）

1. 配额消费与日志写入写放大
- 问题：一次请求会触发多次配额/日志/统计写入，且使用每请求 goroutine 异步消费。
- 影响：高并发下 DB/缓存压力抬升，调度开销上升。
- 证据：
  - `relay/relay_util/quota.go:150`
  - `relay/relay_util/quota.go:154`
  - `relay/relay_util/quota.go:161`
  - `relay/relay_util/quota.go:176`
  - `relay/relay_util/quota.go:193`
- 建议：合并写路径（批量化/队列化），并为异步消费增加有界 worker 池。

2. 上游 HTTP 连接池未调优
- 问题：`http.Transport` 未配置连接复用关键参数。
- 影响：并发下连接复用不足，握手/建连开销上升。
- 证据：
  - `common/requester/http_client.go:12`
- 建议：补齐 `MaxIdleConns`、`MaxIdleConnsPerHost`、`IdleConnTimeout`、`TLSHandshakeTimeout`、`ResponseHeaderTimeout`。

3. 渠道配置在请求路径重复 JSON 解析
- 问题：`ModelHeaders`、`ModelMapping`、`CustomParameter` 每请求解析。
- 影响：增加固定 CPU 开销。
- 证据：
  - `providers/base/common.go:111`
  - `providers/base/common.go:154`
  - `providers/base/common.go:174`
- 建议：在渠道加载阶段预解析并缓存结构体，热路径直接读取。

4. 流式与文本拼接存在字符串累加热点
- 问题：多处 `+=` 累加文本或 arguments。
- 影响：长文本下可能产生额外分配与拷贝。
- 证据：
  - `relay/relay_util/responses_stream.go:253`
  - `relay/relay_util/responses_stream.go:352`
  - `types/responses.go:529`
  - `types/chat.go:71`
- 建议：改为 `strings.Builder` 或分片收集后一次 `Join`。

## P2（次优先，但应纳入治理）

1. 控制面采用周期性全量重载
- 问题：按固定周期加载渠道、分组、价格等全量数据。
- 影响：管理面流量低时问题不大，但在大数据量下会造成无效 DB 压力。
- 证据：
  - `main.go:110`
  - `main.go:153`
  - `model/option.go:145`
  - `model/balancer.go:250`
  - `model/pricing.go:70`
- 建议：改为事件驱动或增量刷新。

2. 管理接口限流实现非原子多次 Redis 往返
- 问题：`LLen/LIndex/LPush/LTrim` 模式。
- 影响：当前影响较小（管理接口低流量），但实现效率一般。
- 证据：
  - `middleware/rate-limit.go:38`
- 建议：迁移到 Lua 原子脚本。

3. 并发读写边界不清（稳定性风险）
- 问题：部分共享 map 直接暴露内部引用。
- 影响：潜在并发读写问题，间接影响性能与可用性。
- 证据：
  - `model/pricing.go:144`
  - `controller/pricing.go:38`
  - `model/balancer.go:230`
- 建议：返回副本或只读视图，避免绕过锁直接访问内部状态。

## 建议推进顺序

1. 先做 P0-1、P0-3、P1-2（对延迟最直接）。
2. 再做 P1-1、P1-3（对吞吐与稳定性收益大）。
3. 最后治理 P2（控制面与并发边界）。
