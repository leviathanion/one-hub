package midjourney

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/middleware"
	"one-api/model"
	provider "one-api/providers/midjourney"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFixI043_MidjourneyParentWorkRequiresCurrentAuthorization(t *testing.T) {
	for _, operation := range []struct{ name, body string }{
		{"action", `{"taskId":"parent-task","customId":"MJ::JOB::upsample::1::parent-task"}`},
		{"change", `{"taskId":"parent-task","action":"UPSCALE","index":1}`},
		{"simple-change", `{"content":"parent-task u1"}`},
		{"modal", `{"taskId":"parent-task","prompt":"edit"}`},
	} {
		for _, grant := range []string{"revoked_group", "current_group", "backup", "admin_explicit", "admin_without_selector", "private_backup"} {
			t.Run(operation.name+"/"+grant, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":1,"description":"accepted","result":"child-task"}`))
				}))
				t.Cleanup(upstream.Close)
				oldHTTPClient := requester.HTTPClient
				requester.HTTPClient = upstream.Client()
				t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
				db, credential := setupI043Midjourney(t, upstream.URL, grant)
				var moneyWrites, taskCreates atomic.Int32
				if err := db.Callback().Update().After("gorm:update").Register("test:i043:money_writes", func(tx *gorm.DB) {
					if (tx.Statement.Table == "users" || tx.Statement.Table == "tokens") && strings.Contains(tx.Statement.SQL.String(), "quota") {
						moneyWrites.Add(1)
					}
				}); err != nil {
					t.Fatal(err)
				}
				if err := db.Callback().Create().After("gorm:create").Register("test:i043:task_create", func(tx *gorm.DB) {
					if tx.Statement.Table == "tasks" {
						taskCreates.Add(1)
					}
				}); err != nil {
					t.Fatal(err)
				}
				router := gin.New()
				router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/mj/submit/"+operation.name, strings.NewReader(operation.body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("mj-api-secret", credential)
				router.ServeHTTP(recorder, request)
				var user model.User
				var token model.Token
				var count int64
				if err := db.First(&user, 1).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.First(&token, 1).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(&model.Task{}).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				allowed := grant == "current_group" || grant == "backup" || grant == "admin_explicit"
				if !allowed {
					if calls.Load() != 0 || moneyWrites.Load() != 0 || taskCreates.Load() != 0 || user.Quota != 1000 || token.RemainQuota != 1000 || count != 1 {
						t.Fatalf("撤权后仍创建工作: upstream=%d moneyWrites=%d taskCreates=%d user=%d token=%d tasks=%d status=%d body=%s", calls.Load(), moneyWrites.Load(), taskCreates.Load(), user.Quota, token.RemainQuota, count, recorder.Code, recorder.Body.String())
					}
					if recorder.Code < 400 {
						t.Fatalf("撤权请求未明确拒绝: %d %s", recorder.Code, recorder.Body.String())
					}
					return
				}
				wantReserve := 20
				if grant == "admin_explicit" {
					wantReserve = 10
				}
				if recorder.Code != http.StatusOK || calls.Load() != 1 || user.Quota != 1000-wantReserve || token.RemainQuota != 1000-wantReserve || count != 2 {
					t.Fatalf("合法父任务工作未按实际组准入: upstream=%d user=%d token=%d tasks=%d status=%d body=%s", calls.Load(), user.Quota, token.RemainQuota, count, recorder.Code, recorder.Body.String())
				}
				child, err := model.GetTaskByTaskId(model.TaskPlatformMidjourney, 1, "child-task")
				if err != nil || child == nil || child.ChannelId != 1 || child.ReservedQuota != int64(wantReserve) {
					t.Fatalf("新工作移动了 owner 或计费组错误: %+v %v", child, err)
				}
			})
		}
	}
}

func setupI043Midjourney(t *testing.T, upstream, grant string) (*gorm.DB, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	oldDB, oldPricing, oldGroups := model.DB, model.PricingInstance, model.GlobalUserGroupRatio
	oldChannels, oldRules, oldMatch, oldModelGroups := model.ChannelGroup.Channels, model.ChannelGroup.Rule, model.ChannelGroup.Match, model.ChannelGroup.ModelGroup
	oldRedis, oldLog, oldBatch, oldLogger, oldOptions := config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled, logger.Logger, config.GlobalOption
	config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled, logger.Logger = false, false, false, zap.NewNop()
	config.GlobalOption = config.NewOptionManager()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	model.DB = db
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{}}
	t.Cleanup(func() {
		for _, limiter := range model.GlobalUserGroupRatio.APILimiter {
			if stopper, ok := limiter.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
		model.DB, model.PricingInstance, model.GlobalUserGroupRatio = oldDB, oldPricing, oldGroups
		model.ChannelGroup.Channels, model.ChannelGroup.Rule, model.ChannelGroup.Match, model.ChannelGroup.ModelGroup = oldChannels, oldRules, oldMatch, oldModelGroups
		config.RedisEnabled, config.LogConsumeEnabled, config.BatchUpdateEnabled, logger.Logger, config.GlobalOption = oldRedis, oldLog, oldBatch, oldLogger, oldOptions
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}, &model.Channel{}, &model.Task{}, &model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	for _, group := range []model.UserGroup{{Symbol: "paid", Ratio: 2, Public: grant != "private_backup"}, {Symbol: "basic", Ratio: 1}} {
		if err := group.Create(); err != nil {
			t.Fatal(err)
		}
	}
	currentGroup, role := "basic", config.RoleCommonUser
	if grant == "current_group" {
		currentGroup = "paid"
	}
	if grant == "admin_explicit" || grant == "admin_without_selector" {
		role = config.RoleAdminUser
	}
	if err := db.Create(&model.User{Id: 1, Username: "i043-user", Password: "test-password", AccessToken: "i043-access", AffCode: "i043-aff", Status: config.UserStatusEnabled, Role: role, Group: currentGroup, Quota: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	credential := strings.Repeat("t", 48)
	token := &model.Token{Id: 1, UserId: 1, Key: credential, Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}
	if grant == "backup" || grant == "private_backup" {
		token.BackupGroup = "paid"
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
	proxy := ""
	channel := &model.Channel{Id: 1, Type: config.ChannelTypeMidjourney, Name: "paid-owner", Key: "i043-upstream-key", Group: "paid", Models: "mj_upscale,mj_modal,mj_variation", Status: config.ChannelStatusEnabled, BaseURL: &upstream, Proxy: &proxy}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.ChannelGroup.Load(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mj_upscale", "mj_modal", "mj_variation"} {
		if err := db.Create(&model.Price{Model: name, Type: model.TimesPriceType, Input: 0.01}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	parentID := "parent-task"
	parent := &model.Task{Platform: model.TaskPlatformMidjourney, UserId: 1, TokenID: 1, ChannelId: 1, TaskID: &parentID, Status: model.TaskStatusSuccess, ProviderState: model.TaskProviderStateClosed, ProviderNamespace: "task-platform:midjourney", ProviderTaskScopeIncarnation: "provider-wide", RequestFingerprint: fmt.Sprintf("%064d", 1), Action: provider.MjActionImagine, Data: model.EncodeMidjourneyTaskData(&model.Midjourney{Prompt: "original", Mode: "fast"})}
	if err := db.Create(parent).Error; err != nil {
		t.Fatal(err)
	}
	if grant == "admin_explicit" {
		credential += "#1"
	}
	return db, credential
}
