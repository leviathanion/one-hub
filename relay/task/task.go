package task

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"one-api/common"
	"one-api/common/logger"
	"one-api/model"
	"one-api/relay/task/base"
)

const (
	asyncTaskSubmitDeadline       = 10 * time.Minute
	taskOwnerMutationDeadline     = 5 * time.Second
	stalePreparedTaskTimeout      = 15 * time.Minute
	staleSubmitStartedTaskTimeout = 15 * time.Minute
	taskProgressTick              = 15 * time.Second
	taskProgressWorkers           = 8
)

var taskWake = make(chan struct{}, 1)

func InitTask() {
	common.SafeGoroutine(Task)
	ActivateUpdateTaskBulk()
}

// Task is a fixed-tick durable progressor. Wakeups only reduce latency; a lost
// wakeup cannot strand an owner because next_action_at remains authoritative.
func Task() {
	ticker := time.NewTicker(taskProgressTick)
	defer ticker.Stop()
	for {
		UpdateTaskBulk()
		select {
		case <-ticker.C:
		case <-taskWake:
		}
	}
}

func ActivateUpdateTaskBulk() {
	select {
	case taskWake <- struct{}{}:
	default:
	}
}

type taskPollGroup struct {
	platform  string
	channelID int
	tasks     []*model.Task
}

func UpdateTaskBulk() {
	ctx := context.WithValue(context.Background(), logger.RequestIdKey, "Task")
	var afterNextActionAt, afterID int64
	for {
		due, err := model.ListDueTaskOwners(ctx, time.Now(), afterNextActionAt, afterID, model.TaskProgressPageSize)
		if err != nil {
			logger.LogError(ctx, "scan due task owners failed: "+err.Error())
			return
		}
		if len(due) == 0 {
			return
		}

		groups := make(map[string]*taskPollGroup)
		for _, owner := range due {
			if owner == nil {
				continue
			}
			scanNextActionAt, scanID := owner.NextActionAt, owner.ID
			if scanNextActionAt > afterNextActionAt || (scanNextActionAt == afterNextActionAt && scanID > afterID) {
				afterNextActionAt, afterID = scanNextActionAt, scanID
			}

			switch owner.ProviderState {
			case model.TaskProviderStatePrepared, model.TaskProviderStateSubmitStarted:
				settleStaleTaskOwner(ctx, owner, time.Now())
				continue
			case model.TaskProviderStateAccepted:
				claim, claimErr := model.ClaimTaskPoll(ctx, owner, time.Now(), model.TaskPollFenceWindow)
				if claimErr != nil || claim.Outcome != model.TaskMutationApplied {
					continue
				}
				if base.TaskTrackingHandle(owner) == "" {
					owner.Status = model.TaskStatusUnknown
					if closeErr := base.FailTaskWithSettlement(ctx, owner, "accepted task is missing provider handle"); closeErr != nil {
						logger.LogError(ctx, "close accepted task without handle: "+closeErr.Error())
					}
					continue
				}
				key := fmt.Sprintf("%s\x00%d", owner.Platform, owner.ChannelId)
				group := groups[key]
				if group == nil {
					group = &taskPollGroup{platform: owner.Platform, channelID: owner.ChannelId}
					groups[key] = group
				}
				group.tasks = append(group.tasks, owner)
			}
		}
		progressTaskGroups(ctx, groups)
		if len(due) < model.TaskProgressPageSize {
			return
		}
	}
}

func progressTaskGroups(parent context.Context, groups map[string]*taskPollGroup) {
	jobs := make(chan *taskPollGroup)
	var workers sync.WaitGroup
	workerCount := taskProgressWorkers
	if len(groups) < workerCount {
		workerCount = len(groups)
	}
	for i := 0; i < workerCount; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for group := range jobs {
				progressTaskGroup(parent, group)
			}
		}()
	}
	for _, group := range groups {
		jobs <- group
	}
	close(jobs)
	workers.Wait()
}

func progressTaskGroup(parent context.Context, group *taskPollGroup) {
	if group == nil || len(group.tasks) == 0 {
		return
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.LogError(parent, fmt.Sprintf("task poll panic platform=%s channel=%d: %v\n%s", group.platform, group.channelID, recovered, debug.Stack()))
		}
	}()
	taskIDs := make([]string, 0, len(group.tasks))
	taskMap := make(map[string]*model.Task, len(group.tasks))
	for _, owner := range group.tasks {
		handle := base.TaskTrackingHandle(owner)
		if handle == "" {
			continue
		}
		taskIDs = append(taskIDs, handle)
		taskMap[handle] = owner
	}
	if len(taskIDs) == 0 {
		return
	}
	UpdateTaskByPlatform(parent, group.platform, map[int][]string{group.channelID: taskIDs}, taskMap)
}

func settleStaleTaskOwner(ctx context.Context, task *model.Task, now time.Time) bool {
	if task == nil || task.ProviderState == model.TaskProviderStateClosed {
		return task != nil && task.ProviderState == model.TaskProviderStateClosed
	}
	switch task.ProviderState {
	case model.TaskProviderStatePrepared:
		if task.CreatedAt > 0 && now.Unix()-task.CreatedAt >= int64(stalePreparedTaskTimeout/time.Second) {
			task.Status = model.TaskStatusLocalFailure
			if err := base.FailTaskWithSettlement(ctx, task, "prepared task submission expired before claim"); err != nil {
				logger.LogError(ctx, "close stale prepared task owner failed: "+err.Error())
			}
			return true
		}
	case model.TaskProviderStateSubmitStarted:
		if task.SubmitStartedAt != nil && now.Unix()-*task.SubmitStartedAt >= int64(staleSubmitStartedTaskTimeout/time.Second) {
			task.Status = model.TaskStatusUnknown
			if err := base.FailTaskWithSettlement(ctx, task, "task submission outcome remained ambiguous"); err != nil {
				logger.LogError(ctx, "close stale submit-started task owner failed: "+err.Error())
			}
			return true
		}
	}
	return false
}

func UpdateTaskByPlatform(ctx context.Context, platform string, taskChannelM map[int][]string, taskM map[string]*model.Task) {
	taskAdaptor, err := GetTaskAdaptorByPlatform(platform)
	if err != nil {
		logger.LogError(ctx, fmt.Sprintf("GetTaskAdaptorByPlatform error: %v", err))
		return
	}
	if err := taskAdaptor.UpdateTaskStatus(ctx, taskChannelM, taskM); err != nil {
		logger.LogError(ctx, fmt.Sprintf("UpdateTaskStatus platform=%s: %v", platform, err))
	}
}
