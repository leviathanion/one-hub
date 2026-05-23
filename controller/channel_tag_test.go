package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useControllerChannelTagTestDB(t *testing.T) {
	t.Helper()

	if logger.Logger == nil {
		logger.SetupLogger()
	}

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration for test database, got %v", err)
	}

	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
	})
}

func TestUpdateChannelsTagUsesSubmittedFieldsForSync(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	if err := model.DB.Create(&model.Channel{
		Id:             1,
		Type:           config.ChannelTypeOpenAI,
		Name:           "tagged-one",
		Key:            "sk-one",
		Group:          "legacy",
		Models:         "gpt-old",
		Tag:            "field-team",
		Other:          "legacy-other",
		TestModel:      "legacy-test",
		AllowExtraBody: true,
	}).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"key":"sk-one","models":"gpt-new","allow_extra_body":false}`)
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel_tag/field-team", commonTest.RequestJSONConfig(), body)
	ctx.Params = gin.Params{{Key: "tag", Value: "field-team"}}

	UpdateChannelsTag(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted tagged channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" {
		t.Fatalf("expected submitted models update, got %q", persisted.Models)
	}
	if persisted.AllowExtraBody {
		t.Fatal("expected submitted allow_extra_body=false to sync")
	}
	if persisted.Other != "legacy-other" || persisted.TestModel != "legacy-test" || persisted.Group != "legacy" {
		t.Fatalf("expected omitted config fields to remain, got other=%q test_model=%q group=%q", persisted.Other, persisted.TestModel, persisted.Group)
	}
}

func TestAddChannelToTagReturnsCreatedChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "tagged-one",
		Key:    "sk-one",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "member-team",
	}).Error; err != nil {
		t.Fatalf("expected channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"name":"created-member","key":"sk-two"}`)
	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel_tag/member-team/channel", commonTest.RequestJSONConfig(), body)
	ctx.Params = gin.Params{{Key: "tag", Value: "member-team"}}

	AddChannelToTag(ctx)

	var resp struct {
		Success bool          `json:"success"`
		Message string        `json:"message"`
		Data    model.Channel `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected success response, got %s", recorder.Body.String())
	}
	if resp.Data.Id == 0 || resp.Data.Name != "created-member" || resp.Data.Key != "sk-two" || resp.Data.Tag != "member-team" {
		t.Fatalf("expected created channel data in response, got %+v", resp.Data)
	}
}
