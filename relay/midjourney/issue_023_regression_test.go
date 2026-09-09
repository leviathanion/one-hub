package midjourney

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/requester"
	"one-api/middleware"
	"one-api/model"
	provider "one-api/providers/midjourney"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestFixI023_LegacyFingerprintOwnerIsReusedAndUpgraded(t *testing.T) {
	const (
		rawBody        = `{"taskId":"parent-task","action":"UPSCALE","index":1}`
		providerTaskID = "legacy-existing"
	)
	for _, test := range []struct {
		name        string
		closeBefore bool
		wantBalance int
		wantState   model.TaskProviderState
	}{
		{name: "accepted_owner", wantBalance: 980, wantState: model.TaskProviderStateAccepted},
		{name: "closed_owner", closeBefore: true, wantBalance: 1000, wantState: model.TaskProviderStateClosed},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":21,"description":"task exists","result":"legacy-existing","properties":{"status":"SUCCESS","imageUrl":"https://cdn.example/legacy.png"}}`))
			}))
			t.Cleanup(upstream.Close)
			oldHTTPClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

			db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
			legacyOwner := &model.Task{
				Platform:                     model.TaskPlatformMidjourney,
				UserId:                       1,
				TokenID:                      1,
				ChannelId:                    1,
				Action:                       provider.MjActionUpscale,
				Status:                       model.TaskStatusSubmitted,
				ReservedQuota:                20,
				Data:                         model.EncodeMidjourneyTaskData(&model.Midjourney{Action: provider.MjActionUpscale, Mode: "fast"}),
				ProviderNamespace:            "task-platform:midjourney",
				ProviderTaskScopeIncarnation: "provider-wide",
				RequestFingerprint:           legacyI023Fingerprint("/mj/submit/change", []byte(rawBody)),
			}
			if result, err := model.CreateTaskBillingOwner(context.Background(), legacyOwner); err != nil || result.Outcome != model.BillingBalanceCommitted {
				t.Fatalf("create legacy owner: result=%+v err=%v", result, err)
			}
			if result, err := model.ClaimTaskSubmission(context.Background(), legacyOwner, "legacy-claim"); err != nil || result.Outcome != model.TaskMutationApplied {
				t.Fatalf("claim legacy owner: result=%+v err=%v", result, err)
			}
			if result, err := model.AcceptTaskSubmission(context.Background(), legacyOwner, providerTaskID); err != nil || result.Outcome != model.TaskMutationApplied {
				t.Fatalf("accept legacy owner: result=%+v err=%v", result, err)
			}
			if test.closeBefore {
				legacyOwner.Status = model.TaskStatusSuccess
				legacyOwner.Progress = 100
				if _, err := model.FinalizeTaskBillingOwner(context.Background(), legacyOwner, 0, "cancel"); err != nil {
					t.Fatalf("close legacy owner: %v", err)
				}
			}

			router := gin.New()
			router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/mj/submit/change", strings.NewReader(rawBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("mj-api-secret", credential)
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), providerTaskID) || strings.Contains(recorder.Body.String(), "persist_midjourney_acceptance_failed") {
				t.Fatalf("legacy owner was not reused: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("legacy retry must reach upstream once: calls=%d", calls.Load())
			}

			var owners []model.Task
			if err := db.Where("platform = ? AND action = ?", model.TaskPlatformMidjourney, provider.MjActionUpscale).Order("id").Find(&owners).Error; err != nil {
				t.Fatal(err)
			}
			if len(owners) != 2 {
				t.Fatalf("legacy retry created unexpected owners: %+v", owners)
			}
			var existing, canceled *model.Task
			for i := range owners {
				owner := &owners[i]
				switch {
				case model.TaskProviderID(owner) == providerTaskID:
					existing = owner
				case owner.ProviderState == model.TaskProviderStateClosed && owner.SettlementDecision == "cancel":
					canceled = owner
				}
			}
			if existing == nil || canceled == nil || existing.ProviderState != test.wantState {
				t.Fatalf("legacy owner state was not retained: existing=%+v canceled=%+v", existing, canceled)
			}
			expectedCtx := newI023FingerprintContext(t, rawBody, 1, 1)
			expectedFingerprint := midjourneyTaskRequestFingerprint(expectedCtx, &model.Midjourney{Action: provider.MjActionUpscale, Mode: "fast"})
			if existing.RequestFingerprint != expectedFingerprint || existing.RequestFingerprint == legacyOwner.RequestFingerprint {
				t.Fatalf("legacy owner fingerprint was not upgraded on reuse: got=%q want=%q old=%q", existing.RequestFingerprint, expectedFingerprint, legacyOwner.RequestFingerprint)
			}
			if canceled.TaskID != nil || canceled.ChargedQuota == nil || *canceled.ChargedQuota != 0 {
				t.Fatalf("provisional legacy retry owner was not refunded: %+v", canceled)
			}

			var user model.User
			var token model.Token
			if err := db.First(&user, 1).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&token, 1).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != test.wantBalance || token.RemainQuota != test.wantBalance {
				t.Fatalf("legacy retry changed settlement balance: user=%d token=%d want=%d", user.Quota, token.RemainQuota, test.wantBalance)
			}
		})
	}
}

func legacyI023Fingerprint(path string, requestBody []byte) string {
	digest := sha256.Sum256(append([]byte(path+"\x00"), requestBody...))
	return hex.EncodeToString(digest[:])
}

func TestFixI023_LegacyFingerprintDoesNotCrossOwnerScope(t *testing.T) {
	const rawBody = `{"taskId":"parent-task","action":"UPSCALE","index":1}`
	for _, test := range []struct {
		name          string
		oldUserID     int
		oldTokenID    int
		oldChannelID  int
		oldAction     string
		wantOldUser   int
		wantOldRemain int
		wantCurrent   int
	}{
		{name: "different_user", oldUserID: 2, oldTokenID: 2, oldChannelID: 1, oldAction: provider.MjActionUpscale, wantOldUser: 2, wantOldRemain: 980, wantCurrent: 1000},
		{name: "different_channel", oldUserID: 1, oldTokenID: 1, oldChannelID: 2, oldAction: provider.MjActionUpscale, wantOldUser: 1, wantOldRemain: 980, wantCurrent: 980},
		{name: "different_action", oldUserID: 1, oldTokenID: 1, oldChannelID: 1, oldAction: provider.MjActionVariation, wantOldUser: 1, wantOldRemain: 980, wantCurrent: 980},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"code":21,"description":"task exists","result":"scope-existing"}`))
			}))
			t.Cleanup(upstream.Close)
			oldHTTPClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

			db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
			if test.oldUserID == 2 {
				if err := db.Create(&model.User{Id: 2, Username: "i023-other", Password: "test-password", Status: config.UserStatusEnabled, Group: "paid", Quota: 1000}).Error; err != nil {
					t.Fatal(err)
				}
				otherKey := strings.Repeat("o", 48)
				if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 2, UserId: 2, Key: otherKey, Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
					t.Fatal(err)
				}
			}
			if test.oldChannelID == 2 {
				proxy := ""
				if err := db.Create(&model.Channel{Id: 2, Type: config.ChannelTypeMidjourney, Name: "i023-other-channel", Key: "i023-other-secret", Group: "paid", Models: "mj_upscale,mj_variation", Status: config.ChannelStatusEnabled, BaseURL: &upstream.URL, Proxy: &proxy}).Error; err != nil {
					t.Fatal(err)
				}
			}

			oldOwner := createI023LegacyAcceptedOwner(t, db, rawBody, test.oldUserID, test.oldTokenID, test.oldChannelID, test.oldAction, "scope-existing")
			router := gin.New()
			router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/mj/submit/change", strings.NewReader(rawBody))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("mj-api-secret", credential)
			router.ServeHTTP(recorder, request)

			if recorder.Code < http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "persist_midjourney_acceptance_failed") {
				t.Fatalf("legacy fingerprint crossed owner scope: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if calls.Load() != 1 {
				t.Fatalf("scope conflict must not retry upstream: calls=%d", calls.Load())
			}
			var durable model.Task
			if err := db.Where("id = ?", oldOwner.ID).First(&durable).Error; err != nil {
				t.Fatal(err)
			}
			if durable.RequestFingerprint != oldOwner.RequestFingerprint || durable.ProviderState != model.TaskProviderStateAccepted {
				t.Fatalf("scope conflict changed the legacy owner: before=%+v after=%+v", oldOwner, durable)
			}
			var user model.User
			if err := db.First(&user, test.wantOldUser).Error; err != nil {
				t.Fatal(err)
			}
			if user.Quota != test.wantOldRemain {
				t.Fatalf("scope conflict changed old owner balance: user=%d quota=%d want=%d", test.wantOldUser, user.Quota, test.wantOldRemain)
			}
			var currentUser model.User
			if err := db.First(&currentUser, 1).Error; err != nil {
				t.Fatal(err)
			}
			if currentUser.Quota != test.wantCurrent {
				t.Fatalf("scope conflict changed current user balance: quota=%d want=%d", currentUser.Quota, test.wantCurrent)
			}
		})
	}
}

func createI023LegacyAcceptedOwner(t *testing.T, db *gorm.DB, rawBody string, userID, tokenID, channelID int, action, providerTaskID string) *model.Task {
	t.Helper()
	owner := &model.Task{
		Platform:                     model.TaskPlatformMidjourney,
		UserId:                       userID,
		TokenID:                      tokenID,
		ChannelId:                    channelID,
		Action:                       action,
		Status:                       model.TaskStatusSubmitted,
		ReservedQuota:                20,
		Data:                         model.EncodeMidjourneyTaskData(&model.Midjourney{Action: action, Mode: "fast"}),
		ProviderNamespace:            "task-platform:midjourney",
		ProviderTaskScopeIncarnation: "provider-wide",
		RequestFingerprint:           legacyI023Fingerprint("/mj/submit/change", []byte(rawBody)),
	}
	if result, err := model.CreateTaskBillingOwner(context.Background(), owner); err != nil || result.Outcome != model.BillingBalanceCommitted {
		t.Fatalf("create scoped legacy owner: result=%+v err=%v", result, err)
	}
	if result, err := model.ClaimTaskSubmission(context.Background(), owner, "legacy-scope-claim"); err != nil || result.Outcome != model.TaskMutationApplied {
		t.Fatalf("claim scoped legacy owner: result=%+v err=%v", result, err)
	}
	if result, err := model.AcceptTaskSubmission(context.Background(), owner, providerTaskID); err != nil || result.Outcome != model.TaskMutationApplied {
		t.Fatalf("accept scoped legacy owner: result=%+v err=%v", result, err)
	}
	return owner
}

func TestFixI023_MidjourneyEquivalentJSONReusesExistingTask(t *testing.T) {
	const (
		firstBody      = `{"taskId":"parent-task","action":"UPSCALE","index":1}`
		secondBody     = "{\n  \"index\": 1,\n  \"action\": \"UPSCALE\",\n  \"taskId\": \"parent-task\"\n}"
		providerTaskID = "u1-existing"
	)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if call == 1 {
			_, _ = w.Write([]byte(`{"code":1,"description":"accepted","result":"u1-existing"}`))
			return
		}
		_, _ = w.Write([]byte(`{"code":21,"description":"task exists","result":"u1-existing","properties":{"status":"SUCCESS","imageUrl":"https://cdn.example/result.png"}}`))
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	db, credential := setupI043Midjourney(t, upstream.URL, "current_group")

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	submit := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/mj/submit/change", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("mj-api-secret", credential)
		router.ServeHTTP(recorder, request)
		return recorder
	}

	first := submit(firstBody)
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), providerTaskID) {
		t.Fatalf("first U1 submit failed: status=%d body=%s", first.Code, first.Body.String())
	}
	second := submit(secondBody)
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), providerTaskID) || strings.Contains(second.Body.String(), "persist_midjourney_acceptance_failed") {
		t.Fatalf("equivalent U1 was not served from the existing task: status=%d body=%s", second.Code, second.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("each client submit should make one upstream call: calls=%d", calls.Load())
	}

	var owners []model.Task
	if err := db.Where("platform = ? AND action = ?", model.TaskPlatformMidjourney, provider.MjActionUpscale).Order("id").Find(&owners).Error; err != nil {
		t.Fatal(err)
	}
	if len(owners) != 2 {
		t.Fatalf("expected one accepted owner and one canceled provisional owner, got %d: %+v", len(owners), owners)
	}
	var accepted, canceled int
	for _, owner := range owners {
		switch {
		case model.TaskProviderID(&owner) == providerTaskID:
			accepted++
			if owner.ProviderState != model.TaskProviderStateAccepted || owner.AcceptanceRecordedAt == nil {
				t.Fatalf("existing provider task was not retained as the accepted owner: %+v", owner)
			}
		case owner.ProviderState == model.TaskProviderStateClosed && owner.SettlementDecision == "cancel":
			canceled++
			if owner.TaskID != nil || owner.ChargedQuota == nil || *owner.ChargedQuota != 0 {
				t.Fatalf("equivalent duplicate provisional owner was not refunded exactly once: %+v", owner)
			}
		}
	}
	if accepted != 1 || canceled != 1 {
		t.Fatalf("duplicate request created an unexpected owner/settlement split: accepted=%d canceled=%d owners=%+v", accepted, canceled, owners)
	}
	if owners[0].RequestFingerprint != owners[1].RequestFingerprint {
		t.Fatalf("equivalent JSON requests produced different fingerprints: %q != %q", owners[0].RequestFingerprint, owners[1].RequestFingerprint)
	}

	var user model.User
	var token model.Token
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 980 || token.RemainQuota != 980 {
		t.Fatalf("equivalent duplicate changed settlement balance: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}

func TestFixI023_MidjourneyFingerprintPreservesSemanticBoundaries(t *testing.T) {
	baseView := func(action string) *model.Midjourney {
		return &model.Midjourney{UserId: 1, ChannelId: 1, Action: action, Mode: "fast"}
	}
	tests := []struct {
		name       string
		bodyA      string
		bodyB      string
		viewA      *model.Midjourney
		viewB      *model.Midjourney
		userA      int
		userB      int
		channelA   int
		channelB   int
		equivalent bool
	}{
		{
			name:       "object_key_order_and_whitespace",
			bodyA:      `{"taskId":"parent-task","action":"UPSCALE","index":1,"params":{"mode":"fast","seed":7}}`,
			bodyB:      "{ \"params\": { \"seed\": 7, \"mode\": \"fast\" }, \"index\": 1, \"action\": \"UPSCALE\", \"taskId\": \"parent-task\" }",
			viewA:      baseView(provider.MjActionUpscale),
			viewB:      baseView(provider.MjActionUpscale),
			userA:      1,
			userB:      1,
			channelA:   1,
			channelB:   1,
			equivalent: true,
		},
		{
			name:     "array_order",
			bodyA:    `{"taskId":"parent-task","action":"UPSCALE","index":1,"images":["one","two"]}`,
			bodyB:    `{"taskId":"parent-task","action":"UPSCALE","index":1,"images":["two","one"]}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "large_number_precision",
			bodyA:    `{"taskId":"parent-task","action":"UPSCALE","index":1,"seed":9007199254740993}`,
			bodyB:    `{"taskId":"parent-task","action":"UPSCALE","index":1,"seed":9007199254740992}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "parent_task",
			bodyA:    `{"taskId":"parent-one","action":"UPSCALE","index":1}`,
			bodyB:    `{"taskId":"parent-two","action":"UPSCALE","index":1}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "action",
			bodyA:    `{"taskId":"parent-task","index":1}`,
			bodyB:    `{"taskId":"parent-task","index":1}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionVariation),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "parameter",
			bodyA:    `{"taskId":"parent-task","action":"UPSCALE","index":1}`,
			bodyB:    `{"taskId":"parent-task","action":"UPSCALE","index":2}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "user",
			bodyA:    `{"taskId":"parent-task","action":"UPSCALE","index":1}`,
			bodyB:    `{"taskId":"parent-task","action":"UPSCALE","index":1}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    2,
			channelA: 1,
			channelB: 1,
		},
		{
			name:     "channel",
			bodyA:    `{"taskId":"parent-task","action":"UPSCALE","index":1}`,
			bodyB:    `{"taskId":"parent-task","action":"UPSCALE","index":1}`,
			viewA:    baseView(provider.MjActionUpscale),
			viewB:    baseView(provider.MjActionUpscale),
			userA:    1,
			userB:    1,
			channelA: 1,
			channelB: 2,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctxA := newI023FingerprintContext(t, test.bodyA, test.userA, test.channelA)
			ctxB := newI023FingerprintContext(t, test.bodyB, test.userB, test.channelB)
			before, ok := common.GetCanonicalRequestBody(ctxA)
			if !ok {
				t.Fatal("request body was not cached")
			}
			fingerprintA := midjourneyTaskRequestFingerprint(ctxA, test.viewA)
			fingerprintB := midjourneyTaskRequestFingerprint(ctxB, test.viewB)
			if (fingerprintA == fingerprintB) != test.equivalent {
				t.Fatalf("fingerprint equivalence=%v, want %v: %q vs %q", fingerprintA == fingerprintB, test.equivalent, fingerprintA, fingerprintB)
			}
			after, ok := common.GetCanonicalRequestBody(ctxA)
			if !ok || !bytes.Equal(before, after) {
				t.Fatalf("fingerprint construction changed the request body: before=%q after=%q", before, after)
			}
		})
	}
}

func newI023FingerprintContext(t *testing.T, body string, userID, channelID int) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/mj/submit/change", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Set("id", userID)
	ctx.Set("channel_id", channelID)
	if _, err := common.CacheRequestBody(ctx); err != nil {
		t.Fatalf("cache request body: %v", err)
	}
	return ctx
}
