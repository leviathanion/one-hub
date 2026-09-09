# 首期不支持 OpenAI Data Residency 区域端点

OpenAI Data Residency 的区域 API 域名在计费与能力契约完成前不进入支持面，并在 provider work 前拒绝。这个判断只依据 OpenAI 的区域域名，不把 Azure、AWS 或 Google Cloud 的部署 region 映射成通用的 `processing_scope`：它们属于各 provider 的部署属性，应按各自 adapter、operation 和定价能力判断。

解除限制的条件是同时具备区域端点识别、明确的价格调整和覆盖公共请求路径的回归测试；在此之前，管理员 capability 声明不能覆盖该限制。
