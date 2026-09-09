package base

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	sunoProvider "one-api/providers/suno"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOwnerBoundTaskProviderUsesRetiredChannelIncarnation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		retire func(*testing.T, *gorm.DB)
	}{
		{
			name: "disabled",
			retire: func(t *testing.T, db *gorm.DB) {
				if err := db.Model(&model.Channel{}).Where("id = ?", 41).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "soft-deleted",
			retire: func(t *testing.T, db *gorm.DB) {
				if err := db.Delete(&model.Channel{}, 41).Error; err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&model.Channel{}, &model.Task{}); err != nil {
				t.Fatal(err)
			}
			originalDB := model.DB
			model.DB = db
			t.Cleanup(func() { model.DB = originalDB })

			baseURL, proxy := "https://suno.example", ""
			channel := &model.Channel{Id: 41, Type: config.ChannelTypeSuno, Name: "suno-owner", Key: "key", Status: config.ChannelStatusEnabled, BaseURL: &baseURL, Proxy: &proxy}
			if err := db.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			providerID := "suno-parent"
			parent := &model.Task{Platform: model.TaskPlatformSuno, UserId: 7, ChannelId: channel.Id, TaskID: &providerID}
			if err := db.Session(&gorm.Session{SkipHooks: true}).Create(parent).Error; err != nil {
				t.Fatal(err)
			}
			tc.retire(t, db)

			gin.SetMode(gin.TestMode)
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", nil)
			ctx.Set("id", 7)
			task := &TaskBase{Platform: model.TaskPlatformSuno, C: ctx, OriginTaskID: providerID, OriginalModel: "chirp-v3-0"}
			if err := task.HandleOriginTaskID(); err != nil {
				t.Fatalf("authorize parent owner: %v", err)
			}
			provider, err := task.GetProviderByModel()
			if err != nil {
				t.Fatalf("load retired owner channel provider: %v", err)
			}
			if _, ok := provider.(*sunoProvider.SunoProvider); !ok || provider.GetChannel().Id != channel.Id || task.OwnerChannelIncarnationID() != channel.Id {
				t.Fatalf("unexpected owner provider=%T channel=%v owner=%d", provider, provider.GetChannel(), task.OwnerChannelIncarnationID())
			}
		})
	}
}

func TestOriginTaskWithoutChannelFailsInsteadOfOrdinaryRouting(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Task{}); err != nil {
		t.Fatal(err)
	}
	originalDB := model.DB
	model.DB = db
	t.Cleanup(func() { model.DB = originalDB })

	providerID := "missing-channel-parent"
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Task{Platform: model.TaskPlatformSuno, UserId: 7, TaskID: &providerID}).Error; err != nil {
		t.Fatal(err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", nil)
	ctx.Set("id", 7)
	task := &TaskBase{Platform: model.TaskPlatformSuno, C: ctx, OriginTaskID: providerID, OriginalModel: "chirp-v3-0"}
	if err := task.HandleOriginTaskID(); err == nil || task.OwnerChannelIncarnationID() != 0 {
		t.Fatalf("invalid parent channel silently became ordinary routing: owner=%d err=%v", task.OwnerChannelIncarnationID(), err)
	}
}
