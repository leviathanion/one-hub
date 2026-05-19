package middleware

import (
	"one-api/common/logger"
	"strings"
	"sync"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

var corsCompatibilityWarnOnce sync.Once

var defaultCORSAllowHeaders = []string{
	"Origin",
	"Content-Length",
	"Content-Type",
	"Accept",
	"Authorization",
	"X-API-Key",
	"api-key",
	"x-goog-api-key",
	"anthropic-version",
	"anthropic-beta",
	"OpenAI-Organization",
	"OpenAI-Project",
	"X-Requested-With",
	"X-Session-Id",
	"Session-Id",
	"OpenAI-Beta",
	"Sec-WebSocket-Protocol",
}

func CORS() gin.HandlerFunc {
	corsConfig := cors.DefaultConfig()
	allowOrigins := ConfiguredCORSAllowOrigins()
	if len(allowOrigins) == 0 {
		corsConfig.AllowAllOrigins = true
		corsConfig.AllowCredentials = true
		corsConfig.AllowHeaders = []string{"*"}
		warnLegacyWildcardCredentialCORS()
	} else {
		corsConfig.AllowOrigins = allowOrigins
		corsConfig.AllowCredentials = !containsCORSWildcardOrigin(allowOrigins)
		if viper.IsSet("cors.allow_credentials") {
			corsConfig.AllowCredentials = viper.GetBool("cors.allow_credentials") && !containsCORSWildcardOrigin(allowOrigins)
		}
		corsConfig.AllowHeaders = ConfiguredCORSAllowHeaders()
	}
	corsConfig.AllowMethods = []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"}
	return cors.New(corsConfig)
}

func warnLegacyWildcardCredentialCORS() {
	corsCompatibilityWarnOnce.Do(func() {
		if logger.Logger != nil {
			logger.Logger.Warn("[SYS] | cors.allow_origins is empty; using legacy credential-capable wildcard CORS compatibility mode, configure cors.allow_origins in production")
		}
	})
}

func ConfiguredCORSAllowOrigins() []string {
	raw := viper.GetStringSlice("cors.allow_origins")
	if len(raw) == 0 {
		if single := strings.TrimSpace(viper.GetString("cors.allow_origins")); single != "" {
			raw = strings.Split(single, ",")
		}
	}
	origins := make([]string, 0, len(raw))
	for _, origin := range raw {
		trimmed := strings.TrimSpace(origin)
		if trimmed != "" {
			origins = append(origins, trimmed)
		}
	}
	return origins
}

func ConfiguredCORSAllowHeaders() []string {
	headers := append([]string(nil), defaultCORSAllowHeaders...)
	raw := viper.GetStringSlice("cors.allow_headers")
	if len(raw) == 0 {
		if single := strings.TrimSpace(viper.GetString("cors.allow_headers")); single != "" {
			raw = strings.Split(single, ",")
		}
	}
	for _, header := range raw {
		trimmed := strings.TrimSpace(header)
		if trimmed == "" || containsCORSHeader(headers, trimmed) {
			continue
		}
		headers = append(headers, trimmed)
	}
	return headers
}

func containsCORSWildcardOrigin(origins []string) bool {
	for _, origin := range origins {
		if strings.TrimSpace(origin) == "*" {
			return true
		}
	}
	return false
}

func containsCORSHeader(headers []string, header string) bool {
	for _, current := range headers {
		if strings.EqualFold(strings.TrimSpace(current), strings.TrimSpace(header)) {
			return true
		}
	}
	return false
}
