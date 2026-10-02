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
var taskWorkerMu sync.Mutex
var taskWorker *progressWorker

type progressWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// StopTask cancels new scans/provider polling and waits for already running
// mutations (including detached bounded settlement) before DB retirement.
func StopTask(ctx context.Context) error {
	taskWorkerMu.Lock()
	worker := taskWorker
	taskWorkerMu.Unlock()
	if worker == nil {
		return nil
	}
	worker.cancel()
	select {
	case <-worker.done:
		return nil
	default:
	}
	select {
	case <-worker.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("task progress still running: %w", ctx.Err())
	}
}

func InitTask() {
	taskWorkerMu.Lock()
	defer taskWorkerMu.Unlock()
	if taskWorker != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	worker := &progressWorker{cancel: cancel, done: make(chan struct{})}
	taskWorker = worker
	common.SafeGoroutine(func() { defer close(worker.done); defer cancel(); runTask(ctx, updateTaskBulk) })
}

// Task is a fixed-tick durable progressor. Wakeups only reduce latency; a lost
// wakeup cannot strand an owner because next_action_at remains authoritative.
func runTask(ctx context.Context, update func(context.Context)) {
	ticker := time.NewTicker(taskProgressTick)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		update(ctx)
		select {
		case <-ctx.Done():
			return
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

func UpdateTaskBulk() { updateTaskBulk(context.Background()) }

func updateTaskBulk(parent context.Context) {
	ctx := context.WithValue(parent, logger.RequestIdKey, "Task")
	if ctx.Err() != nil {
		return
	}
	if _, err := model.DeleteExpiredBackgroundResponseTasks(ctx, time.Now()); err != nil {
		logger.LogError(ctx, "background task cleanup failed: "+err.Error())
	}
	if _, err := model.DeleteExpiredOpenAIBatchTasks(ctx, time.Now()); err != nil {
		logger.LogError(ctx, "batch task cleanup failed: "+err.Error())
	}
	var afterNextActionAt, afterID int64
	for {
		if ctx.Err() != nil {
			return
		}
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
			if ctx.Err() != nil {
				return
			}
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
				if parent.Err() != nil {
					return
				}
				progressTaskGroup(parent, group)
			}
		}()
	}
dispatch:
	for _, group := range groups {
		select {
		case jobs <- group:
		case <-parent.Done():
			break dispatch
		}
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
