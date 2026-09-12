package suno

import (
	"gorm.io/datatypes"
	"one-api/model"
	"strings"
	"testing"
)

func TestTaskPresentationOnlyRedactsFailureReason(t *testing.T) {
	body := `{ "user_id":"provider-user", "text":"token=example", "future":9007199254740993 }`
	task := &model.Task{Action: "MUSIC", FailReason: "account acct-private failed", Data: datatypes.JSON(body)}
	dto := TaskModel2Dto(task)
	if string(dto.Data) != body || strings.Contains(dto.FailReason, "acct-private") {
		t.Fatalf("正文或错误展示边界错误: %+v", dto)
	}
	if task.FailReason != "account acct-private failed" || string(task.Data) != body {
		t.Fatal("持久化事实被展示层修改")
	}
}
