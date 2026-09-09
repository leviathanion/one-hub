package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/notify"
	"one-api/common/requester"
	"one-api/model"

	"gorm.io/gorm"
)

type issue052ControllerTraceKey struct{}

type issue052ControllerNotifier struct {
	name     string
	endpoint string
	calls    atomic.Int32
	contexts chan context.Context
}

func (n *issue052ControllerNotifier) Name() string {
	return n.name
}

func (n *issue052ControllerNotifier) Send(ctx context.Context, title, message string) error {
	n.calls.Add(1)
	n.contexts <- ctx
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.endpoint, nil)
	if err != nil {
		return err
	}
	resp, err := requester.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return errors.New("issue052 test endpoint rejected notification")
	}
	return nil
}

func TestIssue052AutoDisableNotifiesOnlyAfterSuccessfulCAS(t *testing.T) {
	useControllerTestChannelDB(t)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	oldRedisEnabled := config.RedisEnabled
	config.RedisEnabled = false
	t.Cleanup(func() { config.RedisEnabled = oldRedisEnabled })

	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(func() { server.Close() })

	notifier := &issue052ControllerNotifier{
		name:     "issue052-controller",
		endpoint: server.URL,
		contexts: make(chan context.Context, 2),
	}
	notify.AddNotifiers(notifier)

	channelID := 52052
	insertControllerTestChannel(t, &model.Channel{
		Id:     channelID,
		Type:   config.ChannelTypeOpenAI,
		Name:   "issue052-controller-channel",
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "issue052-model",
		Key:    "issue052-channel-key",
	})

	var successfulUpdates atomic.Int32
	if err := model.DB.Callback().Update().After("gorm:update").Register("issue052_count_successful_channel_updates", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "channels" && tx.RowsAffected > 0 {
			successfulUpdates.Add(1)
		}
	}); err != nil {
		t.Fatalf("注册 CAS 观察器失败：%v", err)
	}

	requestCtx := context.WithValue(context.Background(), issue052ControllerTraceKey{}, "issue052-controller-trace")
	updated, err := AutoDisableChannel(channelID, "issue052-controller-channel", "unauthorized", true, requestCtx)
	if err != nil || !updated {
		t.Fatalf("首次自动禁用应提交 CAS 并通知：updated=%v err=%v", updated, err)
	}
	var stored model.Channel
	if err := model.DB.First(&stored, channelID).Error; err != nil {
		t.Fatalf("读取自动禁用渠道失败：%v", err)
	}
	if stored.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("渠道状态未变为自动禁用：%d", stored.Status)
	}
	if successfulUpdates.Load() != 1 || notifier.calls.Load() != 1 || received.Load() != 1 {
		t.Fatalf("首次 CAS/通知次数不正确：cas=%d notifier=%d http=%d", successfulUpdates.Load(), notifier.calls.Load(), received.Load())
	}

	secondUpdated, err := AutoDisableChannel(channelID, "issue052-controller-channel", "same-error", true, requestCtx)
	if err != nil || secondUpdated {
		t.Fatalf("重复错误不应再次成功禁用：updated=%v err=%v", secondUpdated, err)
	}
	if successfulUpdates.Load() != 1 || notifier.calls.Load() != 1 || received.Load() != 1 {
		t.Fatalf("重复错误不应重复 CAS/通知：cas=%d notifier=%d http=%d", successfulUpdates.Load(), notifier.calls.Load(), received.Load())
	}

	preCanceledChannelID := channelID + 1
	insertControllerTestChannel(t, &model.Channel{
		Id:     preCanceledChannelID,
		Type:   config.ChannelTypeOpenAI,
		Name:   "issue052-pre-canceled-channel",
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "issue052-model",
		Key:    "issue052-pre-canceled-key",
	})
	preCanceledContext, cancel := context.WithCancel(context.WithValue(context.Background(), issue052ControllerTraceKey{}, "issue052-pre-canceled-trace"))
	cancel()
	updated, err = AutoDisableChannel(preCanceledChannelID, "issue052-pre-canceled-channel", "unauthorized", true, preCanceledContext)
	if err != nil || !updated {
		t.Fatalf("预取消请求仍应完成自动禁用通知：updated=%v err=%v", updated, err)
	}
	if successfulUpdates.Load() != 2 || notifier.calls.Load() != 2 || received.Load() != 2 {
		t.Fatalf("预取消请求的 CAS/通知次数不正确：cas=%d notifier=%d http=%d", successfulUpdates.Load(), notifier.calls.Load(), received.Load())
	}

	select {
	case ctx := <-notifier.contexts:
		if got := ctx.Value(issue052ControllerTraceKey{}); got != "issue052-controller-trace" {
			t.Fatalf("通知丢失请求追踪值：%v", got)
		}
		deadline, ok := ctx.Deadline()
		if !ok || !deadline.After(time.Now()) {
			t.Fatalf("通知 context 没有有效独立 deadline：%v %v", deadline, ok)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("通知返回后 context 未 cancel：%v", ctx.Err())
		}
	default:
		t.Fatal("未观察到首次通知 context")
	}
	select {
	case ctx := <-notifier.contexts:
		if got := ctx.Value(issue052ControllerTraceKey{}); got != "issue052-pre-canceled-trace" {
			t.Fatalf("预取消通知丢失请求追踪值：%v", got)
		}
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("预取消通知返回后 context 未 cancel：%v", ctx.Err())
		}
	default:
		t.Fatal("未观察到预取消通知 context")
	}
}
