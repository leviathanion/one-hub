package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	ResourceOwnerReserved           = "reserved"
	ResourceOwnerBound              = "bound"
	ResourceReservationSubmit       = "submit"
	ResourceReservationTaskDerived  = "task_derived"
	ResourceOwnerLimit              = 10000
	ResourceOwnerTombstoneRetention = 7 * 24 * time.Hour
	ResourceUploadRetention         = 7 * 24 * time.Hour
	ResourceChatAudioRetention      = 7 * 24 * time.Hour
	ResourceReservationGrace        = 5 * time.Minute
)

var (
	ErrResourceOwnerNotFound      = errors.New("resource owner not found")
	ErrResourceOwnerConflict      = errors.New("resource ownership conflict")
	ErrResourceOwnerCapacity      = errors.New("resource owner capacity exceeded")
	ErrResourceReservationExpired = errors.New("resource reservation expired or released")
)

// ResourceOwner 只保存本地授权、固定渠道和容量事实，不判断上游资源是否可用。
// NULL upstream_id 允许多个未绑定预留；父级局部 ID 使用父 owner 授权，不建此表记录。
type ResourceOwner struct {
	ID                   uint64     `json:"id" gorm:"primaryKey;autoIncrement"`
	Kind                 string     `json:"kind" gorm:"not null;size:32;uniqueIndex:idx_resource_public,priority:2;uniqueIndex:idx_resource_provider,priority:3;uniqueIndex:idx_resource_task_slot,priority:2"`
	UpstreamID           *string    `json:"upstream_id,omitempty" gorm:"size:191;uniqueIndex:idx_resource_public,priority:3;uniqueIndex:idx_resource_provider,priority:4"`
	UserID               int        `json:"user_id" gorm:"not null;index;uniqueIndex:idx_resource_public,priority:1"`
	TokenID              int        `json:"token_id" gorm:"not null"`
	ChannelID            int        `json:"channel_id" gorm:"not null;index"`
	ProviderNamespace    string     `json:"provider_namespace" gorm:"not null;size:64;uniqueIndex:idx_resource_provider,priority:1"`
	ProviderScope        string     `json:"provider_scope" gorm:"not null;size:128;uniqueIndex:idx_resource_provider,priority:2"`
	ParentID             *uint64    `json:"parent_id,omitempty"`
	Phase                string     `json:"phase" gorm:"not null;size:16;index"`
	ReservationKind      string     `json:"reservation_kind" gorm:"not null;size:16"`
	ReservationExpiresAt *time.Time `json:"reservation_expires_at,omitempty" gorm:"index"`
	TaskOwnerID          *string    `json:"task_owner_id,omitempty" gorm:"size:36;uniqueIndex:idx_resource_task_slot,priority:1"`
	Slot                 *string    `json:"slot,omitempty" gorm:"size:64;uniqueIndex:idx_resource_task_slot,priority:3"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	RetainUntil          *time.Time `json:"retain_until,omitempty" gorm:"index"`
	DeleteObservedAt     *time.Time `json:"delete_observed_at,omitempty"`
	UpstreamExpiresAt    *time.Time `json:"upstream_expires_at,omitempty"`
	BatchResultEndpoint  string     `json:"-" gorm:"size:64"`
	BatchResultObserved  bool       `json:"-" gorm:"not null;default:false"`
	BatchOutputFileID    string     `json:"-" gorm:"size:191"`
	BatchErrorFileID     string     `json:"-" gorm:"size:191"`
}

type ResourceOwnerReservation struct {
	Kind                             string
	UserID, TokenID, ChannelID       int
	ProviderNamespace, ProviderScope string
	ParentID                         *uint64
	TaskOwnerID, Slot                string
	SubmitDeadline                   time.Time
}

// 外部 Task acceptance 事务可传入 tx，使转交、绑定与 Task 保存原子提交。
type ResourceOwnerRepository struct{ DB *gorm.DB }

func NewResourceOwnerRepository(db *gorm.DB) *ResourceOwnerRepository {
	return &ResourceOwnerRepository{DB: db}
}

func (r *ResourceOwnerRepository) database(ctx context.Context) (*gorm.DB, error) {
	if r == nil || r.DB == nil {
		return nil, errors.New("resource owner database is not initialized")
	}
	return r.DB.WithContext(responseOwnerContext(ctx)), nil
}

// 不改变值的 UPDATE 先取得写锁：SQLite 没有 FOR UPDATE，仍须在计数前串行化写事务。
func lockResourceOwnerUser(tx *gorm.DB, userID int) error {
	if userID <= 0 {
		return ErrResourceOwnerNotFound
	}
	if err := tx.Model(&User{}).Where("id = ?", userID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
		return err
	}
	var user User
	if err := tx.Select("id").Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", userID).Take(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrResourceOwnerNotFound
		}
		return err
	}
	return nil
}

func resourceOwnerCount(tx *gorm.DB, userID int, now time.Time) (int64, error) {
	var count int64
	err := tx.Model(&ResourceOwner{}).Where("user_id = ? AND delete_observed_at IS NULL", userID).
		Where("retain_until IS NULL OR retain_until > ?", now).
		Where("phase = ? OR (phase = ? AND (reservation_kind = ? OR reservation_expires_at > ?))", ResourceOwnerBound, ResourceOwnerReserved, ResourceReservationTaskDerived, now).Count(&count).Error
	if err != nil {
		return 0, err
	}
	var responseCount int64
	err = tx.Model(&ResponseOwner{}).Where("user_id = ? AND batch_owner_id IS NOT NULL AND state = ? AND expires_at > ?", userID, ResponseOwnerStateActive, now).Count(&responseCount).Error
	return count + responseCount, err
}

func validResourceIdentity(value string, max int) bool {
	return strings.TrimSpace(value) != "" && len(value) <= max
}

func (r *ResourceOwnerRepository) Reserve(ctx context.Context, spec ResourceOwnerReservation) (*ResourceOwner, error) {
	owners, err := r.ReserveMany(ctx, []ResourceOwnerReservation{spec})
	if err != nil {
		return nil, err
	}
	return owners[0], nil
}

func (r *ResourceOwnerRepository) ReserveMany(ctx context.Context, specs []ResourceOwnerReservation) ([]*ResourceOwner, error) {
	db, err := r.database(ctx)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 || len(specs) > ResourceOwnerLimit {
		return nil, ErrResourceOwnerCapacity
	}
	now := time.Now()
	userID := specs[0].UserID
	owners := make([]*ResourceOwner, 0, len(specs))
	for _, s := range specs {
		if s.UserID != userID || s.ChannelID <= 0 || !validResourceIdentity(s.Kind, 32) || !validResourceIdentity(s.ProviderNamespace, 64) || !validResourceIdentity(s.ProviderScope, 128) || !s.SubmitDeadline.After(now) || (s.TaskOwnerID == "") != (s.Slot == "") || len(s.TaskOwnerID) > 36 || len(s.Slot) > 64 {
			return nil, errors.New("invalid resource reservation identity or finite submission deadline")
		}
		expires := s.SubmitDeadline.Add(ResourceReservationGrace)
		o := &ResourceOwner{Kind: s.Kind, UserID: s.UserID, TokenID: s.TokenID, ChannelID: s.ChannelID, ProviderNamespace: s.ProviderNamespace, ProviderScope: s.ProviderScope, ParentID: s.ParentID, Phase: ResourceOwnerReserved, ReservationKind: ResourceReservationSubmit, ReservationExpiresAt: &expires, CreatedAt: now, UpdatedAt: now}
		if s.TaskOwnerID != "" {
			task, slot := s.TaskOwnerID, s.Slot
			o.TaskOwnerID = &task
			o.Slot = &slot
		}
		owners = append(owners, o)
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		count, err := resourceOwnerCount(tx, userID, now)
		if err != nil {
			return err
		}
		if count+int64(len(owners)) > ResourceOwnerLimit {
			return ErrResourceOwnerCapacity
		}
		for _, o := range owners {
			if o.ParentID != nil {
				var parent ResourceOwner
				err := tx.Where("id = ? AND user_id = ? AND channel_id = ? AND phase = ?", *o.ParentID, userID, o.ChannelID, ResourceOwnerBound).Where("retain_until IS NULL OR retain_until > ?", now).Take(&parent).Error
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrResourceOwnerNotFound
				}
				if err != nil {
					return err
				}
			}
			if err := tx.Create(o).Error; err != nil {
				return err
			}
		}
		return nil
	})
	return owners, err
}

func (r *ResourceOwnerRepository) Bind(ctx context.Context, reservationID uint64, userID int, upstreamID string) (*ResourceOwner, error) {
	db, err := r.database(ctx)
	if err != nil {
		return nil, err
	}
	if !validResourceIdentity(upstreamID, 191) {
		return nil, errors.New("invalid upstream resource identity")
	}
	var owner ResourceOwner
	now := time.Now()
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ?", reservationID, userID).Take(&owner).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrResourceOwnerNotFound
			}
			return err
		}
		if owner.Phase == ResourceOwnerBound {
			if owner.RetainUntil != nil && !owner.RetainUntil.After(now) {
				return ErrResourceOwnerNotFound
			}
			if owner.UpstreamID != nil && *owner.UpstreamID == upstreamID {
				return nil
			}
			return ErrResourceOwnerConflict
		}
		if owner.Phase != ResourceOwnerReserved || (owner.ReservationKind == ResourceReservationSubmit && (owner.ReservationExpiresAt == nil || !owner.ReservationExpiresAt.After(now))) {
			return ErrResourceReservationExpired
		}
		var collisions []ResourceOwner
		if err := tx.Where("kind = ? AND upstream_id = ? AND (user_id = ? OR (provider_namespace = ? AND provider_scope = ?))", owner.Kind, upstreamID, userID, owner.ProviderNamespace, owner.ProviderScope).Limit(2).Find(&collisions).Error; err != nil {
			return err
		}
		if len(collisions) != 0 {
			if len(collisions) != 1 || !sameResourceBinding(&collisions[0], &owner, upstreamID) || (collisions[0].RetainUntil != nil && !collisions[0].RetainUntil.After(now)) {
				return ErrResourceOwnerConflict
			}
			// 上游重放同一资源时复用归属，释放本次容量；保留原审计与删除观察，不延长留存。
			if err := tx.Where("id = ? AND phase = ?", owner.ID, ResourceOwnerReserved).Delete(&ResourceOwner{}).Error; err != nil {
				return err
			}
			owner = collisions[0]
			return nil
		}
		count, err := resourceOwnerCount(tx, userID, now)
		if err != nil {
			return err
		}
		if count > ResourceOwnerLimit {
			return ErrResourceOwnerCapacity
		}
		owner.UpstreamID = &upstreamID
		owner.Phase = ResourceOwnerBound
		owner.ReservationExpiresAt = nil
		owner.UpdatedAt = now
		if owner.Kind == "upload" {
			expiry := now.Add(ResourceUploadRetention)
			owner.RetainUntil = &expiry
		}
		if owner.Kind == "chat_audio" {
			expiry := now.Add(ResourceChatAudioRetention)
			owner.RetainUntil = &expiry
		}
		result := tx.Model(&ResourceOwner{}).Where("id = ? AND phase = ?", owner.ID, ResourceOwnerReserved).Updates(map[string]any{"upstream_id": upstreamID, "phase": ResourceOwnerBound, "reservation_expires_at": nil, "updated_at": now, "retain_until": owner.RetainUntil})
		if result.Error != nil {
			if IsUniqueConstraintError(result.Error) {
				return fmt.Errorf("%w: %w", ErrResourceOwnerConflict, result.Error)
			}
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrResourceReservationExpired
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &owner, nil
}

func sameResourceBinding(existing, reserved *ResourceOwner, upstreamID string) bool {
	// SQL collation 不能证明 opaque ID 相等；TokenID 只记录创建者，授权范围是用户。
	if existing.Phase != ResourceOwnerBound || existing.UpstreamID == nil || *existing.UpstreamID != upstreamID || existing.Kind != reserved.Kind || existing.UserID != reserved.UserID || existing.ChannelID != reserved.ChannelID || existing.ProviderNamespace != reserved.ProviderNamespace || existing.ProviderScope != reserved.ProviderScope {
		return false
	}
	if (existing.ParentID == nil) != (reserved.ParentID == nil) || (existing.ParentID != nil && *existing.ParentID != *reserved.ParentID) {
		return false
	}
	if (existing.TaskOwnerID == nil) != (reserved.TaskOwnerID == nil) || (existing.TaskOwnerID != nil && *existing.TaskOwnerID != *reserved.TaskOwnerID) {
		return false
	}
	return (existing.Slot == nil) == (reserved.Slot == nil) && (existing.Slot == nil || *existing.Slot == *reserved.Slot)
}

func (r *ResourceOwnerRepository) Get(ctx context.Context, kind, upstreamID string, userID int) (*ResourceOwner, error) {
	db, err := r.database(ctx)
	if err != nil {
		return nil, err
	}
	if userID <= 0 {
		return nil, ErrResourceOwnerNotFound
	}
	var owner ResourceOwner
	err = db.Where("user_id = ? AND kind = ? AND upstream_id = ? AND phase = ?", userID, kind, upstreamID, ResourceOwnerBound).Where("retain_until IS NULL OR retain_until > ?", time.Now()).Take(&owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrResourceOwnerNotFound
	}
	if err != nil {
		return nil, err
	}
	// SQL collations may ignore case or trailing spaces. An opaque provider ID
	// needs byte-exact proof even when the database query reports a match.
	if owner.UpstreamID == nil || *owner.UpstreamID != upstreamID {
		return nil, ErrResourceOwnerNotFound
	}
	return &owner, nil
}

func (r *ResourceOwnerRepository) ObserveDelete(ctx context.Context, kind, upstreamID string, userID int) error {
	db, err := r.database(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	retain := now.Add(ResourceOwnerTombstoneRetention)
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		owner, err := NewResourceOwnerRepository(tx).Get(ctx, kind, upstreamID, userID)
		if err != nil {
			return err
		}
		if owner.DeleteObservedAt != nil {
			return nil
		}
		return tx.Model(&ResourceOwner{}).Where("id = ?", owner.ID).Updates(map[string]any{"delete_observed_at": now, "retain_until": retain, "updated_at": now}).Error
	})
}

func (r *ResourceOwnerRepository) Release(ctx context.Context, id uint64, userID int) error {
	db, err := r.database(ctx)
	if err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		return tx.Where("id = ? AND user_id = ? AND phase = ?", id, userID, ResourceOwnerReserved).Delete(&ResourceOwner{}).Error
	})
}

// 必须在保存父 Task acceptance 的同一外部事务内调用；SQL 保存持有者，重启可恢复。
func (r *ResourceOwnerRepository) TransferToTask(ctx context.Context, userID int, taskOwnerID string, expectedReservationIDs ...uint64) error {
	db, err := r.database(ctx)
	if err != nil {
		return err
	}
	if taskOwnerID == "" {
		return errors.New("task owner is required")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		now := time.Now()
		var owners []ResourceOwner
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND task_owner_id = ? AND phase = ?", userID, taskOwnerID, ResourceOwnerReserved).Find(&owners).Error; err != nil {
			return err
		}
		if len(owners) == 0 {
			return ErrResourceReservationExpired
		}
		if len(expectedReservationIDs) > 0 {
			found := make(map[uint64]bool, len(owners))
			for _, owner := range owners {
				found[owner.ID] = true
			}
			for _, id := range expectedReservationIDs {
				if !found[id] {
					return ErrResourceReservationExpired
				}
			}
		}
		for _, o := range owners {
			if o.ReservationKind == ResourceReservationSubmit && (o.ReservationExpiresAt == nil || !o.ReservationExpiresAt.After(now)) {
				return ErrResourceReservationExpired
			}
		}
		return tx.Model(&ResourceOwner{}).Where("user_id = ? AND task_owner_id = ? AND phase = ?", userID, taskOwnerID, ResourceOwnerReserved).Updates(map[string]any{"reservation_kind": ResourceReservationTaskDerived, "reservation_expires_at": nil, "updated_at": now}).Error
	})
}

func (r *ResourceOwnerRepository) ReleaseTaskSlots(ctx context.Context, userID int, taskOwnerID string) error {
	db, err := r.database(ctx)
	if err != nil {
		return err
	}
	if taskOwnerID == "" {
		return errors.New("task owner is required")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		return tx.Where("user_id = ? AND task_owner_id = ? AND phase = ?", userID, taskOwnerID, ResourceOwnerReserved).Delete(&ResourceOwner{}).Error
	})
}

// 提交调用栈在 acceptance commit-unknown 后只可清理仍属 submit 的槽。
// 已原子转交 Task 的派生义务必须留给持久任务，不能凭旧内存状态释放。
func (r *ResourceOwnerRepository) ReleaseSubmittingTaskSlots(ctx context.Context, userID int, taskOwnerID string) error {
	db, err := r.database(ctx)
	if err != nil {
		return err
	}
	if taskOwnerID == "" {
		return errors.New("task owner is required")
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := lockResourceOwnerUser(tx, userID); err != nil {
			return err
		}
		return tx.Where("user_id = ? AND task_owner_id = ? AND phase = ? AND reservation_kind = ?", userID, taskOwnerID, ResourceOwnerReserved, ResourceReservationSubmit).Delete(&ResourceOwner{}).Error
	})
}

func (r *ResourceOwnerRepository) TaskSlots(ctx context.Context, userID int, taskOwnerID string) ([]ResourceOwner, error) {
	db, err := r.database(ctx)
	if err != nil {
		return nil, err
	}
	if userID <= 0 || taskOwnerID == "" {
		return nil, ErrResourceOwnerNotFound
	}
	var owners []ResourceOwner
	err = db.Where("user_id = ? AND task_owner_id = ?", userID, taskOwnerID).Order("id").Find(&owners).Error
	return owners, err
}

func ReserveResourceOwner(ctx context.Context, spec ResourceOwnerReservation) (*ResourceOwner, error) {
	return NewResourceOwnerRepository(DB).Reserve(ctx, spec)
}
func BindResourceOwner(ctx context.Context, id uint64, userID int, upstreamID string) (*ResourceOwner, error) {
	return NewResourceOwnerRepository(DB).Bind(ctx, id, userID, upstreamID)
}
func GetResourceOwner(ctx context.Context, kind, upstreamID string, userID int) (*ResourceOwner, error) {
	return NewResourceOwnerRepository(DB).Get(ctx, kind, upstreamID, userID)
}
func ObserveResourceOwnerDelete(ctx context.Context, kind, upstreamID string, userID int) error {
	return NewResourceOwnerRepository(DB).ObserveDelete(ctx, kind, upstreamID, userID)
}
func ReleaseResourceOwnerReservation(ctx context.Context, id uint64, userID int) error {
	return NewResourceOwnerRepository(DB).Release(ctx, id, userID)
}

func DeleteExpiredResourceOwners(ctx context.Context, now time.Time) (int64, error) {
	return NewResourceOwnerRepository(DB).Cleanup(ctx, now)
}

func (r *ResourceOwnerRepository) Cleanup(ctx context.Context, now time.Time) (int64, error) {
	db, err := r.database(ctx)
	if err != nil {
		return 0, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	// DELETE 的谓词在 SQL 写锁内重新检查，不使用扫描快照删除已转交给 Task 的预留。
	result := db.Where("(phase = ? AND reservation_kind = ? AND reservation_expires_at <= ?) OR (phase = ? AND retain_until <= ?)", ResourceOwnerReserved, ResourceReservationSubmit, now, ResourceOwnerBound, now).Delete(&ResourceOwner{})
	return result.RowsAffected, result.Error
}

func CheckResourceOwnerSchema(ctx context.Context) error {
	db, err := NewResourceOwnerRepository(DB).database(ctx)
	if err != nil {
		return err
	}
	var owners []ResourceOwner
	if err := db.Select("id, kind, upstream_id, user_id, token_id, channel_id, provider_namespace, provider_scope, parent_id, phase, reservation_kind, reservation_expires_at, task_owner_id, slot, created_at, updated_at, retain_until, delete_observed_at, upstream_expires_at, batch_result_endpoint, batch_result_observed, batch_output_file_id, batch_error_file_id").Limit(1).Find(&owners).Error; err != nil {
		return err
	}
	for _, index := range []string{"idx_resource_public", "idx_resource_provider", "idx_resource_task_slot"} {
		if !db.Migrator().HasIndex(&ResourceOwner{}, index) {
			return fmt.Errorf("resource owner schema is missing unique index %s", index)
		}
	}
	return nil
}
