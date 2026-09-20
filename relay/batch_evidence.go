package relay

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"one-api/model"
)

// 只保存计价所需用量、去重摘要和来源身份，不保存结果正文或价格。
type batchPendingEvidence struct {
	Digest   string                   `json:"digest"`
	Usage    *backgroundUsageEvidence `json:"usage,omitempty"`
	Conflict bool                     `json:"conflict,omitempty"`
	Keys     []string                 `json:"keys,omitempty"`
}

func restoreBatchEvidence(d openAIBatchData) map[string]*batchObservedUsage {
	observed := make(map[string]*batchObservedUsage, len(d.PendingEvidence))
	for id, saved := range d.PendingEvidence {
		if saved == nil || d.Items[id] == nil {
			continue
		}
		digest, err := hex.DecodeString(saved.Digest)
		e := &batchObservedUsage{usage: saved.Usage.restore(), conflict: saved.Conflict || err != nil || len(digest) != 32, keys: append([]string(nil), saved.Keys...)}
		copy(e.digest[:], digest)
		observed[id] = e
	}
	return observed
}

func mergeBatchEvidence(target, more map[string]*batchObservedUsage) {
	for id, next := range more {
		if next == nil {
			continue
		}
		old := target[id]
		if old == nil {
			copy := *next
			copy.keys = append([]string(nil), next.keys...)
			target[id] = &copy
			continue
		}
		old.conflict = old.conflict || next.conflict || old.digest != next.digest
		for _, key := range next.keys {
			if !batchEvidenceHasKey(old.keys, key) {
				old.keys = append(old.keys, key)
			}
		}
	}
	identities := map[string]string{}
	for id, e := range target {
		for _, key := range e.keys {
			if previous, ok := identities[key]; ok && previous != id {
				target[previous].conflict = true
				e.conflict = true
			} else {
				identities[key] = id
			}
		}
	}
}

func batchEvidenceHasKey(keys []string, key string) bool {
	for _, old := range keys {
		if old == key {
			return true
		}
	}
	return false
}

func batchAccountingEvidence(d openAIBatchData, observed map[string]*batchObservedUsage) map[string]*batchObservedUsage {
	if !d.EvidenceCapacityLimited {
		return observed
	}
	kept := make(map[string]*batchObservedUsage, len(d.PendingEvidence))
	for id := range d.PendingEvidence {
		if evidence := observed[id]; evidence != nil {
			kept[id] = evidence
		}
	}
	return kept
}

// 容量用尽时保留原已持久证据优先，不清空已有可信组件；未能记住的
// 新条目停止后续收费观察，原始文件与资源归属观察不受此计费预算影响。
func checkpointBatchEvidence(d *openAIBatchData, observed map[string]*batchObservedUsage, limit int) error {
	prior := d.PendingEvidence
	ids := make([]string, 0, len(observed))
	for id := range observed {
		if !d.EvidenceCapacityLimited || prior[id] != nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := prior[ids[i]] != nil, prior[ids[j]] != nil
		if a != b {
			return a
		}
		return ids[i] < ids[j]
	})
	d.PendingEvidence = nil
	wasLimited := d.EvidenceCapacityLimited
	d.EvidenceCapacityLimited = true // 预算始终包含降级标记。
	base, err := json.Marshal(d)
	if err != nil {
		return err
	}
	remaining := limit - len(base) - 128
	if remaining < 0 {
		return errors.New("batch base metadata exceeds observation capacity")
	}
	d.PendingEvidence = map[string]*batchPendingEvidence{}
	for _, id := range ids {
		e := observed[id]
		saved := &batchPendingEvidence{Digest: hex.EncodeToString(e.digest[:]), Conflict: e.conflict, Keys: e.keys}
		if !e.conflict {
			saved.Usage = captureBackgroundUsage(saved.Digest, e.usage)
		}
		raw, err := json.Marshal(map[string]*batchPendingEvidence{id: saved})
		if err != nil {
			return err
		}
		cost := len(raw) - 1
		if cost > remaining {
			wasLimited = true
			// 新冲突必须撤销旧可收费证据，不能因容量不足保存旧的有效版本。
			if old := prior[id]; old != nil {
				saved = &batchPendingEvidence{Digest: old.Digest, Conflict: true, Keys: old.Keys}
				raw, _ = json.Marshal(map[string]*batchPendingEvidence{id: saved})
				cost = len(raw) - 1
			}
			if cost > remaining {
				continue
			}
		}
		d.PendingEvidence[id] = saved
		remaining -= cost
	}
	d.EvidenceCapacityLimited = wasLimited
	return nil
}

func rescheduleBatchObservation(parent context.Context, task *model.Task, d openAIBatchData, observed map[string]*batchObservedUsage) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			durable, err := model.GetOpenAIBatchTask(ctx, task.OwnerID)
			if err != nil {
				return err
			}
			if durable.ProviderState == model.TaskProviderStateClosed {
				return nil
			}
			latest, err := batchData(durable)
			if err != nil {
				return err
			}
			latest.TerminalStatus = d.TerminalStatus
			merged := restoreBatchEvidence(latest)
			mergeBatchEvidence(merged, observed)
			observed = merged
			d = latest
			*task = *durable
		}
		if err := checkpointBatchEvidence(&d, observed, model.BatchDataMaxBytes); err != nil {
			return err
		}
		err := rescheduleBatch(ctx, task, d)
		if !errors.Is(err, model.ErrTaskBillingState) {
			return err
		}
	}
	return model.ErrTaskBillingState
}
