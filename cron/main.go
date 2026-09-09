package cron

import (
	"context"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/scheduler"
	"one-api/model"
	"one-api/payment"
	"one-api/providers/codex"

	"github.com/go-co-op/gocron/v2"
	"github.com/spf13/viper"
)

func InitCron() {
	if !config.IsMasterNode {
		logger.SysLog("Cron is disabled on slave node")
		return
	}

	// 未确认订单只观察，不重放支付创建。SQL 查询额度在多实例间共享。
	if err := scheduler.Manager.AddJob("payment_reconcile", gocron.DurationJob(30*time.Second), gocron.NewTask(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := payment.Reconcile(ctx); err != nil {
			logger.SysError("支付核查失败: " + err.Error())
		}
	})); err != nil {
		logger.SysError("支付核查调度失败: " + err.Error())
	}

	// 添加每日统计任务
	err := scheduler.Manager.AddJob(
		"update_daily_statistics",
		gocron.DailyJob(
			1,
			gocron.NewAtTimes(
				gocron.NewAtTime(0, 0, 30),
			)),
		gocron.NewTask(func() {
			model.UpdateStatistics(model.StatisticsUpdateTypeYesterday)
			logger.SysLog("更新昨日统计数据")
		}),
	)
	if err != nil {
		logger.SysError("Cron job error: " + err.Error())
	}

	if config.UserInvoiceMonth {
		// 每月一号早上四点生成上个月的账单数据
		err = scheduler.Manager.AddJob(
			"generate_statistics_month",
			gocron.DailyJob(1, gocron.NewAtTimes(gocron.NewAtTime(4, 0, 0))),
			gocron.NewTask(func() {
				err := model.InsertStatisticsMonth()
				if err != nil {
					logger.SysError("Generate statistics month data error:" + err.Error())
				}
			}),
		)
		if err != nil {
			logger.SysError("Cron job error: " + err.Error())
		}
	}

	// 每十分钟更新一次统计数据
	err = scheduler.Manager.AddJob(
		"update_statistics",
		gocron.DurationJob(10*time.Minute),
		gocron.NewTask(func() {
			model.UpdateStatistics(model.StatisticsUpdateTypeToDay)
			logger.SysLog("10分钟统计数据")
		}),
	)
	if err != nil {
		logger.SysError("Cron job error: " + err.Error())
	}

	err = scheduler.Manager.AddJob(
		"codex_maintenance",
		gocron.DurationJob(codex.AutoRefreshInterval),
		gocron.NewTask(func() {
			codex.RunScheduledMaintenance(context.Background())
		}),
	)
	if err != nil {
		logger.SysError("Cron job error: " + err.Error())
	}
	if err == nil {
		common.SafeGoroutine(func() {
			codex.RunScheduledMaintenance(context.Background())
		})
	}

	// 开启自动更新 并且设置了有效自动更新时间 同时自动更新模式不是system 则会从服务器拉取最新价格表
	autoPriceUpdatesInterval := viper.GetInt("auto_price_updates_interval")
	autoPriceUpdates := viper.GetBool("auto_price_updates")
	autoPriceUpdatesMode := viper.GetString("auto_price_updates_mode")

	if autoPriceUpdates &&
		autoPriceUpdatesInterval > 0 &&
		(autoPriceUpdatesMode == string(model.PriceUpdateModeAdd) ||
			autoPriceUpdatesMode == string(model.PriceUpdateModeOverwrite) ||
			autoPriceUpdatesMode == string(model.PriceUpdateModeUpdate)) {
		// 指定时间周期更新价格表
		err := scheduler.Manager.AddJob(
			"update_pricing_by_service",
			gocron.DurationJob(time.Duration(autoPriceUpdatesInterval)*time.Minute),
			gocron.NewTask(func() {
				err := model.UpdatePriceByPriceService()
				if err != nil {
					logger.SysError("Update Price Error: " + err.Error())
					return
				}
				logger.SysLog("Update Price Done")
			}),
		)
		if err != nil {
			logger.SysError("Cron job error: " + err.Error())
		}
	}
}
