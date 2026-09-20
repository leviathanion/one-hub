package relay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"time"

	"one-api/common/logger"
	"one-api/internal/billing"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	"one-api/types"
)

type OpenAIBatchProgressor struct{}

func (*OpenAIBatchProgressor) UpdateTaskStatus(ctx context.Context, _ map[int][]string, tasks map[string]*model.Task) error {
	for _, task := range tasks {
		pollCtx, cancel := context.WithTimeout(ctx, batchRequestDeadline)
		err := pollOpenAIBatch(pollCtx, task)
		cancel()
		if err != nil && !errors.Is(err, model.ErrTaskBillingState) {
			logger.LogError(ctx, "batch observation: "+err.Error())
		}
	}
	_, err := model.DeleteExpiredOpenAIBatchTasks(ctx, time.Now())
	return err
}

func batchTerminal(status string) bool {
	switch status {
	case "completed", "failed", "expired", "cancelled":
		return true
	}
	return false
}

func pollOpenAIBatch(ctx context.Context, task *model.Task) error {
	if task == nil || task.ProviderState != model.TaskProviderStateAccepted {
		return nil
	}
	d, err := batchData(task)
	if err != nil {
		return err
	}
	// 默认 poll fence 只覆盖短轮询；此操作还顺序读取结果文件。在本地
	// 有限扫描 deadline 内延长同一 SQL fence，不镜像或约束上游状态。
	if _, err := model.SaveOpenAIBatchSnapshot(ctx, task, time.Now().Add(batchRequestDeadline+model.TaskPollInterval)); err != nil {
		return err
	}
	accepted := task.SubmitTime
	if task.AcceptanceRecordedAt != nil {
		accepted = *task.AcceptanceRecordedAt
	}
	if time.Now().Unix()-accepted >= int64(model.BatchTrackingWindow/time.Second) {
		task.Status = model.TaskStatusUnknown
		task.FailReason = "local batch tracking window expired"
		return finalizeOpenAIBatch(ctx, task, d, restoreBatchEvidence(d))
	}
	c := backgroundTaskContext(ctx, task, backgroundResponseData{Group: d.Group})
	p, a, err := batchProvider(c, task.ChannelId)
	if err != nil {
		return rescheduleBatch(ctx, task, d)
	}
	if batchTerminal(d.TerminalStatus) {
		return finishObservedBatch(ctx, task, d, p, a)
	}
	response, apiErr := sendBatchRequest(ctx, c, p, a, http.MethodGet, "/v1/batches/"+url.PathEscape(model.TaskProviderID(task)), "", nil)
	if apiErr != nil {
		return rescheduleBatch(ctx, task, d)
	}
	if response == nil || response.Body == nil {
		return rescheduleBatch(ctx, task, d)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		return rescheduleBatch(ctx, task, d)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return rescheduleBatch(ctx, task, d)
	}
	raw, err := readBatchEnvelope(response.Body)
	if err != nil {
		return rescheduleBatch(ctx, task, d)
	}
	var envelope batchEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.ID != model.TaskProviderID(task) {
		return rescheduleBatch(ctx, task, d)
	}
	if err = observeBatchFiles(ctx, task, raw); err != nil {
		return rescheduleBatch(ctx, task, d)
	}
	// GET observers may advance the fence concurrently. Reload before collecting
	// evidence; finalization still owns the SQL version and single balance action.
	task, err = model.GetOpenAIBatchTask(ctx, task.OwnerID)
	if err != nil {
		return err
	}
	if task.ProviderState != model.TaskProviderStateAccepted {
		return nil
	}
	d, err = batchData(task)
	if err != nil {
		return err
	}
	if !batchTerminal(envelope.Status) {
		return rescheduleBatch(ctx, task, d)
	}
	d.TerminalStatus = envelope.Status
	return finishObservedBatch(ctx, task, d, p, a)
}

func finishObservedBatch(ctx context.Context, task *model.Task, d openAIBatchData, p providersBase.ProviderInterface, a providersBase.BatchRelayInterface) error {
	observed, resourcesReady := collectBatchEvidence(ctx, task, d, p, a)
	if !resourcesReady {
		return rescheduleBatchObservation(ctx, task, d, observed)
	}
	observed = batchAccountingEvidence(d, observed)
	switch d.TerminalStatus {
	case "completed":
		task.Status = model.TaskStatusSuccess
	case "cancelled":
		task.Status = model.TaskStatusCancel
	default:
		task.Status = model.TaskStatusFailure
	}
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return finalizeOpenAIBatch(settleCtx, task, d, observed)
}

func rescheduleBatch(ctx context.Context, task *model.Task, d openAIBatchData) error {
	d.Failures++
	if d.Failures > 3 {
		d.Failures = 3
	}
	raw, err := encodeBatchData(d)
	if err != nil {
		return err
	}
	task.Data = raw
	_, err = model.SaveOpenAIBatchSnapshot(ctx, task, time.Now().Add(time.Duration(1<<uint(d.Failures-1))*model.TaskPollInterval))
	return err
}

type batchObservedUsage struct {
	digest   [32]byte
	usage    *types.Usage
	conflict bool
	keys     []string
}

func collectBatchEvidence(ctx context.Context, task *model.Task, d openAIBatchData, p providersBase.ProviderInterface, a providersBase.BatchRelayInterface) (map[string]*batchObservedUsage, bool) {
	observed := restoreBatchEvidence(d)
	identities := map[string]string{}
	for id, e := range observed {
		for _, key := range e.keys {
			identities[key] = id
		}
	}
	needsResources := false
	for _, item := range d.Items {
		if len(item.Slots) > 0 {
			needsResources = true
			break
		}
	}
	resourcesReady := true
	for _, fileID := range []string{d.OutputFileID, d.ErrorFileID} {
		if fileID == "" {
			continue
		}
		owner, err := model.GetResourceOwner(ctx, "file", fileID, task.UserId)
		if err != nil || owner.ChannelID != task.ChannelId || owner.TaskOwnerID == nil || *owner.TaskOwnerID != task.OwnerID {
			if needsResources {
				resourcesReady = false
			}
			continue
		}
		response, err := batchFileResponse(ctx, p, fileID)
		if err != nil {
			if needsResources && !owner.BatchResultObserved {
				resourcesReady = false
			}
			continue
		}
		resourcesObserved := true
		err = readBatchLines(response.Body, func(raw []byte) error {
			// 完整资源屏障与计费解析独立；未知/重复 custom_id 也不能跳过
			// 真实资源 ID 检查后错误地给整份文件盖“已观察”标记。
			if !owner.BatchResultObserved {
				if err := bindBatchResultResources(ctx, task, d, raw); err != nil {
					resourcesObserved = false
					logger.LogWarn(ctx, "batch resource observation incomplete: "+err.Error())
				}
			}
			var line batchResultLine
			if json.Unmarshal(raw, &line) != nil {
				resourcesObserved = false
				return nil
			}
			item := d.Items[line.CustomID]
			if item == nil || item.Ambiguous || line.Response == nil || line.Response.StatusCode < 200 || line.Response.StatusCode >= 300 {
				return nil
			}
			digest := sha256.Sum256(line.Response.Body)
			if old := observed[line.CustomID]; old != nil {
				if old.digest != digest {
					old.conflict = true
				}
			} else {
				observed[line.CustomID] = &batchObservedUsage{digest: digest, usage: a.ExtractBatchUsage(item.Endpoint, line.Response.Body)}
			}
			var identity struct {
				ID string `json:"id"`
			}
			_ = json.Unmarshal(line.Response.Body, &identity)
			for _, key := range []string{"request:" + line.Response.RequestID, "response:" + identity.ID} {
				if key == "request:" || key == "response:" {
					continue
				}
				sum := sha256.Sum256([]byte(key))
				key = hex.EncodeToString(sum[:])
				if !batchEvidenceHasKey(observed[line.CustomID].keys, key) {
					observed[line.CustomID].keys = append(observed[line.CustomID].keys, key)
				}
				if previous, exists := identities[key]; exists && previous != line.CustomID {
					observed[previous].conflict = true
					observed[line.CustomID].conflict = true
				} else {
					identities[key] = line.CustomID
				}
			}
			return nil
		})
		response.Body.Close()
		if err != nil {
			logger.LogWarn(ctx, "batch result observation incomplete: "+err.Error())
		} else if resourcesObserved && !owner.BatchResultObserved {
			recordBatchFileObserved(ctx, owner)
		}
		if needsResources && !owner.BatchResultObserved {
			current, lookupErr := model.GetResourceOwner(ctx, "file", fileID, task.UserId)
			if lookupErr != nil || !current.BatchResultObserved {
				resourcesReady = false
			}
		}
	}
	return observed, resourcesReady
}

func finalizeOpenAIBatch(ctx context.Context, task *model.Task, d openAIBatchData, observed map[string]*batchObservedUsage) error {
	d.PendingEvidence = nil // 已由 observed 恢复，本次终结后不再需要重启 checkpoint。
	c := backgroundTaskContext(ctx, task, backgroundResponseData{Group: d.Group})
	var total int64
	confirm := false
	d.ObservedItems = len(observed)
	d.Evidence = map[string]*backgroundUsageEvidence{}
	d.LogUsage = &billing.UsageSummary{}
	ids := make([]string, 0, len(observed))
	for id := range observed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	evidenceHash := sha256.New()
	for _, id := range ids {
		evidenceHash.Write([]byte(id))
		evidenceHash.Write([]byte{0})
		evidenceHash.Write(observed[id].digest[:])
		if observed[id].conflict {
			evidenceHash.Write([]byte{1})
		} else {
			evidenceHash.Write([]byte{0})
		}
	}
	d.EvidenceDigest = hex.EncodeToString(evidenceHash.Sum(nil))
	// 个别缺证据/价格不可用不会抹去其他独立 item 的有效组件。
	for customID, evidence := range observed {
		item := d.Items[customID]
		if item == nil || item.Ambiguous || evidence == nil || evidence.conflict {
			continue
		}
		c.Set("billing_original_model", item.BillingOriginalModel)
		quota, err := relay_util.NewPricedQuota(c, item.Model, 0)
		if err != nil {
			continue
		}
		decision := quota.EvaluateProviderUsage(evidence.usage)
		if !decision.Confirm || decision.FinalQuota < 0 || decision.FinalQuota > math.MaxInt64-total {
			continue
		}
		total += decision.FinalQuota
		confirm = true
		d.PricedItems++
		accumulateTaskLogUsage(d.LogUsage, evidence.usage)
		// 只保留量化 evidence，不存输出正文或请求参数。若超预算，保留
		// 稳定来源散列和计数，不因观察存储容量而重发或扣第二次费用。
		d.Evidence[customID] = captureBackgroundUsage(hex.EncodeToString(evidence.digest[:]), evidence.usage)
	}
	raw, err := encodeBatchData(d)
	if err != nil {
		// 大规模证据无需逐项持久 checkpoint；文件仍是可重扫的真实来源。
		d.Evidence = nil
		d.EvidenceSummaryTruncated = true
		raw, err = encodeBatchData(d)
		if err != nil {
			return err
		}
	}
	action := "cancel"
	if confirm {
		action = "confirm"
	}
	result, err := model.FinalizeOpenAIBatchBillingOwner(ctx, task, total, action, raw)
	if err != nil {
		return err
	}
	if result.Outcome != model.BillingBalanceCommitted {
		return errors.New("batch settlement commit is unresolved")
	}
	ProjectAsyncTaskSettlement(ctx, task, result)
	return nil
}
