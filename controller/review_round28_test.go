package controller

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"
	"one-api/providers/codex"

	"github.com/gin-gonic/gin"
)

func TestConsumeCodexResetCreditCommittedUnusableResponseSucceedsAndRotatesCaches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cache.InitCacheManager()
	const channelID = 28
	if err := cache.SetCache("codex:usage:preview:28", "legacy", time.Minute); err != nil {
		t.Fatal(err)
	}

	originalLoad := loadCodexUsageChannelByID
	originalCreate := createCodexUsageProvider
	originalClear := clearCodexUsageCacheForChannel
	clearCalls := 0
	loadCodexUsageChannelByID = func(int) (*model.Channel, error) {
		return &model.Channel{Id: channelID, Type: config.ChannelTypeCodex, Key: "credentials"}, nil
	}
	createCodexUsageProvider = func(*model.Channel) (codexUsageProvider, error) {
		return &fakeCodexUsageProvider{
			resetResult: &codex.CodexResetResult{ChannelID: channelID, Code: "200"},
			resetErr:    errors.Join(codex.ErrResetCreditCommittedResponseUnusable, errors.New("too large")),
		}, nil
	}
	clearCodexUsageCacheForChannel = func(*model.Channel) error {
		clearCalls++
		return nil
	}
	t.Cleanup(func() {
		loadCodexUsageChannelByID = originalLoad
		createCodexUsageProvider = originalCreate
		clearCodexUsageCacheForChannel = originalClear
	})

	ctx, recorder := commonTest.GetContext(http.MethodPost, "/api/channel/28/codex/reset-credit", commonTest.RequestJSONConfig(), nil)
	ctx.Params = gin.Params{{Key: "id", Value: "28"}}
	ConsumeCodexResetCredit(ctx)

	body := recorder.Body.String()
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, `"warning"`) || !strings.Contains(body, "do not retry") {
		t.Fatalf("committed reset must be successful with a safe warning: %s", body)
	}
	if clearCalls != 1 {
		t.Fatalf("v2 generation rotation must always run, calls=%d", clearCalls)
	}
	if _, err := cache.GetCache[string]("codex:usage:preview:28"); !errors.Is(err, cache.CacheNotFound) {
		t.Fatalf("legacy cache must always be invalidated: %v", err)
	}
}
