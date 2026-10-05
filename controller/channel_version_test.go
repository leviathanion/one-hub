package controller

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"one-api/common/config"
	commonTest "one-api/common/test"
	"one-api/model"

	"github.com/gin-gonic/gin"
)

func TestChannelEditAPIVersionConflictPreservesStoredConfiguration(t *testing.T) {
	useControllerChannelTagTestDB(t)
	if err := model.DB.Create(&model.Channel{Id: 35001, Type: config.ChannelTypeOpenAI, Key: "secret", Name: "current", Version: 3}).Error; err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"id":35001,"name":"stale","expected_version":2}`, `{"id":35001,"name":"missing"}`, `{"id":35001,"expected_version":3,"internal_state":{}}`} {
		ctx, response := commonTest.GetContext(http.MethodPut, "/api/channel/", commonTest.RequestJSONConfig(), bytes.NewBufferString(raw))
		UpdateChannel(ctx)
		var result struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Success {
			t.Fatalf("invalid edit accepted: %s", response.Body.String())
		}
		if raw == `{"id":35001,"name":"stale","expected_version":2}` && response.Code != http.StatusConflict {
			t.Fatalf("stale edit status=%d", response.Code)
		}
	}
	row, err := model.GetChannelById(35001)
	if err != nil || row.Name != "current" || row.Version != 3 || len(row.InternalState) != 0 {
		t.Fatal("rejected edit changed channel")
	}
}

func TestTagEditAPIVersionConflictIsAtomic(t *testing.T) {
	useControllerChannelTagTestDB(t)
	for _, id := range []int{35002, 35003} {
		if err := model.DB.Create(&model.Channel{Id: id, Type: config.ChannelTypeOpenAI, Key: "secret", Tag: "version-api", Models: "old", Version: 2}).Error; err != nil {
			t.Fatal(err)
		}
	}
	body := `{"models":"new","expected_versions":{"35002":2,"35003":1}}`
	ctx, response := commonTest.GetContext(http.MethodPut, "/api/channel_tag/version-api", commonTest.RequestJSONConfig(), bytes.NewBufferString(body))
	ctx.Params = gin.Params{{Key: "tag", Value: "version-api"}}
	UpdateChannelsTag(ctx)
	if response.Code != http.StatusConflict {
		t.Fatalf("tag conflict status=%d response=%s", response.Code, response.Body.String())
	}
	rows, err := model.GetChannelsByTag("version-api")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Models != "old" || row.Version != 2 {
			t.Fatal("tag API partially updated configuration")
		}
	}
}
