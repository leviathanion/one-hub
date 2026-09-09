package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/openai"
	xAI "one-api/providers/xAI"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const issue053ImageModel = "issue053-image-model"

func TestIssue053RelayRejectsImageStreamBeforeProviderWork(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		build func(*testing.T) ([]byte, string)
	}{
		{
			name: "generation JSON",
			path: "/v1/images/generations",
			build: func(*testing.T) ([]byte, string) {
				return []byte(`{"model":"` + issue053ImageModel + `","prompt":"draw","stream":true,"future":{"keep":true}}`), "application/json"
			},
		},
		{
			name: "mapped multipart edits",
			path: "/v1/images/edits",
			build: func(t *testing.T) ([]byte, string) {
				return issue053MultipartBody(t, "true", "client-image")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := test.build(t)
			issue053SeedBillingRows(t)
			engine := gin.New()
			engine.POST(test.path, Relay)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body))
			request.Header.Set("Content-Type", contentType)
			engine.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("stream=true 未在 provider work 前拒绝：status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
					Param   string `json:"param"`
					Type    string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("解析 stream gate 错误失败：%v body=%s", err, recorder.Body.String())
			}
			if envelope.Error.Code != "unsupported_capability" || envelope.Error.Param != "stream" || envelope.Error.Type != "invalid_request_error" || !strings.Contains(strings.ToLower(envelope.Error.Message), "stream") {
				t.Fatalf("stream gate 错误不明确：%+v", envelope.Error)
			}
			issue053AssertBillingRowsUnchanged(t)
		})
	}
}

func TestIssue053OpenAIDirectImageProvidersRejectStreamAfterFinalRequestPlanning(t *testing.T) {
	for _, test := range []struct {
		name  string
		path  string
		build func(*testing.T) ([]byte, string)
		call  func(*openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode
	}{
		{
			name: "generation JSON model mapping",
			path: "/v1/images/generations",
			build: func(*testing.T) ([]byte, string) {
				return []byte(`{"model":"client-image","stream":true,"future":{"keep":true}}`), "application/json"
			},
			call: func(provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				_, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue053ImageModel, Prompt: "draw"})
				return apiErr
			},
		},
		{
			name: "mapped multipart edits",
			path: "/v1/images/edits",
			build: func(t *testing.T) ([]byte, string) {
				return issue053MultipartBody(t, "true", "client-image")
			},
			call: func(provider *openai.OpenAIProvider) *types.OpenAIErrorWithStatusCode {
				_, apiErr := provider.CreateImageEdits(&types.ImageEditRequest{Model: issue053ImageModel, Prompt: "draw"})
				return apiErr
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := test.build(t)
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"model":"`+issue053ImageModel+`","data":[{"url":"https://local.test/image.png"}]}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := openai.CreateOpenAIProvider(&model.Channel{
				Type: config.ChannelTypeOpenAI, Key: "issue053-key", Proxy: &proxy, BaseURL: &baseURL,
			}, server.URL)
			ctx := issue053RequestContext(t, test.path, body, contentType)
			provider.SetContext(ctx)
			provider.SetOriginalModel("client-image")
			provider.SetUsage(&types.Usage{PromptTokens: 1})

			apiErr := test.call(provider)
			if apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError || apiErr.Code != "unsupported_capability" || apiErr.Param != "stream" || apiErr.Type != "invalid_request_error" {
				t.Fatalf("direct provider 未返回明确 stream gate：%+v", apiErr)
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("direct provider gate 后仍发送了上游请求：calls=%d", upstreamCalls.Load())
			}
		})
	}
}

func TestIssue053OpenAINonStreamingGenerationPreservesRawAndSettles(t *testing.T) {
	raw := `{"model":"` + issue053ImageModel + `","stream":false,"future":{"keep":true}}`
	var upstreamBody []byte
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+issue053ImageModel+`","data":[{"url":"https://local.test/image.png"}]}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	proxy := ""
	baseURL := server.URL
	provider := openai.CreateOpenAIProvider(&model.Channel{
		Type: config.ChannelTypeOpenAI, Key: "issue053-key", Proxy: &proxy, BaseURL: &baseURL,
	}, server.URL)
	provider.SetContext(issue053RequestContext(t, "/v1/images/generations", []byte(raw), "application/json"))
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue053ImageModel, Prompt: "draw"})
	if apiErr != nil || response == nil || len(response.Data) != 1 {
		t.Fatalf("stream=false generation 失败：response=%+v err=%+v", response, apiErr)
	}
	if upstreamCalls.Load() != 1 || !bytes.Equal(upstreamBody, []byte(raw)) {
		t.Fatalf("stream=false generation wire 改变：calls=%d body=%q want=%q", upstreamCalls.Load(), upstreamBody, raw)
	}
	usage := provider.GetUsage()
	if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 || usage.PromptTokens != 1 {
		t.Fatalf("stream=false generation operation evidence 错误：%+v", usage)
	}
	issue053SettleImageUsage(t, usage, 1)
}

func TestIssue053OpenAINonStreamingMappedEditsPreserveMultipartAndSettle(t *testing.T) {
	raw, contentType := issue053MultipartBody(t, "false", "client-image")
	var upstreamBody []byte
	var upstreamContentType string
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		upstreamContentType = r.Header.Get("Content-Type")
		upstreamBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+issue053ImageModel+`","data":[{"url":"https://local.test/edit.png"}]}`)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	proxy := ""
	baseURL := server.URL
	provider := openai.CreateOpenAIProvider(&model.Channel{
		Type: config.ChannelTypeOpenAI, Key: "issue053-key", Proxy: &proxy, BaseURL: &baseURL,
	}, server.URL)
	provider.SetContext(issue053RequestContext(t, "/v1/images/edits", raw, contentType))
	provider.SetOriginalModel("client-image")
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageEdits(&types.ImageEditRequest{Model: issue053ImageModel, Prompt: "draw"})
	if apiErr != nil || response == nil || len(response.Data) != 1 {
		t.Fatalf("stream=false edits 失败：response=%+v err=%+v", response, apiErr)
	}
	if upstreamCalls.Load() != 1 {
		t.Fatalf("stream=false edits 上游请求次数=%d want=1", upstreamCalls.Load())
	}
	issue053AssertMultipartEditWire(t, upstreamBody, upstreamContentType, issue053ImageModel)
	usage := provider.GetUsage()
	if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
		t.Fatalf("stream=false edits operation evidence 错误：%+v", usage)
	}
	issue053SettleImageUsage(t, usage, 1)
}

func TestIssue053XAIFactoryImageGenerationGateCoversMergePath(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run("stream="+strconv.FormatBool(stream), func(t *testing.T) {
			raw := `{"model":"client-image","prompt":"draw","stream":` + strconv.FormatBool(stream) + `,"future":{"keep":true}}`
			var upstreamBody []byte
			var upstreamCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				upstreamBody, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"model":"`+issue053ImageModel+`","data":[{"url":"https://local.test/xai-image.png"}]}`)
			}))
			t.Cleanup(server.Close)
			previousClient := requester.HTTPClient
			requester.HTTPClient = server.Client()
			t.Cleanup(func() { requester.HTTPClient = previousClient })

			proxy := ""
			baseURL := server.URL
			provider := (xAI.XAIProviderFactory{}).Create(&model.Channel{
				Type: config.ChannelTypeXAI, Key: "issue053-xai-key", Proxy: &proxy, BaseURL: &baseURL, AllowExtraBody: true,
			}).(*xAI.XAIProvider)
			provider.SetContext(issue053RequestContext(t, "/v1/images/generations", []byte(raw), "application/json"))
			provider.SetOriginalModel("client-image")
			provider.SetUsage(&types.Usage{PromptTokens: 1})
			response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue053ImageModel, Prompt: "draw"})

			if stream {
				if response != nil || apiErr == nil || apiErr.StatusCode != http.StatusBadRequest || !apiErr.LocalError || apiErr.Code != "unsupported_capability" || apiErr.Param != "stream" || apiErr.Type != "invalid_request_error" {
					t.Fatalf("XAI merge path 未拒绝 stream=true：response=%+v err=%+v", response, apiErr)
				}
				if upstreamCalls.Load() != 0 {
					t.Fatalf("XAI stream gate 后仍发送上游：calls=%d", upstreamCalls.Load())
				}
				return
			}

			if apiErr != nil || response == nil || len(response.Data) != 1 || upstreamCalls.Load() != 1 {
				t.Fatalf("XAI stream=false 失败：response=%+v err=%+v calls=%d", response, apiErr, upstreamCalls.Load())
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(upstreamBody, &fields); err != nil {
				t.Fatalf("解析 XAI 最终 JSON 失败：%v body=%s", err, upstreamBody)
			}
			var finalStream bool
			if err := json.Unmarshal(fields["stream"], &finalStream); err != nil || finalStream || fields["future"] == nil {
				t.Fatalf("XAI stream=false 或未知字段未保留：%s", upstreamBody)
			}
			usage := provider.GetUsage()
			if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
				t.Fatalf("XAI stream=false operation evidence 错误：%+v", usage)
			}
			issue053SettleImageUsage(t, usage, 1)
		})
	}
}

func issue053RequestContext(t *testing.T, path string, body []byte, contentType string) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", contentType)
	return ctx
}

func issue053MultipartBody(t *testing.T, stream, modelName string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	boundary := writer.Boundary()
	_ = writer.WriteField("model", modelName)
	_ = writer.WriteField("stream", stream)
	_ = writer.WriteField("prompt", "draw")
	_ = writer.WriteField("future_part", "future=3Dvalue")
	part, err := writer.CreateFormFile("image[]", "one.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = part.Write([]byte("png-bytes"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes(), "multipart/form-data; boundary=" + boundary
}

func issue053AssertMultipartEditWire(t *testing.T, body []byte, contentType, expectedModel string) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || mediaType != "multipart/form-data" || strings.TrimSpace(params["boundary"]) == "" {
		t.Fatalf("上游 multipart Content-Type 无效：type=%q params=%v err=%v", contentType, params, err)
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	values := make(map[string][]string)
	var imageBody string
	for {
		part, err := reader.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("解析上游 multipart 失败：%v", err)
		}
		partBody, err := io.ReadAll(part)
		_ = part.Close()
		if err != nil {
			t.Fatalf("读取上游 multipart part 失败：%v", err)
		}
		if part.FormName() == "image[]" {
			imageBody = string(partBody)
		} else {
			values[part.FormName()] = append(values[part.FormName()], string(partBody))
		}
	}
	if values["model"][0] != expectedModel || values["stream"][0] != "false" || values["future_part"][0] != "future=3Dvalue" || imageBody != "png-bytes" {
		t.Fatalf("mapped edits multipart 字段丢失或错误：values=%v image=%q", values, imageBody)
	}
}

func issue053SeedBillingRows(t *testing.T) {
	t.Helper()
	setupRelayTestDB(t, &model.User{}, &model.Token{}, &model.Log{})
	if err := model.DB.Create(&model.User{
		Id: 1, Username: "issue053-user", Password: "password123", AccessToken: "issue053-access",
		Quota: 100000, Group: "default", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("创建 I053 user fixture 失败：%v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "issue053-token", Name: "issue053-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "default",
	}).Error; err != nil {
		t.Fatalf("创建 I053 token fixture 失败：%v", err)
	}
}

func issue053AssertBillingRowsUnchanged(t *testing.T) {
	t.Helper()
	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100000 || user.UsedQuota != 0 || user.RequestCount != 0 || token.RemainQuota != 100000 || token.UsedQuota != 0 {
		t.Fatalf("prework gate 改变了余额：user=%+v token=%+v", user, token)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 0 {
		t.Fatalf("prework gate 产生了消费日志：%+v", logs)
	}
}

func issue053SettleImageUsage(t *testing.T, usage *types.Usage, units int) {
	t.Helper()
	issue053SeedBillingRows(t)
	oldPricing, oldBatch, oldRedis, oldReserve, oldLog := model.PricingInstance, config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota, config.LogConsumeEnabled
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		issue053ImageModel: {Model: issue053ImageModel, Type: model.TimesPriceType, Input: 1},
	}}
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = true
	t.Cleanup(func() {
		model.PricingInstance = oldPricing
		config.BatchUpdateEnabled = oldBatch
		config.RedisEnabled = oldRedis
		config.PreConsumedQuota = oldReserve
		config.LogConsumeEnabled = oldLog
	})

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(context.Background())
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	groupctx.SetRoutingGroup(ctx, "default", groupctx.RoutingGroupSourceUserGroup)
	attempt, err := relay_util.NewAttemptQuota(ctx, issue053ImageModel, 1, relay_util.BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("创建 I053 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("I053 预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("I053 submission claim 失败：%v", err)
	}
	wantCharge := int64(units * 1000)
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || !result.Confirmed || result.ChargedQuota != wantCharge {
		t.Fatalf("I053 图片费用结算错误：result=%+v err=%v usage=%+v", result, err, usage)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || !reflect.DeepEqual(second, result) {
		t.Fatalf("重复 Close 改变 I053 结算：first=%+v second=%+v err=%v", result, second, err)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100000-int(wantCharge) || user.UsedQuota != int(wantCharge) || token.RemainQuota != 100000-int(wantCharge) || token.UsedQuota != int(wantCharge) {
		t.Fatalf("I053 SQL 余额错误：user=%+v token=%+v wantCharge=%d", user, token, wantCharge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Quota != int(wantCharge) || logs[0].ModelName != issue053ImageModel {
		t.Fatalf("I053 consume log 金额或次数错误：logs=%+v wantCharge=%d", logs, wantCharge)
	}
}
