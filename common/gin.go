package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestbody"
	"one-api/types"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"
)

type CanonicalRequestState struct {
	// OriginalBody keeps the normalized client payload before any provider-
	// specific request rewrites. Pre-mapping needs this stable baseline so a
	// provider re-selection can rebuild the body idempotently.
	OriginalBody []byte
	WireBody     []byte
	Body         []byte
	BodyMap      map[string]interface{}
	DecodeMeta   *requestbody.DecodeMeta
}

func ensureCanonicalRequestState(c *gin.Context) *CanonicalRequestState {
	if c == nil {
		return nil
	}
	if cached, exists := c.Get(config.GinCanonicalRequestStateKey); exists {
		if state, ok := cached.(*CanonicalRequestState); ok && state != nil {
			return state
		}
	}

	state := &CanonicalRequestState{}
	if cached, exists := c.Get(config.GinRequestBodyKey); exists {
		if body, ok := cached.([]byte); ok {
			state.Body = body
		}
	}
	if cached, exists := c.Get(config.GinOriginalRequestBodyKey); exists {
		if body, ok := cached.([]byte); ok {
			state.OriginalBody = body
		}
	}
	if cached, exists := c.Get(config.GinWireRequestBodyKey); exists {
		if body, ok := cached.([]byte); ok {
			state.WireBody = body
		}
	}
	if cached, exists := c.Get(config.GinRequestBodyMapKey); exists {
		if bodyMap, ok := cached.(map[string]interface{}); ok {
			state.BodyMap = bodyMap
		}
	}
	if cached, exists := c.Get(config.GinRequestBodyDecodeMetaKey); exists {
		if meta, ok := cached.(*requestbody.DecodeMeta); ok {
			state.DecodeMeta = meta
		}
	}
	c.Set(config.GinCanonicalRequestStateKey, state)
	return state
}

func persistCanonicalRequestState(c *gin.Context, state *CanonicalRequestState) {
	if c == nil || state == nil {
		return
	}
	c.Set(config.GinCanonicalRequestStateKey, state)
	c.Set(config.GinRequestBodyKey, state.Body)
	c.Set(config.GinOriginalRequestBodyKey, state.OriginalBody)
	c.Set(config.GinWireRequestBodyKey, state.WireBody)
	c.Set(config.GinRequestBodyMapKey, state.BodyMap)
	c.Set(config.GinRequestBodyDecodeMetaKey, state.DecodeMeta)
}

func replaceReusableRequestBody(c *gin.Context, requestBody []byte) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
}

func setCanonicalRequestBodyState(c *gin.Context, state *CanonicalRequestState, requestBody []byte, requestMap map[string]interface{}) {
	if state == nil {
		return
	}
	if requestBody == nil {
		requestBody = []byte{}
	}
	if state.OriginalBody == nil {
		state.OriginalBody = requestBody
	}
	state.Body = requestBody
	state.BodyMap = requestMap
	persistCanonicalRequestState(c, state)
	replaceReusableRequestBody(c, requestBody)
}

func CacheRequestBody(c *gin.Context) ([]byte, error) {
	if state := ensureCanonicalRequestState(c); state != nil && state.Body != nil {
		if state.OriginalBody == nil {
			state.OriginalBody = state.Body
		}
		persistCanonicalRequestState(c, state)
		replaceReusableRequestBody(c, state.Body)
		return state.Body, nil
	}

	if c == nil || c.Request == nil || c.Request.Body == nil {
		requestBody := []byte{}
		if state := ensureCanonicalRequestState(c); state != nil {
			setCanonicalRequestBodyState(c, state, requestBody, nil)
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

	state := ensureCanonicalRequestState(c)
	if state != nil {
		setCanonicalRequestBodyState(c, state, requestBody, nil)
	}
	return requestBody, nil
}

func SetReusableRequestBody(c *gin.Context, requestBody []byte) {
	state := ensureCanonicalRequestState(c)
	if state == nil {
		return
	}
	setCanonicalRequestBodyState(c, state, requestBody, nil)
}

// SetCanonicalRequestBody stores the request body shape that downstream
// business logic should observe. This is intentionally the decoded body after
// transport-level normalization so existing bind/reparse helpers keep a stable,
// directly parseable JSON contract. Keep this as a compatibility wrapper and
// prefer SetDecodedRequestState when transport normalization also needs wire
// bytes and decode metadata to stay in sync.
func SetCanonicalRequestBody(c *gin.Context, requestBody []byte) {
	SetReusableRequestBody(c, requestBody)
}

func SetReusableRequestBodyMap(c *gin.Context, requestBody []byte, requestMap map[string]interface{}) {
	state := ensureCanonicalRequestState(c)
	if state == nil {
		return
	}
	setCanonicalRequestBodyState(c, state, requestBody, requestMap)
}

func GetCanonicalRequestBody(c *gin.Context) ([]byte, bool) {
	state := ensureCanonicalRequestState(c)
	if state == nil || state.Body == nil {
		return nil, false
	}
	return state.Body, true
}

func GetOriginalRequestBody(c *gin.Context) ([]byte, bool) {
	state := ensureCanonicalRequestState(c)
	if state == nil || state.OriginalBody == nil {
		return nil, false
	}
	return state.OriginalBody, true
}

// SetDecodedRequestState atomically seeds the canonical request state after
// transport-level decoding so every downstream consumer observes the same
// decoded body without rereading c.Request.Body.
func SetDecodedRequestState(c *gin.Context, wireBody []byte, decodedBody []byte, meta *requestbody.DecodeMeta) {
	state := ensureCanonicalRequestState(c)
	if state == nil {
		return
	}
	if decodedBody == nil {
		decodedBody = []byte{}
	}
	state.OriginalBody = decodedBody
	state.Body = decodedBody
	state.WireBody = wireBody
	state.BodyMap = nil
	state.DecodeMeta = meta
	persistCanonicalRequestState(c, state)
	replaceReusableRequestBody(c, decodedBody)
}

func SetWireRequestBody(c *gin.Context, requestBody []byte) {
	state := ensureCanonicalRequestState(c)
	if state == nil {
		return
	}
	state.WireBody = requestBody
	persistCanonicalRequestState(c, state)
}

func GetWireRequestBody(c *gin.Context) ([]byte, bool) {
	state := ensureCanonicalRequestState(c)
	if state == nil || state.WireBody == nil {
		return nil, false
	}
	return state.WireBody, true
}

func SetRequestBodyDecodeMeta(c *gin.Context, meta *requestbody.DecodeMeta) {
	state := ensureCanonicalRequestState(c)
	if state == nil {
		return
	}
	state.DecodeMeta = meta
	persistCanonicalRequestState(c, state)
}

func GetRequestBodyDecodeMeta(c *gin.Context) (*requestbody.DecodeMeta, bool) {
	state := ensureCanonicalRequestState(c)
	if state == nil || state.DecodeMeta == nil {
		return nil, false
	}
	return state.DecodeMeta, true
}

func GetReusableBodyMap(c *gin.Context) (map[string]interface{}, error) {
	state := ensureCanonicalRequestState(c)
	if state != nil && state.BodyMap != nil {
		return state.BodyMap, nil
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

	if state != nil {
		state.BodyMap = requestMap
		persistCanonicalRequestState(c, state)
	}
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

func SetRequestBodyReparseNeeded(c *gin.Context, needed bool) {
	if c == nil {
		return
	}
	c.Set(config.GinRequestBodyReparseKey, needed)
}

func GetRequestBodyReparseNeeded(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(config.GinRequestBodyReparseKey)
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
