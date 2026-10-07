package cron

import (
	"context"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/logger"
	"one-api/common/scheduler"
	"one-api/internal/lifecycle"
	"one-api/model"
	"one-api/payment"
	"one-api/providers/codex"

	"github.com/go-co-op/gocron/v2"
)

var cronContext, cancelCron = context.WithCancel(context.Background())
var startupWork lifecycle.Group

func Stop(ctx context.Context) error {
	cancelCron()
	startupWork.Close()
	if err := scheduler.Manager.Shutdown(ctx); err != nil {
		return err
	}
	return startupWork.Wait(ctx)
}

func InitCron() {
	if !config.IsMasterNode {
		logger.SysLog("Cron is disabled on slave node")
		return
	}

	// 未确认订单只观察，不重放支付创建。SQL 查询额度在多实例间共享。
	if err := scheduler.Manager.AddJob("payment_reconcile", gocron.DurationJob(30*time.Second), gocron.NewTask(func() {
		ctx, cancel := context.WithTimeout(cronContext, 2*time.Minute)
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
			codex.RunScheduledMaintenance(cronContext)
		}),
	)
	if err != nil {
		logger.SysError("Cron job error: " + err.Error())
	}
	if err == nil {
		if finish, ok := startupWork.Start(); ok {
			common.SafeGoroutine(func() { defer finish(); codex.RunScheduledMaintenance(cronContext) })
		}
	}

	err = scheduler.Manager.AddJob(
		"cleanup_response_owners",
		gocron.DailyJob(1, gocron.NewAtTimes(gocron.NewAtTime(3, 30, 0))),
		gocron.NewTask(func() {
			deleted, cleanupErr := model.DeleteExpiredResponseOwners(cronContext, time.Now())
			if cleanupErr != nil {
				logger.SysError("Cleanup response owners error: " + cleanupErr.Error())
				return
			}
			if deleted > 0 {
				logger.SysLog("清理过期 Responses 归属记录")
			}
			if _, cleanupErr := model.DeleteExpiredResourceOwners(cronContext, time.Now()); cleanupErr != nil {
				logger.SysError("Cleanup resource owners error: " + cleanupErr.Error())
			}
		}),
	)
	if err != nil {
		logger.SysError("Cron job error: " + err.Error())
	}

}
