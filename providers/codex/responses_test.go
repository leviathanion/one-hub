package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/common/responsesws"
	"one-api/types"
)

func (p *CodexProvider) CreateResponsesForTest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.CreateResponses(context.Background(), rawReq)
}

func (p *CodexProvider) CreateResponsesStreamForTest(request *types.OpenAIResponsesRequest) (commonresponses.EventStream, *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	return p.CreateResponsesStream(context.Background(), rawReq)
}

func (p *CodexProvider) CompactResponsesForTest(request *types.OpenAIResponsesRequest) (*types.OpenAIResponsesResponses, *types.OpenAIErrorWithStatusCode) {
	rawReq, errWithCode := p.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		return nil, errWithCode
	}
	rawReq.Operation = "responses.compact.http"
	return p.CompactResponses(context.Background(), rawReq)
}

func (p *CodexProvider) rawResponsesRequestForTest(request *types.OpenAIResponsesRequest) (*commonresponses.Request, *types.OpenAIErrorWithStatusCode) {
	if request == nil {
		return nil, common.StringErrorWrapperLocal("request is required", "invalid_request_error", http.StatusBadRequest)
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "marshal_request_failed", http.StatusInternalServerError)
	}
	envelope, err := commonresponses.ParseRawEnvelope(raw)
	if err != nil {
		return nil, common.ErrorWrapperLocal(err, "invalid_request_error", http.StatusBadRequest)
	}
	downstreamDialect := commonresponses.DownstreamResponses
	if request.ConvertChat {
		downstreamDialect = commonresponses.DownstreamChatCompletions
	}
	headers := requestctx.HeaderSnapshot{}
	principal := requestctx.Principal{}
	channelID := 0
	if p != nil {
		if p.Context != nil && p.Context.Request != nil {
			headers = requestctx.NewHeaderSnapshot(p.Context.Request.Header)
			principal = requestctx.PrincipalFromGin(p.Context)
		}
		if p.Channel != nil {
			channelID = p.Channel.Id
		}
	}
	return &commonresponses.Request{
		Operation: commonresponses.ResponsesCreate,
		Headers:   headers,
		Body:      envelope,
		Control: commonresponses.Control{
			DownstreamDialect: downstreamDialect,
			Stream:            request.Stream,
		},
		Principal: principal,
		ChannelID: channelID,
		Model:     request.Model,
	}, nil
}

func TestChatResponsesRequestUsesControlPlaneForChatDialect(t *testing.T) {
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	rawReq, errWithCode := provider.chatResponsesRequestFromTyped(&types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		Stream:      true,
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("chatResponsesRequestFromTyped returned error: %v", errWithCode.Message)
	}
	if rawReq.Control.DownstreamDialect != commonresponses.DownstreamChatCompletions || !rawReq.Control.Stream {
		t.Fatalf("expected chat-completions stream control, got %+v", rawReq.Control)
	}
	if rawReq.Body.Projection.ConvertChat {
		t.Fatal("expected raw JSON projection not to carry ConvertChat")
	}
	rawBody := string(rawReq.Body.Object.Raw)
	if strings.Contains(rawBody, "ConvertChat") || strings.Contains(rawBody, "convert_chat") {
		t.Fatalf("expected raw upstream body not to contain ConvertChat control, got %s", rawBody)
	}
}

type fakeStringStream struct {
	dataChan        chan string
	errChan         chan error
	closed          chan struct{}
	closeOnce       sync.Once
	observeAccepted func(string) error
}

func (s *fakeStringStream) Recv() (<-chan string, <-chan error) {
	return s.dataChan, s.errChan
}

func (s *fakeStringStream) Close() {
	if s == nil || s.closed == nil {
		return
	}
	s.closeOnce.Do(func() {
		close(s.closed)
	})
}

func (s *fakeStringStream) ObserveAcceptedResponsesEvent(event string) error {
	if s == nil || s.observeAccepted == nil {
		return nil
	}
	return s.observeAccepted(event)
}

func TestCodexResponsesWSAdapterTreatsBinaryProviderFrameAsMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	if _, ok := any(adapter).(responsesws.BinaryProviderFrameCapable); ok {
		t.Fatal("expected codex responses websocket adapter not to opt into binary provider frames")
	}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewBinaryFrame([]byte("binary")))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected binary provider frame to become provider_malformed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterTreatsMalformedTextProviderFrameAsMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected malformed text provider frame to become provider_malformed, got %+v", result)
	}
	if payload := responsesws.ClientPayloadFromError(result.Err); len(payload) != 0 {
		t.Fatalf("expected provider malformed error not to carry client payload, got %q", payload)
	}
}

func TestCodexResponsesWSAdapterMissingProviderFailsClosed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.created"}`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || result.Err == nil || !result.CloseTransport {
		t.Fatalf("expected missing provider to fail closed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterFutureProviderEventShapePassesThrough(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	payload := []byte(`{"type":"response.future","event_id":"evt_future","response":"opaque","future":{"enabled":true}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
		t.Fatalf("expected future provider event to pass through, got %+v", result)
	}
	if string(result.EmitFrame.Payload()) != string(payload) || result.Usage != nil || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected future provider event to pass through byte-identically without usage, got %+v payload=%s", result, result.EmitFrame.Payload())
	}
}

func TestCodexResponsesWSAdapterKnownTerminalBadResponseShapeClosesAsProviderMalformed(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":"opaque"}`)))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || !errors.Is(result.Err, responsesws.ErrInvalidProviderEventPayload) || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected bad known terminal shape to become provider_malformed, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterRejectsConflictingImageIdentityBeforeEmit(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5", accumulator: newCodexTurnUsageAccumulator()}
	payload := []byte(`{"type":"response.output_item.done","item_id":"img_top","output_index":0,"item":{"id":"img_item","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected conflicting image identity to close before emit, got %+v", result)
	}
	var providerErr *types.OpenAIErrorWithStatusCode
	if !errors.As(result.Err, &providerErr) || providerErr.Code != "provider_protocol_error" || providerErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected conflicting image identity error: %#v", result.Err)
	}
	if result.Usage != nil {
		t.Fatalf("conflicting image frame produced usage: %+v", result.Usage)
	}
}

func TestCodexResponsesWSAdapterRejectsTerminalToolOverflowWithoutPartialFrameBilling(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5", accumulator: newCodexTurnUsageAccumulator()}
	prefix := []byte(`{"type":"response.output_item.done","item_id":"ws_prefix","output_index":0,"item":{"id":"ws_prefix","type":"web_search_call","status":"completed","action":{"type":"search"}}}`)
	prefixResult := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(prefix))
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if prefixResult.Err != nil || prefixResult.EmitFrame == nil || prefixResult.Usage == nil || prefixResult.Usage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("expected accepted prefix billing, got %+v", prefixResult)
	}

	outputs := make([]types.ResponsesOutput, 0, 1025)
	outputs = append(outputs, types.ResponsesOutput{ID: "ws_prefix", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}})
	for index := 0; index < 1024; index++ {
		outputs = append(outputs, types.ResponsesOutput{ID: fmt.Sprintf("ws_terminal_%d", index), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}})
	}
	payload, err := json.Marshal(types.OpenAIResponsesStreamResponses{
		Type: "response.completed", Response: &types.OpenAIResponsesResponses{ID: "resp_tool_overflow", Status: "completed", Output: outputs},
	})
	if err != nil {
		t.Fatalf("marshal terminal fixture: %v", err)
	}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Origin != responsesws.RecvDetailOriginProviderMalformed || !result.CloseTransport || result.EmitFrame != nil {
		t.Fatalf("expected terminal overflow to close before emit, got %+v", result)
	}
	var providerErr *types.OpenAIErrorWithStatusCode
	if !errors.As(result.Err, &providerErr) || providerErr.Code != "provider_usage_state_limit" {
		t.Fatalf("unexpected terminal overflow error: %#v", result.Err)
	}
	if result.Usage != nil {
		t.Fatalf("failed terminal re-emitted or partially added billing: %+v", result.Usage)
	}
}

func TestCodexResponsesWSAdapterRejectsDuplicateKeyClientCancel(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	_, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","type":"response.cancel"}`)))
	if err == nil || !strings.Contains(err.Error(), "invalid_event") {
		t.Fatalf("expected duplicate-key client event to become invalid_event, got %v", err)
	}
}

func TestCodexResponsesWSAdapterPreservesOnlyClientPreviousResponseID(t *testing.T) {
	tests := []struct {
		name                   string
		payload                string
		wantPrevious           string
		wantPreviousResponseID bool
	}{
		{
			name:                   "does not inject relay continuation",
			payload:                `{"type":"response.create","model":"gpt-5","input":"hi"}`,
			wantPreviousResponseID: false,
		},
		{
			name:                   "preserves explicit client previous response",
			payload:                `{"type":"response.create","model":"gpt-5","previous_response_id":"resp_client","input":"hi"}`,
			wantPrevious:           "resp_client",
			wantPreviousResponseID: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &codexResponsesWSAdapter{
				provider: &CodexProvider{},
				model:    "gpt-5",
			}

			frame, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(tc.payload)))
			if err != nil {
				t.Fatalf("prepare response.create: %v", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(frame.Payload(), &object); err != nil {
				t.Fatalf("decode prepared frame: %v", err)
			}
			rawPrevious, exists := object["previous_response_id"]
			if exists != tc.wantPreviousResponseID {
				t.Fatalf("previous_response_id presence mismatch: got %v want %v payload=%s", exists, tc.wantPreviousResponseID, frame.Payload())
			}
			if !tc.wantPreviousResponseID {
				return
			}
			var gotPrevious string
			if err := json.Unmarshal(rawPrevious, &gotPrevious); err != nil {
				t.Fatalf("decode previous_response_id: %v", err)
			}
			if gotPrevious != tc.wantPrevious {
				t.Fatalf("previous_response_id mismatch: got %q want %q payload=%s", gotPrevious, tc.wantPrevious, frame.Payload())
			}
		})
	}
}

func TestCodexResponsesWSAdapterTracksEachTurnModelForUsageFallback(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider: &CodexProvider{},
		model:    "gpt-5",
	}

	for _, test := range []struct {
		clientModel string
		wantModel   string
	}{
		{clientModel: "gpt-5", wantModel: "gpt-5"},
		{clientModel: "gpt-5-mini", wantModel: "gpt-5-mini"},
		{clientModel: "gpt-5.6-terra", wantModel: "gpt-5.6-terra"},
	} {
		payload := []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":"hi"}`, test.clientModel))
		frame, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame(payload))
		if err != nil {
			t.Fatalf("prepare response.create for %q: %v", test.clientModel, err)
		}
		adapter.mu.Lock()
		got := adapter.turnModel
		adapter.mu.Unlock()
		if got != test.wantModel {
			t.Fatalf("expected usage fallback model %q for current turn, got %q", test.wantModel, got)
		}
		var encoded struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(frame.Payload(), &encoded); err != nil || encoded.Model != test.wantModel {
			t.Fatalf("expected upstream frame model %q, got model=%q err=%v payload=%s", test.wantModel, encoded.Model, err, frame.Payload())
		}
	}
}

func TestCodexResponsesWSAdapterFiltersOpaqueSessionCreatedBootstrap(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"session.created","session":"opaque"}`)))
	if !result.Filtered || result.Err != nil || result.CloseTransport || result.EmitFrame != nil || result.Usage != nil || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected opaque session.created bootstrap to be filtered, got %+v", result)
	}
}

func TestCodexResponsesWSAdapterTerminalTextProviderFrameExtractsUsage(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider:    &CodexProvider{},
		model:       "gpt-5",
		turnModel:   "gpt-5.6-terra",
		accumulator: newCodexTurnUsageAccumulator(),
	}
	payload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_done","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
		t.Fatalf("expected terminal provider frame to pass through, got %+v", result)
	}
	if string(result.EmitFrame.Payload()) != string(payload) || result.Origin != responsesws.RecvDetailOriginProviderFrame {
		t.Fatalf("expected terminal provider frame to pass through byte-identically, got %+v payload=%s", result, result.EmitFrame.Payload())
	}
	if result.Usage == nil || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 5 || result.Usage.TotalTokens != 8 {
		t.Fatalf("expected terminal usage to be extracted, got %+v", result.Usage)
	}
	if adapter.lastResponse != "resp_done" || adapter.accumulator != nil || adapter.turnModel != "" {
		t.Fatalf("expected terminal frame to update response state and clear turn state, last=%q model=%q accumulator=%+v", adapter.lastResponse, adapter.turnModel, adapter.accumulator)
	}
}

func TestCodexResponsesWSAdapterNormalizesResponseDoneAtProviderBoundary(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider:    &CodexProvider{},
		model:       "gpt-5",
		turnModel:   "gpt-5",
		accumulator: newCodexTurnUsageAccumulator(),
	}
	// Mirrors the Codex websocket tool-call lifecycle: the supplier closes one
	// response turn with response.done before the client sends the next create.
	payload := []byte(`{"type":"response.done","event_id":"evt_done","response":{"id":"resp_tool","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"output":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]},"provider_extension":{"preserved":true}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
		t.Fatalf("expected Codex response.done to normalize and pass through, got %+v", result)
	}
	classified := responsesws.ClassifyResponsesWSEvent(result.EmitFrame.Payload())
	if classified.Malformed || classified.Kind != responsesws.ResponsesSuccessTerminal || classified.EventType != "response.completed" || !classified.HasSequenceNumber || classified.SequenceNumber != 0 {
		t.Fatalf("expected a valid normalized Responses terminal, classified=%+v payload=%s", classified, result.EmitFrame.Payload())
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(result.EmitFrame.Payload(), &object); err != nil {
		t.Fatalf("decode normalized response.done: %v", err)
	}
	if _, ok := object["provider_extension"]; !ok {
		t.Fatalf("expected unknown provider fields to survive normalization, payload=%s", result.EmitFrame.Payload())
	}
	if result.Usage == nil || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 5 || result.Usage.TotalTokens != 8 {
		t.Fatalf("expected response.done usage evidence to survive normalization, got %+v", result.Usage)
	}
	if adapter.lastResponse != "resp_tool" || adapter.accumulator != nil || adapter.turnModel != "" {
		t.Fatalf("expected normalized response.done to finalize adapter turn state, adapter=%+v", adapter)
	}
}

func TestCodexResponsesWSAdapterNormalizesPrivateTerminalAliasesBeforePublicClassification(t *testing.T) {
	for _, test := range []struct {
		name           string
		supplierType   string
		supplierStatus string
		publicType     string
		publicStatus   string
		kind           responsesws.ResponsesTerminalKind
	}{
		{name: "completed", supplierType: "response.completed", supplierStatus: "completed", publicType: "response.completed", publicStatus: "completed", kind: responsesws.ResponsesSuccessTerminal},
		{name: "failed", supplierType: "response.failed", supplierStatus: "failed", publicType: "response.failed", publicStatus: "failed", kind: responsesws.ResponsesFailedTerminal},
		{name: "incomplete", supplierType: "response.incomplete", supplierStatus: "incomplete", publicType: "response.incomplete", publicStatus: "incomplete", kind: responsesws.ResponsesFailedTerminal},
		{name: "terminal status fallback", supplierType: "response.updated", supplierStatus: "completed", publicType: "response.completed", publicStatus: "completed", kind: responsesws.ResponsesSuccessTerminal},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := &codexResponsesWSAdapter{
				provider:    &CodexProvider{},
				model:       "gpt-5",
				turnModel:   "gpt-5",
				accumulator: newCodexTurnUsageAccumulator(),
			}
			payload := []byte(fmt.Sprintf(`{"type":%q,"response":{"id":"resp_alias","status":%q,"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}},"provider_extension":{"preserved":true}}`, test.supplierType, test.supplierStatus))
			result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
			if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
				t.Fatalf("expected supplier terminal alias to normalize, got %+v", result)
			}
			classified := responsesws.ClassifyResponsesWSEvent(result.EmitFrame.Payload())
			if classified.Malformed || classified.Kind != test.kind || classified.EventType != test.publicType || !classified.HasSequenceNumber || classified.SequenceNumber != 0 || classified.Response == nil {
				t.Fatalf("expected public terminal %q, classified=%+v payload=%s", test.publicType, classified, result.EmitFrame.Payload())
			}
			if classified.Response.Status != test.publicStatus || classified.Response.ID != "resp_alias" {
				t.Fatalf("expected response status %q and identity, got %+v", test.publicStatus, classified.Response)
			}
			if result.Usage == nil || result.Usage.TotalTokens != 4 {
				t.Fatalf("expected supplier terminal usage evidence, got %+v", result.Usage)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(result.EmitFrame.Payload(), &object); err != nil || len(object["provider_extension"]) == 0 {
				t.Fatalf("expected unknown supplier fields to survive normalization, err=%v payload=%s", err, result.EmitFrame.Payload())
			}
		})
	}
}

func TestCodexResponsesWSAdapterBackfillsSparseResponseDoneFromCreated(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider:    &CodexProvider{},
		model:       "gpt-5",
		turnModel:   "gpt-5",
		accumulator: newCodexTurnUsageAccumulator(),
	}
	created := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.created","response":{"id":"resp_sparse","status":"in_progress"}}`)))
	if created.Err != nil || created.EmitFrame == nil || adapter.lastResponse != "resp_sparse" {
		t.Fatalf("expected response.created to establish the turn response ID, result=%+v adapter=%+v", created, adapter)
	}
	done := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.done","response":{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`)))
	if done.Err != nil || done.CloseTransport || done.EmitFrame == nil {
		t.Fatalf("expected sparse response.done to normalize, got %+v", done)
	}
	classified := responsesws.ClassifyResponsesWSEvent(done.EmitFrame.Payload())
	if classified.Malformed || classified.Kind != responsesws.ResponsesSuccessTerminal || classified.EventType != "response.completed" || classified.Response == nil {
		t.Fatalf("expected completed Responses terminal, classified=%+v payload=%s", classified, done.EmitFrame.Payload())
	}
	if classified.Response.ID != "resp_sparse" || classified.Response.Status != types.ResponseStatusCompleted || classified.Response.Usage == nil || classified.Response.Usage.TotalTokens != 15 {
		t.Fatalf("expected ID, completed status, and usage to survive normalization, response=%+v", classified.Response)
	}
	if adapter.accumulator != nil || adapter.turnModel != "" {
		t.Fatalf("expected sparse done to finish only the current turn, adapter=%+v", adapter)
	}
	nextFrame, err := adapter.PrepareClientFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.create","model":"gpt-5","input":"next turn"}`)))
	if err != nil || nextFrame.PayloadLen() == 0 || adapter.accumulator == nil || adapter.lastResponse != "" {
		t.Fatalf("expected the same websocket adapter to accept the next turn, frame=%s err=%v adapter=%+v", nextFrame.Payload(), err, adapter)
	}
}

func TestCodexResponsesWSAdapterRejectsSparseResponseDoneWithoutTurnID(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5", accumulator: newCodexTurnUsageAccumulator()}
	done := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.done","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)))
	if done.Err == nil || !done.CloseTransport || done.EmitFrame != nil {
		t.Fatalf("expected response.done without any turn ID to fail closed, got %+v", done)
	}
	if done.Usage == nil || done.Usage.TotalTokens != 2 {
		t.Fatalf("expected billing evidence to survive malformed terminal handling, got %+v", done.Usage)
	}
}

func TestCodexResponsesWSAdapterMapsResponseDoneStatus(t *testing.T) {
	for _, test := range []struct {
		status    string
		eventType string
		kind      responsesws.ResponsesTerminalKind
	}{
		{status: "failed", eventType: "response.failed", kind: responsesws.ResponsesFailedTerminal},
		{status: "incomplete", eventType: "response.incomplete", kind: responsesws.ResponsesFailedTerminal},
	} {
		t.Run(test.status, func(t *testing.T) {
			adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5", turnModel: "gpt-5", accumulator: newCodexTurnUsageAccumulator()}
			responseID := "resp_" + test.status
			created := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, responseID))))
			if created.Err != nil || created.EmitFrame == nil {
				t.Fatalf("expected response.created before sparse done, got %+v", created)
			}
			payload := []byte(fmt.Sprintf(`{"type":"response.done","response":{"status":%q,"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`, test.status))
			result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
			if result.Err != nil || result.CloseTransport || result.Filtered || result.EmitFrame == nil {
				t.Fatalf("expected response.done status mapping, got %+v", result)
			}
			classified := responsesws.ClassifyResponsesWSEvent(result.EmitFrame.Payload())
			if classified.Malformed || classified.Kind != test.kind || classified.EventType != test.eventType || classified.Response == nil || classified.Response.ID != responseID {
				t.Fatalf("expected %s, classified=%+v payload=%s", test.eventType, classified, result.EmitFrame.Payload())
			}
		})
	}
}

func TestCodexResponsesWSAdapterRejectsUnrepresentableCancelledResponseDoneAndKeepsUsage(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5", turnModel: "gpt-5", accumulator: newCodexTurnUsageAccumulator()}
	payload := []byte(`{"type":"response.done","event_id":"evt_cancelled","response":{"id":"resp_cancelled","status":"cancelled","usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`)
	result := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame(payload))
	if result.Err == nil || !result.CloseTransport || result.EmitFrame != nil || result.Filtered {
		t.Fatalf("expected cancelled response.done to fail the adapter boundary explicitly, got %+v", result)
	}
	if result.Usage == nil || result.Usage.TotalTokens != 4 {
		t.Fatalf("expected billing evidence to survive explicit failure, got %+v", result.Usage)
	}
	if !errors.Is(result.Err, responsesws.ErrInvalidProviderEventPayload) {
		t.Fatalf("expected invalid provider event classification, got %v", result.Err)
	}
}

func TestCodexResponsesWSAdapterSynthesizesResponseDoneSequenceAfterProviderFrames(t *testing.T) {
	adapter := &codexResponsesWSAdapter{provider: &CodexProvider{}, model: "gpt-5"}
	progress := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.in_progress","sequence_number":7,"response":{"id":"resp_seq","status":"in_progress"}}`)))
	if progress.Err != nil || progress.EmitFrame == nil {
		t.Fatalf("expected sequenced progress frame, got %+v", progress)
	}
	done := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.done","response":{"id":"resp_seq","status":"completed"}}`)))
	if done.Err != nil || done.EmitFrame == nil {
		t.Fatalf("expected response.done after progress frame, got %+v", done)
	}
	classified := responsesws.ClassifyResponsesWSEvent(done.EmitFrame.Payload())
	if classified.Malformed || classified.Kind != responsesws.ResponsesSuccessTerminal || classified.SequenceNumber != 8 {
		t.Fatalf("expected synthesized sequence 8, classified=%+v payload=%s", classified, done.EmitFrame.Payload())
	}
}

func TestCodexResponsesWSAdapterFiltersDuplicateTerminalDialects(t *testing.T) {
	adapter := &codexResponsesWSAdapter{
		provider:    &CodexProvider{},
		model:       "gpt-5",
		turnModel:   "gpt-5",
		accumulator: newCodexTurnUsageAccumulator(),
	}
	completed := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)))
	if completed.Err != nil || completed.EmitFrame == nil || completed.Filtered {
		t.Fatalf("expected first terminal to pass through, got %+v", completed)
	}
	done := adapter.HandleProviderFrame(context.Background(), responsesws.NewTextFrame([]byte(`{"type":"response.done","response":{"id":"resp_duplicate","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}`)))
	if done.Err != nil || !done.Filtered || done.EmitFrame != nil || done.Usage != nil || done.CloseTransport {
		t.Fatalf("expected duplicate Codex terminal dialect to be filtered, got %+v", done)
	}
}

func TestCollectResponsesStreamResponseAcceptsDataWithoutSpace(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	if resp == nil || resp.ID != "resp_1" {
		t.Fatalf("unexpected response: %#v", resp)
	}

	if provider.Usage.TotalTokens != 0 {
		t.Fatalf("expected stream collector not to finalize provider usage directly, got %d", provider.Usage.TotalTokens)
	}
}

func TestCollectResponsesStreamResponseClosedErrChannelFinishes(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_closed_err\",\"status\":\"completed\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	done := make(chan *types.OpenAIErrorWithStatusCode, 1)
	go func() {
		_, errWithCode := provider.collectResponsesStreamResponse(stream)
		done <- errWithCode
	}()

	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()
	select {
	case errWithCode := <-done:
		if errWithCode != nil {
			t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
		}
	case <-timeout.C:
		t.Fatal("collectResponsesStreamResponse did not finish after err channel closed")
	}
}

func TestCollectResponsesStreamResponseRejectsMalformedAcceptedFrame(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.completed\ndata:{\"type\":\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || errWithCode.Code != "stream_decode_failed" || !errWithCode.UpstreamAccepted {
		t.Fatalf("expected malformed accepted frame to fail closed without replay, got %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseDecodeFailureDrainsBlockedLegacyProducer(t *testing.T) {
	provider := &CodexProvider{}
	producerFinished := make(chan struct{})
	stream, apiErr := requester.RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("trigger\n")),
	}, func(_ *[]byte, dataChan chan string, _ chan error) {
		dataChan <- "data: {\"type\":\n\n"
		dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"late\"}}\n\n"
		close(producerFinished)
	})
	if apiErr != nil {
		t.Fatalf("create legacy stream: %+v", apiErr)
	}

	_, collectedErr := provider.collectResponsesStreamResponse(commonresponses.NewEventStream(stream, commonresponses.IgnoreAcceptedResponsesEvent))
	if collectedErr == nil || collectedErr.Code != "stream_decode_failed" {
		t.Fatalf("expected malformed stream error, got %+v", collectedErr)
	}
	select {
	case <-producerFinished:
	case <-time.After(time.Second):
		t.Fatal("legacy producer remained blocked after collector decode failure")
	}
}

func TestCollectResponsesStreamResponseMarksMissingTerminalAccepted(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.created\ndata:{\"type\":\"response.created\"}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || errWithCode.Code != "no_response" || !errWithCode.UpstreamAccepted {
		t.Fatalf("expected accepted stream without terminal to fail without replay, got %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseTreatsPreIdentityErrorAsRejected(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":429,\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"param\":\"requests\",\"sequence_number\":0}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	response, errWithCode := provider.collectResponsesStreamResponse(stream)
	if response != nil || errWithCode == nil {
		t.Fatalf("expected provider rejection, response=%+v error=%+v", response, errWithCode)
	}
	if errWithCode.UpstreamAccepted || errWithCode.UpstreamAmbiguous || errWithCode.LocalError {
		t.Fatalf("expected pre-identity error to remain a definitive provider rejection, got %+v", errWithCode)
	}
	if errWithCode.StatusCode != http.StatusTooManyRequests || errWithCode.Code != "rate_limit_exceeded" || errWithCode.Message != "slow down" || errWithCode.Param != "requests" {
		t.Fatalf("unexpected mapped provider error: %+v", errWithCode)
	}
	if !errWithCode.ProviderRateLimited {
		t.Fatalf("expected rate-limit classification, got %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseTreatsPostIdentityErrorAsAccepted(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 2),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_started\",\"status\":\"in_progress\"}}\n\n"
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":500,\"code\":\"provider_failed\",\"message\":\"generation failed\",\"sequence_number\":1}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	response, errWithCode := provider.collectResponsesStreamResponse(stream)
	if response != nil || errWithCode == nil || !errWithCode.UpstreamAccepted {
		t.Fatalf("expected post-identity error to retain accepted evidence, response=%+v error=%+v", response, errWithCode)
	}
	if errWithCode.StatusCode != http.StatusInternalServerError || errWithCode.Code != "provider_failed" {
		t.Fatalf("unexpected mapped provider error: %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseMapsStatuslessErrorsByEvidence(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		wantStatus int
	}{
		{
			name:       "server error defaults to bad gateway",
			payload:    "{\"type\":\"error\",\"code\":\"server_error\",\"message\":\"provider failed\",\"sequence_number\":0}",
			wantStatus: http.StatusBadGateway,
		},
		{
			name:       "rate limit maps to too many requests",
			payload:    "{\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"sequence_number\":0}",
			wantStatus: http.StatusTooManyRequests,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := &CodexProvider{}
			stream := &fakeStringStream{
				dataChan: make(chan string, 1),
				errChan:  make(chan error),
			}
			stream.dataChan <- "event: error\ndata: " + test.payload + "\n\n"
			close(stream.dataChan)
			close(stream.errChan)

			_, errWithCode := provider.collectResponsesStreamResponse(stream)
			if errWithCode == nil || errWithCode.StatusCode != test.wantStatus || errWithCode.UpstreamAccepted {
				t.Fatalf("unexpected mapped provider error: %+v", errWithCode)
			}
		})
	}
}

func TestCollectResponsesStreamResponseHidesProviderAccountError(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":401,\"sequence_number\":0,\"error\":{\"type\":\"authentication_error\",\"code\":\"account_deactivated\",\"message\":\"organization org-secret has been deactivated\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || !errWithCode.ProviderAuthRejected || errWithCode.UpstreamAccepted {
		t.Fatalf("unexpected mapped account error: %+v", errWithCode)
	}
	if errWithCode.Code != "provider_account_error" || strings.Contains(errWithCode.Message, "org-secret") || strings.Contains(errWithCode.Message, "deactivated") {
		t.Fatalf("provider account detail leaked: %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseHidesStatusOnlyProviderAccountError(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":401,\"sequence_number\":0,\"code\":\"unauthorized\",\"message\":\"organization org-secret has been deactivated\"}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || !errWithCode.ProviderAuthRejected || errWithCode.Code != "provider_account_error" {
		t.Fatalf("unexpected mapped account error: %+v", errWithCode)
	}
	if strings.Contains(errWithCode.Message, "org-secret") || strings.Contains(errWithCode.Message, "deactivated") {
		t.Fatalf("provider account detail leaked: %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponsePreservesRequestLevelForbidden(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":403,\"sequence_number\":0,\"error\":{\"type\":\"invalid_request_error\",\"code\":\"tool_forbidden\",\"message\":\"tool is not allowed\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || errWithCode.StatusCode != http.StatusForbidden || errWithCode.ProviderAuthRejected || errWithCode.Code != "tool_forbidden" || errWithCode.Message != "tool is not allowed" {
		t.Fatalf("request-level forbidden was misclassified: %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseStillHidesStructuredForbiddenAuth(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"status\":403,\"sequence_number\":0,\"error\":{\"type\":\"authentication_error\",\"code\":\"invalid_api_key\",\"message\":\"organization org-secret rejected the credential\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || !errWithCode.ProviderAuthRejected || errWithCode.Code != "provider_account_error" {
		t.Fatalf("structured forbidden auth was not classified: %+v", errWithCode)
	}
	if strings.Contains(errWithCode.Message, "org-secret") || strings.Contains(errWithCode.Message, "credential") {
		t.Fatalf("structured forbidden auth detail leaked: %+v", errWithCode)
	}
}

func TestCollectResponsesStreamResponseRejectsRealtimeDoneAsTerminal(t *testing.T) {
	provider := &CodexProvider{}
	stream := &fakeStringStream{
		dataChan: make(chan string, 1),
		errChan:  make(chan error),
	}
	stream.dataChan <- "event: response.done\ndata:{\"type\":\"response.done\",\"response\":{\"id\":\"resp_realtime\",\"status\":\"completed\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode == nil || errWithCode.Code != "no_response" || !errWithCode.UpstreamAccepted {
		t.Fatalf("expected Realtime terminal dialect not to terminate a Responses stream, got %+v", errWithCode)
	}
}

func TestCodexAcceptedResponsesEventRejectsOversizedImageIdentityAtomically(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})
	itemID := strings.Repeat("i", 257)
	event := fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":%q,\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"high\",\"size\":\"1024x1024\"}}\n\n", itemID)
	gotErr := handler.ObserveAcceptedResponsesEvent(event)
	var providerErr *types.OpenAIErrorWithStatusCode
	if !errors.As(gotErr, &providerErr) || providerErr.StatusCode != http.StatusBadGateway || providerErr.Code != "provider_usage_state_limit" {
		t.Fatalf("unexpected Codex image identity overflow error: %#v", gotErr)
	}
	if handler.accumulator.imageTracker.PartialImageCount(&types.ResponsesOutput{ID: itemID}, nil) != 0 {
		t.Fatal("oversized image identity changed Codex image state")
	}
}

func TestCodexAcceptedResponsesEventRejectsTerminalToolOverflowAtomically(t *testing.T) {
	prefix := types.ResponsesOutput{ID: "ws_prefix", Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	outputs := make([]types.ResponsesOutput, 0, 1025)
	outputs = append(outputs, prefix)
	for index := 0; index < 1024; index++ {
		outputs = append(outputs, types.ResponsesOutput{ID: fmt.Sprintf("ws_terminal_%d", index), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}})
	}
	terminal, err := json.Marshal(types.OpenAIResponsesStreamResponses{
		Type: "response.completed", Response: &types.OpenAIResponsesResponses{ID: "resp_tool_overflow", Status: "completed", Output: outputs},
	})
	if err != nil {
		t.Fatalf("marshal terminal fixture: %v", err)
	}
	prefixPayload, err := json.Marshal(types.OpenAIResponsesStreamResponses{Type: "response.output_item.done", Item: &prefix})
	if err != nil {
		t.Fatalf("marshal prefix fixture: %v", err)
	}
	handler := newCodexResponsesStreamHandler(&types.Usage{})
	if err := handler.ObserveAcceptedResponsesEvent("data: " + string(prefixPayload) + "\n\n"); err != nil {
		t.Fatalf("accepted prefix failed: %v", err)
	}
	streamErr := handler.ObserveAcceptedResponsesEvent("data: " + string(terminal) + "\n\n")
	var providerErr *types.OpenAIErrorWithStatusCode
	if !errors.As(streamErr, &providerErr) || providerErr.Code != "provider_usage_state_limit" || providerErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("unexpected terminal tool overflow error: %#v", streamErr)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
	if handler.Usage.ExtraBilling[key].CallCount != 1 || !handler.Usage.ProviderExtraBilling[key] {
		t.Fatalf("failed terminal changed or deauthorized prefix billing: %+v", handler.Usage)
	}
}

func TestCreateResponsesPreservesTerminalToolOverflowClassification(t *testing.T) {
	outputs := make([]types.ResponsesOutput, 1025)
	for index := range outputs {
		outputs[index] = types.ResponsesOutput{ID: fmt.Sprintf("ws_%d", index), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	}
	terminal, err := json.Marshal(types.OpenAIResponsesStreamResponses{
		Type: "response.completed", Response: &types.OpenAIResponsesResponses{ID: "resp_tool_overflow", Status: "completed", Output: outputs},
	})
	if err != nil {
		t.Fatalf("marshal terminal fixture: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: %s\n\n", terminal)
	}))
	defer server.Close()
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL

	response, apiErr := provider.CreateResponsesForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"})
	if response != nil || apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted {
		t.Fatalf("expected accepted terminal tool overflow, response=%+v error=%+v", response, apiErr)
	}
	if provider.Usage != nil && len(provider.Usage.ExtraBilling) != 0 {
		t.Fatalf("failed terminal partially committed billing: %+v", provider.Usage.ExtraBilling)
	}
}

func TestCollectResponsesStreamResponsePreservesEmptyReasoningSummary(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"id\":\"rs_1\",\"status\":\"completed\",\"summary\":[]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"
		stream.errChan <- io.EOF
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("collectResponsesStreamResponse returned error: %v", errWithCode.Message)
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("unexpected marshal error: %v", err)
	}

	if !strings.Contains(string(data), "\"summary\":[]") {
		t.Fatalf("expected marshaled response to preserve empty summary array, got %s", string(data))
	}
}

func TestCollectResponsesStreamResponseAcceptsWrappedEOF(t *testing.T) {
	provider := &CodexProvider{}
	provider.Usage = &types.Usage{}

	stream := &fakeStringStream{
		dataChan: make(chan string),
		errChan:  make(chan error),
	}

	go func() {
		stream.dataChan <- "event: response.completed\ndata:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_wrapped_eof\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"
		stream.errChan <- fmt.Errorf("wrapped: %w", io.EOF)
	}()

	resp, errWithCode := provider.collectResponsesStreamResponse(stream)
	if errWithCode != nil {
		t.Fatalf("expected wrapped EOF to terminate stream cleanly, got %v", errWithCode.Message)
	}
	if resp == nil || resp.ID != "resp_wrapped_eof" {
		t.Fatalf("expected wrapped EOF response, got %#v", resp)
	}
}

func TestCompactResponsesUsesCompactEndpoint(t *testing.T) {
	var (
		gotPath   string
		gotAccept string
		bodyBytes []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccept = r.Header.Get("Accept")

		var err error
		bodyBytes, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read request body: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_1","object":"response.compaction","output":[{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model:   "gpt-5",
		Input:   "hello",
		Stream:  true,
		Include: []string{"reasoning.encrypted_content", "custom.include"},
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if gotPath != "/backend-api/codex/responses/compact" {
		t.Fatalf("expected compact endpoint path, got %q", gotPath)
	}
	if gotAccept != "application/json" {
		t.Fatalf("expected JSON accept header, got %q", gotAccept)
	}

	var raw map[string]any
	if err := json.Unmarshal(bodyBytes, &raw); err != nil {
		t.Fatalf("failed to decode upstream request body: %v", err)
	}
	if _, exists := raw["stream"]; exists {
		t.Fatalf("expected compact request body to omit stream, got %s", string(bodyBytes))
	}
	if _, exists := raw["context_management"]; exists {
		t.Fatalf("expected compact request body to omit context_management, got %s", string(bodyBytes))
	}
	if _, exists := raw["truncation"]; exists {
		t.Fatalf("expected compact request body to omit truncation, got %s", string(bodyBytes))
	}
	if _, exists := raw["include"]; exists {
		t.Fatalf("expected compact request body to omit include, got %s", string(bodyBytes))
	}
	if _, exists := raw["store"]; exists {
		t.Fatalf("expected compact request body to omit store, got %s", string(bodyBytes))
	}

	if resp.Object != "response.compaction" {
		t.Fatalf("expected compaction object, got %q", resp.Object)
	}
	if len(resp.Output) != 1 || resp.Output[0].Type != "compaction" {
		t.Fatalf("expected compaction output item, got %#v", resp.Output)
	}
	if resp.Output[0].EncryptedContent == nil || *resp.Output[0].EncryptedContent != "ciphertext" {
		t.Fatalf("expected encrypted content to be preserved, got %#v", resp.Output[0].EncryptedContent)
	}
	if provider.Usage.TotalTokens != 18 {
		t.Fatalf("expected provider usage to be updated, got %d", provider.Usage.TotalTokens)
	}
}

func TestCompactResponsesObservesTerminalImageBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_compact_image","status":"completed","tools":[{"type":"image_generation","model":"gpt-image-2","quality":"auto","size":"auto"}],"output":[{"id":"img_1","type":"image_generation_call","status":"completed","quality":"high","size":"1024x1024"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL

	if _, apiErr := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"}); apiErr != nil {
		t.Fatalf("CompactResponses returned error: %v", apiErr.Message)
	}
	key := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|1024x1024|0")
	if provider.Usage.ExtraBilling[key].CallCount != 1 || !provider.Usage.ProviderExtraBilling[key] {
		t.Fatalf("compact terminal image billing was not observed: %+v", provider.Usage)
	}
}

func TestCompactResponsesRejectsTerminalToolOverflowAsAccepted(t *testing.T) {
	outputs := make([]types.ResponsesOutput, 1025)
	for index := range outputs {
		outputs[index] = types.ResponsesOutput{ID: fmt.Sprintf("ws_%d", index), Type: types.InputTypeWebSearchCall, Status: "completed", Action: map[string]any{"type": "search"}}
	}
	response := &types.OpenAIResponsesResponses{ID: "resp_compact_tool_overflow", Status: "completed", Output: outputs, Usage: &types.ResponsesUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Errorf("encode compact response: %v", err)
		}
	}))
	defer server.Close()
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL

	result, apiErr := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", Input: "hello"})
	if result != nil || apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted {
		t.Fatalf("expected accepted compact terminal overflow, result=%+v error=%+v", result, apiErr)
	}
	if len(provider.Usage.ExtraBilling) != 0 {
		t.Fatalf("failed compact terminal partially committed billing: %+v", provider.Usage.ExtraBilling)
	}
}

func TestCompactResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_2","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func TestCompactResponsesDoesNotBackfillUsageFromRetainedOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_cmp_3","object":"response.compaction","output":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"retained context that should not be billed as completion"}]},{"id":"cmp_1","type":"compaction","encrypted_content":"ciphertext"}]}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{
		PromptTokens:     99,
		CompletionTokens: 77,
		TotalTokens:      176,
	}

	resp, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil {
		t.Fatalf("expected usage to be initialized")
	}
	if resp.Usage.InputTokens != 99 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 99 {
		t.Fatalf("expected missing compact usage to preserve prompt tokens without output backfill, got %#v", resp.Usage)
	}
	if provider.Usage.PromptTokens != 99 || provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != 99 {
		t.Fatalf("expected provider usage to preserve prompt tokens without output backfill, got %#v", provider.Usage)
	}
}

func TestCompactResponsesPreservesDetailedUsageAndExtraBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_cmp_4",
			"object":"response.compaction",
			"tools":[{"type":"web_search_preview","search_context_size":"high"}],
			"output":[{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}],
			"usage":{
				"input_tokens":11,
				"output_tokens":7,
				"total_tokens":18,
				"input_tokens_details":{"cached_tokens":4,"text_tokens":2,"image_tokens":3},
				"output_tokens_details":{"reasoning_tokens":5}
			}
		}`))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	_, errWithCode := provider.CompactResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: "hello",
	})
	if errWithCode != nil {
		t.Fatalf("CompactResponses returned error: %v", errWithCode.Message)
	}

	if provider.Usage.PromptTokensDetails.CachedTokens != 4 || provider.Usage.PromptTokensDetails.TextTokens != 2 || provider.Usage.PromptTokensDetails.ImageTokens != 3 {
		t.Fatalf("expected compact usage details to be preserved, got %#v", provider.Usage.PromptTokensDetails)
	}
	if provider.Usage.CompletionTokensDetails.ReasoningTokens != 5 {
		t.Fatalf("expected compact reasoning usage to be preserved, got %#v", provider.Usage.CompletionTokensDetails)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected compact responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCreateResponsesDoesNotSynthesizeUsageAndPreservesExtraBilling(t *testing.T) {
	originalDisable := config.DisableTokenEncoders
	config.DisableTokenEncoders = true
	t.Cleanup(func() {
		config.DisableTokenEncoders = originalDisable
	})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_text.delta\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_text.delta\",\"delta\":\"hello from codex\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_4\",\"object\":\"response\",\"status\":\"completed\",\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	resp, errWithCode := provider.CreateResponsesForTest(&types.OpenAIResponsesRequest{
		Model: "gpt-3.5-turbo",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.Usage == nil || resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 0 || resp.Usage.TotalTokens != 11 || resp.Usage.ProviderReported {
		t.Fatalf("provider-missing response content produced token usage, got %#v", resp.Usage)
	}
	if provider.Usage.CompletionTokens != 0 || provider.Usage.TotalTokens != provider.Usage.PromptTokens || provider.Usage.ProviderReported {
		t.Fatalf("provider-missing response content produced authoritative usage, got %#v", provider.Usage)
	}
	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected create responses to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
}

func TestCodexResponsesStreamHandlerAccumulatesToolBillingAndTerminalUsage(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"},{"type":"image_generation","model":"gpt-image-2","quality":"auto","size":"auto","partial_images":2}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)
	searchDone := []byte(`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}}}`)
	handler.HandlerResponsesStream(&searchDone, dataChan, errChan)

	imageAdded := []byte(`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"image_generation_call","id":"img_1","status":"in_progress"}}`)
	handler.HandlerResponsesStream(&imageAdded, dataChan, errChan)
	partialImage := []byte(`data: {"type":"response.image_generation_call.partial_image","item_id":"img_1","output_index":2,"partial_image_index":0,"partial_image_b64":"preview"}`)
	handler.HandlerResponsesStream(&partialImage, dataChan, errChan)
	imageDone := []byte(`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"image_generation_call","id":"img_1","status":"completed","quality":"high","size":"2048x2048"}}`)
	handler.HandlerResponsesStream(&imageDone, dataChan, errChan)
	completed := []byte(`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)

	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected a single high web search charge, got %+v", billing)
	}
	imageKey := types.BuildExtraBillingKey(types.APIToolTypeImageGeneration, "gpt-image-2|high|2048x2048|1")
	if imageBilling := handler.Usage.ExtraBilling[imageKey]; imageBilling.CallCount != 1 {
		t.Fatalf("expected image billing only after successful response terminal, got %+v", handler.Usage.ExtraBilling)
	}
}

func TestCodexResponsesStreamHandlerDoesNotDoubleCountTerminalToolBilling(t *testing.T) {
	handler := newCodexResponsesStreamHandler(&types.Usage{})

	dataChan := make(chan string, 8)
	errChan := make(chan error, 1)

	created := []byte(`data: {"type":"response.created","response":{"tools":[{"type":"web_search_preview","search_context_size":"high"}]}}`)
	handler.HandlerResponsesStream(&created, dataChan, errChan)

	added := []byte(`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search"}}}`)
	handler.HandlerResponsesStream(&added, dataChan, errChan)

	completed := []byte(`data: {"type":"response.completed","sequence_number":1,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8},"tools":[{"type":"web_search_preview","search_context_size":"high"}],"output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search"}}]}}`)
	handler.HandlerResponsesStream(&completed, dataChan, errChan)

	billing, ok := handler.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected terminal stream handler to preserve tool extra billing, got %+v", handler.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected terminal stream handler to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesStreamConvertChatDoesNotDoubleCountToolBilling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"web_search_call\",\"id\":\"ws_1\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream_chat\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8},\"tools\":[{\"type\":\"web_search_preview\",\"search_context_size\":\"high\"}],\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello from codex\"}]},{\"id\":\"ws_1\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 11}

	stream, errWithCode := provider.CreateResponsesStreamForTest(&types.OpenAIResponsesRequest{
		Model:       "gpt-5",
		ConvertChat: true,
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponsesStream returned error: %v", errWithCode.Message)
	}
	defer stream.Close()

	dataChan, errChan := stream.Recv()
	receivedChunks := 0
	timeout := time.NewTimer(500 * time.Millisecond)
	defer timeout.Stop()
	for receivedChunks < 2 {
		select {
		case _, ok := <-dataChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			receivedChunks++
		case err, ok := <-errChan:
			if !ok {
				receivedChunks = 2
				continue
			}
			if err != nil && err != io.EOF {
				t.Fatalf("unexpected stream error: %v", err)
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for convert-chat stream output")
		}
	}

	billing, ok := provider.Usage.ExtraBilling[types.APIToolTypeWebSearchPreview]
	if !ok {
		t.Fatalf("expected convert-chat stream to preserve tool extra billing, got %+v", provider.Usage.ExtraBilling)
	}
	if billing.Type != "high" || billing.CallCount != 1 {
		t.Fatalf("expected convert-chat stream to charge web search once, got %+v", billing)
	}
}

func TestCreateResponsesStreamConvertChatRejectsImageTrackerErrorBeforeConversion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.done\",\"item_id\":\"img_top\",\"output_index\":0,\"item\":{\"id\":\"img_item\",\"type\":\"image_generation_call\",\"status\":\"completed\",\"quality\":\"high\",\"size\":\"1024x1024\"}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{}

	stream, apiErr := provider.CreateResponsesStreamForTest(&types.OpenAIResponsesRequest{Model: "gpt-5", ConvertChat: true, Input: "hello"})
	if apiErr != nil {
		t.Fatalf("CreateResponsesStream returned error: %v", apiErr.Message)
	}
	defer stream.Close()
	dataChan, errChan := stream.Recv()
	dataCount := 0
	errorCount := 0
	deadline := time.After(5 * time.Second)
	for dataChan != nil || errChan != nil {
		select {
		case _, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			dataCount++
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if errors.Is(err, io.EOF) {
				continue
			}
			errorCount++
			var providerErr *types.OpenAIErrorWithStatusCode
			if !errors.As(err, &providerErr) || providerErr.Code != "provider_protocol_error" {
				t.Fatalf("unexpected convert-chat tracker error: %#v", err)
			}
		case <-deadline:
			t.Fatal("convert-chat stream did not terminate after tracker error")
		}
	}
	if dataCount != 0 || errorCount != 1 {
		t.Fatalf("converted stream delivered data=%d errors=%d, want no offending chunk and one error", dataCount, errorCount)
	}
}

func TestCreateChatCompletionStreamReusesCustomToolCallConverter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.created\",\"response\":{\"id\":\"resp_custom\",\"model\":\"gpt-5.6-terra\",\"service_tier\":\"flex\",\"status\":\"in_progress\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.output_item.added\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"id\":\"ctc_1\",\"status\":\"in_progress\",\"call_id\":\"call_1\",\"name\":\"shell\",\"input\":\"\"}}\n\n"))
		_, _ = w.Write([]byte("event: response.custom_tool_call_input.delta\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.custom_tool_call_input.delta\",\"output_index\":0,\"item_id\":\"ctc_1\",\"delta\":\"echo ok\"}\n\n"))
		_, _ = w.Write([]byte("event: response.custom_tool_call_input.done\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.custom_tool_call_input.done\",\"output_index\":0,\"item_id\":\"ctc_1\",\"input\":\"echo ok\"}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_custom\",\"model\":\"gpt-5.6-terra\",\"service_tier\":\"flex\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8},\"output\":[{\"type\":\"custom_tool_call\",\"id\":\"ctc_1\",\"status\":\"completed\",\"call_id\":\"call_1\",\"name\":\"shell\",\"input\":\"echo ok\"}]}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, "", nil)
	provider.Channel.BaseURL = &server.URL
	provider.Usage = &types.Usage{PromptTokens: 3}
	stream, errWithCode := provider.CreateChatCompletionStream(&types.ChatCompletionRequest{
		Model: "gpt-5.6-terra",
		Messages: []types.ChatCompletionMessage{{
			Role:    types.ChatMessageRoleUser,
			Content: "run a command",
		}},
	})
	if errWithCode != nil {
		t.Fatalf("CreateChatCompletionStream returned error: %v", errWithCode.Message)
	}
	defer stream.Close()

	dataChan, errChan := stream.Recv()
	chunks := make([]types.ChatCompletionStreamResponse, 0, 4)
	timeout := time.NewTimer(time.Second)
	defer timeout.Stop()
	for dataChan != nil || errChan != nil {
		select {
		case data, ok := <-dataChan:
			if !ok {
				dataChan = nil
				continue
			}
			var chunk types.ChatCompletionStreamResponse
			if err := json.Unmarshal([]byte(data), &chunk); err != nil {
				t.Fatalf("decode converted Chat chunk %q: %v", data, err)
			}
			chunks = append(chunks, chunk)
		case err, ok := <-errChan:
			if !ok {
				errChan = nil
				continue
			}
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatalf("unexpected stream error: %v", err)
			}
		case <-timeout.C:
			t.Fatal("timed out waiting for converted custom tool stream")
		}
	}

	if len(chunks) != 4 {
		t.Fatalf("expected created, custom header, custom delta and terminal chunks, got %#v", chunks)
	}
	header := chunks[1].Choices[0].Delta.ToolCalls[0]
	if header.Type != types.ToolChoiceTypeCustom || header.Id != "call_1" || header.Custom == nil || header.Custom.Name != "shell" || header.Function != nil {
		t.Fatalf("unexpected Codex custom tool header: %#v", header)
	}
	delta := chunks[2].Choices[0].Delta.ToolCalls[0]
	if delta.Custom == nil || delta.Custom.Input != "echo ok" || delta.Function != nil {
		t.Fatalf("unexpected Codex custom tool delta: %#v", delta)
	}
	if chunks[3].Model != "gpt-5.6-terra" || chunks[3].ServiceTier != "flex" {
		t.Fatalf("expected actual model/tier on terminal chunk, got model=%q tier=%q", chunks[3].Model, chunks[3].ServiceTier)
	}
	finishReason, ok := chunks[3].Choices[0].FinishReason.(string)
	if !ok || finishReason != types.FinishReasonToolCalls {
		t.Fatalf("expected finish_reason=%q, got %#v", types.FinishReasonToolCalls, chunks[3].Choices[0].FinishReason)
	}
}

func TestCreateResponsesBackfillsPromptCacheKeyFromRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_3\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	key := `{"access_token":"access-token","account_id":"acct-123"}`
	provider := newTestCodexProviderWithContext(t, key, "", nil)
	provider.Channel.BaseURL = &server.URL

	resp, errWithCode := provider.CreateResponsesForTest(&types.OpenAIResponsesRequest{
		Model:          "gpt-5",
		PromptCacheKey: "stable-cache-key",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	})
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if resp.PromptCacheKey != "stable-cache-key" {
		t.Fatalf("expected response prompt_cache_key to be backfilled, got %q", resp.PromptCacheKey)
	}
}

func TestCreateResponsesBodyIncludesPromptCachePolicyDecision(t *testing.T) {
	var upstreamBody map[string]json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if err := json.Unmarshal(body, &upstreamBody); err != nil {
			t.Fatalf("decode upstream body: %v body=%s", err, body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\n"))
		_, _ = w.Write([]byte("data:{\"type\":\"response.completed\",\"response\":{\"id\":\"resp_prompt_policy\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"total_tokens\":8}}}\n\n"))
	}))
	defer server.Close()

	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
	})

	provider := newTestCodexProviderWithContext(t, `{"access_token":"access-token","account_id":"acct-123"}`, `{"prompt_cache_key_strategy":"user_id"}`, nil)
	provider.Context.Set("id", int64(7))
	provider.Channel.BaseURL = &server.URL

	request := &types.OpenAIResponsesRequest{
		Model: "gpt-5",
		Input: []types.InputResponses{
			{
				Type: types.InputTypeMessage,
				Role: types.ChatMessageRoleUser,
				Content: []types.ContentResponses{
					{Type: types.ContentTypeInputText, Text: "hello"},
				},
			},
		},
	}
	rawReq, errWithCode := provider.rawResponsesRequestForTest(request)
	if errWithCode != nil {
		t.Fatalf("rawResponsesRequestForTest returned error: %v", errWithCode.Message)
	}
	expectedKey := promptCacheKeyForRequestStrategy(&types.OpenAIResponsesRequest{Model: "gpt-5"}, provider.Context, codexPromptCacheStrategyUserID)
	rawReq.Policy.PromptCache = &commonresponses.PromptCacheDecision{
		Key:    expectedKey,
		Source: commonresponses.PromptCacheRouteHint,
	}
	resp, errWithCode := provider.CreateResponses(context.Background(), rawReq)
	if errWithCode != nil {
		t.Fatalf("CreateResponses returned error: %v", errWithCode.Message)
	}

	if string(upstreamBody["prompt_cache_key"]) != `"`+expectedKey+`"` {
		t.Fatalf("expected upstream prompt_cache_key %q, got %s body=%#v", expectedKey, upstreamBody["prompt_cache_key"], upstreamBody)
	}
	if resp.PromptCacheKey != expectedKey {
		t.Fatalf("expected response prompt_cache_key backfill from policy, got %q want %q", resp.PromptCacheKey, expectedKey)
	}
}

func (h *CodexResponsesStreamHandler) HandlerResponsesStream(rawLine *[]byte, dataChan chan string, errChan chan error) {
	if err := h.ObserveAcceptedResponsesEvent(string(*rawLine)); err != nil {
		*rawLine = requester.StreamClosed
		errChan <- err
		return
	}
	dataChan <- string(*rawLine)
}
