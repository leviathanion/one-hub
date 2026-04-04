package controller

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseCodexAuthFileEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	fileWriter, err := writer.CreateFormFile("file", "codex-user.json")
	if err != nil {
		t.Fatalf("CreateFormFile returned error: %v", err)
	}
	if _, err = fileWriter.Write([]byte(`{"type":"codex","email":"dev@example.com","access_token":"access-token","refresh_token":"refresh-token"}`)); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/codex/auth-files/parse", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	ParseCodexAuthFile(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	var resp struct {
		Success bool `json:"success"`
		Data    struct {
			Credentials string `json:"credentials"`
			Email       string `json:"email"`
		} `json:"data"`
	}
	if err = json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if !resp.Success {
		t.Fatalf("expected success response, got body %s", recorder.Body.String())
	}
	if resp.Data.Email != "dev@example.com" {
		t.Fatalf("expected email to round-trip, got %q", resp.Data.Email)
	}
	if resp.Data.Credentials == "" {
		t.Fatalf("expected normalized credentials JSON, got empty string")
	}
}

func TestParseCodexChannelTemplateRejectsEmptyModels(t *testing.T) {
	_, err := parseCodexChannelTemplate(`{"type":101,"name":"codex","models":"","group":"default"}`)
	if err == nil {
		t.Fatalf("expected empty models to be rejected")
	}
}

func TestParseCodexChannelTemplateRejectsInvalidRuntimeConfigJSON(t *testing.T) {
	_, err := parseCodexChannelTemplate(`{"type":101,"name":"codex","models":"gpt-5","group":"default","other":"{\"prompt_cache_key_strategy\":"}`)
	if err == nil {
		t.Fatal("expected invalid runtime config json to be rejected")
	}
}

func TestParseCodexChannelTemplateRejectsUnsupportedCodexOtherFields(t *testing.T) {
	_, err := parseCodexChannelTemplate(`{"type":101,"name":"codex","models":"gpt-5","group":"default","other":"{\"user_agent_regex\":\"^Codex/\"}"}`)
	if err == nil {
		t.Fatal("expected unsupported Codex other fields to be rejected")
	}
}
