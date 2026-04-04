package task

import (
	"context"
	"fmt"
	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/relay/task/base"
	"sync"
	"sync/atomic"
	"time"
)

var (
	taskActive int32 = 0
	lock       sync.Mutex
	cond       = sync.NewCond(&lock)
)

const emptyTaskIDSweepDelay = time.Minute

func InitTask() {
	common.SafeGoroutine(func() {
		Task()
	})

	ActivateUpdateTaskBulk()
}

func Task() {
	for {
		lock.Lock()
		for atomic.LoadInt32(&taskActive) == 0 {
			cond.Wait() // 等待激活信号
		}
		lock.Unlock()
		UpdateTaskBulk()
	}
}

func ActivateUpdateTaskBulk() {
	if atomic.LoadInt32(&taskActive) == 1 {
		return
	}

	lock.Lock()
	atomic.StoreInt32(&taskActive, 1)
	cond.Signal() // 通知等待的任务
	lock.Unlock()
}

func DeactivateTask() {
	if atomic.LoadInt32(&taskActive) == 0 {
		return
	}

	lock.Lock()
	atomic.StoreInt32(&taskActive, 0)
	lock.Unlock()
}

func UpdateTaskBulk() {
	ctx := context.WithValue(context.Background(), logger.RequestIdKey, "Task")
	for {
		logger.LogInfo(ctx, "running")
		allTasks := model.GetAllUnFinishSyncTasks(500)
		platformTask := make(map[string][]*model.Task)

		if len(allTasks) == 0 {
			DeactivateTask()
			logger.LogInfo(ctx, "no tasks, waiting...")
			return
		}

		for _, t := range allTasks {
			platformTask[t.Platform] = append(platformTask[t.Platform], t)
		}
		for platform, tasks := range platformTask {
			if len(tasks) == 0 {
				continue
			}
			taskChannelM := make(map[int][]string)
			taskM := make(map[string]*model.Task)
			nullTaskIds := make([]*model.Task, 0)
			for _, task := range tasks {
				trackingHandle := base.TaskTrackingHandle(task)
				if trackingHandle == "" {
					if !shouldSweepEmptyTaskID(task) {
						continue
					}
					nullTaskIds = append(nullTaskIds, task)
					continue
				}
				taskM[trackingHandle] = task
				taskChannelM[task.ChannelId] = append(taskChannelM[task.ChannelId], trackingHandle)
			}

			if len(nullTaskIds) > 0 {
				failedTaskIDs := make([]int64, 0, len(nullTaskIds))
				for _, task := range nullTaskIds {
					if err := base.FailTaskWithSettlement(ctx, task, "task_id 为空，无法继续同步任务状态"); err != nil {
						logger.LogError(ctx, fmt.Sprintf("Fix null task_id task error: %v", err))
						continue
					}
					failedTaskIDs = append(failedTaskIDs, task.ID)
				}
				if len(failedTaskIDs) > 0 {
					logger.LogInfo(ctx, fmt.Sprintf("Fix null task_id task success: %v", failedTaskIDs))
				}
			}
			if len(taskChannelM) == 0 {
				continue
			}
			UpdateTaskByPlatform(ctx, platform, taskChannelM, taskM)
		}
		time.Sleep(time.Duration(15) * time.Second)
	}
}

func shouldSweepEmptyTaskID(task *model.Task) bool {
	if task == nil || base.TaskTrackingHandle(task) != "" {
		return false
	}
	if !base.TaskAcceptedWithoutTrackingHandle(task) {
		return false
	}
	if task.SubmitTime <= 0 {
		return true
	}
	return time.Now().Unix()-task.SubmitTime >= int64(emptyTaskIDSweepDelay/time.Second)
}

func UpdateTaskByPlatform(ctx context.Context,
	platform string, taskChannelM map[int][]string, taskM map[string]*model.Task) {
	taskAdaptor, err := GetTaskAdaptorByPlatform(platform)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("GetTaskAdaptorByPlatform error: %v", err))
		return
	}

	taskAdaptor.UpdateTaskStatus(ctx, taskChannelM, taskM)
}
