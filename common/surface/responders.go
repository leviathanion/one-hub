package surface

import (
	"one-api/common/logger"

	"github.com/gin-gonic/gin"
)

func OpenAIRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	openAIContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func ClaudeRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	claudeContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func GeminiRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	geminiContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func RerankRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	rerankContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func MidjourneyRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	midjourneyContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func TaskRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	taskContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func RecraftRequestBodyDecodeFailure(c *gin.Context, statusCode int, message string) {
	recraftContract.RenderJSONError(c, NewTransportError(statusCode, message))
}

func LogLocalError(c *gin.Context, err *SurfaceError) {
	if c == nil || err == nil || !err.Local || logger.Logger == nil || c.Request == nil {
		return
	}
	logger.LogError(c.Request.Context(), err.Message)
}
