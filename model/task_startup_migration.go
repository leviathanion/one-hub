package model

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-gormigrate/gormigrate/v2"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type startupTaskColumns struct {
	OwnerID                      *string `gorm:"type:varchar(36)"`
	ProviderState                *string `gorm:"type:varchar(20)"`
	ProviderNamespace            *string `gorm:"type:varchar(64)"`
	ProviderTaskScopeIncarnation *string `gorm:"type:varchar(191)"`
	RequestFingerprint           *string `gorm:"type:char(64)"`
	SubmissionClaimID            *string `gorm:"type:varchar(36)"`
	AcceptanceRecordedAt         *int64
	Version                      uint64 `gorm:"default:0"`
	NextActionAt                 int64  `gorm:"default:0"`
	ReservedQuota                int64  `gorm:"default:0"`
	TokenQuotaApplied            bool   `gorm:"default:false"`
	ChargedQuota                 *int64
	SettlementDecision           *string `gorm:"type:varchar(24)"`
	BalanceApplyOutcome          *string `gorm:"type:varchar(32)"`
	SubmitStartedAt              *int64
	OwnerClosedAt                *int64
}

type startupLegacyTask struct {
	Task
	Quota      *int64
	Properties datatypes.JSON
}

func migrateHistoricalTasks() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090005", Migrate: func(db *gorm.DB) error {
		if !db.Migrator().HasTable("tasks") {
			return nil
		}
		if err := addStartupMigrationColumns(db, "tasks", &startupTaskColumns{}); err != nil {
			return err
		}
		return db.Transaction(func(tx *gorm.DB) error {
			var rows []startupLegacyTask
			return tx.Table("tasks").FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
				for _, row := range rows {
					if row.OwnerID != "" && row.ChargedQuota != nil {
						continue
					}
					// 新版在途 owner 无需转换；旧记录必须有明确终态，不能重放旧结算。
					if row.OwnerID != "" && row.ProviderState != TaskProviderStateClosed {
						continue
					}
					charged, err := historicalTaskCharge(row)
					if err != nil {
						return startupRowError("tasks", row.ID, err.Error())
					}
					updates := map[string]any{"charged_quota": charged}
					if row.OwnerID == "" {
						if row.ProviderState != "" {
							return startupRowError("tasks", row.ID, "已有执行状态但缺少 owner_id")
						}
						if err := initializeHistoricalTask(&row.Task, "tasks", row.ID, charged); err != nil {
							return err
						}
						updates["owner_id"] = row.OwnerID
						updates["provider_state"] = row.ProviderState
						updates["provider_namespace"] = row.ProviderNamespace
						updates["provider_task_scope_incarnation"] = row.ProviderTaskScopeIncarnation
						updates["request_fingerprint"] = row.RequestFingerprint
						updates["settlement_decision"] = row.SettlementDecision
						updates["task_id"] = row.TaskID
						updates["next_action_at"] = 0
					}
					if err := tx.Table("tasks").Where("id = ?", row.ID).Updates(updates).Error; err != nil {
						return err
					}
				}
				return nil
			}).Error
		})
	}, Rollback: startupMigrationRollback}
}

func historicalTaskCharge(row startupLegacyTask) (int64, error) {
	if row.Status != TaskStatusSuccess && row.Status != TaskStatusFailure && row.Status != TaskStatusCancel {
		return 0, fmt.Errorf("旧任务尚无明确终态，须先完成旧任务及结算核查")
	}
	if row.ChargedQuota != nil {
		if *row.ChargedQuota < 0 {
			return 0, fmt.Errorf("最终额度为负数")
		}
		return *row.ChargedQuota, nil
	}
	if row.Quota == nil || *row.Quota < 0 {
		return 0, fmt.Errorf("缺少非负历史额度")
	}
	var snapshot struct {
		Status   string `json:"status"`
		Envelope struct {
			Command struct {
				UserID     int    `json:"user_id"`
				TokenID    int    `json:"token_id"`
				ChannelID  int    `json:"channel_id"`
				FinalQuota *int64 `json:"final_quota"`
			} `json:"command"`
		} `json:"envelope"`
	}
	if len(row.Properties) > 0 {
		if err := json.Unmarshal(row.Properties, &snapshot); err != nil {
			return 0, fmt.Errorf("旧结算快照不是有效 JSON")
		}
	}
	if snapshot.Status != "" {
		cmd := snapshot.Envelope.Command
		if cmd.UserID != row.UserId || cmd.TokenID != row.TokenID || cmd.ChannelID != row.ChannelId || cmd.FinalQuota == nil || *cmd.FinalQuota != *row.Quota {
			return 0, fmt.Errorf("旧结算快照与任务归属或额度不一致")
		}
		if snapshot.Status == "committed" && row.Status == TaskStatusSuccess {
			return *cmd.FinalQuota, nil
		}
		if snapshot.Status == "rolled_back" && row.Status != TaskStatusSuccess && *cmd.FinalQuota == 0 {
			return 0, nil
		}
		return 0, fmt.Errorf("旧结算快照尚未完成或与终态冲突")
	}
	// 更早版本的失败记录仍可能保留预扣额度，不能将其解释成最终扣费。
	if row.Status != TaskStatusSuccess && *row.Quota != 0 {
		return 0, fmt.Errorf("旧失败任务仍有非零额度，缺少退款完成证据")
	}
	return *row.Quota, nil
}

func initializeHistoricalTask(task *Task, source string, sourceID int64, charged int64) error {
	if task.UserId <= 0 || task.ChannelId <= 0 || task.Platform == "" {
		return startupRowError(source, sourceID, "缺少任务用户、渠道或平台")
	}
	if id := TaskProviderID(task); len(id) > 191 || (task.TaskID != nil && id != *task.TaskID) {
		return startupRowError(source, sourceID, "上游任务 ID 非法，不能自动改写")
	}
	if task.TaskID != nil && *task.TaskID == "" {
		task.TaskID = nil
	}
	key := fmt.Sprintf("one-hub/startup/%s/%d", source, sourceID)
	task.OwnerID = uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)).String()
	task.ProviderNamespace = "task-platform:" + task.Platform
	task.ProviderTaskScopeIncarnation = "provider-wide"
	// 旧库没有原请求，使用独立域的记录摘要阻止被当作新请求的可重放凭据。
	task.RequestFingerprint = fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
	task.ProviderState = TaskProviderStateClosed
	task.ChargedQuota = &charged
	task.SettlementDecision = "confirm"
	if task.Status != TaskStatusSuccess {
		task.SettlementDecision = "cancel"
	}
	task.NextActionAt = 0
	return nil
}

func migrateHistoricalMidjourney() *gormigrate.Migration {
	return &gormigrate.Migration{ID: "202609090007", Migrate: func(db *gorm.DB) error {
		if !db.Migrator().HasTable("midjourneys") {
			return nil
		}
		return db.Transaction(func(tx *gorm.DB) error {
			var rows []Midjourney
			return tx.Table("midjourneys").FindInBatches(&rows, 200, func(_ *gorm.DB, _ int) error {
				for _, row := range rows {
					quota := int64(row.Quota)
					task := Task{
						Platform: TaskPlatformMidjourney, UserId: row.UserId, TokenID: row.TokenID,
						ChannelId: row.ChannelId, Action: row.Action, Status: TaskStatus(row.Status),
						SubmitTime: row.SubmitTime, StartTime: row.StartTime, FinishTime: row.FinishTime,
						FailReason: row.FailReason, Data: EncodeMidjourneyTaskData(&row),
					}
					charged, err := historicalTaskCharge(startupLegacyTask{Task: task, Quota: &quota})
					if err != nil {
						return startupRowError("midjourneys", int64(row.Id), err.Error())
					}
					task.TaskID = &row.MjId
					if row.Progress != "" {
						task.Progress, err = strconv.Atoi(strings.TrimSuffix(row.Progress, "%"))
						if err != nil || task.Progress < 0 || task.Progress > 100 {
							return startupRowError("midjourneys", int64(row.Id), "进度不是 0 到 100 的百分比")
						}
					}
					// MJ 公共时间字段保持毫秒；统一 owner 的创建时间使用秒。
					task.CreatedAt = row.SubmitTime / 1000
					if err := initializeHistoricalTask(&task, "midjourneys", int64(row.Id), charged); err != nil {
						return err
					}
					var existing Task
					err = tx.Where("owner_id = ?", task.OwnerID).First(&existing).Error
					if err == nil {
						if !sameHistoricalMidjourneyTask(existing, task) {
							return startupRowError("midjourneys", int64(row.Id), "目标 owner 与原记录不一致")
						}
						continue
					}
					if err != gorm.ErrRecordNotFound {
						return err
					}
					if err := tx.Create(&task).Error; err != nil {
						return startupRowError("midjourneys", int64(row.Id), "写入统一任务失败，请检查任务 ID 唯一性和数据库约束")
					}
				}
				return nil
			}).Error
		})
	}, Rollback: startupMigrationRollback}
}

func sameHistoricalMidjourneyTask(existing, expected Task) bool {
	// JSON 列可能被数据库重新排版；按旧记录的具体类型比较，保留整数精度。
	var existingView, expectedView Midjourney
	if json.Unmarshal(existing.Data, &existingView) != nil || json.Unmarshal(expected.Data, &expectedView) != nil || existingView != expectedView {
		return false
	}
	existing.Data, expected.Data = nil, nil
	// 统一表分配本地 ID 和更新时间，源记录没有这两项统一 owner 元数据。
	existing.ID, existing.UpdatedAt = expected.ID, expected.UpdatedAt
	if expected.CreatedAt == 0 {
		existing.CreatedAt = 0 // 旧记录没有提交时间时，创建时间由 GORM 生成。
	}
	return reflect.DeepEqual(existing, expected)
}
