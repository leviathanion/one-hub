package notify

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/notify/channel"
	"one-api/common/requester"
)

type issue052TraceKey struct{}

type issue052HTTPServer struct {
	server    *httptest.Server
	delay     time.Duration
	started   atomic.Int32
	completed atomic.Int32
}

func newIssue052HTTPServer(t *testing.T, delay time.Duration) *issue052HTTPServer {
	return newIssue052HTTPServerWithGate(t, delay, nil, nil)
}

func newIssue052HTTPServerWithGate(t *testing.T, delay time.Duration, entered chan<- struct{}, release <-chan struct{}) *issue052HTTPServer {
	t.Helper()
	httpServer := &issue052HTTPServer{delay: delay}
	httpServer.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		httpServer.started.Add(1)
		if entered != nil {
			entered <- struct{}{}
			select {
			case <-release:
			case <-req.Context().Done():
				return
			}
		}
		if delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-req.Context().Done():
				return
			}
		}
		httpServer.completed.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if req.URL.Path == "/message/push" {
			_, _ = io.WriteString(w, `{"code":0,"message":"ok"}`)
			return
		}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(func() { httpServer.server.Close() })
	return httpServer
}

type issue052ObservedNotifier struct {
	name     string
	delegate Notifier
	contexts chan context.Context
}

func (n *issue052ObservedNotifier) Name() string {
	return n.name
}

func (n *issue052ObservedNotifier) Send(ctx context.Context, title, message string) error {
	n.contexts <- ctx
	return n.delegate.Send(ctx, title, message)
}

func newIssue052RealHTTPNotifiers(t *testing.T, slowFirst bool, slow, healthy *issue052HTTPServer) (*Notify, <-chan context.Context, <-chan context.Context) {
	t.Helper()
	slowNotifier := &issue052ObservedNotifier{
		name:     "issue052-slow",
		delegate: channel.NewWeCom(slow.server.URL),
		contexts: make(chan context.Context, 1),
	}
	healthyNotifier := &issue052ObservedNotifier{
		name:     "issue052-healthy",
		delegate: channel.NewPushdeer("issue052-key", healthy.server.URL),
		contexts: make(chan context.Context, 1),
	}
	notify := New()
	if slowFirst {
		notify.addChannels(slowNotifier, healthyNotifier)
	} else {
		notify.addChannels(healthyNotifier, slowNotifier)
	}
	return notify, slowNotifier.contexts, healthyNotifier.contexts
}

func assertIssue052NotifierContext(t *testing.T, ctx context.Context, parentDeadline time.Time, label string) {
	t.Helper()
	if ctx == nil {
		t.Fatalf("%s 未收到 context", label)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatalf("%s 没有独立 deadline", label)
	}
	if !deadline.After(parentDeadline) {
		t.Fatalf("%s 继承了已耗尽的 health deadline：%s <= %s", label, deadline, parentDeadline)
	}
	if remaining := time.Until(deadline); remaining > notificationSendTimeout || remaining <= 0 {
		t.Fatalf("%s deadline 不在独立通知预算内：剩余 %s", label, remaining)
	}
	if got := ctx.Value(issue052TraceKey{}); got != "issue052-trace" {
		t.Fatalf("%s 丢失追踪值：%v", label, got)
	}
}

func TestIssue052NotifySendsConfiguredHTTPChannelsWithIndependentBudgets(t *testing.T) {
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	for _, slowFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "slow_added_first", false: "healthy_added_first"}[slowFirst], func(t *testing.T) {
			t.Parallel()
			slow := newIssue052HTTPServer(t, 6*time.Second)
			healthy := newIssue052HTTPServer(t, 0)
			notify, slowContexts, healthyContexts := newIssue052RealHTTPNotifiers(t, slowFirst, slow, healthy)

			parent, cancel := context.WithTimeout(context.WithValue(context.Background(), issue052TraceKey{}, "issue052-trace"), 5*time.Second)
			parentDeadline, _ := parent.Deadline()
			started := time.Now()
			notify.Send(parent, "I052", "slow notifier must not block healthy notifier")
			elapsed := time.Since(started)

			if slow.completed.Load() != 1 || healthy.completed.Load() != 1 {
				t.Fatalf("两路本地 HTTP 都应完成：slow started=%d completed=%d healthy started=%d completed=%d", slow.started.Load(), slow.completed.Load(), healthy.started.Load(), healthy.completed.Load())
			}
			if elapsed < 5500*time.Millisecond || elapsed > 9*time.Second {
				t.Fatalf("当前调用应等待两路有限通知收尾，耗时 %s", elapsed)
			}

			var slowCtx, healthyCtx context.Context
			select {
			case slowCtx = <-slowContexts:
			default:
				t.Fatal("慢 HTTP notifier 未被调用")
			}
			select {
			case healthyCtx = <-healthyContexts:
			default:
				t.Fatal("健康 HTTP notifier 未被调用")
			}
			assertIssue052NotifierContext(t, slowCtx, parentDeadline, "慢 notifier")
			assertIssue052NotifierContext(t, healthyCtx, parentDeadline, "健康 notifier")
			if slowCtx == healthyCtx {
				t.Fatal("两路 notifier 不应共享同一个 context")
			}
			if !errors.Is(slowCtx.Err(), context.Canceled) || !errors.Is(healthyCtx.Err(), context.Canceled) {
				t.Fatalf("每路通知 context 都应在当前调用收尾后 cancel：slow=%v healthy=%v", slowCtx.Err(), healthyCtx.Err())
			}
			cancel()
		})
	}
}

func TestIssue052NotifyStartsBothHTTPChannelsBeforeEitherGateReleases(t *testing.T) {
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	slow := newIssue052HTTPServerWithGate(t, 0, entered, release)
	healthy := newIssue052HTTPServerWithGate(t, 0, entered, release)
	notify, slowContexts, healthyContexts := newIssue052RealHTTPNotifiers(t, true, slow, healthy)
	done := make(chan struct{})
	go func() {
		notify.Send(context.Background(), "I052", "both HTTP channels must enter before release")
		close(done)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("并行通知未让两路 HTTP 都进入 gate")
		}
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("释放 gate 后通知没有收尾")
	}
	if slow.completed.Load() != 1 || healthy.completed.Load() != 1 {
		t.Fatalf("两路 HTTP gate 释放后都应完成：slow=%d healthy=%d", slow.completed.Load(), healthy.completed.Load())
	}
	select {
	case <-slowContexts:
	default:
		t.Fatal("慢 notifier 未进入")
	}
	select {
	case <-healthyContexts:
	default:
		t.Fatal("健康 notifier 未进入")
	}
}

func TestIssue052NotifyContinuesAfterFastHTTPRejection(t *testing.T) {
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })

	var rejected atomic.Int32
	rejectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		rejected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"errcode":1,"errmsg":"rejected"}`)
	}))
	t.Cleanup(func() { rejectServer.Close() })
	healthy := newIssue052HTTPServer(t, 0)
	slowRejected := &issue052ObservedNotifier{
		name:     "issue052-fast-reject",
		delegate: channel.NewWeCom(rejectServer.URL),
		contexts: make(chan context.Context, 1),
	}
	healthyNotifier := &issue052ObservedNotifier{
		name:     "issue052-fast-reject-healthy",
		delegate: channel.NewPushdeer("issue052-key", healthy.server.URL),
		contexts: make(chan context.Context, 1),
	}
	notify := New()
	notify.addChannels(slowRejected, healthyNotifier)
	notify.Send(context.Background(), "I052", "one notifier rejects quickly")
	if rejected.Load() != 1 || healthy.completed.Load() != 1 {
		t.Fatalf("快速拒绝不应阻断健康通知：rejected=%d healthy=%d", rejected.Load(), healthy.completed.Load())
	}
}

func TestIssue052NotifyWaitsForAndBoundsSlowNotifier(t *testing.T) {
	oldHTTPClient := requester.HTTPClient
	requester.HTTPClient = &http.Client{}
	t.Cleanup(func() { requester.HTTPClient = oldHTTPClient })
	slow := newIssue052HTTPServer(t, 11*time.Second)
	notifier := &issue052ObservedNotifier{
		name:     "issue052-deadline",
		delegate: channel.NewWeCom(slow.server.URL),
		contexts: make(chan context.Context, 1),
	}
	notify := New()
	notify.addChannel(notifier)
	started := time.Now()
	notify.Send(context.Background(), "I052", "slow notifier deadline")
	if elapsed := time.Since(started); elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("慢 notifier 未在独立 10s deadline 内被等待并收尾：耗时 %s", elapsed)
	}
	if slow.started.Load() != 1 || slow.completed.Load() != 0 {
		t.Fatalf("慢 HTTP 应在独立 deadline 触发后退出：started=%d completed=%d", slow.started.Load(), slow.completed.Load())
	}
	select {
	case ctx := <-notifier.contexts:
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("慢 notifier context 没有 deadline")
		}
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatalf("慢 notifier 应由独立 deadline 结束：%v", ctx.Err())
		}
	default:
		t.Fatal("未观察到慢 notifier context")
	}
}
