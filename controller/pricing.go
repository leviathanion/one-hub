package controller

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"one-api/common"
	"one-api/model"

	"github.com/spf13/viper"
	"gorm.io/datatypes"

	"github.com/gin-gonic/gin"
)

type manualPriceFields struct {
	Model       string   `json:"model"`
	Type        string   `json:"type"`
	ChannelType int      `json:"channel_type"`
	Input       *float64 `json:"input"`
	Output      *float64 `json:"output"`
	Locked      bool     `json:"locked"`

	ExtraRatios *datatypes.JSONType[map[string]float64] `json:"extra_ratios,omitempty"`
	RateRules   json.RawMessage                         `json:"rate_rules"`
}

type updatePriceRequest struct {
	ExpectedVersion int64 `json:"expected_version"`
	manualPriceFields
}

func (r updatePriceRequest) price() (*model.Price, bool, error) {
	if r.ExpectedVersion < 1 {
		return nil, false, errors.New("expected_version must be positive")
	}
	return r.manualPriceFields.price()
}

func (r manualPriceFields) price() (*model.Price, bool, error) {
	if r.Input == nil || r.Output == nil {
		return nil, false, errors.New("input and output are required; explicit zero is allowed")
	}
	price := &model.Price{
		Model:       r.Model,
		Type:        r.Type,
		ChannelType: r.ChannelType,
		Input:       *r.Input,
		Output:      *r.Output,
		Locked:      r.Locked,
		ExtraRatios: r.ExtraRatios,
	}
	if len(r.RateRules) == 0 {
		return price, false, nil
	}
	if bytes.Equal(bytes.TrimSpace(r.RateRules), []byte("null")) {
		return nil, true, errors.New("rate_rules must be an object; use {} to clear rules")
	}
	var rules model.PriceRateRules
	if err := json.Unmarshal(r.RateRules, &rules); err != nil {
		return nil, true, fmt.Errorf("rate_rules: %w", err)
	}
	encoded := datatypes.NewJSONType(rules)
	price.RateRules = &encoded
	return price, true, nil
}

func decodeStrictPriceRequest(c *gin.Context, dst any) error {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, model.MaxRemotePriceCatalogBytes)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("request must contain one JSON value")
		}
		return err
	}
	return nil
}

func respondPriceMutationError(c *gin.Context, err error) {
	status := http.StatusOK
	if errors.Is(err, model.ErrPublicationVersionConflict) || errors.Is(err, model.ErrPriceChangePlanMismatch) {
		status = http.StatusConflict
	}
	common.APIRespondWithError(c, status, err)
}

func GetPricesList(c *gin.Context) {
	pricesType := c.DefaultQuery("type", "db")
	if pricesType == "db" {
		prices, version := model.PricingInstance.GetAllPricesListWithVersion()
		c.JSON(http.StatusOK, gin.H{
			"success": true,
			"message": "",
			"data":    prices,
			"version": version,
		})
		return
	}
	prices := model.GetPricesList(pricesType)
	if prices == nil {
		common.APIRespondWithError(c, http.StatusOK, errors.New("pricing data not found"))
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    prices,
	})
}

func GetAllModelList(c *gin.Context) {
	prices := model.PricingInstance.GetAllPrices()
	channelModel := model.ChannelGroup.Rule

	modelsMap := make(map[string]bool)
	for modelName := range prices {
		modelsMap[modelName] = true
	}

	for _, modelMap := range channelModel {
		for modelName := range modelMap {
			if _, ok := prices[modelName]; !ok {
				modelsMap[modelName] = true
			}
		}
	}

	var models []string
	for modelName := range modelsMap {
		models = append(models, modelName)
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    models,
	})
}

func AddPrice(c *gin.Context) {
	var request updatePriceRequest
	if err := decodeStrictPriceRequest(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	price, _, err := request.price()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	if err := model.PricingInstance.AddPriceAtVersion(price, request.ExpectedVersion); err != nil {
		respondPriceMutationError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func UpdatePrice(c *gin.Context) {
	modelName := c.Param("model")
	if modelName == "" || len(modelName) < 2 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("model name is required"))
		return
	}
	modelName = modelName[1:]
	modelName, _ = url.PathUnescape(modelName)

	var request updatePriceRequest
	if err := decodeStrictPriceRequest(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	price, rateRulesPresent, err := request.price()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if err := model.PricingInstance.UpdatePriceAtVersion(modelName, price, rateRulesPresent, request.ExpectedVersion); err != nil {
		respondPriceMutationError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

func DeletePrice(c *gin.Context) {
	modelName := c.Param("model")
	if modelName == "" || len(modelName) < 2 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("model name is required"))
		return
	}
	modelName = modelName[1:]
	modelName, _ = url.PathUnescape(modelName)

	var request struct {
		ExpectedVersion int64 `json:"expected_version"`
	}
	if err := decodeStrictPriceRequest(c, &request); err != nil || request.ExpectedVersion < 1 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("expected_version must be positive"))
		return
	}
	if err := model.PricingInstance.DeletePriceAtVersion(modelName, request.ExpectedVersion); err != nil {
		respondPriceMutationError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

type PriceBatchRequest struct {
	OriginalModels  []string          `json:"original_models"`
	Models          []string          `json:"models"`
	ExpectedVersion int64             `json:"expected_version"`
	Price           manualPriceFields `json:"price"`
}

func BatchSetPrices(c *gin.Context) {
	pricesBatch := &PriceBatchRequest{}
	if err := decodeStrictPriceRequest(c, pricesBatch); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if pricesBatch.ExpectedVersion < 1 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("expected_version must be positive"))
		return
	}
	price, _, err := pricesBatch.Price.price()
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	batch := &model.BatchPrices{Models: pricesBatch.Models, Price: *price}
	if err := model.PricingInstance.BatchSetPricesAtVersion(batch, pricesBatch.OriginalModels, pricesBatch.ExpectedVersion); err != nil {
		respondPriceMutationError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

type PriceBatchDeleteRequest struct {
	Models          []string `json:"models" binding:"required"`
	ExpectedVersion int64    `json:"expected_version"`
}

func BatchDeletePrices(c *gin.Context) {
	pricesBatch := &PriceBatchDeleteRequest{}
	if err := decodeStrictPriceRequest(c, pricesBatch); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}

	if pricesBatch.ExpectedVersion < 1 {
		common.APIRespondWithError(c, http.StatusOK, errors.New("expected_version must be positive"))
		return
	}
	if err := model.PricingInstance.BatchDeletePricesAtVersion(pricesBatch.Models, pricesBatch.ExpectedVersion); err != nil {
		respondPriceMutationError(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
}

type priceChangePreviewRequest struct {
	Mode   model.PriceUpdateMode `json:"mode"`
	Source json.RawMessage       `json:"source"`
}

type priceChangeApplyRequest struct {
	Mode        model.PriceUpdateMode `json:"mode"`
	Source      json.RawMessage       `json:"source"`
	BaseVersion int64                 `json:"base_version"`
	Digest      string                `json:"digest"`
}

func decodePriceChangeSource(raw json.RawMessage) ([]*model.Price, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("source is required")
	}
	if int64(len(raw)) > model.MaxRemotePriceCatalogBytes {
		return nil, errors.New("source exceeds price catalog size limit")
	}
	return model.DecodeRemotePriceCatalog(raw)
}

func PreviewPriceChange(c *gin.Context) {
	var request priceChangePreviewRequest
	if err := decodeStrictPriceRequest(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	prices, err := decodePriceChangeSource(request.Source)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	preview, err := model.PreviewPriceChange(c.Request.Context(), prices, request.Mode)
	if err != nil {
		respondPriceMutationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": preview})
}

func ApplyPriceChange(c *gin.Context) {
	var request priceChangeApplyRequest
	if err := decodeStrictPriceRequest(c, &request); err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	prices, err := decodePriceChangeSource(request.Source)
	if err != nil {
		common.APIRespondWithError(c, http.StatusOK, err)
		return
	}
	version, err := model.ApplyPriceChange(c.Request.Context(), model.PricingInstance, prices, request.Mode, request.BaseVersion, request.Digest)
	if err != nil {
		respondPriceMutationError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "version": version})
}

func GetUpdatePriceService(c *gin.Context) {
	updatePriceService := viper.GetString("update_price_service")
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    updatePriceService,
		"message": "",
	})
}
