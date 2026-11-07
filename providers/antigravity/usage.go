package antigravity

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"
)

// UsageSnapshot 只包含可展示的上游模型额度，不携带账号身份或原始响应。
type UsageSnapshot struct {
	ChannelID int          `json:"channel_id"`
	FetchedAt int64        `json:"fetched_at"`
	Models    []ModelQuota `json:"models"`
}

type ModelQuota struct {
	ModelID           string   `json:"model_id"`
	DisplayName       string   `json:"display_name,omitempty"`
	RemainingFraction *float64 `json:"remaining_fraction,omitempty"`
	ResetTime         string   `json:"reset_time,omitempty"`
}

// GetUsageSnapshot 按需查询，不以本地计费或限流状态推算上游剩余额度。
func (p *AntigravityProvider) GetUsageSnapshot(ctx context.Context) (*UsageSnapshot, error) {
	catalog, err := p.fetchModelCatalog(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := &UsageSnapshot{
		ChannelID: p.Channel.Id,
		FetchedAt: time.Now().Unix(),
		Models:    make([]ModelQuota, 0, len(catalog)),
	}
	for id, raw := range catalog {
		quota := ModelQuota{ModelID: id}
		var entry map[string]json.RawMessage
		if json.Unmarshal(raw, &entry) == nil {
			_ = json.Unmarshal(entry["displayName"], &quota.DisplayName)
			var info map[string]json.RawMessage
			if json.Unmarshal(entry["quotaInfo"], &info) == nil {
				var fraction *float64
				if json.Unmarshal(info["remainingFraction"], &fraction) == nil && fraction != nil && !math.IsNaN(*fraction) && !math.IsInf(*fraction, 0) && *fraction >= 0 && *fraction <= 1 {
					quota.RemainingFraction = fraction
				}
				var resetTime string
				if json.Unmarshal(info["resetTime"], &resetTime) == nil {
					if _, err := time.Parse(time.RFC3339, resetTime); err == nil {
						quota.ResetTime = resetTime
					}
				}
			}
		}
		snapshot.Models = append(snapshot.Models, quota)
	}
	sort.Slice(snapshot.Models, func(i, j int) bool { return snapshot.Models[i].ModelID < snapshot.Models[j].ModelID })
	return snapshot, nil
}
