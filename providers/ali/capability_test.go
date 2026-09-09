package ali

import (
	"testing"

	"gorm.io/datatypes"

	"one-api/model"
)

func TestAliFactoryRejectsPluginSearchBeforeProviderConstruction(t *testing.T) {
	plugin := datatypes.NewJSONType(model.PluginType{"web_search": {"enable": true}})
	channel := &model.Channel{Plugin: &plugin}
	if _, err := (AliProviderFactory{}).AssessChatRequest(channel, "qwen-plus", nil); err == nil {
		t.Fatal("factory accepted enabled DashScope search without billing evidence")
	}
	if _, err := (AliProviderFactory{}).AssessChatRequest(channel, "unsupported-model", nil); err != nil {
		t.Fatalf("plugin search does not activate for unsupported model: %v", err)
	}
}
