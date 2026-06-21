package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/model"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReadinessStatusDatabaseAndRedisBranches(t *testing.T) {
	originalDB := model.DB
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalFailOpenSet := viper.IsSet("responses_ws.active_lease_redis_fail_open")
	originalFailOpen := viper.GetBool("responses_ws.active_lease_redis_fail_open")
	viper.Reset()
	t.Cleanup(func() {
		model.DB = originalDB
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
		viper.Reset()
		if originalFailOpenSet {
			viper.Set("responses_ws.active_lease_redis_fail_open", originalFailOpen)
		}
	})

	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
	if err != nil {
		t.Fatalf("open readiness test database: %v", err)
	}
	model.DB = testDB
	config.RedisEnabled = false
	commonredis.RDB = nil

	data, ready := readinessStatus(context.Background())
	if !ready || data.Database != "ok" || data.Redis != "disabled" || data.ResponsesWS["active_lease_backend"] != "local" {
		t.Fatalf("expected DB ok and Redis disabled to be ready, data=%+v ready=%v", data, ready)
	}

	model.DB = nil
	data, ready = readinessStatus(context.Background())
	if ready || data.Database != "unavailable" {
		t.Fatalf("expected missing DB to fail readiness, data=%+v ready=%v", data, ready)
	}
	model.DB = testDB

	config.RedisEnabled = true
	commonredis.RDB = nil
	viper.Set("responses_ws.active_lease_redis_fail_open", true)
	data, ready = readinessStatus(context.Background())
	if !ready || data.Redis != "degraded_fail_open" || data.ResponsesWS["active_lease_backend"] != "redis_degraded_fail_open" {
		t.Fatalf("expected Redis unavailable fail-open readiness degradation, data=%+v ready=%v", data, ready)
	}

	viper.Set("responses_ws.active_lease_redis_fail_open", false)
	data, ready = readinessStatus(context.Background())
	if ready || data.Redis != "unavailable" || data.ResponsesWS["active_lease_backend"] != "redis" {
		t.Fatalf("expected Redis unavailable fail-closed readiness failure, data=%+v ready=%v", data, ready)
	}
}
