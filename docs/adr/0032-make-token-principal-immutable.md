---
status: accepted
---

# 让 Token principal 不可变

Token ID 在其整个生命周期内绑定一个 User。更改 principal 需要创建新 Token 并吊销旧 Token；不支持在线转移和任何跨实例 active-attempt registry。

这使授权、预扣、结算、长生命周期 session 容量和审计记录在同一个 principal 上一致，而无需协调所有活动请求。保留可变转移被拒绝，因为管理写入无法跨进程原子地移动每个在途和持久 owner。
