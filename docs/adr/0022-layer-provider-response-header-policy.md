---
status: accepted
---

# 按 operation 分层 provider 响应头暴露

provider 响应头使用默认拒绝策略：安全的请求元数据与当前 operation 所需的头和未修改的 body 表示组合。Exact-wire 保留普通 provider status、body、error 和 stream 语义，但不授权 provider cookie、account/project identity、upstream rate-limit state、hop-by-hop 字段或未知未来头。已确认的共享账号认证或配额失败保留 provider HTTP status，但把其 body 替换为稳定公共账号错误 envelope；这是凭据边界，不是跨协议规范化。完整透传和黑名单方式被拒绝，因为共享 provider 凭据使账号元数据成为独立安全边界；专用账号例外需要未来的显式 channel capability，而不是成为默认。
