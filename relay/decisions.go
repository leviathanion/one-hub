package relay

import (
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"net/http"
	"strings"

	"one-api/common"
	"one-api/common/jsonobject"
	"one-api/model"
	"one-api/providers"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

type relayDecisions struct {
	relayBase
	body json.RawMessage
}

func NewRelayDecisions(c *gin.Context) *relayDecisions {
	return &relayDecisions{relayBase: relayBase{c: c}}
}

func (r *relayDecisions) setRequest() error {
	body, err := common.CacheRequestBody(r.c)
	if err != nil {
		return err
	}
	envelope, err := jsonobject.Parse(body)
	if err != nil {
		return err
	}
	var modelName string
	if raw := envelope.Fields["model"]; raw != nil && jsonv2.Unmarshal(raw, &modelName) != nil {
		return errors.New("model must be a string")
	}
	if strings.TrimSpace(modelName) == "" {
		return errors.New("model is required")
	}
	r.body = body
	r.setOriginalModel(modelName)
	setRequestChannelCapability(r.c, func(channel *model.Channel) error {
		return providerCapabilityGateError(providers.AssessDecisions(channel))
	})
	return nil
}

func (r *relayDecisions) getPromptTokens() (int, error) {
	// 文本、图片及问题的计量由上游提供。仍使用普通请求的额度预扣。
	return 0, nil
}

func (r *relayDecisions) send() (*types.OpenAIErrorWithStatusCode, bool) {
	provider, ok := r.provider.(providersBase.DecisionsInterface)
	if !ok {
		return common.StringErrorWrapperLocal("channel does not implement Decisions", "channel_error", http.StatusServiceUnavailable), true
	}

	response, apiErr := provider.CreateDecision(r.modelName, r.body)
	if apiErr != nil {
		return apiErr, true
	}
	apiErr = responseJsonClient(r.c, response)
	return apiErr, apiErr != nil
}
