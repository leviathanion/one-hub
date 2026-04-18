package surface

import (
	"encoding/json"
	"net/http"
	"one-api/common/logger"
	"one-api/common/utils"
	"one-api/providers/claude"
	"one-api/providers/gemini"
	providerMidjourney "one-api/providers/midjourney"
	providerRecraftAI "one-api/providers/recraftAI"
	"one-api/types"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type ErrorKind string

const (
	ErrorKindLocal     ErrorKind = "local"
	ErrorKindTransport ErrorKind = "transport"
	ErrorKindUpstream  ErrorKind = "upstream"
)

type SurfaceError struct {
	StatusCode int
	Message    string
	Code       any
	Type       string
	Param      string
	Local      bool
	Kind       ErrorKind
}

func NewLocalError(statusCode int, message string, code any) *SurfaceError {
	return &SurfaceError{
		StatusCode: statusCode,
		Message:    message,
		Code:       code,
		Type:       "one_hub_error",
		Local:      true,
		Kind:       ErrorKindLocal,
	}
}

func NewTransportError(statusCode int, message string) *SurfaceError {
	err := NewLocalError(statusCode, message, "invalid_request")
	err.Kind = ErrorKindTransport
	return err
}

func FromOpenAIError(err *types.OpenAIErrorWithStatusCode) *SurfaceError {
	if err == nil {
		return nil
	}
	kind := ErrorKindUpstream
	if err.LocalError {
		kind = ErrorKindLocal
	}
	return &SurfaceError{
		StatusCode: err.StatusCode,
		Message:    err.Message,
		Code:       err.Code,
		Type:       err.Type,
		Param:      err.Param,
		Local:      err.LocalError,
		Kind:       kind,
	}
}

func (e *SurfaceError) ToOpenAIErrorWithStatusCode() *types.OpenAIErrorWithStatusCode {
	if e == nil {
		return nil
	}
	statusCode := e.StatusCode
	if statusCode == 0 {
		statusCode = http.StatusBadRequest
	}
	return &types.OpenAIErrorWithStatusCode{
		OpenAIError: types.OpenAIError{
			Code:    e.Code,
			Message: e.Message,
			Type:    e.Type,
			Param:   e.Param,
		},
		StatusCode: statusCode,
		LocalError: e.Local,
	}
}

func (e *SurfaceError) CodeString(defaultCode string) string {
	if e == nil {
		return defaultCode
	}
	switch v := e.Code.(type) {
	case string:
		if strings.TrimSpace(v) != "" {
			return v
		}
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	}
	return defaultCode
}

func NormalizeSurfaceError(c *gin.Context, err *SurfaceError) *SurfaceError {
	if err == nil {
		err = &SurfaceError{}
	}

	normalized := *err
	openAIErr := NormalizeOpenAIError(c, err.ToOpenAIErrorWithStatusCode())
	normalized.StatusCode = openAIErr.StatusCode
	normalized.Message = openAIErr.Message
	normalized.Code = openAIErr.Code
	normalized.Type = openAIErr.Type
	normalized.Param = openAIErr.Param
	normalized.Local = openAIErr.LocalError

	if normalized.Kind == "" {
		if normalized.Local {
			normalized.Kind = ErrorKindLocal
		} else {
			normalized.Kind = ErrorKindUpstream
		}
	}

	return &normalized
}

type Contract interface {
	Name() string
	RenderJSONError(c *gin.Context, err *SurfaceError)
	RenderStreamError(c *gin.Context, err *SurfaceError)
}

type contract struct {
	name         string
	renderJSON   func(c *gin.Context, err *SurfaceError)
	renderStream func(c *gin.Context, err *SurfaceError)
}

func (c contract) Name() string {
	return c.name
}

func (c contract) RenderJSONError(ctx *gin.Context, err *SurfaceError) {
	if c.renderJSON == nil {
		return
	}
	c.renderJSON(ctx, err)
}

func (c contract) RenderStreamError(ctx *gin.Context, err *SurfaceError) {
	if c.renderStream == nil {
		return
	}
	c.renderStream(ctx, err)
}

var (
	requestIDPattern = regexp.MustCompile(`\(request id: [^\)]+\)`)
	quotaKeywords    = []string{"余额", "额度", "quota", "无可用渠道", "令牌"}

	openAIContract = contract{
		name: "openai",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			openAIErr := normalized.ToOpenAIErrorWithStatusCode()
			c.JSON(errStatusCode(normalized), types.OpenAIErrorResponse{
				Error: openAIErr.OpenAIError,
			})
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			openAIErr := normalized.ToOpenAIErrorWithStatusCode()
			writeStreamError(c, "data: ", types.OpenAIErrorResponse{
				Error: openAIErr.OpenAIError,
			})
		},
	}

	claudeContract = contract{
		name: "claude",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			openAIErr := normalized.ToOpenAIErrorWithStatusCode()
			claudeErr := claude.OpenaiErrToClaudeErr(openAIErr)
			c.JSON(errStatusCode(normalized), claudeErr.ClaudeError)
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			claudeErr := claude.OpenaiErrToClaudeErr(normalized.ToOpenAIErrorWithStatusCode())
			writeStreamError(c, "event: error\ndata: ", claudeErr.ClaudeError)
		},
	}

	geminiContract = contract{
		name: "gemini",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			openAIErr := normalized.ToOpenAIErrorWithStatusCode()
			geminiErr := gemini.OpenaiErrToGeminiErr(openAIErr)
			c.JSON(errStatusCode(normalized), geminiErr.GeminiErrorResponse)
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			geminiErr := gemini.OpenaiErrToGeminiErr(normalized.ToOpenAIErrorWithStatusCode())
			writeStreamError(c, "data: ", geminiErr.GeminiErrorResponse)
		},
	}

	rerankContract = contract{
		name: "rerank",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			detail := NormalizeRerankDetail(c, normalized.ToOpenAIErrorWithStatusCode())
			c.JSON(errStatusCode(normalized), gin.H{
				"detail": detail,
			})
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			writeStreamError(c, "data: ", gin.H{
				"detail": NormalizeRerankDetail(c, normalized.ToOpenAIErrorWithStatusCode()),
			})
		},
	}

	midjourneyContract = contract{
		name: "midjourney",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			c.JSON(errStatusCode(normalized), gin.H{
				"description": errMessage(normalized),
				"type":        "one_hub_error",
				"code":        providerMidjourney.MjRequestError,
			})
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			writeStreamError(c, "data: ", gin.H{
				"description": errMessage(normalized),
				"type":        "one_hub_error",
				"code":        providerMidjourney.MjRequestError,
			})
		},
	}

	taskContract = contract{
		name: "task",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			c.JSON(errStatusCode(normalized), types.TaskResponse[any]{
				Code:    normalized.CodeString("invalid_request"),
				Message: errMessage(normalized),
			})
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			writeStreamError(c, "data: ", types.TaskResponse[any]{
				Code:    normalized.CodeString("invalid_request"),
				Message: errMessage(normalized),
			})
		},
	}

	recraftContract = contract{
		name: "recraft",
		renderJSON: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			c.JSON(errStatusCode(normalized), providerRecraftAI.RecraftError{
				Code:    normalized.CodeString("invalid_request"),
				Message: errMessage(normalized),
			})
			c.Abort()
		},
		renderStream: func(c *gin.Context, err *SurfaceError) {
			if c == nil {
				return
			}
			normalized := NormalizeSurfaceError(c, err)
			writeStreamError(c, "data: ", providerRecraftAI.RecraftError{
				Code:    normalized.CodeString("invalid_request"),
				Message: errMessage(normalized),
			})
		},
	}
)

func OpenAIContract() Contract {
	return openAIContract
}

func ClaudeContract() Contract {
	return claudeContract
}

func GeminiContract() Contract {
	return geminiContract
}

func RerankContract() Contract {
	return rerankContract
}

func MidjourneyContract() Contract {
	return midjourneyContract
}

func TaskContract() Contract {
	return taskContract
}

func RecraftContract() Contract {
	return recraftContract
}

func NormalizeOpenAIError(c *gin.Context, err *types.OpenAIErrorWithStatusCode) (errWithStatusCode types.OpenAIErrorWithStatusCode) {
	if err != nil {
		errWithStatusCode = *err
	}
	if errWithStatusCode.StatusCode == 0 {
		errWithStatusCode.StatusCode = http.StatusBadRequest
	}
	if strings.TrimSpace(errWithStatusCode.OpenAIError.Type) == "" {
		errWithStatusCode.OpenAIError.Type = "one_hub_error"
	}
	if strings.Contains(errWithStatusCode.Message, "(request id:") {
		errWithStatusCode.Message = strings.TrimSpace(requestIDPattern.ReplaceAllString(errWithStatusCode.Message, ""))
	}
	if (!errWithStatusCode.LocalError && errWithStatusCode.OpenAIError.Type == "one_hub_error") ||
		strings.HasSuffix(errWithStatusCode.OpenAIError.Type, "_api_error") {
		errWithStatusCode.OpenAIError.Type = "system_error"
		if utils.ContainsString(errWithStatusCode.Message, quotaKeywords) {
			errWithStatusCode.OpenAIError.Message = "上游负载已饱和，请稍后再试"
			errWithStatusCode.StatusCode = http.StatusTooManyRequests
		}
	}
	if code, ok := errWithStatusCode.OpenAIError.Code.(string); ok &&
		code == "bad_response_status_code" &&
		strings.TrimSpace(errWithStatusCode.OpenAIError.Message) == "" {
		errWithStatusCode.OpenAIError.Message = "Provider API error: bad response status code " + errWithStatusCode.OpenAIError.Param
	}
	if errWithStatusCode.StatusCode == http.StatusTooManyRequests {
		errWithStatusCode.OpenAIError.Message = "当前分组上游负载已饱和，请稍后再试"
	}
	requestID := ""
	if c != nil {
		requestID = c.GetString(logger.RequestIdKey)
	}
	if requestID != "" && strings.TrimSpace(errWithStatusCode.OpenAIError.Message) != "" {
		errWithStatusCode.OpenAIError.Message = utils.MessageWithRequestId(errWithStatusCode.OpenAIError.Message, requestID)
	}
	return errWithStatusCode
}

func NormalizeRerankDetail(c *gin.Context, err *types.OpenAIErrorWithStatusCode) string {
	return errMessage(NormalizeSurfaceError(c, FromOpenAIError(err)))
}

func writeStreamError(c *gin.Context, prefix string, payload any) {
	if c == nil {
		return
	}
	bytes, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_, _ = c.Writer.Write([]byte(prefix + string(bytes) + "\n\n"))
	c.Writer.Flush()
}

func errStatusCode(err *SurfaceError) int {
	if err == nil || err.StatusCode == 0 {
		return http.StatusBadRequest
	}
	return err.StatusCode
}

func errMessage(err *SurfaceError) string {
	if err == nil {
		return ""
	}
	return err.Message
}
