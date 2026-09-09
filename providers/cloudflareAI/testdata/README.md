`chat_usage.json` 是按 Cloudflare 官方 [Execute AI model](https://developers.cloudflare.com/api/resources/ai/methods/run/) 的 `result.response`、`result.usage` schema 构造的测试样例；100/20/120 为任务 R05 指定的测试计量，不是真实账户抓包。

2026-09-06 核对 [llama-3.1-8b-instruct 的官方模型定义](https://developers.cloudflare.com/workers-ai/models/llama-3.1-8b-instruct/) 和 [llama-4-scout-17b-16e-instruct 的官方 schema](https://github.com/cloudflare/cloudflare-docs/blob/production/src/content/workers-ai-models/llama-4-scout-17b-16e-instruct.json)：同步结果声明 usage.prompt_tokens/completion_tokens/total_tokens；流式定义仅为 text/event-stream binary，不能据此认定所有 SSE 都含 usage。

集成测试中的 SSE 有 usage/无 usage、零值/缺失样例均为构造的适配器边界测试，只证明遇到对应数据帧时保留快照且不在 [DONE] 重复计量。未获得部署启用型号和脱敏真实响应；R05 的真实 JSON/SSE fixture 验收仍需运营方材料。无可靠 usage 的流继续按缺证据释放预扣，不估算收费。
