package controller

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/logger"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

func TestParseCodexChannelTemplateCanonicalizesLegacyRequiredWebsocketMode(t *testing.T) {
	channel, err := parseCodexChannelTemplate(`{"type":101,"name":"codex","models":"gpt-5","group":"default","other":"{\"websocket_mode\":\"required\"}"}`)
	if err != nil {
		t.Fatalf("expected legacy Codex websocket_mode template to be accepted, got %v", err)
	}
	if channel.Other != `{"websocket_mode":"force"}` {
		t.Fatalf("expected legacy Codex websocket_mode to canonicalize to force, got %q", channel.Other)
	}
}

func TestParseCodexChannelTemplateRejectsUnsupportedCodexOtherFields(t *testing.T) {
	_, err := parseCodexChannelTemplate(`{"type":101,"name":"codex","models":"gpt-5","group":"default","other":"{\"user_agent_regex\":\"^Codex/\"}"}`)
	if err == nil {
		t.Fatal("expected unsupported Codex other fields to be rejected")
	}
}

func TestImportCodexAuthFilesStreamsMoreThanDefaultMultipartPartLimit(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{
		channelAfterFiles: true,
		files:             buildCodexAuthUploadFiles(1001, "files"),
	})

	resp := decodeCodexImportResponse(t, recorder)
	if !resp.Success {
		t.Fatalf("expected import success, got body %s", recorder.Body.String())
	}
	if resp.Data.Count != 1001 {
		t.Fatalf("expected response count 1001, got %d", resp.Data.Count)
	}
	assertControllerChannelCount(t, 1001)
}

func TestImportCodexAuthFilesAcceptsChannelBeforeAndFileField(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{
		files: []codexAuthUploadFile{
			{field: "file", filename: "single.json", email: "single@example.com"},
		},
	})

	resp := decodeCodexImportResponse(t, recorder)
	if !resp.Success {
		t.Fatalf("expected import success, got body %s", recorder.Body.String())
	}
	assertControllerChannelCount(t, 1)
}

func TestImportCodexAuthFilesAcceptsMixedFilesAndFileFields(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{
		files: []codexAuthUploadFile{
			{field: "files", filename: "first.json", email: "first@example.com"},
			{field: "file", filename: "second.json", email: "second@example.com"},
		},
	})

	resp := decodeCodexImportResponse(t, recorder)
	if !resp.Success {
		t.Fatalf("expected import success, got body %s", recorder.Body.String())
	}
	assertControllerChannelCount(t, 2)
}

func TestImportCodexAuthFilesRejectsMissingChannelWithoutWriting(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{
		omitChannel: true,
		files: []codexAuthUploadFile{
			{field: "files", filename: "auth.json", email: "auth@example.com"},
		},
	})

	resp := decodeCodexImportResponse(t, recorder)
	if resp.Success || !strings.Contains(resp.Message, "channel template is required") {
		t.Fatalf("expected missing channel failure, got body %s", recorder.Body.String())
	}
	assertControllerChannelCount(t, 0)
}

func TestImportCodexAuthFilesRejectsMissingFileWithoutWriting(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{})

	resp := decodeCodexImportResponse(t, recorder)
	if resp.Success || !strings.Contains(resp.Message, "at least one auth file is required") {
		t.Fatalf("expected missing file failure, got body %s", recorder.Body.String())
	}
	assertControllerChannelCount(t, 0)
}

func TestImportCodexAuthFilesRejectsInvalidAuthFileWithoutWriting(t *testing.T) {
	useControllerChannelDB(t)
	gin.SetMode(gin.TestMode)

	recorder := performImportCodexAuthFilesRequest(t, importCodexAuthFilesRequest{
		files: []codexAuthUploadFile{
			{field: "files", filename: "good.json", email: "good@example.com"},
			{field: "files", filename: "bad.json", raw: `{"type":"gemini","access_token":"token"}`},
		},
	})

	resp := decodeCodexImportResponse(t, recorder)
	if resp.Success || !strings.Contains(resp.Message, "bad.json: unsupported auth file type") {
		t.Fatalf("expected invalid auth file failure with filename, got body %s", recorder.Body.String())
	}
	assertControllerChannelCount(t, 0)
}

type importCodexAuthFilesRequest struct {
	omitChannel       bool
	channelAfterFiles bool
	files             []codexAuthUploadFile
}

type codexAuthUploadFile struct {
	field    string
	filename string
	email    string
	raw      string
}

func buildCodexAuthUploadFiles(count int, field string) []codexAuthUploadFile {
	files := make([]codexAuthUploadFile, 0, count)
	for i := 0; i < count; i++ {
		files = append(files, codexAuthUploadFile{
			field:    field,
			filename: fmt.Sprintf("codex-%04d.json", i),
			email:    fmt.Sprintf("codex-%04d@example.com", i),
		})
	}
	return files
}

func performImportCodexAuthFilesRequest(t *testing.T, params importCodexAuthFilesRequest) *httptest.ResponseRecorder {
	t.Helper()

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)

	if !params.omitChannel && !params.channelAfterFiles {
		writeCodexImportChannelTemplate(t, writer)
	}
	for _, file := range params.files {
		writeCodexAuthUploadFile(t, writer, file)
	}
	if !params.omitChannel && params.channelAfterFiles {
		writeCodexImportChannelTemplate(t, writer)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("Close returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/codex/auth-files/import", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	ImportCodexAuthFiles(ctx)
	return recorder
}

func writeCodexImportChannelTemplate(t *testing.T, writer *multipart.Writer) {
	t.Helper()

	template := fmt.Sprintf(`{"type":%d,"name":"Imported Codex","models":"gpt-5","group":"default"}`, config.ChannelTypeCodex)
	if err := writer.WriteField("channel", template); err != nil {
		t.Fatalf("WriteField returned error: %v", err)
	}
}

func writeCodexAuthUploadFile(t *testing.T, writer *multipart.Writer, file codexAuthUploadFile) {
	t.Helper()

	field := file.field
	if field == "" {
		field = "files"
	}
	raw := file.raw
	if raw == "" {
		raw = fmt.Sprintf(`{"type":"codex","email":%q,"access_token":"access-token","refresh_token":"refresh-token"}`, file.email)
	}
	fileWriter, err := writer.CreateFormFile(field, file.filename)
	if err != nil {
		t.Fatalf("CreateFormFile returned error: %v", err)
	}
	if _, err = fileWriter.Write([]byte(raw)); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
}

func decodeCodexImportResponse(t *testing.T, recorder *httptest.ResponseRecorder) struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    struct {
		Count int `json:"count"`
	} `json:"data"`
} {
	t.Helper()

	if recorder.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", recorder.Code)
	}

	var resp struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Count int `json:"count"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	return resp
}

func useControllerChannelDB(t *testing.T) {
	t.Helper()

	if logger.Logger == nil {
		logger.SetupLogger()
	}

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))), &gorm.Config{})
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

func assertControllerChannelCount(t *testing.T, want int64) {
	t.Helper()

	var count int64
	if err := model.DB.Model(&model.Channel{}).Count(&count).Error; err != nil {
		t.Fatalf("expected channel count query to succeed, got %v", err)
	}
	if count != want {
		t.Fatalf("expected %d persisted channels, got %d", want, count)
	}
}
