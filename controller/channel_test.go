package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestBuildChannelsForCreateKeepsCodexJSONIntact(t *testing.T) {
	channel := model.Channel{
		Type: config.ChannelTypeCodex,
		Key: "{\n" +
			`  "access_token": "access-token",` + "\n" +
			`  "refresh_token": "refresh-token"` + "\n" +
			"}",
		Name: "codex",
	}

	channels := buildChannelsForCreate(channel)
	if len(channels) != 1 {
		t.Fatalf("expected a single codex channel, got %d", len(channels))
	}
	if channels[0].Key != channel.Key {
		t.Fatalf("expected codex key to stay intact, got %q", channels[0].Key)
	}
}

func TestBuildChannelsForCreateSplitsNonCodexKeysByNewline(t *testing.T) {
	channel := model.Channel{
		Type: config.ChannelTypeOpenAI,
		Key:  "key-1\nkey-2",
		Name: "openai",
	}

	channels := buildChannelsForCreate(channel)
	if len(channels) != 2 {
		t.Fatalf("expected two channels, got %d", len(channels))
	}
	if channels[0].Key != "key-1" || channels[1].Key != "key-2" {
		t.Fatalf("unexpected keys after split: %#v", channels)
	}
	if channels[1].Name != "openai_2" {
		t.Fatalf("expected suffixed second channel name, got %q", channels[1].Name)
	}
}

func TestAddChannelRejectsExistingTag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "existing",
		Key:    "sk-existing",
		Group:  "default",
		Models: "gpt-old",
		Tag:    "shared-tag",
	}).Error; err != nil {
		t.Fatalf("expected existing tagged channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"type":1,"name":"new","key":"sk-new","models":"gpt-new","group":"default","tag":"shared-tag","base_url":"https://new.example"}`)
	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/", commonTest.RequestJSONConfig(), body)

	AddChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if resp.Success {
		t.Fatalf("expected existing tag create to be rejected, got %s", recorder.Body.String())
	}
	if resp.Message != "标签已存在，请到标签编辑里新增 key" {
		t.Fatalf("unexpected rejection message: %q", resp.Message)
	}

	var count int64
	if err := model.DB.Model(&model.Channel{}).Where("tag = ?", "shared-tag").Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected rejected create not to insert a new tagged channel, got count=%d", count)
	}
}

func TestAddChannelAllowsNewTag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	body := bytes.NewBufferString(`{"type":1,"name":"new","key":"sk-new","models":"gpt-new","group":"default","tag":"fresh-tag"}`)
	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/", commonTest.RequestJSONConfig(), body)

	AddChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected new tag create to succeed, got %s", recorder.Body.String())
	}

	var count int64
	if err := model.DB.Model(&model.Channel{}).Where("tag = ?", "fresh-tag").Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != 1 {
		t.Fatalf("expected new tag create to insert one channel, got count=%d", count)
	}
}
