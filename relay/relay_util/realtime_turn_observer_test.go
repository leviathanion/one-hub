package relay_util

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	zapobserver "go.uber.org/zap/zaptest/observer"
	"net/http"
	"net/http/httptest"
	"one-api/common/limit"
	"one-api/internal/billing"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	runtimesession "one-api/runtime/session"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func realtimeBillingFixture(t *testing.T) *gin.Context {
	t.Helper()
	logger.Logger = zap.NewNop()
	originalDB := model.DB
	originalPricing := model.PricingInstance
	originalLog := config.LogConsumeEnabled
	originalBatch := config.BatchUpdateEnabled
	originalGroupPolicy := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}, &model.UserGroup{}, &model.Channel{}, &model.Log{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	model.PricingInstance = &model.Pricing{Prices: make(map[string]*model.Price)}
	config.LogConsumeEnabled = false
	config.BatchUpdateEnabled = false
	t.Cleanup(func() {
		model.DB = originalDB
		for _, limiter := range model.GlobalUserGroupRatio.APILimiter {
			if stopper, ok := limiter.(interface{ Stop() }); ok {
				stopper.Stop()
			}
		}
		model.GlobalUserGroupRatio = originalGroupPolicy
		model.PricingInstance = originalPricing
		config.LogConsumeEnabled = originalLog
		config.BatchUpdateEnabled = originalBatch
	})
	if err := db.Create(&model.User{Id: 1, Username: "u", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled, Group: "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 1, UserId: 1, Key: "token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Price{Model: "gpt-test", Type: model.TokensPriceType, Input: 1, Output: 1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := db.Create(&model.UserGroup{Symbol: "paid", Name: "Paid", Ratio: 1, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime", nil)
	c.Set("id", 1)
	c.Set("token_id", 1)
	c.Set("channel_id", 1)
	c.Set("token_name", "test")
	c.Set("group_ratio", 1.0)
	groupctx.SetRoutingGroup(c, "paid", groupctx.RoutingGroupSourceUserGroup)
	return c
}

func realtimeTestModels() runtimesession.ModelBinding {
	return runtimesession.ModelBinding{RequestedModel: "gpt-test", ProviderModel: "gpt-test", BillingModel: "gpt-test"}
}

func boundedRealtimeAdmission() runtimesession.TurnAdmission {
	return runtimesession.TurnAdmission{
		ExplicitClientCreate:      true,
		AutomaticFeaturesDisabled: true,
		Models:                    realtimeTestModels(),
		PromptTokens:              10,
		MaxOutputTokens:           100,
	}
}

func TestRealtimeTurnConfirmsProviderUsageAndRefundsDifference(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.AdmitBoundedTurn(boundedRealtimeAdmission()); err != nil {
		t.Fatal(err)
	}
	reserved := observer.attempt.ReservedQuota()
	if reserved <= 30 {
		t.Fatalf("reservation=%d does not cover the terminal charge", reserved)
	}
	assertRealtimeUserQuota(t, 1000-int(reserved))

	usage := &types.UsageEvent{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, Source: types.UsageSourceRealtimeResponse, BillingBasis: types.UsageBillingBasisTokens, ProviderTokenEvidence: true}
	if err := observer.ObserveTurnUsage(usage); err != nil {
		t.Fatal(err)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: types.EventTypeResponseDone, Usage: usage})
	assertRealtimeUserQuota(t, 970)

	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: types.EventTypeResponseDone, Usage: usage})
	assertRealtimeUserQuota(t, 970)
}

func TestRealtimeRejectsProviderInitiatedWorkBeforeReserve(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.AdmitTurn(); err == nil {
		t.Fatal("provider-initiated work unexpectedly admitted")
	}
	assertRealtimeUserQuota(t, 1000)
}

func TestRealtimeProviderInitiatedObservationSettlesWithoutSyntheticReserve(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels()}); err != nil {
		t.Fatal(err)
	}
	if observer.attempt == nil || !observer.attempt.SubmissionClaimed() || observer.attempt.ReservedQuota() != 0 {
		t.Fatalf("provider observation did not create a zero-reserve claimed attempt: %+v", observer.attempt)
	}
	assertRealtimeUserQuota(t, 1000)
	usage := &types.UsageEvent{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, Source: types.UsageSourceRealtimeResponse, BillingBasis: types.UsageBillingBasisTokens, ProviderTokenEvidence: true}
	if err := observer.ObserveTurnUsage(usage); err != nil {
		t.Fatal(err)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: types.EventTypeResponseDone, Usage: usage})
	assertRealtimeUserQuota(t, 970)
}

func TestRealtimeToolOnlyUsageEventSettlesWithoutSourceTag(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels()}); err != nil {
		t.Fatal(err)
	}
	key := types.APIToolTypeWebSearch
	billing := types.ExtraBilling{ServiceType: key, Type: "medium", CallCount: 1}
	usage := &types.UsageEvent{
		ExtraBilling:         map[string]types.ExtraBilling{key: billing},
		ProviderExtraBilling: map[string]bool{key: true},
	}
	if err := observer.ObserveTurnUsage(usage); err != nil {
		t.Fatal(err)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: types.EventTypeResponseDone, Usage: usage})
	wantCharge := saturatingCeilToInt(defaultExtraServicePrices.WebSearchGA * config.QuotaPerUnit)
	assertRealtimeUserQuota(t, 1000-wantCharge)
}

func TestRealtimeUsageMarkersDoNotAuthorizeEarlierUntrustedValues(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels()}); err != nil {
		t.Fatal(err)
	}
	key := types.APIToolTypeWebSearch
	if err := observer.ObserveTurnUsage(&types.UsageEvent{
		InputTokens: 100,
		TotalTokens: 100,
		ExtraBilling: map[string]types.ExtraBilling{
			key: {ServiceType: key, Type: "medium", CallCount: 100},
		},
	}); err != nil {
		t.Fatal(err)
	}
	trusted := &types.UsageEvent{
		InputTokens:           5,
		TotalTokens:           5,
		ProviderTokenEvidence: true,
		ExtraBilling: map[string]types.ExtraBilling{
			key: {ServiceType: key, Type: "medium", CallCount: 1},
		},
		ProviderExtraBilling: map[string]bool{key: true},
	}
	if err := observer.ObserveTurnUsage(trusted); err != nil {
		t.Fatal(err)
	}
	if observer.observedUsage.InputTokens != 5 || observer.observedUsage.TotalTokens != 5 || observer.observedUsage.ExtraBilling[key].CallCount != 1 {
		t.Fatalf("later provider markers authorized earlier untrusted values: %+v", observer.observedUsage)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: types.EventTypeResponseDone, Usage: trusted})
	wantCharge := 5 + saturatingCeilToInt(defaultExtraServicePrices.WebSearchGA*config.QuotaPerUnit)
	assertRealtimeUserQuota(t, 1000-wantCharge)
}

func TestRealtimeObservationAttemptDoesNotCapturePricePolicy(t *testing.T) {
	c := realtimeBillingFixture(t)
	originalPricing := model.PricingInstance
	model.PricingInstance = nil
	t.Cleanup(func() { model.PricingInstance = originalPricing })
	attempt, err := NewObservationAttemptQuota(c, "gpt-test", BillingAttemptSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if attempt.quota.price.Model != "" || attempt.quota.settlementPrice != nil || len(attempt.quota.billingDiagnostics) != 0 {
		t.Fatalf("observation attempt captured a price policy before settlement: %+v", attempt.quota)
	}
}

func TestRealtimeProviderInitiatedObservationSurvivesLiveDatabaseFailure(t *testing.T) {
	c := realtimeBillingFixture(t)
	db := model.DB
	model.DB = nil
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels()})
	model.DB = db
	if err == nil {
		t.Fatal("expected live principal refresh to report the database failure")
	}
	if observer.attempt == nil || !observer.attempt.SubmissionClaimed() || observer.attempt.ReservedQuota() != 0 {
		t.Fatalf("database failure erased already observed provider work: %+v", observer.attempt)
	}
}

func TestRealtimeRevokedProviderInitiatedObservationStillSettlesObservedWork(t *testing.T) {
	c := realtimeBillingFixture(t)
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("status", config.TokenStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels()}); err == nil {
		t.Fatal("expected revoked principal to reject future provider-initiated work")
	}
	if observer.attempt == nil || !observer.attempt.SubmissionClaimed() || observer.attempt.ReservedQuota() != 0 {
		t.Fatalf("revocation erased already observed provider work: %+v", observer.attempt)
	}
	usage := &types.UsageEvent{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, Source: types.UsageSourceRealtimeResponse, BillingBasis: types.UsageBillingBasisTokens, ProviderTokenEvidence: true}
	if err := observer.ObserveTurnUsage(usage); err != nil {
		t.Fatal(err)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "provider_initiated_admission_failed"})
	assertRealtimeUserQuota(t, 970)
}

func TestRealtimePostClaimFailureWithoutUsageCancelsReservation(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.AdmitBoundedTurn(boundedRealtimeAdmission()); err != nil {
		t.Fatal(err)
	}
	if err := observer.RollbackTurnAdmission("write_failed_after_claim"); err != nil {
		t.Fatal(err)
	}
	assertRealtimeUserQuota(t, 1000)
}

func TestRealtimeObserverFactoryCreatesIndependentTurnOwners(t *testing.T) {
	c := realtimeBillingFixture(t)
	factory := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)
	first, ok := factory().(*RealtimeTurnObserver)
	if !ok || first == nil {
		t.Fatalf("unexpected first observer: %#v", first)
	}
	second, ok := factory().(*RealtimeTurnObserver)
	if !ok || second == nil || first == second || first.requestContext == second.requestContext {
		t.Fatalf("factory did not create independent observers: first=%p second=%p", first, second)
	}
}

func TestRealtimeTurnAdmissionRevalidatesRevokedToken(t *testing.T) {
	c := realtimeBillingFixture(t)
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Update("status", config.TokenStatusDisabled).Error; err != nil {
		t.Fatal(err)
	}
	observer := &RealtimeTurnObserver{requestContext: c, models: realtimeTestModels()}
	if err := observer.AdmitBoundedTurn(boundedRealtimeAdmission()); err == nil {
		t.Fatal("revoked token started a new realtime work action")
	}
	assertRealtimeUserQuota(t, 1000)
}

func TestRealtimeObserverFactoryDetachesTurnFromAttachmentContext(t *testing.T) {
	c := realtimeBillingFixture(t)
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.Clone(requestCtx)
	factory := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)
	observer, ok := factory().(*RealtimeTurnObserver)
	if !ok || observer == nil {
		t.Fatalf("unexpected observer: %#v", observer)
	}
	cancel()
	if err := observer.AdmitBoundedTurn(boundedRealtimeAdmission()); err != nil {
		t.Fatalf("attachment cancellation leaked into turn admission: %v", err)
	}
	if err := observer.RollbackTurnAdmission("test_cleanup"); err != nil {
		t.Fatalf("detached turn cleanup: %v", err)
	}
}

func assertRealtimeUserQuota(t *testing.T, want int) {
	t.Helper()
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != want {
		t.Fatalf("user quota=%d, want %d", user.Quota, want)
	}
}

func TestRealtimeObservedWorkExhaustionStopsFutureWork(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		userQuota, tokenQuota int
		unlimited             bool
	}{
		{"user", 5, 100, false},
		{"token", 100, 5, false},
		{"unlimited token still checks user", 5, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			if err := model.DB.Model(&model.User{}).Where("id = ?", 1).Update("quota", tc.userQuota).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Model(&model.Token{}).Where("id = ?", 1).Updates(map[string]any{"remain_quota": tc.tokenQuota, "unlimited_quota": tc.unlimited}).Error; err != nil {
				t.Fatal(err)
			}
			observer := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)().(*RealtimeTurnObserver)
			if err := observer.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels(), WorkID: "work-1"}); err != nil {
				t.Fatal(err)
			}
			if err := observer.ObserveTurnUsage(&types.UsageEvent{InputTokens: 7, TotalTokens: 7, ProviderTokenEvidence: true}); err != nil {
				t.Fatal(err)
			}
			observer.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-1", TerminationReason: "response.done"})
			result := observer.FinalizationResult()
			if !result.StopFutureWork || result.Unsettled {
				t.Fatalf("exhaustion result=%+v", result)
			}
			assertRealtimeUserQuota(t, tc.userQuota-7)
			if err := observer.policy.CheckFutureWork(realtimeTestModels(), true); err == nil {
				t.Fatal("exhausted principal admitted later work")
			}
			observer.FinalizeTurn(runtimesession.TurnFinalizePayload{})
			assertRealtimeUserQuota(t, tc.userQuota-7)
		})
	}
}

func TestRealtimeAliasAuthorizationAndBillingRemainIndependent(t *testing.T) {
	for _, billingModel := range []string{"voice-public", "upstream-voice"} {
		t.Run(billingModel, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			c.Set("billing_original_model", billingModel == "voice-public")
			var token model.Token
			if err := model.DB.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			token.Setting.Set(model.TokenSetting{Limits: model.LimitsConfig{LimitModelSetting: model.LimitModelSetting{Enabled: true, Models: []string{"voice-public"}}}})
			if err := model.DB.Model(&token).Update("setting", token.Setting).Error; err != nil {
				t.Fatal(err)
			}
			model.PricingInstance.Prices["voice-public"] = &model.Price{Model: "voice-public", Type: model.TokensPriceType, Input: 1, Output: 1}
			model.PricingInstance.Prices["upstream-voice"] = &model.Price{Model: "upstream-voice", Type: model.TokensPriceType, Input: 2, Output: 2}
			models := runtimesession.ModelBinding{RequestedModel: "voice-public", ProviderModel: "upstream-voice", BillingModel: billingModel, BillingOriginal: billingModel == "voice-public"}
			policy := NewRealtimeWorkPolicy(c, models, func(requested string) (runtimesession.ModelBinding, error) {
				bound := models
				bound.RequestedModel = requested
				return bound, nil
			})
			if _, err := policy.ResolveModel("voice-public"); err != nil {
				t.Fatalf("public alias rejected: %v", err)
			}
			if _, err := policy.ResolveModel("upstream-voice"); err == nil {
				t.Fatal("provider name granted public permission")
			} else {
				var apiErr *types.OpenAIErrorWithStatusCode
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusForbidden || apiErr.Code != "permission_denied" {
					t.Fatalf("model permission denial lost its error semantics: %v", err)
				}
			}
			for _, manual := range []bool{false, true} {
				observer := NewRealtimeTurnObserverFactory(c, models, policy)().(*RealtimeTurnObserver)
				admission := runtimesession.TurnAdmission{Models: models, ExplicitClientCreate: manual}
				var err error
				if manual {
					err = observer.AdmitBoundedTurn(admission)
				} else {
					err = observer.ObserveProviderInitiatedTurn(admission)
				}
				if err != nil {
					t.Fatalf("manual=%v admission: %v", manual, err)
				}
				if observer.attempt.ModelName() != billingModel {
					t.Fatalf("billing model=%q", observer.attempt.ModelName())
				}
				if err := observer.ObserveTurnUsage(&types.UsageEvent{InputTokens: 7, TotalTokens: 7, ResponseModel: "upstream-voice", ProviderTokenEvidence: true}); err != nil {
					t.Fatal(err)
				}
				observer.FinalizeTurn(runtimesession.TurnFinalizePayload{Models: runtimesession.ModelBinding{ReportedModel: "other-reported-model"}})
			}
			wantCharge := 14
			if billingModel == "upstream-voice" {
				wantCharge = 28
			}
			assertRealtimeUserQuota(t, 1000-wantCharge)
		})
	}
}

func TestRealtimeSharedWorkAuthorizationDoesNotCountTwice(t *testing.T) {
	c := realtimeBillingFixture(t)
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter = map[string]limit.RateLimiter{"paid": limit.NewMemoryLimiter(1, 1, time.Minute, false)}
	model.GlobalUserGroupRatio.Unlock()
	models := realtimeTestModels()
	policy := NewRealtimeWorkPolicy(c, models, nil)
	if err := policy.CheckFutureWork(models, false); err != nil {
		t.Fatal(err)
	}
	if err := policy.CheckFutureWork(models, true); err != nil {
		t.Fatal(err)
	}
	factory := NewRealtimeTurnObserverFactory(c, models, policy)
	transcription := factory().(*RealtimeTurnObserver)
	if err := transcription.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: models, WorkID: "input-1", WorkAuthorized: true, Transcription: true}); err != nil {
		t.Fatalf("authorized transcription: %v", err)
	}
	response := factory().(*RealtimeTurnObserver)
	if err := response.ObserveProviderInitiatedTurn(runtimesession.TurnAdmission{Models: models, WorkID: "input-1", WorkAuthorized: true}); err != nil {
		t.Fatalf("associated response consumed a second permit: %v", err)
	}
	transcription.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "transcription.failed"})
	response.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "response.done"})
	if err := policy.CheckFutureWork(models, true); err == nil {
		t.Fatal("second logical work bypassed RPM")
	}
	assertRealtimeUserQuota(t, 1000)
}

func TestRealtimeTranscriptionCommitUnknownIsCachedAndNeverReplayed(t *testing.T) {
	c := realtimeBillingFixture(t)
	core, recorded := zapobserver.New(zap.ErrorLevel)
	logger.Logger = zap.New(core)
	observer := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)().(*RealtimeTurnObserver)
	guarded := runtimesession.GuardTurnObserver(observer)
	if err := runtimesession.AdmitBoundedTurn(guarded, runtimesession.TurnAdmission{Models: realtimeTestModels(), Transcription: true, WorkID: "input-1", InputItemID: "item-1"}); err != nil {
		t.Fatal(err)
	}
	if err := guarded.ObserveTurnUsage(&types.UsageEvent{InputTokens: 7, TotalTokens: 7, ProviderTokenEvidence: true}); err != nil {
		t.Fatal(err)
	}
	originalApply := applyBillingSettlement
	var calls atomic.Int32
	applyBillingSettlement = func(ctx context.Context, cmd billing.SettlementCommand, opts *billing.SettlementOptions) (billing.SettlementResult, error) {
		calls.Add(1)
		result, err := originalApply(ctx, cmd, opts)
		if err != nil {
			return result, err
		}
		return billing.SettlementResult{TruthApplied: true, BalanceOutcome: model.BillingBalanceCommitUnknown}, errors.New("commit acknowledgement lost")
	}
	t.Cleanup(func() { applyBillingSettlement = originalApply })
	guarded.FinalizeTurn(runtimesession.TurnFinalizePayload{SessionID: "session-1", TerminationReason: "transcription.completed"})
	first := runtimesession.TurnResult(guarded)
	if !first.Unsettled || first.Err == nil || !first.StopFutureWork {
		t.Fatalf("unknown commit appears settled: %+v", first)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			guarded.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "connection_closed"})
			_ = runtimesession.RollbackTurnAdmission(guarded, "transcription.failed")
		}()
	}
	workers.Wait()
	if got := runtimesession.TurnResult(guarded); got != first {
		t.Fatalf("cached result changed: first=%+v got=%+v", first, got)
	}
	if calls.Load() != 1 {
		t.Fatalf("unknown commit replayed %d times", calls.Load())
	}
	assertRealtimeUserQuota(t, 993)
	entries := recorded.FilterMessageSnippet("realtime billing finalization:").All()
	if len(entries) != 1 {
		t.Fatalf("diagnostic count=%d", len(entries))
	}
	for _, want := range []string{`"session_id":"session-1"`, `"work_id":"input-1"`, `"input_item_id":"item-1"`, `"unsettled":true`, `"error_class":"commit_unknown"`, `"charged_quota":7`} {
		if !strings.Contains(entries[0].Message, want) {
			t.Fatalf("diagnostic missing %s: %s", want, entries[0].Message)
		}
	}
}

func TestRealtimeTranscriptionRollbackFailureRemainsUnsettledAfterBoundedAttempts(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)().(*RealtimeTurnObserver)
	if err := observer.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: realtimeTestModels(), Transcription: true, WorkID: "input-1"}); err != nil {
		t.Fatal(err)
	}
	originalRefund := applyBillingRefund
	var calls int
	applyBillingRefund = func(ctx context.Context, userID, tokenID int, tokenApplied bool, amount int64) (model.BillingBalanceResult, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok || ctx.Err() != nil {
			t.Errorf("cleanup context must be live and bounded")
		}
		return model.BillingBalanceResult{Outcome: model.BillingBalanceDefinitelyRolledBack}, errors.New("database unavailable")
	}
	t.Cleanup(func() { applyBillingRefund = originalRefund })
	if err := observer.RollbackTurnAdmission("not_sent"); err == nil {
		t.Fatal("refund failure was hidden")
	}
	first := observer.FinalizationResult()
	if !first.Unsettled || first.Err == nil || !first.StopFutureWork {
		t.Fatalf("refund result=%+v", first)
	}
	if err := observer.RollbackTurnAdmission("duplicate"); err != first.Err {
		t.Fatalf("did not preserve original error: %v", err)
	}
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{})
	if calls != 2 {
		t.Fatalf("bounded rollback attempted %d times", calls)
	}
}

func TestRealtimeTranscriptionReserveUnknownRetainsUnsettledOwner(t *testing.T) {
	c := realtimeBillingFixture(t)
	observer := NewRealtimeTurnObserverFactory(c, realtimeTestModels(), nil)().(*RealtimeTurnObserver)
	originalReserve, originalRefund := applyBillingReserve, applyBillingRefund
	var reserves, refunds int
	applyBillingReserve = func(ctx context.Context, userID, tokenID int, amount int64) (model.BillingBalanceResult, error) {
		reserves++
		result, err := originalReserve(ctx, userID, tokenID, amount)
		if err != nil {
			return result, err
		}
		return model.BillingBalanceResult{Outcome: model.BillingBalanceCommitUnknown, CommitAttempted: true}, errors.New("reserve acknowledgement lost")
	}
	applyBillingRefund = func(ctx context.Context, userID, tokenID int, tokenApplied bool, amount int64) (model.BillingBalanceResult, error) {
		refunds++
		return originalRefund(ctx, userID, tokenID, tokenApplied, amount)
	}
	t.Cleanup(func() { applyBillingReserve, applyBillingRefund = originalReserve, originalRefund })
	admission := runtimesession.TurnAdmission{Models: realtimeTestModels(), SessionID: "session-1", Transcription: true, WorkID: "input-1"}
	if err := observer.AdmitBoundedTurn(admission); err == nil {
		t.Fatal("unknown reserve was admitted")
	}
	first := observer.FinalizationResult()
	if observer.attempt == nil || !first.Unsettled || first.Err == nil || !first.StopFutureWork {
		t.Fatalf("unknown reserve owner lost: %+v", first)
	}
	_ = observer.RollbackTurnAdmission("not_sent")
	observer.FinalizeTurn(runtimesession.TurnFinalizePayload{})
	if err := observer.AdmitBoundedTurn(admission); err == nil {
		t.Fatal("unknown reserve owner was reopened")
	}
	if reserves != 1 || refunds != 0 {
		t.Fatalf("unknown reserve replayed or guessed refund: reserves=%d refunds=%d", reserves, refunds)
	}
	if got := observer.FinalizationResult(); got != first {
		t.Fatalf("unknown reserve final result changed: %+v", got)
	}
}

func TestRealtimeTranscriptionKeepsItsOwnOriginalModelBillingPolicy(t *testing.T) {
	for _, tc := range []struct {
		name            string
		sessionOriginal bool
		turnOriginal    bool
		sameModelName   bool
	}{
		{name: "transcription uses original model", turnOriginal: true},
		{name: "transcription allows reported model", sessionOriginal: true},
		{name: "same name mapping still preserves original policy", turnOriginal: true, sameModelName: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := realtimeBillingFixture(t)
			c.Set("billing_original_model", tc.sessionOriginal)
			sessionModels := realtimeTestModels()
			sessionModels.BillingOriginal = tc.sessionOriginal
			publicModel, providerModel := "transcription-public", "transcription-upstream"
			if tc.sameModelName {
				providerModel = publicModel
			}
			for name, rate := range map[string]float64{publicModel: 1, "transcription-upstream": 2, "transcription-reported": 3} {
				extraRatios := datatypes.NewJSONType(map[string]float64{config.UsageExtraInputAudioTranscription: 1})
				model.PricingInstance.Prices[name] = &model.Price{Model: name, Type: model.TokensPriceType, Input: rate, Output: rate, ExtraRatios: &extraRatios}
			}
			models := runtimesession.ModelBinding{RequestedModel: publicModel, ProviderModel: providerModel, BillingModel: providerModel, BillingOriginal: tc.turnOriginal}
			if tc.turnOriginal {
				models.BillingModel = publicModel
			}
			observer := NewRealtimeTurnObserverFactory(c, sessionModels, nil)().(*RealtimeTurnObserver)
			if err := observer.AdmitBoundedTurn(runtimesession.TurnAdmission{Models: models, Transcription: true, WorkID: "input-1"}); err != nil {
				t.Fatal(err)
			}
			usage := &types.UsageEvent{Source: types.UsageSourceInputAudioTranscription, ResponseModel: "transcription-reported"}
			usage.SetProviderIndependentUsageUnit(config.UsageExtraInputAudioTranscription, 7)
			if err := observer.ObserveTurnUsage(usage); err != nil {
				t.Fatal(err)
			}
			observer.FinalizeTurn(runtimesession.TurnFinalizePayload{TerminationReason: "transcription.completed"})
			if result := observer.FinalizationResult(); result.Unsettled || result.Err != nil {
				t.Fatalf("transcription finalization=%+v", result)
			}
			wantCharge := 21
			if tc.turnOriginal {
				wantCharge = 7
			}
			assertRealtimeUserQuota(t, 1000-wantCharge)
		})
	}
}
