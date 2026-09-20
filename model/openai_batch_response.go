package model

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// BindOpenAIBatchResponse 将派生容量转移到既有 ResponseOwner。Batch 来源键
// 仅用于幂等和容量统计，不创建第二份可授予访问权的资源记录。
func BindOpenAIBatchResponse(ctx context.Context, task *Task, reservationID uint64, slot, responseID string) error {
	if DB == nil || task == nil || task.OwnerID == "" || task.Platform != TaskPlatformOpenAIBatch || reservationID == 0 || slot == "" || !validResourceIdentity(responseID, 191) {
		return ErrTaskIdentity
	}
	return DB.WithContext(normalizeModelContext(ctx)).Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, task.UserId); err != nil {
			return err
		}
		var existing ResponseOwner
		err := tx.Where("batch_owner_id = ? AND batch_slot = ?", task.OwnerID, slot).Take(&existing).Error
		if err == nil {
			if !existing.ExpiresAt.After(time.Now()) {
				return ErrResponseOwnerNotFound
			}
			if existing.ResponseID == responseID && existing.UserID == task.UserId && existing.ChannelID == task.ChannelId {
				return nil
			}
			return ErrResponseOwnerConflict
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var capacity ResourceOwner
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND channel_id = ? AND task_owner_id = ? AND slot = ? AND kind = ? AND phase = ? AND reservation_kind = ?", reservationID, task.UserId, task.ChannelId, task.OwnerID, slot, "response", ResourceOwnerReserved, ResourceReservationTaskDerived).Take(&capacity).Error; err != nil {
			return ErrResourceReservationExpired
		}
		owner, err := NewResponseOwner(responseID, task.UserId, task.TokenID, task.ChannelId, time.Now(), task.ProviderNamespace, task.ProviderTaskScopeIncarnation)
		if err != nil {
			return err
		}
		owner.BatchOwnerID = &task.OwnerID
		owner.BatchSlot = &slot
		if err := tx.Create(owner).Error; err != nil {
			if IsUniqueConstraintError(err) {
				return ErrResponseOwnerConflict
			}
			return err
		}
		return tx.Where("id = ?", capacity.ID).Delete(&ResourceOwner{}).Error
	})
}

// 文件来源投影与真实 ID 绑定同事务；任务清理后仍可验证已登记派生 ID。
func BindOpenAIBatchFile(ctx context.Context, task *Task, reservationID uint64, id, endpoint, role string) error {
	if DB == nil || task == nil || task.Platform != TaskPlatformOpenAIBatch {
		return ErrTaskIdentity
	}
	if role != "output" && role != "error" {
		return ErrTaskIdentity
	}
	return DB.WithContext(normalizeModelContext(ctx)).Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, task.UserId); err != nil {
			return err
		}
		var root ResourceOwner
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("kind = ? AND user_id = ? AND channel_id = ? AND task_owner_id = ? AND slot = ? AND phase = ?", "batch", task.UserId, task.ChannelId, task.OwnerID, "batch", ResourceOwnerBound).Take(&root).Error; err != nil {
			return err
		}
		observedID := root.BatchOutputFileID
		column := "batch_output_file_id"
		if role == "error" {
			observedID = root.BatchErrorFileID
			column = "batch_error_file_id"
		}
		// 重复回执不重新要求子 File 元数据仍存活，也不重新创建子 owner。
		if observedID != "" {
			if observedID == id {
				return nil
			}
			return ErrResourceOwnerConflict
		}
		owner, err := NewResourceOwnerRepository(tx).Bind(ctx, reservationID, task.UserId, id)
		if err != nil {
			return err
		}
		if owner.Kind != "file" || owner.ChannelID != task.ChannelId || owner.TaskOwnerID == nil || *owner.TaskOwnerID != task.OwnerID || owner.Slot == nil || *owner.Slot != role {
			return ErrTaskIdentity
		}
		if err := tx.Model(&ResourceOwner{}).Where("id = ?", owner.ID).Update("batch_result_endpoint", endpoint).Error; err != nil {
			return err
		}
		return tx.Model(&ResourceOwner{}).Where("id = ?", root.ID).Update(column, id).Error
	})
}

// MarkOpenAIBatchFileObserved 保存不可变结果文件已完整通过资源交付屏障的
// 本地事实，不延长任何资源留存，也不声明上游任务成功或子资源仍可访问。
func MarkOpenAIBatchFileObserved(ctx context.Context, owner *ResourceOwner) error {
	if DB == nil || owner == nil || owner.ID == 0 || owner.Kind != "file" || owner.UpstreamID == nil || owner.TaskOwnerID == nil || owner.Slot == nil || (*owner.Slot != "output" && *owner.Slot != "error") {
		return ErrResourceOwnerNotFound
	}
	result := DB.WithContext(normalizeModelContext(ctx)).Model(&ResourceOwner{}).
		Where("id = ? AND user_id = ? AND channel_id = ? AND kind = ? AND upstream_id = ? AND task_owner_id = ? AND slot = ? AND phase = ? AND batch_result_endpoint = ?", owner.ID, owner.UserID, owner.ChannelID, "file", *owner.UpstreamID, *owner.TaskOwnerID, *owner.Slot, ResourceOwnerBound, owner.BatchResultEndpoint).
		Where("retain_until IS NULL OR retain_until > ?", time.Now()).Update("batch_result_observed", true)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		current, err := GetResourceOwner(ctx, "file", *owner.UpstreamID, owner.UserID)
		if err != nil {
			return err
		}
		if current.ID != owner.ID || current.ChannelID != owner.ChannelID || current.TaskOwnerID == nil || *current.TaskOwnerID != *owner.TaskOwnerID || current.BatchResultEndpoint != owner.BatchResultEndpoint || !current.BatchResultObserved {
			return ErrResourceOwnerConflict
		}
	}
	return nil
}
