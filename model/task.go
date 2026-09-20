package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var taskOwnerSchemaColumns = []string{
	"id", "owner_id", "task_id", "platform", "user_id", "channel_id",
	"provider_state", "provider_namespace", "provider_task_scope_incarnation", "request_fingerprint",
	"submission_claim_id", "acceptance_recorded_at", "version", "next_action_at",
	"reserved_quota", "charged_quota", "settlement_decision", "owner_closed_at",
}

const (
	TaskPlatformSuno       = "suno"
	TaskPlatformKling      = "kling"
	TaskPlatformMidjourney = "midjourney"
)

var ErrTaskLookupConflict = errors.New("task lookup conflict")

type TaskStatus string

const (
	TaskStatusNotStart     TaskStatus = "NOT_START"
	TaskStatusSubmitted               = "SUBMITTED"
	TaskStatusQueued                  = "QUEUED"
	TaskStatusInProgress              = "IN_PROGRESS"
	TaskStatusFailure                 = "FAILURE"
	TaskStatusCancel                  = "CANCEL"
	TaskStatusSuccess                 = "SUCCESS"
	TaskStatusLocalFailure            = "LOCAL_FAILURE"
	TaskStatusUnknown                 = "UNKNOWN"
)

type TaskProviderState string

const (
	TaskProviderStatePrepared      TaskProviderState = "prepared"
	TaskProviderStateSubmitStarted TaskProviderState = "submit_started"
	TaskProviderStateAccepted      TaskProviderState = "accepted"
	TaskProviderStateClosed        TaskProviderState = "closed"
)

type Task struct {
	ID         int64          `json:"id" gorm:"primary_key;AUTO_INCREMENT;index:idx_tasks_progress,priority:3"`
	OwnerID    string         `json:"-" gorm:"type:varchar(36);not null;uniqueIndex"`
	CreatedAt  int64          `json:"created_at" gorm:"index"`
	UpdatedAt  int64          `json:"updated_at"`
	TaskID     *string        `json:"task_id" gorm:"type:varchar(191);index;uniqueIndex:idx_task_provider_identity,priority:3;uniqueIndex:idx_task_public_identity,priority:3"`
	Platform   string         `json:"platform" gorm:"type:varchar(30);index;uniqueIndex:idx_task_public_identity,priority:1"` // task family
	UserId     int            `json:"user_id" gorm:"index;uniqueIndex:idx_task_public_identity,priority:2"`
	ChannelId  int            `json:"channel_id" gorm:"index"`
	Action     string         `json:"action" gorm:"type:varchar(40);index"` // 任务类型, song, lyrics, description-mode
	Status     TaskStatus     `json:"status" gorm:"type:varchar(20);index"` // 任务状态
	FailReason string         `json:"fail_reason"`
	SubmitTime int64          `json:"submit_time" gorm:"index"`
	StartTime  int64          `json:"start_time" gorm:"index"`
	FinishTime int64          `json:"finish_time" gorm:"index"`
	Progress   int            `json:"progress"`
	Data       datatypes.JSON `json:"data" gorm:"type:json"`
	TokenID    int            `json:"token_id" gorm:"default:0"`

	ProviderState                TaskProviderState `json:"-" gorm:"type:varchar(20);index:idx_tasks_progress,priority:1"`
	ProviderNamespace            string            `json:"-" gorm:"type:varchar(64);not null;uniqueIndex:idx_task_provider_identity,priority:1"`
	ProviderTaskScopeIncarnation string            `json:"-" gorm:"type:varchar(191);not null;uniqueIndex:idx_task_provider_identity,priority:2"`
	RequestFingerprint           string            `json:"-" gorm:"type:char(64);not null"`
	SubmissionClaimID            string            `json:"-" gorm:"type:varchar(36)"`
	AcceptanceRecordedAt         *int64            `json:"-"`
	Version                      uint64            `json:"-" gorm:"not null;default:0"`
	NextActionAt                 int64             `json:"-" gorm:"not null;index:idx_tasks_progress,priority:2"`
	ReservedQuota                int64             `json:"-" gorm:"not null;default:0"`
	TokenQuotaApplied            bool              `json:"-" gorm:"not null;default:false"`
	ChargedQuota                 *int64            `json:"-"`
	SettlementDecision           string            `json:"-" gorm:"type:varchar(24)"`
	BalanceApplyOutcome          string            `json:"-" gorm:"type:varchar(32)"`
	SubmitStartedAt              *int64            `json:"-"`
	OwnerClosedAt                *int64            `json:"-"`
}

func (task *Task) BeforeCreate(_ *gorm.DB) error {
	if task == nil {
		return errors.New("task is required")
	}
	if strings.TrimSpace(task.OwnerID) == "" {
		task.OwnerID = uuid.NewString()
	}
	return nil
}

func (task *Task) FinalQuota() int {
	if task == nil || task.ChargedQuota == nil {
		return 0
	}
	return int(*task.ChargedQuota)
}

func (task Task) MarshalJSON() ([]byte, error) {
	type storedTask Task
	return json.Marshal(struct {
		storedTask
		Quota int `json:"quota"`
	}{storedTask(task), task.FinalQuota()})
}

func TaskProviderID(task *Task) string {
	if task == nil || task.TaskID == nil {
		return ""
	}
	return taskProviderIdentity(task, *task.TaskID)
}

func SetTaskProviderID(task *Task, providerTaskID string) {
	if task == nil {
		return
	}
	providerTaskID = taskProviderIdentity(task, providerTaskID)
	if strings.TrimSpace(providerTaskID) == "" {
		task.TaskID = nil
		return
	}
	task.TaskID = &providerTaskID
}

func CheckTaskOwnerSchema(ctx context.Context) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	db := DB.WithContext(normalizeModelContext(ctx))
	if err := requireProjectionColumnsRemoved(db, "tasks", taskProjectionColumns); err != nil {
		return err
	}
	var tasks []Task
	if err := db.Select(taskOwnerSchemaColumns).Limit(1).Find(&tasks).Error; err != nil {
		return err
	}
	for _, index := range []string{"idx_tasks_owner_id", "idx_tasks_progress", "idx_task_provider_identity", "idx_task_public_identity"} {
		if !db.Migrator().HasIndex(&Task{}, index) {
			return fmt.Errorf("task owner schema is missing index %s", index)
		}
	}
	return nil
}

func GetTaskByTaskIds(platform string, userId int, taskIds []string) (task []*Task, err error) {
	// 最多返回100个任务
	err = DB.Omit("channel_id", "charged_quota", "user_id").Where("platform = ? and user_id = ? and task_id in (?)", platform, userId, taskIds).Limit(100).
		Find(&task).Error
	if err != nil {
		return nil, err
	}
	requestedIDs := make(map[string]struct{}, len(taskIds))
	if taskUsesOpaqueProviderID(platform) {
		for _, id := range taskIds {
			requestedIDs[id] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(task))
	for _, item := range task {
		if item == nil {
			continue
		}
		providerTaskID := TaskProviderID(item)
		if taskUsesOpaqueProviderID(platform) {
			if _, exact := requestedIDs[providerTaskID]; !exact {
				return nil, ErrTaskLookupConflict
			}
		}
		if _, ok := seen[providerTaskID]; ok {
			return nil, fmt.Errorf("%w: platform=%s user_id=%d task_id=%s", ErrTaskLookupConflict, platform, userId, providerTaskID)
		}
		seen[providerTaskID] = struct{}{}
	}

	return
}

func GetTaskByTaskId(platform string, userId int, taskId string) (task *Task, err error) {
	var tasks []*Task
	err = DB.Where("platform = ? and user_id = ? and task_id = ?", platform, userId, taskId).Limit(2).Find(&tasks).Error
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return nil, nil
	}
	if len(tasks) > 1 {
		return nil, fmt.Errorf("%w: platform=%s user_id=%d task_id=%s", ErrTaskLookupConflict, platform, userId, taskId)
	}

	if taskUsesOpaqueProviderID(platform) && TaskProviderID(tasks[0]) != taskId {
		return nil, ErrTaskLookupConflict
	}
	return tasks[0], nil
}

type TaskQueryParams struct {
	PaginationParams
	Platform       string `form:"platform"`
	ChannelID      string `form:"channel_id"`
	TaskID         string `form:"task_id"`
	UserID         string `form:"user_id"`
	Action         string `form:"action"`
	Status         string `form:"status"`
	StartTimestamp int64  `form:"start_timestamp"`
	EndTimestamp   int64  `form:"end_timestamp"`
	UserIDs        []int  `form:"user_ids"`
	TokenID        int    `form:"token_id"`
}

var allowedTaskOrderFields = map[string]bool{
	"id":          true,
	"created_at":  true,
	"submit_time": true,
	"finish_time": true,
	"channel_id":  true,
	"user_id":     true,
	"platform":    true,
}

func GetAllTasks(params *TaskQueryParams) (*DataResult[Task], error) {
	tx := DB
	var tasks []*Task

	if params.ChannelID != "" {
		tx = tx.Where("channel_id = ?", params.ChannelID)
	}
	if params.Platform != "" {
		tx = tx.Where("platform = ?", params.Platform)
	}
	if params.UserID != "" {
		tx = tx.Where("user_id = ?", params.UserID)
	}
	if len(params.UserIDs) != 0 {
		tx = tx.Where("user_id in (?)", params.UserIDs)
	}
	if params.TaskID != "" {
		tx = tx.Where("task_id = ?", params.TaskID)
	}
	if params.Action != "" {
		tx = tx.Where("action = ?", params.Action)
	}
	if params.Status != "" {
		tx = tx.Where("status = ?", params.Status)
	}
	if params.StartTimestamp != 0 {
		tx = tx.Where("submit_time >= ?", params.StartTimestamp)
	}
	if params.EndTimestamp != 0 {
		tx = tx.Where("submit_time <= ?", params.EndTimestamp)
	}

	return PaginateAndOrder(tx, &params.PaginationParams, &tasks, allowedTaskOrderFields)
}

func GetAllUserTasks(userId int, params *TaskQueryParams) (*DataResult[Task], error) {
	tx := DB.Omit("channel_id").Where("user_id = ?", userId)
	var tasks []*Task

	if params.TokenID > 0 {
		tx = tx.Where("token_id = ?", params.TokenID)
	}

	if params.Platform != "" {
		tx = tx.Where("platform = ?", params.Platform)
	}

	if params.TaskID != "" {
		tx = tx.Where("task_id = ?", params.TaskID)
	}
	if params.Action != "" {
		tx = tx.Where("action = ?", params.Action)
	}
	if params.Status != "" {
		tx = tx.Where("status = ?", params.Status)
	}
	if params.StartTimestamp != 0 {
		tx = tx.Where("submit_time >= ?", params.StartTimestamp)
	}
	if params.EndTimestamp != 0 {
		tx = tx.Where("submit_time <= ?", params.EndTimestamp)
	}

	return PaginateAndOrder(tx, &params.PaginationParams, &tasks, allowedTaskOrderFields)
}

// 原生 OpenAI Task 的上游 ID 是不透明标识，不以空白或大小写归一化别名。
// 其余既有任务家族保留原来的标识约定。
func taskProviderIdentity(task *Task, id string) string {
	if task != nil && taskUsesOpaqueProviderID(task.Platform) {
		return id
	}
	return strings.TrimSpace(id)
}

func taskUsesOpaqueProviderID(platform string) bool {
	return platform == TaskPlatformOpenAIResponsesBackground || platform == TaskPlatformOpenAIBatch
}
