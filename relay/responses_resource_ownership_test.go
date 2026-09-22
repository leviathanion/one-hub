package relay

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
)

func TestResponsesResourceRouteDoesNotAuthorizeUnknownContinuation(t *testing.T) {
	setupRelayTestDB(t, &model.ResourceOwner{}, &model.ResponseOwner{})
	seedRequestResource(t, "file", "file_owned", 11, 31)
	for _, userID := range []int{11, 12} {
		owner, err := model.NewResponseOwner(fmt.Sprintf("resp_user_%d", userID), userID, 21, 31, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if err := model.CreateResponseOwner(nil, owner); err != nil {
			t.Fatal(err)
		}
	}

	for _, path := range []string{"/v1/responses", "/v1/responses/input_tokens", "websocket"} {
		for _, scenario := range []struct {
			name       string
			responseID string
			role       int
			ignoredPin bool
			adminPin   bool
			allowed    bool
		}{
			{name: "own response", responseID: "resp_user_11", role: config.RoleCommonUser, allowed: true},
			{name: "another user response", responseID: "resp_user_12", role: config.RoleCommonUser},
			{name: "unknown response", responseID: "resp_missing", role: config.RoleCommonUser},
			{name: "administrator without selector", responseID: "resp_user_12", role: config.RoleAdminUser},
			{name: "ignored administrator selector", responseID: "resp_missing", role: config.RoleAdminUser, ignoredPin: true},
			{name: "explicit administrator selector", responseID: "resp_missing", role: config.RoleAdminUser, adminPin: true, allowed: true},
		} {
			for _, store := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/store=%t", path, scenario.name, store), func(t *testing.T) {
					raw := []byte(fmt.Sprintf(`{"model":"gpt-5","store":%t,"previous_response_id":%q,"input":[{"type":"input_file","file_id":"file_owned"}],"future":{"value":1.00e+9}}`, store, scenario.responseID))
					ctx, _ := responsesOwnerTestContext(11, 21)
					ctx.Set("role", scenario.role)
					if scenario.ignoredPin {
						ctx.Set("specific_channel_id", 31)
						ctx.Set("specific_channel_id_ignore", true)
					}
					if scenario.adminPin {
						ctx.Set("specific_channel_id", 31)
						ctx.Set("long_lived_admin_selected_channel", 31)
					}
					var err error
					if path == "websocket" {
						if err = prepareResourceRequest(ctx, raw, "responses"); err != nil {
							t.Fatal(err)
						}
						var request types.OpenAIResponsesRequest
						if err := json.Unmarshal(raw, &request); err != nil {
							t.Fatal(err)
						}
						_, err = PrepareResponsesTurnAffinity(ResponsesAffinityInput{Context: ctx, Request: &request})
					} else {
						ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
						err = NewRelayResponses(ctx).setRequest()
					}
					if ctx.GetInt(relay_util.ResourceOwnerChannelKey) != 31 {
						t.Fatal("测试未建立自有文件的固定渠道")
					}
					if scenario.allowed {
						if err != nil {
							t.Fatalf("同渠道的自有资源和 Response 应通过归属检查: %v", err)
						}
						return
					}
					apiErr := responsesOwnershipAPIError(err)
					if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || apiErr.Code != "previous_response_not_found" {
						t.Fatalf("资源渠道授予了未知 Response 权限: err=%v apiErr=%+v", err, apiErr)
					}
				})
			}
		}
	}
}
