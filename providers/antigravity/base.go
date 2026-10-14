package antigravity

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/model"
	"one-api/providers/base"
	"one-api/types"
)

type AntigravityProviderFactory struct{}

func (AntigravityProviderFactory) Create(channel *model.Channel) base.ProviderInterface {
	proxy := ""
	if channel.Proxy != nil {
		proxy = *channel.Proxy
	}
	p := &AntigravityProvider{BaseProvider: base.BaseProvider{
		Channel: channel, Config: base.ProviderConfig{BaseURL: DefaultBaseURL},
		Usage: &types.Usage{},
	}}
	p.Requester = requester.NewHTTPRequester(proxy, p.handleHTTPError)
	return p
}

type AntigravityProvider struct {
	base.BaseProvider
	credentials *Credentials
}

func (p *AntigravityProvider) SetContext(c *gin.Context) {
	p.BaseProvider.SetContext(c)
	p.bindCredentialObservation()
}

func (p *AntigravityProvider) bindCredentialObservation() {
	p.Requester.ObserveRequest = func(req *http.Request) {
		if p.Context != nil {
			values := providerresponse.RequestCredentials(req)
			values = append(values, p.credentialValues()...)
			requestctx.SetProviderCredentials(p.Context, values)
		}
	}
}

func (p *AntigravityProvider) credentialValues() []string {
	if p.credentials == nil {
		return nil
	}
	return []string{p.credentials.AccessToken, p.credentials.RefreshToken, p.credentials.Subject, p.credentials.ProjectID, p.credentials.Email}
}

func (p *AntigravityProvider) prepareCredentials(ctx context.Context) error {
	channel, creds, err := ResolveCredentials(ctx, p.Channel)
	if err != nil {
		return err
	}
	// 连接配置与 token 使用同一个数据库快照，避免刷新期间管理员修改被混用。
	oldProxy, newProxy := "", ""
	if p.Channel.Proxy != nil {
		oldProxy = *p.Channel.Proxy
	}
	if channel.Proxy != nil {
		newProxy = *channel.Proxy
	}
	p.Channel, p.credentials = channel, creds
	if oldProxy != newProxy {
		p.SetRequester(requester.NewHTTPRequester(newProxy, p.handleHTTPError))
	}
	p.bindCredentialObservation()
	return nil
}

func (p *AntigravityProvider) GetRequestHeaders() map[string]string {
	headers := map[string]string{}
	p.CommonRequestHeaders(headers)
	headers["Content-Type"] = "application/json"
	headers["User-Agent"] = DefaultOAuthConfig().UserAgent
	if p.credentials != nil {
		headers["Authorization"] = "Bearer " + p.credentials.AccessToken
	}
	return headers
}

func (p *AntigravityProvider) upstreamURL(action string) string {
	return strings.TrimRight(p.GetBaseURL(), "/") + "/v1internal:" + action
}

func (p *AntigravityProvider) MaterializeChatRemoteMedia(request *types.ChatCompletionRequest, fetcher base.RemoteMediaFetcher) error {
	return base.MaterializeChatRemoteMedia(request, fetcher)
}
