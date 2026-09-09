package controller

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"one-api/common/config"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

type optionBatchUpdateRequest struct {
	ExpectedVersion int64                   `json:"expected_version"`
	Updates         []optionMutationRequest `json:"updates"`
}

func UpdateOptionBatch(c *gin.Context) {
	var request optionBatchUpdateRequest
	if err := decodeOptionRequest(c, &request); err != nil || len(request.Updates) == 0 || request.ExpectedVersion < 1 {
		writeInvalidOptionRequest(c)
		return
	}

	updates := make([]model.OptionMutation, 0, len(request.Updates))
	for _, update := range request.Updates {
		mutation, err := optionRequestMutation(update.Key, update.Value, update.Inherit)
		if err != nil {
			writeInvalidOptionRequest(c)
			return
		}
		updates = append(updates, mutation)
	}
	version, err := model.ApplyOptionMutations(c.Request.Context(), request.ExpectedVersion, updates)
	if err != nil {
		writeOptionUpdateFailure(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"updated_keys": optionMutationKeys(updates),
		},
		"version": version,
	})
}

func decodeOptionRequest(c *gin.Context, dst any) error {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func optionMutationKeys(updates []model.OptionMutation) []string {
	keys := make([]string, 0, len(updates))
	for _, update := range updates {
		keys = append(keys, config.GlobalOption.NormalizeKey(update.Key))
	}
	return keys
}

func writeInvalidOptionRequest(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{
		"success": false,
		"message": "无效的参数",
	})
}

func writeOptionUpdateFailure(c *gin.Context, err error) {
	var validationErr *config.OptionValidationError
	if errors.Is(err, model.ErrPublicationVersionConflict) {
		c.JSON(http.StatusConflict, gin.H{"success": false, "message": err.Error()})
		return
	}
	if !errors.As(err, &validationErr) {
		c.JSON(http.StatusInternalServerError, gin.H{"success": false, "message": err.Error()})
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
