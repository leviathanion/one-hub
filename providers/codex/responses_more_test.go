package codex

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/internal/requesthints"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func TestCodexResponsesUsageAndBillingHelpers(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	if cloned := cloneCodexExtraBilling(nil); cloned != nil {
		t.Fatalf("expected nil extra billing clone, got %+v", cloned)
	}

	billing := map[string]types.ExtraBilling{
		types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high"): {
			ServiceType: types.APIToolTypeWebSearchPreview,
			Type:        "high",
			CallCount:   1,
		},
	}
	cloned := cloneCodexExtraBilling(billing)
	cloned[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")] = types.ExtraBilling{ServiceType: types.APIToolTypeWebSearchPreview, Type: "high", CallCount: 99}
	if billing[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected cloned extra billing to be detached from source, got %+v", billing)
	}

	target := &types.Usage{PromptTokens: 1}
	target.TextBuilder.WriteString("assistant transcript")
	resolved := &types.Usage{PromptTokens: 3, CompletionTokens: 5, TotalTokens: 8}
	applyResolvedCodexUsage(target, resolved)
	if target.PromptTokens != 3 || target.TotalTokens != 8 || target.TextBuilder.String() != "assistant transcript" {
		t.Fatalf("expected resolved usage to replace counters while preserving text, got %+v text=%q", target, target.TextBuilder.String())
	}

	if got := codexResponsesSearchType(nil); got != "" {
		t.Fatalf("expected nil search type to be empty, got %q", got)
	}
	if got := codexResponsesSearchType(&types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview}}}); got != "medium" {
		t.Fatalf("expected web search default search type, got %q", got)
	}
	if got := codexResponsesSearchType(&types.OpenAIResponsesResponses{Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}}}); got != "high" {
		t.Fatalf("expected explicit web search type, got %q", got)
	}

	usage := &types.Usage{}
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeWebSearchCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeCodeInterpreterCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeFileSearchCall}, "")
	applyCodexResponsesAddedToolBilling(usage, &types.ResponsesOutput{Type: types.InputTypeImageGenerationCall, Quality: "high", Size: "1024x1024"}, "")
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")].CallCount != 1 {
		t.Fatalf("expected web search preview billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeCodeInterpreter, "")].CallCount != 1 {
		t.Fatalf("expected code interpreter billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeFileSearch, "")].CallCount != 1 {
		t.Fatalf("expected file search billing, got %+v", usage.ExtraBilling)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "high-1024x1024")].CallCount != 1 {
		t.Fatalf("expected image generation billing, got %+v", usage.ExtraBilling)
	}

	if !codexResponsesUsageHandlerEventType("response.done") || codexResponsesUsageHandlerEventType("response.updated") {
		t.Fatal("expected usage-handler event classification to match supported event types")
	}

	response := &types.OpenAIResponsesResponses{
		Output: []types.ResponsesOutput{
			{
				Type:    types.InputTypeMessage,
				Role:    types.ChatMessageRoleAssistant,
				Content: []types.ContentResponses{{Type: types.ContentTypeOutputText, Text: "hello"}},
			},
			{Type: types.InputTypeWebSearchCall, ID: "ws_1", Status: "completed"},
		},
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview, SearchContextSize: "high"}},
	}

	seed := &types.Usage{PromptTokens: 7}
	seed.TextBuilder.WriteString("seed transcript")
	resolvedUsage := resolveCodexResponsesUsage(seed, nil, response, "gpt-5", true)
	if resolvedUsage == nil || resolvedUsage.PromptTokens != 7 || resolvedUsage.CompletionTokens <= 0 || resolvedUsage.TotalTokens <= resolvedUsage.PromptTokens {
		t.Fatalf("expected resolved usage to backfill prompt/output tokens, got %+v", resolvedUsage)
	}
	if resolvedUsage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected resolved usage to preserve extra billing, got %+v", resolvedUsage.ExtraBilling)
	}

	finalUsage := &types.Usage{PromptTokens: 3}
	finalUsage.TextBuilder.WriteString("final transcript")
	finalizeCodexResponsesUsage(finalUsage, &types.OpenAIResponsesResponses{
		Usage: &types.ResponsesUsage{InputTokens: 2, OutputTokens: 4, TotalTokens: 6},
	}, "gpt-5", false)
	if finalUsage.PromptTokens != 2 || finalUsage.CompletionTokens != 4 || finalUsage.TextBuilder.String() != "final transcript" {
		t.Fatalf("expected finalizeCodexResponsesUsage to overwrite counters and keep text, got %+v text=%q", finalUsage, finalUsage.TextBuilder.String())
	}
}

func TestCodexResponsesIncludePromptCacheAndSystemMessageHelpers(t *testing.T) {
	request := &types.OpenAIResponsesRequest{}
	ensureCodexIncludes(request)
	if includes, ok := request.Include.([]string); !ok || len(includes) != 1 || includes[0] != codexReasoningEncryptedContentInclude {
		t.Fatalf("expected nil include to default to encrypted content include, got %#v", request.Include)
	}

	request.Include = func() {}
	ensureCodexIncludes(request)
	if includes, ok := request.Include.([]string); !ok || len(includes) != 1 || includes[0] != codexReasoningEncryptedContentInclude {
		t.Fatalf("expected marshal failure include fallback, got %#v", request.Include)
	}

	request.Include = map[string]any{"bad": "shape"}
	ensureCodexIncludes(request)
	if includes, ok := request.Include.([]string); !ok || len(includes) != 1 || includes[0] != codexReasoningEncryptedContentInclude {
		t.Fatalf("expected object include fallback, got %#v", request.Include)
	}

	request.Include = "output_text.annotations"
	ensureCodexIncludes(request)
	if includes, ok := request.Include.([]string); !ok || len(includes) != 2 || includes[0] != "output_text.annotations" || includes[1] != codexReasoningEncryptedContentInclude {
		t.Fatalf("expected string include normalization, got %#v", request.Include)
	}

	if got := appendUniqueStrings([]string{"", " one ", codexReasoningEncryptedContentInclude}, "two"); len(got) != 3 || got[0] != "one" || got[1] != codexReasoningEncryptedContentInclude || got[2] != "two" {
		t.Fatalf("expected appendUniqueStrings to trim blanks and append unique suffix, got %#v", got)
	}

	normalizeCodexBuiltinTools(nil)
	request = &types.OpenAIResponsesRequest{
		Tools: []types.ResponsesTools{{Type: types.APIToolTypeWebSearchPreview}},
		ToolChoice: []any{
			map[string]any{"type": "web_search_preview_2025_03_11"},
			"unchanged",
		},
	}
	normalizeCodexBuiltinTools(request)
	if request.Tools[0].Type != types.APIToolTypeWebSearch {
		t.Fatalf("expected builtin tool alias normalization, got %q", request.Tools[0].Type)
	}
	toolChoices, ok := request.ToolChoice.([]any)
	if !ok {
		t.Fatalf("expected normalized tool choice slice, got %T", request.ToolChoice)
	}
	firstChoice, _ := toolChoices[0].(map[string]any)
	if firstChoice["type"] != types.APIToolTypeWebSearch || toolChoices[1] != "unchanged" {
		t.Fatalf("expected normalized tool choice entries, got %#v", toolChoices)
	}

	type toolChoiceShape struct {
		Type string `json:"type"`
	}
	if normalized, ok := normalizeCodexToolChoiceValue(toolChoiceShape{Type: "web_search_preview_2025_03_11"}); !ok {
		t.Fatal("expected struct tool_choice to normalize via json round-trip")
	} else {
		normalizedMap, _ := normalized.(map[string]any)
		if normalizedMap["type"] != types.APIToolTypeWebSearch {
			t.Fatalf("expected struct tool choice normalization, got %#v", normalized)
		}
	}
	invalidToolChoice := make(chan int)
	if _, ok := normalizeCodexToolChoiceValue(invalidToolChoice); ok {
		t.Fatal("expected unsupported tool_choice value to fail normalization")
	}
	if got := normalizeCodexToolChoiceMap(nil); got != nil {
		t.Fatalf("expected nil tool choice map to remain nil, got %#v", got)
	}
	if got := normalizeCodexBuiltinToolType(""); got != "" {
		t.Fatalf("expected blank builtin tool type to remain blank, got %q", got)
	}
	if got := normalizeCodexBuiltinToolType("file_search"); got != "file_search" {
		t.Fatalf("expected non-web-search builtin tool type to remain unchanged, got %q", got)
	}

	response := &types.OpenAIResponsesResponses{}
	backfillCodexResponsePromptCacheKey(response, &types.OpenAIResponsesRequest{PromptCacheKey: "stable"})
	if response.PromptCacheKey != "stable" {
		t.Fatalf("expected response prompt cache key backfill, got %q", response.PromptCacheKey)
	}
	backfillCodexResponsePromptCacheKey(response, &types.OpenAIResponsesRequest{PromptCacheKey: "ignored"})
	if response.PromptCacheKey != "stable" {
		t.Fatalf("expected existing response prompt cache key to win, got %q", response.PromptCacheKey)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Authorization", "Bearer sk-test-auth-header")
	ctx.Request.Header.Set("X-Session-Id", "session-xyz")
	ctx.Request.Header.Set("Session_Id", "legacy-session")
	ctx.Set("token_id", int32(42))
	ctx.Set("id", int64(7))
	ctx.Set("float_id", float64(9))
	ctx.Set("string_id", " 11 ")
	ctx.Set("bad_id", "nan")

	if got, ok := codexContextInt(ctx, "token_id"); !ok || got != 42 {
		t.Fatalf("expected int32 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "id"); !ok || got != 7 {
		t.Fatalf("expected int64 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "float_id"); !ok || got != 9 {
		t.Fatalf("expected float64 context coercion, got %d ok=%v", got, ok)
	}
	if got, ok := codexContextInt(ctx, "string_id"); !ok || got != 11 {
		t.Fatalf("expected string context coercion, got %d ok=%v", got, ok)
	}
	if _, ok := codexContextInt(ctx, "bad_id"); ok {
		t.Fatal("expected invalid numeric string to fail coercion")
	}
	if _, ok := codexContextInt(ctx, "missing_id"); ok {
		t.Fatal("expected missing context key to fail coercion")
	}

	if got := normalizePromptCacheStrategy(""); got != codexPromptCacheStrategyOff {
		t.Fatalf("expected blank strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy("AUTO"); got != codexPromptCacheStrategyAuto {
		t.Fatalf("expected auto strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy(" session_id "); got != codexPromptCacheStrategySessionID {
		t.Fatalf("expected session-id strategy normalization, got %q", got)
	}
	if got := normalizePromptCacheStrategy("weird"); got != codexPromptCacheStrategyOff {
		t.Fatalf("expected unknown strategy fallback, got %q", got)
	}
	if got := codexPromptCacheIdentity(nil, codexPromptCacheStrategyAuto); got != "" {
		t.Fatalf("expected nil context prompt cache identity to be empty, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyOff); got != "" {
		t.Fatalf("expected off strategy to disable prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategySessionID); got != "one-hub:codex:prompt-cache:session:session-xyz" {
		t.Fatalf("expected session-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyTokenID); got != "one-hub:codex:prompt-cache:token:42" {
		t.Fatalf("expected token-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyUserID); got != "one-hub:codex:prompt-cache:user:7" {
		t.Fatalf("expected user-id prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyAuthHeader); got != "one-hub:codex:prompt-cache:auth:test-auth-header" {
		t.Fatalf("expected auth-header prompt cache identity, got %q", got)
	}
	if got := codexPromptCacheIdentity(ctx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:session:session-xyz" {
		t.Fatalf("expected auto strategy to prefer session identity, got %q", got)
	}

	stableKeyRequest := &types.OpenAIResponsesRequest{}
	ensureStablePromptCacheKey(stableKeyRequest, ctx, codexPromptCacheStrategyUserID)
	expectedStableKey := uuid.NewSHA1(uuid.NameSpaceOID, []byte("one-hub:codex:prompt-cache:user:7")).String()
	if stableKeyRequest.PromptCacheKey != expectedStableKey {
		t.Fatalf("expected generated stable prompt cache key %q, got %q", expectedStableKey, stableKeyRequest.PromptCacheKey)
	}
	ensureStablePromptCacheKey(stableKeyRequest, ctx, codexPromptCacheStrategyOff)
	if stableKeyRequest.PromptCacheKey != expectedStableKey {
		t.Fatalf("expected existing prompt cache key to remain stable, got %q", stableKeyRequest.PromptCacheKey)
	}

	derivedKeyRequest := &types.OpenAIResponsesRequest{}
	requesthints.Set(ctx, map[string]string{requesthints.ResponsesPromptCacheKey: "derived-prompt-cache"})
	ensureStablePromptCacheKey(derivedKeyRequest, ctx, codexPromptCacheStrategyOff)
	if derivedKeyRequest.PromptCacheKey != "derived-prompt-cache" {
		t.Fatalf("expected derived prompt cache key from relay context to win, got %q", derivedKeyRequest.PromptCacheKey)
	}
}

func TestCodexResponsesRoutingHintResolver(t *testing.T) {
	originalSettings := RoutingHintSettingsInstance
	RoutingHintSettingsInstance = RoutingHintSettings{
		PromptCacheKeyStrategy: codexPromptCacheStrategyAuto,
		ModelRegex:             "^gpt-5$",
		UserAgentRegex:         "CodexClient",
	}
	t.Cleanup(func() {
		RoutingHintSettingsInstance = originalSettings
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	ctx.Request.Header.Set("X-Session-Id", "hint-session")
	ctx.Set("token_id", 42)

	request := &types.OpenAIResponsesRequest{Model: "gpt-5"}
	hints := requesthints.ResolveResponses(ctx, request)
	expectedKey := promptCacheKeyForStrategy(ctx, codexPromptCacheStrategyAuto)
	if got := hints[requesthints.ResponsesPromptCacheKey]; got != expectedKey {
		t.Fatalf("expected resolver to publish derived prompt cache key %q, got %#v", expectedKey, hints)
	}

	requesthints.Set(ctx, nil)
	request.PromptCacheKey = "client-key"
	if hints := requesthints.ResolveResponses(ctx, request); len(hints) != 0 {
		t.Fatalf("expected explicit prompt_cache_key to skip resolver, got %#v", hints)
	}
}

func TestCodexPromptCacheAutoPriorityFallsBackAcrossSignals(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(headers map[string]string) *gin.Context {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		for key, value := range headers {
			ctx.Request.Header.Set(key, value)
		}
		return ctx
	}

	sessionCtx := newCtx(map[string]string{
		"Authorization": "Bearer sk-auth-priority",
		"X-Session-Id":  "session-priority",
	})
	sessionCtx.Set("token_id", 11)
	sessionCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(sessionCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:session:session-priority" {
		t.Fatalf("expected session id to win auto priority, got %q", got)
	}

	authCtx := newCtx(map[string]string{
		"Authorization": "Bearer sk-auth-priority",
	})
	authCtx.Set("token_id", 11)
	authCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(authCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:auth:auth-priority" {
		t.Fatalf("expected auth header to win when session id is absent, got %q", got)
	}

	tokenCtx := newCtx(nil)
	tokenCtx.Set("token_id", 11)
	tokenCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(tokenCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:token:11" {
		t.Fatalf("expected token id to win when auth header is absent, got %q", got)
	}

	userCtx := newCtx(nil)
	userCtx.Set("id", 22)
	if got := codexPromptCacheIdentity(userCtx, codexPromptCacheStrategyAuto); got != "one-hub:codex:prompt-cache:user:22" {
		t.Fatalf("expected user id to win when token id is absent, got %q", got)
	}
}

func TestCodexResponsesSystemMessageAndStreamObserverHelpers(t *testing.T) {
	if !isSystemInputMessage(types.InputResponses{Role: types.ChatMessageRoleDeveloper}) {
		t.Fatal("expected developer role to be treated as system input")
	}
	if isSystemInputMessage(types.InputResponses{Type: types.InputTypeFileSearchCall, Role: types.ChatMessageRoleSystem}) {
		t.Fatal("expected non-message input type not to be treated as system input")
	}
	if !isMergeableInputMessage(types.InputResponses{Role: types.ChatMessageRoleUser}) {
		t.Fatal("expected user message to be mergeable")
	}
	if isMergeableInputMessage(types.InputResponses{Role: types.ChatMessageRoleAssistant}) {
		t.Fatal("expected assistant message not to be mergeable")
	}
	if got := extractInputMessageText(types.InputResponses{Content: "direct"}); got != "direct" {
		t.Fatalf("expected direct string content extraction, got %q", got)
	}
	if got := extractInputMessageText(types.InputResponses{
		Content: []types.ContentResponses{
			{Type: types.ContentTypeInputText, Text: "first"},
			{Type: types.ContentTypeOutputText, Text: "second"},
		},
	}); got != "first\nsecond" {
		t.Fatalf("expected list content extraction, got %q", got)
	}
	if got := extractInputMessageText(types.InputResponses{Content: func() {}}); got != "" {
		t.Fatalf("expected unparsable content extraction fallback, got %q", got)
	}

	stringPrepended := prependSystemTextToInputMessage(types.InputResponses{Content: "hello"}, "system")
	if content, _ := stringPrepended.Content.(string); content != "system\n\nhello" {
		t.Fatalf("expected system text to prepend string content, got %#v", stringPrepended.Content)
	}
	listPrepended := prependSystemTextToInputMessage(types.InputResponses{
		Content: []types.ContentResponses{
			{Type: types.ContentTypeInputText, Text: "hello"},
		},
	}, "system")
	parsedList, err := listPrepended.ParseContent()
	if err != nil || parsedList[0].Text != "system\n\nhello" {
		t.Fatalf("expected system text to prepend list content, parsed=%#v err=%v", parsedList, err)
	}
	fallbackPrepended := prependSystemTextToInputMessage(types.InputResponses{Content: func() {}}, "system")
	if content, _ := fallbackPrepended.Content.(string); content != "system" {
		t.Fatalf("expected unparsable content prepend fallback, got %#v", fallbackPrepended.Content)
	}

	request := &types.OpenAIResponsesRequest{
		Input: []types.InputResponses{
			{Role: types.ChatMessageRoleSystem, Content: "system one"},
			{Role: types.ChatMessageRoleDeveloper, Content: []types.ContentResponses{{Type: types.ContentTypeInputText, Text: "system two"}}},
			{Role: types.ChatMessageRoleUser, Content: []types.ContentResponses{{Type: types.ContentTypeInputText, Text: "hello"}}},
			{Role: types.ChatMessageRoleSystem, Content: "orphan"},
		},
	}
	mergeSystemInputMessagesForCodex(request)
	inputs, err := request.ParseInput()
	if err != nil {
		t.Fatalf("expected merged request input to parse, got %v", err)
	}
	if len(inputs) != 2 {
		t.Fatalf("expected merged input list, got %#v", inputs)
	}
	firstContents, err := inputs[0].ParseContent()
	if err != nil || firstContents[0].Text != "system one\n\nsystem two\n\nhello" {
		t.Fatalf("expected leading system text to merge into first user message, contents=%#v err=%v", firstContents, err)
	}
	secondContents, err := inputs[1].ParseContent()
	if err != nil || len(secondContents) != 1 || secondContents[0].Text != "orphan" {
		t.Fatalf("expected trailing system content to become synthetic user message, contents=%#v err=%v", secondContents, err)
	}

	var nilHandler *CodexResponsesStreamHandler
	nilHandler.observeUsageEvent(`{"type":"response.output_text.delta","delta":"hello"}`)

	usage := &types.Usage{}
	handler := newCodexResponsesStreamHandler(usage)
	handler.observeUsageEvent("{bad-json")
	handler.observeUsageEvent(`{"type":"response.output_text.delta","delta":"hello"}`)
	handler.observeUsageEvent(`{"type":"response.output_item.added","item":{"type":"web_search_call","id":"ws_1"},"response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.observeUsageEvent(`{"type":"response.done","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"type":"web_search_call","id":"ws_1","status":"completed"}]}}`)

	if usage.TextBuilder.String() != "hello" {
		t.Fatalf("expected stream observer to accumulate text deltas, got %q", usage.TextBuilder.String())
	}
	if usage.TotalTokens != 8 || usage.PromptTokens != 3 || usage.CompletionTokens != 5 {
		t.Fatalf("expected terminal usage snapshot to be applied, got %+v", usage)
	}
	if usage.ExtraBilling[types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "high")].CallCount != 1 {
		t.Fatalf("expected stream observer to preserve tool billing, got %+v", usage.ExtraBilling)
	}

	rawResponse, err := json.Marshal(request)
	if err != nil || len(rawResponse) == 0 {
		t.Fatalf("expected merged request to remain json serializable, err=%v", err)
	}
}
