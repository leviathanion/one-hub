package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-gormigrate/gormigrate/v2"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/common/providerendpoint"
	"one-api/internal/testutil/sqlitetest"
)

func TestCustomEndpointMigrationPreservesEffectiveSettings(t *testing.T) {
	for _, tc := range []struct {
		name, raw, base, responses, messages string
		disabled                             bool
		invalid                              bool
	}{
		{"null", `null`, "https://base.example", "/v1/responses", "", false, false},
		{"empty", `{}`, "https://base.example", "/v1/responses", "", false, false},
		{"aliases", `{"customize":{"16":"/tenant%2Fa/responses?k=1&k=2","016":"/tenant%2Fa/responses?k=1&k=2"}}`, "https://base.example", "/tenant%2Fa/responses?k=1&k=2", "", false, false},
		{"disabled", `{"customize":{"16":"disable"}}`, "https://base.example", "", "", true, false},
		{"non_string_default", `{"customize":{"16":123}}`, "https://base.example", "/v1/responses", "", false, false},
		{"whitespace", `{"customize":{"16":" /tenant/responses "}}`, "https://base.example", " /tenant/responses ", "", false, false},
		{"claude_root", `{"claude":{"enabled":true,"base_url":"https://claude.example/root/"}}`, "https://base.example", "/v1/responses", "https://claude.example/root/v1/messages", false, false},
		{"claude_cloudflare", `{"claude":{"enabled":true,"base_url":"https://gateway.ai.cloudflare.com/account/gateway/anthropic/"}}`, "https://base.example", "/v1/responses", "https://gateway.ai.cloudflare.com/account/gateway/anthropic/messages", false, false},
		{"claude_inherited", `{"claude":{"enabled":true}}`, "https://base.example/root/", "/v1/responses", "https://base.example/root/v1/messages", false, false},
		{"claude_normalized", `{"claude":{"enabled":true}}`, " https://base.example/root/// ", "/v1/responses", "https://base.example/root/v1/messages", false, false},
		{"claude_default", `{"claude":{"enabled":true}}`, "", "/v1/responses", "https://api.anthropic.com/v1/messages", false, false},
		{"conflict", `{"customize":{"16":"/a","016":"/b"}}`, "https://base.example", "", "", false, true},
		{"unknown", `{"customize":{"999":"/future"}}`, "https://base.example", "", "", false, true},
		{"mixed", `{"endpoints":{},"customize":{}}`, "https://base.example", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var plugin *datatypes.JSONType[PluginType]
			if err := json.Unmarshal([]byte(tc.raw), &plugin); err != nil {
				t.Fatal(err)
			}
			channel := &Channel{Type: config.ChannelTypeCustom, Plugin: plugin, BaseURL: &tc.base}
			changed, err := migrateCustomEndpointConfig(channel)
			if tc.invalid {
				if err == nil {
					t.Fatal("ambiguous migration accepted")
				}
				return
			}
			if err != nil || !changed {
				t.Fatalf("migration: %v changed=%v", err, changed)
			}
			uri, err := channel.ResolveEndpoint(providerendpoint.Responses)
			if err != nil || uri != tc.responses {
				t.Fatalf("Responses=%q,%v want %q", uri, err, tc.responses)
			}
			messages, err := channel.CustomClaudeEndpointIdentity()
			if err != nil || messages != tc.messages {
				t.Fatalf("Messages=%q,%v want %q", messages, err, tc.messages)
			}
			before, _ := json.Marshal(channel.Plugin)
			if changed, err := migrateCustomEndpointConfig(channel); err != nil || changed {
				t.Fatalf("second migration: %v changed=%v", err, changed)
			}
			after, _ := json.Marshal(channel.Plugin)
			if string(before) != string(after) {
				t.Fatal("idempotent migration changed configuration")
			}
		})
	}
}

func TestCustomEndpointDatabaseMigrationIsAtomicFullAndIdempotent(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{true: "rollback", false: "success"}[fail], func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&Channel{}); err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 205; i++ {
				plugin := datatypes.NewJSONType(PluginType{"customize": {"16": "/tenant/responses"}, "vendor": {"retained": "value"}, "claude": {"enabled": false, "base_url": "https://claude.example/root"}})
				channel := Channel{Id: i, Type: config.ChannelTypeCustom, Plugin: &plugin, CredentialRevision: 7}
				if i == 2 {
					channel.Type = config.ChannelTypeOpenAI
				}
				if i == 3 {
					channel.DeletedAt = gorm.DeletedAt{Time: time.Now(), Valid: true}
				}
				if i == 205 && fail {
					plugin.Data()["customize"]["016"] = "/conflict"
				}
				if err := db.Create(&channel).Error; err != nil {
					t.Fatal(err)
				}
			}
			migration := migrateCustomChannelEndpoints()
			migrator := gormigrate.New(db, gormigrate.DefaultOptions, []*gormigrate.Migration{migration})
			err = migrator.Migrate()
			if fail {
				if err == nil || !strings.Contains(err.Error(), "205") {
					t.Fatalf("expected failing channel identity: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err := migrator.Migrate(); err != nil {
					t.Fatal(err)
				}
				if err := migration.Migrate(db); err != nil {
					t.Fatal(err)
				}
			}
			var channels []Channel
			if err := db.Unscoped().Order("id").Find(&channels).Error; err != nil {
				t.Fatal(err)
			}
			if len(channels) != 205 {
				t.Fatal("migration lost rows")
			}
			for _, channel := range channels {
				_, legacy := channel.Plugin.Data()["customize"]
				if legacy != (fail || channel.Type != config.ChannelTypeCustom) {
					t.Fatalf("unexpected format on channel %d", channel.Id)
				}
				if channel.CredentialRevision != 7 || channel.Plugin.Data()["vendor"]["retained"] != "value" {
					t.Fatal("migration changed independent state")
				}
				if !legacy {
					setting, err := providerendpoint.Read(channel.EndpointSettings(), providerendpoint.Messages)
					if err != nil || setting.Enabled || setting.UpstreamURL != "https://claude.example/root/v1/messages" {
						t.Fatal("disabled Claude address was lost")
					}
				}
			}
		})
	}
}
