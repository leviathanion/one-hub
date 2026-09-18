---
status: accepted
---

# 使用软 Prompt Cache Affinity

Chat 和 Responses 共享既有 affinity 机制，对同一 group、canonical model 和 `prompt_cache_key` 优先选择先前 channel，使用既有的一小时默认生命周期。当该 channel 不可用时，该偏好可以回退到普通调度并记录 miss；这保留可用性，因为 prompt caching 是成本和延迟优化，而不是资源 ownership；既有管理员定义的 affinity 规则仍然可用。
