package relay

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	stabilityAI "one-api/providers/stabilityAI"
	"one-api/relay/relay_util"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestIssue036StabilitySuccessfulImagesPublishOperationUnitsAndSettleSQL(t *testing.T) {
	imageBase64 := issue036ValidPNGBase64(t)
	for _, test := range []struct {
		name   string
		model  string
		input  float64
		charge int64
	}{
		{name: "stable-image-core", model: "stable-image-core", input: 15, charge: 15000},
		{name: "sd3", model: "sd3", input: 32.5, charge: 32500},
		{name: "sd3-turbo", model: "sd3-turbo", input: 20, charge: 20000},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage, response, apiErr, calls := issue036StabilityGeneration(t, test.model, `{"image":"`+imageBase64+`","finish_reason":"SUCCESS"}`, http.StatusOK)
			if apiErr != nil || response == nil || len(response.Data) != 1 || response.Data[0].B64JSON != imageBase64 {
				t.Fatalf("Stability 成功图片响应错误：response=%+v err=%+v", response, apiErr)
			}
			if calls != 1 {
				t.Fatalf("Stability 上游请求次数=%d，want=1", calls)
			}
			if usage == nil || usage.ProviderOperationUnits == nil || *usage.ProviderOperationUnits != 1 {
				t.Fatalf("有效图片未建立单次 operation 证据：%+v", usage)
			}
			if usage.PromptTokens != 1 {
				t.Fatalf("provider 不应写入伪 PromptTokens=1000：%+v", usage)
			}
			defaultPrice, ok := findPriceInList(model.GetDefaultPrice(), test.model)
			if !ok {
				t.Fatalf("缺少 I036 默认价格：%s", test.model)
			}
			db, restarted := issue036PrepareDefaultBillingDB(t, defaultPrice)
			if restarted.PublishedVersion() != 2 {
				t.Fatalf("默认价格重启后 version=%d，want=2", restarted.PublishedVersion())
			}
			var stored model.Price
			if err := db.Where("model = ?", test.model).Take(&stored).Error; err != nil {
				t.Fatal(err)
			}
			if stored.Type != model.TimesPriceType || stored.Input != test.input || stored.Output != 0 {
				t.Fatalf("默认价格未接入重启后的 billing publisher：%+v", stored)
			}
			issue036SettleUsageOnCurrentDB(t, usage, test.model, test.charge)
		})
	}
}

func TestIssue036StabilityFailuresEmptyAndFilteredResultsDoNotPublishOperationEvidence(t *testing.T) {
	imageBase64 := issue036ValidPNGBase64(t)
	headerOnlyBase64 := base64.StdEncoding.EncodeToString(issue036PNGHeader(t, 1, 1))
	truncatedIDATBase64 := base64.StdEncoding.EncodeToString(issue036TruncatedIDAT(t))
	oversizedHeaderBase64 := base64.StdEncoding.EncodeToString(issue036PNGHeader(t, 16385, 1))
	for _, test := range []struct {
		name    string
		body    string
		status  int
		wantErr bool
	}{
		{name: "empty image", body: `{"image":"","finish_reason":"SUCCESS"}`, status: http.StatusOK},
		{name: "invalid base64", body: `{"image":"not-base64","finish_reason":"SUCCESS"}`, status: http.StatusOK},
		{name: "PNG header without IDAT", body: `{"image":"` + headerOnlyBase64 + `","finish_reason":"SUCCESS"}`, status: http.StatusOK},
		{name: "truncated IDAT", body: `{"image":"` + truncatedIDATBase64 + `","finish_reason":"SUCCESS"}`, status: http.StatusOK},
		{name: "PNG dimensions exceed budget", body: `{"image":"` + oversizedHeaderBase64 + `","finish_reason":"SUCCESS"}`, status: http.StatusOK},
		{name: "filtered result", body: `{"image":"` + imageBase64 + `","finish_reason":"CONTENT_FILTERED"}`, status: http.StatusOK},
		{name: "provider error", body: `{"name":"bad_request","errors":["filtered"]}`, status: http.StatusBadRequest, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			usage, _, apiErr, calls := issue036StabilityGeneration(t, "stable-image-core", test.body, test.status)
			if calls != 1 {
				t.Fatalf("Stability 上游请求次数=%d，want=1", calls)
			}
			if usage == nil || usage.ProviderOperationUnits != nil {
				t.Fatalf("失败/空/过滤结果建立了 operation 证据：%+v", usage)
			}
			if (apiErr != nil) != test.wantErr {
				t.Fatalf("错误状态=%v，want=%v，err=%+v", apiErr != nil, test.wantErr, apiErr)
			}
			issue036SettleUsageThroughSQL(t, usage, "stable-image-core", 15, 0)
		})
	}
}

func issue036StabilityGeneration(t *testing.T, modelName, responseBody string, status int) (*types.Usage, *types.ImageResponse, *types.OpenAIErrorWithStatusCode, int) {
	t.Helper()
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, responseBody)
	}))
	t.Cleanup(server.Close)
	previousClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = previousClient })

	proxy, baseURL := "", server.URL
	provider := (stabilityAI.StabilityAIProviderFactory{}).Create(&model.Channel{
		Type:    config.ChannelTypeStabilityAI,
		Key:     "i036-test-key",
		Proxy:   &proxy,
		BaseURL: &baseURL,
	}).(*stabilityAI.StabilityAIProvider)
	provider.SetContext(issue036ProviderContext(t, "/v1/images/generations", `{"model":"`+modelName+`","prompt":"draw"}`))
	provider.SetOriginalModel(modelName)
	provider.SetUsage(&types.Usage{PromptTokens: 1})
	response, apiErr := provider.CreateImageGenerations(&types.ImageRequest{
		Model:          modelName,
		Prompt:         "draw",
		ResponseFormat: "b64_json",
	})
	return provider.GetUsage(), response, apiErr, calls
}

func issue036ProviderContext(t *testing.T, path, body string) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	return ctx
}

func issue036SettleUsageThroughSQL(t *testing.T, usage *types.Usage, modelName string, input float64, wantCharge int64) {
	t.Helper()
	issue036PrepareBillingDB(t, modelName, input)
	issue036SettleUsageOnCurrentDB(t, usage, modelName, wantCharge)
}

func issue036SettleUsageOnCurrentDB(t *testing.T, usage *types.Usage, modelName string, charge int64) {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	ctx.Set("id", 1)
	ctx.Set("token_id", 1)
	groupctx.SetRoutingGroup(ctx, "issue036", groupctx.RoutingGroupSourceUserGroup)

	attempt, err := relay_util.NewAttemptQuota(ctx, modelName, 1, relay_util.BillingAttemptSpec{})
	if err != nil {
		t.Fatalf("创建 I036 billing attempt 失败：%v", err)
	}
	if err := attempt.ApplyReserve(context.Background()); err != nil {
		t.Fatalf("I036 预扣失败：%v", err)
	}
	if err := attempt.ClaimSubmission(); err != nil {
		t.Fatalf("I036 submission claim 失败：%v", err)
	}
	result, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || result.Confirmed != (charge > 0) || result.ChargedQuota != charge {
		t.Fatalf("I036 operation→SQL 结算错误：result=%+v err=%v usage=%+v want=%d", result, err, usage, charge)
	}
	second, err := attempt.CloseFromProviderResult(context.Background(), usage, false)
	if err != nil || second != result {
		t.Fatalf("重复 Close 改变 I036 结算：first=%+v second=%+v err=%v", result, second, err)
	}

	var user model.User
	var token model.Token
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.DB.First(&token, 1).Error; err != nil {
		t.Fatal(err)
	}
	const startingQuota = 100000
	if user.Quota != startingQuota-int(charge) || user.UsedQuota != int(charge) || token.RemainQuota != startingQuota-int(charge) || token.UsedQuota != int(charge) {
		t.Fatalf("I036 SQL 余额错误：user=%+v token=%+v wantCharge=%d", user, token, charge)
	}
	var logs []model.Log
	if err := model.DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error; err != nil {
		t.Fatal(err)
	}
	if len(logs) != boolToInt(charge > 0) {
		t.Fatalf("I036 consume log 数量错误：got=%d want=%d logs=%+v", len(logs), boolToInt(charge > 0), logs)
	}
	if charge > 0 && (logs[0].Quota != int(charge) || logs[0].ModelName != modelName) {
		t.Fatalf("I036 consume log 金额或模型错误：log=%+v wantQuota=%d wantModel=%s", logs[0], charge, modelName)
	}
}

func issue036PrepareBillingDB(t *testing.T, modelName string, input float64) {
	t.Helper()
	issue036PrepareBillingFixture(t, &model.Price{Model: modelName, Type: model.TimesPriceType, Input: input, Output: 0}, false)
}

func issue036PrepareDefaultBillingDB(t *testing.T, price *model.Price) (*gorm.DB, *model.Pricing) {
	t.Helper()
	return issue036PrepareBillingFixture(t, price, true)
}

func issue036PrepareBillingFixture(t *testing.T, price *model.Price, publishDefault bool) (*gorm.DB, *model.Pricing) {
	t.Helper()
	originalLogger := logger.Logger
	logger.Logger = zap.NewNop()
	originalDB := model.DB
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("创建 I036 SQLite 失败：%v", err)
	}
	models := []any{&model.User{}, &model.Token{}, &model.UserGroup{}, &model.Log{}}
	if publishDefault {
		models = append(models, &model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{})
	}
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatalf("迁移 I036 SQLite 失败：%v", err)
	}
	if publishDefault {
		if err := model.EnsurePublicationVersionRows(db); err != nil {
			t.Fatalf("初始化 I036 publication version 失败：%v", err)
		}
	}
	model.DB = db

	originalPricing := model.PricingInstance
	pricing := &model.Pricing{Prices: make(map[string]*model.Price)}
	model.PricingInstance = pricing
	if !publishDefault {
		pricing.Prices[price.Model] = price
	}
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
	groups["issue036"] = &model.UserGroup{Symbol: "issue036", Ratio: 1}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()

	if err := model.DB.Create(&model.User{
		Id: 1, Username: "issue036-user", Password: "password123", AccessToken: "issue036-access",
		Quota: 100000, Group: "issue036", Status: config.UserStatusEnabled, Role: config.RoleCommonUser,
	}).Error; err != nil {
		t.Fatalf("写入 I036 user fixture 失败：%v", err)
	}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{
		Id: 1, UserId: 1, Key: "issue036-token", Name: "issue036-token", Status: config.TokenStatusEnabled,
		ExpiredTime: -1, RemainQuota: 100000, Group: "issue036",
	}).Error; err != nil {
		t.Fatalf("写入 I036 token fixture 失败：%v", err)
	}
	if publishDefault {
		if err := pricing.Init(); err != nil {
			t.Fatalf("初始化 I036 默认 billing publisher 失败：%v", err)
		}
		if err := pricing.SyncPricing([]*model.Price{price}, string(model.PriceUpdateModeSystem)); err != nil {
			t.Fatalf("发布 I036 默认价格失败：%v", err)
		}
		restarted := &model.Pricing{Prices: make(map[string]*model.Price)}
		if err := restarted.Init(); err != nil {
			t.Fatalf("重启 I036 默认 billing publisher 失败：%v", err)
		}
		model.PricingInstance = restarted
		pricing = restarted
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
	return db, pricing
}

func issue036ValidPNGBase64(t *testing.T) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString(issue036ValidPNGBytes(t))
}

func issue036ValidPNGBytes(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	picture := image.NewRGBA(image.Rect(0, 0, 1, 1))
	picture.Set(0, 0, color.RGBA{R: 0x20, G: 0x80, B: 0xc0, A: 0xff})
	if err := png.Encode(&body, picture); err != nil {
		t.Fatalf("构造 I036 PNG fixture 失败：%v", err)
	}
	return body.Bytes()
}

func issue036PNGHeader(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var body bytes.Buffer
	_, _ = body.Write([]byte{137, 80, 78, 71, 13, 10, 26, 10})
	writeChunk := func(kind string, payload []byte) {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(payload)))
		_, _ = body.Write(length[:])
		_, _ = body.WriteString(kind)
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
	writeChunk("IHDR", ihdr)
	writeChunk("IEND", nil)
	return body.Bytes()
}

func issue036TruncatedIDAT(t *testing.T) []byte {
	t.Helper()
	body := issue036ValidPNGBytes(t)
	chunkType := bytes.Index(body, []byte("IDAT"))
	if chunkType < 0 {
		t.Fatal("valid I036 PNG fixture has no IDAT chunk")
	}
	dataStart := chunkType + len("IDAT")
	if dataStart >= len(body) {
		t.Fatal("valid I036 PNG fixture has empty IDAT chunk")
	}
	return body[:dataStart+1]
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestIssue036StabilityDefaultsUseTimesUnitsInAnEmptyPricingDB(t *testing.T) {
	db, pricing := issue036PreparePricingDB(t)
	defaults := issue036StabilityDefaults(t)
	if err := pricing.Init(); err != nil {
		t.Fatalf("初始化空 I036 pricing 失败：%v", err)
	}
	if pricing.PublishedVersion() != 1 {
		t.Fatalf("空 pricing 初始 version=%d，want=1", pricing.PublishedVersion())
	}
	if err := pricing.SyncPricing(defaults, string(model.PriceUpdateModeSystem)); err != nil {
		t.Fatalf("写入 I036 空库默认价格失败：%v", err)
	}
	if pricing.PublishedVersion() != 2 {
		t.Fatalf("默认价格发布后 version=%d，want=2", pricing.PublishedVersion())
	}
	for _, test := range []struct {
		model string
		input float64
	}{
		{model: "stable-image-core", input: 15},
		{model: "sd3", input: 32.5},
		{model: "sd3-turbo", input: 20},
	} {
		price, ok := pricing.FindExactPrice(test.model)
		if !ok || price.Type != model.TimesPriceType || price.Input != test.input || price.Output != 0 || price.ChannelType != config.ChannelTypeStabilityAI {
			t.Fatalf("空库默认价格错误：model=%s price=%+v exists=%v", test.model, price, ok)
		}
		var stored model.Price
		if err := db.Where("model = ?", test.model).Take(&stored).Error; err != nil {
			t.Fatal(err)
		}
		if stored.Type != model.TimesPriceType || stored.Input != test.input || stored.Output != 0 {
			t.Fatalf("空库默认价格未持久化：stored=%+v", stored)
		}
	}

	whisper, ok := findPriceInList(model.GetDefaultPrice(), "whisper-1")
	if !ok || whisper.Type != model.TokensPriceType || whisper.Input != 50 || whisper.Output != 0 {
		t.Fatalf("I026 Whisper 默认被 I036 默认构造破坏：price=%+v exists=%v", whisper, ok)
	}
}

func TestIssue036SystemSyncDoesNotOverwriteExistingLegacyTokenRows(t *testing.T) {
	db, pricing := issue036PreparePricingDB(t)
	legacy := []*model.Price{
		{Model: "stable-image-core", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 15, Output: 15},
		{Model: "sd3", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 32.5, Output: 32.5},
		{Model: "sd3-turbo", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 20, Output: 20},
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	if err := pricing.SyncPricing(issue036StabilityDefaults(t), string(model.PriceUpdateModeSystem)); err != nil {
		t.Fatalf("system add-only 同步失败：%v", err)
	}
	if pricing.PublishedVersion() != 1 {
		t.Fatalf("已有旧行被 system 同步改变 version=%d，want=1", pricing.PublishedVersion())
	}

	// 用新的 Pricing 实例模拟重启；数据库中的旧 tokens 形状仍是事实来源。
	restarted := &model.Pricing{Prices: make(map[string]*model.Price)}
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	if restarted.PublishedVersion() != 1 {
		t.Fatalf("重启后旧目录 version=%d，want=1", restarted.PublishedVersion())
	}
	for _, want := range legacy {
		got, ok := restarted.FindExactPrice(want.Model)
		if !ok || got.Type != model.TokensPriceType || got.Input != want.Input || got.Output != want.Output {
			t.Fatalf("system/restart 覆盖了既有 tokens 行：want=%+v got=%+v exists=%v", want, got, ok)
		}
	}
}

func TestIssue036TargetedPriceCASUpdatesOnlyApprovedRowsAndSurvivesRestart(t *testing.T) {
	db, pricing := issue036PreparePricingDB(t)
	legacy := []*model.Price{
		{Model: "stable-image-core", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 15, Output: 15},
		{Model: "sd3", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 32.5, Output: 32.5},
		{Model: "sd3-turbo", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 20, Output: 20},
		{Model: "issue036-untouched", Type: model.TokensPriceType, ChannelType: config.ChannelTypeOpenAI, Input: 7, Output: 8},
		{Model: "issue036-custom-times", Type: model.TimesPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 99, Output: 0},
		{Model: "issue036-locked", Type: model.TokensPriceType, ChannelType: config.ChannelTypeStabilityAI, Input: 4, Output: 5, Locked: true},
	}
	if err := db.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}

	source := issue036StabilityDefaults(t)
	source = append(source, &model.Price{
		Model: "issue036-locked", Type: model.TimesPriceType, ChannelType: config.ChannelTypeStabilityAI,
		Input: 123, Output: 0, Locked: true,
	})
	preview, err := model.PreviewPriceChange(context.Background(), source, model.PriceUpdateModeUpdate)
	if err != nil {
		t.Fatalf("生成 I036 定向发布 preview 失败：%v", err)
	}
	if preview.BaseVersion != 1 || preview.Digest == "" {
		t.Fatalf("I036 preview identity 错误：%+v", preview)
	}
	if _, err := model.BumpPublicationVersionCAS(context.Background(), db, model.PublicationOwnerPrice, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := model.ApplyPriceChange(context.Background(), pricing, source, model.PriceUpdateModeUpdate, preview.BaseVersion, preview.Digest); !errors.Is(err, model.ErrPublicationVersionConflict) {
		t.Fatalf("旧 I036 preview 未被 CAS 拒绝：%v", err)
	}
	var unchanged model.Price
	if err := db.Where("model = ?", "stable-image-core").Take(&unchanged).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Type != model.TokensPriceType || unchanged.Input != 15 || unchanged.Output != 15 {
		t.Fatalf("CAS 冲突仍修改了目标行：%+v", unchanged)
	}

	if err := pricing.Init(); err != nil {
		t.Fatal(err)
	}
	preview, err = model.PreviewPriceChange(context.Background(), source, model.PriceUpdateModeUpdate)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := model.ApplyPriceChange(context.Background(), pricing, source, model.PriceUpdateModeUpdate, preview.BaseVersion, preview.Digest)
	if err != nil || newVersion != 3 || pricing.PublishedVersion() != 3 {
		t.Fatalf("I036 定向 CAS 发布失败：version=%d published=%d err=%v", newVersion, pricing.PublishedVersion(), err)
	}

	for _, want := range []struct {
		model  string
		type_  string
		input  float64
		output float64
		locked bool
	}{
		{model: "stable-image-core", type_: model.TimesPriceType, input: 15, output: 0},
		{model: "sd3", type_: model.TimesPriceType, input: 32.5, output: 0},
		{model: "sd3-turbo", type_: model.TimesPriceType, input: 20, output: 0},
		{model: "issue036-untouched", type_: model.TokensPriceType, input: 7, output: 8},
		{model: "issue036-custom-times", type_: model.TimesPriceType, input: 99, output: 0},
		{model: "issue036-locked", type_: model.TokensPriceType, input: 4, output: 5, locked: true},
	} {
		var got model.Price
		if err := db.Where("model = ?", want.model).Take(&got).Error; err != nil {
			t.Fatal(err)
		}
		if got.Type != want.type_ || got.Input != want.input || got.Output != want.output || got.Locked != want.locked {
			t.Fatalf("I036 定向发布越界或未更新：want=%+v got=%+v", want, got)
		}
	}

	restarted := &model.Pricing{Prices: make(map[string]*model.Price)}
	if err := restarted.Init(); err != nil {
		t.Fatal(err)
	}
	if restarted.PublishedVersion() != 3 {
		t.Fatalf("定向发布后重启 version=%d，want=3", restarted.PublishedVersion())
	}
	for _, modelName := range []string{"stable-image-core", "sd3", "sd3-turbo"} {
		price, ok := restarted.FindExactPrice(modelName)
		if !ok || price.Type != model.TimesPriceType || price.Output != 0 {
			t.Fatalf("重启后 Stability 默认未保持：model=%s price=%+v exists=%v", modelName, price, ok)
		}
	}
}

func issue036PreparePricingDB(t *testing.T) (*gorm.DB, *model.Pricing) {
	t.Helper()
	originalDB := model.DB
	originalPricing := model.PricingInstance
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("创建 I036 pricing SQLite 失败：%v", err)
	}
	if err := db.AutoMigrate(&model.Price{}, &model.ModelInfo{}, &model.PublicationVersion{}); err != nil {
		t.Fatalf("迁移 I036 pricing SQLite 失败：%v", err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatalf("初始化 I036 publication version 失败：%v", err)
	}
	model.DB = db
	pricing := &model.Pricing{Prices: make(map[string]*model.Price)}
	model.PricingInstance = pricing
	t.Cleanup(func() {
		model.DB = originalDB
		model.PricingInstance = originalPricing
	})
	return db, pricing
}

func issue036StabilityDefaults(t *testing.T) []*model.Price {
	t.Helper()
	byModel := make(map[string]*model.Price, 3)
	for _, price := range model.GetDefaultPrice() {
		if price != nil {
			switch price.Model {
			case "stable-image-core", "sd3", "sd3-turbo":
				byModel[price.Model] = price
			}
		}
	}
	defaults := make([]*model.Price, 0, 3)
	for _, modelName := range []string{"stable-image-core", "sd3", "sd3-turbo"} {
		price, ok := byModel[modelName]
		if !ok {
			t.Fatalf("缺少 I036 默认价格：%s", modelName)
		}
		defaults = append(defaults, price)
	}
	return defaults
}

func findPriceInList(prices []*model.Price, modelName string) (*model.Price, bool) {
	for _, price := range prices {
		if price != nil && price.Model == modelName {
			return price, true
		}
	}
	return nil, false
}
