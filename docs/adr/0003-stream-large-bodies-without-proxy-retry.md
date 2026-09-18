---
status: accepted
---

# 流式传递大请求体，不做代理级重试

> ADR-0030 继续采用该执行边界：Billing Owner 只调用一次配置好的 requester，任何返回错误都不会触发第二次 Application Submission 或 provider failover；它不承诺缺少 provider 幂等契约时的 provider-observable at-most-once。

不需要检查或转换请求体的操作把它作为一次性流转发，而不是预读大体积可重放副本或写临时文件。一旦配置的 requester 返回错误，relay 就认为 provider 执行结果歧义，不再发起第二次应用层调用或 provider failover。可重放的请求体和幂等头不会扩大该执行权；更强的外部副作用保证需要 provider 自己的幂等契约。

需要做有界策略或转换检查的生成请求，在请求体超过检查上界时被拒绝。支持这些罕见的超大请求需要磁盘暂存或绕过适用的策略，两者都不足以证明这里新增复杂度的合理性。
