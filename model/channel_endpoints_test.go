package model

import (
	"encoding/json"
	"reflect"
	"testing"

	"gorm.io/datatypes"
	"one-api/common/config"
	"one-api/common/providerendpoint"
)

func TestCustomEndpointRuntimeRejectsLegacyWithoutConverting(t *testing.T) {
	for _, raw := range []string{`{"customize":{"16":"/v1/responses"}}`, `{"claude":{"enabled":true}}`, `{"endpoints":{},"customize":{}}`} {
		var plugin datatypes.JSONType[PluginType]
		if err := json.Unmarshal([]byte(raw), &plugin); err != nil {
			t.Fatal(err)
		}
		channel := &Channel{Type: config.ChannelTypeCustom, Plugin: &plugin}
		before, _ := json.Marshal(channel.Plugin)
		if err := channel.ValidateRuntimeConfigJSON(); err == nil {
			t.Fatal("legacy config accepted")
		}
		if _, err := channel.ResolveEndpoint(providerendpoint.Responses); err == nil {
			t.Fatal("legacy config resolved at runtime")
		}
		after, _ := json.Marshal(channel.Plugin)
		if string(before) != string(after) {
			t.Fatal("runtime rewrote legacy data")
		}
	}
	channel := &Channel{Type: config.ChannelTypeCustom}
	if uri, err := channel.ResolveEndpoint(providerendpoint.Responses); err != nil || uri != "" {
		t.Fatalf("missing config enabled endpoint: %q %v", uri, err)
	}
}

func TestCustomEndpointDisableRetainsAddressAndFencesBackgroundIdentity(t *testing.T) {
	useTestChannelDB(t)
	plugin := NewCustomEndpointPlugin()
	plugin.Data()["endpoints"][providerendpoint.Messages] = (providerendpoint.Setting{Enabled: true, UpstreamURL: "https://claude.example.com/custom/messages"}).Data()
	channel := &Channel{Id: 19, Type: config.ChannelTypeCustom, Name: "endpoint", Key: "key", BaseURL: stringPtr("https://base.example"), Group: "default", Models: "test", Plugin: plugin}
	insertTestChannel(t, channel)
	updated := NewCustomEndpointPlugin()
	updated.Data()["endpoints"][providerendpoint.Messages] = (providerendpoint.Setting{Enabled: false, UpstreamURL: "https://claude.example.com/custom/messages"}).Data()
	if err := (&Channel{Id: 19, Plugin: updated}).UpdateRaw(false); err == nil {
		t.Fatal("background changed endpoint identity")
	}
	body, _ := json.Marshal(map[string]any{"id": 19, "plugin": updated})
	var edit ChannelEditRequest
	if err := json.Unmarshal(body, &edit); err != nil {
		t.Fatal(err)
	}
	if err := edit.Update(); err != nil {
		t.Fatal(err)
	}
	stored, err := GetChannelById(19)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Id != 19 || stored.CredentialRevision != 1 || !reflect.DeepEqual(stored.EndpointSettings()[providerendpoint.Messages], updated.Data()["endpoints"][providerendpoint.Messages]) {
		t.Fatal("disable lost saved URL or failed to fence identity")
	}
}

func TestDisabledEndpointAddressChangesStillRequireAdministratorEdit(t *testing.T) {
	for _, definition := range providerendpoint.Definitions() {
		id := definition.ID
		t.Run(id, func(t *testing.T) {
			useTestChannelDB(t)
			plugin := NewCustomEndpointPlugin()
			plugin.Data()["endpoints"][id] = (providerendpoint.Setting{UpstreamURL: "https://old.example/resource"}).Data()
			channel := &Channel{Id: 9, Type: config.ChannelTypeCustom, Key: "key", Plugin: plugin}
			insertTestChannel(t, channel)
			updated := NewCustomEndpointPlugin()
			updated.Data()["endpoints"][id] = (providerendpoint.Setting{UpstreamURL: "https://new.example/resource"}).Data()
			if err := (&Channel{Id: 9, Plugin: updated}).UpdateRaw(false); err == nil {
				t.Fatal("background replaced disabled resource target")
			}
			stored, err := GetChannelById(9)
			if err != nil || !reflect.DeepEqual(stored.EndpointSettings()[id], plugin.Data()["endpoints"][id]) {
				t.Fatal("rejected edit modified saved target")
			}
		})
	}
}
