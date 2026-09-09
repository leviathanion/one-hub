package model

import (
	"encoding/json"
	"fmt"
	"strings"

	"gorm.io/datatypes"
)

// Midjourney is a public/admin projection of the unified AsyncTask aggregate.
// It is not a second persistence owner.
type Midjourney struct {
	Id          int    `json:"id"`
	Code        int    `json:"code"`
	UserId      int    `json:"user_id"`
	Action      string `json:"action"`
	MjId        string `json:"mj_id"`
	Prompt      string `json:"prompt"`
	PromptEn    string `json:"prompt_en"`
	Description string `json:"description"`
	State       string `json:"state"`
	SubmitTime  int64  `json:"submit_time"`
	StartTime   int64  `json:"start_time"`
	FinishTime  int64  `json:"finish_time"`
	ImageUrl    string `json:"image_url"`
	Status      string `json:"status"`
	Progress    string `json:"progress"`
	FailReason  string `json:"fail_reason"`
	ChannelId   int    `json:"channel_id"`
	Quota       int    `json:"quota"`
	Buttons     string `json:"buttons"`
	Properties  string `json:"properties"`
	Mode        string `json:"mode,omitempty"`
	TokenID     int    `json:"token_id"`
}

type MJTaskQueryParams struct {
	ChannelID      int    `form:"channel_id"`
	TokenID        int    `form:"token_id"`
	MjID           string `form:"mj_id"`
	StartTimestamp int    `form:"start_timestamp"`
	EndTimestamp   int    `form:"end_timestamp"`
	PaginationParams
}

func GetAllUserMJTask(userID int, params *MJTaskQueryParams) (*DataResult[Midjourney], error) {
	return getMidjourneyTaskViews(userID, params)
}

func GetAllMJTasks(params *MJTaskQueryParams) (*DataResult[Midjourney], error) {
	return getMidjourneyTaskViews(0, params)
}

func getMidjourneyTaskViews(userID int, params *MJTaskQueryParams) (*DataResult[Midjourney], error) {
	if params == nil {
		params = &MJTaskQueryParams{}
	}
	query := DB.Where("platform = ?", TaskPlatformMidjourney)
	if userID > 0 {
		query = query.Where("user_id = ?", userID)
	}
	if params.ChannelID != 0 {
		query = query.Where("channel_id = ?", params.ChannelID)
	}
	if params.TokenID > 0 {
		query = query.Where("token_id = ?", params.TokenID)
	}
	if params.MjID != "" {
		query = query.Where("task_id = ?", params.MjID)
	}
	if params.StartTimestamp != 0 {
		query = query.Where("submit_time >= ?", params.StartTimestamp)
	}
	if params.EndTimestamp != 0 {
		query = query.Where("submit_time <= ?", params.EndTimestamp)
	}
	pagination := params.PaginationParams
	pagination.Order = strings.ReplaceAll(pagination.Order, "mj_id", "task_id")
	var tasks []*Task
	result, err := PaginateAndOrder(query, &pagination, &tasks, map[string]bool{
		"id": true, "user_id": true, "action": true, "task_id": true,
		"submit_time": true, "start_time": true, "finish_time": true,
		"status": true, "channel_id": true,
	})
	if err != nil {
		return nil, err
	}
	views := make([]*Midjourney, 0, len(tasks))
	for _, task := range tasks {
		view := MidjourneyFromTask(task)
		if userID > 0 {
			view.ChannelId = 0
		}
		views = append(views, view)
	}
	return &DataResult[Midjourney]{Data: &views, Page: result.Page, Size: result.Size, TotalCount: result.TotalCount}, nil
}

func GetByMJId(userID int, mjID string) *Midjourney {
	task, err := GetTaskByTaskId(TaskPlatformMidjourney, userID, mjID)
	if err != nil || task == nil {
		return nil
	}
	return MidjourneyFromTask(task)
}

func GetByMJIds(userID int, mjIDs []string) []*Midjourney {
	var tasks []*Task
	if err := DB.Where("platform = ? AND user_id = ? AND task_id IN ?", TaskPlatformMidjourney, userID, mjIDs).Find(&tasks).Error; err != nil {
		return nil
	}
	result := make([]*Midjourney, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, MidjourneyFromTask(task))
	}
	return result
}

func MidjourneyFromTask(task *Task) *Midjourney {
	if task == nil {
		return nil
	}
	view := &Midjourney{}
	_ = json.Unmarshal(task.Data, view)
	view.Id = int(task.ID)
	view.UserId = task.UserId
	view.TokenID = task.TokenID
	view.ChannelId = task.ChannelId
	view.MjId = TaskProviderID(task)
	view.Action = task.Action
	view.SubmitTime = task.SubmitTime
	view.StartTime = task.StartTime
	view.FinishTime = task.FinishTime
	view.Status = string(task.Status)
	view.Progress = formatTaskProgress(task.Progress)
	view.FailReason = task.FailReason
	view.Quota = task.FinalQuota()
	return view
}

func EncodeMidjourneyTaskData(view *Midjourney) datatypes.JSON {
	if view == nil {
		return nil
	}
	data, _ := json.Marshal(view)
	return datatypes.JSON(data)
}

func formatTaskProgress(progress int) string {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	return fmt.Sprintf("%d%%", progress)
}
