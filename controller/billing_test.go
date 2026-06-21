package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerBillingTestDB(t *testing.T) {
	t.Helper()

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatalf("expected billing test schema migration to succeed, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestBillingUnlimitedTokenUsesTypedUserQuotaFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerBillingTestDB(t)

	originalDisplayInCurrencyEnabled := config.DisplayInCurrencyEnabled
	config.DisplayInCurrencyEnabled = false
	t.Cleanup(func() {
		config.DisplayInCurrencyEnabled = originalDisplayInCurrencyEnabled
	})

	user := model.User{
		Username:    "billing-user",
		Password:    "password123",
		DisplayName: "billing",
		Status:      config.UserStatusEnabled,
		Quota:       100,
		UsedQuota:   25,
		Group:       "default",
		AffCode:     "billing-aff",
	}
	if err := model.DB.Create(&user).Error; err != nil {
		t.Fatalf("expected user fixture to persist, got %v", err)
	}

	token := model.Token{
		Id:             77,
		UserId:         user.Id,
		Status:         config.TokenStatusEnabled,
		Name:           "unlimited",
		ExpiredTime:    -1,
		RemainQuota:    999,
		UsedQuota:      888,
		UnlimitedQuota: true,
		Group:          "default",
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&token).Error; err != nil {
		t.Fatalf("expected token fixture to persist, got %v", err)
	}

	subscriptionCtx, subscriptionRecorder := commonTest.GetContext(http.MethodGet, "/dashboard/billing/subscription", nil, nil)
	subscriptionCtx.Set("token_id", token.Id)
	subscriptionCtx.Set("id", user.Id)

	GetSubscription(subscriptionCtx)

	if subscriptionRecorder.Code != http.StatusOK {
		t.Fatalf("expected subscription request to complete, got %d body=%s", subscriptionRecorder.Code, subscriptionRecorder.Body.String())
	}
	var subscription OpenAISubscriptionResponse
	if err := json.Unmarshal(subscriptionRecorder.Body.Bytes(), &subscription); err != nil {
		t.Fatalf("expected subscription JSON response, got %v", err)
	}
	if subscription.SoftLimitUSD != 125 || subscription.HardLimitUSD != 125 || subscription.SystemHardLimitUSD != 125 {
		t.Fatalf("expected unlimited token subscription to use user quota totals, got %+v", subscription)
	}

	usageCtx, usageRecorder := commonTest.GetContext(http.MethodGet, "/dashboard/billing/usage", nil, nil)
	usageCtx.Set("token_id", token.Id)
	usageCtx.Set("id", user.Id)

	GetUsage(usageCtx)

	if usageRecorder.Code != http.StatusOK {
		t.Fatalf("expected usage request to complete, got %d body=%s", usageRecorder.Code, usageRecorder.Body.String())
	}
	var usage OpenAIUsageResponse
	if err := json.Unmarshal(usageRecorder.Body.Bytes(), &usage); err != nil {
		t.Fatalf("expected usage JSON response, got %v", err)
	}
	if usage.TotalUsage != 2500 {
		t.Fatalf("expected unlimited token usage to use user used_quota, got %+v", usage)
	}
}
