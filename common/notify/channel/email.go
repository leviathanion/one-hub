package channel

import (
	"context"
	"errors"
	"one-api/common/config"
	"one-api/common/stmp"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/html"
	"github.com/gomarkdown/markdown/parser"
)

type Email struct {
	To string
}

func NewEmail(to string) *Email {
	return &Email{
		To: to,
	}
}

func (e *Email) Name() string {
	return "Email"
}

func (e *Email) Send(ctx context.Context, title, message string) error {
	options := config.GlobalOption.RuntimeSnapshot()
	to := e.To
	if to == "" {
		to = config.RootUserEmail
	}

	host := options.String("SMTPServer", config.SMTPServer)
	account := options.String("SMTPAccount", config.SMTPAccount)
	token := options.String("SMTPToken", config.SMTPToken)
	if host == "" || account == "" || token == "" || to == "" {
		return errors.New("smtp config is not set, skip send email notifier")
	}

	p := parser.NewWithExtensions(parser.CommonExtensions | parser.DefinitionLists | parser.OrderedListStart)
	doc := p.Parse([]byte(message))

	htmlFlags := html.CommonFlags | html.HrefTargetBlank
	opts := html.RendererOptions{Flags: htmlFlags}
	renderer := html.NewRenderer(opts)

	body := markdown.Render(doc, renderer)

	emailClient := stmp.NewStmp(host, options.Int("SMTPPort", config.SMTPPort), account, token, options.String("SMTPFrom", config.SMTPFrom))

	return emailClient.SendContext(ctx, to, title, string(body))
}
