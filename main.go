package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"one-api/cli"
	"one-api/common"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/notify"
	"one-api/common/oidc"
	"one-api/common/redis"
	"one-api/common/requestctx"
	"one-api/common/requester"
	"one-api/common/storage"
	"one-api/common/telegram"
	"one-api/common/webauthn"
	"one-api/common/wsconn"
	"one-api/controller"
	"one-api/cron"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
	"one-api/payment"
	"one-api/providers/codex"
	"one-api/relay/task"
	"one-api/router"
	"one-api/safty"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
	"github.com/spf13/viper"
)

//go:embed web/build
var buildFS embed.FS

//go:embed web/build/index.html
var indexPage []byte

func main() {
	cli.InitCli()
	if err := config.InitConf(); err != nil {
		log.Fatal("配置错误: ", err)
	}
	requestctx.ConfigureExactWireOwnedRequestHeaders(config.ExactWireIngressOwnedHeaders)
	metrics.InitRequestBodyDecodeMetrics()
	if viper.GetString("log_level") == "debug" {
		config.Debug = true
	}

	logger.SetupLogger()
	logger.SysLog("One Hub " + config.Version + " started")
	middleware.WarnResponsesWSAnonymousCapacityBucketIfEnabled()

	// Initialize user token
	err := common.InitUserToken()
	if err != nil {
		logger.FatalLog("failed to initialize user token: " + err.Error())
	}

	// Initialize SQL Database
	model.SetupDB()
	defer model.CloseDB()
	// Initialize Redis
	redis.InitRedisClient()
	// 组限流器使用已确定的 Redis/内存后端，后续同版本同步会复用该实例。
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		logger.FatalLog("failed to load user group policy: " + err.Error())
	}
	codex.InitExecutionSessionManager()
	cache.InitCacheManager()
	// Initialize options
	model.InitOptionMap()
	// Initialize oidc
	oidc.InitOIDCConfig()
	// Initialize wenauthn
	webauthn.InitWebAuthn()
	model.NewPricing()
	publicationCtx, stopPublicationWatchers := context.WithCancel(context.Background())
	defer stopPublicationWatchers()
	model.HandleOldTokenMaxId()

	initMemoryCache()
	initSync(publicationCtx)

	common.InitTokenEncoders()
	requester.InitHttpClient()
	// Initialize Telegram bot
	telegram.InitTelegramBot()

	task.InitTask()
	notify.InitNotifier()
	cron.InitCron()
	storage.InitStorage()
	// 初始化安全检查器
	safty.InitSaftyTools()
	// 初始化账单数据
	if config.UserInvoiceMonth {
		logger.SysLog("Enable User Invoice Monthly Data")
		go model.InsertStatisticsMonth()
	}
	initHttpServer()
}

func initMemoryCache() {
	if viper.GetBool("memory_cache_enabled") {
		config.MemoryCacheEnabled = true
	}

	if !config.MemoryCacheEnabled {
		return
	}

	syncFrequency := viper.GetInt("sync_frequency")
	model.TokenCacheSeconds = syncFrequency

	logger.SysLog("memory cache enabled")
	logger.SysLog(fmt.Sprintf("sync frequency: %d seconds", syncFrequency))
	go SyncChannelCache(syncFrequency)
}

func initSync(ctx context.Context) {
	// go controller.AutomaticallyUpdateChannels(viper.GetInt("channel.update_frequency"))
	go controller.AutomaticallyTestChannels(viper.GetInt("channel.test_frequency"))
	go model.WatchPricePublication(ctx)
	go model.WatchOptionsPublication(ctx)
	go model.WatchUserGroupPublication(ctx)
}

func initHttpServer() {
	if viper.GetString("gin_mode") != "debug" {
		gin.SetMode(gin.ReleaseMode)
	}

	server := gin.New()
	server.Use(middleware.Recovery())
	server.Use(middleware.RequestId())
	middleware.SetUpLogger(server)

	trustedHeader := viper.GetString("trusted_header")
	if trustedHeader != "" {
		server.TrustedPlatform = trustedHeader
	}

	store := cookie.NewStore([]byte(config.SessionSecret))
	store.Options(sessions.Options{
		Path:     "/",
		MaxAge:   2592000, // 30 days
		HttpOnly: true,
		Secure:   false,
		SameSite: http.SameSiteStrictMode,
	})
	server.Use(sessions.Sessions("session", store))

	router.SetRouter(server, buildFS, indexPage)
	port := viper.GetString("port")

	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: server,
	}
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.FatalLog("failed to start HTTP server: " + err.Error())
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	signal.Stop(stop)

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := gracefulShutdown(shutdownCtx, httpServer, wsconn.ShutdownActive); err != nil {
		logger.FatalLog("failed to shutdown server: " + err.Error())
	}
}

func gracefulShutdown(ctx context.Context, httpServer *http.Server, drainWebSockets func(context.Context) error) error {
	var shutdownHTTP func(context.Context) error
	if httpServer != nil {
		shutdownHTTP = httpServer.Shutdown
	}
	return gracefulShutdownSteps(ctx, shutdownHTTP, drainWebSockets, payment.Resources.Close)
}

func gracefulShutdownSteps(ctx context.Context, shutdownHTTP func(context.Context) error, drainWebSockets func(context.Context) error, closePayments func()) error {
	var shutdownErrs []error
	httpDrained := true
	if shutdownHTTP != nil {
		if err := shutdownHTTP(ctx); err != nil {
			httpDrained = false
			logger.SysError("failed to shutdown HTTP server: " + err.Error())
			shutdownErrs = append(shutdownErrs, fmt.Errorf("http shutdown: %w", err))
		}
	}
	if drainWebSockets != nil {
		if err := drainWebSockets(ctx); err != nil {
			logger.SysError("failed to drain active websocket connections: " + err.Error())
			shutdownErrs = append(shutdownErrs, fmt.Errorf("websocket drain: %w", err))
		}
	}
	if httpDrained && closePayments != nil {
		closePayments()
	}
	return errors.Join(shutdownErrs...)
}

func SyncChannelCache(frequency int) {
	// 只有 从 服务器端获取数据的时候才会用到
	if config.IsMasterNode {
		logger.SysLog("master node does't synchronize the channel")
		return
	}
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		logger.SysLog("syncing channels from database")
		if err := model.ChannelGroup.Load(); err != nil {
			logger.SysError("failed to sync channels from database: " + err.Error())
		}
		model.ModelOwnedBysInstance.Load()
	}
}
