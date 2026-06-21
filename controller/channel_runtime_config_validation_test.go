package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestGetModelListRejectsInvalidRuntimeOtherBeforeProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := bytes.NewBufferString(fmt.Sprintf(`{"type":%d,"key":"test-key","other":"v1beta"}`, config.ChannelTypeGemini))
	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/models", commonTest.RequestJSONConfig(), body)

	GetModelList(ctx)

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected JSON response, got %v", err)
	}
	if resp.Success || !strings.Contains(resp.Message, "other") {
		t.Fatalf("expected invalid other to be rejected before model list provider creation, got success=%v message=%q", resp.Success, resp.Message)
	}
}

func TestTestChannelRejectsInvalidRuntimeOtherBeforeProvider(t *testing.T) {
	channel := &model.Channel{
		Type:      config.ChannelTypeGemini,
		Key:       "test-key",
		TestModel: "gemini-pro",
		Other:     "v1beta",
	}

	openaiErr, err := testChannel(channel, "gemini-pro")
	if openaiErr != nil {
		t.Fatalf("expected validation to fail before provider request, got provider error %+v", openaiErr)
	}
	if err == nil || !strings.Contains(err.Error(), "other") {
		t.Fatalf("expected invalid other validation error, got %v", err)
	}
}
