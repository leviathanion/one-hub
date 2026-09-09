package relay

import (
	"encoding/json"
	"errors"
	"github.com/gin-gonic/gin"
	"io"
	"math"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/model"
	"one-api/relay/relay_util"
	"one-api/types"
)

func PreviewPriceRules(c *gin.Context) {
	var request struct {
		Price       model.Price          `json:"price"`
		Facts       model.PriceRuleFacts `json:"facts"`
		Usage       types.Usage          `json:"usage"`
		ExtraTokens map[string]int       `json:"extra_tokens"`
		GroupRatio  *float64             `json:"group_ratio"`
	}
	if err := decodePricePreview(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	price := &request.Price
	err := model.ValidatePrice(price)
	if err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	usage := &request.Usage
	if !usageHasPreviewBase(usage) {
		common.APIRespondWithError(c, http.StatusBadRequest, errors.New("preview requires explicit non-negative prompt_tokens and completion_tokens"))
		return
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	for key, value := range request.ExtraTokens {
		prompt, ok := model.ExtraKeyIsPrompt[key]
		limit := usage.CompletionTokens
		if prompt {
			limit = usage.PromptTokens
		}
		if !ok || key == config.UsageExtraInputAudioTranscription || value < 0 || value > limit {
			common.APIRespondWithError(c, http.StatusBadRequest, errors.New("invalid preview extra token quantity"))
			return
		}
		usage.SetExtraTokens(key, value)
		usage.ProviderTokenFields[key] = true
	}
	// 仅在模拟路径标记数量；返回结果不能进入真实 Attempt 或授权余额操作。
	usage.MarkProviderReported()
	groupRatio := 1.0
	if request.GroupRatio != nil {
		groupRatio = *request.GroupRatio
	}
	decision, metadata, err := relay_util.PreviewTokenPrice(*price, request.Facts, usage, groupRatio)
	if err != nil {
		common.APIRespondWithError(c, http.StatusBadRequest, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": gin.H{"decision": decision, "metadata": metadata}})
}

func usageHasPreviewBase(usage *types.Usage) bool {
	return usage.ProviderTokenFields["prompt_tokens"] && usage.ProviderTokenFields["completion_tokens"] && usage.PromptTokens >= 0 && usage.CompletionTokens >= 0 && usage.PromptTokens <= math.MaxInt/2 && usage.CompletionTokens <= math.MaxInt/2
}

func decodePricePreview(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, model.MaxPriceRulesBytes*2)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("preview requires one JSON object")
	}
	return nil
}
