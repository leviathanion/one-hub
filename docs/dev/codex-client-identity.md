---
title: "Codex 渠道的 UA 与 originator"
layout: doc
outline: deep
lastUpdated: true
---

# Codex 渠道的 UA 与 originator

## 文档状态

- 状态：当前实现。
- 适用范围：Codex 渠道的 Responses HTTP、compact、Chat 适配、Responses WebSocket、realtime 和用量查询／额度重置；OAuth 登录和刷新使用独立认证协议。
- 文档口径：本文定义 UA／originator 的识别、透传和兜底契约；其余 Responses 协议构造见 [Codex Official Upstream 架构设计](./codex-official-upstream-architecture.md)，页面配置见 [Codex 渠道](../use/Codex.md#user-agent-与-originator-透传及兜底)。
- 关联代码：`providers/codex/wire/client_identity.go`、`providers/codex/wire/client_detection.go`、`providers/codex/client_identity.go`、`providers/codex/credential_policy.go`、`common/codexpolicy/schema.go`、`model/channel_validation.go`。

## 解析规则

UA 与 originator 先共同决定是否允许客户端透传，再决定兜底：

1. 客户端身份符合已知 Codex 格式时，保留客户端提供的非空 UA／originator。只提供其中一项时，不补另一项，也不根据 UA 推导 originator。
2. 未识别为 Codex（包括两项均缺失）时，忽略客户端这两项，分别使用 `other.codex.default_user_agent` 和 `other.codex.default_originator`。
3. 未配置或为空的兜底字段分别使用 PI 默认值：UA 为 `pi (<platform> <release>; <arch>)`，originator 为 `pi`。

例如渠道只设置 `default_originator` 时，兜底 UA 仍由代码生成。配置不会强制覆盖已识别的 Codex 客户端值。

```json
{
  "codex": {
    "default_user_agent": "pi (linux 6.12.1; x64)",
    "default_originator": "pi"
  }
}
```

通常无需填写这两个字段；省略时 UA 使用代理运行环境的平台、内核版本和架构。Linux、macOS 和 Windows 均按 PI 的 Node 命名生成，例如 Go `amd64` 对应 `x64`、Windows 对应 `win32`。读取系统版本失败或运行在未支持的平台时，版本使用 `unknown`。

## Codex 识别边界

已知名称包括 `codex_cli_rs`、`codex-cli`、`codex-tui`、`codex_vscode`、`codex_vscode_copilot`、`codex_app`、`codex_chatgpt_desktop`、`codex_atlas`、`codex_exec`、`codex_sdk_ts`，以及官方使用的 `Codex ` 家族（如 `Codex Desktop`）。识别不区分大小写，发送时保留原值。

originator 按名称识别；UA 按有边界的产品名称识别，并支持 app-server 的最后一个 `(clientInfo.name; version)` 后缀。例如：

```text
cccc/0.141.0 (Mac OS 14.6.1; arm64) Apple_Terminal/453 (codex-tui; 0.141.0)
```

这种 UA 的前缀被 `CODEX_INTERNAL_ORIGINATOR_OVERRIDE` 改写，但后缀仍可识别。`evil-codex_cli_rs/1`、`my-codex-client/1` 和裸 `codex` 不会因子串而命中。

识别只选择兼容处理分支，不提供身份认证；这些 header 可以由调用方伪造。没有可识别标记的自定义 app-server 客户端使用渠道／PI 兜底。新增已知客户端名称应有源码或实际请求样本依据。

## 共享边界与验证

`providers/codex/wire.ResolveClientIdentity` 是唯一的 UA／originator 解析入口。realtime 连接复用签名使用相同结果，身份变化会改变兼容性签名。

渠道 policy 在 provider 实例内缓存一份解析结果（含错误），以渠道 ID 和 `Other` 内容为 key；配置变化会重新解析，运行时渠道快照替换时清空缓存。Responses、realtime 和用量路径共用该缓存；Responses 专属的 `model_headers` 拒绝规则在其入口单独检查，不改变其他路径的配置边界。

已识别 Codex 请求的待透传值只做 HTTP 安全、单值和每字段 16 KiB 上限检查，不用旧的 originator 字符白名单限制上游客户端名称；非法值返回本地 400，缺失值保持省略。未识别请求的这两项直接忽略，不进入透传校验。配置采用相同的 HTTP 安全和大小限制，保存时拒绝非法配置，运行时拒绝使用非法配置。上述失败均发生在凭据获取／刷新之前，不发送上游推理请求或自动重试。

错误来源和状态码见 [Codex 错误语义](./codex-official-upstream-architecture.md#错误语义)，重试授权见 [错误标记与建连重试门禁](./responses-ws-attempt-replay-architecture.md#错误标记与建连重试门禁)。local-only 恢复与身份变化后的复用判定见 [Codex realtime affinity](./channel-affinity-architecture.md#_5-codex-realtime-affinity)。

缺失 UA 时通过空的 Go header map 项抑制 transport 自动生成 `Go-http-client/1.1`；实际 wire 不发送空 UA。HTTP 和 WebSocket 本地服务端测试覆盖客户端两项、单项、配置单项、代码兜底及非 Codex 输入，并验证未知 Responses body 字段仍保留。

本地测试已验证 66 个 HTTP／WS 请求用例及配置、连接复用边界；macOS／Windows 仅完成交叉编译，未在对应系统或真实上游联调。全仓 Go 子包测试通过，主程序测试因工作区缺少 `web/build` 未完成。

## 变更影响

旧的固定 `codex-tui/0.135.0 ...` 与 `codex_cli_rs/2026-06-29 ... one-hub` 默认 UA 均被替换，UA 推导 `codex-tui`／`pi` 的逻辑删除。非 Codex 客户端随带的 UA／originator 不再影响上游身份。已有 `default_originator` 保留为显式配置，并扩展到上述所有操作；这可能改变 realtime 和用量请求的身份。

两个配置字段都只对未识别为 Codex 的请求生效。已识别 Codex 客户端缺少 UA 或 originator 时，配置不再补齐该缺失项。

完整且已识别的 Codex 身份若有效出站头未变，compatibility hash 保持不变。partial identity、非 Codex、身份缺失、配置兜底及旧 `model_headers` 身份覆盖等场景的出站头可能变化，从而触发绑定替换和上游重连。新旧版本节点并存时可能重复替换，不能保证只重连一次；正常进程重启也会清空本地 session。以当前有效出站身份判定复用，不兼容性放宽旧 hash。

debug 审计的 reason 已改为 `client-identity-present`、`partial-client-identity`、`non-codex-or-missing-client-identity`，用于区分透传、省略与兜底；依赖这些 debug 值的外部分析脚本需相应调整。

不需要数据库迁移。若部署环境要求指定身份，应设置上述两个兜底字段。历史 `model_headers` 不是 UA／originator 的配置入口；Codex 管理端仍拒绝非空 `model_headers`。

## 本地源码依据

本次以以下工作区快照核对行为，不将其他代理的改写策略直接复制到 one-hub：

- PI `e4ce7b449`：`packages/ai/src/utils/pi-user-agent.ts`、`packages/ai/src/api/openai-codex-responses.ts`；UA 使用运行时 OS 信息，originator 固定 `pi`。
- Codex `7498521d28`：`codex-rs/login/src/auth/default_client.rs`、`codex-rs/app-server/src/request_processors/initialize_processor.rs`；确认 originator override、first-party 名称和 UA 后缀来源。
- sub2api `efe9aab1e`：`backend/internal/pkg/openai/request.go`；确认扩展名称和被 override 的 UA 后缀识别。其 UA／originator 配对改写不采用。
- CLIProxyAPI `05391d7b`：`internal/runtime/executor/codex_executor_request.go`、`internal/runtime/executor/codex_websockets_request.go`；其配置覆盖和逐字段补全与本策略不同，不采用。
- codex-proxy-rs `fba89410`：`backend/crates/gateway-api/src/openai/auth.rs`；参考复合 UA 中按产品边界识别 Codex 的做法，不引入其版本门禁。
