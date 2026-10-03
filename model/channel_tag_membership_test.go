package model

import (
	"errors"
	"testing"

	"one-api/common/config"

	"gorm.io/gorm"
)

func TestUpdateChannelsTagConfigRollsBackMembershipConflict(t *testing.T) {
	useTestChannelDB(t)
	modelValue := "old-model"
	insertTestChannel(t, &Channel{Id: 27011, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "stays", Tag: "target", Key: "key-a", Models: modelValue})
	insertTestChannel(t, &Channel{Id: 27012, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "moves-out", Tag: "target", Key: "key-b", Models: modelValue})
	insertTestChannel(t, &Channel{Id: 27013, Type: config.ChannelTypeOpenAI, Status: config.ChannelStatusEnabled, Name: "moves-in", Tag: "other", Key: "key-c", Models: modelValue})

	callback := "test:swap_tag_before_config_update"
	if err := DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "channels" {
			return
		}
		db := tx.Session(&gorm.Session{NewDB: true})
		if err := db.Exec("UPDATE channels SET tag = ? WHERE id = ?", "other", 27012).Error; err != nil {
			tx.AddError(err)
			return
		}
		if err := db.Exec("UPDATE channels SET tag = ? WHERE id = ?", "target", 27013).Error; err != nil {
			tx.AddError(err)
		}
	}); err != nil {
		t.Fatalf("register callback: %v", err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })

	fields := ChannelTagSubmittedFields{"models": {}}
	if err := UpdateChannelsTagWithSubmittedFields("target", &Channel{Models: "new-model"}, fields); !errors.Is(err, ErrChannelVersionConflict) {
		t.Fatalf("update tag config: %v", err)
	}

	var channels []Channel
	if err := DB.Where("id IN ?", []int{27011, 27012, 27013}).Order("id").Find(&channels).Error; err != nil {
		t.Fatalf("load channels: %v", err)
	}
	if channels[0].Models != modelValue {
		t.Fatalf("conflicted batch partially updated a member: %+v", channels[0])
	}
	if channels[1].Tag != "target" || channels[1].Models != modelValue {
		t.Fatalf("moved-out member was updated: %+v", channels[1])
	}
	if channels[2].Tag != "other" || channels[2].Models != modelValue {
		t.Fatalf("moved-in member was adopted by the batch: %+v", channels[2])
	}
}
