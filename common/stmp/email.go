package stmp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/utils"
	"strings"

	"github.com/wneessen/go-mail"
)

var SendResetError = &mail.SendError{
	Reason: mail.ErrSMTPReset,
}

type StmpConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

func NewStmp(host string, port int, username string, password string, from string) *StmpConfig {
	if from == "" {
		from = username
	}

	return &StmpConfig{
		Host:     host,
		Port:     port,
		Username: username,
		Password: password,
		From:     from,
	}
}

func (s *StmpConfig) Send(to, subject, body string) error {
	return s.SendContext(context.Background(), to, subject, body)
}

func (s *StmpConfig) SendContext(ctx context.Context, to, subject, body string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, mail.DefaultTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}

	message := mail.NewMsg()
	if err := message.From(s.From); err != nil {
		return err
	}
	if err := message.To(to); err != nil {
		return err
	}
	message.Subject(subject)
	message.SetGenHeader("References", s.getReferences())
	message.SetBodyString(mail.TypeTextHTML, body)
	message.SetUserAgent(fmt.Sprintf("One Hub %s // https://github.com/MartialBE/one-hub", config.Version))

	client, cleanup, err := s.newClient(ctx, nil)
	if err != nil {
		return err
	}
	defer cleanup()

	if err := dialAndSend(ctx, client, message); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	return nil
}

func (s *StmpConfig) newClient(ctx context.Context, tlsConfig *tls.Config) (*mail.Client, func(), error) {
	if tlsConfig == nil {
		tlsConfig = &tls.Config{ServerName: s.Host, MinVersion: mail.DefaultTLSMinVersion}
	}
	var connection net.Conn
	var stopCancel func() bool
	cleanup := func() {
		if stopCancel != nil {
			stopCancel()
		}
		if connection != nil {
			_ = connection.Close()
		}
	}
	client, err := mail.NewClient(
		s.Host,
		mail.WithPort(s.Port),
		mail.WithUsername(s.Username),
		mail.WithPassword(s.Password),
		mail.WithSMTPAuth(mail.SMTPAuthPlain),
		mail.WithTLSConfig(tlsConfig),
		mail.WithDialContextFunc(func(dialCtx context.Context, network, address string) (net.Conn, error) {
			var implicitTLS *tls.Config
			if s.Port == 465 {
				implicitTLS = tlsConfig
			}
			conn, err := dialSMTP(dialCtx, network, address, implicitTLS)
			if err != nil {
				return nil, err
			}
			connection = conn
			if encrypted, ok := conn.(*tls.Conn); ok {
				connection = encrypted.NetConn()
			}
			transport := connection
			// 绑定整个发送期限，不能绑定库在认证完成后就取消的建连子 context。
			// 库可能延长 socket deadline；主动关闭仍约束 DATA、RSET 和 QUIT。
			stopCancel = context.AfterFunc(ctx, func() { _ = transport.Close() })
			return conn, nil
		}),
	)

	if err != nil {
		return nil, cleanup, err
	}

	switch s.Port {
	case 465:
		client.SetSSL(true)
	case 587:
		client.SetTLSPolicy(mail.TLSMandatory)
		client.SetSMTPAuth(mail.SMTPAuthLogin)
	}

	return client, cleanup, nil
}

func dialSMTP(ctx context.Context, network, address string, tlsConfig *tls.Config) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
	}
	if tlsConfig != nil {
		encrypted := tls.Client(conn, tlsConfig)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return encrypted, nil
	}
	return conn, nil
}

func (s *StmpConfig) getReferences() string {
	froms := strings.Split(s.From, "@")
	return fmt.Sprintf("<%s.%s@%s>", froms[0], utils.GetUUID(), froms[1])
}

func (s *StmpConfig) Render(to, subject, content string) error {
	body := getDefaultTemplate(content)

	return s.Send(to, subject, body)
}

func GetSystemStmp() (*StmpConfig, error) {
	options := config.GlobalOption.RuntimeSnapshot()
	host := options.String("SMTPServer", config.SMTPServer)
	port := options.Int("SMTPPort", config.SMTPPort)
	account := options.String("SMTPAccount", config.SMTPAccount)
	token := options.String("SMTPToken", config.SMTPToken)
	if host == "" || port == 0 || account == "" || token == "" {
		return nil, fmt.Errorf("SMTP 信息未配置")
	}

	return NewStmp(host, port, account, token, options.String("SMTPFrom", config.SMTPFrom)), nil
}

func SendPasswordResetEmail(userName, email, link string) error {
	options := config.GlobalOption.RuntimeSnapshot()
	stmp, err := GetSystemStmp()

	if err != nil {
		return err
	}

	contentTemp := `<p style="font-size: 30px">Hi <strong>%s,</strong></p>
	<p>
		您正在进行密码重置。点击下方按钮以重置密码。
	</p>
	
	<p style="text-align: center; font-size: 13px;">
		<a target="__blank" href="%s" class="button" style="color: #ffffff;">重置密码</a>
	</p>
	
	<p style="color: #858585; padding-top: 15px;">
		如果链接无法点击，请尝试点击下面的链接或将其复制到浏览器中打开<br> %s
	</p>
	<p style="color: #858585;">重置链接 %d 分钟内有效，如果不是本人操作，请忽略。</p>`

	subject := fmt.Sprintf("%s密码重置", options.String("SystemName", config.SystemName))
	content := fmt.Sprintf(contentTemp, userName, link, link, common.VerificationValidMinutes)

	return stmp.Render(email, subject, content)
}

func SendVerificationCodeEmail(email, code string) error {
	options := config.GlobalOption.RuntimeSnapshot()
	stmp, err := GetSystemStmp()

	if err != nil {
		return err
	}

	contentTemp := `
	<p>
		您正在进行邮箱验证。您的验证码为: 
	</p>
	
	<p style="text-align: center; font-size: 30px; color: #58a6ff;">
		<strong>%s</strong>
	</p>
	
	<p style="color: #858585; padding-top: 15px;">
		验证码 %d 分钟内有效，如果不是本人操作，请忽略。
	</p>`

	subject := fmt.Sprintf("%s邮箱验证邮件", options.String("SystemName", config.SystemName))
	content := fmt.Sprintf(contentTemp, code, common.VerificationValidMinutes)

	return stmp.Render(email, subject, content)
}

func dialAndSend(ctx context.Context, c *mail.Client, messages ...*mail.Msg) error {
	if err := c.DialWithContext(ctx); err != nil {
		return fmt.Errorf("dial failed: %w", err)
	}
	defer c.Close()

	if err := c.Send(messages...); err != nil {
		if errors.Is(err, SendResetError) {
			return nil
		}
		return fmt.Errorf("send failed: %w", err)
	}
	return nil
}

func SendQuotaWarningCodeEmail(userName, email string, quota int, noMoreQuota bool) error {
	stmp, err := GetSystemStmp()

	if err != nil {
		return err
	}

	contentTemp := `<p style="font-size: 30px">Hi <strong>%s,</strong></p>
		<p>
			%s，当前剩余额度为 %d，为了不影响您的使用，请及时充值。
		</p>
		
		<p style="text-align: center; font-size: 13px;">
			<a target="__blank" href="%s" class="button" style="color: #ffffff;">点击充值</a>
		</p>
		
		<p style="color: #858585; padding-top: 15px;">
			如果链接无法点击，请尝试点击下面的链接或将其复制到浏览器中打开<br> %s
		</p>`

	subject := "您的额度即将用尽"
	if noMoreQuota {
		subject = "您的额度已用尽"
	}
	topUpLink := fmt.Sprintf("%s/topup", config.ServerAddress)

	content := fmt.Sprintf(contentTemp, userName, subject, quota, topUpLink, topUpLink)

	return stmp.Render(email, subject, content)
}
