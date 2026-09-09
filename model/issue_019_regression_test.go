package model

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"one-api/common/config"

	"gorm.io/datatypes"
)

func i019Plugin(endpoint any) *datatypes.JSONType[PluginType] {
	enabled := endpoint != "disable"
	if endpoint == nil || !enabled {
		endpoint = ""
	}
	plugin := datatypes.NewJSONType(PluginType{"endpoints": {
		"openai.responses":        map[string]any{"enabled": enabled, "upstream_url": endpoint},
		"openai.chat_completions": map[string]any{"enabled": true, "upstream_url": "/v1/chat/completions"},
	}})
	return &plugin
}

func TestFixI019_CustomResponsesIdentityChangesRequireExplicitEdit(t *testing.T) {
	for _, entry := range []string{"ordinary_edit", "raw_edit", "tag_edit"} {
		for _, endpoint := range []any{
			"https://other.example/tenant-a/responses?account=one",
			"https://original.example/tenant-b/responses?account=one",
			"https://original.example/tenant-a/responses?account=two",
			"https://original.example/tenant%2Fa/responses?account=one",
			"/v2/responses",
			"disable",
			nil,
		} {
			t.Run(entry+"/"+strconv.Quote(fmtI019Endpoint(endpoint)), func(t *testing.T) {
				useTestChannelDB(t)
				original := "https://original.example/tenant-a/responses?account=one"
				channel := &Channel{Id: 19, Type: config.ChannelTypeCustom, Name: "original", Key: "i019-test-key", Group: "default", Tag: "i019-tag", Models: "i019-model", BaseURL: stringPtr("https://base.example"), Plugin: i019Plugin(original)}
				insertTestChannel(t, channel)
				candidate := &Channel{Id: 19, Name: "must-not-persist", Plugin: i019Plugin(endpoint)}
				var err error
				switch entry {
				case "ordinary_edit":
					body, marshalErr := json.Marshal(map[string]any{"id": channel.Id, "name": candidate.Name, "plugin": candidate.Plugin})
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					var edit ChannelEditRequest
					if err := json.Unmarshal(body, &edit); err != nil {
						t.Fatal(err)
					}
					err = edit.Update()
				case "raw_edit":
					err = candidate.UpdateRaw(false)
				case "tag_edit":
					err = UpdateChannelsTagWithSubmittedFields("i019-tag", candidate, ChannelTagSubmittedFields{"plugin": {}})
				}
				if entry == "ordinary_edit" {
					if err != nil {
						t.Fatalf("管理编辑应允许修改端点: %v", err)
					}
					stored, loadErr := GetChannelById(channel.Id)
					if loadErr != nil || stored.Name != candidate.Name || stored.CredentialRevision != 1 || !reflect.DeepEqual(stored.EndpointSettings()["openai.responses"], i019Plugin(endpoint).Data()["endpoints"]["openai.responses"]) {
						t.Fatalf("管理编辑未原地保存端点: %v", loadErr)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "Responses") {
					t.Fatalf("后台端点身份变更未被明确拒绝: %v", err)
				}
				stored, err := GetChannelById(channel.Id)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Name != channel.Name || stored.EndpointSettings()["openai.responses"].(map[string]any)["upstream_url"] != original {
					t.Fatalf("被拒绝的编辑修改了原渠道: %+v", stored)
				}
			})
		}
	}
}

func TestFixI019_AllowsExplicitEndpointEditsAndNewChannels(t *testing.T) {
	for _, baseURL := range []string{"https://original.example", "https://gateway.ai.cloudflare.com/tenant/gateway/openai"} {
		t.Run(baseURL, func(t *testing.T) {
			useTestChannelDB(t)
			channel := &Channel{Id: 19, Type: config.ChannelTypeCustom, Name: "old", Key: "i019-test-key", Group: "default", Models: "i019-model", BaseURL: stringPtr(baseURL), Plugin: i019Plugin("/v1/responses?tenant=one&tenant=two")}
			insertTestChannel(t, channel)
			plugin := i019Plugin("/v1/responses?tenant=one&tenant=two")
			plugin.Data()["endpoints"]["openai.chat_completions"] = map[string]any{"enabled": true, "upstream_url": "https://chat.example/other/chat"}
			weight := uint(7)
			candidate := &Channel{Id: 19, Name: "business-edit", Weight: &weight, Plugin: plugin}
			if err := candidate.UpdateRaw(false); err == nil {
				t.Fatal("后台不能改写携带渠道凭据的 Chat 上游地址")
			}
			if err := candidate.UpdateRawWithOptions(false, ChannelUpdateOptions{AllowIdentityChange: true}); err != nil {
				t.Fatalf("非身份编辑失败: %v", err)
			}
			stored, err := GetChannelById(19)
			if err != nil || stored.Name != "business-edit" || stored.Weight == nil || *stored.Weight != 7 {
				t.Fatalf("业务编辑未保存: %+v %v", stored, err)
			}
			newChannel := &Channel{Type: config.ChannelTypeCustom, Name: "new-owner", Key: "i019-new-test-key", Group: "default", Models: "i019-model", BaseURL: stringPtr(baseURL), Plugin: i019Plugin("https://other.example/new-namespace/responses")}
			if err := newChannel.Insert(); err != nil {
				t.Fatalf("新渠道配置新身份失败: %v", err)
			}
			if newChannel.Id == 19 {
				t.Fatal("新渠道重用了旧身份")
			}
		})
	}
}

func fmtI019Endpoint(endpoint any) string {
	if endpoint == nil {
		return "removed"
	}
	return endpoint.(string)
}

func TestCustomEndpointsRejectNumericAliases(t *testing.T) {
	plugin := i019Plugin("/v1/responses")
	plugin.Data()["endpoints"]["016"] = map[string]any{"enabled": true, "upstream_url": "/v1/responses"}
	channel := &Channel{Type: config.ChannelTypeCustom, Plugin: plugin}
	if err := channel.ValidateRuntimeConfigJSON(); err == nil {
		t.Fatal("numeric endpoint aliases must not enter runtime config")
	}
}

func TestFixI019_IgnoredCustomMappingOnOtherChannelTypesCanChange(t *testing.T) {
	useTestChannelDB(t)
	channel := &Channel{Id: 19, Type: config.ChannelTypeOpenAI, Key: "i019-test-key", BaseURL: stringPtr("https://original.example"), Plugin: i019Plugin("https://ignored.example/a")}
	insertTestChannel(t, channel)
	if err := (&Channel{Id: 19, Plugin: i019Plugin("https://ignored.example/b")}).UpdateRaw(false); err != nil {
		t.Fatalf("未参与非 Custom 端点构造的字段不应变为身份: %v", err)
	}
}

func TestFixI019_ResponsesWhitespaceNormalizationPreservesNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, base, original, replacement string
		allowed                           bool
	}{
		{"encoded_space_changes_namespace", "https://original.example", "/tenant/responses ", "https://original.example/tenant/responses%20", false},
		{"trimmed_relative_changes_realtime_named_namespace", "https://original.example", "/tenant/responses ", "https://original.example/tenant/responses", false},
		{"leading_space_changes_ws_namespace", "https://original.example/root", " /tenant/responses", "https://original.example/root/tenant/responses", false},
		{"leading_space_changes_http_namespace", "https://original.example/root", " /tenant/responses", "https://original.example/root%20/tenant/responses", false},
		{"business_edit_preserves_http_only_configuration", "https://original.example/root", "\t /tenant/responses \t", "\t /tenant/responses \t", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestChannelDB(t)
			channel := &Channel{Id: 19, Type: config.ChannelTypeCustom, Name: "original", Key: "i019-test-key", BaseURL: stringPtr(tc.base), Plugin: i019Plugin(tc.original)}
			insertTestChannel(t, channel)
			err := (&Channel{Id: 19, Name: "business-edit", Plugin: i019Plugin(tc.replacement)}).UpdateRaw(false)
			if tc.allowed {
				if err != nil {
					t.Fatalf("实际同一 Responses 命名空间被拒绝: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "Responses") {
					t.Fatalf("实际不同 Responses 命名空间被接受: %v", err)
				}
				stored, err := GetChannelById(19)
				if err != nil || stored.EndpointSettings()["openai.responses"].(map[string]any)["upstream_url"] != tc.original {
					t.Fatalf("被拒绝的编辑修改了旧端点: %+v %v", stored, err)
				}
			}
		})
	}
}

func TestFixI019_RealtimeNamedResponsesEndpointRemainsImmutable(t *testing.T) {
	for _, tc := range []struct{ name, original, replacement string }{
		{"relative_to_absolute", "/tenant/responses", "https://original.example/root/tenant/responses"},
		{"relative_trailing_space_removed", "/tenant/responses ", "/tenant/responses"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestChannelDB(t)
			channel := &Channel{Id: 19, Type: config.ChannelTypeCustom, Name: "original", Key: "i019-test-key", BaseURL: stringPtr("https://original.example/root"), Plugin: i019Plugin(tc.original)}
			insertTestChannel(t, channel)
			if err := (&Channel{Id: 19, Plugin: i019Plugin(tc.replacement)}).UpdateRaw(false); err == nil {
				t.Fatal("普通模型下同址的编辑改变了 -realtime 命名分支的实际端点")
			}
			stored, err := GetChannelById(19)
			if err != nil || stored.EndpointSettings()["openai.responses"].(map[string]any)["upstream_url"] != tc.original {
				t.Fatalf("被拒绝的编辑修改了端点: %+v %v", stored, err)
			}
		})
	}
}
