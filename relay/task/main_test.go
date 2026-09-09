package task

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/requester"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	providerbase "one-api/providers/base"
	taskbase "one-api/relay/task/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type taskTestProvider struct{ channel *model.Channel }

func (p *taskTestProvider) GetRequestHeaders() map[string]string                    { return nil }
func (p *taskTestProvider) GetUsage() *types.Usage                                  { return nil }
func (p *taskTestProvider) SetUsage(*types.Usage)                                   {}
func (p *taskTestProvider) SetContext(*gin.Context)                                 {}
func (p *taskTestProvider) SetOriginalModel(string)                                 {}
func (p *taskTestProvider) GetOriginalModel() string                                { return "task-model" }
func (p *taskTestProvider) GetChannel() *model.Channel                              { return p.channel }
func (p *taskTestProvider) ModelMappingHandler(name string) (string, error)         { return name, nil }
func (p *taskTestProvider) GetRequester() *requester.HTTPRequester                  { return nil }
func (p *taskTestProvider) CustomParameterHandler() (map[string]interface{}, error) { return nil, nil }

type taskSubmitTestAdaptor struct {
	ctx            *gin.Context
	task           *model.Task
	provider       providerbase.ProviderInterface
	relayCalls     atomic.Int32
	relayErr       *taskbase.TaskError
	providerID     string
	ownerChannelID int
}

func (a *taskSubmitTestAdaptor) Init() *taskbase.TaskError { return nil }
func (a *taskSubmitTestAdaptor) Relay() *taskbase.TaskError {
	a.relayCalls.Add(1)
	if a.relayErr != nil {
		return a.relayErr
	}
	model.SetTaskProviderID(a.task, a.providerID)
	return nil
}
func (a *taskSubmitTestAdaptor) HandleError(err *taskbase.TaskError) {
	if err != nil {
		a.ctx.JSON(err.StatusCode, err)
	}
}
func (a *taskSubmitTestAdaptor) GetModelName() string                        { return "task-model" }
func (a *taskSubmitTestAdaptor) GetTask() *model.Task                        { return a.task }
func (a *taskSubmitTestAdaptor) SetProvider() *taskbase.TaskError            { return nil }
func (a *taskSubmitTestAdaptor) GetProvider() providerbase.ProviderInterface { return a.provider }
func (a *taskSubmitTestAdaptor) OwnerChannelIncarnationID() int              { return a.ownerChannelID }
func (a *taskSubmitTestAdaptor) GinResponse() {
	a.ctx.JSON(http.StatusOK, gin.H{"task_id": a.task.TaskID})
}
func (a *taskSubmitTestAdaptor) UpdateTaskStatus(context.Context, map[int][]string, map[string]*model.Task) error {
	return nil
}

func taskSubmitFixture(t *testing.T) (*gin.Context, *taskSubmitTestAdaptor) {
	t.Helper()
	originalDB := model.DB
	originalPricing := model.PricingInstance
	originalGet := getTaskAdaptorFunc
	model.GlobalUserGroupRatio.Lock()
	originalGroups := model.GlobalUserGroupRatio.UserGroup
	model.GlobalUserGroupRatio.Unlock()
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.User{}, &model.Token{}, &model.Channel{}, &model.Price{}, &model.ModelInfo{}, &model.UserGroup{}, &model.Task{}, &model.PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	model.PricingInstance = &model.Pricing{Prices: map[string]*model.Price{
		"task-model": {Model: "task-model", Type: model.TimesPriceType, Input: 0.1},
	}}
	t.Cleanup(func() {
		model.DB = originalDB
		model.PricingInstance = originalPricing
		getTaskAdaptorFunc = originalGet
		model.GlobalUserGroupRatio.Lock()
		model.GlobalUserGroupRatio.UserGroup = originalGroups
		model.GlobalUserGroupRatio.Unlock()
	})
	if err := db.Create(&model.User{Id: 1, Username: "u", Password: "password123", AccessToken: "access", Quota: 1000, Status: config.UserStatusEnabled, Group: "paid"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{Id: 1, UserId: 1, Key: "token", Status: config.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: 1000}).Error; err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Id: 1, Type: config.ChannelTypeKling, Name: "channel", Key: "key", Status: config.ChannelStatusEnabled, Models: "task-model"}
	if err := db.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.Price{Model: "task-model", Type: model.TimesPriceType, Input: 0.1}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.PricingInstance.Init(); err != nil {
		t.Fatal(err)
	}
	enabled := true
	if err := db.Create(&model.UserGroup{Symbol: "paid", Name: "Paid", Ratio: 1, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	model.GlobalUserGroupRatio.Lock()
	groups := make(map[string]*model.UserGroup, len(originalGroups)+1)
	for symbol, group := range originalGroups {
		groups[symbol] = group
	}
	groups["paid"] = &model.UserGroup{Symbol: "paid", Name: "Paid", Ratio: 1, Enable: &enabled}
	model.GlobalUserGroupRatio.UserGroup = groups
	model.GlobalUserGroupRatio.Unlock()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/kling/v1/videos/text2video", nil)
	c.Set("id", 1)
	c.Set("token_id", 1)
	c.Set("channel_id", 1)
	groupctx.SetRoutingGroup(c, "paid", groupctx.RoutingGroupSourceUserGroup)
	adaptor := &taskSubmitTestAdaptor{
		ctx:        c,
		task:       &model.Task{Platform: model.TaskPlatformKling, UserId: 1, TokenID: 1, Action: "text2video"},
		provider:   &taskTestProvider{channel: channel},
		providerID: "provider-task-1",
	}
	getTaskAdaptorFunc = func(int, *gin.Context) (taskbase.TaskInterface, error) { return adaptor, nil }
	return c, adaptor
}

func TestRelayTaskSubmitCallsProviderOnceAndPersistsAcceptanceBeforeDelivery(t *testing.T) {
	c, adaptor := taskSubmitFixture(t)
	RelayTaskSubmit(c)
	if adaptor.relayCalls.Load() != 1 {
		t.Fatalf("provider submit calls=%d, want 1", adaptor.relayCalls.Load())
	}
	var task model.Task
	if err := model.DB.First(&task, adaptor.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateAccepted || model.TaskProviderID(&task) != "provider-task-1" {
		t.Fatalf("task was delivered without durable acceptance: %+v", task)
	}
}

func TestPrepareTaskAttemptOwnerUsesOnlyMatchingBoundIncarnation(t *testing.T) {
	t.Run("matching retired owner channel", func(t *testing.T) {
		ctx, adaptor := taskSubmitFixture(t)
		if err := model.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
			t.Fatal(err)
		}
		adaptor.ownerChannelID = 1
		if taskErr := prepareTaskAttemptOwner(ctx, adaptor); taskErr != nil {
			t.Fatalf("matching bound channel rejected: %+v", taskErr)
		}
		if adaptor.task.ID == 0 || adaptor.task.ProviderState != model.TaskProviderStateSubmitStarted {
			t.Fatalf("bound child owner was not reserved and claimed: %+v", adaptor.task)
		}
	})

	t.Run("ordinary task cannot use retired channel", func(t *testing.T) {
		ctx, adaptor := taskSubmitFixture(t)
		if err := model.DB.Model(&model.Channel{}).Where("id = ?", 1).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
			t.Fatal(err)
		}
		if taskErr := prepareTaskAttemptOwner(ctx, adaptor); taskErr == nil || adaptor.task.ID != 0 {
			t.Fatalf("ordinary task used retired channel: task=%+v err=%+v", adaptor.task, taskErr)
		}
	})

	t.Run("provider channel must equal owner channel", func(t *testing.T) {
		ctx, adaptor := taskSubmitFixture(t)
		adaptor.ownerChannelID = 2
		taskErr := prepareTaskAttemptOwner(ctx, adaptor)
		if taskErr == nil || taskErr.Code != "task_owner_channel_conflict" || adaptor.task.ID != 0 {
			t.Fatalf("mismatched provider obtained bound admission: task=%+v err=%+v", adaptor.task, taskErr)
		}
	})
}

func TestRelayTaskSubmitDoesNotRetryAndCancelsWithoutUsage(t *testing.T) {
	_, adaptor := taskSubmitFixture(t)
	adaptor.relayErr = taskbase.StringTaskError(http.StatusBadGateway, "submit_failed", "ambiguous provider failure", false)
	RelayTaskSubmit(adaptor.ctx)
	if adaptor.relayCalls.Load() != 1 {
		t.Fatalf("provider submit calls=%d, want 1", adaptor.relayCalls.Load())
	}
	var task model.Task
	if err := model.DB.First(&task, adaptor.task.ID).Error; err != nil {
		t.Fatal(err)
	}
	if task.ProviderState != model.TaskProviderStateClosed || task.Status != model.TaskStatusUnknown || task.ChargedQuota == nil || *task.ChargedQuota != 0 {
		t.Fatalf("ambiguous task did not cancel its reservation: %+v", task)
	}
	var user model.User
	if err := model.DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 1000 {
		t.Fatalf("ambiguous task user quota=%d, want 1000", user.Quota)
	}
}

func TestTaskSubmissionClaimFailureNeverCallsProvider(t *testing.T) {
	c, adaptor := taskSubmitFixture(t)
	adaptor.task.ID = 99
	if taskErr := prepareTaskAttemptOwner(c, adaptor); taskErr == nil {
		t.Fatal("expected conflicting local owner to fail")
	}
	if adaptor.relayCalls.Load() != 0 {
		t.Fatal("provider was called without a submission claim")
	}
}

func TestTaskProvider429UpdatesFutureChannelHealthWithoutRetry(t *testing.T) {
	c, adaptor := taskSubmitFixture(t)
	originalCooldownSeconds := config.RetryCooldownSeconds
	config.RetryCooldownSeconds = 60
	t.Cleanup(func() { config.RetryCooldownSeconds = originalCooldownSeconds })

	channel := adaptor.provider.GetChannel()
	model.ChannelGroup.Cooldowns.Delete("1:task-model")
	adaptor.relayErr = taskbase.StringTaskError(http.StatusTooManyRequests, "rate_limited", "provider rate limited", false)
	RelayTaskSubmit(c)

	if adaptor.relayCalls.Load() != 1 {
		t.Fatalf("provider submit calls=%d, want exactly one", adaptor.relayCalls.Load())
	}
	if !model.ChannelGroup.IsInCooldown(channel.Id, adaptor.GetModelName()) {
		t.Fatal("task provider 429 did not enter model cooldown")
	}
}

func TestTaskBillingAdmissionPreservesFailureClass(t *testing.T) {
	for _, test := range []struct {
		name    string
		err     error
		outcome model.BillingBalanceOutcome
		status  int
		code    string
	}{
		{name: "user quota", err: model.ErrBillingUserQuotaInsufficient, status: http.StatusPaymentRequired, code: "insufficient_user_quota"},
		{name: "token quota", err: model.ErrTokenQuotaInsufficient, status: http.StatusForbidden, code: "insufficient_token_quota"},
		{name: "database", err: errors.New("database unavailable"), status: http.StatusServiceUnavailable, code: "task_billing_admission_unavailable"},
		{name: "commit unknown", err: errors.New("commit acknowledgement lost"), outcome: model.BillingBalanceCommitUnknown, status: http.StatusServiceUnavailable, code: "task_billing_commit_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status, code := taskBillingAdmissionStatus(test.err, test.outcome)
			if status != test.status || code != test.code {
				t.Fatalf("status/code=%d/%q want %d/%q", status, code, test.status, test.code)
			}
		})
	}
}
