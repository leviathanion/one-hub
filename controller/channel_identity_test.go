package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestChannelEditPreservesResourceBindingsWhileUpdatingOriginalRow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	if err := model.DB.AutoMigrate(&model.Task{}, &model.ResponseOwner{}); err != nil {
		t.Fatal(err)
	}
	headers := `{"OpenAI-Organization":"org-a","OpenAI-Project":"project-a"}`
	original := model.Channel{Type: config.ChannelTypeOpenAI, Name: "original", Key: "key-a", Models: "gpt-5", Group: "default", ModelHeaders: &headers}
	if err := original.Insert(); err != nil {
		t.Fatal(err)
	}
	task := model.Task{OwnerID: "task-owner", ChannelId: original.Id}
	response := model.ResponseOwner{ProviderNamespace: "openai", ResponseScopeIncarnation: "scope", ResponseID: "response-a", UserID: 1, ChannelID: original.Id, State: model.ResponseOwnerStateActive, ExpiresAt: time.Now().Add(time.Hour)}
	if err := model.DB.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.Create(&response).Error; err != nil {
		t.Fatal(err)
	}
	for _, update := range []map[string]any{
		{"model_headers": `{"openai-organization":"org-b","OpenAI-Project":"project-a"}`},
		{"model_headers": `{"OpenAI-Organization":"org-a","OPENAI-PROJECT":"project-b"}`},
		{"model_headers": `{}`},
		{"key": "key-another-account"},
	} {
		update["id"] = original.Id
		body, _ := json.Marshal(update)
		ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), bytes.NewBuffer(body))
		UpdateChannel(ctx)
		var result struct {
			Success bool   `json:"success"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if !result.Success {
			t.Fatalf("identity update failed: %s", recorder.Body.String())
		}
	}
	var count int64
	if err := model.DB.Model(&model.Channel{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("edit must not clone channel: count=%d err=%v", count, err)
	}
	if err := model.DB.First(&task, task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&response, response.ID).Error; err != nil {
		t.Fatal(err)
	}
	persisted, err := model.GetChannelById(original.Id)
	if err != nil || persisted.Key != "key-another-account" || *persisted.ModelHeaders != "{}" || task.ChannelId != original.Id || response.ChannelID != original.Id {
		t.Fatalf("original owner binding changed: channel=%+v task=%d response=%d err=%v", persisted, task.ChannelId, response.ChannelID, err)
	}
}

func TestChannelTagCanEditHeadersAndBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	useControllerChannelTagTestDB(t)
	headers := `{"OpenAI-Organization":"org-a"}`
	baseURL := "https://api.example.test"
	channel := model.Channel{Type: config.ChannelTypeOpenAI, Name: "tag-member", Key: "key-a", Tag: "tag", BaseURL: &baseURL, ModelHeaders: &headers}
	if err := channel.Insert(); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"model_headers":"{\"OPENAI-ORGANIZATION\":\"org-b\"}"}`,
		`{"base_url":"https://other.example.test"}`,
	} {
		ctx, recorder := commonTest.GetContext(http.MethodPut, "/api/channel_tag/tag", commonTest.RequestJSONConfig(), bytes.NewBufferString(body))
		ctx.Params = gin.Params{{Key: "tag", Value: "tag"}}
		UpdateChannelsTag(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil || !result.Success {
			t.Fatalf("tag identity edit failed: %s err=%v", recorder.Body.String(), err)
		}
	}
	persisted, err := model.GetChannelById(channel.Id)
	if err != nil || persisted.GetBaseURL() != "https://other.example.test" || persisted.Key != "key-a" || persisted.ModelHeaders == nil || *persisted.ModelHeaders != `{"OPENAI-ORGANIZATION":"org-b"}` {
		t.Fatalf("tag edit did not preserve desired values: %v", err)
	}

}
