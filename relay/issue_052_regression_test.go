package relay

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
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type issue052RelayTraceKey struct{}

type issue052RelayHTTPServer struct {
	server    *httptest.Server
	delay     time.Duration
	started   atomic.Int32
	completed atomic.Int32
}

func newIssue052RelayHTTPServer(t *testing.T, delay time.Duration) *issue052RelayHTTPServer {
	t.Helper()
	endpoint := &issue052RelayHTTPServer{delay: delay}
	endpoint.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		endpoint.started.Add(1)
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-req.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		endpoint.completed.Add(1)
	}))
	t.Cleanup(func() { endpoint.server.Close() })
	return endpoint
}

func waitIssue052RelayProcessing(t *testing.T, done <-chan struct{}) {
	t.Helper()
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		t.Fatal("等待 relay 后处理完成超时")
	}
}

type issue052RelayNotifier struct {
	name     string
	endpoint string
	calls    atomic.Int32
	contexts chan context.Context
}

func (n *issue052RelayNotifier) Name() string {
	return n.name
}

func (n *issue052RelayNotifier) Send(ctx context.Context, title, message string) error {
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

func useIssue052RelayDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatalf("打开 relay I052 测试数据库失败：%v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("获取 relay I052 测试连接池失败：%v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatalf("迁移 relay I052 渠道表失败：%v", err)
	}
	oldDB, oldRedisEnabled := model.DB, config.RedisEnabled
	model.DB = db
	config.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = oldDB
		config.RedisEnabled = oldRedisEnabled
		_ = sqlDB.Close()
	})
	return db
}

func newIssue052RelayContext(t *testing.T, canceled bool) *gin.Context {
	t.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	requestCtx := context.WithValue(context.Background(), issue052RelayTraceKey{}, "issue052-relay-trace")
	if canceled {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithCancel(requestCtx)
		cancel()
	} else {
		var cancel context.CancelFunc
		requestCtx, cancel = context.WithTimeout(requestCtx, 5*time.Second)
		t.Cleanup(cancel)
	}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(requestCtx)
	ctx.Set("new_model", "issue052-model")
	return ctx
}

func TestIssue052RelayObserverWaitsForIndependentHTTPNotifications(t *testing.T) {
	db := useIssue052RelayDB(t)
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	oldAutomaticDisable := config.AutomaticDisableChannelEnabled
	config.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { config.AutomaticDisableChannelEnabled = oldAutomaticDisable })

	channelID := 52052
	if err := db.Create(&model.Channel{
		Id:     channelID,
		Type:   config.ChannelTypeOpenAI,
		Name:   "issue052-relay-channel",
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "issue052-model",
		Key:    "issue052-relay-key",
	}).Error; err != nil {
		t.Fatalf("写入 relay I052 渠道夹具失败：%v", err)
	}
	slow := newIssue052RelayHTTPServer(t, 6*time.Second)
	healthy := newIssue052RelayHTTPServer(t, 0)
	slowNotifier := &issue052RelayNotifier{name: "issue052-relay-slow", endpoint: slow.server.URL, contexts: make(chan context.Context, 2)}
	healthyNotifier := &issue052RelayNotifier{name: "issue052-relay-healthy", endpoint: healthy.server.URL, contexts: make(chan context.Context, 2)}
	notify.AddNotifiers(slowNotifier, healthyNotifier)
	originalProcess := processChannelRelayErrorFunc
	var pendingDone chan struct{}
	processChannelRelayErrorFunc = func(ctx context.Context, channelID int, channelName string, apiErr *types.OpenAIErrorWithStatusCode, channelType int) {
		done := pendingDone
		defer close(done)
		originalProcess(ctx, channelID, channelName, apiErr, channelType)
	}
	t.Cleanup(func() {
		if pendingDone != nil {
			waitIssue052RelayProcessing(t, pendingDone)
		}
		processChannelRelayErrorFunc = originalProcess
	})

	var successfulUpdates atomic.Int32
	if err := db.Callback().Update().After("gorm:update").Register("issue052_relay_count_successful_updates", func(tx *gorm.DB) {
		if tx.Statement.Schema != nil && tx.Statement.Schema.Table == "channels" && tx.RowsAffected > 0 {
			successfulUpdates.Add(1)
		}
	}); err != nil {
		t.Fatalf("注册 relay I052 CAS 观察器失败：%v", err)
	}

	ctx := newIssue052RelayContext(t, false)
	providerErr := &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "unauthorized"},
		StatusCode:  http.StatusUnauthorized,
	}
	pendingDone = make(chan struct{})
	started := time.Now()
	observeRelayProviderFailure(ctx, &model.Channel{Id: channelID, Name: "issue052-relay-channel", Type: config.ChannelTypeOpenAI}, providerErr)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("observer 不应被慢通知阻塞前台调用，耗时 %s", elapsed)
	}
	waitIssue052RelayProcessing(t, pendingDone)
	if slow.completed.Load() != 1 || healthy.completed.Load() != 1 {
		t.Fatalf("慢/健康本地 HTTP 都应完成：slow started=%d completed=%d healthy started=%d completed=%d", slow.started.Load(), slow.completed.Load(), healthy.started.Load(), healthy.completed.Load())
	}
	if successfulUpdates.Load() != 1 || slowNotifier.calls.Load() != 1 || healthyNotifier.calls.Load() != 1 {
		t.Fatalf("首次 observer 的 CAS/通知次数不正确：cas=%d slow=%d healthy=%d", successfulUpdates.Load(), slowNotifier.calls.Load(), healthyNotifier.calls.Load())
	}
	var stored model.Channel
	if err := db.First(&stored, channelID).Error; err != nil {
		t.Fatalf("读取 relay I052 渠道状态失败：%v", err)
	}
	if stored.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("observer 未持久化自动禁用状态：%d", stored.Status)
	}

	var slowCtx, healthyCtx context.Context
	select {
	case slowCtx = <-slowNotifier.contexts:
	default:
		t.Fatal("慢 notifier 未被调用")
	}
	select {
	case healthyCtx = <-healthyNotifier.contexts:
	default:
		t.Fatal("健康 notifier 未被调用")
	}
	for label, notificationCtx := range map[string]context.Context{"slow": slowCtx, "healthy": healthyCtx} {
		deadline, ok := notificationCtx.Deadline()
		if !ok || !deadline.After(time.Now()) {
			t.Fatalf("%s notifier context 没有独立有效 deadline：%v %v", label, deadline, ok)
		}
		if got := notificationCtx.Value(issue052RelayTraceKey{}); got != "issue052-relay-trace" {
			t.Fatalf("%s notifier context 丢失 trace：%v", label, got)
		}
		if !errors.Is(notificationCtx.Err(), context.Canceled) {
			t.Fatalf("%s notifier context 返回后未 cancel：%v", label, notificationCtx.Err())
		}
	}

	pendingDone = make(chan struct{})
	observeRelayProviderFailure(ctx, &model.Channel{Id: channelID, Name: "issue052-relay-channel", Type: config.ChannelTypeOpenAI}, providerErr)
	waitIssue052RelayProcessing(t, pendingDone)
	if successfulUpdates.Load() != 1 || slowNotifier.calls.Load() != 1 || healthyNotifier.calls.Load() != 1 {
		t.Fatalf("重复 provider error 不应重复 CAS/通知：cas=%d slow=%d healthy=%d", successfulUpdates.Load(), slowNotifier.calls.Load(), healthyNotifier.calls.Load())
	}

	preCanceledChannelID := channelID + 1
	if err := db.Create(&model.Channel{
		Id:     preCanceledChannelID,
		Type:   config.ChannelTypeOpenAI,
		Name:   "issue052-pre-canceled-relay-channel",
		Status: config.ChannelStatusEnabled,
		Group:  "default",
		Models: "issue052-model",
		Key:    "issue052-pre-canceled-relay-key",
	}).Error; err != nil {
		t.Fatalf("写入预取消 relay 渠道夹具失败：%v", err)
	}
	preCanceledCtx := newIssue052RelayContext(t, true)
	pendingDone = make(chan struct{})
	started = time.Now()
	observeRelayProviderFailure(preCanceledCtx, &model.Channel{Id: preCanceledChannelID, Name: "issue052-pre-canceled-relay-channel", Type: config.ChannelTypeOpenAI}, providerErr)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("预取消 observer 不应被慢通知阻塞前台调用，耗时 %s", elapsed)
	}
	waitIssue052RelayProcessing(t, pendingDone)
	if successfulUpdates.Load() != 2 || slowNotifier.calls.Load() != 2 || healthyNotifier.calls.Load() != 2 {
		t.Fatalf("预取消 provider error 的 CAS/通知次数不正确：cas=%d slow=%d healthy=%d", successfulUpdates.Load(), slowNotifier.calls.Load(), healthyNotifier.calls.Load())
	}
	stored = model.Channel{}
	if err := db.First(&stored, preCanceledChannelID).Error; err != nil {
		t.Fatalf("读取预取消 relay 渠道状态失败：%v", err)
	}
	if stored.Status != config.ChannelStatusAutoDisabled {
		t.Fatalf("预取消 provider error 未持久化自动禁用状态：%d", stored.Status)
	}
	select {
	case preCanceledNotificationCtx := <-slowNotifier.contexts:
		if got := preCanceledNotificationCtx.Value(issue052RelayTraceKey{}); got != "issue052-relay-trace" {
			t.Fatalf("预取消慢 notifier 丢失 trace：%v", got)
		}
		if !errors.Is(preCanceledNotificationCtx.Err(), context.Canceled) {
			t.Fatalf("预取消慢 notifier 返回后未 cancel：%v", preCanceledNotificationCtx.Err())
		}
	default:
		t.Fatal("未观察到预取消慢 notifier")
	}
	select {
	case preCanceledNotificationCtx := <-healthyNotifier.contexts:
		if got := preCanceledNotificationCtx.Value(issue052RelayTraceKey{}); got != "issue052-relay-trace" {
			t.Fatalf("预取消健康 notifier 丢失 trace：%v", got)
		}
		if !errors.Is(preCanceledNotificationCtx.Err(), context.Canceled) {
			t.Fatalf("预取消健康 notifier 返回后未 cancel：%v", preCanceledNotificationCtx.Err())
		}
	default:
		t.Fatal("未观察到预取消健康 notifier")
	}
}

func TestIssue052RelayObserverSkipsLocalErrorsBeforeSQLAndNotification(t *testing.T) {
	db := useIssue052RelayDB(t)
	oldAutomaticDisable := config.AutomaticDisableChannelEnabled
	config.AutomaticDisableChannelEnabled = true
	t.Cleanup(func() { config.AutomaticDisableChannelEnabled = oldAutomaticDisable })
	channelID := 52053
	if err := db.Create(&model.Channel{Id: channelID, Type: config.ChannelTypeOpenAI, Name: "issue052-local", Status: config.ChannelStatusEnabled, Group: "default", Models: "issue052-model", Key: "issue052-local-key"}).Error; err != nil {
		t.Fatal(err)
	}
	observeRelayProviderFailure(newIssue052RelayContext(t, true), &model.Channel{Id: channelID, Name: "issue052-local", Type: config.ChannelTypeOpenAI}, &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{Message: "local"},
		StatusCode:  http.StatusUnauthorized,
		LocalError:  true,
	})
	var stored model.Channel
	if err := db.First(&stored, channelID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != config.ChannelStatusEnabled {
		t.Fatalf("local error 不应触发自动禁用：%d", stored.Status)
	}
}
