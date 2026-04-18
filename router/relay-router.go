package router

import (
	"one-api/common/surface"
	"one-api/middleware"
	"one-api/relay"
	"one-api/relay/midjourney"
	"one-api/relay/task"
	"one-api/relay/task/kling"
	"one-api/relay/task/suno"

	"github.com/gin-gonic/gin"
)

func SetRelayRouter(router *gin.Engine) {
	router.Use(middleware.CORS())
	// https://platform.openai.com/docs/api-reference/introduction
	setOpenAIRouter(router)
	setMJRouter(router)
	setSunoRouter(router)
	setClaudeRouter(router)
	setGeminiRouter(router)
	setRecraftRouter(router)
	setKlingRouter(router)
}

func setOpenAIRouter(router *gin.Engine) {
	modelsRouter := router.Group("/v1/models")
	modelsRouter.Use(middleware.OpenaiAuth(), middleware.Distribute())
	{
		modelsRouter.GET("", relay.ListModelsByToken)
		modelsRouter.GET("/:model", relay.RetrieveModel)
	}
	relayV1Router := router.Group("/v1")
	relayV1Router.Use(middleware.RelayPanicRecover(), middleware.OpenaiAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relayV1Router.GET("/realtime", relay.ChatRealtime)
	}

	// Trade-off: only structured relay endpoints opt into request-body decode.
	// That keeps auth/rate limiting ahead of decompression and preserves raw
	// pass-through contracts plus NoRoute behavior for everything else.
	structuredRelayV1Router := relayV1Router.Group("")
	structuredRelayV1Router.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.OpenAIRequestBodyDecodeFailure))
	{
		structuredRelayV1Router.POST("/completions", relay.Relay)
		structuredRelayV1Router.POST("/chat/completions", relay.Relay)
		structuredRelayV1Router.POST("/responses", relay.Relay)
		structuredRelayV1Router.POST("/responses/compact", relay.Relay)
		// structuredRelayV1Router.POST("/edits", controller.Relay)
		structuredRelayV1Router.POST("/images/generations", relay.Relay)
		structuredRelayV1Router.POST("/images/edits", relay.Relay)
		structuredRelayV1Router.POST("/images/variations", relay.Relay)
		structuredRelayV1Router.POST("/embeddings", relay.Relay)
		// structuredRelayV1Router.POST("/engines/:model/embeddings", controller.RelayEmbeddings)
		structuredRelayV1Router.POST("/audio/transcriptions", relay.Relay)
		structuredRelayV1Router.POST("/audio/translations", relay.Relay)
		structuredRelayV1Router.POST("/audio/speech", relay.Relay)
		structuredRelayV1Router.POST("/moderations", relay.Relay)
	}

	rerankRelayV1Router := relayV1Router.Group("")
	rerankRelayV1Router.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.RerankRequestBodyDecodeFailure))
	{
		rerankRelayV1Router.POST("/rerank", relay.RelayRerank)
	}

	rawRelayV1Router := relayV1Router.Group("")
	rawRelayV1Router.Use(middleware.SpecifiedChannel())
	{
		rawRelayV1Router.Any("/files", relay.RelayOnly)
		rawRelayV1Router.Any("/files/*any", relay.RelayOnly)
		rawRelayV1Router.Any("/fine_tuning/*any", relay.RelayOnly)
		rawRelayV1Router.Any("/assistants", relay.RelayOnly)
		rawRelayV1Router.Any("/assistants/*any", relay.RelayOnly)
		rawRelayV1Router.Any("/threads", relay.RelayOnly)
		rawRelayV1Router.Any("/threads/*any", relay.RelayOnly)
		rawRelayV1Router.Any("/batches/*any", relay.RelayOnly)
		rawRelayV1Router.Any("/vector_stores/*any", relay.RelayOnly)
		rawRelayV1Router.DELETE("/models/:model", relay.RelayOnly)
	}
}

func setMJRouter(router *gin.Engine) {
	relayMjRouter := router.Group("/mj")
	registerMjRouterGroup(relayMjRouter)

	relayMjModeRouter := router.Group("/:mode/mj")
	registerMjRouterGroup(relayMjModeRouter)
}

// Author: Calcium-Ion
// GitHub: https://github.com/Calcium-Ion/new-api
// Path: router/relay-router.go
func registerMjRouterGroup(relayMjRouter *gin.RouterGroup) {
	relayMjRouter.GET("/image/:id", midjourney.RelayMidjourneyImage)
	relayMjRouter.Use(middleware.RelayMJPanicRecover(), middleware.MjAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relayMjRouter.GET("/task/:id/fetch", midjourney.RelayMidjourney)
		relayMjRouter.GET("/task/:id/image-seed", midjourney.RelayMidjourney)
	}

	structuredRelayMjRouter := relayMjRouter.Group("")
	structuredRelayMjRouter.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.MidjourneyRequestBodyDecodeFailure))
	{
		structuredRelayMjRouter.POST("/submit/action", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/shorten", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/modal", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/imagine", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/change", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/simple-change", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/describe", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/blend", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/notify", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/task/list-by-condition", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/insight-face/swap", midjourney.RelayMidjourney)
		structuredRelayMjRouter.POST("/submit/upload-discord-images", midjourney.RelayMidjourney)
	}
}

func setSunoRouter(router *gin.Engine) {
	relaySunoRouter := router.Group("/suno")
	relaySunoRouter.Use(middleware.RelaySunoPanicRecover(), middleware.OpenaiAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relaySunoRouter.GET("/fetch/:id", suno.GetFetchByID)
	}

	structuredRelaySunoRouter := relaySunoRouter.Group("")
	structuredRelaySunoRouter.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.TaskRequestBodyDecodeFailure))
	{
		structuredRelaySunoRouter.POST("/submit/:action", task.RelayTaskSubmit)
		structuredRelaySunoRouter.POST("/fetch", suno.GetFetch)
	}
}

func setClaudeRouter(router *gin.Engine) {
	relayClaudeRouter := router.Group("/claude")
	relayV1Router := relayClaudeRouter.Group("/v1")
	relayV1Router.Use(middleware.APIEnabled("claude"), middleware.RelayCluadePanicRecover(), middleware.ClaudeAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relayV1Router.GET("/models", relay.ListClaudeModelsByToken)
	}

	structuredRelayClaudeV1Router := relayV1Router.Group("")
	structuredRelayClaudeV1Router.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.ClaudeRequestBodyDecodeFailure))
	{
		structuredRelayClaudeV1Router.POST("/messages", relay.Relay)
	}
}

func setGeminiRouter(router *gin.Engine) {
	relayGeminiRouter := router.Group("/gemini")
	relayGeminiRouter.Use(middleware.APIEnabled("gemini"), middleware.RelayGeminiPanicRecover(), middleware.GeminiAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relayGeminiRouter.GET("/:version/models", relay.ListGeminiModelsByToken)
	}

	structuredRelayGeminiRouter := relayGeminiRouter.Group("")
	structuredRelayGeminiRouter.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.GeminiRequestBodyDecodeFailure))
	{
		structuredRelayGeminiRouter.POST("/:version/models/:model", relay.Relay)
	}
}

func setRecraftRouter(router *gin.Engine) {
	relayRecraftRouter := router.Group("/recraftAI/v1")
	relayRecraftRouter.Use(middleware.RelayPanicRecover(), middleware.OpenaiAuth(), middleware.Distribute(), middleware.DynamicRedisRateLimiter())
	{
		relayRecraftRouter.POST("/images/generations", relay.Relay)
		relayRecraftRouter.POST("/images/vectorize", relay.RelayRecraftAI)
		relayRecraftRouter.POST("/images/removeBackground", relay.RelayRecraftAI)
		relayRecraftRouter.POST("/images/clarityUpscale", relay.RelayRecraftAI)
		relayRecraftRouter.POST("/images/generativeUpscale", relay.RelayRecraftAI)
		relayRecraftRouter.POST("/styles", relay.RelayRecraftAI)
	}
}

func setKlingRouter(router *gin.Engine) {
	relayKlingRouter := router.Group("/kling")
	relayKlingRouter.Use(middleware.RelayKlingPanicRecover(), middleware.OpenaiAuth(), middleware.Distribute())
	relayKlingRouter.GET("/v1/videos/text2video/:id", kling.GetFetchByID)
	relayKlingRouter.GET("/v1/videos/image2video/:id", kling.GetFetchByID)

	relayKlingRouter.Use(middleware.DynamicRedisRateLimiter())
	{
		structuredRelayKlingRouter := relayKlingRouter.Group("")
		structuredRelayKlingRouter.Use(middleware.NormalizeEncodedRequestBodyWithFailureResponder(surface.TaskRequestBodyDecodeFailure))
		structuredRelayKlingRouter.POST("/v1/:class/:action", task.RelayTaskSubmit)
	}
}
