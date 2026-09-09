package suno

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/model"
	taskbase "one-api/relay/task/base"

	"github.com/gin-gonic/gin"
)

func TestSunoNullRequestFailsBeforeOriginOrProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/suno/submit/music", strings.NewReader("null"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "action", Value: "music"}}
	task := &SunoTask{TaskBase: taskbase.TaskBase{Platform: model.TaskPlatformSuno, C: ctx}}
	if taskErr := task.Init(); taskErr == nil || taskErr.Code != "invalid_request" || task.Request != nil {
		t.Fatalf("null request was not rejected explicitly: request=%+v err=%+v", task.Request, taskErr)
	}
}
