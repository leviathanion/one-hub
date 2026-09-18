---
status: superseded by ADR-0011, ADR-0012
---

# 在提交时冻结 Batch item Price Policy

> Batch 不属于普通用户初始支持面，新方案不保留下面描述的 observe-only Batch 元数据和结算子系统。

Batch 输入上传保留 `custom_id`、endpoint、请求模型、请求的 service tier 和 hosted-tool 定价上下文的有界投影。hosted-tool 上下文只包含 web-search service/context size 和 image quality/size。`/v1/chat/completions` 上的 Chat Search 模型为每个成功 item 精确重建一笔已提交的 web-search 固定费用，不受 provider 输出遗漏或重复影响。成功的 Batch submission 为每个不同请求模型冻结一份 Price Policy 和显式存在的 Batch input/output multiplier；显式 `0x` 保持免费，缺失值继承 `1x`。Batch retrieval 之后把该快照绑定到 provider 输出文件。输出 JSONL item 按逐字节 `custom_id` 连接，因此空白不同和混合模型的 item 保留各自的请求定价边界。

结算仍优先使用显式配置的 exact 或 wildcard actual-model 价格。未注册的 provider version 或 alias 使用 item 冻结的请求 Price Policy 和 Batch multiplier。没有注册 actual model 的 legacy 或未关联输出被标记为 unpriced，绝不落到资源请求的空模型全局价格。省略 hosted-tool 定价字段的 terminal Responses item 会从其提交的请求上下文重建。没有 response ID 的成功输出方言使用稳定的 channel/Batch/output-file/`custom_id` observation identity，因此重复读取输出文件只结算一次。

Batch create 行还冻结 submitter user、token、token name、routing-group 字段、group ratio、source IP 和现有 observation log 路径所需的 user agent。输出读取即使由另一名管理员执行，也使用该快照进行归属和价格运算；observation identity 不变。该快照在 user 或 token 之后被删除时仍是历史归属事实。缺失的 legacy metadata 会被诊断，绝不回退到 reader。该快照不引入 Batch ownership，也不把 observe-only Batch 处理扩展为持久配额结算。

schema 通过普通自动迁移新增 input-item 和 Batch-job 定价元数据表，包括用于惰性过期清理的 `expires_at` 前缀索引。既有 Batch job 不回填，因为其提交时目录无法重建。元数据 30 天后过期，过期行在后续元数据写入时惰性删除。上传投影上限为 50,000 item 和每 JSONL 行 8 MiB。成功的 upload/create/list/retrieve 控制响应最多 1 MiB，在其 status/body 发布到下游之前提交到元数据；输出复制只做有界内存查找。元数据数据库工作有两秒 deadline。provider 成功但本地元数据失败仍可能丢失 observation 计费；消除该边界需要有意排除在有界观察设计之外的持久 outbox。
