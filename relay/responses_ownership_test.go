package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/providers/openai"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

func TestResponsesLifecycleIOContextIsBounded(t *testing.T) {
	originalTimeout := responsesLifecycleIOTimeout
	responsesLifecycleIOTimeout = 10 * time.Millisecond
	t.Cleanup(func() { responsesLifecycleIOTimeout = originalTimeout })

	ctx, cancel := boundedResponsesLifecycleContext(context.Background())
	defer cancel()
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("bounded lifecycle context error = %v", ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle I/O context did not honor its deadline")
	}
}

func responsesOwnerTestContext(userID, tokenID int) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("id", userID)
	ctx.Set("token_id", tokenID)
	ctx.Set("channel_type", config.ChannelTypeOpenAI)
	return ctx, recorder
}

func TestPrepareResponsesContinuationOwnershipEnforcesUserAndChannel(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	owner, err := model.NewResponseOwner("resp_owned", 11, 21, 31, time.Now())
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	ctx, _ := responsesOwnerTestContext(11, 21)
	route, err := prepareResponsesContinuationOwnership(ctx, &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID})
	if err != nil || !route.Strict || route.ChannelID != 31 || explicitChannelPinID(ctx) != 31 {
		t.Fatalf("expected strict owner channel 31, route=%+v pin=%d err=%v", route, explicitChannelPinID(ctx), err)
	}
	pinnedMismatch, _ := responsesOwnerTestContext(11, 21)
	pinnedMismatch.Set("specific_channel_id", owner.ChannelID+1)
	_, err = prepareResponsesContinuationOwnership(pinnedMismatch, &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID})
	apiErr := responsesOwnershipAPIError(err)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected owner channel mismatch to fail uniformly, got %+v", apiErr)
	}

	otherUser, _ := responsesOwnerTestContext(12, 22)
	_, err = prepareResponsesContinuationOwnership(otherUser, &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID})
	apiErr = responsesOwnershipAPIError(err)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected cross-user lookup to fail uniformly, got %+v", apiErr)
	}

	store := false
	storeFalse, _ := responsesOwnerTestContext(11, 21)
	route, err = prepareResponsesContinuationOwnership(storeFalse, &types.OpenAIResponsesRequest{
		Store:              &store,
		PreviousResponseID: owner.ResponseID,
	})
	if err != nil || !route.Strict || route.ChannelID != owner.ChannelID || explicitChannelPinID(storeFalse) != owner.ChannelID {
		t.Fatalf("expected durable owner to remain strict for store=false continuation, route=%+v pin=%d err=%v", route, explicitChannelPinID(storeFalse), err)
	}

	missing, _ := responsesOwnerTestContext(11, 21)
	_, err = prepareResponsesContinuationOwnership(missing, &types.OpenAIResponsesRequest{PreviousResponseID: "resp_missing"})
	if apiErr = responsesOwnershipAPIError(err); apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected missing owner to use uniform previous-response error, got %+v", apiErr)
	}

	if err := model.TombstoneResponseOwner(nil, owner.ResponseID, owner.UserID); err != nil {
		t.Fatalf("tombstone response owner: %v", err)
	}
	deleted, _ := responsesOwnerTestContext(11, 21)
	_, err = prepareResponsesContinuationOwnership(deleted, &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID})
	if apiErr = responsesOwnershipAPIError(err); apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected tombstoned owner to use uniform previous-response error, got %+v", apiErr)
	}
}

func TestResponsesContinuationUsesDisabledOrSoftDeletedOwnerIncarnation(t *testing.T) {
	for _, test := range []struct {
		name       string
		disable    bool
		softDelete bool
	}{
		{name: "disabled", disable: true},
		{name: "soft-deleted", softDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			setupRelayTestDB(t, &model.Channel{}, &model.ResponseOwner{})
			proxy := ""
			channel := &model.Channel{Id: 31, Type: config.ChannelTypeOpenAI, Name: "owner", Key: "sk-owner", Status: config.ChannelStatusEnabled, Models: "gpt-5", Proxy: &proxy, Other: `{}`}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			owner, err := model.NewResponseOwner("resp_owner_lifecycle", 11, 21, channel.Id, time.Now(), "channel-type:1", "provider-wide")
			if err != nil {
				t.Fatalf("new response owner: %v", err)
			}
			if err := model.CreateResponseOwner(context.Background(), owner); err != nil {
				t.Fatalf("create response owner: %v", err)
			}
			if test.disable {
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.softDelete {
				if err := model.DB.Delete(&model.Channel{}, channel.Id).Error; err != nil {
					t.Fatal(err)
				}
			}

			ctx, _ := responsesOwnerTestContext(11, 21)
			route, err := prepareResponsesContinuationOwnership(ctx, &types.OpenAIResponsesRequest{Model: "gpt-5", PreviousResponseID: owner.ResponseID})
			if err != nil || !route.Strict || responseOwnerChannelID(ctx) != channel.Id {
				t.Fatalf("prepare durable continuation: route=%+v err=%v", route, err)
			}
			selected, err := fetchChannel(ctx, "gpt-5")
			if err != nil || selected.Id != channel.Id || selected.Key != "sk-owner" {
				t.Fatalf("durable continuation must retain owner incarnation: channel=%+v err=%v", selected, err)
			}
		})
	}
}

func TestStoreFalseContinuationUsesOnlyUserScopedEphemeralProof(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	store := false
	ctx, _ := responsesOwnerTestContext(41, 51)
	recordResponsesEphemeralProof(ctx, "resp_ephemeral", 61)

	sameUser, _ := responsesOwnerTestContext(41, 99)
	route, err := prepareResponsesContinuationOwnership(sameUser, &types.OpenAIResponsesRequest{Store: &store, PreviousResponseID: "resp_ephemeral"})
	if err != nil || route.Strict || route.ChannelID != 61 || currentPreferredChannelID(sameUser) != 61 || explicitChannelPinID(sameUser) != 0 {
		t.Fatalf("expected soft user-scoped proof, route=%+v preferred=%d pin=%d err=%v", route, currentPreferredChannelID(sameUser), explicitChannelPinID(sameUser), err)
	}

	defaultStore, _ := responsesOwnerTestContext(41, 100)
	route, err = prepareResponsesContinuationOwnership(defaultStore, &types.OpenAIResponsesRequest{PreviousResponseID: "resp_ephemeral"})
	if err != nil || route.Strict || route.ChannelID != 61 || currentPreferredChannelID(defaultStore) != 61 {
		t.Fatalf("new response store choice invalidated prior ephemeral proof, route=%+v preferred=%d err=%v", route, currentPreferredChannelID(defaultStore), err)
	}

	otherUser, _ := responsesOwnerTestContext(42, 51)
	_, err = prepareResponsesContinuationOwnership(otherUser, &types.OpenAIResponsesRequest{Store: &store, PreviousResponseID: "resp_ephemeral"})
	if apiErr := responsesOwnershipAPIError(err); apiErr == nil || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected proof to reject another user, got %+v", apiErr)
	}
}

func TestClearResponsesEphemeralProofRequiresMatchingOwnerChannel(t *testing.T) {
	ctx, _ := responsesOwnerTestContext(41, 51)
	recordResponsesEphemeralProof(ctx, "resp_ephemeral_clear", 61)

	clearResponsesEphemeralProof(ctx, "resp_ephemeral_clear", 62)
	if channelID, ok := lookupResponsesEphemeralProof(ctx, "resp_ephemeral_clear"); !ok || channelID != 61 {
		t.Fatalf("another channel must not clear the proof, channel=%d ok=%v", channelID, ok)
	}

	clearResponsesEphemeralProof(ctx, "resp_ephemeral_clear", 61)
	if channelID, ok := lookupResponsesEphemeralProof(ctx, "resp_ephemeral_clear"); ok || channelID != 0 {
		t.Fatalf("matching owner channel must clear the proof, channel=%d ok=%v", channelID, ok)
	}
}

func TestResponsesInputTokensEnforcesContinuationOwnershipWithoutAffinity(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	owner, err := model.NewResponseOwner("resp_input_tokens_owned", 71, 72, 73, time.Now())
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	owned, _ := responsesOwnerTestContext(owner.UserID, owner.TokenID)
	owned.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"gpt-5","previous_response_id":"resp_input_tokens_owned"}`))
	relay := NewRelayResponses(owned)
	if err := relay.setRequest(); err != nil {
		t.Fatalf("set owned input_tokens request: %v", err)
	}
	if !relay.strictOwnerRoute || explicitChannelPinID(owned) != owner.ChannelID {
		t.Fatalf("expected strict owner route on channel %d, strict=%t pin=%d", owner.ChannelID, relay.strictOwnerRoute, explicitChannelPinID(owned))
	}
	if currentChannelAffinityState(owned) != nil {
		t.Fatalf("input_tokens must not prepare ordinary affinity, got %#v", currentChannelAffinityState(owned))
	}
	if !relayShouldSkipRetryAfterAffinityFailure(relay) {
		t.Fatal("strict input_tokens continuation must not retry another channel")
	}

	otherUser, _ := responsesOwnerTestContext(owner.UserID+1, owner.TokenID+1)
	otherUser.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(`{"model":"gpt-5","previous_response_id":"resp_input_tokens_owned"}`))
	otherRelay := NewRelayResponses(otherUser)
	err = otherRelay.setRequest()
	apiErr := responsesOwnershipAPIError(err)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
		t.Fatalf("expected cross-user input_tokens continuation to fail before provider work, got %+v", apiErr)
	}
}

func TestAdministratorPinnedUnknownOwnerUsesOnlyTheExplicitChannel(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, _ := responsesOwnerTestContext(11, 21)
	ctx.Set("specific_channel_id", 31)

	route, err := prepareResponsesContinuationOwnership(ctx, &types.OpenAIResponsesRequest{PreviousResponseID: "resp_missing"})
	if err != nil || !route.Strict || route.ChannelID != 31 {
		t.Fatalf("expected administrator-pinned unknown owner to stay on its explicit channel, route=%+v err=%v", route, err)
	}

	store := false
	route, err = prepareResponsesContinuationOwnership(ctx, &types.OpenAIResponsesRequest{Store: &store, PreviousResponseID: "resp_ephemeral"})
	if err != nil || route.Strict || route.ChannelID != 31 {
		t.Fatalf("expected explicit pin to remain a valid store=false route, route=%+v err=%v", route, err)
	}
}

func TestResponsesWSDurableOwnerBindsEveryTurnToItsChannel(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	owner, err := model.NewResponseOwner("resp_ws_owned", 11, 21, 31, time.Now())
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ctx, _ := responsesOwnerTestContext(11, 21)
	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: ctx,
		Request: &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID},
	})
	if err != nil || !candidate.StrictOwnerRoute || candidate.OwnershipChannelID != owner.ChannelID {
		t.Fatalf("expected authoritative WS continuation route, candidate=%+v err=%v", candidate, err)
	}
	if err := responsesAffinityOwnerConflict(candidate, owner.ChannelID); err != nil {
		t.Fatalf("expected owner channel to pass, got %v", err)
	}
	if err := responsesAffinityOwnerConflict(candidate, owner.ChannelID+1); err == nil {
		t.Fatal("expected a WS connected to another channel to reject the continuation")
	}
}

func TestResponsesContinuationOwnerStoreFailureReturns503(t *testing.T) {
	originalDB := model.DB
	model.DB = nil
	t.Cleanup(func() { model.DB = originalDB })
	ctx, _ := responsesOwnerTestContext(11, 21)

	_, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: ctx,
		Request: &types.OpenAIResponsesRequest{PreviousResponseID: "resp_unknown"},
	})
	apiErr := responsesOwnershipAPIError(err)
	if apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable || apiErr.Code != "responses_owner_store_unavailable" {
		t.Fatalf("expected owner store failure to stop continuation with 503, got %+v", apiErr)
	}
}

func TestResponsesDurableOwnerOverridesStaleSoftAffinity(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	owner, err := model.NewResponseOwner("resp_durable_owner", 11, 21, 73, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	ctx, _ := responsesOwnerTestContext(11, 21)
	ctx.Set(channelAffinityStateContextKey, &channelAffinityState{
		Hit:                true,
		PreferredChannelID: 99,
	})

	candidate, err := PrepareResponsesTurnAffinity(ResponsesAffinityInput{
		Context: ctx,
		Request: &types.OpenAIResponsesRequest{PreviousResponseID: owner.ResponseID},
	})
	if err != nil {
		t.Fatalf("durable owner must override soft affinity: %v", err)
	}
	if !candidate.StrictOwnerRoute || candidate.OwnershipChannelID != owner.ChannelID {
		t.Fatalf("unexpected durable route: %+v", candidate)
	}
	if err := responsesAffinityOwnerConflict(candidate, owner.ChannelID); err != nil {
		t.Fatalf("soft affinity must not conflict with durable owner: %v", err)
	}
}

func TestResponsesWSSoftPromptAffinityIsNotAConnectionOwner(t *testing.T) {
	candidate := &ResponsesTurnAffinity{
		State: &channelAffinityState{
			Hit:                true,
			PreferredChannelID: 91,
		},
	}
	if err := responsesAffinityOwnerConflict(candidate, 92); err != nil {
		t.Fatalf("soft affinity must remain a hint after the websocket is fixed to a channel: %v", err)
	}

	candidate.OwnershipChannelID = 91
	if err := responsesAffinityOwnerConflict(candidate, 92); err == nil {
		t.Fatal("durable continuation owner mismatch must still fail closed")
	}
}

func TestStoredResponsesUnaryPersistsOwnerBeforeBody(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(71, 72)
	request := types.OpenAIResponsesRequest{Model: "gpt-5"}
	provider := &affinityResponsesProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 73, Type: config.ChannelTypeOpenAI},
	}}
	relay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	apiErr, done := relay.send()
	if apiErr != nil || done {
		t.Fatalf("expected stored unary success, done=%t err=%v", done, apiErr)
	}
	owner, err := model.GetResponseOwner(ctx.Request.Context(), "resp_123", ctx.GetInt("id"))
	if err != nil || owner.UserID != 71 || owner.TokenID != 72 || owner.ChannelID != 73 {
		t.Fatalf("expected durable owner, owner=%+v err=%v", owner, err)
	}
	if !strings.Contains(recorder.Body.String(), `"id":"resp_123"`) {
		t.Fatalf("expected response after owner commit, got %q", recorder.Body.String())
	}
}

func TestStoredResponsesUnaryOwnerFailureIsUpstreamAccepted(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(0, 0)
	request := types.OpenAIResponsesRequest{Model: "gpt-5"}
	provider := &affinityResponsesProvider{BaseProvider: providersBase.BaseProvider{
		Channel: &model.Channel{Id: 73, Type: config.ChannelTypeOpenAI},
	}}
	relay := &relayResponses{
		relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"}, responsesRequest: request,
		rawEnvelope: responsesTestRawEnvelope(t, request), operation: responsesOperationCreate,
	}

	apiErr, done := relay.send()
	if apiErr == nil || !done || !apiErr.UpstreamAccepted || apiErr.Code != "responses_owner_persist_failed" {
		t.Fatalf("expected post-create owner failure to retain the accepted marker, done=%t err=%+v", done, apiErr)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("expected response id to remain behind the owner barrier, got %q", recorder.Body.String())
	}
}

func TestPersistStoredResponseOwnerIgnoresClientCancellation(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, _ := responsesOwnerTestContext(71, 72)
	requestCtx, cancel := context.WithCancel(ctx.Request.Context())
	ctx.Request = ctx.Request.WithContext(requestCtx)
	cancel()

	if apiErr := persistStoredResponseOwner(ctx, "resp_detached_owner", 73); apiErr != nil {
		t.Fatalf("expected detached owner commit after provider acceptance, got %+v", apiErr)
	}
	owner, err := model.GetResponseOwner(context.Background(), "resp_detached_owner", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 73 {
		t.Fatalf("expected canceled client not to abort owner persistence, owner=%+v err=%v", owner, err)
	}
}

func TestStoredResponsesStreamBuffersUntilOwnerCommit(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(81, 82)
	request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true}
	stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error, 1)}
	go func() {
		stream.dataChan <- "event: response.created\n"
		stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_stream_owner\",\"model\":\"gpt-5\",\"status\":\"in_progress\"}}\n\n"
		stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_stream_owner\",\"model\":\"gpt-5\",\"status\":\"completed\"}}\n\n"
		stream.errChan <- io.EOF
	}()
	provider := &streamAffinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 83, Type: config.ChannelTypeOpenAI}},
		stream:       stream,
	}
	relay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	apiErr, done := relay.send()
	if apiErr != nil || done {
		t.Fatalf("expected stored stream success, done=%t err=%v", done, apiErr)
	}
	owner, err := model.GetResponseOwner(ctx.Request.Context(), "resp_stream_owner", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 83 {
		t.Fatalf("expected streamed owner, owner=%+v err=%v", owner, err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "response.created") || !strings.Contains(body, "response.completed") {
		t.Fatalf("expected ordered buffered stream, got %q", body)
	}
}

func TestStoredResponsesStreamRejectsResponseIDChangeAfterOwnerBarrier(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(81, 82)
	stream := &fakeRelayStream{dataChan: make(chan string, 3), errChan: make(chan error, 1)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_owner_a\",\"status\":\"in_progress\"}}\n\n"
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_owner_b\",\"status\":\"completed\"}}\n"
	stream.dataChan <- "\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 83)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected changed response id to fail closed, got %+v", apiErr)
	}
	if body := recorder.Body.String(); !strings.Contains(body, "resp_owner_a") || strings.Contains(body, "resp_owner_b") || !strings.Contains(body, `"code":"invalid_provider_response"`) || strings.Contains(body, `"sequence_number":2`) {
		t.Fatalf("mismatched terminal crossed owner delivery barrier: %q", body)
	}
	owner, err := model.GetResponseOwner(context.Background(), "resp_owner_a", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 83 {
		t.Fatalf("first response id owner must remain durable: owner=%+v err=%v", owner, err)
	}
	if _, err := model.GetResponseOwner(context.Background(), "resp_owner_b", ctx.GetInt("id")); !errors.Is(err, model.ErrResponseOwnerNotFound) {
		t.Fatalf("mismatched response id must not get an owner, got %v", err)
	}
}

func TestResponsesNativeStreamStopsAfterLifecycleError(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(1, 2)
	stream := &fakeRelayStream{dataChan: make(chan string, 4), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_native_a\",\"status\":\"in_progress\"}}\n\n"
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_native_b\",\"status\":\"completed\"}}\n"
	stream.dataChan <- "\n"
	stream.dataChan <- "data: {\"type\":\"response.output_text.done\",\"sequence_number\":2,\"text\":\"late-after-error\"}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, observer)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway || openAIErrorCodeString(apiErr.Code, "") != "invalid_provider_response" {
		t.Fatalf("expected lifecycle failure, got %+v", apiErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "resp_native_a") || strings.Contains(body, "resp_native_b") || strings.Contains(body, "late-after-error") {
		t.Fatalf("native lifecycle cut delivered the wrong provider frames: %q", body)
	}
	if strings.Count(body, "event: error") != 1 {
		t.Fatalf("native lifecycle error was not rendered exactly once: %q", body)
	}
	if strings.Contains(body, `"sequence_number":2`) {
		t.Fatalf("synthetic error reused a sequence after rejected identity: %q", body)
	}
}

func TestStoredResponsesStreamDoesNotExposeIDWhenOwnerCommitFails(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(0, 0)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_hidden\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)
	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 91)
	if apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected owner commit failure, got %+v", apiErr)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("response id escaped before owner commit: %q", recorder.Body.String())
	}
}

func TestStoredResponsesOwnerFailureDrainsBlockedLegacyProducer(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(0, 0)
	secondSendStarted := make(chan struct{})
	producerFinished := make(chan struct{})
	stream, constructionErr := requester.RequestNoTrimStream[string](nil, &http.Response{
		Body: io.NopCloser(strings.NewReader("trigger\n")),
	}, func(_ *[]byte, dataChan chan string, _ chan error) {
		dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_hidden_legacy\"}}\n\n"
		close(secondSendStarted)
		dataChan <- "data: {\"type\":\"response.output_text.delta\",\"sequence_number\":1,\"delta\":\"late\"}\n\n"
		close(producerFinished)
	})
	if constructionErr != nil {
		t.Fatalf("create legacy stream: %+v", constructionErr)
	}

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, commonresponses.NewEventStream(stream, commonresponses.IgnoreAcceptedResponsesEvent), observer, 91)
	if apiErr == nil || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected owner commit failure, got %+v", apiErr)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("response id escaped before owner commit: %q", recorder.Body.String())
	}
	select {
	case <-secondSendStarted:
	case <-time.After(time.Second):
		t.Fatal("legacy producer never reached its second raw channel send")
	}
	select {
	case <-producerFinished:
	case <-time.After(time.Second):
		t.Fatal("legacy producer remained blocked after stored owner failure")
	}
}

func TestResponsesNativeStreamRejectsEOFWithoutTerminal(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(1, 2)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_incomplete\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	observer.SetResponseIDObserver(func(responseID string) {
		recordResponsesEphemeralProof(ctx, responseID, 91)
	})
	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, observer)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected invalid provider response, got %+v", apiErr)
	}
	if !ctx.GetBool(responsesStreamErrorAlreadyRenderedContextKey) {
		t.Fatal("expected response stream error to be marked as rendered")
	}
	if channelID, ok := lookupResponsesEphemeralProof(ctx, "resp_incomplete"); !ok || channelID != 91 {
		t.Fatalf("exposed store:false response id lost its proof, channel=%d ok=%v", channelID, ok)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) ||
		!strings.Contains(body, `"code":"invalid_provider_response"`) ||
		!strings.Contains(body, `"sequence_number":1`) {
		t.Fatalf("expected sequenced Responses error event, got %q", body)
	}
}

func TestResponsesSSEEventFramerCommitsOnlyCompleteBoundedEvents(t *testing.T) {
	event := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_frame\"}}\n\n"
	framer := newResponsesSSEEventFramer(len(event))
	var observed []string
	visit := func(raw string) (bool, error) {
		observed = append(observed, raw)
		return false, nil
	}
	if stopped, err := framer.PushChunk(event[:len(event)-1], visit); stopped || err != nil || !framer.HasPending() || len(observed) != 0 {
		t.Fatalf("partial event was committed: stopped=%v err=%v pending=%v observed=%d", stopped, err, framer.HasPending(), len(observed))
	}
	if stopped, err := framer.PushChunk(event[len(event)-1:], visit); stopped || err != nil || framer.HasPending() || len(observed) != 1 || observed[0] != event {
		t.Fatalf("complete exact-limit event mismatch: stopped=%v err=%v pending=%v observed=%q", stopped, err, framer.HasPending(), observed)
	}

	overflow := newResponsesSSEEventFramer(len(event) - 1)
	called := false
	_, err := overflow.PushChunk(event, func(string) (bool, error) {
		called = true
		return false, nil
	})
	if !errors.Is(err, requester.ErrSSEEventTooLarge) || called || overflow.HasPending() {
		t.Fatalf("oversized event crossed framing boundary: err=%v called=%v pending=%v", err, called, overflow.HasPending())
	}
}

func TestResponsesNativeSyntheticErrorOmitsOverflowedSequence(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(1, 2)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":9223372036854775807,\"response\":{\"id\":\"resp_max_sequence\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
	if apiErr == nil {
		t.Fatal("missing terminal must still fail")
	}
	body := recorder.Body.String()
	errorMarker := "event: error\ndata: "
	errorStart := strings.LastIndex(body, errorMarker)
	if errorStart < 0 {
		t.Fatalf("synthetic error missing: %q", body)
	}
	errorJSON := body[errorStart+len(errorMarker):]
	if lineEnd := strings.IndexByte(errorJSON, '\n'); lineEnd >= 0 {
		errorJSON = errorJSON[:lineEnd]
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(errorJSON), &payload); err != nil {
		t.Fatalf("decode synthetic error %q: %v", errorJSON, err)
	}
	if _, exists := payload["sequence_number"]; exists {
		t.Fatalf("synthetic error must omit a non-incrementable sequence: %q", body)
	}
}

func TestResponsesNativePreservesProviderTrackingFailureWithPendingEvent(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(1, 2)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error, 1)}
	stream.dataChan <- "event: response.output_item.done\n"
	stream.errChan <- &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "provider_usage_state_limit", Message: "provider usage state limit exceeded"},
		StatusCode:  http.StatusBadGateway,
		LocalError:  true,
	}
	close(stream.dataChan)
	close(stream.errChan)

	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "provider_usage_state_limit" {
		t.Fatalf("provider tracking failure classification was lost: %+v", apiErr)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "response.output_item.done") || !strings.Contains(body, `"code":"provider_usage_state_limit"`) || strings.Count(body, "event: error") != 1 {
		t.Fatalf("pending offending event crossed tracking failure boundary: %q", body)
	}
}

func TestResponsesNativeStreamStopsAfterTerminal(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(1, 2)
	stream := &fakeRelayStream{dataChan: make(chan string, 1)}
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"id\":\"resp_done\",\"status\":\"completed\"}}\n\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":1,\"text\":\"late\"}\n\n"
	close(stream.dataChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, observer)
	if apiErr != nil {
		t.Fatalf("expected terminal event to complete the stream, got %+v", apiErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "response.completed") || strings.Contains(body, "late") || strings.Contains(body, "invalid_provider_response") {
		t.Fatalf("expected delivery to stop exactly at the terminal event, got %q", body)
	}
}

func TestStoredResponsesStreamStopsAfterTerminal(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(101, 102)
	stream := &fakeRelayStream{dataChan: make(chan string, 1)}
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":0,\"response\":{\"id\":\"resp_stored_done\",\"status\":\"completed\"}}\n\ndata: {\"type\":\"response.output_text.done\",\"sequence_number\":1,\"text\":\"late\"}\n\n"
	close(stream.dataChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 103)
	if apiErr != nil {
		t.Fatalf("expected terminal event to complete the stored stream, got %+v", apiErr)
	}
	owner, err := model.GetResponseOwner(ctx.Request.Context(), "resp_stored_done", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 103 {
		t.Fatalf("expected terminal response owner to be committed, owner=%+v err=%v", owner, err)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "response.completed") || strings.Contains(body, "late") || strings.Contains(body, "invalid_provider_response") {
		t.Fatalf("expected stored delivery to stop exactly at the terminal event, got %q", body)
	}
}

func TestStoredResponsesStreamDoesNotRejectProviderSequenceDetails(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(101, 102)
	stream := &fakeRelayStream{dataChan: make(chan string, 2), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":1,\"response\":{\"id\":\"resp_bad_terminal\",\"status\":\"in_progress\"}}\n\n"
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_bad_terminal\",\"status\":\"completed\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 103)
	if apiErr != nil {
		t.Fatalf("provider-owned sequence details must not invalidate exact-wire delivery: %+v", apiErr)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"response.completed"`) || strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected the provider terminal to be delivered without a synthetic error: %q", body)
	}
	owner, err := model.GetResponseOwner(ctx.Request.Context(), "resp_bad_terminal", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 103 {
		t.Fatalf("expected stored owner to remain established, owner=%+v err=%v", owner, err)
	}
}

func TestStoredResponsesStreamForwardsTopLevelErrorBeforeResponseID(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(101, 102)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"sequence_number\":0}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 103)
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "rate_limit_exceeded" || apiErr.UpstreamAccepted {
		t.Fatalf("expected rendered provider rejection to reach settlement, got %+v", apiErr)
	}
	if !ctx.GetBool(responsesStreamErrorAlreadyRenderedContextKey) {
		t.Fatal("outer error renderer must not duplicate the provider terminal event")
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, "rate_limit_exceeded") {
		t.Fatalf("expected buffered terminal error to reach the client, got %q", body)
	}
}

func TestResponsesStreamTopLevelErrorBeforeResponseIDIsProviderRejection(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})

	for _, test := range []struct {
		name  string
		store bool
	}{
		{name: "ephemeral", store: false},
		{name: "stored", store: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(111, 112)
			stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
			stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\",\"sequence_number\":0}\n\n"
			close(stream.dataChan)
			close(stream.errChan)

			request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &test.store}
			provider := &streamAffinityResponsesProvider{
				BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 113, Type: config.ChannelTypeOpenAI}},
				stream:       stream,
			}
			relay := &relayResponses{
				relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
				responsesRequest: request,
				rawEnvelope:      responsesTestRawEnvelope(t, request),
				operation:        responsesOperationCreate,
			}

			apiErr, done := relay.send()
			if apiErr == nil || !done || apiErr.UpstreamAccepted || openAIErrorCodeString(apiErr.Code, "") != "rate_limit_exceeded" {
				t.Fatalf("expected definitive provider rejection, done=%t err=%+v", done, apiErr)
			}
			if body := recorder.Body.String(); !strings.Contains(body, `"type":"error"`) || strings.Count(body, "rate_limit_exceeded") != 1 {
				t.Fatalf("expected one exact provider error event, got %q", body)
			}
		})
	}
}

func TestResponsesStreamTopLevelErrorAfterResponseIDKeepsAcceptedFloor(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, _ := responsesOwnerTestContext(111, 112)
	stream := &fakeRelayStream{dataChan: make(chan string, 2), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_started\",\"status\":\"in_progress\"}}\n\n"
	stream.dataChan <- "event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"failed\",\"sequence_number\":1}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	store := false
	request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &store}
	provider := &streamAffinityResponsesProvider{
		BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 113, Type: config.ChannelTypeOpenAI}},
		stream:       stream,
	}
	relay := &relayResponses{
		relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
		responsesRequest: request,
		rawEnvelope:      responsesTestRawEnvelope(t, request),
		operation:        responsesOperationCreate,
	}

	apiErr, done := relay.send()
	if apiErr == nil || !done || !apiErr.UpstreamAccepted || openAIErrorCodeString(apiErr.Code, "") != "server_error" {
		t.Fatalf("expected accepted provider failure after response id, done=%t err=%+v", done, apiErr)
	}
}

func TestStoredResponsesStreamEmitsProtocolErrorAfterCommittedID(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(101, 102)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
	stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_missing_terminal\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 103)
	if apiErr == nil || apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected invalid provider response, got %+v", apiErr)
	}
	owner, err := model.GetResponseOwner(ctx.Request.Context(), "resp_missing_terminal", ctx.GetInt("id"))
	if err != nil || owner.ChannelID != 103 {
		t.Fatalf("expected owner to remain committed, owner=%+v err=%v", owner, err)
	}
	if body := recorder.Body.String(); !strings.Contains(body, `"sequence_number":1`) {
		t.Fatalf("expected protocol error after committed response id, got %q", body)
	}
}

func TestStoredResponsesStreamSeparatesPartialEventAfterCommittedID(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	ctx, recorder := responsesOwnerTestContext(101, 102)
	stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error)}
	go func() {
		stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_partial_event\"}}\n\n"
		stream.dataChan <- `data: {"type":"response.output_text.delta","sequence_number":1,"delta":"partial"}`
		stream.errChan <- errors.New("provider stream reset")
		close(stream.dataChan)
		close(stream.errChan)
	}()

	observer := commonresponses.NewStreamObserver()
	_, apiErr := responseStoredResponsesStreamClient(ctx, stream, observer, 103)
	if apiErr == nil {
		t.Fatal("partial stored provider event was accepted as complete")
	}
	body := recorder.Body.String()
	if strings.Contains(body, `"delta":"partial"`) {
		t.Fatalf("stored incomplete provider event crossed the complete-event boundary: %q", body)
	}
	if strings.Count(body, "event: error") != 1 {
		t.Fatalf("expected one stored terminal error event, got %q", body)
	}
}

func TestResponsesStreamErrorsPreserveAcceptedQuotaFloor(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})

	for _, test := range []struct {
		name  string
		store bool
	}{
		{name: "ephemeral", store: false},
		{name: "stored", store: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, _ := responsesOwnerTestContext(111, 112)
			stream := &fakeRelayStream{dataChan: make(chan string, 2), errChan: make(chan error)}
			stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_accepted_" + test.name + "\",\"status\":\"in_progress\"}}\n\n"
			stream.dataChan <- "data: {\"type\":\"response.done\",\"sequence_number\":1,\"response\":{\"id\":\"resp_accepted_" + test.name + "\",\"status\":\"completed\"}}\n\n"
			close(stream.dataChan)
			close(stream.errChan)

			request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &test.store}
			provider := &streamAffinityResponsesProvider{
				BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 113, Type: config.ChannelTypeOpenAI}},
				stream:       stream,
			}
			relay := &relayResponses{
				relayBase:        relayBase{c: ctx, provider: provider, modelName: "gpt-5"},
				responsesRequest: request,
				rawEnvelope:      responsesTestRawEnvelope(t, request),
				operation:        responsesOperationCreate,
			}

			apiErr, done := relay.send()
			if apiErr == nil || !done || !apiErr.UpstreamAccepted || apiErr.Code != "invalid_provider_response" {
				t.Fatalf("expected accepted stream protocol error, done=%t err=%+v", done, apiErr)
			}
		})
	}
}

func TestNativeResponsesStreamCancellationSurfacesAcceptedFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	requestCtx, cancel := context.WithCancel(context.Background())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestCtx)
	stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error)}
	cancel()

	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
	if apiErr == nil || openAIErrorCodeString(apiErr.Code, "") != "request_canceled" || apiErr.StatusCode != 499 {
		t.Fatalf("accepted stream cancellation must remain an error for floor settlement, got %+v", apiErr)
	}
}

func TestNativeResponsesStreamSeparatesPartialEventFromProxyError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error, 1)}
	stream.dataChan <- `data: {"type":"response.output_text.delta","sequence_number":0,"delta":"partial"}`
	stream.errChan <- errors.New("provider stream reset")
	close(stream.dataChan)
	close(stream.errChan)

	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
	if apiErr == nil {
		t.Fatal("partial provider event was accepted as a complete stream")
	}
	body := recorder.Body.String()
	if strings.Contains(body, `"delta":"partial"`) {
		t.Fatalf("incomplete provider event crossed the complete-event boundary: %q", body)
	}
	if strings.Count(body, "event: error") != 1 {
		t.Fatalf("expected one terminal error event, got %q", body)
	}
}

func TestResponsesNativeAndStoredRejectUnacceptedUsage(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	for _, stored := range []bool{false, true} {
		for _, test := range []struct {
			name string
			tail string
		}{
			{"incomplete", "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_prefix\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9999,\"output_tokens\":9999,\"total_tokens\":19998}}}\n"},
			{"identity_conflict", "data: {\"type\":\"response.completed\",\"sequence_number\":2,\"response\":{\"id\":\"resp_bad\",\"status\":\"completed\",\"usage\":{\"input_tokens\":9999,\"output_tokens\":9999,\"total_tokens\":19998}}}\n\n"},
		} {
			t.Run(fmt.Sprintf("stored=%t/%s", stored, test.name), func(t *testing.T) {
				ctx, recorder := responsesOwnerTestContext(121, 122)
				usage := &types.Usage{}
				handler := &openai.OpenAIResponsesStreamHandler{Usage: usage}
				body := "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_prefix\"}}\n\n" +
					"data: {\"type\":\"response.output_item.done\",\"sequence_number\":1,\"item\":{\"id\":\"ws_prefix\",\"type\":\"web_search_call\",\"status\":\"completed\",\"action\":{\"type\":\"search\"}}}\n\n" + test.tail
				rawStream, constructionErr := requester.RequestNoTrimStreamWithEmitterOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.HandlerResponsesStreamWithEmitter, requester.StreamReadOptions{})
				if constructionErr != nil {
					t.Fatal(constructionErr)
				}
				stream := commonresponses.NewEventStream(rawStream, handler.ObserveAcceptedResponsesEvent)
				observer := commonresponses.NewStreamObserver()
				var apiErr *types.OpenAIErrorWithStatusCode
				if stored {
					_, apiErr = responseStoredResponsesStreamClient(ctx, stream, observer, 123)
				} else {
					_, apiErr = responseNativeResponsesStreamClient(ctx, stream, observer)
				}
				key := types.BuildExtraBillingKey(types.APIToolTypeWebSearchPreview, "medium")
				if apiErr == nil || usage.TotalTokens != 0 || usage.ExtraBilling[key].CallCount != 1 || observer.TerminalSeen() || strings.Contains(recorder.Body.String(), "9999") {
					t.Fatalf("rejected terminal contaminated accepted prefix: err=%v usage=%+v terminal=%t body=%s", apiErr, usage, observer.TerminalSeen(), recorder.Body.String())
				}
			})
		}
	}
}

func TestStoredResponsesPreservesTrackingFailureBeforeOwner(t *testing.T) {
	for _, code := range []string{"provider_usage_state_limit", "provider_protocol_error"} {
		t.Run(code, func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(131, 132)
			stream := &fakeRelayStream{dataChan: make(chan string), errChan: make(chan error)}
			go func() {
				defer close(stream.dataChan)
				defer close(stream.errChan)
				stream.dataChan <- "event: response.output_item.done\n"
				stream.errChan <- &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: code, Message: "tracking failed"}, StatusCode: 502, LocalError: true}
			}()
			_, apiErr := responseStoredResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver(), 133)
			if apiErr == nil || apiErr.Code != code || recorder.Body.Len() != 0 {
				t.Fatalf("typed failure before owner changed: err=%+v body=%q", apiErr, recorder.Body.String())
			}
		})
	}
}

func TestResponsesAccountingFailureKeepsLifecyclePrefix(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	for _, stored := range []bool{false, true} {
		for _, prefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("stored=%t/prefix=%t", stored, prefix), func(t *testing.T) {
				ctx, recorder := responsesOwnerTestContext(141, 142)
				stream := &fakeRelayStream{dataChan: make(chan string, 3), errChan: make(chan error)}
				if prefix {
					stream.dataChan <- "data: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_accounting\"}}\n\n"
				}
				stream.dataChan <- "data: {\"type\":\"response.completed\",\"sequence_number\":1,\"response\":{\"id\":\"resp_accounting\",\"status\":\"completed\"}}\n\n"
				stream.dataChan <- "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
				close(stream.dataChan)
				close(stream.errChan)
				calls := 0
				stream.observeAccepted = func(event string) error {
					calls++
					if strings.Contains(event, "response.completed") {
						return &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Code: "provider_usage_state_limit", Message: "tracking limit"}, StatusCode: 502, LocalError: true}
					}
					return nil
				}
				observer := commonresponses.NewStreamObserver()
				identityCalls := 0
				observer.SetResponseIDObserver(func(string) { identityCalls++ })
				var apiErr *types.OpenAIErrorWithStatusCode
				if stored {
					_, apiErr = responseStoredResponsesStreamClient(ctx, stream, observer, 143)
				} else {
					_, apiErr = responseNativeResponsesStreamClient(ctx, stream, observer)
				}
				wantCalls, wantIdentityCalls := 1, 0
				if prefix {
					wantCalls, wantIdentityCalls = 2, 1
				}
				body := recorder.Body.String()
				if apiErr == nil || apiErr.Code != "provider_usage_state_limit" || calls != wantCalls || identityCalls != wantIdentityCalls || observer.TerminalSeen() || strings.Contains(body, "response.completed") || strings.Contains(body, "late") || strings.Contains(body, `"sequence_number":2`) {
					t.Fatalf("accounting rejection changed prefix: err=%v calls=%d identities=%d terminal=%t body=%q", apiErr, calls, identityCalls, observer.TerminalSeen(), body)
				}
				if stored && !prefix && recorder.Body.Len() != 0 {
					t.Fatalf("unowned event was delivered: %q", body)
				}
			})
		}
	}
}

func TestResponsesChatFirstAccountingFailurePreservesAcceptedFloor(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(151, 152)
	handler := &openai.OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_rejected\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
	calls := 0
	stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(func(string) error {
		calls++
		return common.StringErrorWrapperLocal("tracking limit", "provider_usage_state_limit", http.StatusBadGateway)
	}), requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	_, apiErr = responseStreamClient(ctx, stream, nil)
	if apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted || calls != 1 {
		t.Fatalf("first rejected event lost accepted execution: error=%+v calls=%d", apiErr, calls)
	}
	if strings.Contains(recorder.Body.String(), "late") || strings.Count(recorder.Body.String(), `"error"`) != 1 {
		t.Fatalf("unexpected stream after first accounting failure: %s", recorder.Body.String())
	}
}

func TestResponsesFirstAccountingFailureIsAcceptedByRelay(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	for _, stored := range []bool{false, true} {
		t.Run(fmt.Sprintf("stored=%t", stored), func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(161, 162)
			calls := 0
			stream := &fakeRelayStream{dataChan: make(chan string, 2), errChan: make(chan error), observeAccepted: func(string) error {
				calls++
				return common.StringErrorWrapperLocal("tracking limit", "provider_usage_state_limit", 502)
			}}
			stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_first_rejected\",\"status\":\"completed\"}}\n\n"
			stream.dataChan <- "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
			close(stream.dataChan)
			close(stream.errChan)
			request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &stored}
			provider := &streamAffinityResponsesProvider{
				BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 163, Type: config.ChannelTypeOpenAI}}, stream: stream,
			}
			r := &relayResponses{relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"}, responsesRequest: request, rawEnvelope: responsesTestRawEnvelope(t, request), operation: responsesOperationCreate}
			apiErr, done := r.send()
			if apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted || !done || calls != 1 {
				t.Fatalf("first rejected event lost execution boundary: err=%+v done=%t calls=%d", apiErr, done, calls)
			}
			if strings.Contains(recorder.Body.String(), "resp_first_rejected") || strings.Contains(recorder.Body.String(), "late") {
				t.Fatalf("rejected or late event was delivered: %s", recorder.Body.String())
			}
			var count int64
			if err := model.DB.Model(&model.ResponseOwner{}).Count(&count).Error; err != nil || count != 0 {
				t.Fatalf("rejected event created owner: count=%d err=%v", count, err)
			}
		})
	}
}

func TestResponsesIDLessPrefixPreventsDefinitiveRejection(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	for _, mode := range []string{"native", "stored", "chat"} {
		for _, prefix := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/prefix=%t", mode, prefix), func(t *testing.T) {
				ctx, _ := responsesOwnerTestContext(191, 192)
				body := ": keepalive\n\n"
				if prefix {
					body += "data: {\"type\":\"future.event\",\"payload\":\"accepted without response ID\"}\n\n"
				}
				body += "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"upstream error\"}\n\n"
				var apiErr *types.OpenAIErrorWithStatusCode
				if mode == "chat" {
					handler := &openai.OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
					stream, err := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{RequireProtocolTerminal: true})
					if err != nil {
						t.Fatal(err)
					}
					_, apiErr = responseStreamClient(ctx, stream, nil)
				} else {
					stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
					stream.dataChan <- body
					close(stream.dataChan)
					close(stream.errChan)
					stored := mode == "stored"
					request := types.OpenAIResponsesRequest{Model: "gpt-5", Stream: true, Store: &stored}
					provider := &streamAffinityResponsesProvider{BaseProvider: providersBase.BaseProvider{Channel: &model.Channel{Id: 193, Type: config.ChannelTypeOpenAI}}, stream: stream}
					r := &relayResponses{relayBase: relayBase{c: ctx, provider: provider, modelName: "gpt-5"}, responsesRequest: request, rawEnvelope: responsesTestRawEnvelope(t, request), operation: responsesOperationCreate}
					var done bool
					apiErr, done = r.send()
					if !done {
						t.Fatal("stream error cannot reopen submission")
					}
				}
				if apiErr == nil || apiErr.Code != "server_error" || apiErr.UpstreamAccepted != prefix {
					t.Fatalf("incorrect prefix acceptance: prefix=%t err=%+v", prefix, apiErr)
				}
			})
		}
	}
}

func TestResponsesChatFirstDecodeFailurePreservesAcceptedExecution(t *testing.T) {
	ctx, recorder := responsesOwnerTestContext(201, 202)
	handler := &openai.OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"type\":\"response.created\",\"response\":[]}\n\n" + "data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n"
	stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	_, apiErr = responseStreamClient(ctx, stream, nil)
	if apiErr == nil || !apiErr.UpstreamAccepted || strings.Contains(recorder.Body.String(), "late") || strings.Count(recorder.Body.String(), `"error"`) != 1 {
		t.Fatalf("decode failure lost accepted execution: error=%+v body=%s", apiErr, recorder.Body.String())
	}
}

func TestResponsesChatInitialSizeFailurePreservesAcceptedExecution(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(fmt.Sprintf("physical=%t", physical), func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(171, 172)
			handler := &openai.OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
			body := "data: " + strings.Repeat("x", 17<<20) + "\n\n"
			if !physical {
				body = strings.Repeat(": "+strings.Repeat("x", 1020)+"\n", (17<<20)/1023)
			}
			stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{RequireProtocolTerminal: true, MaxLineBytes: 16 << 20})
			if apiErr != nil {
				t.Fatal(apiErr)
			}
			_, apiErr = responseStreamClient(ctx, stream, nil)
			if apiErr == nil || apiErr.Code != "provider_usage_state_limit" || !apiErr.UpstreamAccepted || strings.Count(recorder.Body.String(), `"error"`) != 1 {
				t.Fatalf("opened stream size failure lost execution: error=%+v body=%s", apiErr, recorder.Body.String())
			}
		})
	}
}

type responsesFailingWriter struct {
	header    http.Header
	writes    int
	failAfter int
}

func (w *responsesFailingWriter) Header() http.Header { return w.header }
func (w *responsesFailingWriter) WriteHeader(int)     {}
func (w *responsesFailingWriter) Flush()              {}
func (w *responsesFailingWriter) Write(data []byte) (int, error) {
	w.writes++
	if w.writes <= w.failAfter {
		return len(data), nil
	}
	return 0, errors.New("client disconnected")
}

func TestResponsesDeliveryFailureRetainsAcceptedUsageWithoutSecondWrite(t *testing.T) {
	writer := &responsesFailingWriter{header: make(http.Header)}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	usage := 0
	stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error), observeAccepted: func(string) error { usage = 5; return nil }}
	stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_delivery_failure\",\"status\":\"completed\"}}\n\n"
	close(stream.dataChan)
	close(stream.errChan)
	_, apiErr := responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
	if apiErr == nil || apiErr.Code != "write_response_body_failed" || usage != 5 || writer.writes != 1 {
		t.Fatalf("delivery failure changed provider facts or retried write: err=%v usage=%d writes=%d", apiErr, usage, writer.writes)
	}
}

func TestResponsesChatDeliveryFailureRetainsBillingAndSuppressesOuterWrite(t *testing.T) {
	writer := &responsesFailingWriter{header: make(http.Header)}
	ctx, _ := gin.CreateTestContext(writer)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	handler := &openai.OpenAIResponsesStreamHandler{Usage: &types.Usage{}}
	body := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_chat_delivery\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
	stream, apiErr := requester.RequestNoTrimStreamWithOptions[string](nil, &http.Response{Body: io.NopCloser(strings.NewReader(body))}, handler.ChatSSEHandler(handler.ObserveAcceptedResponsesEvent), requester.StreamReadOptions{RequireProtocolTerminal: true})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	_, apiErr = responseStreamClient(ctx, stream, nil)
	if apiErr == nil || apiErr.Code != "stream_write_failed" || !apiErr.UpstreamAccepted || !ctx.GetBool(streamErrorAlreadyRenderedContextKey) || handler.Usage.TotalTokens != 5 || writer.writes != 1 {
		t.Fatalf("delivery failure lost accounting or allows another write: error=%+v usage=%+v writes=%d rendered=%t", apiErr, handler.Usage, writer.writes, ctx.GetBool(streamErrorAlreadyRenderedContextKey))
	}
}

func TestStoredResponsesDeliveryFailureRetainsOwnerAndSuppressesOuterWrite(t *testing.T) {
	for _, failAfter := range []int{0, 1} {
		t.Run(fmt.Sprintf("fail_after=%d", failAfter), func(t *testing.T) {
			setupRelayTestDB(t, &model.ResponseOwner{})
			writer := &responsesFailingWriter{header: make(http.Header), failAfter: failAfter}
			ctx, _ := gin.CreateTestContext(writer)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			ctx.Set("id", 181)
			ctx.Set("token_id", 182)
			ctx.Set("channel_type", config.ChannelTypeOpenAI)
			accepted := 0
			stream := &fakeRelayStream{dataChan: make(chan string, 2), errChan: make(chan error), observeAccepted: func(string) error { accepted++; return nil }}
			stream.dataChan <- "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_delivery_failure\"}}\n\n"
			stream.dataChan <- "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_delivery_failure\",\"status\":\"completed\"}}\n\n"
			close(stream.dataChan)
			close(stream.errChan)
			_, apiErr := responseStoredResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver(), 183)
			if apiErr == nil || apiErr.Code != "write_response_body_failed" || !ctx.GetBool(streamErrorAlreadyRenderedContextKey) || writer.writes != failAfter+1 || accepted != failAfter+1 {
				t.Fatalf("delivery failure changed accepted prefix or permits outer write: err=%+v accepted=%d writes=%d rendered=%t", apiErr, accepted, writer.writes, ctx.GetBool(streamErrorAlreadyRenderedContextKey))
			}
			var count int64
			if err := model.DB.Model(&model.ResponseOwner{}).Where("response_id = ?", "resp_delivery_failure").Count(&count).Error; err != nil || count != 1 {
				t.Fatalf("committed owner lost on delivery failure: count=%d err=%v", count, err)
			}
		})
	}
}

func TestResponsesMultilineSSESecurityAtDelivery(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	for _, stored := range []bool{false, true} {
		t.Run(fmt.Sprint(stored), func(t *testing.T) {
			ctx, recorder := responsesOwnerTestContext(211, 212)
			stream := &fakeRelayStream{dataChan: make(chan string, 1), errChan: make(chan error)}
			stream.dataChan <- "event: response.created\r\ndata: {\"type\":\"response.created\",\r\ndata: \"response\":{\"id\":\"resp_multidata\",\"account_id\":\"acct-secret\"}}\r\n\r\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_multidata\",\"status\":\"completed\"}}\r\n\r\n"
			close(stream.dataChan)
			close(stream.errChan)
			var apiErr *types.OpenAIErrorWithStatusCode
			if stored {
				_, apiErr = responseStoredResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver(), 213)
			} else {
				_, apiErr = responseNativeResponsesStreamClient(ctx, stream, commonresponses.NewStreamObserver())
			}
			if apiErr != nil || strings.Contains(recorder.Body.String(), "acct-secret") || !strings.Contains(recorder.Body.String(), "response.completed") {
				t.Fatalf("multiline metadata security failed: err=%+v body=%s", apiErr, recorder.Body.String())
			}
		})
	}
}
