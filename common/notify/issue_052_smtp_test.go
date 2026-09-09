package notify

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/notify/channel"
	"one-api/common/requester"
)

func TestIssue052SMTPGreetingCannotBlockNotificationCompletion(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	connectionClosed := make(chan struct{})
	go func() {
		defer close(connectionClosed)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(notificationSendTimeout + 3*time.Second))
		accepted <- conn
		_, _ = io.Copy(io.Discard, conn)
	}()
	previousOptions := config.GlobalOption
	previousHost, previousPort := config.SMTPServer, config.SMTPPort
	previousAccount, previousToken, previousFrom := config.SMTPAccount, config.SMTPToken, config.SMTPFrom
	config.GlobalOption = config.NewOptionManager()
	config.SMTPServer, config.SMTPPort = "localhost", listener.Addr().(*net.TCPAddr).Port
	config.SMTPAccount, config.SMTPToken, config.SMTPFrom = "sender@example.com", "local-test", "sender@example.com"
	t.Cleanup(func() {
		config.GlobalOption = previousOptions
		config.SMTPServer, config.SMTPPort = previousHost, previousPort
		config.SMTPAccount, config.SMTPToken, config.SMTPFrom = previousAccount, previousToken, previousFrom
	})
	healthyDone := make(chan struct{}, 1)
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthyDone <- struct{}{}
		_, _ = io.WriteString(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer healthy.Close()
	previousHTTPClient := requester.HTTPClient
	requester.HTTPClient = healthy.Client()
	t.Cleanup(func() { requester.HTTPClient = previousHTTPClient })
	notifier := New()
	notifier.addChannels(channel.NewEmail("to@example.com"), channel.NewWeCom(healthy.URL))
	done := make(chan struct{})
	go func() {
		notifier.Send(context.Background(), "SMTP 生命周期", "真实 HTTP 通道继续发送")
		close(done)
	}()
	select {
	case conn := <-accepted:
		defer conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP 未进入等待 greeting 的阶段")
	}
	select {
	case <-healthyDone:
	case <-time.After(2 * time.Second):
		t.Fatal("健康 HTTP 通道被 SMTP 阻塞")
	}
	select {
	case <-done:
	case <-time.After(notificationSendTimeout + time.Second):
		t.Fatal("通知期限结束后整体调用仍未返回")
	}
	select {
	case <-connectionClosed:
	case <-time.After(time.Second):
		t.Fatal("通知返回后 SMTP 连接未释放")
	}
}
