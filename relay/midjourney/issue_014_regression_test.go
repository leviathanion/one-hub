package midjourney

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/requester"
	"one-api/middleware"
	"one-api/model"
	provider "one-api/providers/midjourney"
	taskbase "one-api/relay/task/base"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestFixI014_MidjourneyUploadURLArrayCompletesSameOwner(t *testing.T) {
	for _, test := range []struct {
		name  string
		input float64
		want  int64
	}{
		{name: "explicit_prehold", input: 0.05, want: 100},
		{name: "default_zero_price", input: 0, want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			const rawResponse = `{"code":1,"description":"success","result":["https://cdn.discordapp.com/one.png","https://cdn.discordapp.com/two.png"]}`
			var calls atomic.Int32
			var observedUserQuota, observedTokenQuota atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var user model.User
				var token model.Token
				if model.DB != nil && model.DB.First(&user, 1).Error == nil {
					observedUserQuota.Store(int64(user.Quota))
				}
				if model.DB != nil && model.DB.First(&token, 1).Error == nil {
					observedTokenQuota.Store(int64(token.RemainQuota))
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(rawResponse))
			}))
			t.Cleanup(upstream.Close)
			oldHTTPClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

			db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
			addI014ModelSupport(t, db, provider.MjActionUpload, test.input)

			router := gin.New()
			router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/mj/submit/upload-discord-images", strings.NewReader(`{"base64Array":["data:image/png;base64,aW1hZ2U="]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("mj-api-secret", credential)
			router.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusOK {
				t.Fatalf("upload URL array was rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if !bytes.Equal(recorder.Body.Bytes(), []byte(rawResponse)) {
				t.Fatalf("upload response body was changed: got=%s want=%s", recorder.Body.Bytes(), rawResponse)
			}
			if calls.Load() != 1 {
				t.Fatalf("upload must be sent exactly once: calls=%d", calls.Load())
			}
			wantDuringUpload := int64(1000) - test.want
			if observedUserQuota.Load() != wantDuringUpload || observedTokenQuota.Load() != wantDuringUpload {
				t.Fatalf("upload was not observed after the expected pre-consumption: user=%d token=%d want=%d", observedUserQuota.Load(), observedTokenQuota.Load(), wantDuringUpload)
			}

			owner := loadI014UploadOwner(t, db)
			if owner.ProviderState != model.TaskProviderStateClosed || owner.Status != model.TaskStatusSuccess || owner.Progress != 100 || owner.NextActionAt != 0 {
				t.Fatalf("upload owner was not synchronously closed: %+v", owner)
			}
			if owner.TaskID != nil {
				t.Fatalf("synchronous upload fabricated provider task id: %q", model.TaskProviderID(&owner))
			}
			if owner.ReservedQuota != test.want || owner.ChargedQuota == nil || *owner.ChargedQuota != 0 || owner.SettlementDecision != "cancel" {
				t.Fatalf("upload owner settlement is wrong: reserved=%d charged=%v decision=%q", owner.ReservedQuota, owner.ChargedQuota, owner.SettlementDecision)
			}
			view := model.MidjourneyFromTask(&owner)
			if view.Action != provider.MjActionUpload || view.Code != 1 || view.Description != "success" || view.Status != string(model.TaskStatusSuccess) || view.Progress != "100%" {
				t.Fatalf("upload view was not saved: %+v", view)
			}

			assertI014Balances(t, db, 1000)
			if _, err := taskbase.FinalizeTaskSettlement(context.Background(), &owner); err != nil {
				t.Fatalf("repeated upload finalization failed: %v", err)
			}
			assertI014Balances(t, db, 1000)
			closed := loadI014UploadOwner(t, db)
			if closed.ProviderState != model.TaskProviderStateClosed || closed.NextActionAt != 0 || closed.ChargedQuota == nil || *closed.ChargedQuota != 0 {
				t.Fatalf("repeated finalization reopened or changed upload owner: %+v", closed)
			}
		})
	}
}

func TestFixI014_MidjourneyUploadInvalidResultDoesNotFakeSuccess(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "empty_array", body: `{"code":1,"description":"success","result":[]}`},
		{name: "empty_body", body: ``},
		{name: "empty_url", body: `{"code":1,"description":"success","result":[""]}`},
		{name: "invalid_url", body: `{"code":1,"description":"success","result":["not-a-url"]}`},
		{name: "scalar_result", body: `{"code":1,"description":"success","result":"https://cdn.discordapp.com/image.png"}`},
		{name: "provider_failure", body: `{"code":2,"description":"rejected","result":[]}`},
		{name: "provider_code_21", body: `{"code":21,"description":"already queued","result":[]}`},
		{name: "provider_code_22", body: `{"code":22,"description":"queued","result":[]}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			t.Cleanup(upstream.Close)
			oldHTTPClient := requester.HTTPClient
			requester.HTTPClient = upstream.Client()
			t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

			db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
			addI014ModelSupport(t, db, provider.MjActionUpload, 0.05)

			router := gin.New()
			router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/mj/submit/upload-discord-images", strings.NewReader(`{"base64Array":["data:image/png;base64,aW1hZ2U="]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("mj-api-secret", credential)
			router.ServeHTTP(recorder, request)

			if calls.Load() != 1 {
				t.Fatalf("invalid upload must not be retried: calls=%d", calls.Load())
			}
			owner := loadI014UploadOwner(t, db)
			if owner.ProviderState != model.TaskProviderStateClosed || owner.Status == model.TaskStatusSuccess || owner.NextActionAt != 0 {
				t.Fatalf("invalid upload became a successful or pollable owner: %+v", owner)
			}
			if owner.ChargedQuota == nil || *owner.ChargedQuota != 0 || owner.SettlementDecision != "cancel" {
				t.Fatalf("invalid upload did not cancel its reservation: charged=%v decision=%q", owner.ChargedQuota, owner.SettlementDecision)
			}
			assertI014Balances(t, db, 1000)
			if (test.name == "scalar_result" || test.name == "invalid_url") && !strings.Contains(recorder.Body.String(), "midjourney_upload_response_invalid") {
				t.Fatalf("invalid upload result was exposed as success: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if (test.name == "provider_code_21" || test.name == "provider_code_22") && !bytes.Equal(recorder.Body.Bytes(), []byte(test.body)) {
				t.Fatalf("upload provider error response was rewritten: got=%s want=%s", recorder.Body.Bytes(), test.body)
			}
		})
	}
}

func TestFixI014_MidjourneyUploadSuccessFinalizesAfterRequestCancellation(t *testing.T) {
	var calls atomic.Int32
	requestContext, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"code":1,"description":"success","result":["https://cdn.discordapp.com/image.png"]}`))
		cancelRequest()
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
	addI014ModelSupport(t, db, provider.MjActionUpload, 0.05)

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mj/submit/upload-discord-images", strings.NewReader(`{"base64Array":["data:image/png;base64,aW1hZ2U="]}`)).WithContext(requestContext)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("mj-api-secret", credential)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("successful upload was lost after request cancellation: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("canceled upload was retried: calls=%d", calls.Load())
	}
	owner := loadI014UploadOwner(t, db)
	if owner.ProviderState != model.TaskProviderStateClosed || owner.Status != model.TaskStatusSuccess || owner.NextActionAt != 0 || owner.ChargedQuota == nil || *owner.ChargedQuota != 0 {
		t.Fatalf("canceled upload was not finalized: %+v", owner)
	}
	assertI014Balances(t, db, 1000)
}

func TestFixI014_FinalizationFailureDoesNotResubmitUpload(t *testing.T) {
	var calls atomic.Int32
	var db *gorm.DB
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		model.DB = nil
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"description":"success","result":["https://cdn.discordapp.com/image.png"]}`))
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	var credential string
	db, credential = setupI043Midjourney(t, upstream.URL, "current_group")
	addI014ModelSupport(t, db, provider.MjActionUpload, 0.05)

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mj/submit/upload-discord-images", strings.NewReader(`{"base64Array":["data:image/png;base64,aW1hZ2U="]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("mj-api-secret", credential)
	func() {
		defer func() { model.DB = db }()
		router.ServeHTTP(recorder, request)
	}()

	if calls.Load() != 1 {
		t.Fatalf("finalization failure caused upload retry: calls=%d", calls.Load())
	}
	if recorder.Code < http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "finalize_midjourney_upload_failed") {
		t.Fatalf("finalization failure was not reported: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	owner := loadI014UploadOwner(t, db)
	if owner.ProviderState == model.TaskProviderStateClosed || owner.Status == model.TaskStatusSuccess {
		t.Fatalf("failed finalization falsely closed upload owner: %+v", owner)
	}
}

func TestFixI014_AsyncMidjourneyStillRequiresProviderTaskID(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"description":"accepted","result":""}`))
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
	addI014ModelSupport(t, db, provider.MjActionImagine, 0.01)

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mj/submit/imagine", strings.NewReader(`{"prompt":"a cat"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("mj-api-secret", credential)
	router.ServeHTTP(recorder, request)

	if calls.Load() != 1 {
		t.Fatalf("async task missing ID was unexpectedly retried: calls=%d", calls.Load())
	}
	if recorder.Code < http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "provider_task_id_missing") {
		t.Fatalf("async missing task ID was accepted: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func addI014ModelSupport(t *testing.T, db *gorm.DB, action string, input float64) {
	t.Helper()
	if err := db.Model(&model.Channel{}).Where("id = ?", 1).Update("models", "mj_upscale,mj_modal,mj_variation,mj_"+strings.ToLower(action)).Error; err != nil {
		t.Fatalf("enable Midjourney %s model: %v", action, err)
	}
	if err := model.PricingInstance.AddPrice(&model.Price{Model: "mj_" + strings.ToLower(action), Type: model.TimesPriceType, Input: input}); err != nil {
		t.Fatalf("create Midjourney %s price: %v", action, err)
	}
	if err := model.ChannelGroup.Load(); err != nil {
		t.Fatalf("reload Midjourney %s routing: %v", action, err)
	}
}

func loadI014UploadOwner(t *testing.T, db *gorm.DB) model.Task {
	t.Helper()
	var owner model.Task
	if err := db.Where("action = ?", provider.MjActionUpload).Order("id desc").First(&owner).Error; err != nil {
		t.Fatalf("load upload task owner: %v", err)
	}
	return owner
}

func assertI014Balances(t *testing.T, db *gorm.DB, want int) {
	t.Helper()
	var user model.User
	var token model.Token
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != want || token.RemainQuota != want {
		t.Fatalf("unexpected balances: user=%d token=%d want=%d", user.Quota, token.RemainQuota, want)
	}
}
