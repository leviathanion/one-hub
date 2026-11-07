package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"
	"time"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"
	"one-api/providers/antigravity"

	"github.com/gin-gonic/gin"
)

func stubAntigravityUsage(t *testing.T) {
	t.Helper()
	load, fetch := loadAntigravityUsageChannel, fetchAntigravityUsage
	t.Cleanup(func() {
		loadAntigravityUsageChannel, fetchAntigravityUsage = load, fetch
	})
}

func TestAntigravityUsageRejectsInvalidTargetsBeforeProviderWork(t *testing.T) {
	stubAntigravityUsage(t)
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name, id string
		channel  *model.Channel
		loadErr  error
	}{
		{name: "invalid id", id: "bad"},
		{name: "nonpositive id", id: "0"},
		{name: "missing channel", id: "7", loadErr: errors.New("missing")},
		{name: "wrong type", id: "7", channel: &model.Channel{Type: config.ChannelTypeOpenAI, Key: "key"}},
		{name: "not authorized", id: "7", channel: &model.Channel{Type: config.ChannelTypeAntigravity, Key: " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loadAntigravityUsageChannel = func(context.Context, int) (*model.Channel, error) { return tc.channel, tc.loadErr }
			fetchAntigravityUsage = func(context.Context, *model.Channel) (*antigravity.UsageSnapshot, error) {
				t.Fatal("invalid target reached provider")
				return nil, nil
			}
			ctx, rec := commonTest.GetContext(http.MethodGet, "/api/channel/7/antigravity/usage", commonTest.RequestJSONConfig(), nil)
			ctx.Params = gin.Params{{Key: "id", Value: tc.id}}
			GetAntigravityChannelUsage(ctx)
			var resp struct{ Success bool }
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Success {
				t.Fatalf("invalid target accepted: %s, err=%v", rec.Body, err)
			}
		})
	}
}

func TestAntigravityUsageAllowsTaggedChannelAndReturnsSnapshot(t *testing.T) {
	stubAntigravityUsage(t)
	gin.SetMode(gin.TestMode)
	loadAntigravityUsageChannel = func(ctx context.Context, id int) (*model.Channel, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 15*time.Second {
			t.Fatal("lookup is not bounded by management request deadline")
		}
		return &model.Channel{Id: id, Type: config.ChannelTypeAntigravity, Tag: "team", Key: "credentials"}, nil
	}
	fetchAntigravityUsage = func(ctx context.Context, channel *model.Channel) (*antigravity.UsageSnapshot, error) {
		if channel.Id != 7 || channel.Tag != "team" || ctx.Err() != nil {
			t.Fatal("tagged channel or request context lost")
		}
		zero, full := 0.0, 1.0
		return &antigravity.UsageSnapshot{
			ChannelID: channel.Id,
			FetchedAt: 123,
			Models: []antigravity.ModelQuota{
				{ModelID: "empty-model", DisplayName: "已用尽模型", RemainingFraction: &zero, ResetTime: "2026-11-08T12:34:56Z"},
				{ModelID: "full-model", RemainingFraction: &full},
				{ModelID: "unknown-model"},
			},
		}, nil
	}
	ctx, rec := commonTest.GetContext(http.MethodGet, "/api/channel/7/antigravity/usage", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}
	GetAntigravityChannelUsage(ctx)
	// 使用独立 JSON 样本检查客户端实际消费的字段，避免生产 DTO 的错误 tag
	// 在编码和解码两端同时生效而掩盖契约变化。
	wantJSON := `{
		"success":true,"message":"",
		"data":{"channel_id":7,"fetched_at":123,"models":[
			{"model_id":"empty-model","display_name":"已用尽模型","remaining_fraction":0,"reset_time":"2026-11-08T12:34:56Z"},
			{"model_id":"full-model","remaining_fraction":1},
			{"model_id":"unknown-model"}
		]}
	}`
	var got, want any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(wantJSON), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage JSON contract changed: got=%s want=%s", rec.Body, wantJSON)
	}
}

func TestAntigravityUsagePropagatesCancellationAndDoesNotReturnStaleSuccess(t *testing.T) {
	stubAntigravityUsage(t)
	gin.SetMode(gin.TestMode)
	loadAntigravityUsageChannel = func(context.Context, int) (*model.Channel, error) {
		return &model.Channel{Id: 7, Type: config.ChannelTypeAntigravity, Key: "credentials"}, nil
	}
	calls := 0
	fetchAntigravityUsage = func(ctx context.Context, _ *model.Channel) (*antigravity.UsageSnapshot, error) {
		calls++
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatal("request cancellation lost")
		}
		return nil, ctx.Err()
	}
	ctx, rec := commonTest.GetContext(http.MethodGet, "/api/channel/7/antigravity/usage", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}
	canceled, cancel := context.WithCancel(ctx.Request.Context())
	cancel()
	ctx.Request = ctx.Request.WithContext(canceled)
	GetAntigravityChannelUsage(ctx)
	var resp struct{ Success bool }
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Success || calls != 1 {
		t.Fatalf("canceled usage response=%s calls=%d err=%v", rec.Body, calls, err)
	}
}
