package main

import (
	"context"
	"embed"
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
	"one-api/controller"
	"one-api/cron"
	"one-api/internal/lifecycle"
	"one-api/metrics"
	"one-api/middleware"
	"one-api/model"
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
	background := &lifecycle.Group{}
	runBackground := func(work func()) {
		finish, ok := background.Start()
		if ok {
			go func() { defer finish(); work() }()
		}
	}
	stopBackground := func(ctx context.Context) error {
		stopPublicationWatchers()
		background.Close()
		if err := cron.Stop(ctx); err != nil {
			return err
		}
		return background.Wait(ctx)
	}
	defer stopPublicationWatchers()
	model.HandleOldTokenMaxId()

	initMemoryCache(publicationCtx, runBackground)
	initSync(publicationCtx, runBackground)

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
		runBackground(func() { _ = model.InsertStatisticsMonth() })
	}
	initHttpServer(stopBackground)
}

func initMemoryCache(ctx context.Context, run func(func())) {
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
	run(func() { SyncChannelCache(ctx, syncFrequency) })
}

func initSync(ctx context.Context, run func(func())) {
	// go controller.AutomaticallyUpdateChannels(viper.GetInt("channel.update_frequency"))
	run(func() { controller.AutomaticallyTestChannelsContext(ctx, viper.GetInt("channel.test_frequency")) })
	run(func() { model.WatchPricePublication(ctx) })
	run(func() { model.WatchOptionsPublication(ctx) })
	run(func() { model.WatchUserGroupPublication(ctx) })
}

func initHttpServer(stopBackground func(context.Context) error) {
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

	requests := &lifecycle.Group{}
	httpServer := &http.Server{
		Addr:    ":" + port,
		Handler: requests.Handler(server),
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
	if err := gracefulShutdown(shutdownCtx, httpServer, requests, stopBackground); err != nil {
		// FatalLog exits immediately. Do not close shared SQL beneath producers that
		// failed to drain; an unsuccessful bounded stop cannot promise final durability.
		logger.FatalLog("failed to shutdown server: " + err.Error())
	}
}

func SyncChannelCache(ctx context.Context, frequency int) {
	// 只有 从 服务器端获取数据的时候才会用到
	if config.IsMasterNode {
		logger.SysLog("master node does't synchronize the channel")
		return
	}
	if frequency <= 0 {
		return
	}
	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		logger.SysLog("syncing channels from database")
		if err := model.ChannelGroup.Load(); err != nil {
			logger.SysError("failed to sync channels from database: " + err.Error())
		}
		model.ModelOwnedBysInstance.Load()
	}
}
