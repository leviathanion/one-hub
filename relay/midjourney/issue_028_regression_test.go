package midjourney

import (
	"context"
	"encoding/json"
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

func TestIssue028MidjourneyAcceptanceRecognizesCancelAndSettlesOwner(t *testing.T) {
	var providerCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":21,"description":"already queued","result":"issue-028-accepted","properties":{"status":"CANCEL","imageUrl":""}}`))
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
	if recorder.Code != http.StatusOK || providerCalls.Load() != 1 {
		t.Fatalf("CANCEL acceptance was not returned once: status=%d calls=%d body=%s", recorder.Code, providerCalls.Load(), recorder.Body.String())
	}

	owner, err := model.GetTaskByTaskId(model.TaskPlatformMidjourney, 1, "issue-028-accepted")
	if err != nil || owner == nil {
		t.Fatalf("accepted owner was not persisted: owner=%+v err=%v", owner, err)
	}
	if owner.ProviderState != model.TaskProviderStateClosed || owner.Status != model.TaskStatusCancel || owner.Progress != 100 || owner.NextActionAt != 0 || owner.ChargedQuota == nil || *owner.ChargedQuota != 0 || owner.SettlementDecision != "cancel" {
		t.Fatalf("CANCEL acceptance did not close/refund owner: %+v", owner)
	}
	assertIssue028MidjourneyBalances(t, db, 1000)

	// The public fetch path reads the same durable status after acceptance.
	fetchRecorder := httptest.NewRecorder()
	fetch, _ := gin.CreateTestContext(fetchRecorder)
	fetch.Request = httptest.NewRequest(http.MethodGet, "/mj/task/issue-028-accepted/fetch", nil)
	fetch.Params = gin.Params{{Key: "id", Value: "issue-028-accepted"}}
	fetch.Set("id", 1)
	if apiErr := RelayMidjourneyTask(fetch, provider.RelayModeMidjourneyTaskFetch); apiErr != nil {
		t.Fatalf("public CANCEL fetch failed: %+v", apiErr)
	}
	var public provider.MidjourneyDto
	if err := json.Unmarshal(fetchRecorder.Body.Bytes(), &public); err != nil {
		t.Fatalf("decode public task: %v body=%s", err, fetchRecorder.Body.String())
	}
	if public.Status != string(model.TaskStatusCancel) || public.Progress != "100%" {
		t.Fatalf("public task lost CANCEL terminal status: %+v", public)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("public fetch unexpectedly called provider: %d", providerCalls.Load())
	}

	if _, err := taskbase.FinalizeTaskSettlement(context.Background(), owner); err != nil {
		t.Fatalf("duplicate acceptance finalization failed: %v", err)
	}
	assertIssue028MidjourneyBalances(t, db, 1000)
}

func assertIssue028MidjourneyBalances(t *testing.T, db *gorm.DB, want int) {
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
		t.Fatalf("balances user=%d token=%d want=%d", user.Quota, token.RemainQuota, want)
	}
}
