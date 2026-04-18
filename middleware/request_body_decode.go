package middleware

import (
	"fmt"
	"io"
	"net/http"
	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/requestbody"
	"one-api/common/surface"
	"one-api/metrics"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

type RequestBodyDecodeFailureResponder func(c *gin.Context, statusCode int, message string)

func NormalizeEncodedRequestBody() gin.HandlerFunc {
	return NormalizeEncodedRequestBodyWithFailureResponder(surface.OpenAIRequestBodyDecodeFailure)
}

func NormalizeEncodedRequestBodyWithContract(contract surface.Contract) gin.HandlerFunc {
	if contract == nil {
		return NormalizeEncodedRequestBody()
	}
	return NormalizeEncodedRequestBodyWithFailureResponder(func(c *gin.Context, statusCode int, message string) {
		contract.RenderJSONError(c, surface.NewTransportError(statusCode, message))
	})
}

func NormalizeEncodedRequestBodyWithFailureResponder(responder RequestBodyDecodeFailureResponder) gin.HandlerFunc {
	if responder == nil {
		responder = surface.OpenAIRequestBodyDecodeFailure
	}
	return func(c *gin.Context) {
		if !config.RequestBodyDecodeEnabled || c.Request == nil || c.Request.Body == nil {
			c.Next()
			return
		}

		contentEncoding := joinedContentEncodingValues(c.Request.Header)
		if contentEncoding == "" {
			c.Next()
			return
		}

		originalBody := c.Request.Body

		// Wire bytes and decoded bytes are bounded independently because
		// compressed payloads can be slightly larger than the decoded body.
		// Coupling them creates false 413s when operators tighten only the
		// decoded-body safety budget.
		wireBody, decodedBody, meta, err := requestbody.DecodeBody(originalBody, contentEncoding, requestbody.Limits{
			MaxWireBytes:      config.RequestBodyDecodeMaxWireBytes,
			MaxDecodedBytes:   config.RequestBodyDecodeMaxDecodedBytes,
			MaxExpansionRatio: config.RequestBodyDecodeMaxExpansionRatio,
			MaxEncodingLayers: config.RequestBodyDecodeMaxLayers,
		})
		closeOriginalRequestBody(c, originalBody)
		if err != nil {
			recordRequestBodyDecodeResult(contentEncoding, "failure", 0)
			logRequestBodyDecodeFailure(c, contentEncoding, wireBody, err)

			statusCode := http.StatusBadRequest
			message := err.Error()
			if decodeErr, ok := err.(*requestbody.DecodeError); ok {
				statusCode = decodeErr.StatusCode()
				message = decodeErr.Error()
			}

			responder(c, statusCode, message)
			return
		}

		common.SetDecodedRequestState(c, wireBody, decodedBody, meta)

		c.Request.Header.Del("Content-Encoding")
		c.Request.ContentLength = int64(len(decodedBody))
		c.Request.Header.Set("Content-Length", fmt.Sprintf("%d", len(decodedBody)))

		recordRequestBodyDecodeResult(contentEncoding, "success", len(decodedBody))
		logRequestBodyDecodeSuccess(c, meta)

		c.Next()
	}
}

func recordRequestBodyDecodeResult(contentEncoding, outcome string, decodedBytes int) {
	normalized := requestbody.NormalizeContentEncodingLabel(contentEncoding, config.RequestBodyDecodeMaxLayers)
	metrics.RecordRequestBodyDecode(normalized, outcome, decodedBytes)
}

func joinedContentEncodingValues(header http.Header) string {
	if header == nil {
		return ""
	}
	return strings.TrimSpace(strings.Join(header.Values("Content-Encoding"), ","))
}

func logRequestBodyDecodeSuccess(c *gin.Context, meta *requestbody.DecodeMeta) {
	if meta == nil || logger.Logger == nil {
		return
	}

	logger.Logger.Info("request body decoded",
		zap.String("request_id", c.GetString(logger.RequestIdKey)),
		zap.String("path", c.Request.URL.Path),
		zap.Strings("content_encodings", meta.ContentEncodings),
		zap.Int("wire_bytes", meta.WireBytes),
		zap.Int("decoded_bytes", meta.DecodedBytes),
		zap.Float64("expansion_ratio", meta.ExpansionRatio),
	)
}

func logRequestBodyDecodeFailure(c *gin.Context, contentEncoding string, wireBody []byte, err error) {
	if logger.Logger == nil {
		return
	}

	logger.Logger.Warn("request body decode failed",
		zap.String("request_id", c.GetString(logger.RequestIdKey)),
		zap.String("path", c.Request.URL.Path),
		zap.String("content_encoding", strings.ToLower(strings.TrimSpace(contentEncoding))),
		zap.Int("wire_bytes", len(wireBody)),
		zap.String("decode_outcome", "failure"),
		zap.Error(err),
	)
}

func closeOriginalRequestBody(c *gin.Context, body io.Closer) {
	if body == nil {
		return
	}
	if err := body.Close(); err != nil {
		logRequestBodyCloseFailure(c, err)
	}
}

func logRequestBodyCloseFailure(c *gin.Context, err error) {
	if logger.Logger == nil {
		return
	}

	// Trade-off: by the time decode finishes we already hold the canonical body
	// bytes in memory, so a Close failure is logged as cleanup debt instead of
	// flipping an otherwise valid request into a user-visible 5xx.
	logger.Logger.Warn("request body close failed after decode",
		zap.String("request_id", c.GetString(logger.RequestIdKey)),
		zap.String("path", c.Request.URL.Path),
		zap.Error(err),
	)
}
