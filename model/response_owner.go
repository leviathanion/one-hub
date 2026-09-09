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
	ResponseOwnerStateActive  = "active"
	ResponseOwnerStateDeleted = "deleted"

	ResponseOwnerRetention          = 30 * 24 * time.Hour
	ResponseOwnerTombstoneRetention = 7 * 24 * time.Hour
)

var (
	ErrResponseOwnerNotFound = errors.New("response owner not found")
	ErrResponseOwnerConflict = errors.New("response owner conflicts with existing ownership")
)

var responseOwnerSchemaColumns = []string{
	"id",
	"provider_namespace",
	"response_scope_incarnation",
	"response_id",
	"user_id",
	"token_id",
	"channel_id",
	"state",
	"created_at",
	"updated_at",
	"deleted_at",
	"expires_at",
}

// ResponseOwner is the durable routing and authorization boundary for stored
// Responses. The upstream payload stays at the provider; one-hub only retains
// the minimum identity needed to route lifecycle calls without leaking IDs
// across users.
type ResponseOwner struct {
	ID                       uint64     `json:"id" gorm:"primaryKey;autoIncrement"`
	ProviderNamespace        string     `json:"provider_namespace" gorm:"not null;size:64;uniqueIndex:idx_response_owner_provider_identity,priority:1"`
	ResponseScopeIncarnation string     `json:"response_scope_incarnation" gorm:"not null;size:191;uniqueIndex:idx_response_owner_provider_identity,priority:2"`
	ResponseID               string     `json:"response_id" gorm:"not null;size:191;uniqueIndex:idx_response_owner_provider_identity,priority:3;uniqueIndex:idx_response_owner_public_identity,priority:2"`
	UserID                   int        `json:"user_id" gorm:"not null;index;uniqueIndex:idx_response_owner_public_identity,priority:1"`
	TokenID                  int        `json:"token_id" gorm:"not null;default:0"`
	ChannelID                int        `json:"channel_id" gorm:"not null;index"`
	State                    string     `json:"state" gorm:"not null;size:16;index"`
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
	DeletedAt                *time.Time `json:"deleted_at,omitempty"`
	ExpiresAt                time.Time  `json:"expires_at" gorm:"not null;index"`
}

func NewResponseOwner(responseID string, userID, tokenID, channelID int, now time.Time, identity ...string) (*ResponseOwner, error) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || userID <= 0 || channelID <= 0 {
		return nil, errors.New("response id, user id, and channel id are required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	providerNamespace := "unknown-provider"
	responseScope := "provider-wide"
	if len(identity) > 0 && strings.TrimSpace(identity[0]) != "" {
		providerNamespace = strings.TrimSpace(identity[0])
	}
	if len(identity) > 1 && strings.TrimSpace(identity[1]) != "" {
		responseScope = strings.TrimSpace(identity[1])
	}
	return &ResponseOwner{
		ProviderNamespace:        providerNamespace,
		ResponseScopeIncarnation: responseScope,
		ResponseID:               responseID,
		UserID:                   userID,
		TokenID:                  tokenID,
		ChannelID:                channelID,
		State:                    ResponseOwnerStateActive,
		CreatedAt:                now,
		UpdatedAt:                now,
		ExpiresAt:                now.Add(ResponseOwnerRetention + ResponseOwnerTombstoneRetention),
	}, nil
}

func ConservativeResponseOwnerIdentity(channel *Channel) (providerNamespace, responseScope string, err error) {
	if channel == nil || channel.Id <= 0 || channel.Type == 0 {
		return "", "", errors.New("channel identity is required")
	}
	return fmt.Sprintf("channel-type:%d", channel.Type), "provider-wide", nil
}

func CreateResponseOwner(ctx context.Context, owner *ResponseOwner) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	return createResponseOwner(DB.WithContext(responseOwnerContext(ctx)), owner)
}

func createResponseOwner(db *gorm.DB, owner *ResponseOwner) error {
	if db == nil || owner == nil {
		return errors.New("response owner database and record are required")
	}
	owner.ResponseID = strings.TrimSpace(owner.ResponseID)
	if owner.ResponseID == "" || owner.UserID <= 0 || owner.ChannelID <= 0 || strings.TrimSpace(owner.ProviderNamespace) == "" || strings.TrimSpace(owner.ResponseScopeIncarnation) == "" || owner.State != ResponseOwnerStateActive || owner.ExpiresAt.IsZero() {
		return errors.New("invalid response owner record")
	}

	result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(owner)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 1 {
		return nil
	}

	var existing ResponseOwner
	err := db.Where("user_id = ? AND response_id = ?", owner.UserID, owner.ResponseID).First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = db.Where("provider_namespace = ? AND response_scope_incarnation = ? AND response_id = ?", owner.ProviderNamespace, owner.ResponseScopeIncarnation, owner.ResponseID).First(&existing).Error
	}
	if err != nil {
		return err
	}
	if existing.State == ResponseOwnerStateActive && existing.UserID == owner.UserID && existing.ChannelID == owner.ChannelID && existing.ProviderNamespace == owner.ProviderNamespace && existing.ResponseScopeIncarnation == owner.ResponseScopeIncarnation {
		return nil
	}
	return fmt.Errorf("%w: response_id=%s", ErrResponseOwnerConflict, owner.ResponseID)
}

func GetResponseOwner(ctx context.Context, responseID string, userID int) (*ResponseOwner, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	return getResponseOwner(DB.WithContext(responseOwnerContext(ctx)), responseID, time.Now(), userID)
}

// CheckResponseOwnerSchema verifies the durable Responses routing schema without
// mutating it. Replica nodes intentionally skip AutoMigrate and stay unready
// until a master has completed the migration during a rolling deployment.
func CheckResponseOwnerSchema(ctx context.Context) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	return checkResponseOwnerSchema(DB.WithContext(responseOwnerContext(ctx)))
}

func checkResponseOwnerSchema(db *gorm.DB) error {
	if db == nil {
		return errors.New("response owner database is required")
	}
	var owners []ResponseOwner
	if err := db.Select(responseOwnerSchemaColumns).Limit(1).Find(&owners).Error; err != nil {
		return err
	}
	for _, index := range []string{"idx_response_owner_provider_identity", "idx_response_owner_public_identity"} {
		if !db.Migrator().HasIndex(&ResponseOwner{}, index) {
			return fmt.Errorf("response owner schema is missing unique index %s", index)
		}
	}
	return nil
}

func getResponseOwner(db *gorm.DB, responseID string, now time.Time, userIDs ...int) (*ResponseOwner, error) {
	responseID = strings.TrimSpace(responseID)
	if db == nil || responseID == "" {
		return nil, ErrResponseOwnerNotFound
	}
	query := db.Where("response_id = ? AND expires_at > ?", responseID, now)
	if len(userIDs) > 0 && userIDs[0] > 0 {
		query = query.Where("user_id = ?", userIDs[0])
	}
	var owners []ResponseOwner
	err := query.Limit(2).Find(&owners).Error
	if err == nil && len(owners) == 0 {
		return nil, ErrResponseOwnerNotFound
	}
	if err != nil {
		return nil, err
	}
	if len(owners) > 1 {
		return nil, fmt.Errorf("%w: response_id=%s", ErrResponseOwnerConflict, responseID)
	}
	return &owners[0], nil
}

func TombstoneResponseOwner(ctx context.Context, responseID string, userID int) error {
	if DB == nil {
		return errors.New("database is not initialized")
	}
	return tombstoneResponseOwner(DB.WithContext(responseOwnerContext(ctx)), responseID, userID, time.Now())
}

func tombstoneResponseOwner(db *gorm.DB, responseID string, userID int, now time.Time) error {
	responseID = strings.TrimSpace(responseID)
	if db == nil || responseID == "" || userID <= 0 {
		return ErrResponseOwnerNotFound
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var owner ResponseOwner
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("response_id = ? AND user_id = ? AND expires_at > ?", responseID, userID, now).
			First(&owner).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrResponseOwnerNotFound
		}
		if err != nil {
			return err
		}
		if owner.State == ResponseOwnerStateDeleted {
			return nil
		}
		if owner.State != ResponseOwnerStateActive {
			return ErrResponseOwnerNotFound
		}
		owner.State = ResponseOwnerStateDeleted
		owner.DeletedAt = &now
		owner.UpdatedAt = now
		return tx.Save(&owner).Error
	})
}

func DeleteExpiredResponseOwners(ctx context.Context, now time.Time) (int64, error) {
	if DB == nil {
		return 0, errors.New("database is not initialized")
	}
	if now.IsZero() {
		now = time.Now()
	}
	result := DB.WithContext(responseOwnerContext(ctx)).Where("expires_at <= ?", now).Delete(&ResponseOwner{})
	return result.RowsAffected, result.Error
}

func responseOwnerContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}
