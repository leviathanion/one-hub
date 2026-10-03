package scheduler

import (
	"context"
	"fmt"
	"one-api/common/logger"
	"sync"

	"github.com/go-co-op/gocron/v2"
)

type TaskManager struct {
	scheduler gocron.Scheduler
	jobs      map[string]*JobInfo
	mu        sync.RWMutex
	stopOnce  sync.Once
	stopDone  chan struct{}
	stopErr   error
	closed    bool
}

type JobInfo struct {
	Job        gocron.Job
	Name       string
	Definition gocron.JobDefinition
	Task       gocron.Task
	Options    []gocron.JobOption
}

var (
	Manager *TaskManager
)

func init() {
	scheduler, err := gocron.NewScheduler()
	if err != nil {
		logger.SysError("初始化调度器失败: " + err.Error())
		return
	}

	Manager = &TaskManager{
		scheduler: scheduler,
		jobs:      make(map[string]*JobInfo),
	}

	Manager.scheduler.Start()
}

func (tm *TaskManager) AddJob(name string, definition gocron.JobDefinition, task gocron.Task, options ...gocron.JobOption) error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.closed {
		return fmt.Errorf("scheduler is shutting down")
	}

	// 如果任务已存在，先移除
	if oldJob, exists := tm.jobs[name]; exists {
		tm.scheduler.RemoveJob(oldJob.Job.ID())
	}

	job, err := tm.scheduler.NewJob(
		definition,
		task,
		options...,
	)

	if err != nil {
		return fmt.Errorf("添加任务失败: %v", err)
	}

	tm.jobs[name] = &JobInfo{
		Job:        job,
		Name:       name,
		Definition: definition,
		Task:       task,
		Options:    options,
	}

	return nil
}

// 获取所有任务信息
func (tm *TaskManager) GetJob(name string) *JobInfo {
	tm.mu.RLock()
	defer tm.mu.RUnlock()

	return tm.jobs[name]
}

// Shutdown uses the existing scheduler's stop/join semantics and the process
// deadline. An unfinished job is reported; SQL is not retired on that path.
func (tm *TaskManager) Shutdown(ctx context.Context) error {
	if tm == nil {
		return nil
	}
	tm.stopOnce.Do(func() {
		tm.mu.Lock()
		tm.closed = true
		tm.stopDone = make(chan struct{})
		tm.mu.Unlock()
		go func() { tm.stopErr = tm.scheduler.Shutdown(); close(tm.stopDone) }()
	})
	select {
	case <-tm.stopDone:
		return tm.stopErr
	default:
	}
	select {
	case <-tm.stopDone:
		return tm.stopErr
	case <-ctx.Done():
		return ctx.Err()
	}
}
