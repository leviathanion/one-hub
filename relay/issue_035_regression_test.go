package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	cloudflareAI "one-api/providers/cloudflareAI"
	recraftAI "one-api/providers/recraftAI"
	"one-api/providers/zhipu"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const issue035BillingModel = "issue035-image-model"

func TestIssue035ImageProvidersPublishOperationEvidenceAndSettleSQL(t *testing.T) {
	for _, test := range []struct {
		name      string
		modelName string
		build     func(*testing.T) *types.Usage
		units     int
	}{
		{
			name:      "Recraft生成多图按有效结果数",
			modelName: issue035BillingModel,
			build:     issue035RecraftGenerationUsage,
			units:     2,
		},
		{
			name:      "Zhipu生成多图按有效结果数",
			modelName: issue035BillingModel,
			build:     issue035ZhipuGenerationUsage,
			units:     2,
		},
		{
			name:      "Cloudflare有效PNG单图",
			modelName: issue035BillingModel,
			build:     issue035CloudflareGenerationUsage,
			units:     1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage := test.build(t)
			if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != test.units {
				t.Fatalf("有效图片结果未建立计次证据：%+v", usage)
			}
			issue035SettleUsageThroughSQL(t, usage, test.modelName, test.units)
		})
	}
}

func TestIssue035RecraftNativeEvidencePreservesBodyAndSettlesSQL(t *testing.T) {
	for _, test := range []struct {
		name string
		path string
		body string
	}{
		{
			name: "vectorize",
			path: "/v1/images/vectorize",
			body: `{"image":{"url":"https://local.test/vectorized.png"},"id":123,"future":{"keep":true}}`,
		},
		{
			name: "removeBackground",
			path: "/v1/images/removeBackground",
			body: `{"image":{"url":"https://local.test/no-background.png"},"id":123,"future":{"keep":true}}`,
		},
		{
			name: "styles",
			path: "/v1/styles",
			body: `{"id":"style-local","image":[],"future":{"keep":true}}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage, gotBody, apiErr := issue035RecraftNative(t, test.path, test.body, http.StatusOK)
			if apiErr != nil || gotBody != test.body {
				t.Fatalf("Recraft 原生响应未保持原始 body：body=%q err=%+v", gotBody, apiErr)
			}
			if usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
				t.Fatalf("Recraft %s 未建立计次证据：%+v", test.name, usage)
			}
			issue035SettleUsageThroughSQL(t, usage, issue035BillingModel, 1)
		})
	}
}

func TestIssue035ImageFailuresAndEmptySuccessDoNotPublishOperationEvidence(t *testing.T) {
	for _, test := range []struct {
		name    string
		build   func(*testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode)
		wantErr bool
	}{
		{
			name: "Recraft空成功",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				usage, _, apiErr := issue035RecraftGenerationResult(t, `{"model":"`+issue035BillingModel+`","data":[]}`)
				return usage, apiErr
			},
		},
		{
			name: "Zhipu空成功",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				usage, _, apiErr := issue035ZhipuGenerationResult(t, `{"model":"`+issue035BillingModel+`","data":[]}`)
				return usage, apiErr
			},
		},
		{
			name: "Recraft失败",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				usage, _, apiErr := issue035RecraftNative(t, "/v1/styles", `{"code":"provider_failed","message":"failed"}`, http.StatusBadGateway)
				return usage, apiErr
			},
			wantErr: true,
		},
		{
			name: "Cloudflare空body",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				return issue035CloudflareGenerationResult(t, nil)
			},
			wantErr: true,
		},
		{
			name: "Cloudflare非空非法PNG",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				return issue035CloudflareGenerationResult(t, []byte("not-a-png"))
			},
			wantErr: true,
		},
		{
			name: "Cloudflare小输入超大尺寸PNG",
			build: func(t *testing.T) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
				return issue035CloudflareGenerationResult(t, issue035PNGWithDimensions(t, 1<<20, 1<<20))
			},
			wantErr: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage, apiErr := test.build(t)
			if usage.ProviderOperationUnits != nil {
				t.Fatalf("失败或空成功建立了计次证据：%+v err=%+v", usage, apiErr)
			}
			if (apiErr != nil) != test.wantErr {
				t.Fatalf("错误状态不符：err=%+v wantErr=%v", apiErr, test.wantErr)
			}
			if strings.Contains(test.name, "超大尺寸") && (apiErr == nil || !strings.Contains(apiErr.Message, "budget")) {
				t.Fatalf("超大尺寸 PNG 未在解码前被预算拒绝：%+v", apiErr)
			}
			issue035SettleUsageThroughSQL(t, usage, issue035BillingModel, 0)
		})
	}
}

func issue035RecraftGenerationUsage(t *testing.T) *types.Usage {
	usage, response, apiErr := issue035RecraftGenerationResult(t, `{"model":"`+issue035BillingModel+`","data":[{"url":"https://local.test/one.png"},{"url":""},{"url":"https://local.test/two.png"}]}`)
	if apiErr != nil || response == nil || len(response.Data) != 3 {
		t.Fatalf("Recraft 图片生成失败：response=%+v err=%+v", response, apiErr)
	}
	return usage
}

func issue035RecraftGenerationResult(t *testing.T, responseBody string) (*types.Usage, *types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	withIssue035HTTPClient(t, server)

	proxy := ""
	baseURL := server.URL
	provider := (recraftAI.RecraftProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeRecraft,
		Key:     "recraft-test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*recraftAI.RecraftProvider)
	ctx := issue035ProviderContext(t, "/v1/images/generations", `{"prompt":"draw","model":"`+issue035BillingModel+`","n":3}`)
	provider.SetContext(ctx)
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue035BillingModel, Prompt: "draw", N: 3})
	return provider.GetUsage(), response, apiErr
}

func issue035ZhipuGenerationUsage(t *testing.T) *types.Usage {
	usage, response, apiErr := issue035ZhipuGenerationResult(t, `{"model":"`+issue035BillingModel+`","data":[{"url":"https://local.test/one.png"},{"url":""},{"url":"https://local.test/two.png"}]}`)
	if apiErr != nil || response == nil || len(response.Data) != 3 {
		t.Fatalf("Zhipu 图片生成失败：response=%+v err=%+v", response, apiErr)
	}
	return usage
}

func issue035ZhipuGenerationResult(t *testing.T, responseBody string) (*types.Usage, *types.ImageResponse, *types.OpenAIErrorWithStatusCode) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	withIssue035HTTPClient(t, server)

	proxy := ""
	baseURL := server.URL
	provider := (zhipu.ZhipuProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeZhipu,
		Key:     "local-id.local-secret",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*zhipu.ZhipuProvider)
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue035BillingModel, Prompt: "draw", N: 3})
	return provider.GetUsage(), response, apiErr
}

func issue035CloudflareGenerationUsage(t *testing.T) *types.Usage {
	pngBody := issue035ValidPNG(t)
	usage, apiErr := issue035CloudflareGenerationResult(t, pngBody)
	if apiErr != nil {
		t.Fatalf("Cloudflare 图片生成失败：%+v", apiErr)
	}
	return usage
}

func issue035CloudflareGenerationResult(t *testing.T, body []byte) (*types.Usage, *types.OpenAIErrorWithStatusCode) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	withIssue035HTTPClient(t, server)

	proxy := ""
	baseURL := server.URL + "/accounts/%s/ai/run/%s"
	provider := (cloudflareAI.CloudflareAIProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeCloudflareAI,
		Key:     "local-account|local-token",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*cloudflareAI.CloudflareAIProvider)
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{Model: issue035BillingModel, Prompt: "draw", ResponseFormat: "b64_json"})
	if apiErr == nil && (response == nil || len(response.Data) != 1) {
		t.Fatalf("Cloudflare 有效 PNG 生成失败：response=%+v err=%+v", response, apiErr)
	}
	return provider.GetUsage(), apiErr
}

func issue035RecraftNative(t *testing.T, path, body string, status int) (*types.Usage, string, *types.OpenAIErrorWithStatusCode) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	withIssue035HTTPClient(t, server)

	proxy := ""
	baseURL := server.URL
	provider := (recraftAI.RecraftProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeRecraft,
		Key:     "recraft-test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*recraftAI.RecraftProvider)
	provider.SetContext(issue035ProviderContext(t, "/recraftAI"+path, `{"source":"local"}`))
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateRelay(path)
	if apiErr != nil {
		return provider.GetUsage(), "", apiErr
	}
	if response == nil || response.Body == nil {
		t.Fatal("Recraft 原生成功响应缺少 body")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读取 Recraft 原生响应 body 失败：%v", err)
	}
	return provider.GetUsage(), string(data), nil
}

func issue035ProviderContext(t *testing.T, path, body string) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx
}

func withIssue035HTTPClient(t *testing.T, server *httptest.Server) {
	t.Helper()
	previous := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previous })
}

func issue035ValidPNG(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, color.RGBA{R: 0x20, G: 0x80, B: 0xc0, A: 0xff})
	if err := png.Encode(&body, picture); err != nil {
		t.Fatalf("构造 PNG fixture 失败：%v", err)
	}
	return body.Bytes()
}

func issue035PNGWithDimensions(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var body bytes.Buffer
	body.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	chunk := func(kind string, payload []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
		_, _ = body.Write(length[:])
		_, _ = io.WriteString(&body, kind)
		_, _ = body.Write(payload)
		crcInput := make([]byte, len(kind)+len(payload))
		copy(crcInput, kind)
		copy(crcInput[len(kind):], payload)
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], crc32.ChecksumIEEE(crcInput))
		_, _ = body.Write(crc[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:4], width)
	binary.BigEndian.PutUint32(ihdr[4:8], height)
	ihdr[8] = 8
	ihdr[9] = 6
	chunk("IHDR", ihdr)
	chunk("IEND", nil)
	return body.Bytes()
}

func issue035SettleUsageThroughSQL(t *testing.T, usage *types.Usage, modelName string, units int) {
	t.Helper()
	issue035PrepareBillingDB(t, modelName)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	groupctx.SetRoutingGroup(ctx, "issue035", groupctx.RoutingGroupSourceUserGroup)

	attempt, err := relay_util.NewAttemptQuota(ctx, modelName, 1, relay_util.BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("创建 I035 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("I035 预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("I035 submission claim 失败：%v", err)
	}
	wantCharge := int64(units * 1000)
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || result.Confirmed != (units > 0) || result.ChargedQuota != wantCharge {
		t.Fatalf("I035 SQL 结算错误：result=%+v err=%v usage=%+v", result, err, usage)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || !reflect.DeepEqual(second, result) {
		t.Fatalf("重复 Close 改变 I035 结算：first=%+v second=%+v err=%v", result, second, err)
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
		t.Fatalf("I035 SQL 余额错误：user=%+v token=%+v wantCharge=%d", user, token, wantCharge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatalf("读取 I035 consume log 失败：%v", err)
	}
	wantLogCount := 0
	if units > 0 {
		wantLogCount = 1
	}
	if len(logs) != wantLogCount {
		t.Fatalf("I035 consume log 数量错误：got=%d want=%d logs=%+v", len(logs), wantLogCount, logs)
	}
	if wantLogCount == 1 && (logs[0].Quota != int(wantCharge) || logs[0].ModelName != modelName) {
		t.Fatalf("I035 consume log 金额或模型错误：log=%+v wantQuota=%d wantModel=%s", logs[0], wantCharge, modelName)
	}
}

func issue035PrepareBillingDB(t *testing.T, modelName string) {
	t.Helper()
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("创建 I035 SQLite 失败：%v", err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserGroup{}, &model.Log{}); err != nil {
		t.Fatalf("迁移 I035 SQLite 失败：%v", err)
	}
	model.DB = db

	originalPricing := model.PricingInstance
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		modelName: {Model: modelName, Type: model.TimesPriceType, Input: 1},
	}}
	originalBatch, originalRedis, originalPreConsumed, originalLog := config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota, config.LogConsumeEnabled
	config.BatchUpdateEnabled = false
	config.RedisEnabled = false
	config.PreConsumedQuota = 50
	config.LogConsumeEnabled = true

	model.GlobalUserGroupRatio.Lock()
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for key, value := range originalGroups {
		groups[key] = value
	}
	groups["issue035"] = &model.UserGroup{Symbol: "issue035", Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()

	if err := model.DB.Create(&model.User{
		Id: 1, Username: "issue035-user", Password: "password123", AccessToken: "issue035-access",
		Quota: 100000, Group: "issue035", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("写入 I035 user fixture 失败：%v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "issue035-token", Name: "issue035-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "issue035",
	}).Error; err != nil {
		t.Fatalf("写入 I035 token fixture 失败：%v", err)
	}

	t.Cleanup(func() {
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
		model.DB = originalDB
		model.PricingInstance = originalPricing
		config.BatchUpdateEnabled, config.RedisEnabled, config.PreConsumedQuota, config.LogConsumeEnabled = originalBatch, originalRedis, originalPreConsumed, originalLog
		logger.Logger = originalLogger
	})
}
