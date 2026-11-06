package controller

import (
	"errors"
	"net/http"
	"one-api/common"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// 批量归属请求仅承载有界的模型名与分类，不接收完整模型描述或价格。
const maxModelOwnershipRequestBytes = 1 << 20

func GetModelOwnershipCatalog(c *gin.Context) {
	rows, err := model.GetModelOwnershipCatalog(c.Request.Context())
	if err != nil {
		common.APIRespondWithError(c, http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func GetModelOwnershipSuggestions(c *gin.Context) {
	var request struct {
		Models []string `json:"models"`
	}
	if !decodeModelOwnershipRequest(c, &request) {
		return
	}
	rows, err := model.FetchModelsDevOwnershipSuggestions(c.Request.Context(), request.Models)
	if err != nil {
		var validation *model.ModelOwnershipValidationError
		status := http.StatusBadGateway
		if errors.As(err, &validation) {
			status = http.StatusBadRequest
		}
		common.APIRespondWithError(c, status, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": rows})
}

func BatchSetModelOwnership(c *gin.Context) {
	var request model.ModelOwnershipBatchRequest
	if !decodeModelOwnershipRequest(c, &request) {
		return
	}
	result, err := model.ApplyModelOwnershipBatch(c.Request.Context(), request)
	if err != nil {
		var validation *model.ModelOwnershipValidationError
		status := http.StatusInternalServerError
		if errors.As(err, &validation) || errors.Is(err, gorm.ErrRecordNotFound) {
			status = http.StatusBadRequest
		}
		common.APIRespondWithError(c, status, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "data": result})
}

func decodeModelOwnershipRequest(c *gin.Context, dst any) bool {
	err := decodeStrictJSONRequest(c, dst, maxModelOwnershipRequestBytes)
	if err == nil {
		return true
	}
	if errors.Is(err, errMultipleJSONValues) {
		err = errors.New("请求只能包含一个 JSON 对象")
	}
	status := http.StatusBadRequest
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		status = http.StatusRequestEntityTooLarge
	}
	common.APIRespondWithError(c, status, err)
	return false
}
