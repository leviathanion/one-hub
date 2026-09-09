package controller

import (
	"context"
	"errors"
	"testing"

	"one-api/common/config"
	commonredis "one-api/common/redis"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestReadinessStatusDatabaseAndRedisBranches(t *testing.T) {
	originalDB := model.DB
	originalPricing := model.PricingInstance
	originalGroups := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	originalOptions := config.GlobalOption
	originalRedisEnabled := config.RedisEnabled
	originalRedisClient := commonredis.RDB
	originalFailOpenSet := viper.IsSet("responses_ws.active_lease_redis_fail_open")
	originalFailOpen := viper.GetBool("responses_ws.active_lease_redis_fail_open")
	originalProbe := probeResponsesWSActiveLeaseBackend
	viper.Reset()
	t.Cleanup(func() {
		model.DB = originalDB
		model.PricingInstance = originalPricing
		model.GlobalUserGroupRatio = originalGroups
		config.GlobalOption = originalOptions
		config.RedisEnabled = originalRedisEnabled
		commonredis.RDB = originalRedisClient
		probeResponsesWSActiveLeaseBackend = originalProbe
		viper.Reset()
		if originalFailOpenSet {
			viper.Set("responses_ws.active_lease_redis_fail_open", originalFailOpen)
		}
	})

	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open readiness test database: %v", err)
	}
	model.DB = testDB
	config.RedisEnabled = false
	commonredis.RDB = nil

	data, ready := readinessStatus(context.Background())
	if ready || data.Database != "unavailable" {
		t.Fatalf("expected database without response owner schema to stay unready, data=%+v ready=%v", data, ready)
	}
	if err := testDB.AutoMigrate(&model.ResponseOwner{}, &model.Task{}, &model.Price{}, &model.ModelInfo{}, &model.Option{}, &model.UserGroup{}, &model.PublicationVersion{}, &model.User{}, &model.Payment{}, &model.Order{}, &model.Redemption{}); err != nil {
		t.Fatalf("migrate durable owner readiness schema: %v", err)
	}
	if err := model.EnsurePublicationVersionRows(testDB); err != nil {
		t.Fatal(err)
	}
	model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	config.GlobalOption = config.NewOptionManager()
	readinessOption := "default"
	config.GlobalOption.RegisterStringOption("ReadinessOption", &readinessOption, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if err := model.LoadAndPublishOptions(context.Background()); err != nil {
		t.Fatal(err)
	}

	data, ready = readinessStatus(context.Background())
	if !ready || data.Database != "ok" || data.Redis != "disabled" || data.ResponsesWS["active_lease_backend"] != "local" {
		t.Fatalf("expected DB ok and Redis disabled to be ready, data=%+v ready=%v", data, ready)
	}
	if data.UserGroups.State != "ok" || data.UserGroups.PublishedVersion != 1 || data.UserGroups.DatabaseHead != 1 {
		t.Fatalf("expected published user group readiness, got %+v", data.UserGroups)
	}
	if data.ResponsesWS["liveness"] != "ok" {
		t.Fatalf("expected nonzero ResponsesWS liveness defaults, data=%+v", data)
	}
	viper.Set("responses_websocket_client_pong_miss_timeout_ms", 0)
	viper.Set("responses_ws.max_lifetime_ms", 0)
	data, ready = readinessStatus(context.Background())
	if !ready || data.ResponsesWS["liveness"] != "degraded" {
		t.Fatalf("explicitly disabled liveness must be visible without failing readiness, data=%+v ready=%v", data, ready)
	}
	disabled, ok := data.ResponsesWS["liveness_disabled"].([]string)
	if !ok || len(disabled) != 2 {
		t.Fatalf("expected disabled liveness dimensions, got %#v", data.ResponsesWS["liveness_disabled"])
	}
	viper.Set("responses_websocket_client_pong_miss_timeout_ms", 10000)
	viper.Set("responses_ws.max_lifetime_ms", 3600000)

	if _, err := model.BumpPublicationVersionCAS(context.Background(), testDB, model.PublicationOwnerUserGroup, 1); err != nil {
		t.Fatal(err)
	}
	if err := testDB.Callback().Query().After("gorm:query").Register("readiness_group_load_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_groups" {
			tx.AddError(errors.New("group load failed"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	data, ready = readinessStatus(context.Background())
	if ready || data.UserGroups.State != "unavailable" || data.UserGroups.PublishedVersion != 1 || data.UserGroups.DatabaseHead != 2 || data.UserGroups.LastSyncError == "" {
		t.Fatalf("policy publication failure not exposed: %+v ready=%v", data.UserGroups, ready)
	}
	if err := testDB.Callback().Query().Remove("readiness_group_load_failure"); err != nil {
		t.Fatal(err)
	}
	data, ready = readinessStatus(context.Background())
	if !ready || data.UserGroups.State != "ok" || data.UserGroups.PublishedVersion != 2 || data.UserGroups.LastSyncError != "" {
		t.Fatalf("policy publication did not recover: %+v ready=%v", data.UserGroups, ready)
	}

	model.DB = nil
	data, ready = readinessStatus(context.Background())
	if ready || data.Database != "unavailable" || data.UserGroups.State != "unavailable" || data.UserGroups.LastSyncError == "" {
		t.Fatalf("expected missing DB to fail readiness, data=%+v ready=%v", data, ready)
	}
	model.DB = testDB

	config.RedisEnabled = true
	probeResponsesWSActiveLeaseBackend = func(context.Context) error { return nil }
	data, ready = readinessStatus(context.Background())
	if !ready || data.Redis != "ok" || data.ResponsesWS["active_lease_backend"] != "redis" || data.ResponsesWS["active_lease_probe"] != "ok" {
		t.Fatalf("expected equivalent Redis lease probe success, data=%+v ready=%v", data, ready)
	}
	probeResponsesWSActiveLeaseBackend = func(context.Context) error { return errors.New("lua denied") }
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
