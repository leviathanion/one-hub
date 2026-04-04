package base

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"one-api/common"
	"one-api/internal/billing"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
	"strings"
)

type taskSettlementStatus string

const (
	taskSettlementStatusReserved   taskSettlementStatus = "reserved"
	taskSettlementStatusCommitted  taskSettlementStatus = "committed"
	taskSettlementStatusRolledBack taskSettlementStatus = "rolled_back"
)

type taskTrackingSnapshot struct {
	ProviderAccepted bool   `json:"provider_accepted,omitempty"`
	ProviderTaskID   string `json:"provider_task_id,omitempty"`
}

type taskSettlementSnapshot struct {
	Envelope billing.SettlementEnvelope `json:"envelope"`
	Status   taskSettlementStatus       `json:"status"`
	Tracking taskTrackingSnapshot       `json:"tracking,omitempty"`
}

type TaskSettlementFinalizeResult struct {
	Handled      bool
	PersistTask  bool
	Deduplicated bool
}

func BuildTaskSettlementSnapshotProperties(quota *relay_util.Quota, task *model.Task) ([]byte, int, error) {
	if quota == nil {
		return nil, 0, errors.New("task settlement quota is nil")
	}
	if task == nil {
		return nil, 0, errors.New("task settlement task is nil")
	}

	identity := buildTaskSettlementIdentity(task)
	if identity == "" {
		return nil, 0, errors.New("task settlement identity is empty")
	}

	envelope := quota.BuildSettlementEnvelope(
		&types.Usage{PromptTokens: 1, CompletionTokens: 0, TotalTokens: 1},
		false,
		billing.SettlementRequestKindAsyncTask,
		identity,
		true,
	)
	if envelope == nil {
		return nil, 0, errors.New("task settlement envelope is nil")
	}

	// Async task pricing is fixed at submit time, but these providers do not expose
	// meaningful token usage. Keep the quota snapshot and log projection free of
	// synthetic tokens that are only used to drive fixed-price calculation.
	envelope.Command.UsageSummary = billing.UsageSummary{}

	// Async task finalization has no meaningful unary request latency.
	envelope.Options.Projection.RequestTime = 0
	snapshot := taskSettlementSnapshot{
		Envelope: *envelope,
		Status:   taskSettlementStatusReserved,
	}
	properties, err := json.Marshal(snapshot)
	if err != nil {
		return nil, 0, err
	}
	return properties, envelope.Command.FinalQuota, nil
}

func FinalizeTaskSettlement(ctx context.Context, task *model.Task, success bool) (TaskSettlementFinalizeResult, error) {
	snapshot, err := parseTaskSettlementSnapshot(task)
	if err != nil {
		return TaskSettlementFinalizeResult{}, err
	}
	if snapshot == nil {
		return TaskSettlementFinalizeResult{PersistTask: true}, nil
	}
	if snapshot.Status != taskSettlementStatusReserved {
		return TaskSettlementFinalizeResult{Handled: true}, nil
	}

	command := snapshot.Envelope.Command
	if !success {
		command.FinalQuota = 0
		command.Fingerprint = ""
	}

	result, err := billing.ApplySettlement(ctx, command, &snapshot.Envelope.Options)
	if err != nil {
		return TaskSettlementFinalizeResult{Handled: true}, err
	}
	if result.Deduplicated && result.FingerprintConflict {
		return TaskSettlementFinalizeResult{
			Handled:      true,
			Deduplicated: true,
		}, nil
	}

	snapshot.Envelope.Command = command
	if success {
		snapshot.Status = taskSettlementStatusCommitted
	} else {
		snapshot.Status = taskSettlementStatusRolledBack
	}

	properties, err := json.Marshal(snapshot)
	if err != nil {
		return TaskSettlementFinalizeResult{Handled: true}, err
	}
	task.Properties = properties
	task.Quota = command.FinalQuota
	return TaskSettlementFinalizeResult{
		Handled:      true,
		PersistTask:  true,
		Deduplicated: result.Deduplicated,
	}, nil
}

func FailTaskWithSettlement(ctx context.Context, task *model.Task, reason string) error {
	if task == nil {
		return nil
	}

	if strings.TrimSpace(reason) != "" {
		task.FailReason = reason
	}
	task.Status = model.TaskStatusFailure

	result, err := FinalizeTaskSettlement(ctx, task, false)
	if err != nil {
		return err
	}
	if !result.Handled {
		quota := task.Quota
		if quota > 0 {
			if err := refundLegacyTaskReserve(task, quota); err != nil {
				return err
			}
			model.RecordLog(task.UserId, model.LogTypeSystem, fmt.Sprintf("异步任务执行失败 %s，补偿 %s", task.TaskID, common.LogQuota(quota)))
		}
	}
	if !result.PersistTask {
		return nil
	}
	task.Progress = 100
	return task.Update()
}

func buildTaskSettlementIdentity(task *model.Task) string {
	if task == nil {
		return ""
	}
	if task.ID > 0 {
		return fmt.Sprintf("task:%d:finalize", task.ID)
	}
 
	platform := strings.TrimSpace(task.Platform)
	taskID := strings.TrimSpace(task.TaskID)
	if platform == "" || taskID == "" {
		return ""
	}
	return fmt.Sprintf(
		"platform=%s|user=%d|channel=%d|task=%s|finalize",
		url.QueryEscape(platform),
		task.UserId,
		task.ChannelId,
		url.QueryEscape(taskID),
	)
}

func parseTaskSettlementSnapshot(task *model.Task) (*taskSettlementSnapshot, error) {
	if task == nil || len(task.Properties) == 0 {
		return nil, nil
	}

	var snapshot taskSettlementSnapshot
	if err := json.Unmarshal(task.Properties, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Status == "" {
		return nil, nil
	}
	return &snapshot, nil
}

func HasTaskSettlementSnapshot(task *model.Task) bool {
	snapshot, err := parseTaskSettlementSnapshot(task)
	return err == nil && snapshot != nil
}

func TaskTrackingHandle(task *model.Task) string {
	if task == nil {
		return ""
	}
	if taskID := strings.TrimSpace(task.TaskID); taskID != "" {
		return taskID
	}
	snapshot, err := parseTaskSettlementSnapshot(task)
	if err != nil || snapshot == nil {
		return ""
	}
	return strings.TrimSpace(snapshot.Tracking.ProviderTaskID)
}

func TaskAcceptedWithoutTrackingHandle(task *model.Task) bool {
	snapshot, err := parseTaskSettlementSnapshot(task)
	if err != nil || snapshot == nil {
		return false
	}
	return snapshot.Tracking.ProviderAccepted && strings.TrimSpace(TaskTrackingHandle(task)) == ""
}

func MarkTaskProviderAccepted(task *model.Task, providerTaskID string) ([]byte, error) {
	snapshot, err := parseTaskSettlementSnapshot(task)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, errors.New("task settlement snapshot is missing")
	}
	snapshot.Tracking.ProviderAccepted = true
	snapshot.Tracking.ProviderTaskID = strings.TrimSpace(providerTaskID)
	return json.Marshal(snapshot)
}

func refundLegacyTaskReserve(task *model.Task, quota int) error {
	if quota <= 0 || task == nil {
		return nil
	}
	if task.TokenID <= 0 {
		return model.IncreaseUserQuota(task.UserId, quota)
	}

	token, err := model.GetTokenById(task.TokenID)
	if err != nil {
		return err
	}
	return model.ApplyTokenUserQuotaDeltaDirect(task.TokenID, task.UserId, token.UnlimitedQuota, -quota)
}
