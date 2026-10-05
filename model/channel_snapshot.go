package model

import (
	"context"
	"errors"
	"one-api/common/credentials"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var ErrChannelVersionConflict = errors.New("渠道已被修改，请重新读取后再提交")

// ChannelCredentialStore adapts the common credential protocol to the channel's
// single authoritative row. It does not interpret provider or OAuth semantics.
type ChannelCredentialStore struct{}

func (ChannelCredentialStore) Load(ctx context.Context, id int) (credentials.Snapshot, error) {
	if DB == nil {
		return credentials.Snapshot{}, errors.New("database is not initialized")
	}
	row, err := GetChannelIncarnationByID(ctx, id)
	if err != nil {
		return credentials.Snapshot{}, err
	}
	return credentials.Snapshot{ChannelID: row.Id, Type: row.Type, Key: row.Key, Version: row.Version, InternalState: row.InternalState, Deleted: row.DeletedAt.Valid}, nil
}

func (ChannelCredentialStore) CompareAndSwap(ctx context.Context, expected credentials.Snapshot, data []byte, key *string) (bool, error) {
	if DB == nil {
		return false, errors.New("database is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	updates := map[string]any{"internal_state": datatypes.JSON(data), "version": gorm.Expr("version + 1")}
	if key != nil {
		updates["key"] = *key
	}
	result := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(gormlogger.Silent)}).Model(&Channel{}).
		Where("id = ? AND version = ?", expected.ChannelID, expected.Version).Updates(updates)
	// Even an ambiguous write can have committed. Never leave the old credential
	// snapshot routable while its durable outcome is being resolved.
	if key != nil && (result.Error != nil || result.RowsAffected > 0) {
		ChannelGroup.failClosedChannels([]int{expected.ChannelID})
	}
	return result.RowsAffected == 1, result.Error
}
