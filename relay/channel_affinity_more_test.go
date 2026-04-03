package relay

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/internal/requesthints"
	runtimeaffinity "one-api/runtime/channelaffinity"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func withChannelAffinitySettings(t *testing.T, settings config.ChannelAffinitySettings) *runtimeaffinity.Manager {
	t.Helper()

	originalSettings := config.ChannelAffinitySettingsInstance
	originalRedisEnabled := config.RedisEnabled
	config.ChannelAffinitySettingsInstance = settings
	config.RedisEnabled = false

	manager := runtimeaffinity.ConfigureDefault(channelAffinityManagerOptions(settings))
	manager.Clear()

	t.Cleanup(func() {
		manager.Clear()
		config.ChannelAffinitySettingsInstance = originalSettings
		config.RedisEnabled = originalRedisEnabled
	})
	return manager
}

func newAffinityTestContext(method, target string) *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, target, nil)
	return ctx
}

func TestChannelAffinityContextHelpersAndMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 60,
		MaxEntries:        20,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "realtime-default",
				Enabled:         true,
				Kind:            "realtime",
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "header", Key: "x-session-id", Alias: config.ChannelAffinityAliasSessionID},
				},
			},
		},
	}
	settings.Normalize()
	withChannelAffinitySettings(t, settings)

	if currentChannelAffinityState(nil) != nil {
		t.Fatal("expected nil context to have no affinity state")
	}

	ctx := newAffinityTestContext(http.MethodGet, "/v1/realtime?session_id=fallback-session")
	ctx.Set("token_id", 88)
	ctx.Set("token_group", "team-a")
	ctx.Set(channelAffinityStateContextKey, "wrong-type")
	if currentChannelAffinityState(ctx) != nil {
		t.Fatal("expected wrong typed affinity state to be ignored")
	}

	rememberChannelAffinityKey(ctx, channelAffinityKindRealtime, "session-123")
	state := currentChannelAffinityState(ctx)
	if state == nil || state.Lookup == nil || currentChannelAffinityKey(ctx) == "" {
		t.Fatalf("expected remembered affinity binding, got state=%#v", state)
	}

	setPreferredChannelFromAffinity(ctx, 42)
	if got := currentPreferredChannelID(ctx); got != 42 {
		t.Fatalf("expected preferred channel id 42, got %d", got)
	}
	setPreferredChannelFromAffinity(ctx, 0)
	if got := currentPreferredChannelID(ctx); got != 0 {
		t.Fatalf("expected preferred channel reset, got %d", got)
	}

	ctx.Set(channelAffinityStrictContextKey, true)
	ctx.Set(channelAffinitySkipRetryContextKey, true)
	ctx.Set(channelAffinityIgnoreCooldownContextKey, true)
	setChannelAffinitySelectedPreferred(ctx, true)
	if !currentChannelAffinityStrict(ctx) || !currentChannelAffinitySkipRetry(ctx) || !currentChannelAffinityIgnorePreferredCooldown(ctx) || !currentChannelAffinitySelectedPreferred(ctx) {
		t.Fatal("expected affinity context flags to round-trip")
	}
	if !shouldSkipRetryAfterAffinityFailure(ctx) {
		t.Fatal("expected selected preferred + skip retry to suppress retry")
	}

	ctx.Set(config.GinChannelAffinityMetaKey, map[string]any{"alpha": "one"})
	meta := currentChannelAffinityLogMeta(ctx)
	meta["alpha"] = "mutated"
	if currentChannelAffinityLogMeta(ctx)["alpha"] != "one" {
		t.Fatal("expected currentChannelAffinityLogMeta to clone stored metadata")
	}
	mergeChannelAffinityMeta(ctx, map[string]any{"beta": 2})
	if got := currentChannelAffinityLogMeta(ctx)["beta"]; got != 2 {
		t.Fatalf("expected merged metadata to be visible, got %#v", got)
	}
	mergeChannelAffinityMeta(nil, map[string]any{"ignored": true})

	if got := channelAffinityScope(ctx); got != "token:88" {
		t.Fatalf("expected token affinity scope, got %q", got)
	}
	userCtx := newAffinityTestContext(http.MethodGet, "/v1/responses")
	userCtx.Set("id", 19)
	if got := channelAffinityScope(userCtx); got != "user:19" {
		t.Fatalf("expected user affinity scope, got %q", got)
	}
	anonCtx := newAffinityTestContext(http.MethodGet, "/v1/responses")
	if got := channelAffinityScope(anonCtx); got != "anonymous" {
		t.Fatalf("expected anonymous affinity scope, got %q", got)
	}
}

func TestChannelAffinityEvaluationLookupAndHelperFunctions(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 30,
		MaxEntries:        10,
		Rules: []config.ChannelAffinityRule{
			{
				Name:                    "responses-prompt",
				Enabled:                 true,
				Kind:                    "responses",
				ModelRegex:              "^gpt-5$",
				PathRegex:               "^/v1/responses$",
				UserAgentRegex:          "CodexClient",
				IncludeGroup:            true,
				IncludeModel:            true,
				IncludePath:             true,
				IncludeRuleName:         true,
				Strict:                  true,
				SkipRetryOnFailure:      true,
				IgnorePreferredCooldown: true,
				RecordOnSuccess:         true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
					{Source: "request_hint", Key: requesthints.ResponsesPromptCacheKey, Alias: config.ChannelAffinityAliasPromptCacheKey},
					{Source: "query", Key: "trace", Alias: "trace_id", ValueRegex: "^trace-"},
				},
			},
			{
				Name:            "responses-previous",
				Enabled:         true,
				Kind:            "responses",
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
				},
			},
			{
				Name:            "realtime-session",
				Enabled:         true,
				Kind:            "realtime",
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "header", Key: "x-session-id", Alias: config.ChannelAffinityAliasSessionID},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	ctx := newAffinityTestContext(http.MethodPost, "/v1/responses?trace=trace-123")
	ctx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	ctx.Set("token_group", "team-a")
	request := &types.OpenAIResponsesRequest{
		Model:              "gpt-5",
		PromptCacheKey:     "pc-1",
		PreviousResponseID: "resp-prev-1",
		Metadata: map[string]string{
			"tenant": "tenant-a",
		},
	}

	promptTemplate := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasPromptCacheKey, settings.DefaultTTLSeconds)
	promptKey := promptTemplate.BuildKey(request.PromptCacheKey)
	manager.SetRecord(promptKey, runtimeaffinity.Record{
		ChannelID:         77,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)

	state := evaluateChannelAffinity(ctx, channelAffinityKindResponses, channelAffinityInput{ResponsesRequest: request})
	if state == nil || !state.Hit || state.PreferredChannelID != 77 {
		t.Fatalf("expected channel affinity hit, got %#v", state)
	}
	if state.Lookup == nil || state.Lookup.Template.Source != config.ChannelAffinityAliasPromptCacheKey {
		t.Fatalf("expected prompt_cache_key binding to win, got %#v", state.Lookup)
	}
	if len(state.RequestBindings) < 2 {
		t.Fatalf("expected multiple request bindings, got %#v", state.RequestBindings)
	}

	applyChannelAffinityState(ctx, state)
	if got := currentPreferredChannelID(ctx); got != 77 {
		t.Fatalf("expected preferred channel id 77 after apply, got %d", got)
	}
	if !currentChannelAffinityStrict(ctx) || !currentChannelAffinitySkipRetry(ctx) || !currentChannelAffinityIgnorePreferredCooldown(ctx) {
		t.Fatal("expected applied state to set strict/skip-retry/ignore-cooldown flags")
	}
	if channelID, ok := lookupChannelAffinity(ctx, channelAffinityKindResponses, request.PromptCacheKey); !ok || channelID != 77 {
		t.Fatalf("expected lookup to resolve recorded prompt cache key binding, got channel=%d ok=%v", channelID, ok)
	}

	recordCurrentChannelAffinity(ctx, channelAffinityKindResponses, 88)
	if channelID, ok := lookupChannelAffinity(ctx, channelAffinityKindResponses, request.PromptCacheKey); !ok || channelID != 88 {
		t.Fatalf("expected recordCurrentChannelAffinity to update prompt cache binding, got channel=%d ok=%v", channelID, ok)
	}

	recordResponsesChannelAffinity(ctx, 88, &types.OpenAIResponsesResponses{
		ID:             "resp-final-1",
		PromptCacheKey: "pc-1",
	})
	responseCtx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	responseCtx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	responseCtx.Set("token_group", "team-a")
	responseRequest := &types.OpenAIResponsesRequest{Model: "gpt-5", PreviousResponseID: "resp-final-1"}
	prepareResponsesChannelAffinity(responseCtx, responseRequest)
	if channelID, ok := lookupChannelAffinity(responseCtx, channelAffinityKindResponses, "resp-final-1"); !ok || channelID != 88 {
		t.Fatalf("expected response id derived binding to be recorded, got channel=%d ok=%v", channelID, ok)
	}

	clearCurrentChannelAffinity(ctx)
	if _, ok := lookupChannelAffinity(ctx, channelAffinityKindResponses, request.PromptCacheKey); ok {
		t.Fatal("expected clearCurrentChannelAffinity to remove current binding")
	}

	lock := channelAffinityLock(ctx, channelAffinityKindResponses, request.PromptCacheKey)
	lock()
	unboundCtx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
	unlock := channelAffinityLock(unboundCtx, channelAffinityKindRealtime, "session-xyz")
	unlock()

	explicitPinCtx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	explicitPinCtx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	explicitPinCtx.Set("token_group", "team-a")
	explicitPinCtx.Set("specific_channel_id", 999)
	pinnedState := evaluateChannelAffinity(explicitPinCtx, channelAffinityKindResponses, channelAffinityInput{ResponsesRequest: request})
	if pinnedState == nil || pinnedState.Hit {
		t.Fatalf("expected explicit pin to skip affinity hit selection, got %#v", pinnedState)
	}

	if got := extractChannelAffinityValue(ctx, channelAffinityInput{ResponsesRequest: request, RealtimeSessionID: "session-fallback"}, config.ChannelAffinityKeySource{Source: "request_field", Key: "prompt_cache_key"}); got != "pc-1" {
		t.Fatalf("expected request field extraction, got %q", got)
	}
	if got := extractChannelAffinityValue(ctx, channelAffinityInput{RealtimeSessionID: "session-fallback"}, config.ChannelAffinityKeySource{Source: "header", Key: "missing", Alias: config.ChannelAffinityAliasSessionID}); got != "session-fallback" {
		t.Fatalf("expected realtime session id fallback extraction, got %q", got)
	}
	if got := extractChannelAffinityValue(ctx, channelAffinityInput{}, config.ChannelAffinityKeySource{Source: "query", Key: "trace", ValueRegex: "^trace-"}); got != "trace-123" {
		t.Fatalf("expected query extraction, got %q", got)
	}
	if got := extractChannelAffinityValue(ctx, channelAffinityInput{}, config.ChannelAffinityKeySource{Source: "query", Key: "trace", ValueRegex: "^skip-"}); got != "" {
		t.Fatalf("expected value regex mismatch to reject extraction, got %q", got)
	}
	if got := extractChannelAffinityRequestField(request, "metadata.tenant"); got != "tenant-a" {
		t.Fatalf("expected metadata request field extraction, got %q", got)
	}
	if got := extractChannelAffinityRequestField(request, "metadata."); got != "" {
		t.Fatalf("expected empty metadata key to be rejected, got %q", got)
	}
	if got := extractChannelAffinityRequestField(nil, "prompt_cache_key"); got != "" {
		t.Fatalf("expected nil request extraction to return empty string, got %q", got)
	}

	derivedCtx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	derivedCtx.Request.Header.Set("User-Agent", "CodexClient/1.0")
	derivedCtx.Set("token_group", "team-a")
	derivedRequest := &types.OpenAIResponsesRequest{Model: "gpt-5"}
	derivedPromptCacheKey := "derived-prompt-cache"
	requesthints.Set(derivedCtx, map[string]string{requesthints.ResponsesPromptCacheKey: derivedPromptCacheKey})
	derivedTemplate := newChannelAffinityTemplate(derivedCtx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_hint", config.ChannelAffinityAliasPromptCacheKey, settings.DefaultTTLSeconds)
	manager.SetRecord(derivedTemplate.BuildKey(derivedPromptCacheKey), runtimeaffinity.Record{
		ChannelID:         66,
		ResumeFingerprint: "model:gpt-5",
	}, time.Minute)

	if got := extractChannelAffinityValue(derivedCtx, channelAffinityInput{}, config.ChannelAffinityKeySource{Source: "request_hint", Key: requesthints.ResponsesPromptCacheKey}); got != derivedPromptCacheKey {
		t.Fatalf("expected request hint extraction, got %q", got)
	}
	derivedState := evaluateChannelAffinity(derivedCtx, channelAffinityKindResponses, channelAffinityInput{ResponsesRequest: derivedRequest})
	applyChannelAffinityState(derivedCtx, derivedState)
	if got := currentPreferredChannelID(derivedCtx); got != 66 {
		t.Fatalf("expected request hint affinity hit to select channel 66, got %d", got)
	}

	template := newChannelAffinityTemplate(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0], "request_field", config.ChannelAffinityAliasPromptCacheKey, settings.DefaultTTLSeconds)
	if template.Source != config.ChannelAffinityAliasPromptCacheKey || template.InputSource != "request_field" {
		t.Fatalf("unexpected affinity template: %+v", template)
	}
	if got := template.BuildKey("pc-1"); got == "" {
		t.Fatal("expected affinity template to build a stable key")
	}
	if got := (channelAffinityTemplate{}).BuildKey("pc-1"); got != "" {
		t.Fatalf("expected empty template not to build a key, got %q", got)
	}

	values := derivedResponseAffinityValues(&types.OpenAIResponsesResponses{ID: "resp-x", PromptCacheKey: "pc-x"})
	if values[config.ChannelAffinityAliasResponseID] != "resp-x" || values[config.ChannelAffinityAliasPromptCacheKey] != "pc-x" {
		t.Fatalf("unexpected derived response affinity values: %#v", values)
	}
	if derivedResponseAffinityValues(nil) != nil {
		t.Fatal("expected nil response to have no derived affinity values")
	}

	metaCtx := newAffinityTestContext(http.MethodGet, "/v1/responses")
	refreshChannelAffinityMeta(metaCtx, nil, 0)
	if currentChannelAffinityLogMeta(metaCtx)["channel_affinity_hit"] != false {
		t.Fatalf("expected nil state refresh to record miss metadata, got %#v", currentChannelAffinityLogMeta(metaCtx))
	}
	refreshChannelAffinityMeta(metaCtx, state, 88)
	meta := currentChannelAffinityLogMeta(metaCtx)
	if meta["channel_affinity_selected_id"] != 88 || meta["channel_affinity_rule"] == "" {
		t.Fatalf("expected populated affinity metadata, got %#v", meta)
	}

	if got := channelAffinityModelName(channelAffinityKindResponses, channelAffinityInput{ResponsesRequest: request}); got != "gpt-5" {
		t.Fatalf("expected responses model name, got %q", got)
	}
	if got := channelAffinityModelName(channelAffinityKindRealtime, channelAffinityInput{ModelName: "gpt-4o-realtime"}); got != "gpt-4o-realtime" {
		t.Fatalf("expected realtime model name, got %q", got)
	}

	stats := ChannelAffinityCacheStats()
	if stats["enabled"] != true || stats["rules_count"] != 3 {
		t.Fatalf("unexpected cache stats: %#v", stats)
	}
	manager.Set("to-clear", 1, time.Minute)
	if cleared := ClearChannelAffinityCache(); cleared < 1 {
		t.Fatalf("expected cache clear to remove entries, got %d", cleared)
	}

	if !channelAffinityRuleMatches(ctx, channelAffinityKindResponses, "gpt-5", settings.Rules[0]) {
		t.Fatal("expected channel affinity rule to match request")
	}
	if channelAffinityRuleMatches(ctx, channelAffinityKindRealtime, "gpt-5", settings.Rules[0]) {
		t.Fatal("expected mismatched rule kind not to match")
	}

	affinityRegexCache.Store("poisoned", "not-a-regexp")
	if channelAffinityRegexMatch("poisoned", "value") {
		t.Fatal("expected non-regexp cache entry to fail safely")
	}
	if !channelAffinityRegexMatch("", "value") {
		t.Fatal("expected blank regex to match by default")
	}
	if channelAffinityRegexMatch("[", "value") {
		t.Fatal("expected invalid regex to fail")
	}
	if !channelAffinityRegexMatch("^trace-", " trace-123 ") {
		t.Fatal("expected regex match to trim whitespace from value")
	}
	affinityRegexCache.Store("poisoned", regexp.MustCompile("^safe$"))

	if len(appendUniqueChannelAffinityBinding(nil, nil)) != 0 {
		t.Fatal("expected nil binding not to be appended")
	}
	binding := &channelAffinityBinding{Key: "binding-key"}
	bindings := appendUniqueChannelAffinityBinding(nil, binding)
	bindings = appendUniqueChannelAffinityBinding(bindings, binding)
	if len(bindings) != 1 {
		t.Fatalf("expected duplicate affinity binding to be ignored, got %#v", bindings)
	}

	recorderTemplate := channelAffinityTemplate{RuleName: "rule-a", Source: "prompt", Parts: []string{"a"}, RecordOnSuccess: true}
	recorders := appendChannelAffinityRecorder(nil, "prompt", recorderTemplate)
	recorders = appendChannelAffinityRecorder(recorders, "prompt", recorderTemplate)
	recorders = appendChannelAffinityRecorder(recorders, "", recorderTemplate)
	if len(recorders["prompt"]) != 1 {
		t.Fatalf("expected duplicate affinity recorder to be ignored, got %#v", recorders)
	}
	if got := appendChannelAffinityRecorder(nil, "prompt", channelAffinityTemplate{RecordOnSuccess: false}); len(got) != 0 {
		t.Fatalf("expected non-recording template not to be added, got %#v", got)
	}

	if got := channelAffinityResumeFingerprint(channelAffinityKindRealtime, channelAffinityInput{ModelName: "gpt-5"}); got != "" {
		t.Fatalf("expected realtime affinity fingerprint to be empty, got %q", got)
	}
	if got := channelAffinityResumeFingerprint(channelAffinityKindResponses, channelAffinityInput{ResponsesRequest: request}); got != "model:gpt-5" {
		t.Fatalf("expected responses fingerprint to include model, got %q", got)
	}
	if got := channelAffinityStateResumeFingerprint(nil); got != "" {
		t.Fatalf("expected nil state fingerprint to be empty, got %q", got)
	}
	if !channelAffinityRecordMatchesState(runtimeaffinity.Record{ResumeFingerprint: ""}, state) {
		t.Fatal("expected empty record fingerprint to be treated as compatible")
	}
	if channelAffinityRecordMatchesState(runtimeaffinity.Record{ResumeFingerprint: "model:other"}, state) {
		t.Fatal("expected mismatched fingerprint to be rejected")
	}

	if got := defaultChannelAffinityAlias(channelAffinityKindRealtime); got != config.ChannelAffinityAliasSessionID {
		t.Fatalf("expected realtime default alias session_id, got %q", got)
	}
	if got := defaultChannelAffinityAlias(channelAffinityKindResponses); got != config.ChannelAffinityAliasPromptCacheKey {
		t.Fatalf("expected responses default alias prompt_cache_key, got %q", got)
	}
}

func TestChannelAffinityTemplateUsesRoutingGroupScope(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rule := config.ChannelAffinityRule{
		Name:            "responses-prompt",
		Enabled:         true,
		Kind:            "responses",
		IncludeGroup:    true,
		IncludeRuleName: true,
		RecordOnSuccess: true,
	}

	tokenCtx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	tokenCtx.Set("token_group", "team-a")
	tokenTemplate := newChannelAffinityTemplate(tokenCtx, channelAffinityKindResponses, "gpt-5", rule, "request_field", config.ChannelAffinityAliasPromptCacheKey, 60)

	backupCtx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	backupCtx.Set("token_group", "team-a")
	groupctx.SetRoutingGroup(backupCtx, "team-b", groupctx.RoutingGroupSourceBackupGroup)
	backupTemplate := newChannelAffinityTemplate(backupCtx, channelAffinityKindResponses, "gpt-5", rule, "request_field", config.ChannelAffinityAliasPromptCacheKey, 60)

	if len(tokenTemplate.Parts) == 0 || len(backupTemplate.Parts) == 0 {
		t.Fatalf("expected affinity templates to include scoped parts, got token=%#v backup=%#v", tokenTemplate, backupTemplate)
	}
	if !hasChannelAffinityPart(tokenTemplate.Parts, "group:team-a") {
		t.Fatalf("expected default affinity scope to use token group, got %#v", tokenTemplate.Parts)
	}
	if !hasChannelAffinityPart(backupTemplate.Parts, "group:team-b") {
		t.Fatalf("expected affinity scope to use routing group after fallback, got %#v", backupTemplate.Parts)
	}

	tokenKey := tokenTemplate.BuildKey("pc-1")
	backupKey := backupTemplate.BuildKey("pc-1")
	if tokenKey == "" || backupKey == "" {
		t.Fatalf("expected affinity templates to build keys, got token=%q backup=%q", tokenKey, backupKey)
	}
	if tokenKey == backupKey {
		t.Fatalf("expected different routing groups to build different affinity keys, got %q", tokenKey)
	}

	refreshChannelAffinityMeta(backupCtx, nil, 0)
	meta := currentChannelAffinityLogMeta(backupCtx)
	if meta["using_group"] != "team-b" || meta["token_group"] != "team-a" {
		t.Fatalf("expected routing group metadata in affinity meta, got %#v", meta)
	}
	if meta["routing_group_source"] != groupctx.RoutingGroupSourceBackupGroup {
		t.Fatalf("expected affinity meta to expose routing group source, got %#v", meta)
	}
}

func hasChannelAffinityPart(parts []string, target string) bool {
	for _, part := range parts {
		if part == target {
			return true
		}
	}
	return false
}

func TestChannelAffinityFallbackRulesAndGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("disabled settings short circuit", func(t *testing.T) {
		withChannelAffinitySettings(t, config.ChannelAffinitySettings{})

		ctx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
		if state := evaluateChannelAffinity(ctx, channelAffinityKindRealtime, channelAffinityInput{RealtimeSessionID: "sess-disabled"}); state != nil {
			t.Fatalf("expected disabled settings to skip affinity evaluation, got %#v", state)
		}
		if binding := defaultChannelAffinityBinding(ctx, channelAffinityKindRealtime, "sess-disabled"); binding != nil {
			t.Fatalf("expected disabled settings to skip default affinity binding, got %#v", binding)
		}
	})

	t.Run("fallback binding lookup and mismatched record update", func(t *testing.T) {
		settings := config.ChannelAffinitySettings{
			Enabled:           true,
			DefaultTTLSeconds: 45,
			MaxEntries:        20,
			Rules: []config.ChannelAffinityRule{
				{
					Name:            "responses-by-id",
					Enabled:         true,
					Kind:            "responses",
					IncludeRuleName: true,
					RecordOnSuccess: true,
					KeySources: []config.ChannelAffinityKeySource{
						{Source: "request_field", Key: "previous_response_id", Alias: config.ChannelAffinityAliasResponseID},
					},
				},
			},
		}
		settings.Normalize()
		manager := withChannelAffinitySettings(t, settings)

		rememberChannelAffinityKey(nil, channelAffinityKindRealtime, "ignored")
		applyChannelAffinityState(nil, nil)

		emptyCtx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
		rememberChannelAffinityKey(emptyCtx, channelAffinityKindRealtime, "")
		emptyState := currentChannelAffinityState(emptyCtx)
		if emptyState == nil || emptyState.Lookup != nil || len(emptyState.RequestBindings) != 0 {
			t.Fatalf("expected empty affinity remember to keep placeholder state only, got %#v", emptyState)
		}

		ctx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
		ctx.Set("token_id", 55)

		binding := defaultChannelAffinityBinding(ctx, channelAffinityKindRealtime, "fallback-session")
		if binding == nil {
			t.Fatal("expected realtime default binding to fall back to synthetic default rule")
		}
		if binding.Template.RuleName != string(channelAffinityKindRealtime) || binding.Template.InputSource != "default" {
			t.Fatalf("unexpected fallback realtime binding template: %+v", binding.Template)
		}
		if key := buildChannelAffinityKey(ctx, channelAffinityKindRealtime, "fallback-session"); key != binding.Key {
			t.Fatalf("expected helper key to match binding key, got %q want %q", key, binding.Key)
		}

		manager.SetRecord(binding.Key, runtimeaffinity.Record{ChannelID: 71}, time.Minute)
		if channelID, ok := lookupChannelAffinity(ctx, channelAffinityKindRealtime, "fallback-session"); !ok || channelID != 71 {
			t.Fatalf("expected default binding lookup to resolve fallback channel, got channel=%d ok=%v", channelID, ok)
		}

		ctx.Set(channelAffinityStateContextKey, &channelAffinityState{
			Kind:   channelAffinityKindResponses,
			Lookup: binding,
		})
		recordCurrentChannelAffinity(ctx, channelAffinityKindRealtime, 72)
		record, ok := manager.Get(binding.Key)
		if !ok || record.ChannelID != 72 {
			t.Fatalf("expected mismatched state recordCurrentChannelAffinity to update current key, got record=%+v ok=%v", record, ok)
		}
	})
}

func TestChannelAffinityAdditionalNilAndRecorderBranches(t *testing.T) {
	gin.SetMode(gin.TestMode)

	settings := config.ChannelAffinitySettings{
		Enabled:           true,
		DefaultTTLSeconds: 30,
		MaxEntries:        10,
		Rules: []config.ChannelAffinityRule{
			{
				Name:            "responses-default",
				Enabled:         true,
				Kind:            "responses",
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "request_field", Key: "prompt_cache_key", Alias: config.ChannelAffinityAliasPromptCacheKey},
				},
			},
			{
				Name:            "realtime-session",
				Enabled:         true,
				Kind:            "realtime",
				IncludeRuleName: true,
				RecordOnSuccess: true,
				KeySources: []config.ChannelAffinityKeySource{
					{Source: "header", Key: "x-session-id", Alias: config.ChannelAffinityAliasSessionID},
				},
			},
		},
	}
	settings.Normalize()
	manager := withChannelAffinitySettings(t, settings)

	setPreferredChannelFromAffinity(nil, 1)
	setChannelAffinitySelectedPreferred(nil, true)
	if currentPreferredChannelID(nil) != 0 || currentChannelAffinityStrict(nil) || currentChannelAffinitySkipRetry(nil) || currentChannelAffinityIgnorePreferredCooldown(nil) || currentChannelAffinitySelectedPreferred(nil) {
		t.Fatal("expected nil affinity helper accessors to return zero values")
	}
	if meta := currentChannelAffinityLogMeta(nil); meta != nil {
		t.Fatalf("expected nil context log meta to stay nil, got %#v", meta)
	}
	if scope := channelAffinityScope(nil); scope != "" {
		t.Fatalf("expected nil channel affinity scope to be empty, got %q", scope)
	}

	ctx := newAffinityTestContext(http.MethodPost, "/v1/responses")
	ctx.Set("token_id", 7)
	ctx.Set(channelAffinityStateContextKey, &channelAffinityState{
		Kind: channelAffinityKindResponses,
		RequestBindings: []*channelAffinityBinding{
			nil,
			{
				Template: channelAffinityTemplate{RecordOnSuccess: false, TTL: time.Minute},
				Key:      "skip-record",
			},
		},
	})
	recordCurrentChannelAffinity(ctx, channelAffinityKindResponses, 91)
	if _, ok := manager.Get("skip-record"); ok {
		t.Fatal("expected non-recording response bindings not to be persisted")
	}

	ctx.Set(channelAffinityStateContextKey, &channelAffinityState{
		Kind:   channelAffinityKindResponses,
		Lookup: &channelAffinityBinding{Key: "current-key"},
		RequestBindings: []*channelAffinityBinding{
			{
				Template: channelAffinityTemplate{RecordOnSuccess: true, TTL: time.Minute},
				Key:      "current-key",
			},
		},
		DerivedRecorders: map[string][]channelAffinityTemplate{config.ChannelAffinityAliasResponseID: {{}}},
	})
	recordResponsesChannelAffinity(ctx, 91, &types.OpenAIResponsesResponses{ID: "resp-derived"})
	if _, ok := manager.Get("current-key"); !ok {
		t.Fatal("expected response affinity recording to update the current request binding")
	}
	recordResponsesChannelAffinity(nil, 91, nil)
	recordResponsesChannelAffinity(ctx, 0, nil)

	unboundCtx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
	unlock := channelAffinityLock(unboundCtx, channelAffinityKindRealtime, "")
	unlock()
	clearCurrentChannelAffinity(nil)
	clearCurrentChannelAffinity(unboundCtx)

	disabled := config.ChannelAffinitySettingsInstance
	config.ChannelAffinitySettingsInstance = config.ChannelAffinitySettings{}
	if got := prepareRealtimeChannelAffinity(unboundCtx, "gpt-5", "session-miss"); got != 0 {
		t.Fatalf("expected realtime affinity preparation miss for disabled settings, got %d", got)
	}
	config.ChannelAffinitySettingsInstance = disabled

	realtimeCtx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
	realtimeCtx.Request.Header.Set("X-Session-Id", "session-hit")
	realtimeCtx.Set("token_id", 7)
	realtimeBinding := defaultChannelAffinityBinding(realtimeCtx, channelAffinityKindRealtime, "session-hit")
	manager.SetRecord(realtimeBinding.Key, runtimeaffinity.Record{ChannelID: 111}, time.Minute)
	if got := prepareRealtimeChannelAffinity(realtimeCtx, "gpt-5", "session-hit"); got != 111 {
		t.Fatalf("expected realtime affinity preparation hit, got %d", got)
	}
	if buildChannelAffinityKey(realtimeCtx, channelAffinityKindRealtime, "") != "" {
		t.Fatal("expected blank affinity values not to build a key")
	}

	wrongMetaCtx := newAffinityTestContext(http.MethodGet, "/v1/realtime")
	wrongMetaCtx.Set(config.GinChannelAffinityMetaKey, "wrong-type")
	if meta := currentChannelAffinityLogMeta(wrongMetaCtx); meta != nil {
		t.Fatalf("expected wrong typed affinity metadata to be ignored, got %#v", meta)
	}
}
