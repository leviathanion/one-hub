---
title: "自定义渠道上游接口配置"
layout: doc
outline: deep
lastUpdated: true
---

# 自定义渠道上游接口配置

## 文档状态

- 状态：当前实现。
- 适用范围：自定义渠道（type=8）的 `plugin.endpoints` 配置、页面控件与一次性数据库迁移。

## 语义与范围

自定义渠道（type=8）使用 `plugin.endpoints` 统一保存上游接口的启用状态与地址。页面按 OpenAI API、Claude API 分组，每行显示接口名称、启用开关和上游地址。窄屏下接口名称移至控件上方，开关和地址仍保持同行。关闭保留地址；修改随表单统一保存，单渠道和标签编辑继续使用现有影响范围确认。

配置对象是上游接口，而非客户端入口或适配器能力。开关不能授予代理尚未实现的操作，也不创建协议转换能力。已有 `compatible_response` 显式转换契约保持：禁用原生 Responses 不等于禁止 Responses→Chat；该转换必须使用已启用、可表示请求的 Chat 接口。已发送上游的请求不会因配置或传输错误获得重试授权。

本次覆盖原 `customize` 中的 11 个接口和 Claude Messages。模型发现、旧 Chat Realtime、其他 provider 的插件不纳入该表单，不凭本次结构调整扩大接口支持面。Responses 配置覆盖当前实现的 create、compact、input_tokens、资源读取/删除/input_items 及原生 Responses WebSocket；各操作仍受独立 capability、资源 owner 和计费边界控制。

## 数据契约

渠道 `base_url` 和凭据继续属于渠道连接配置，不复制进每个接口。`plugin` 仍为原 JSON 数据库列，接口条目使用稳定语义名称：

```json
{
  "endpoints": {
    "openai.chat_completions": {"enabled": true, "upstream_url": ""},
    "openai.responses": {"enabled": true, "upstream_url": "/custom/responses"},
    "openai.embeddings": {"enabled": false, "upstream_url": "/custom/embeddings"},
    "anthropic.messages": {"enabled": true, "upstream_url": "/v1/messages"}
  }
}
```

| 字段/状态 | 语义 |
| --- | --- |
| `enabled` | 必填布尔值；false 停用，保留地址 |
| `upstream_url` 缺失或空字符串 | 使用该接口内置默认路径 |
| `upstream_url` 为路径 | 拼接渠道基础地址 |
| `upstream_url` 为完整 HTTP(S) URL | 使用独立上游目标，继续使用渠道凭据 |
| 接口条目缺失 | 关闭，不动态继承新版本增加的接口 |

`common/providerendpoint` 是接口目录、默认路径和配置解析的唯一所有者。管理端通过 `/api/channel/endpoints` 读取目录；新建表单按目录显式写入预设。读取已有渠道时绝不补全为开启。配置属于代理拥有的管理 envelope，未知接口名、拼错的配置字段、非布尔开关、非字符串地址、URL 用户凭据及 fragment 均拒绝；这不限制客户端请求的上游扩展字段。

地址采用现有自定义渠道的拼接规则，而非 URL 根路径替换：基础地址 `https://example.com/root` 加 `/v1/messages` 得到 `https://example.com/root/v1/messages`。Cloudflare Gateway 路径沿用现有 `/v1` 去除规则。保留原始查询参数、重复查询键、转义路径和既有 HTTP/WS 空白处理差异；前端不得在未修改配置时重新格式化这些地址。

普通完整 URL 直接指定上游接口；Responses 的资源操作会在该地址路径上追加资源 ID 或操作后缀，并保留查询语义。旧 `-realtime` 模型名分支的特殊拼接不在本次修复范围。

## 全链路影响面

| 边界 | 实现要求 |
| --- | --- |
| 单渠道新增、批量新增、编辑、模型列表探测 | 使用统一管理配置校验；旧格式明确失败 |
| 标签读取、标签编辑、后台同步 | 同一表单结构和字段提交语义，取消不提交；后台不能绕过连接身份保护 |
| SQL、缓存、渠道选择器 | SQL 保存新结构，缓存发布原样传递；升级必须停止所有旧进程并重建内存缓存 |
| OpenAI provider 配置 | 由新接口对象解析 URI，缺失/关闭不回退到默认开启 |
| 普通接口选路 | 候选阶段排除关闭接口；固定渠道则明确失败 |
| Chat、Speech、Transcription、Responses capability | 复用同一 URI 解析；实际目标协议决定应检查的接口 |
| Claude 原生选路、模型列表过滤、provider 构造 | 统一读取 `anthropic.messages`，相对路径和完整 URL 使用同一上游地址规则 |
| Responses HTTP/WS 和存量资源 | 地址身份比较、后缀拼接、查询和转义保留，owner 固定时不切换渠道 |
| 凭据刷新 fence | 所有已保存上游目标（包括停用接口）都属于连接配置；管理员目标修改增加 revision 并受刷新 fence 保护，后台同步不能改写 |

## 一次性全量迁移

迁移 `202609080001` 只在数据库升级阶段运行，运行时不调用旧格式转换，不双读、不双写。新旧结构不能混用；前端和外部管理客户端必须随服务端同时升级。

迁移包含全部 type=8 行，包括软删除记录，按 ID 分批读取并在同一事务中更新。只更新 `plugin`，不更换渠道 ID、凭据、revision 或资源绑定。非自定义渠道和其他插件键保持原值。遇到歧义或无法迁移的配置，整个迁移失败回滚，报告渠道 ID，不打印凭据和完整配置。

| 旧配置 | 新配置 |
| --- | --- |
| plugin/customize/接口项缺失，或原值为空 | 对原支持的 OpenAI 接口显式写入开启和空地址 |
| `disable` | 关闭，空地址；已经丢失的旧地址不可恢复 |
| 路径或完整地址 | 开启，原样保留 |
| 同值数字别名，如 `16`、`016` | 合并为一个 `openai.responses` |
| 冲突数字别名、无法归属的旧配置键 | 整体迁移失败，升级前处理 |
| Claude 未开启 | Messages 关闭，已有显式地址仍保留 |
| Claude 显式 base_url | 按原规则追加 `/v1/messages`，保存完整上游 URL |
| Claude 继承渠道地址 | 保持继承；若原来的规范化会改变实际 wire，则固化原有效地址 |

原值为非字符串时，旧代码按默认地址解释；迁移保留这个实际行为。新管理接口不接受这种类型。重复运行迁移不重写有效的新配置；混合新旧字段直接报错。历史迁移仍按原顺序执行，新迁移在最后收敛到唯一新格式。

## 升级与回滚

1. 备份数据库和对应程序版本，暂停管理写入并停止全部主从服务进程，等待旧请求结束。
2. 启动一个新版主节点执行迁移。迁移失败时保持停机，按报出的渠道 ID 修正备份恢复后的旧配置，重新执行升级。
3. 验证迁移完成、代表性接口实际地址与升级前一致，再启动其余新版节点并恢复流量。不能新旧版本混跑，也不能旧从节点继续读取迁移后的配置。
4. 回滚需要停机恢复升级前数据库与匹配的程序。不能只回滚二进制；旧结构不能同时保存“关闭”和地址，自动降级会丢数据。恢复备份会丢弃升级后的业务写入，需在恢复流量前完成升级验收。

## 验证要求

验证新建预设、缺失即关闭、关闭保存地址、同行布局及键盘/标签关联；验证旧格式拒绝、SQL 全量迁移的事务性、幂等和软删除覆盖。后端覆盖路径/完整地址、Cloudflare、查询及转义、Responses HTTP/WS 身份与资源后缀、Claude native、固定渠道禁用、候选排除和后台身份保护。协议回归检查未知字段、上游错误、流式交付以及歧义执行后不重试。

2026-09-08 验证记录：

- `go test ./... -timeout 180s`：通过；受影响的 model、providers、relay、controller、middleware、router 均执行回归。
- `go build`：通过，包含新前端产物。
- `web` 下 `npm test`：108 项通过，包括接口状态往返和真实组件渲染。
- 修改的 JSX 文件 ESLint：无错误；`npm run build`：通过，仍有既有 Browserslist 数据过期和大 bundle 提示。
- 浏览器隔离测试页加载真实组件及后端接口目录：验证地址编辑、关闭、保存后重载、键盘重新开启；1280px/375px 布局中开关和地址同行，375px 无横向溢出。此测试页的持久化使用测试 localStorage，实际 SQL 读写由后端测试覆盖。
- SQLite 实库测试验证 205 行跨批次全量迁移、软删除行、非目标类型、独立插件数据、revision 不变、幂等，以及末批失败时整体回滚。未对现有业务数据库执行迁移。

完整后端运行过程中，未修改的 `TestIssue039ResponsesWSCompletedInjectRecoversThroughNextTurn` 曾出现时序性断言失败；独立重复 10 次及最终全量复跑通过。本次没有修改该 WS 用例或 actor 实现。

MySQL、PostgreSQL 实库迁移尚未验证：当前环境未找到相应数据库测试工具或 Docker。迁移使用现有 GORM JSON 列和事务接口，但这不能替代两个数据库上的升级演练；部署到这两种数据库前需在备份副本验证。
