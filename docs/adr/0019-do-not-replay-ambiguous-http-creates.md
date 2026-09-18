---
status: accepted
---

# 不重放歧义的 HTTP create

> ADR-0030 保留“不重放”边界，但取代本文的“歧义时不退款”计费规则：存在 Authoritative Provider Usage 才 Confirm，否则 Cancel 并返回，不换渠道或重放同一 Work Action。

传输结果歧义的 HTTP create 直接暴露而不重试：provider 可能已经生成输出、调用工具、对共享账号收费或创建 stored resource。Billing Owner 遵循 ADR-0030，在 submission claim 后绝不换渠道；Authoritative Provider Usage 确认客户收费，而其缺失则取消预扣，即使 one-hub 可能仍承担 provider 成本。请求体可重放性和未提交的下游响应不能证明重放安全。
