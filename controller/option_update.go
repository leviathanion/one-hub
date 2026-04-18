package controller

import (
	"encoding/json"
	"errors"
	"net/http"
	"one-api/common/config"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type optionBatchUpdateRequest struct {
	Updates []optionUpdateRequest `json:"updates"`
}

func UpdateOptionBatch(c *gin.Context) {
	var request optionBatchUpdateRequest
	if err := decodeOptionRequest(c, &request); err != nil || request.Updates == nil {
		writeInvalidOptionRequest(c)
		return
	}

	updates := make([]config.OptionUpdate, 0, len(request.Updates))
	for _, update := range request.Updates {
		value, err := normalizeOptionValue(update.Value)
		if err != nil {
			writeInvalidOptionRequest(c)
			return
		}
		updates = append(updates, config.OptionUpdate{
			Key:   update.Key,
			Value: value,
		})
	}

	prepared, err := config.PrepareOptionUpdates(updates, config.OptionGroupValidationStrict)
	if err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	if err := persistOptionUpdates(prepared); err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"updated_keys": prepared.UpdatedKeys,
		},
	})
}

func decodeOptionRequest(c *gin.Context, dst any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.UseNumber()
	return decoder.Decode(dst)
}

func persistOptionUpdates(prepared *config.PreparedOptionUpdates) error {
	if len(prepared.Updates) == 0 {
		return nil
	}

	options := make([]model.Option, 0, len(prepared.Updates))
	for _, update := range prepared.Updates {
		options = append(options, model.Option{
			Key:   update.Key,
			Value: update.Value,
		})
	}

	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		return model.SaveOptionsTx(tx, options)
	}); err != nil {
		return &config.OptionValidationError{Message: err.Error()}
	}

	for _, update := range prepared.Updates {
		if err := config.GlobalOption.Set(update.Key, update.Value); err != nil {
			return &config.OptionValidationError{
				Key:     update.Key,
				Message: err.Error(),
			}
		}
	}

	return nil
}

func writeInvalidOptionRequest(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{
		"success": false,
		"message": "无效的参数",
	})
}

func writeOptionUpdateFailure(c *gin.Context, err error) {
	var validationErr *config.OptionValidationError
	if !errors.As(err, &validationErr) {
		writeInvalidOptionRequest(c)
		return
	}

	payload := gin.H{
		"success": false,
		"message": validationErr.Message,
	}
	if validationErr.Key != "" {
		payload["data"] = gin.H{
			"failed_key": validationErr.Key,
		}
	}
	c.JSON(http.StatusOK, payload)
}
