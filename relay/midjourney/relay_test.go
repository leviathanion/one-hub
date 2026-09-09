package midjourney

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	provider "one-api/providers/midjourney"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestPath2RelayModeMidjourneyDistinguishesChangeProtocols(t *testing.T) {
	tests := []struct {
		path string
		want int
	}{
		{path: "/mj/submit/change", want: provider.RelayModeMidjourneyChange},
		{path: "/mj/submit/simple-change", want: provider.RelayModeMidjourneySimpleChange},
		{path: "/relax/mj/submit/simple-change", want: provider.RelayModeMidjourneySimpleChange},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := Path2RelayModeMidjourney(test.path); got != test.want {
				t.Fatalf("Path2RelayModeMidjourney(%q)=%d, want %d", test.path, got, test.want)
			}
		})
	}
}

func TestConvertSimpleChangeParamsValidatesActionBeforeIndexing(t *testing.T) {
	tests := []struct {
		content string
		action  string
		index   int
		valid   bool
	}{
		{content: "task-1 u1", action: provider.MjActionUpscale, index: 1, valid: true},
		{content: "task-1   V4", action: provider.MjActionVariation, index: 4, valid: true},
		{content: "task-1 r", action: provider.MjActionReRoll, valid: true},
		{content: "task-1 ", valid: false},
		{content: "task-1 u", valid: false},
		{content: "task-1 u5", valid: false},
		{content: "task-1 x1", valid: false},
	}
	for _, test := range tests {
		t.Run(test.content, func(t *testing.T) {
			got := ConvertSimpleChangeParams(test.content)
			if !test.valid {
				if got != nil {
					t.Fatalf("ConvertSimpleChangeParams(%q)=%+v, want nil", test.content, got)
				}
				return
			}
			if got == nil || got.TaskId != "task-1" || got.Action != test.action || got.Index != test.index {
				t.Fatalf("ConvertSimpleChangeParams(%q)=%+v", test.content, got)
			}
		})
	}
}

func TestNormalizeMidjourneySimpleChangeMakesContentOnlyRequestExecutable(t *testing.T) {
	request := &provider.MidjourneyRequest{Content: "task-1 v3"}
	if err := normalizeMidjourneySubmitRequest(provider.RelayModeMidjourneySimpleChange, request); err != nil {
		t.Fatalf("normalize content-only request: %+v", err)
	}
	if request.TaskId != "task-1" || request.Action != provider.MjActionVariation || request.Index != 3 {
		t.Fatalf("content-only request was not normalized into change fields: %+v", request)
	}
	modelName, mjErr, ok := GetMjRequestModel(provider.RelayModeMidjourneySimpleChange, request, "")
	if !ok || mjErr != nil || modelName != "mj_variation" {
		t.Fatalf("normalized simple-change did not reach pricing/selection semantics: model=%q err=%+v ok=%v", modelName, mjErr, ok)
	}
}

func TestNormalizeMidjourneySimpleChangeRejectsMalformedContent(t *testing.T) {
	for _, content := range []string{"", "task-1", "task-1 u", "task-1 x1"} {
		t.Run(content, func(t *testing.T) {
			request := &provider.MidjourneyRequest{Content: content}
			if err := normalizeMidjourneySubmitRequest(provider.RelayModeMidjourneySimpleChange, request); err == nil {
				t.Fatalf("malformed content %q was accepted: %+v", content, request)
			}
		})
	}
}

func TestMidjourneySubmitOriginRoutingBoundary(t *testing.T) {
	tests := []struct {
		mode int
		want bool
	}{
		{mode: provider.RelayModeMidjourneyChange, want: true},
		{mode: provider.RelayModeMidjourneySimpleChange, want: true},
		{mode: provider.RelayModeMidjourneyModal, want: true},
		{mode: provider.RelayModeMidjourneyImagine, want: false},
		{mode: provider.RelayModeMidjourneyDescribe, want: false},
		{mode: provider.RelayModeMidjourneyBlend, want: false},
		{mode: provider.RelayModeMidjourneyShorten, want: false},
		{mode: provider.RelayModeMidjourneyUpload, want: false},
	}
	for _, test := range tests {
		if got := midjourneySubmitUsesOriginTask(test.mode); got != test.want {
			t.Fatalf("midjourneySubmitUsesOriginTask(%d)=%v, want %v", test.mode, got, test.want)
		}
	}
}

func TestCoverPlusActionRejectsMalformedCustomIDWithoutPanic(t *testing.T) {
	for _, customID := range []string{"", "MJ", "MJ::JOB", "MJ::JOB::upsample", "MJ::JOB::variation"} {
		t.Run(customID, func(t *testing.T) {
			request := &provider.MidjourneyRequest{CustomId: customID}
			if apiErr := CoverPlusActionToNormalAction(request); apiErr == nil {
				t.Fatalf("malformed customId %q was accepted: %+v", customID, request)
			}
		})
	}
	if apiErr := CoverPlusActionToNormalAction(nil); apiErr == nil {
		t.Fatal("nil action request was accepted")
	}
}

func TestMidjourneyOriginTaskIDUsesTaskIDForEveryOwnerBoundMode(t *testing.T) {
	for _, mode := range []int{
		provider.RelayModeMidjourneySimpleChange,
		provider.RelayModeMidjourneyModal,
	} {
		request := &provider.MidjourneyRequest{TaskId: "owner-task"}
		got, apiErr := midjourneyOriginTaskID(mode, request)
		if apiErr != nil || got != "owner-task" {
			t.Fatalf("mode %d owner id=%q err=%+v", mode, got, apiErr)
		}
	}
	change := &provider.MidjourneyRequest{TaskId: "owner-task", Action: provider.MjActionUpscale, Index: 1}
	if got, apiErr := midjourneyOriginTaskID(provider.RelayModeMidjourneyChange, change); apiErr != nil || got != "owner-task" {
		t.Fatalf("change owner id=%q err=%+v", got, apiErr)
	}
	if got, apiErr := midjourneyOriginTaskID(provider.RelayModeMidjourneyShorten, &provider.MidjourneyRequest{}); apiErr != nil || got != "" {
		t.Fatalf("shorten was treated as parent-bound: id=%q err=%+v", got, apiErr)
	}
}

func TestPrepareMidjourneyOwnerUsesOnlyMatchingBoundIncarnation(t *testing.T) {
	fixture := func(t *testing.T) *gin.Context {
		t.Helper()
		db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Task{}); err != nil {
			t.Fatal(err)
		}
		originalDB := model.DB
		model.DB = db
		t.Cleanup(func() { model.DB = originalDB })
		if err := db.Create(&model.User{Id: 1, Username: "u", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 1, UserId: 1, Key: "token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.Channel{Id: 1, Type: config.ChannelTypeMidjourney, Name: "retired-owner", Key: "key", Status: config.ChannelStatusManuallyDisabled}).Error; err != nil {
			t.Fatal(err)
		}
		gin.SetMode(gin.TestMode)
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/mj/submit/change", nil)
		ctx.Set("id", 1)
		ctx.Set("token_id", 1)
		ctx.Set("channel_id", 1)
		return ctx
	}
	view := func() *model.Midjourney {
		return &model.Midjourney{UserId: 1, TokenID: 1, ChannelId: 1, Action: provider.MjActionUpscale}
	}

	t.Run("matching retired owner channel", func(t *testing.T) {
		ctx := fixture(t)
		task, apiErr := prepareMidjourneyTaskOwner(ctx, view(), false, 1)
		if apiErr != nil || task == nil || task.ProviderState != model.TaskProviderStateSubmitStarted {
			t.Fatalf("matching owner channel rejected: task=%+v err=%+v", task, apiErr)
		}
	})

	t.Run("ordinary task cannot use retired channel", func(t *testing.T) {
		ctx := fixture(t)
		if task, apiErr := prepareMidjourneyTaskOwner(ctx, view(), false, 0); apiErr == nil || task != nil {
			t.Fatalf("ordinary task used retired channel: task=%+v err=%+v", task, apiErr)
		}
	})

	t.Run("task channel must equal owner channel", func(t *testing.T) {
		ctx := fixture(t)
		if task, apiErr := prepareMidjourneyTaskOwner(ctx, view(), false, 2); apiErr == nil || task != nil {
			t.Fatalf("mismatched owner channel admitted: task=%+v err=%+v", task, apiErr)
		}
	})
}
