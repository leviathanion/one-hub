package relay

import (
	"fmt"
	"net/http"
	"testing"

	"one-api/types"
)

func TestDecideResponsesAttemptReplay(t *testing.T) {
	retryableErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded"},
		StatusCode:  http.StatusTooManyRequests,
	}
	base := ResponsesAttemptSnapshot{
		AttemptID:  "attempt",
		OpeningID:  "opening",
		Upstream:   ResponsesAttemptUpstreamRejectedBeforeAccept,
		Downstream: ResponsesAttemptDownstreamUncommitted,
		Accounting: ResponsesAttemptAccountingZeroChargeProofAvailable,
		Failure:    retryableErr,
		Origin:     ResponsesAttemptFailureOriginWSProviderRequestError,
		Replay:     ResponsesAttemptReplayRawCreateFirstTurn,
		Affinity:   ResponsesAttemptAffinityFree,
		Turn:       ResponsesAttemptTurnFirst,
	}

	tests := []struct {
		name         string
		mutate       func(*ResponsesAttemptSnapshot)
		wantDecision ResponsesAttemptReplayDecision
		wantBarrier  ResponsesReplayBlockingBarrier
	}{
		{
			name:         "retry provider rejection before accept",
			wantDecision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			wantBarrier:  ReplayBarrierNone,
		},
		{
			name: "pending create cancel blocks retry",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.PendingCreateCancel = true
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierClientCancel,
		},
		{
			name: "retry provider 5xx rejection before accept",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Failure = &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusServiceUnavailable}
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			wantBarrier:  ReplayBarrierNone,
		},
		{
			name: "downstream committed blocks replay",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Downstream = ResponsesAttemptDownstreamCommitted
			},
			wantDecision: ResponsesAttemptDecisionSurface,
			wantBarrier:  ReplayBarrierDownstreamCommitted,
		},
		{
			name: "watermark committed blocks replay independently",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Watermark.Committed = true
			},
			wantDecision: ResponsesAttemptDecisionSurface,
			wantBarrier:  ReplayBarrierDownstreamCommitted,
		},
		{
			name: "provider accepted blocks replay",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Upstream = ResponsesAttemptUpstreamAccepted
			},
			wantDecision: ResponsesAttemptDecisionSurface,
			wantBarrier:  ReplayBarrierProviderAccepted,
		},
		{
			name: "usage evidence blocks replay",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Accounting = ResponsesAttemptAccountingUsageSeen
			},
			wantDecision: ResponsesAttemptDecisionSurface,
			wantBarrier:  ReplayBarrierAccounting,
		},
		{
			name: "continuation rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Continuation.PreviousResponseID = "resp_previous"
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierContinuation,
		},
		{
			name: "strict continuation anchor rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Continuation.Strict = true
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierContinuation,
		},
		{
			name: "explicit pin rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Affinity = ResponsesAttemptAffinityExplicitPin
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierAffinity,
		},
		{
			name: "preferred skip retry rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Affinity = ResponsesAttemptAffinityPreferred
				snapshot.SkipRetryAfterPreferredFailure = true
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierPreferredSkipRetry,
		},
		{
			name: "missing raw replay capability rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Replay = ResponsesAttemptReplayNone
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierReplayCapability,
		},
		{
			name: "non retryable status rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Failure = &types.OpenAIErrorWithStatusCode{StatusCode: http.StatusBadRequest}
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndSurface,
			wantBarrier:  ReplayBarrierNonRetryableStatus,
		},
		{
			name: "ambiguous transport never retries",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Upstream = ResponsesAttemptUpstreamAmbiguous
			},
			wantDecision: ResponsesAttemptDecisionNoRetryAmbiguous,
			wantBarrier:  ReplayBarrierAmbiguous,
		},
		{
			name: "unknown upstream surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Upstream = ResponsesAttemptUpstreamUnknown
			},
			wantDecision: ResponsesAttemptDecisionSurface,
			wantBarrier:  ReplayBarrierNone,
		},
		{
			name: "not attempted can retry with raw first turn",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Upstream = ResponsesAttemptUpstreamNotAttempted
				snapshot.Accounting = ResponsesAttemptAccountingNoEvidence
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			wantBarrier:  ReplayBarrierNone,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := base
			if tc.mutate != nil {
				tc.mutate(&snapshot)
			}
			got := DecideResponsesAttemptReplay(snapshot)
			if got.Decision != tc.wantDecision || got.Barrier != tc.wantBarrier {
				t.Fatalf("decision=%+v, want decision=%v barrier=%v", got, tc.wantDecision, tc.wantBarrier)
			}
		})
	}
}

func TestResponsesAttemptRetryableFailureStatuses(t *testing.T) {
	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusServiceUnavailable, want: true},
		{status: http.StatusTemporaryRedirect, want: false},
		{status: http.StatusRequestTimeout, want: false},
		{status: http.StatusGatewayTimeout, want: false},
		{status: cloudflareStatusTimeout, want: false},
		{status: http.StatusUnauthorized, want: false},
		{status: http.StatusForbidden, want: false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%d", tt.status), func(t *testing.T) {
			got := responsesAttemptRetryableFailure(&types.OpenAIErrorWithStatusCode{StatusCode: tt.status})
			if got != tt.want {
				t.Fatalf("retryable(%d)=%v, want %v", tt.status, got, tt.want)
			}
		})
	}
}

func TestClassifyResponsesChannelFailureUsesExactQuotaSignals(t *testing.T) {
	if got := classifyResponsesChannelFailure(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "invalid_quota_parameter"},
		StatusCode:  http.StatusBadRequest,
	}); got != ChannelFailureNone {
		t.Fatalf("expected invalid_quota_parameter not to be quota exhaustion, got %v", got)
	}
	if got := classifyResponsesChannelFailure(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "insufficient_quota"},
		StatusCode:  http.StatusBadRequest,
	}); got != ChannelFailureQuotaExhausted {
		t.Fatalf("expected insufficient_quota to be quota exhaustion, got %v", got)
	}
	if got := classifyResponsesChannelFailure(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "usage_limit_reached"},
		StatusCode:  http.StatusTooManyRequests,
	}); got != ChannelFailureQuotaExhausted {
		t.Fatalf("expected usage_limit_reached 429 to be quota exhaustion, got %v", got)
	}
	if got := classifyResponsesChannelFailure(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Code: "insufficient_quota"},
		StatusCode:  http.StatusTooManyRequests,
	}); got != ChannelFailureQuotaExhausted {
		t.Fatalf("expected insufficient_quota 429 to be quota exhaustion, got %v", got)
	}
	if got := classifyResponsesChannelFailure(&types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Type: "rate_limit_error", Code: "rate_limit_exceeded"},
		StatusCode:  http.StatusTooManyRequests,
	}); got != ChannelFailureRateLimited {
		t.Fatalf("expected generic rate limit 429 to remain rate limited, got %v", got)
	}
}
