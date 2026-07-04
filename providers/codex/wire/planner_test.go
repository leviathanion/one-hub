package wire

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/jsonobject"
	"one-api/common/requestctx"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
)

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

func snapshot(headers map[string][]string) HeaderSnapshot {
	httpHeaders := http.Header{}
	for name, values := range headers {
		httpHeaders[name] = append([]string(nil), values...)
	}
	return requestctx.NewHeaderSnapshot(httpHeaders)
}

func mustEnvelope(t *testing.T, raw string) *commonresponses.RawEnvelope {
	t.Helper()
	envelope, err := commonresponses.ParseRawEnvelope([]byte(raw))
	if err != nil {
		t.Fatalf("parse raw envelope: %v", err)
	}
	return envelope
}

func mustDecodeObject(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("decode JSON object: %v body=%s", err, raw)
	}
	return object
}

func TestResponsesCreatePlannerUsesOfficialHeadersAndRawBody(t *testing.T) {
	envelope := mustEnvelope(t, `{
		"model":"gpt-5",
		"stream":false,
		"input":"hi",
		"prompt_cache_key":"pc-client",
		"client_metadata":{
			"session_id":"sess-body",
			"thread_id":"thread-body",
			"x-codex-window-id":"window-body",
			"x-codex-installation-id":"install-body",
			"future":"keep"
		},
		"unknown_number":12345678901234567890,
		"future_object":{"enabled":true},
		"reasoning":{"effort":"medium"},
		"include":["output_text.annotations"]
	}`)
	headers := snapshot(map[string][]string{
		"Authorization":   {"Bearer downstream-token"},
		"X-Session-Id":    {"legacy-session"},
		"Conversation_id": {"legacy-conversation"},
		"Connection":      {"keep-alive"},
		"OpenAI-Beta":     {"responses=v1"},
		"User-Agent":      {"codex_cli_rs/1.0"},
		"originator":      {"codex_cli_rs"},
	})
	metadata, err := MetadataFromResponsesBody(envelope.Object)
	if err != nil {
		t.Fatalf("metadata from body: %v", err)
	}
	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   headers,
		Metadata:  metadata,
		Policy: ChannelPolicy{
			DefaultOriginator: "codex_cli_rs",
		},
		Principal: PrincipalFingerprint{Kind: "api_key", HMAC: "principal-hmac"},
		ChannelID: 9,
	})
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	plan, err := BuildHeaders(HeaderPlanInput{
		Operation: OpResponsesCreate,
		Headers:   headers,
		Credential: Credential{
			AccessToken: "upstream-token",
			AccountID:   "acct-123",
		},
		Policy: ChannelPolicy{
			DefaultOriginator: "codex_cli_rs",
		},
		Identity: identity,
	})
	if err != nil {
		t.Fatalf("build headers: %v", err)
	}

	upstreamHeaders := plan.HTTPHeader()
	if got := upstreamHeaders.Get("Authorization"); got != "Bearer upstream-token" {
		t.Fatalf("expected upstream Authorization from channel token, got %q", got)
	}
	if got := upstreamHeaders.Get("ChatGPT-Account-ID"); got != "acct-123" {
		t.Fatalf("expected channel account id, got %q", got)
	}
	if got := upstreamHeaders.Get("session-id"); got != "sess-body" {
		t.Fatalf("expected session-id from body metadata, got %q", got)
	}
	if got := upstreamHeaders.Get("thread-id"); got != "thread-body" {
		t.Fatalf("expected thread-id from body metadata, got %q", got)
	}
	if got := upstreamHeaders.Get("x-client-request-id"); got != "thread-body" {
		t.Fatalf("expected client request id to fall back to thread-id, got %q", got)
	}
	if got := upstreamHeaders.Get("x-codex-installation-id"); got != "install-body" {
		t.Fatalf("expected create installation id from body metadata, got %q", got)
	}
	for _, forbidden := range []string{"session_id", "x-session-id", "Conversation_id", "Connection", "OpenAI-Beta"} {
		if got := upstreamHeaders.Get(forbidden); got != "" {
			t.Fatalf("expected %s to be absent from create headers, got %q", forbidden, got)
		}
	}

	body, err := PlanResponsesCreateBody(envelope.Object, CreateBodyInput{Model: "gpt-5-codex", Stream: true})
	if err != nil {
		t.Fatalf("plan create body: %v", err)
	}
	object := mustDecodeObject(t, body)
	if string(object["model"]) != `"gpt-5-codex"` || string(object["stream"]) != `true` || string(object["store"]) != `false` {
		t.Fatalf("expected model/stream/store patches, got %s", body)
	}
	if string(object["prompt_cache_key"]) != `"pc-client"` {
		t.Fatalf("expected prompt_cache_key to be preserved, got %s", object["prompt_cache_key"])
	}
	if string(object["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected unknown numeric field to be preserved, got %s", object["unknown_number"])
	}
	if string(object["future_object"]) != `{"enabled":true}` {
		t.Fatalf("expected unknown object field to be preserved, got %s", object["future_object"])
	}
	var clientMetadata map[string]json.RawMessage
	if err := json.Unmarshal(object["client_metadata"], &clientMetadata); err != nil {
		t.Fatalf("decode client_metadata: %v", err)
	}
	if string(clientMetadata["future"]) != `"keep"` || string(clientMetadata["x-codex-installation-id"]) != `"install-body"` {
		t.Fatalf("expected client_metadata to be preserved, got %s", object["client_metadata"])
	}
	var include []string
	if err := json.Unmarshal(object["include"], &include); err != nil {
		t.Fatalf("decode include: %v", err)
	}
	if strings.Join(include, ",") != "output_text.annotations,reasoning.encrypted_content" {
		t.Fatalf("expected reasoning include to be appended without dropping existing include, got %#v", include)
	}
}

func TestResponsesCreateBodyPromptCachePolicy(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		decision  *commonresponses.PromptCacheDecision
		wantKey   string
		wantField bool
	}{
		{
			name: "client body wins over policy",
			raw:  `{"model":"gpt-5","input":"hi","prompt_cache_key":"pc-client"}`,
			decision: &commonresponses.PromptCacheDecision{
				Key:    "pc-policy",
				Source: commonresponses.PromptCacheRouteHint,
			},
			wantKey:   "pc-client",
			wantField: true,
		},
		{
			name: "policy fills missing body field",
			raw:  `{"model":"gpt-5","input":"hi"}`,
			decision: &commonresponses.PromptCacheDecision{
				Key:    "pc-policy",
				Source: commonresponses.PromptCacheRouteHint,
			},
			wantKey:   "pc-policy",
			wantField: true,
		},
		{
			name:      "missing policy stays absent",
			raw:       `{"model":"gpt-5","input":"hi"}`,
			wantField: false,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			envelope := mustEnvelope(t, tt.raw)
			body, err := PlanResponsesCreateBody(envelope.Object, CreateBodyInput{
				Model:       "gpt-5-codex",
				Stream:      true,
				PromptCache: tt.decision,
			})
			if err != nil {
				t.Fatalf("plan create body: %v", err)
			}
			object := mustDecodeObject(t, body)
			rawKey, ok := object["prompt_cache_key"]
			if ok != tt.wantField {
				t.Fatalf("prompt_cache_key presence mismatch: got %v body=%s", ok, body)
			}
			if tt.wantField && string(rawKey) != `"`+tt.wantKey+`"` {
				t.Fatalf("expected prompt_cache_key %q, got %s body=%s", tt.wantKey, rawKey, body)
			}
		})
	}
}

func TestResponsesCreateBodyRejectsInvalidPromptCacheKey(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		decision *commonresponses.PromptCacheDecision
	}{
		{
			name: "invalid client body key",
			raw:  `{"model":"gpt-5","input":"hi","prompt_cache_key":"bad\nkey"}`,
		},
		{
			name: "non-string client body key",
			raw:  `{"model":"gpt-5","input":"hi","prompt_cache_key":123}`,
		},
		{
			name: "invalid policy key",
			raw:  `{"model":"gpt-5","input":"hi"}`,
			decision: &commonresponses.PromptCacheDecision{
				Key:    "bad\nkey",
				Source: commonresponses.PromptCacheRouteHint,
			},
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			object, parseErr := jsonobject.Parse([]byte(tt.raw))
			if parseErr != nil {
				t.Fatalf("parse raw object: %v", parseErr)
			}
			_, err := PlanResponsesCreateBody(object, CreateBodyInput{
				Model:       "gpt-5-codex",
				Stream:      true,
				PromptCache: tt.decision,
			})
			if err == nil || !strings.Contains(err.Error(), "prompt_cache_key") {
				t.Fatalf("expected prompt_cache_key rejection, got %v", err)
			}
		})
	}
}

func TestResponsesCreateBodyRejectsIncludeStringWhenAppendingReasoning(t *testing.T) {
	envelope := mustEnvelope(t, `{"model":"gpt-5","input":"hi","reasoning":{"effort":"medium"},"include":"output_text.annotations"}`)
	_, err := PlanResponsesCreateBody(envelope.Object, CreateBodyInput{Model: "gpt-5-codex", Stream: true})
	if err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("expected include string to be rejected instead of rewritten, got %v", err)
	}
}

func TestValidUnixMillisStringRejectsOutOfRangeValues(t *testing.T) {
	if !validUnixMillisString("1710000000123") {
		t.Fatal("expected ordinary unix millis string to be valid")
	}
	if validUnixMillisString("99999999999999999999999999999999") {
		t.Fatal("expected out-of-range unix millis string to be rejected")
	}
	if validUnixMillisString("-1") {
		t.Fatal("expected negative unix millis string to be rejected")
	}
}

func TestResolveIdentityFallbacksAndRejectsBadSingletonHeaders(t *testing.T) {
	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers: snapshot(map[string][]string{
			"User-Agent": {""},
			"originator": {""},
			"session-id": {""},
			"thread-id":  {""},
		}),
		Policy: ChannelPolicy{DefaultOriginator: "codex_cli_rs"},
	})
	if err != nil {
		t.Fatalf("expected empty required identity headers to fall back, got %v", err)
	}
	if identity.UserAgent == "" || identity.UserAgent != DefaultUserAgent() {
		t.Fatalf("expected default user agent, got %q", identity.UserAgent)
	}
	if identity.Originator != "codex_cli_rs" {
		t.Fatalf("expected default originator, got %q", identity.Originator)
	}
	if identity.SessionID == "" || identity.ThreadID == "" || identity.ClientRequestID != identity.ThreadID {
		t.Fatalf("expected generated session/thread and client request fallback, got %+v", identity)
	}
	if identity.Sources["session-id"] != SourceGenerated || identity.Sources["thread-id"] != SourceGenerated {
		t.Fatalf("expected generated sources for empty identity fields, got %+v", identity.Sources)
	}

	_, _, err = ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   snapshot(map[string][]string{"session-id": {"bad\nvalue"}}),
	})
	if err == nil || !strings.Contains(err.Error(), "session-id") {
		t.Fatalf("expected invalid non-empty session-id to be rejected, got %v", err)
	}

	_, _, err = ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   snapshot(map[string][]string{"thread-id": {"one", "two"}}),
	})
	if err == nil || !strings.Contains(err.Error(), "thread-id") {
		t.Fatalf("expected multi-value singleton header to be rejected, got %v", err)
	}
}

func TestResponsesCreateOmitsMissingInstallationID(t *testing.T) {
	identity, decisions, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   snapshot(nil),
		Policy: ChannelPolicy{
			DefaultOriginator:           "codex_cli_rs",
			GenerateProxyInstallationID: true,
		},
		Principal: PrincipalFingerprint{Kind: "api_key", HMAC: "principal-hmac"},
		ChannelID: 42,
	})
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	if identity.InstallationID != "" {
		t.Fatalf("expected ordinary create to omit missing installation id, got %q", identity.InstallationID)
	}
	foundOmit := false
	for _, decision := range decisions {
		if decision.Name == "x-codex-installation-id" && decision.Action == "omit" {
			foundOmit = true
			break
		}
	}
	if !foundOmit {
		t.Fatalf("expected installation id omit decision, got %+v", decisions)
	}
}

func TestResponsesCreateInstallationIDHeaderFallback(t *testing.T) {
	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers: snapshot(map[string][]string{
			"x-codex-installation-id": {"install-header"},
		}),
		Policy: ChannelPolicy{DefaultOriginator: "codex_cli_rs"},
	})
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	plan, err := BuildHeaders(HeaderPlanInput{
		Operation:  OpResponsesCreate,
		Credential: Credential{AccessToken: "upstream-token"},
		Policy:     ChannelPolicy{DefaultOriginator: "codex_cli_rs"},
		Identity:   identity,
	})
	if err != nil {
		t.Fatalf("build headers: %v", err)
	}
	if got := plan.HTTPHeader().Get("x-codex-installation-id"); got != "install-header" {
		t.Fatalf("expected create installation id from header fallback, got %q", got)
	}
}

func TestCompactPlannerOmitsClientMetadataAndProjectsInstallationID(t *testing.T) {
	envelope := mustEnvelope(t, `{
		"model":"gpt-5",
		"input":"hello",
		"stream":true,
		"client_metadata":{
			"session_id":"sess-body",
			"thread_id":"thread-body",
			"x-codex-installation-id":"install-body"
		}
	}`)
	body, err := PlanResponsesCompactBody(envelope.Object, envelope.Projection, "gpt-5-codex", nil)
	if err != nil {
		t.Fatalf("plan compact body: %v", err)
	}
	object := mustDecodeObject(t, body)
	for _, forbidden := range []string{"client_metadata", "stream", "store", "include"} {
		if _, ok := object[forbidden]; ok {
			t.Fatalf("expected compact body to omit %s, got %s", forbidden, body)
		}
	}
	if string(object["model"]) != `"gpt-5-codex"` || string(object["input"]) != `"hello"` {
		t.Fatalf("expected compact body to keep compact payload fields, got %s", body)
	}

	metadata, err := MetadataFromResponsesBody(envelope.Object)
	if err != nil {
		t.Fatalf("metadata from body: %v", err)
	}
	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCompact,
		Metadata:  metadata,
		Policy:    ChannelPolicy{DefaultOriginator: "codex_cli_rs", GenerateProxyInstallationID: true},
		Principal: PrincipalFingerprint{Kind: "user", HMAC: "principal-hmac"},
		ChannelID: 11,
	})
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	plan, err := BuildHeaders(HeaderPlanInput{
		Operation:  OpResponsesCompact,
		Credential: Credential{AccessToken: "upstream-token"},
		Policy:     ChannelPolicy{DefaultOriginator: "codex_cli_rs", GenerateProxyInstallationID: true},
		Identity:   identity,
	})
	if err != nil {
		t.Fatalf("build compact headers: %v", err)
	}
	headers := plan.HTTPHeader()
	if got := headers.Get("x-codex-installation-id"); got != "install-body" {
		t.Fatalf("expected compact installation id from body metadata, got %q", got)
	}
	if got := headers.Get("Accept"); got != "application/json" {
		t.Fatalf("expected compact JSON accept header, got %q", got)
	}
	if got := headers.Get("OpenAI-Beta"); got != "" {
		t.Fatalf("expected compact HTTP headers to omit OpenAI-Beta, got %q", got)
	}
}

func TestResolveIdentityRejectsInvalidOptionalMetadata(t *testing.T) {
	cases := []struct {
		name      string
		metadata  string
		wantParam string
	}{
		{
			name:      "parent thread id must be string",
			metadata:  `"x-codex-parent-thread-id":{"bad":true}`,
			wantParam: "x-codex-parent-thread-id",
		},
		{
			name:      "parent thread id rejects control characters",
			metadata:  `"x-codex-parent-thread-id":"bad\nvalue"`,
			wantParam: "x-codex-parent-thread-id",
		},
		{
			name:      "subagent must be string",
			metadata:  `"x-openai-subagent":{"bad":true}`,
			wantParam: "x-openai-subagent",
		},
		{
			name:      "subagent rejects invalid token",
			metadata:  `"x-openai-subagent":"bad value"`,
			wantParam: "x-openai-subagent",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			envelope := mustEnvelope(t, `{"model":"gpt-5","input":"hi","client_metadata":{`+tt.metadata+`}}`)
			metadata, err := MetadataFromResponsesBody(envelope.Object)
			if err != nil {
				t.Fatalf("metadata from body: %v", err)
			}
			_, _, err = ResolveIdentity(IdentityInput{
				Operation: OpResponsesCreate,
				Metadata:  metadata,
				Policy:    ChannelPolicy{DefaultOriginator: "codex_cli_rs"},
			})
			if err == nil || !strings.Contains(err.Error(), tt.wantParam) {
				t.Fatalf("expected %s rejection, got %v", tt.wantParam, err)
			}
		})
	}
}

func TestResponsesCreateBodyRejectsUnsupportedOfficialInputs(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		wantParam string
	}{
		{
			name:      "client metadata not object",
			raw:       `{"model":"gpt-5","input":"hi","client_metadata":"bad"}`,
			wantParam: "client_metadata",
		},
		{
			name:      "client metadata null",
			raw:       `{"model":"gpt-5","input":"hi","client_metadata":null}`,
			wantParam: "client_metadata",
		},
		{
			name:      "temperature and top_p",
			raw:       `{"model":"gpt-5","input":"hi","temperature":0.7,"top_p":0.9}`,
			wantParam: "temperature",
		},
		{
			name:      "context management",
			raw:       `{"model":"gpt-5","input":"hi","context_management":[{"type":"compaction"}]}`,
			wantParam: "context_management",
		},
		{
			name:      "truncation",
			raw:       `{"model":"gpt-5","input":"hi","truncation":"auto"}`,
			wantParam: "truncation",
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			envelope := mustEnvelope(t, tt.raw)
			_, err := PlanResponsesCreateBody(envelope.Object, CreateBodyInput{Model: "gpt-5-codex", Stream: true})
			if err == nil || !strings.Contains(err.Error(), tt.wantParam) {
				t.Fatalf("expected %s rejection, got %v", tt.wantParam, err)
			}
		})
	}

	if _, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-5","model":"gpt-4"}`)); err == nil {
		t.Fatal("expected duplicate top-level key to be rejected before planning")
	}
}

func TestResponsesWSPlannerFiltersHandshakeAndPatchesFrameMetadata(t *testing.T) {
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{
		"type":"response.create",
		"model":"gpt-5",
		"input":"hi",
		"client_metadata":{
			"session_id":"sess-frame",
			"thread_id":"thread-frame",
			"x-codex-turn-state":{"step":1},
			"future":"keep"
		},
		"unknown_number":12345678901234567890
	}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	headers := snapshot(map[string][]string{
		"Content-Type":                          {"application/json"},
		"Accept":                                {"text/event-stream"},
		"Connection":                            {"upgrade"},
		"x-codex-turn-state":                    {`{"header":true}`},
		"x-codex-installation-id":               {"install-header"},
		"traceparent":                           {"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"},
		"x-responsesapi-include-timing-metrics": {"true"},
	})
	metadata, err := MetadataFromResponsesFrame(frame)
	if err != nil {
		t.Fatalf("metadata from frame: %v", err)
	}
	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesWSOpen,
		Headers:   headers,
		Metadata:  metadata,
		Policy:    ChannelPolicy{DefaultOriginator: "codex_cli_rs", GenerateProxyInstallationID: true},
		Principal: PrincipalFingerprint{Kind: "api_key", HMAC: "principal-hmac"},
		ChannelID: 12,
	})
	if err != nil {
		t.Fatalf("resolve identity: %v", err)
	}
	plan, err := BuildHeaders(HeaderPlanInput{
		Operation:  OpResponsesWSOpen,
		Headers:    headers,
		Credential: Credential{AccessToken: "upstream-token", AccountID: "acct-123"},
		Policy:     ChannelPolicy{DefaultOriginator: "codex_cli_rs", GenerateProxyInstallationID: true},
		Identity:   identity,
	})
	if err != nil {
		t.Fatalf("build WS headers: %v", err)
	}
	upstreamHeaders := plan.HTTPHeader()
	if got := upstreamHeaders.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
		t.Fatalf("expected ResponsesWS beta header, got %q", got)
	}
	for _, forbidden := range []string{"Content-Type", "Accept", "Connection", "x-codex-turn-state", "x-codex-installation-id", "traceparent", "tracestate"} {
		if got := upstreamHeaders.Get(forbidden); got != "" {
			t.Fatalf("expected %s to be absent from WS handshake, got %q", forbidden, got)
		}
	}
	if got := upstreamHeaders.Get("x-responsesapi-include-timing-metrics"); got != "true" {
		t.Fatalf("expected allowed WS optional header to be copied, got %q", got)
	}

	encoded, err := PlanResponsesWSFrame(frame, FramePatchInput{
		Identity:      identity,
		Model:         "gpt-5-codex",
		ResponsesLite: true,
		Clock:         fixedClock{now: time.UnixMilli(1710000000123)},
	})
	if err != nil {
		t.Fatalf("plan WS frame: %v", err)
	}
	object := mustDecodeObject(t, encoded)
	if string(object["model"]) != `"gpt-5-codex"` {
		t.Fatalf("expected frame model patch, got %s", object["model"])
	}
	if string(object["unknown_number"]) != `12345678901234567890` {
		t.Fatalf("expected frame unknown field to be preserved, got %s", object["unknown_number"])
	}
	var clientMetadata map[string]json.RawMessage
	if err := json.Unmarshal(object["client_metadata"], &clientMetadata); err != nil {
		t.Fatalf("decode frame client_metadata: %v", err)
	}
	if string(clientMetadata["session_id"]) != `"sess-frame"` || string(clientMetadata["thread_id"]) != `"thread-frame"` {
		t.Fatalf("expected original frame identity metadata to be preserved, got %s", object["client_metadata"])
	}
	if string(clientMetadata["x-codex-turn-state"]) != `{"step":1}` || string(clientMetadata["future"]) != `"keep"` {
		t.Fatalf("expected frame metadata to preserve turn state and unknown keys, got %s", object["client_metadata"])
	}
	if string(clientMetadata["ws_request_header_x_openai_internal_codex_responses_lite"]) != `"true"` {
		t.Fatalf("expected responses-lite metadata stamp, got %s", object["client_metadata"])
	}
	if string(clientMetadata["x-codex-ws-stream-request-start-ms"]) != `"1710000000123"` {
		t.Fatalf("expected deterministic stream-start stamp, got %s", object["client_metadata"])
	}
	if string(clientMetadata["x-codex-installation-id"]) == "" {
		t.Fatalf("expected generated installation id to be patched into frame metadata, got %s", object["client_metadata"])
	}
}

func TestResponsesWSFrameRejectsNullClientMetadata(t *testing.T) {
	frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"hi","client_metadata":null}`))
	if err != nil {
		t.Fatalf("parse frame: %v", err)
	}
	_, err = PlanResponsesWSFrame(frame, FramePatchInput{
		Identity: Identity{SessionID: "sess", ThreadID: "thread"},
		Model:    "gpt-5-codex",
		Clock:    fixedClock{now: time.UnixMilli(1710000000123)},
	})
	if err == nil || !strings.Contains(err.Error(), "client_metadata") {
		t.Fatalf("expected null client_metadata rejection, got %v", err)
	}
}

func TestResponsesWSFrameRejectsInvalidClientMetadata(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "oversized metadata",
			raw:  `{"type":"response.create","model":"gpt-5","input":"hi","client_metadata":{"future":"` + strings.Repeat("x", 64*1024) + `"}}`,
		},
		{
			name: "invalid reserved metadata",
			raw:  `{"type":"response.create","model":"gpt-5","input":"hi","client_metadata":{"session_id":"bad\nvalue"}}`,
		},
		{
			name: "valid reserved metadata mismatches open identity",
			raw:  `{"type":"response.create","model":"gpt-5","input":"hi","client_metadata":{"session_id":"sess-frame-other"}}`,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			frame, err := responsesws.ParseRawResponsesCreateFrame([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parse frame: %v", err)
			}
			_, err = PlanResponsesWSFrame(frame, FramePatchInput{
				Identity: Identity{
					SessionID:      "sess-open",
					ThreadID:       "thread-open",
					WindowID:       "window-open",
					InstallationID: "install-open",
				},
				Model: "gpt-5-codex",
				Clock: fixedClock{now: time.UnixMilli(1710000000123)},
			})
			if err == nil || !strings.Contains(err.Error(), "client_metadata") {
				t.Fatalf("expected client_metadata rejection, got %v", err)
			}
		})
	}
}

func TestResolveIdentityRejectsUntrustedAttestation(t *testing.T) {
	_, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   snapshot(map[string][]string{"x-oai-attestation": {"abc.def"}}),
		Policy:    ChannelPolicy{TrustClientAttestation: false},
	})
	if err == nil || !strings.Contains(err.Error(), "x-oai-attestation") {
		t.Fatalf("expected untrusted client attestation to be rejected, got %v", err)
	}

	identity, _, err := ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers: snapshot(map[string][]string{
			"x-oai-attestation":     {"abc.def"},
			"x-codex-turn-metadata": {`{"turn":"meta"}`},
		}),
		Policy: ChannelPolicy{TrustClientAttestation: true},
	})
	if err != nil {
		t.Fatalf("expected trusted attestation and JSON turn metadata to pass, got %v", err)
	}
	if identity.TurnMetadata != `{"turn":"meta"}` {
		t.Fatalf("expected JSON-object turn metadata, got %q", identity.TurnMetadata)
	}

	_, _, err = ResolveIdentity(IdentityInput{
		Operation: OpResponsesCreate,
		Headers:   snapshot(map[string][]string{"x-codex-turn-metadata": {"not-json"}}),
		Policy:    ChannelPolicy{TrustClientAttestation: true},
	})
	if err == nil || !strings.Contains(err.Error(), "x-codex-turn-metadata") {
		t.Fatalf("expected non-JSON turn metadata to be rejected, got %v", err)
	}
}
