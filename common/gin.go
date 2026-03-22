package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

func CacheRequestBody(c *gin.Context) ([]byte, error) {
	if cached, exists := c.Get(config.GinRequestBodyKey); exists {
		if requestBody, ok := cached.([]byte); ok {
			if _, exists := c.Get(config.GinOriginalRequestBodyKey); !exists {
				c.Set(config.GinOriginalRequestBodyKey, requestBody)
			}
			c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
			return requestBody, nil
		}
	}

	if c == nil || c.Request == nil || c.Request.Body == nil {
		requestBody := []byte{}
		if c != nil {
			c.Set(config.GinOriginalRequestBodyKey, requestBody)
			SetReusableRequestBody(c, requestBody)
		}
		return requestBody, nil
	}

	requestBody, err := io.ReadAll(c.Request.Body)
	if err != nil {
		return nil, err
	}
	if err = c.Request.Body.Close(); err != nil {
		return nil, err
	}

	c.Set(config.GinOriginalRequestBodyKey, requestBody)
	SetReusableRequestBody(c, requestBody)
	return requestBody, nil
}

func SetReusableRequestBody(c *gin.Context, requestBody []byte) {
	c.Set(config.GinRequestBodyKey, requestBody)
	c.Set(config.GinRequestBodyMapKey, nil)
	c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
}

func SetReusableRequestBodyMap(c *gin.Context, requestBody []byte, requestMap map[string]interface{}) {
	c.Set(config.GinRequestBodyKey, requestBody)
	c.Set(config.GinRequestBodyMapKey, requestMap)
	c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
}

func GetOriginalRequestBody(c *gin.Context) ([]byte, bool) {
	if cached, exists := c.Get(config.GinOriginalRequestBodyKey); exists {
		if requestBody, ok := cached.([]byte); ok {
			return requestBody, true
		}
	}
	return nil, false
}

func GetReusableBodyMap(c *gin.Context) (map[string]interface{}, error) {
	if cached, exists := c.Get(config.GinRequestBodyMapKey); exists {
		if requestMap, ok := cached.(map[string]interface{}); ok && requestMap != nil {
			return requestMap, nil
		}
	}

	requestBody, err := CacheRequestBody(c)
	if err != nil {
		return nil, err
	}
	if len(requestBody) == 0 {
		return nil, nil
	}

	requestMap := make(map[string]interface{})
	if err = json.Unmarshal(requestBody, &requestMap); err != nil {
		return nil, err
	}

	c.Set(config.GinRequestBodyMapKey, requestMap)
	return requestMap, nil
}

func CloneReusableBodyMap(c *gin.Context) (map[string]interface{}, error) {
	requestMap, err := GetReusableBodyMap(c)
	if err != nil || requestMap == nil {
		return nil, err
	}

	clone, ok := cloneJSONValue(requestMap).(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("clone reusable body map: unexpected value type %T", requestMap)
	}
	return clone, nil
}

func UnmarshalBodyReusable(c *gin.Context, v any) error {
	requestBody, err := CacheRequestBody(c)
	if err != nil {
		return err
	}

	c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
	err = c.ShouldBind(v)
	if err != nil {
		if errs, ok := err.(validator.ValidationErrors); ok {
			// 返回第一个错误字段的名称
			return fmt.Errorf("field %s is required", errs[0].Field())
		}
		return err
	}

	// c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
	return nil
}

func cloneJSONValue(value interface{}) interface{} {
	// Only clones the standard Go shapes produced by json.Unmarshal.
	switch typed := value.(type) {
	case map[string]interface{}:
		cloned := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			cloned[key] = cloneJSONValue(item)
		}
		return cloned
	case []interface{}:
		cloned := make([]interface{}, len(typed))
		for i, item := range typed {
			cloned[i] = cloneJSONValue(item)
		}
		return cloned
	default:
		return value
	}
}

func ErrorWrapper(err error, code string, statusCode int) *types.OpenAIErrorWithStatusCode {
	errString := "error"
	if err != nil {
		errString = err.Error()
	}

	if strings.Contains(errString, "Post") || strings.Contains(errString, "dial") {
		logger.SysError(fmt.Sprintf("error: %s", errString))
		errString = "请求上游地址失败"
	}

	return StringErrorWrapper(errString, code, statusCode)
}

func ErrorWrapperLocal(err error, code string, statusCode int) *types.OpenAIErrorWithStatusCode {
	openaiErr := ErrorWrapper(err, code, statusCode)
	openaiErr.LocalError = true
	return openaiErr
}

func ErrorToOpenAIError(err error) *types.OpenAIError {
	return &types.OpenAIError{
		Code:    "system error",
		Message: err.Error(),
		Type:    "one_hub_error",
	}
}

func StringErrorWrapper(err string, code string, statusCode int) *types.OpenAIErrorWithStatusCode {
	openAIError := types.OpenAIError{
		Message: err,
		Type:    "one_hub_error",
		Code:    code,
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: openAIError,
		StatusCode:  statusCode,
	}
}

func StringErrorWrapperLocal(err string, code string, statusCode int) *types.OpenAIErrorWithStatusCode {
	openaiErr := StringErrorWrapper(err, code, statusCode)
	openaiErr.LocalError = true
	return openaiErr

}

func AbortWithMessage(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": message,
			"type":    "one_hub_error",
		},
	})
	c.Abort()
	logger.LogError(c.Request.Context(), message)
}

func AbortWithErr(c *gin.Context, statusCode int, err error) {
	c.JSON(statusCode, err)
	c.Abort()
	logger.LogError(c.Request.Context(), err.Error())
}

func APIRespondWithError(c *gin.Context, status int, err error) {
	c.JSON(status, gin.H{
		"success": false,
		"message": err.Error(),
	})
}

func StringRerankErrorWrapper(err string, code string, statusCode int) *types.RerankErrorWithStatusCode {
	rerankError := types.RerankError{
		Detail: err,
	}
	return &types.RerankErrorWithStatusCode{
		RerankError: rerankError,
		StatusCode:  statusCode,
	}
}

func StringRerankErrorWrapperLocal(err string, code string, statusCode int) *types.RerankErrorWithStatusCode {
	rerankError := StringRerankErrorWrapper(err, code, statusCode)
	rerankError.LocalError = true
	return rerankError

}

func OpenAIErrorToRerankError(err *types.OpenAIErrorWithStatusCode) *types.RerankErrorWithStatusCode {
	return &types.RerankErrorWithStatusCode{
		RerankError: types.RerankError{
			Detail: err.Message,
		},
		StatusCode: err.StatusCode,
		LocalError: err.LocalError,
	}
}
