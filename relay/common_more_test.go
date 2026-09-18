package relay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	claudeprovider "one-api/providers/claude"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type partialErrorReadCloser struct {
	sent bool
}

func (r *partialErrorReadCloser) Read(data []byte) (int, error) {
	if !r.sent {
		r.sent = true
		data[0] = '{'
		return 1, nil
	}
	return 0, errors.New("provider body read failed")
}

func (*partialErrorReadCloser) Close() error { return nil }

func TestResponseCustomFiltersProviderHeaders(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", nil)

	err := responseCustom(ctx, &types.AudioResponseWrapper{
		Headers: map[string]string{
			"Content-Type":        "text/plain",
			"X-Request-Id":        "req_audio",
			"Set-Cookie":          "provider_session=secret",
			"Openai-Organization": "org_shared",
			"X-Provider-Debug":    "internal",
		},
		Body: []byte("transcript"),
	}, providerresponse.OperationAudioTranscription)
	if err != nil {
		t.Fatalf("expected audio response to succeed, got %v", err)
	}
	if recorder.Body.String() != "transcript" || recorder.Header().Get("Content-Type") != "text/plain" || recorder.Header().Get("X-Request-Id") != "req_audio" {
		t.Fatalf("expected response body and safe headers, got body=%q headers=%#v", recorder.Body.String(), recorder.Header())
	}
	for _, forbidden := range []string{"Set-Cookie", "Openai-Organization", "X-Provider-Debug"} {
		if recorder.Header().Get(forbidden) != "" {
			t.Fatalf("expected %s to be filtered, got %#v", forbidden, recorder.Header())
		}
	}
}

func TestResponseMultipartAbortsCommittedCopyFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	logger.Logger = zap.NewNop()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil)

	defer func() {
		if recovered := recover(); recovered != http.ErrAbortHandler {
			t.Fatalf("expected http.ErrAbortHandler, got %#v", recovered)
		}
		if got := recorder.Body.String(); got != "{" {
			t.Fatalf("expected only the committed provider prefix, got %q", got)
		}
	}()
	responseMultipart(ctx, &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &partialErrorReadCloser{},
	}, providerresponse.Policy{Operation: providerresponse.OperationBinaryDownload, DataPath: providerresponse.DataPathExactWire, BodyUnmodified: true})
}

func TestPath2RelayAndLimitModelHelpers(t *testing.T) {
	ginCtx := newRelayTestContext(nil)

	paths := []string{
		"/v1/chat/completions",
		"/v1/completions",
		"/v1/embeddings",
		"/v1/moderations",
		"/v1/images/generations",
		"/recraftAI/v1/images/generations",
		"/v1/images/edits",
		"/v1/images/variations",
		"/v1/audio/speech",
		"/v1/audio/transcriptions",
		"/v1/audio/translations",
		"/claude",
		"/gemini",
		"/v1/responses",
		"/recraftAI/v1/styles",
	}
	for _, path := range paths {
		if relay := Path2Relay(ginCtx, path); relay == nil {
			t.Fatalf("expected path %q to resolve to a relay handler", path)
		}
	}
	if relay := Path2Relay(ginCtx, "/unknown"); relay != nil {
		t.Fatalf("expected unmatched path to return nil relay, got %#v", relay)
	}

	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected missing token_setting not to restrict models, got %v", err)
	}
	ginCtx.Set("token_setting", "wrong-type")
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected wrong typed token_setting not to restrict models, got %v", err)
	}

	setting := &model.TokenSetting{}
	ginCtx.Set("token_setting", setting)
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected disabled model limits not to restrict models, got %v", err)
	}

	setting.Limits.LimitModelSetting.Enabled = true
	if err := checkLimitModel(ginCtx, "gpt-5"); err == nil || !strings.Contains(err.Error(), "No available models") {
		t.Fatalf("expected empty model allow-list to reject usage, got %v", err)
	}

	setting.Limits.LimitModelSetting.Models = []string{"gpt-5"}
	if err := checkLimitModel(ginCtx, "gpt-5"); err != nil {
		t.Fatalf("expected allow-listed model to pass, got %v", err)
	}
	if err := checkLimitModel(ginCtx, "gpt-4o"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("expected mismatched model to fail, got %v", err)
	}
}

func TestChannelSelectionHelpers(t *testing.T) {
	ctx := newRelayTestContext(nil)

	if explicitChannelPinID(nil) != 0 {
		t.Fatal("expected nil context to have no explicit channel pin")
	}
	ctx.Set("specific_channel_id", 99)
	if explicitChannelPinID(ctx) != 99 {
		t.Fatalf("expected explicit channel pin id, got %d", explicitChannelPinID(ctx))
	}
	ctx.Set("specific_channel_id_ignore", true)
	if explicitChannelPinID(ctx) != 0 {
		t.Fatalf("expected ignored specific channel id to return zero, got %d", explicitChannelPinID(ctx))
	}
	ctx.Set("specific_channel_id_ignore", false)

	setPreferredChannelFromAffinity(ctx, 88)
	ctx.Set(channelAffinityIgnoreCooldownContextKey, true)
	ctx.Set(channelAffinityStrictContextKey, true)
	ctx.Set("skip_channel_ids", []int{5, 6})
	ctx.Set("allow_channel_type", []int{config.ChannelTypeCodex})
	selection := currentRealtimeChannelSelection(ctx)
	if selection.preferredChannelID != 88 || !selection.ignorePreferredCooldown || !selection.strictPreferredChannel {
		t.Fatalf("expected affinity selection flags to be captured, got %+v", selection)
	}
	if len(selection.skipChannelIDs) != 2 || len(selection.allowChannelTypes) != 1 {
		t.Fatalf("expected selection filters to be copied from gin context, got %+v", selection)
	}

	originalWaitBudget := config.PreferredChannelWaitMilliseconds
	originalWaitPoll := config.PreferredChannelWaitPollMilliseconds
	config.PreferredChannelWaitMilliseconds = 0
	config.PreferredChannelWaitPollMilliseconds = 0
	if preferredChannelWaitBudget(nil) != 0 || preferredChannelWaitPollInterval(nil) != 50*time.Millisecond {
		t.Fatalf("expected wait helpers to normalize zero config values, got budget=%v poll=%v", preferredChannelWaitBudget(nil), preferredChannelWaitPollInterval(nil))
	}
	config.PreferredChannelWaitMilliseconds = 125
	config.PreferredChannelWaitPollMilliseconds = 10
	if preferredChannelWaitBudget(nil) != 125*time.Millisecond || preferredChannelWaitPollInterval(nil) != 10*time.Millisecond {
		t.Fatalf("unexpected wait helper values, got budget=%v poll=%v", preferredChannelWaitBudget(nil), preferredChannelWaitPollInterval(nil))
	}
	config.PreferredChannelWaitMilliseconds = originalWaitBudget
	config.PreferredChannelWaitPollMilliseconds = originalWaitPoll

	if requestContextErr(nil) != nil {
		t.Fatal("expected nil request context error to be nil")
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	req := ctx.Request.WithContext(cancelCtx)
	ctx.Request = req
	if !errors.Is(requestContextErr(ctx), context.Canceled) {
		t.Fatalf("expected canceled request context to surface, got %v", requestContextErr(ctx))
	}

	recordPreferredChannelWaitMeta(ctx, 250*time.Millisecond, 100*time.Millisecond, true, false)
	meta := currentChannelAffinityLogMeta(ctx)
	if meta["channel_affinity_wait_budget_ms"] != int64(250) || meta["channel_affinity_wait_exhausted"] != true {
		t.Fatalf("expected wait metadata to be recorded, got %#v", meta)
	}
}

func TestGroupManagerFallbackAndFetchChannelByID(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	originalUserGroups := model.GlobalUserGroupRatio.UserGroup
	originalAPILimiter := model.GlobalUserGroupRatio.APILimiter
	originalPublicGroups := append([]string(nil), model.GlobalUserGroupRatio.PublicGroup...)
	model.GlobalUserGroupRatio.UserGroup = map[string]*model.UserGroup{
		"backup": {Symbol: "backup", Ratio: 1.75},
	}
	model.GlobalUserGroupRatio.APILimiter = nil
	model.GlobalUserGroupRatio.PublicGroup = nil
	t.Cleanup(func() {
		model.GlobalUserGroupRatio.UserGroup = originalUserGroups
		model.GlobalUserGroupRatio.APILimiter = originalAPILimiter
		model.GlobalUserGroupRatio.PublicGroup = originalPublicGroups
	})

	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	ctx.Set("token_group", "primary")
	ctx.Set("token_backup_group", "backup")

	groupManager := NewGroupManager(ctx)
	var attempted []string
	channel, err := groupManager.TryWithGroups("gpt-5", nil, func(group string) (*model.Channel, error) {
		attempted = append(attempted, group)
		if group == "primary" {
			return nil, errors.New("primary unavailable")
		}
		return &model.Channel{Id: 7}, nil
	})
	if err != nil || channel == nil || channel.Id != 7 {
		t.Fatalf("expected fallback group to succeed, got channel=%#v err=%v", channel, err)
	}
	if len(attempted) != 2 || attempted[0] != "primary" || attempted[1] != "backup" {
		t.Fatalf("expected primary then backup group attempts, got %#v", attempted)
	}
	if !ctx.GetBool("is_backupGroup") || ctx.GetFloat64("group_ratio") != 1.75 {
		t.Fatalf("expected fallback group metadata to be written to context, got is_backupGroup=%v group_ratio=%v", ctx.GetBool("is_backupGroup"), ctx.GetFloat64("group_ratio"))
	}
	if got := groupctx.CurrentRoutingGroup(ctx); got != "backup" {
		t.Fatalf("expected fallback to update routing group, got %q", got)
	}
	if got := groupctx.CurrentRoutingGroupSource(ctx); got != groupctx.RoutingGroupSourceBackupGroup {
		t.Fatalf("expected fallback to update routing group source, got %q", got)
	}

	if _, err := groupManager.tryGroup("", "gpt-5", nil, nil); err == nil {
		t.Fatal("expected empty group lookup to fail")
	}
	if err := groupManager.setGroupRatio("missing"); err == nil {
		t.Fatal("expected missing group ratio lookup to fail")
	}
	if err := groupManager.createGroupError("backup", "gpt-5", &model.Channel{Id: 9}); err == nil || !strings.Contains(err.Error(), "数据库一致性") {
		t.Fatalf("expected broken channel reference error, got %v", err)
	}

	originalDB := model.DB
	testDB, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("expected in-memory sqlite database, got %v", err)
	}
	if err := testDB.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("expected channel schema migration, got %v", err)
	}
	model.DB = testDB
	t.Cleanup(func() {
		model.DB = originalDB
		if sqlDB, dbErr := testDB.DB(); dbErr == nil {
			_ = sqlDB.Close()
		}
	})

	enabled := newRelayTestCodexChannel(101)
	disabled := newRelayTestCodexChannel(102)
	disabled.Status = config.ChannelStatusManuallyDisabled
	invalidRuntimeConfig := newRelayTestCodexChannel(103)
	invalidRuntimeConfig.Other = "2024-05-01-preview"
	if err := testDB.Create(enabled).Error; err != nil {
		t.Fatalf("expected enabled channel insert, got %v", err)
	}
	if err := testDB.Create(disabled).Error; err != nil {
		t.Fatalf("expected disabled channel insert, got %v", err)
	}
	if err := testDB.Create(invalidRuntimeConfig).Error; err != nil {
		t.Fatalf("expected invalid runtime config fixture insert, got %v", err)
	}

	if channel, err := fetchChannelById(enabled.Id); err != nil || channel == nil || channel.Id != enabled.Id {
		t.Fatalf("expected enabled channel lookup to succeed, got channel=%#v err=%v", channel, err)
	}
	if _, err := fetchChannelById(9999); err == nil || !strings.Contains(err.Error(), "无效的渠道 Id") {
		t.Fatalf("expected missing channel lookup to fail, got %v", err)
	}
	if _, err := fetchChannelById(disabled.Id); err == nil || !strings.Contains(err.Error(), "已被禁用") {
		t.Fatalf("expected disabled channel lookup to fail, got %v", err)
	}
	var invalidConfigErr *model.InvalidChannelRuntimeConfigError
	if _, err := fetchChannelById(invalidRuntimeConfig.Id); err == nil || !errors.As(err, &invalidConfigErr) {
		t.Fatalf("expected invalid runtime config channel lookup to fail closed, got %v", err)
	}
}

func TestRelayCommonStreamingAndRetryHelpers(t *testing.T) {
	originalLogger := logger.Logger
	originalRetryStatusCodes := config.RetryStatusCodes
	logger.Logger = zap.NewNop()
	if err := config.SetRetryStatusCodes(config.DefaultRetryStatusCodes); err != nil {
		t.Fatalf("expected default retry status codes to parse, got %v", err)
	}
	t.Cleanup(func() {
		logger.Logger = originalLogger
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)

	stream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		stream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_stream\"}\n\n"
		stream.errChan <- io.EOF
	}()
	firstResponseTime := responseGeneralStreamClient(ginCtx, stream, func() string {
		return "event: response.done\ndata: {\"id\":\"resp_stream\"}\n\n"
	})
	if firstResponseTime.IsZero() {
		t.Fatal("expected responseGeneralStreamClient to record first response time")
	}
	if body := recorder.Body.String(); !strings.Contains(body, "response.created") || !strings.Contains(body, "response.done") {
		t.Fatalf("expected general stream client to write stream data and end payload, got %q", body)
	}

	observerRecorder := httptest.NewRecorder()
	observerCtx, _ := gin.CreateTestContext(observerRecorder)
	observerCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	var observed []string
	observerStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		observerStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_observed\"}\n\n"
		observerStream.errChan <- errors.New("stream broken Authorization: Bearer secret-token api_key=query-secret https://provider.example/v1?token=url-secret session session-secret")
	}()
	firstResponseTime = responseGeneralStreamClientWithObserver(observerCtx, observerStream, func() string {
		return "ignored-end"
	}, func(line string) {
		observed = append(observed, line)
	})
	if firstResponseTime.IsZero() {
		t.Fatal("expected observed general stream client to record first response time")
	}
	if len(observed) != 1 || !strings.Contains(observed[0], "resp_observed") {
		t.Fatalf("expected observer to receive upstream stream data, got %#v", observed)
	}
	if body := observerRecorder.Body.String(); !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"message":"stream interrupted"`) {
		t.Fatalf("expected general stream client stable error payload, got %q", body)
	} else {
		for _, forbidden := range []string{"stream broken", "Authorization", "secret-token", "query-secret", "provider.example", "url-secret", "session-secret"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("expected general stream error body not to leak %q, got %q", forbidden, body)
			}
		}
	}

	jsonRecorder := httptest.NewRecorder()
	jsonCtx, _ := gin.CreateTestContext(jsonRecorder)
	jsonCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	if errWithCode := responseJsonClient(jsonCtx, map[string]any{"ok": true}); errWithCode != nil {
		t.Fatalf("expected json client helper to succeed, got %v", errWithCode)
	}
	if body := jsonRecorder.Body.String(); !strings.Contains(body, `"ok":true`) {
		t.Fatalf("expected json client helper to write response body, got %q", body)
	}

	rawResponse := &types.ChatCompletionResponse{
		ID:     "chatcmpl_typed",
		Object: "chat.completion",
		Model:  "gpt-5",
		Choices: []types.ChatCompletionChoice{{
			Index: 0,
			Message: types.ChatCompletionMessage{
				Role:             types.ChatMessageRoleAssistant,
				Content:          "answer",
				ReasoningContent: "plan",
			},
			FinishReason: types.FinishReasonStop,
		}},
		Usage: &types.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3},
	}
	rawResponse.SetProviderRawJSON([]byte(`{"id":"chatcmpl_raw","account_id":"acct-secret","choices":[{"message":{"content":"model says access_token is a public label","reasoning":"plan"}}],"future":{"exact":true}}`))

	typedRecorder := httptest.NewRecorder()
	typedCtx, _ := gin.CreateTestContext(typedRecorder)
	typedCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	if errWithCode := responseJsonClient(typedCtx, rawResponse); errWithCode != nil {
		t.Fatalf("expected captured provider response to succeed, got %v", errWithCode)
	}
	if body := typedRecorder.Body.String(); !strings.Contains(body, `"id":"chatcmpl_typed"`) || !strings.Contains(body, `"reasoning_content":"plan"`) || !strings.Contains(body, `"completion_tokens":2`) || strings.Contains(body, `"chatcmpl_raw"`) {
		t.Fatalf("expected raw capture without replay opt-in to render normalized response, got %q", body)
	}

	rawResponse.EnableProviderRawJSONReplay()
	rawRecorder := httptest.NewRecorder()
	rawCtx, _ := gin.CreateTestContext(rawRecorder)
	rawCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	rawCtx.Set(requestctx.ProviderResponseStatusContextKey, http.StatusCreated)
	rawCtx.Set(requestctx.ProviderResponseHeadersContextKey, http.Header{
		"Cache-Control":    {"private, no-store"},
		"Content-Encoding": {"gzip"},
		"Content-Length":   {"123"},
		"Digest":           {"sha-256=:YWJj:"},
		"Etag":             {`"raw-v1"`},
	})
	if errWithCode := responseJsonClient(rawCtx, rawResponse); errWithCode != nil {
		t.Fatalf("expected opted-in raw provider response to succeed, got %v", errWithCode)
	}
	if rawRecorder.Code != http.StatusCreated || !strings.Contains(rawRecorder.Body.String(), `"account_id":"acct-secret"`) || !strings.Contains(rawRecorder.Body.String(), `"content":"model says access_token is a public label"`) || !strings.Contains(rawRecorder.Body.String(), `"future":{"exact":true}`) {
		t.Fatalf("expected exact provider status/body after replay opt-in, got status=%d body=%q", rawRecorder.Code, rawRecorder.Body.String())
	}
	if rawRecorder.Header().Get("Content-Encoding") != "gzip" || rawRecorder.Header().Get("Content-Length") != "123" || rawRecorder.Header().Get("Digest") != "sha-256=:YWJj:" || rawRecorder.Header().Get("Etag") != `"raw-v1"` {
		t.Fatalf("security rewrite retained invalid representation validators: %#v", rawRecorder.Header())
	}
	if rawRecorder.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("security rewrite dropped cache safety directive: %#v", rawRecorder.Header())
	}

	retryCtx := newRelayTestContext(nil)
	if !shouldRetry(retryCtx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusInternalServerError}, config.ChannelTypeCodex) {
		t.Fatal("expected 5xx responses to remain retryable")
	}
	if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusGatewayTimeout}, config.ChannelTypeCodex) {
		t.Fatal("expected 504 responses to use the default retry status policy")
	}
	if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusNotImplemented}, config.ChannelTypeCodex) {
		t.Fatal("expected unconfigured 5xx statuses to remain non-retryable by default")
	}
	for _, status := range []int{http.StatusNotFound} {
		if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: status}, config.ChannelTypeCodex) {
			t.Fatalf("expected status %d to be non-retryable", status)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: status}, config.ChannelTypeCodex) {
			t.Fatalf("expected upstream status %d to retry on another channel", status)
		}
	}
	if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "authentication_error", Code: "provider_authentication_failed"},
		StatusCode:  http.StatusUnauthorized,
	}, config.ChannelTypeCodex) {
		t.Fatal("expected upstream provider authentication failures to retry on another channel")
	}
	if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "token_invalidated"},
		StatusCode:  http.StatusUnauthorized,
	}, config.ChannelTypeCodex) {
		t.Fatal("expected token_invalidated upstream credential failures to retry on another channel")
	}
	retryCtx.Set("specific_channel_id", 99)
	if shouldRetry(retryCtx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusTooManyRequests}, config.ChannelTypeCodex) {
		t.Fatal("expected explicit channel pins to disable retries")
	}
	if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusTooManyRequests, LocalError: true}, config.ChannelTypeCodex) {
		t.Fatal("expected local realtime errors to disable retries")
	}
	if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusServiceUnavailable, LocalError: true, UpstreamNotAttempted: true}, config.ChannelTypeCodex) {
		t.Fatal("expected channel-local pre-provider failure to retry another channel")
	}
	pinnedLocal := newRelayTestContext(nil)
	pinnedLocal.Set("specific_channel_id", 99)
	if shouldRetry(pinnedLocal, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusServiceUnavailable, LocalError: true, UpstreamNotAttempted: true}, config.ChannelTypeCodex) {
		t.Fatal("expected explicit channel pin to block pre-provider fallback")
	}
	if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:  "previous_response_not_found",
			Param: "previous_response_id",
		},
		StatusCode: http.StatusConflict,
		LocalError: true,
	}, config.ChannelTypeCodex) {
		t.Fatal("expected local stale continuation errors to disable retries")
	}
	if !shouldRetryBadRequest(config.ChannelTypeAnthropic, &types.OpenAIErrorWithStatusCode{
		OpenAIError:            types.OpenAIError{Message: "provider account rejected the request"},
		StatusCode:             http.StatusBadRequest,
		ProviderQuotaExhausted: true,
	}) {
		t.Fatal("expected redacted Anthropic balance exhaustion to retain retry disposition")
	}
	if !shouldRetryBadRequest(config.ChannelTypeGemini, &types.OpenAIErrorWithStatusCode{
		OpenAIError:          types.OpenAIError{Message: "provider account rejected the request"},
		StatusCode:           http.StatusBadRequest,
		ProviderAuthRejected: true,
	}) {
		t.Fatal("expected redacted Gemini credential rejection to retain retry disposition")
	}
	if !shouldRetryBadRequest(config.ChannelTypeAnthropic, &types.OpenAIErrorWithStatusCode{
		OpenAIError:         types.OpenAIError{Message: "provider account rejected the request"},
		StatusCode:          http.StatusBadRequest,
		ProviderRateLimited: true,
	}) {
		t.Fatal("expected redacted provider rate limit to retain retry disposition")
	}

	if err := config.SetRetryStatusCodes("401"); err != nil {
		t.Fatalf("expected retry status override to parse, got %v", err)
	}
	if !shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}, config.ChannelTypeCodex) {
		t.Fatal("expected configured status 401 to remain retryable")
	}
	if shouldRetry(newRelayTestContext(nil), &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusInternalServerError}, config.ChannelTypeCodex) {
		t.Fatal("expected status 500 to stop retrying after retry status override")
	}
}

func TestShouldRetryReadsLatestRuntimePublication(t *testing.T) {
	originalManager := config.GlobalOption
	manager := config.NewOptionManager()
	retryStatusCodes := "401"
	manager.RegisterStringOption("RetryStatusCodes", &retryStatusCodes, config.OptionMetadata{Visibility: config.OptionVisibilityPublic})
	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"RetryStatusCodes": "401"}); err != nil {
		t.Fatalf("publish initial retry policy: %v", err)
	}
	config.GlobalOption = manager
	t.Cleanup(func() { config.GlobalOption = originalManager })

	ctx := newRelayTestContext(nil)
	if !shouldRetry(ctx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}, config.ChannelTypeCodex) {
		t.Fatal("initial publication should retry 401")
	}
	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{"RetryStatusCodes": "503"}); err != nil {
		t.Fatalf("publish updated retry policy: %v", err)
	}
	if shouldRetry(ctx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}, config.ChannelTypeCodex) {
		t.Fatal("a later retry decision retained the old publication")
	}
	if !shouldRetry(ctx, &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusServiceUnavailable}, config.ChannelTypeCodex) {
		t.Fatal("a later retry decision should observe the newer publication")
	}
}

func TestResponseJSONClientProjectsOnlyPublicResponsesCacheFields(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	response := &types.OpenAIResponsesResponses{
		ID: "resp_typed_cache",
		Usage: &types.ResponsesUsage{
			InputTokens: 18,
			InputTokensDetails: &types.ResponsesUsageInputTokensDetails{
				CachedTokens:     2,
				CacheWriteTokens: 5,
			},
		},
	}

	if apiErr := responseJsonClient(ctx, response); apiErr != nil {
		t.Fatalf("typed Responses delivery failed: %v", apiErr)
	}
	body := recorder.Body.String()
	for _, publicField := range []string{`"cached_tokens":2`, `"cache_write_tokens":5`} {
		if !strings.Contains(body, publicField) {
			t.Fatalf("public cache field %s missing from typed Responses wire: %s", publicField, body)
		}
	}
	for _, privateField := range []string{"cache_creation_input_tokens", "cache_read_input_tokens", "cached_tokens_internal"} {
		if strings.Contains(body, privateField) {
			t.Fatalf("provider cache evidence %q leaked from typed Responses wire: %s", privateField, body)
		}
	}
	if response.Usage.InputTokensDetails.CachedTokens != 2 || response.Usage.InputTokensDetails.CacheWriteTokens != 5 {
		t.Fatalf("typed delivery mutated internal evidence: %+v", response.Usage.InputTokensDetails)
	}
}

func TestProcessProviderPayloadAPIErrorBestEffortControlPlane(t *testing.T) {
	gin.SetMode(gin.TestMode)

	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	originalProcess := processChannelRelayErrorFunc
	errCh := make(chan *types.OpenAIErrorWithStatusCode, 1)
	processChannelRelayErrorFunc = func(_ context.Context, channelID int, channelName string, apiErr *types.OpenAIErrorWithStatusCode, channelType int) {
		if channelID != 77 || channelName != "provider-control" || channelType != config.ChannelTypeOpenAI {
			t.Errorf("unexpected channel context id=%d name=%q type=%d", channelID, channelName, channelType)
		}
		errCh <- apiErr
	}
	t.Cleanup(func() {
		processChannelRelayErrorFunc = originalProcess
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model=gpt-5", nil)
	ctx.Set("original_model", "gpt-5")
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	ctx.Set("channel_id", 77)

	channel := &model.Channel{Id: 77, Name: "provider-control", Type: config.ChannelTypeOpenAI}
	processProviderPayloadAPIError(ctx, channel, []byte(`{`), "test_malformed")
	processProviderPayloadAPIError(ctx, channel, []byte(`{"type":"response.output_text.delta","status_code":503,"message":"metadata"}`), "test_non_error")
	processProviderPayloadAPIError(ctx, nil, []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"usage limit reached"}}`), "test_no_channel")

	select {
	case apiErr := <-errCh:
		t.Fatalf("expected malformed, non-error, and nil-channel payloads not to reach channel relay handling, got %#v", apiErr)
	case <-time.After(100 * time.Millisecond):
	}

	processProviderPayloadAPIError(ctx, channel, []byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"usage limit reached"}}`), "test_provider_error")
	select {
	case apiErr := <-errCh:
		if apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Code != "usage_limit_reached" || !apiErr.ProviderQuotaExhausted {
			t.Fatalf("expected safe usage-limit provider error, got %#v", apiErr)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for provider error control-plane handling")
	}
}

func TestFetchChannelByModelWithSelectionFiltersCustomClaudeRelayChannels(t *testing.T) {
	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})

	weight := uint(1)
	proxy := ""
	modelName := "claude-3-5-sonnet-20241022"
	disabledPlugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled": false,
		}},
	})
	invalidPlugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled":      true,
			"upstream_url": "://bad-url/v1/messages",
		}},
	})

	model.ChannelGroup = model.ChannelsChooser{
		Channels: map[int]*model.ChannelChoice{
			71: {
				Channel: &model.Channel{
					Id:     71,
					Type:   config.ChannelTypeCustom,
					Status: config.ChannelStatusEnabled,
					Group:  "default",
					Models: modelName,
					Weight: &weight,
					Proxy:  &proxy,
					Plugin: &disabledPlugin,
				},
			},
			73: {
				Channel: &model.Channel{
					Id:     73,
					Type:   config.ChannelTypeCustom,
					Status: config.ChannelStatusEnabled,
					Group:  "default",
					Models: modelName,
					Weight: &weight,
					Proxy:  &proxy,
					Plugin: &invalidPlugin,
				},
			},
			72: {
				Channel: &model.Channel{
					Id:     72,
					Type:   config.ChannelTypeAnthropic,
					Status: config.ChannelStatusEnabled,
					Group:  "default",
					Models: modelName,
					Weight: &weight,
					Proxy:  &proxy,
				},
			},
		},
		Rule: map[string]map[string][][]int{
			"default": {
				modelName: {{71, 73, 72}},
			},
		},
		ModelGroup: map[string]map[string]bool{
			modelName: {
				"default": true,
			},
		},
	}

	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", nil)
	ctx.Set("token_group", "default")
	ctx.Set("allow_channel_type", AllowChannelType)

	channel, err := fetchChannelByModelWithSelection(ctx, modelName, currentRealtimeChannelSelection(ctx))
	if err != nil {
		t.Fatalf("expected Claude channel lookup to succeed, got %v", err)
	}
	if channel == nil || channel.Id != 72 {
		t.Fatalf("expected disabled and invalid custom Claude channels to be filtered out, got %#v", channel)
	}
}

func TestPrepareProviderForCustomClaudeRelay(t *testing.T) {
	weight := uint(1)
	proxy := ""
	plugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled":      true,
			"upstream_url": "https://claude-proxy.example.com/api/v1/messages",
		}},
	})
	channel := &model.Channel{
		Id:     81,
		Type:   config.ChannelTypeCustom,
		Key:    "sk-custom",
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "claude-3-5-sonnet-20241022",
		Weight: &weight,
		BaseURL: func() *string {
			baseURL := "https://openai-proxy.example.com"
			return &baseURL
		}(),
		Proxy:  &proxy,
		Plugin: &plugin,
	}

	claudeCtx := newRelayTestContext(nil)
	claudeCtx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", nil)
	claudeCtx.Request.Header.Set("anthropic-version", "2024-01-01")
	provider, newModelName, err := prepareProviderForChannel(claudeCtx, "claude-3-5-sonnet-20241022", channel)
	if err != nil {
		t.Fatalf("expected custom Claude relay provider selection to succeed, got %v", err)
	}
	claudeProvider, ok := provider.(*claudeprovider.ClaudeProvider)
	if !ok {
		t.Fatalf("expected Claude provider for custom Claude relay, got %T", provider)
	}
	if newModelName != "claude-3-5-sonnet-20241022" {
		t.Fatalf("unexpected mapped model name: %q", newModelName)
	}
	if fullURL := claudeProvider.GetFullRequestURL(claudeProvider.Config.ChatCompletions); fullURL != "https://claude-proxy.example.com/api/v1/messages" {
		t.Fatalf("unexpected Claude request URL: %q", fullURL)
	}
	headers := claudeProvider.GetRequestHeaders()
	if headers["x-api-key"] != "sk-custom" || headers["anthropic-version"] != "2024-01-01" {
		t.Fatalf("unexpected Claude request headers: %#v", headers)
	}

	openAICtx := newRelayTestContext(nil)
	openAICtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	provider, _, err = prepareProviderForChannel(openAICtx, "claude-3-5-sonnet-20241022", channel)
	if err != nil {
		t.Fatalf("expected normal custom channel provider selection to succeed, got %v", err)
	}
	if _, ok := provider.(*openai.OpenAIProvider); !ok {
		t.Fatalf("expected normal custom channel requests to keep using OpenAI-compatible provider, got %T", provider)
	}

	invalidPlugin := datatypes.NewJSONType(model.PluginType{
		"endpoints": {"anthropic.messages": map[string]any{
			"enabled":      true,
			"upstream_url": "https://claude-proxy.example.com/v1/messages/v1/messages",
		}},
	})
	channel.Plugin = &invalidPlugin
	provider, _, err = prepareProviderForChannel(claudeCtx, "claude-3-5-sonnet-20241022", channel)
	if err != nil {
		t.Fatalf("expected full /v1/messages Claude URL to pass through unchanged, got %v", err)
	}
	claudeProvider, ok = provider.(*claudeprovider.ClaudeProvider)
	if !ok {
		t.Fatalf("expected Claude provider after preserving full Claude URL, got %T", provider)
	}
	if fullURL := claudeProvider.GetFullRequestURL(claudeProvider.Config.ChatCompletions); fullURL != "https://claude-proxy.example.com/v1/messages/v1/messages" {
		t.Fatalf("unexpected preserved Claude request URL: %q", fullURL)
	}
}

func TestCustomClaudeSelectionPreservesMappedRemoteMediaRawWire(t *testing.T) {
	var mediaGets atomic.Int32
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mediaGets.Add(1)
		_, _ = io.WriteString(w, "must not fetch")
	}))
	t.Cleanup(media.Close)
	var gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_custom","type":"message","role":"assistant","model":"claude-sonnet-4","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(upstream.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	proxy := ""
	plugin := datatypes.NewJSONType(model.PluginType{"endpoints": {"anthropic.messages": map[string]any{"enabled": true, "upstream_url": upstream.URL + "/v1/messages"}}})
	mapping := `{"public-claude":"claude-sonnet-4"}`
	channel := &model.Channel{Type: config.ChannelTypeCustom, Key: "sk-custom", Proxy: &proxy, Plugin: &plugin, ModelMapping: &mapping}
	raw := `{"model":"public-claude","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + media.URL + `/a.png","future_source":true}}]}],"future_request":{"kept":true}}`
	ctx := newRelayTestContext(nil)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/claude/v1/messages", strings.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "application/json")
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache custom Claude request: %v", err)
	}
	provider, mappedModel, err := prepareProviderForChannel(ctx, "public-claude", channel)
	if err != nil {
		t.Fatalf("select custom Claude provider: %v", err)
	}
	claudeProvider, ok := provider.(*claudeprovider.ClaudeProvider)
	if !ok {
		t.Fatalf("selected provider=%T, want ClaudeProvider", provider)
	}
	request := &claudeprovider.ClaudeRequest{}
	if err := common.UnmarshalBodyReusable(ctx, request); err != nil {
		t.Fatalf("decode custom Claude request: %v", err)
	}
	request.Model = mappedModel
	claudeProvider.SetUsage(&types.Usage{})
	if _, apiErr := claudeProvider.CreateClaudeChat(request); apiErr != nil {
		t.Fatalf("send custom Claude request: %v", apiErr)
	}
	if mediaGets.Load() != 0 {
		t.Fatalf("custom Claude path fetched remote media %d times", mediaGets.Load())
	}
	for _, want := range []string{`"model":"claude-sonnet-4"`, `"future_source":true`, `"future_request":{"kept":true}`} {
		if !strings.Contains(gotBody, want) {
			t.Fatalf("custom Claude mapped raw body lost %s: %s", want, gotBody)
		}
	}
}

func TestRelayCommonAdditionalProviderAndSelectionBranches(t *testing.T) {
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	t.Cleanup(func() {
		logger.Logger = originalLogger
	})

	if provider, modelName, err := prepareProviderForChannel(newRelayTestContext(nil), "gpt-5", nil); err == nil || provider != nil || modelName != "" {
		t.Fatalf("expected nil channel provider preparation to fail, provider=%#v model=%q err=%v", provider, modelName, err)
	}

	testDB := setupRelayTestDB(t, &model.Channel{})

	pinnedChannel := newRelayTestCodexChannel(301)
	if err := testDB.Create(pinnedChannel).Error; err != nil {
		t.Fatalf("expected pinned channel fixture to persist, got %v", err)
	}

	pinnedCtx := newRelayTestContext(nil)
	pinnedCtx.Set("specific_channel_id", pinnedChannel.Id)
	if channel, err := fetchChannel(pinnedCtx, "gpt-5"); err != nil || channel == nil || channel.Id != pinnedChannel.Id {
		t.Fatalf("expected fetchChannel to honor explicit channel pin, got channel=%#v err=%v", channel, err)
	}

	canceledCtx := newRelayTestContext(nil)
	reqCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledCtx.Request = canceledCtx.Request.WithContext(reqCtx)
	if channel, err := fetchChannelByModelWithSelection(canceledCtx, "gpt-5", realtimeChannelSelection{}); !errors.Is(err, context.Canceled) || channel != nil {
		t.Fatalf("expected canceled request context to stop channel selection, channel=%#v err=%v", channel, err)
	}

	channelGroupSnapshot := snapshotChannelGroup()
	t.Cleanup(func() {
		restoreChannelGroup(channelGroupSnapshot)
	})
	model.ChannelGroup = buildRealtimeTestChannelGroup(11)

	filterCtx := newRelayTestContext(nil)
	filterCtx.Set("token_group", "default")
	filterCtx.Set("skip_only_chat", true)
	filterCtx.Set("is_stream", true)
	if _, err := fetchChannelByModelWithSelection(filterCtx, "missing-model", realtimeChannelSelection{
		skipChannelIDs:    []int{11},
		allowChannelTypes: []int{config.ChannelTypeCodex},
	}); err == nil {
		t.Fatal("expected filtered selection with a missing model to fail")
	}

	observerRecorder := httptest.NewRecorder()
	observerCtx, _ := gin.CreateTestContext(observerRecorder)
	observerCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	var observed []string
	eofStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		eofStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_end\"}\n\n"
		eofStream.errChan <- io.EOF
	}()
	responseGeneralStreamClientWithObserver(observerCtx, eofStream, func() string {
		return "event: response.done\ndata: {\"id\":\"resp_end\"}\n\n"
	}, func(line string) {
		observed = append(observed, line)
	})
	if len(observed) != 2 || !strings.Contains(observed[1], "response.done") {
		t.Fatalf("expected observer to receive stream end payload as well, got %#v", observed)
	}

	closedRecorder := httptest.NewRecorder()
	closedCtx, _ := gin.CreateTestContext(closedRecorder)
	closedCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	closedStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	close(closedStream.dataChan)
	close(closedStream.errChan)
	if firstResponseTime := responseGeneralStreamClientWithObserver(closedCtx, closedStream, nil, nil); !firstResponseTime.IsZero() {
		t.Fatalf("expected closed stream without data to return zero first response time, got %v", firstResponseTime)
	}

	closedAfterDataRecorder := httptest.NewRecorder()
	closedAfterDataCtx, _ := gin.CreateTestContext(closedAfterDataRecorder)
	closedAfterDataCtx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	closedAfterDataStream := &fakeRelayStream{
		dataChan: make(chan string),
		errChan:  make(chan error, 1),
	}
	go func() {
		closedAfterDataStream.dataChan <- "event: response.created\ndata: {\"id\":\"resp_close\"}\n\n"
		close(closedAfterDataStream.dataChan)
		close(closedAfterDataStream.errChan)
	}()
	firstResponseTime := responseGeneralStreamClientWithObserver(closedAfterDataCtx, closedAfterDataStream, nil, nil)
	if firstResponseTime.IsZero() {
		t.Fatal("expected data channel close after first chunk to preserve first response time")
	}
	if body := closedAfterDataRecorder.Body.String(); !strings.Contains(body, "resp_close") {
		t.Fatalf("expected closed-after-data stream body to preserve upstream payload, got %q", body)
	}
}
