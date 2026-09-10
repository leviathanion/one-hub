package router

import (
	"fmt"
	"io"
	"net/http"
	ratelimit "one-api/common/limit"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
)

func TestSafetyAlertsRequireAdminSelectedChannel(t *testing.T) {
	var calls atomic.Int64
	fixture := newLongLivedRouteFixtureWithHTTP(t, false, true, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/safety/alerts/alert_test" || r.Header.Get("Authorization") != "Bearer upstream-secret" {
			t.Errorf("wrong upstream request: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"alert_test","response_id":"resp_1","reason":null,"request_paused":true}`)
	})
	var channel model.Channel
	model.GlobalUserGroupRatio.Lock()
	model.GlobalUserGroupRatio.APILimiter["realtime-route"] = ratelimit.NewMemoryLimiter(100, 100, time.Minute, false)
	model.GlobalUserGroupRatio.Unlock()
	if err := fixture.db.First(&channel).Error; err != nil {
		t.Fatal(err)
	}
	do := func(credential string) (int, string) {
		baseURL := "http" + strings.TrimPrefix(strings.TrimSuffix(fixture.url, "/v1/responses"), "ws")
		req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/safety/alerts/alert_test", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+credential)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	for _, credential := range []string{fixture.token, fmt.Sprintf("%s#%d", fixture.token, channel.Id)} {
		status, _ := do(credential)
		if status != http.StatusForbidden || calls.Load() != 0 {
			t.Fatalf("ordinary user accessed project alert: %d calls=%d", status, calls.Load())
		}
	}
	if err := fixture.db.Model(&model.User{}).Where("id = ?", fixture.userID).Update("role", config.RoleAdminUser).Error; err != nil {
		t.Fatal(err)
	}
	status, body := do(fmt.Sprintf("%s#%d", fixture.token, channel.Id))
	if status != http.StatusOK || calls.Load() != 1 || !strings.Contains(body, `"reason":null`) {
		t.Fatalf("admin alert relay failed: %d %s calls=%d", status, body, calls.Load())
	}
}
