package midjourney

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/requester"
	"one-api/middleware"
	"one-api/model"
	provider "one-api/providers/midjourney"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestFixI013_MidjourneyShortenAcceptsPromptOnly(t *testing.T) {
	var calls atomic.Int32
	var requestPath string
	var requestBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		requestPath = r.URL.Path
		requestBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"description":"accepted","result":"shorten-task"}`))
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
	addI013ShortenSupport(t, db)

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mj/submit/shorten", strings.NewReader(`{"prompt":"shorten this prompt"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("mj-api-secret", credential)
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("prompt-only shorten was rejected: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("shorten must reach upstream exactly once: calls=%d", calls.Load())
	}
	if requestPath != "/mj/submit/shorten" {
		t.Fatalf("unexpected upstream path: %q", requestPath)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(requestBody, &wire); err != nil {
		t.Fatalf("decode upstream request: %v", err)
	}
	var prompt string
	if err := json.Unmarshal(wire["prompt"], &prompt); err != nil || prompt != "shorten this prompt" {
		t.Fatalf("upstream prompt was not preserved: %q err=%v body=%s", prompt, err, requestBody)
	}
	if _, ok := wire["taskId"]; ok {
		t.Fatalf("independent shorten unexpectedly sent a parent task id: %s", requestBody)
	}

	child, err := model.GetTaskByTaskId(model.TaskPlatformMidjourney, 1, "shorten-task")
	if err != nil || child == nil {
		t.Fatalf("accepted shorten task was not persisted: task=%+v err=%v", child, err)
	}
	if child.Action != provider.MjActionShorten || child.ReservedQuota != 20 {
		t.Fatalf("shorten task lost action or reservation: action=%q reserved=%d", child.Action, child.ReservedQuota)
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
		t.Fatalf("shorten did not preserve pre-consumption: user=%d token=%d", user.Quota, token.RemainQuota)
	}
}

func TestFixI013_MidjourneyShortenRequiresPromptBeforeProvider(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":1,"result":"unexpected"}`))
	}))
	t.Cleanup(upstream.Close)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = upstream.Client()
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
	addI013ShortenSupport(t, db)

	router := gin.New()
	router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mj/submit/shorten", strings.NewReader(`{"prompt":"   "}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("mj-api-secret", credential)
	router.ServeHTTP(recorder, request)

	if recorder.Code < http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "prompt_is_required") {
		t.Fatalf("missing shorten prompt was not rejected clearly: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("missing prompt reached upstream: calls=%d", calls.Load())
	}
	var count int64
	if err := db.Model(&model.Task{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("missing prompt created a task owner: count=%d", count)
	}
}

func TestFixI013_MidjourneyParentOperationsRejectMissingOrForeignTaskBeforeProvider(t *testing.T) {
	operations := []struct {
		name        string
		missingBody string
		foreignBody string
	}{
		{
			name:        "action",
			missingBody: `{"customId":"MJ::JOB::upsample::1::parent-task"}`,
			foreignBody: `{"taskId":"foreign-task","customId":"MJ::JOB::upsample::1::foreign-task"}`,
		},
		{
			name:        "change",
			missingBody: `{"action":"UPSCALE","index":1}`,
			foreignBody: `{"taskId":"foreign-task","action":"UPSCALE","index":1}`,
		},
		{
			name:        "simple-change",
			missingBody: `{"content":" u1"}`,
			foreignBody: `{"content":"foreign-task u1"}`,
		},
		{
			name:        "modal",
			missingBody: `{"prompt":"edit"}`,
			foreignBody: `{"taskId":"foreign-task","prompt":"edit"}`,
		},
	}

	for _, operation := range operations {
		for _, scenario := range []struct {
			name    string
			body    string
			foreign bool
		}{
			{name: "missing_task_id", body: operation.missingBody},
			{name: "foreign_task_owner", body: operation.foreignBody, foreign: true},
		} {
			t.Run(operation.name+"/"+scenario.name, func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"code":1,"result":"unexpected"}`))
				}))
				t.Cleanup(upstream.Close)
				oldHTTPClient := requester.HTTPClient
				requester.HTTPClient = upstream.Client()
				t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

				db, credential := setupI043Midjourney(t, upstream.URL, "current_group")
				if scenario.foreign {
					foreignID := "foreign-task"
					foreign := &model.Task{
						Platform:                     model.TaskPlatformMidjourney,
						UserId:                       2,
						TokenID:                      99,
						ChannelId:                    1,
						TaskID:                       &foreignID,
						Status:                       model.TaskStatusSuccess,
						ProviderState:                model.TaskProviderStateClosed,
						ProviderNamespace:            "task-platform:midjourney",
						ProviderTaskScopeIncarnation: "provider-wide",
						RequestFingerprint:           fmt.Sprintf("%064d", 2),
						Action:                       provider.MjActionImagine,
						Data:                         model.EncodeMidjourneyTaskData(&model.Midjourney{Prompt: "foreign", Mode: "fast"}),
					}
					if err := db.Create(foreign).Error; err != nil {
						t.Fatal(err)
					}
				}

				router := gin.New()
				router.POST("/mj/submit/:operation", middleware.MjAuth(), middleware.Distribute(), RelayMidjourney)
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodPost, "/mj/submit/"+operation.name, strings.NewReader(scenario.body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("mj-api-secret", credential)
				router.ServeHTTP(recorder, request)

				if recorder.Code < http.StatusBadRequest {
					t.Fatalf("%s request was accepted: status=%d body=%s", scenario.name, recorder.Code, recorder.Body.String())
				}
				if calls.Load() != 0 {
					t.Fatalf("%s request reached upstream: calls=%d", scenario.name, calls.Load())
				}
				var count int64
				if err := db.Model(&model.Task{}).Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				wantCount := int64(1)
				if scenario.foreign {
					wantCount = 2
				}
				if count != wantCount {
					t.Fatalf("%s request created an unexpected owner: count=%d want=%d", scenario.name, count, wantCount)
				}
			})
		}
	}
}

func addI013ShortenSupport(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Model(&model.Channel{}).Where("id = ?", 1).Update("models", "mj_upscale,mj_modal,mj_variation,mj_shorten").Error; err != nil {
		t.Fatalf("enable shorten channel model: %v", err)
	}
	if err := model.PricingInstance.AddPrice(&model.Price{Model: "mj_shorten", Type: model.TimesPriceType, Input: 0.01}); err != nil {
		t.Fatalf("create shorten price: %v", err)
	}
	if err := model.ChannelGroup.Load(); err != nil {
		t.Fatalf("reload shorten channel routing: %v", err)
	}
}
