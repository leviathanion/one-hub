package codex

import (
	"errors"
	"net/http"

	"one-api/common"
	"one-api/common/requestctx"
	"one-api/providers/codex/wire"
	"one-api/types"
)

func (p *CodexProvider) applyClientIdentityHeaders(headers *codexHeaderBag) error {
	policy, err := p.codexChannelPolicyFor(p.codexChannel())
	if err != nil {
		return err
	}
	var inbound http.Header
	if p != nil && p.Context != nil && p.Context.Request != nil {
		inbound = p.Context.Request.Header
	}
	identity, _, err := wire.ResolveClientIdentity(requestctx.NewHeaderSnapshot(inbound), policy)
	if err != nil {
		return err
	}
	headers.Set("User-Agent", identity.UserAgent)
	headers.Set("originator", identity.Originator)
	return nil
}

func codexClientIdentityError(err error) *types.OpenAIErrorWithStatusCode {
	var violation *wire.Violation
	if errors.As(err, &violation) {
		return codexWireError(violation)
	}
	return common.ErrorWrapperLocal(err, "channel_config_error", http.StatusServiceUnavailable)
}

// header 构造同时可能产生客户端、配置和凭据错误；仅凭据错误沿用 token 错误协议。
// 保留原始错误链，供用量操作和 OAuth fence 判断错误来源。
type codexHeaderTokenError struct{ cause error }

func (e *codexHeaderTokenError) Error() string { return "failed to get token: " + e.cause.Error() }
func (e *codexHeaderTokenError) Unwrap() error { return e.cause }

func (p *CodexProvider) requestHeaderError(err error) *types.OpenAIErrorWithStatusCode {
	var tokenErr *codexHeaderTokenError
	if errors.As(err, &tokenErr) {
		return p.handleTokenError(tokenErr)
	}
	return codexClientIdentityError(err)
}

func clientIdentityHeaderMap(headers *codexHeaderBag) map[string]string {
	result := headers.Map()
	if !headers.Has("User-Agent") {
		// 空值只用于抑制 Go transport 的默认 UA，不会发送空的 UA header。
		result["User-Agent"] = ""
	}
	return result
}
