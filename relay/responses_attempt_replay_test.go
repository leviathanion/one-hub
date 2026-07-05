package relay

import (
	"fmt"
	"net/http"
	"testing"

	"one-api/common/config"
	"one-api/types"
)

func TestDecideResponsesAttemptReplay(t *testing.T) {
	originalRetryStatusCodes := config.RetryStatusCodes
	if err := config.SetRetryStatusCodes(config.DefaultRetryStatusCodes); err != nil {
		t.Fatalf("expected default retry status codes to parse, got %v", err)
	}
	t.Cleanup(func() {
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

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
			name: "retry provider credential rejection before accept",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Failure = &types.OpenAIErrorWithStatusCode{
					OpenAIError: types.OpenAIError{Type: "invalid_request_error", Code: "token_invalidated"},
					StatusCode:  http.StatusUnauthorized,
				}
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
			name: "non strict continuation can retry",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Continuation.PreviousResponseID = "resp_previous"
			},
			wantDecision: ResponsesAttemptDecisionRollbackAndRetryNextChannel,
			wantBarrier:  ReplayBarrierNone,
		},
		{
			name: "strict continuation anchor rolls back and surfaces",
			mutate: func(snapshot *ResponsesAttemptSnapshot) {
				snapshot.Continuation.PreviousResponseID = "resp_previous"
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
	originalRetryStatusCodes := config.RetryStatusCodes
	if err := config.SetRetryStatusCodes(config.DefaultRetryStatusCodes); err != nil {
		t.Fatalf("expected default retry status codes to parse, got %v", err)
	}
	t.Cleanup(func() {
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	tests := []struct {
		status int
		want   bool
	}{
		{status: http.StatusTooManyRequests, want: true},
		{status: http.StatusServiceUnavailable, want: true},
		{status: http.StatusTemporaryRedirect, want: true},
		{status: http.StatusRequestTimeout, want: true},
		{status: http.StatusGatewayTimeout, want: true},
		{status: http.StatusNotImplemented, want: false},
		{status: 524, want: false},
		{status: http.StatusUnauthorized, want: true},
		{status: http.StatusForbidden, want: true},
		{status: http.StatusNotFound, want: false},
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

func TestResponsesAttemptRetryableFailureUsesConfiguredStatusPolicy(t *testing.T) {
	originalRetryStatusCodes := config.RetryStatusCodes
	if err := config.SetRetryStatusCodes("401"); err != nil {
		t.Fatalf("expected retry status override to parse, got %v", err)
	}
	t.Cleanup(func() {
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	if !responsesAttemptRetryableFailure(&types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("expected configured status 401 to be retryable")
	}
	if responsesAttemptRetryableFailure(&types.OpenAIErrorWithStatusCode{StatusCode: http.StatusServiceUnavailable}) {
		t.Fatal("expected status 503 to stop retrying after retry status override")
	}
}

func TestResponsesAttemptRetryableFailureCredentialSignals(t *testing.T) {
	originalRetryStatusCodes := config.RetryStatusCodes
	if err := config.SetRetryStatusCodes(config.DefaultRetryStatusCodes); err != nil {
		t.Fatalf("expected default retry status codes to parse, got %v", err)
	}
	t.Cleanup(func() {
		if err := config.SetRetryStatusCodes(originalRetryStatusCodes); err != nil {
			t.Fatalf("restore retry status codes: %v", err)
		}
	})

	if !responsesAttemptRetryableFailure(&types.OpenAIErrorWithStatusCode{StatusCode: http.StatusUnauthorized}) {
		t.Fatal("expected status-only 401 to be retryable after provider boundary")
	}
	for _, code := range []string{"provider_authentication_failed", "invalid_api_key", "token_invalidated"} {
		t.Run(code, func(t *testing.T) {
			got := responsesAttemptRetryableFailure(&types.OpenAIErrorWithStatusCode{
				OpenAIError: types.OpenAIError{Type: "authentication_error", Code: code},
				StatusCode:  http.StatusUnauthorized,
			})
			if !got {
				t.Fatalf("expected credential code %q to be retryable", code)
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
