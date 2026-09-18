---
status: accepted
---

# 不把 turn 取消放进 provider wire 协议

代理把取消视为一次在途 attempt 的生命周期：下游取消停止该 attempt、关闭其上游传输并阻止重试。它不定义代理特有的 `response.cancel` 契约，也不合成 `response.cancelled` provider 事件。原生 provider 客户端事件保持有序透传数据，跨协议 adapter 拒绝没有定义映射的事件。未来的 agent/session API 可以用自己稳定的 turn 标识引入定向取消；这以今天不把可定向取消作为代理特性为代价，换取当前 relay 与 provider wire 行为兼容。
