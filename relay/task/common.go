package task

import (
	"errors"
	"one-api/common/config"
	"one-api/model"
	"one-api/relay/task/base"
	"one-api/relay/task/kling"
	taskmidjourney "one-api/relay/task/midjourney"
	"one-api/relay/task/suno"

	"github.com/gin-gonic/gin"
)

func GetTaskAdaptor(relayType int, c *gin.Context) (base.TaskInterface, error) {
	switch relayType {
	case config.RelayModeSuno:
		return &suno.SunoTask{
			TaskBase: getTaskBase(c, model.TaskPlatformSuno),
		}, nil
	case config.RelayModeKling:
		return &kling.KlingTask{
			TaskBase: getTaskBase(c, model.TaskPlatformKling),
		}, nil
	default:
		return nil, errors.New("adaptor not found")
	}
}

func GetTaskAdaptorByPlatform(platform string) (base.TaskProgressInterface, error) {
	relayType := config.RelayModeUnknown

	switch platform {
	case model.TaskPlatformSuno:
		relayType = config.RelayModeSuno
	case model.TaskPlatformKling:
		relayType = config.RelayModeKling
	case model.TaskPlatformMidjourney:
		return &taskmidjourney.Task{}, nil
	}

	adaptor, err := GetTaskAdaptor(relayType, nil)
	if err != nil {
		return nil, err
	}
	progressor, ok := adaptor.(base.TaskProgressInterface)
	if !ok {
		return nil, errors.New("task progress adaptor not found")
	}
	return progressor, nil
}

func getTaskBase(c *gin.Context, platform string) base.TaskBase {
	return base.TaskBase{
		Platform: platform,
		C:        c,
	}
}
