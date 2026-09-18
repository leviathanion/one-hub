---
status: accepted
---

# 在已注册 HTTP 表面转发客户端请求头

Exact-wire HTTP relay 在显式注册的 API 表面上转发客户端请求头，除了 hop-by-hop 字段以及代理自己拥有的凭据或路由选择器。因此配置的 channel endpoint 位于请求信任边界之内，可以观察到这些请求头；运维不得把 channel 指向不受信任的 endpoint。provider 响应头暴露由 ADR-0022 单独治理；本文不再授权 `Set-Cookie` 或未知响应头。

该选择保留受支持的请求协议。接口范围单独强制：canonical 资源路径必须留在已注册路由族内，不跟随重定向，代理用渠道凭据替换 provider 认证字段。
