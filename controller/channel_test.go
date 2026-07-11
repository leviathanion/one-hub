package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestAddChannelCanonicalizesCodexLegacyRequiredWebsocketMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	payload, err := json.Marshal(map[string]any{
		"type":   config.ChannelTypeCodex,
		"name":   "codex-required",
		"key":    "codex-token",
		"models": "gpt-5",
		"group":  "default",
		"other":  `{"websocket_mode":"required"}`,
	})
	if err != nil {
		t.Fatalf("expected request payload to marshal, got %v", err)
	}
	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/", commonTest.RequestJSONConfig(), bytes.NewBuffer(payload))

	AddChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected Codex legacy websocket_mode create to succeed, got %s", recorder.Body.String())
	}

	var persisted model.Channel
	if err := model.DB.Where("name = ?", "codex-required").First(&persisted).Error; err != nil {
		t.Fatalf("expected created Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Other != `{"websocket_mode":"force"}` {
		t.Fatalf("expected Codex websocket_mode to persist as force, got %q", persisted.Other)
	}
}

func TestUpdateChannelCanonicalizesCodexLegacyRequiredWebsocketMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeCodex,
		Name:   "codex-old",
		Key:    "codex-token",
		Group:  "default",
		Models: "gpt-5",
	}).Error; err != nil {
		t.Fatalf("expected Codex channel fixture to persist, got %v", err)
	}

	payload, err := json.Marshal(map[string]any{
		"id":     1,
		"type":   config.ChannelTypeCodex,
		"name":   "codex-new",
		"key":    "codex-token",
		"models": "gpt-5",
		"group":  "default",
		"other":  `{"websocket_mode":"required"}`,
	})
	if err != nil {
		t.Fatalf("expected request payload to marshal, got %v", err)
	}
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), bytes.NewBuffer(payload))

	UpdateChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected Codex legacy websocket_mode update to succeed, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Codex channel lookup to succeed, got %v", err)
	}
	if persisted.Other != `{"websocket_mode":"force"}` {
		t.Fatalf("expected Codex websocket_mode to persist as force, got %q", persisted.Other)
	}
}

func TestUpdateChannelPartialResponseContainsCompletePersistedChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	fixture := &model.Channel{
		Id: 1, Type: config.ChannelTypeOpenAI, Name: "before", Key: "persisted-key",
		Group: "persisted-group", Models: "gpt-old,gpt-stable",
	}
	if err := model.DB.Create(fixture).Error; err != nil {
		t.Fatalf("create channel fixture: %v", err)
	}
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), bytes.NewBufferString(`{"id":1,"name":"after"}`))

	UpdateChannel(ctx)

	var response struct {
		Success bool          `json:"success"`
		Message string        `json:"message"`
		Data    model.Channel `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !response.Success {
		t.Fatalf("partial update failed: %s", recorder.Body.String())
	}
	if response.Data.Name != "after" || response.Data.Models != fixture.Models || response.Data.Group != fixture.Group || response.Data.Key != fixture.Key {
		t.Fatalf("partial response omitted persisted fields: %#v", response.Data)
	}
}

func TestUpdateChannelOmittedOtherPreservesAzureRequiredOther(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	const originalOther = `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}`
	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-old",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-old",
		Other:  originalOther,
	}).Error; err != nil {
		t.Fatalf("expected Azure channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"id":1,"type":3,"name":"azure-new","key":"sk-azure","models":"gpt-new","group":"default"}`)
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), body)

	UpdateChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected omitted other update to succeed, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.Other != originalOther {
		t.Fatalf("expected omitted other to be preserved while models update, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestUpdateChannelOmittedOtherPreservesOpenAIOptionalOther(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	const originalOther = `{"responses_ws_transport":"http_bridge","responses_ws_native":false}`
	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeOpenAI,
		Name:   "openai-old",
		Key:    "sk-openai",
		Group:  "default",
		Models: "gpt-old",
		Other:  originalOther,
	}).Error; err != nil {
		t.Fatalf("expected OpenAI channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(fmt.Sprintf(`{"id":1,"type":%d,"name":"openai-new","key":"sk-openai","models":"gpt-new","group":"default"}`, config.ChannelTypeOpenAI))
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), body)

	UpdateChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if !resp.Success {
		t.Fatalf("expected omitted optional other update to succeed, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted OpenAI channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-new" || persisted.Other != originalOther {
		t.Fatalf("expected omitted optional other to be preserved while models update, models=%q other=%q", persisted.Models, persisted.Other)
	}
}

func TestUpdateChannelExplicitEmptyOtherRejectsAzureAndLeavesDBUnchanged(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	const originalOther = `{"api_version":"2024-05-01-preview"}`
	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure-old",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-old",
		Other:  originalOther,
	}).Error; err != nil {
		t.Fatalf("expected Azure channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"id":1,"type":3,"name":"azure-new","key":"sk-azure","models":"gpt-new","group":"default","other":""}`)
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), body)

	UpdateChannel(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if resp.Success {
		t.Fatalf("expected explicit empty other update to be rejected, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Models != "gpt-old" || persisted.Other != originalOther || persisted.Name != "azure-old" {
		t.Fatalf("expected rejected update not to mutate DB, name=%q models=%q other=%q", persisted.Name, persisted.Models, persisted.Other)
	}
}

func TestBatchUpdateChannelsAzureApiRejectsLegacyValuePayload(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)

	if err := model.DB.Create(&model.Channel{
		Id:     1,
		Type:   config.ChannelTypeAzure,
		Name:   "azure",
		Key:    "sk-azure",
		Group:  "default",
		Models: "gpt-5",
		Other:  `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}`,
	}).Error; err != nil {
		t.Fatalf("expected Azure channel fixture to persist, got %v", err)
	}

	body := bytes.NewBufferString(`{"ids":[1],"value":"2024-06-01"}`)
	ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/batch/azure_api", commonTest.RequestJSONConfig(), body)

	BatchUpdateChannelsAzureApi(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if resp.Success || resp.Message != "api_version is required" {
		t.Fatalf("expected legacy value payload to be rejected, got %s", recorder.Body.String())
	}

	persisted, err := model.GetChannelById(1)
	if err != nil {
		t.Fatalf("expected persisted Azure channel lookup to succeed, got %v", err)
	}
	if persisted.Other != `{"api_version":"2024-05-01-preview","responses_ws_transport":"http_bridge"}` {
		t.Fatalf("expected rejected legacy payload not to mutate other, got %q", persisted.Other)
	}
}
