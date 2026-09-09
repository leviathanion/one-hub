package stmp

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wneessen/go-mail"
)

type smtpLifecycleServer struct {
	port      int
	reached   chan struct{}
	closed    chan struct{}
	delivered atomic.Int32
	tlsConfig *tls.Config
}

func newSMTPLifecycleServer(t *testing.T, stall string) *smtpLifecycleServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	certificateServer := httptest.NewTLSServer(http.NotFoundHandler())
	certificate := certificateServer.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(certificateServer.Certificate())
	certificateServer.Close()
	server := &smtpLifecycleServer{
		port: listener.Addr().(*net.TCPAddr).Port, reached: make(chan struct{}), closed: make(chan struct{}),
		tlsConfig: &tls.Config{ServerName: "example.com", RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	go func() {
		defer close(server.closed)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		blocked := func(stage string) bool {
			if stage != stall {
				return false
			}
			close(server.reached)
			_, _ = io.Copy(io.Discard, conn)
			return true
		}
		if blocked("greeting") {
			return
		}
		_, _ = io.WriteString(conn, "220 localhost\r\n")
		reader := bufio.NewReader(conn)
		encrypted := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			fields := strings.Fields(line)
			if len(fields) == 0 {
				return
			}
			command := fields[0]
			if blocked(command) {
				return
			}
			switch command {
			case "EHLO", "HELO":
				if encrypted {
					_, _ = io.WriteString(conn, "250-localhost\r\n250 AUTH PLAIN LOGIN\r\n")
				} else {
					_, _ = io.WriteString(conn, "250-localhost\r\n250-STARTTLS\r\n250 AUTH PLAIN LOGIN\r\n")
				}
			case "STARTTLS":
				_, _ = io.WriteString(conn, "220 start TLS\r\n")
				if blocked("tls_handshake") {
					return
				}
				tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12})
				if err := tlsConn.Handshake(); err != nil {
					return
				}
				conn, reader, encrypted = tlsConn, bufio.NewReader(tlsConn), true
			case "AUTH":
				_, _ = io.WriteString(conn, "235 authenticated\r\n")
			case "MAIL", "RCPT", "NOOP", "RSET":
				_, _ = io.WriteString(conn, "250 ok\r\n")
			case "DATA":
				_, _ = io.WriteString(conn, "354 send data\r\n")
				for {
					line, err = reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == ".\r\n" {
						break
					}
				}
				server.delivered.Add(1)
				if blocked("data_reply") {
					return
				}
				_, _ = io.WriteString(conn, "250 queued\r\n")
			case "QUIT":
				_, _ = io.WriteString(conn, "221 bye\r\n")
				return
			default:
				t.Errorf("未预期的 SMTP 命令: %s", command)
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-server.closed:
		case <-time.After(6 * time.Second):
			t.Error("SMTP 测试连接未释放")
		}
	})
	return server
}

func sendWithLifecycleServer(ctx context.Context, server *smtpLifecycleServer) error {
	sender := NewStmp("localhost", server.port, "sender@example.com", "local-test", "sender@example.com")
	client, cleanup, err := sender.newClient(ctx, server.tlsConfig)
	if err != nil {
		return err
	}
	defer cleanup()
	message := mail.NewMsg()
	if err := message.From(sender.From); err != nil {
		return err
	}
	if err := message.To("to@example.com"); err != nil {
		return err
	}
	message.Subject("主题")
	message.SetBodyString(mail.TypeTextHTML, "正文")
	return dialAndSend(ctx, client, message)
}

func TestIssue052SMTPCancellationClosesEveryProtocolPhase(t *testing.T) {
	for _, phase := range []string{"greeting", "EHLO", "tls_handshake", "AUTH", "MAIL", "DATA", "data_reply", "RSET", "QUIT"} {
		t.Run(phase, func(t *testing.T) {
			server := newSMTPLifecycleServer(t, phase)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- sendWithLifecycleServer(ctx, server) }()
			select {
			case <-server.reached:
			case err := <-result:
				t.Fatalf("未进入 %s: %v", phase, err)
			case <-time.After(2 * time.Second):
				t.Fatal("未进入目标 SMTP 阶段")
			}
			cancel()
			select {
			case err := <-result:
				// DATA 已被 250 确认后，RSET/QUIT 失败不能把已投递邮件改成未投递。
				if phase == "RSET" || phase == "QUIT" {
					if err != nil || server.delivered.Load() != 1 {
						t.Fatalf("已确认投递的结果被改写: %v", err)
					}
				} else if err == nil {
					t.Fatal("未完成的发送被取消后仍返回成功")
				}
			case <-time.After(time.Second):
				t.Fatal("取消未终止 SMTP I/O")
			}
			select {
			case <-server.closed:
			case <-time.After(time.Second):
				t.Fatal("发送已返回但连接未关闭")
			}
		})
	}
}

func TestIssue052SMTPDeadlineAndSuccessfulSend(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		server := newSMTPLifecycleServer(t, "greeting")
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		err := NewStmp("localhost", server.port, "sender@example.com", "test", "sender@example.com").SendContext(ctx, "to@example.com", "主题", "正文")
		if err == nil || time.Since(started) > time.Second {
			t.Fatalf("SMTP deadline 未生效: %v", err)
		}
	})
	t.Run("success", func(t *testing.T) {
		server := newSMTPLifecycleServer(t, "")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := sendWithLifecycleServer(ctx, server); err != nil {
			t.Fatal(err)
		}
		if server.delivered.Load() != 1 {
			t.Fatalf("成功邮件投递次数=%d", server.delivered.Load())
		}
	})
	t.Run("cancelled_before_dial", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := NewStmp("unused.invalid", 25, "sender@example.com", "test", "sender@example.com").SendContext(ctx, "to@example.com", "主题", "正文"); !errors.Is(err, context.Canceled) {
			t.Fatalf("预取消仍进入建连: %v", err)
		}
	})
}

func TestIssue052SMTPImplicitTLSKeepsCertificateVerification(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	address := server.Listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := dialSMTP(ctx, "tcp", address, &tls.Config{ServerName: "example.com", MinVersion: tls.VersionTLS12}); err == nil {
		_ = conn.Close()
		t.Fatal("不受信任的 TLS 证书被接受")
	}
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	conn, err := dialSMTP(ctx, "tcp", address, &tls.Config{ServerName: "example.com", RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if encrypted, ok := conn.(*tls.Conn); !ok || !encrypted.ConnectionState().HandshakeComplete {
		t.Fatal("隐式 TLS 没有保留真实加密连接类型")
	}
	_, err = fmt.Fprintf(conn, "GET / HTTP/1.0\r\nHost: example.com\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
}

func TestIssue052SMTPImplicitTLSHandshakeHasDeadline(t *testing.T) {
	server := newSMTPLifecycleServer(t, "greeting")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	conn, err := dialSMTP(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", server.port), server.tlsConfig)
	if conn != nil {
		_ = conn.Close()
	}
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("隐式 TLS 握手未按期限结束: %v", err)
	}
}

func TestIssue052SMTPInvalidAddressesFailBeforeDial(t *testing.T) {
	for _, test := range []struct{ from, to string }{
		{from: "invalid", to: "to@example.com"},
		{from: "sender@example.com", to: "invalid"},
	} {
		if err := NewStmp("unused.invalid", 25, "user", "local-test", test.from).Send(test.to, "主题", "正文"); err == nil {
			t.Fatal("无效地址被接受")
		}
	}
}
