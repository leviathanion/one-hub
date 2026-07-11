package model

import (
	"context"
	"testing"

	"gorm.io/gorm"
)

func round24Priority(value int64) *int64 { return &value }

func TestUpdateChannelsTagPriorityGuardsSnapshottedMembership(t *testing.T) {
	useTestChannelDB(t)
	insertTestChannel(t, &Channel{Id: 1, Name: "moving-out", Tag: "target", Priority: round24Priority(1)})
	insertTestChannel(t, &Channel{Id: 2, Name: "moving-in", Tag: "other", Priority: round24Priority(2)})

	callback := "test:round24_move_tag_before_priority_update"
	if err := DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "channels" {
			return
		}
		db := tx.Session(&gorm.Session{NewDB: true})
		if err := db.Exec("UPDATE channels SET tag = ? WHERE id = ?", "other", 1).Error; err != nil {
			tx.AddError(err)
			return
		}
		if err := db.Exec("UPDATE channels SET tag = ? WHERE id = ?", "target", 2).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	if err := UpdateChannelsTagPriorityWithContext(context.Background(), "target", 9); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	var channels []Channel
	if err := DB.Order("id").Find(&channels).Error; err != nil {
		t.Fatalf("load channels: %v", err)
	}
	if channels[0].Tag != "other" || channels[0].Priority == nil || *channels[0].Priority != 1 {
		t.Fatalf("moved-out candidate was modified: %+v", channels[0])
	}
	if channels[1].Tag != "target" || channels[1].Priority == nil || *channels[1].Priority != 2 {
		t.Fatalf("concurrently moved-in row was modified: %+v", channels[1])
	}
}

func TestUpdateChannelsTagPriorityUsesSingleBatchUpdate(t *testing.T) {
	useTestChannelDB(t)
	for id := 1; id <= 3; id++ {
		insertTestChannel(t, &Channel{Id: id, Name: "batch", Tag: "target", Priority: round24Priority(int64(id))})
	}

	updates := 0
	callback := "test:round24_count_priority_updates"
	if err := DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			updates++
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	if err := UpdateChannelsTagPriority("target", 9); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	if updates != 1 {
		t.Fatalf("expected one batch UPDATE, got %d", updates)
	}
	var count int64
	if err := DB.Model(&Channel{}).Where("tag = ? AND priority = ?", "target", 9).Count(&count).Error; err != nil {
		t.Fatalf("count updated channels: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected all three channels updated, got %d", count)
	}
}
