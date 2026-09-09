package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestFixI019_EndpointEditsCannotMoveOwnedResponses(t *testing.T) {
	for _, destination := range []string{"unchanged", "another_host", "another_namespace", "trimmed_relative_vs_encoded_absolute"} {
		t.Run(destination, func(t *testing.T) {
			setupRelayTestDB(t, &model.Channel{}, &model.ResponseOwner{}, &model.User{}, &model.Token{}, &model.Log{})
			if err := model.DB.Create(&model.User{Id: 101, Username: "i019-owner", Password: "test-only", AccessToken: "test-only-access", Status: config.UserStatusEnabled, Group: "default", Quota: 1000}).Error; err != nil {
				t.Fatal(err)
			}
			if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 201, UserId: 101, Key: "test-only-token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
				t.Fatal(err)
			}
			var originalCalls, wrongCalls atomic.Int64
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { wrongCalls.Add(1); w.WriteHeader(http.StatusNotFound) }))
			t.Cleanup(other.Close)
			original := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expectedPath := "/tenant-a/responses"
				if r.Method != http.MethodPost {
					expectedPath += "/resp_i019"
				}
				if r.URL.Path != expectedPath {
					wrongCalls.Add(1)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				originalCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				switch r.Method {
				case http.MethodGet:
					if r.URL.Path != "/tenant-a/responses/resp_i019" {
						t.Errorf("读取路径=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"id":"resp_i019","status":"completed"}`)
				case http.MethodDelete:
					if r.URL.Path != "/tenant-a/responses/resp_i019" {
						t.Errorf("删除路径=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"id":"resp_i019","deleted":true}`)
				case http.MethodPost:
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["previous_response_id"] != "resp_i019" {
						t.Errorf("续接请求=%v err=%v", body, err)
					}
					if r.URL.Path != "/tenant-a/responses" {
						t.Errorf("续接路径=%s", r.URL.Path)
					}
					fmt.Fprint(w, `{"id":"resp_i019_next","model":"gpt-4o","status":"completed","output":[]}`)
				}
			}))
			t.Cleanup(original.Close)
			oldHTTP := requester.HTTPClient
			requester.HTTPClient = original.Client()
			t.Cleanup(func() { requester.HTTPClient = oldHTTP })
			plugin := func(endpoint string) *datatypes.JSONType[model.PluginType] {
				value := datatypes.NewJSONType(model.PluginType{"endpoints": {"openai.responses": map[string]any{"enabled": true, "upstream_url": endpoint}}})
				return &value
			}
			proxy := ""
			baseURL, originalEndpoint := other.URL, original.URL+"/tenant-a/responses"
			if destination == "trimmed_relative_vs_encoded_absolute" {
				baseURL, originalEndpoint = original.URL, "/tenant-a/responses "
			}
			channel := &model.Channel{Id: 301, Type: config.ChannelTypeCustom, Key: "test-only", Name: "original", Status: config.ChannelStatusEnabled, BaseURL: &baseURL, Proxy: &proxy, Models: "gpt-4o", Group: "default", Other: `{"responses_stored_lifecycle":true}`, Plugin: plugin(originalEndpoint)}
			if err := model.DB.Create(channel).Error; err != nil {
				t.Fatal(err)
			}
			owner, err := model.NewResponseOwner("resp_i019", 101, 201, channel.Id, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := model.CreateResponseOwner(context.Background(), owner); err != nil {
				t.Fatal(err)
			}
			newEndpoint := other.URL + "/tenant-a/responses"
			if destination == "another_namespace" {
				newEndpoint = original.URL + "/tenant-b/responses"
			}
			if destination == "trimmed_relative_vs_encoded_absolute" {
				newEndpoint = original.URL + "/tenant-a/responses%20"
			}
			if destination != "unchanged" {
				edit := &model.Channel{Id: channel.Id, Plugin: plugin(newEndpoint)}
				if err := edit.UpdateWithOptions(false, model.ChannelUpdateOptions{SubmittedFields: map[string]json.RawMessage{"plugin": {}}}); err == nil {
					t.Fatal("允许原地修改已有资源的端点身份")
				}
			}
			metadata := &model.Channel{Id: channel.Id, Name: "元数据编辑"}
			if err := metadata.UpdateWithOptions(false, model.ChannelUpdateOptions{SubmittedFields: map[string]json.RawMessage{"name": {}}}); err != nil {
				t.Fatal(err)
			}
			newChannel := *channel
			newChannel.Id = 302
			newChannel.Plugin = plugin(newEndpoint)
			if err := newChannel.Insert(); err != nil {
				t.Fatalf("新渠道不能使用新端点: %v", err)
			}
			invokeStored := func(method string) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(method, "/v1/responses/resp_i019", nil)
				ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
				ctx.Set("id", owner.UserID)
				ctx.Set("token_id", owner.TokenID)
				StoredResponses(ctx)
				if recorder.Code != http.StatusOK {
					t.Fatalf("旧资源%s失败: %d %s", method, recorder.Code, recorder.Body.String())
				}
			}
			invokeStored(http.MethodGet)
			ctx, _ := responsesOwnerTestContext(owner.UserID, owner.TokenID)
			request := &types.OpenAIResponsesRequest{Model: "gpt-4o", PreviousResponseID: owner.ResponseID}
			if route, err := prepareResponsesContinuationOwnership(ctx, request); err != nil || route.ChannelID != channel.Id {
				t.Fatalf("续接owner改变: %+v %v", route, err)
			}
			selected, err := fetchChannel(ctx, "gpt-4o")
			if err != nil || selected.Id != channel.Id {
				t.Fatalf("续接渠道错误: %+v %v", selected, err)
			}
			provider, ok := providers.GetProvider(selected, ctx).(providersBase.ResponsesInterface)
			if !ok {
				t.Fatal("自定义渠道未使用Responses factory")
			}
			provider.SetUsage(&types.Usage{})
			body, err := commonresponses.ParseRawEnvelope([]byte(`{"model":"gpt-4o","previous_response_id":"resp_i019","input":"continue","store":false}`))
			if err != nil {
				t.Fatal(err)
			}
			if _, apiErr := provider.CreateResponses(context.Background(), &commonresponses.Request{Operation: commonresponses.ResponsesCreate, Body: body, Model: "gpt-4o"}); apiErr != nil {
				t.Fatal(apiErr)
			}
			invokeStored(http.MethodDelete)
			stored, err := model.GetResponseOwner(context.Background(), owner.ResponseID, owner.UserID)
			if err != nil || stored.ChannelID != channel.Id || stored.State != model.ResponseOwnerStateDeleted {
				t.Fatalf("旧资源删除未正确tombstone: %+v %v", stored, err)
			}
			if originalCalls.Load() != 3 || wrongCalls.Load() != 0 {
				t.Fatalf("上游调用错误: 原端点=%d 新端点=%d", originalCalls.Load(), wrongCalls.Load())
			}
		})
	}
}
