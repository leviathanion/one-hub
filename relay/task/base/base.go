package base

import (
	"context"
	"errors"
	"net/http"
	"one-api/model"
	"one-api/providers/base"
	"one-api/relay"
	"one-api/types"
	"time"

	"github.com/gin-gonic/gin"
)

type TaskBase struct {
	Platform       string
	C              *gin.Context
	OriginalModel  string
	ModelName      string
	Task           *model.Task
	OriginTaskID   string
	ownerChannelID int
	BaseProvider   base.ProviderInterface
	Response       any
}

type TaskInterface interface {
	Init() *TaskError
	Relay() *TaskError
	HandleError(err *TaskError)
	GetModelName() string
	GetTask() *model.Task
	SetProvider() *TaskError
	GetProvider() base.ProviderInterface
	OwnerChannelIncarnationID() int
	GinResponse()
}

type TaskProgressInterface interface {
	UpdateTaskStatus(ctx context.Context, taskChannelM map[int][]string, taskM map[string]*model.Task) error
}

func (t *TaskBase) InitTask() {
	if t.Task != nil || t.C == nil {
		return
	}
	userID := t.C.GetInt("id")
	tokenId := t.C.GetInt("token_id")
	t.Task = &model.Task{
		Platform:   t.Platform,
		UserId:     userID,
		TokenID:    tokenId,
		SubmitTime: time.Now().Unix(),
		Status:     model.TaskStatusNotStart,
		Progress:   0,
	}
}

func (t *TaskBase) GetModelName() string {
	billingOriginalModel := t.C.GetBool("billing_original_model")
	if billingOriginalModel {
		return t.OriginalModel
	}
	return t.ModelName
}

func (t *TaskBase) GetTask() *model.Task {
	return t.Task
}

func (t *TaskBase) GetProvider() base.ProviderInterface {
	return t.BaseProvider
}

func (t *TaskBase) GetProviderByModel() (base.ProviderInterface, error) {
	var (
		provider  base.ProviderInterface
		modelName string
		fail      error
	)
	if t.ownerChannelID > 0 {
		provider, modelName, fail = relay.GetProviderForOwnerChannel(t.C, t.OriginalModel, t.ownerChannelID)
	} else {
		provider, modelName, fail = relay.GetProvider(t.C, t.OriginalModel)
	}
	if fail != nil {
		return nil, fail
	}
	t.ModelName = modelName

	return provider, nil
}

func (t *TaskBase) OwnerChannelIncarnationID() int {
	if t == nil {
		return 0
	}
	return t.ownerChannelID
}

func (t *TaskBase) HandleOriginTaskID() error {
	userId := t.C.GetInt("id")
	if t.OriginTaskID == "" {
		return nil
	}

	task, err := model.GetTaskByTaskId(t.Platform, userId, t.OriginTaskID)
	if err != nil {
		return err
	}

	if task == nil {
		return errors.New("origin task not found")
	}

	if task.ChannelId <= 0 {
		return errors.New("origin task owner channel is invalid")
	}
	t.ownerChannelID = task.ChannelId

	return nil
}

func (t *TaskBase) GinResponse() {
	t.C.JSON(200, t.Response)
}

type TaskError struct {
	Code                 string `json:"code"`
	Message              string `json:"message"`
	Data                 any    `json:"data"`
	StatusCode           int    `json:"-"`
	LocalError           bool   `json:"-"`
	UpstreamNotAttempted bool   `json:"-"`
	UpstreamAmbiguous    bool   `json:"-"`
	UpstreamAccepted     bool   `json:"-"`
	ProviderRejected     bool   `json:"-"`
	Error                error  `json:"-"`
}

func OpenAIErrToTaskErr(errWithCode *types.OpenAIErrorWithStatusCode) *TaskError {
	code, _ := errWithCode.Code.(string)
	providerRejected := !errWithCode.LocalError && !errWithCode.UpstreamNotAttempted && !errWithCode.UpstreamAmbiguous && !errWithCode.UpstreamAccepted && errWithCode.StatusCode >= http.StatusBadRequest && errWithCode.StatusCode < http.StatusInternalServerError && errWithCode.StatusCode != http.StatusRequestTimeout

	return &TaskError{
		Code:                 code,
		Message:              errWithCode.Message,
		StatusCode:           errWithCode.StatusCode,
		LocalError:           errWithCode.LocalError,
		UpstreamNotAttempted: errWithCode.UpstreamNotAttempted,
		UpstreamAmbiguous:    errWithCode.UpstreamAmbiguous,
		UpstreamAccepted:     errWithCode.UpstreamAccepted,
		ProviderRejected:     providerRejected,
	}
}

func RejectedTaskError(httpCode int, code, message string) *TaskError {
	return &TaskError{Code: code, Message: message, StatusCode: httpCode, ProviderRejected: true}
}

func StringTaskError(httpCode int, code, message string, local bool) *TaskError {
	return &TaskError{
		Code:       code,
		Message:    message,
		StatusCode: httpCode,
		LocalError: local,
	}
}
