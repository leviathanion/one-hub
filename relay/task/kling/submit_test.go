package kling

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"one-api/model"
	taskbase "one-api/relay/task/base"

	"github.com/gin-gonic/gin"
)

func TestKlingNullRequestFailsBeforeOriginOrProviderWork(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", strings.NewReader("null"))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Params = gin.Params{{Key: "class", Value: "videos"}, {Key: "action", Value: "text2video"}}
	task := &KlingTask{TaskBase: taskbase.TaskBase{Platform: model.TaskPlatformKling, C: ctx}}
	if taskErr := task.Init(); taskErr == nil || taskErr.Code != "invalid_request" || task.Request != nil {
		t.Fatalf("null request was not rejected explicitly: request=%+v err=%+v", task.Request, taskErr)
	}
}
