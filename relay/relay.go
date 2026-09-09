package relay

import (
	"net/http"
	"one-api/common"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	commonRequester "one-api/common/requester"
	"one-api/common/utils"
	"one-api/model"
	providersBase "one-api/providers/base"
	"time"

	"github.com/gin-gonic/gin"
)

func RelayOnly(c *gin.Context) {
	provider, _, fail := GetProvider(c, "")
	if fail != nil {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, fail.Error())
		return
	}

	urlBuilder, ok := provider.(providersBase.RawRelayURLBuilder)
	if !ok {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, "selected provider does not support raw resource relay")
		return
	}

	url, err := urlBuilder.BuildRawRelayURL(c.Request.URL.EscapedPath(), c.Request.URL.RawQuery)
	if err != nil {
		common.AbortWithMessage(c, http.StatusServiceUnavailable, "selected provider cannot build raw resource relay URL")
		return
	}

	mapHeaders := provider.GetRequestHeaders()
	requester := provider.GetRequester()
	req, err := requester.NewRequest(c.Request.Method, url, requester.WithContext(c.Request.Context()), requester.WithBody(c.Request.Body), requester.WithHeader(mapHeaders))
	if err != nil {
		common.AbortWithMessage(c, http.StatusBadRequest, err.Error())
		return
	}

	if req.Body != nil {
		defer req.Body.Close()
	}
	if c.Request.ContentLength >= 0 {
		req.ContentLength = c.Request.ContentLength
	}
	if err := requestctx.ApplyRegisteredExactWireRequestHeaders(req.Header, requestctx.NewHeaderSnapshot(c.Request.Header)); err != nil {
		common.AbortWithMessage(c, http.StatusBadRequest, "invalid request header")
		return
	}

	response, errWithCode := requester.SendRequestRawNoRedirect(req)
	if errWithCode != nil {
		newErrWithCode := FilterOpenAIErr(c, errWithCode)
		relayResponseWithOpenAIErr(c, &newErrWithCode)
		return
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusBadRequest {
		errWithCode = commonRequester.HandleErrorResp(response, requester.ErrorHandler, requester.PrefixProviderErrors, true)
		if replayProviderRawResponse(c, errWithCode, providerresponse.Policy{
			Operation:      providerresponse.OperationRawRelay,
			DataPath:       providerresponse.DataPathExactWire,
			BodyUnmodified: true,
		}) {
			return
		}
		newErrWithCode := FilterOpenAIErr(c, errWithCode)
		relayResponseWithOpenAIErr(c, &newErrWithCode)
		return
	}

	errWithCode = responseMultipart(c, response, providerresponse.Policy{
		Operation:        providerresponse.OperationRawRelay,
		DataPath:         providerresponse.DataPathExactWire,
		BodyUnmodified:   true,
		PreserveRedirect: true,
	})

	if errWithCode != nil {
		newErrWithCode := FilterOpenAIErr(c, errWithCode)
		relayResponseWithOpenAIErr(c, &newErrWithCode)
		return
	}

	requestTime := 0
	requestStartTimeValue := c.Request.Context().Value("requestStartTime")
	if requestStartTimeValue != nil {
		requestStartTime, ok := requestStartTimeValue.(time.Time)
		if ok {
			requestTime = int(time.Since(requestStartTime).Milliseconds())
		}
	}
	metadata := utils.AppendUserAgentMetadata(nil, c.Request.UserAgent())
	model.RecordConsumeLog(c.Request.Context(), c.GetInt("id"), c.GetInt("channel_id"), 0, 0, 0, 0, 0, "", c.GetString("token_name"), 0, rawRelayConsumeLogContent(c), requestTime, false, metadata, c.ClientIP())
}

func rawRelayConsumeLogContent(c *gin.Context) string {
	if c == nil || c.Request == nil || c.Request.URL == nil {
		return "中继:"
	}
	return "中继:" + c.Request.URL.EscapedPath()
}
