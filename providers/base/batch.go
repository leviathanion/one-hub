package base

import "one-api/types"

// BatchRelayInterface 只构造原生 Batch 地址及观察真实结果证据。
// relay 负责先证明 custom_id 与准入项的关联；提取失败不能阻止结果交付。
type BatchRelayInterface interface {
	BuildBatchRelayURL(escapedPath, rawQuery string) (string, error)
	ExtractBatchUsage(endpoint string, body []byte) *types.Usage
}
