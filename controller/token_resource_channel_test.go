package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/fakeredis"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func tokenResourceChannelDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Token{}, &model.Channel{}); err != nil {
		t.Fatal(err)
	}
	oldDB, oldRedis := model.DB, config.RedisEnabled
	model.DB, config.RedisEnabled = db, false
	t.Cleanup(func() { model.DB, config.RedisEnabled = oldDB, oldRedis; sqlDB, _ := db.DB(); _ = sqlDB.Close() })
	if err := db.Create(&model.Channel{Id: 7, Name: "resource-create"}).Error; err != nil {
		t.Fatal(err)
	}
	token := model.Token{Id: 1, UserId: 1, Key: "resource-token", Name: "before", RemainQuota: 100, ExpiredTime: -1}
	token.Setting.Set(model.TokenSetting{ResourceChannelID: 7})
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func tokenResourceChannelRequest(t *testing.T, handler gin.HandlerFunc, role int, target string, body string) (bool, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("id", 1)
	c.Set("role", role)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	c.Request = httptest.NewRequest("PUT", target, bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler(c)
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response["success"] == true, response
}

func TestTokenResourceChannelUserCannotAssignOrClear(t *testing.T) {
	tokenResourceChannelDB(t)
	for _, role := range []int{config.RoleCommonUser, config.RoleReliableUser} {
		for _, setting := range []string{`{}`, `{"resource_channel_id":0}`, `{"resource_channel_id":99}`, `{"resource_channel_id":-3}`} {
			ok, response := tokenResourceChannelRequest(t, UpdateToken, role, "/", fmt.Sprintf(`{"id":1,"name":"user edit","expected_remain_quota":100,"remain_quota":100,"expired_time":-1,"setting":%s}`, setting))
			if !ok {
				t.Fatalf("user edit failed: %v", response)
			}
			token, err := model.GetTokenById(1)
			if err != nil {
				t.Fatal(err)
			}
			if token.Setting.Data().ResourceChannelID != 7 {
				t.Fatalf("user changed admin route: %+v", token.Setting.Data())
			}
			wire, _ := json.Marshal(response)
			if bytes.Contains(wire, []byte("resource_channel_id")) {
				t.Fatalf("admin setting leaked: %s", wire)
			}
		}
	}
	for _, handler := range []gin.HandlerFunc{GetToken, GetUserTokensList} {
		ok, response := tokenResourceChannelRequest(t, handler, config.RoleCommonUser, "/", "")
		if !ok {
			t.Fatalf("read failed: %v", response)
		}
		wire, _ := json.Marshal(response)
		if bytes.Contains(wire, []byte("resource_channel_id")) {
			t.Fatalf("read leaked admin route: %s", wire)
		}
	}
}

func TestTokenResourceChannelAdminValidationAndClear(t *testing.T) {
	tokenResourceChannelDB(t)
	for _, handler := range []gin.HandlerFunc{UpdateTokenByAdmin, UpdateToken} {
		for _, test := range []struct {
			id      int
			success bool
		}{{-1, false}, {999, false}, {7, true}, {0, true}} {
			ok, response := tokenResourceChannelRequest(t, handler, config.RoleAdminUser, "/", fmt.Sprintf(`{"id":1,"name":"admin edit","expected_remain_quota":100,"remain_quota":100,"expired_time":-1,"setting":{"resource_channel_id":%d}}`, test.id))
			if ok != test.success {
				t.Fatalf("channel %d: %v", test.id, response)
			}
			token, err := model.GetTokenById(1)
			if err != nil {
				t.Fatal(err)
			}
			if ok && token.Setting.Data().ResourceChannelID != test.id {
				t.Fatalf("route did not persist: %+v", token.Setting.Data())
			}
		}
	}
	for _, invalid := range []string{`1.5`, `"7"`, `9223372036854775808`} {
		ok, response := tokenResourceChannelRequest(t, UpdateTokenByAdmin, config.RoleAdminUser, "/", fmt.Sprintf(`{"id":1,"setting":{"resource_channel_id":%s}}`, invalid))
		if ok {
			t.Fatalf("malformed channel accepted: %v", response)
		}
	}
}

func TestTokenResourceChannelStatusOnlyPreservesConfiguration(t *testing.T) {
	tokenResourceChannelDB(t)
	for _, handler := range []gin.HandlerFunc{UpdateToken, UpdateTokenByAdmin} {
		ok, response := tokenResourceChannelRequest(t, handler, config.RoleAdminUser, "/?status_only=true", `{"id":1,"status":2,"setting":{"resource_channel_id":999}}`)
		if !ok {
			t.Fatal(response)
		}
		token, err := model.GetTokenById(1)
		if err != nil {
			t.Fatal(err)
		}
		if token.Setting.Data().ResourceChannelID != 7 {
			t.Fatalf("status-only update changed route: %+v", token.Setting.Data())
		}
	}
}

func TestTokenResourceChannelCreatePermission(t *testing.T) {
	db := tokenResourceChannelDB(t)
	oldSecret := viper.Get("user_token_secret")
	viper.Set("user_token_secret", "token-resource-test")
	t.Cleanup(func() { viper.Set("user_token_secret", oldSecret) })
	if err := common.InitUserToken(); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		role, channel, want int
		success             bool
	}{
		{config.RoleCommonUser, 7, 0, true}, {config.RoleReliableUser, 7, 0, true},
		{config.RoleAdminUser, 7, 7, true}, {config.RoleAdminUser, -1, 0, false}, {config.RoleAdminUser, 99, 0, false},
	} {
		ok, response := tokenResourceChannelRequest(t, AddToken, test.role, "/", fmt.Sprintf(`{"name":"created","expired_time":-1,"setting":{"resource_channel_id":%d}}`, test.channel))
		if ok != test.success {
			t.Fatalf("create role %d route %d: %v", test.role, test.channel, response)
		}
		if ok {
			var created model.Token
			if err := db.Order("id DESC").First(&created).Error; err != nil {
				t.Fatal(err)
			}
			if created.Setting.Data().ResourceChannelID != test.want {
				t.Fatalf("unexpected created route: %+v", created.Setting.Data())
			}
		}
	}
}

func TestTokenResourceChannelUpdateInvalidatesCache(t *testing.T) {
	tokenResourceChannelDB(t)
	server, err := fakeredis.Start()
	if err != nil {
		t.Fatal(err)
	}
	oldRedis, oldEnabled := commonredis.RDB, config.RedisEnabled
	commonredis.RDB, config.RedisEnabled = server.Client(), true
	cache.InitCacheManager()
	t.Cleanup(func() {
		_ = commonredis.RDB.Close()
		_ = server.Close()
		commonredis.RDB, config.RedisEnabled = oldRedis, oldEnabled
		cache.InitCacheManager()
	})
	token, err := model.GetTokenById(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.SetCache(fmt.Sprintf(model.UserTokensKey, token.Key), token, time.Minute); err != nil {
		t.Fatal(err)
	}
	ok, response := tokenResourceChannelRequest(t, UpdateTokenByAdmin, config.RoleAdminUser, "/", `{"id":1,"name":"clear","expected_remain_quota":100,"remain_quota":100,"expired_time":-1,"setting":{"resource_channel_id":0}}`)
	if !ok {
		t.Fatal(response)
	}
	refreshed, err := model.CacheGetTokenByKey(token.Key)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.Setting.Data().ResourceChannelID != 0 {
		t.Fatalf("stale token route remains cached: %+v", refreshed.Setting.Data())
	}
}
