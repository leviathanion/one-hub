package providerendpoint

import "testing"

func TestEndpointSettingsSeparatePermissionAndAddress(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings map[string]any
		want     string
		invalid  bool
	}{
		{"missing", nil, "", false},
		{"default", map[string]any{Responses: map[string]any{"enabled": true}}, DefaultResponsesURI, false},
		{"disabled_keeps_address", map[string]any{Responses: Setting{UpstreamURL: "/retained"}.Data()}, "", false},
		{"raw_path", map[string]any{Responses: Setting{Enabled: true, UpstreamURL: " /tenant%2Fa/responses?a=1&a=2 "}.Data()}, " /tenant%2Fa/responses?a=1&a=2 ", false},
		{"old_string", map[string]any{Responses: "disable"}, "", true},
		{"missing_enabled", map[string]any{Responses: map[string]any{"upstream_url": ""}}, "", true},
		{"string_enabled", map[string]any{Responses: map[string]any{"enabled": "false"}}, "", true},
		{"null_url", map[string]any{Responses: map[string]any{"enabled": true, "upstream_url": nil}}, "", true},
		{"typo", map[string]any{Responses: map[string]any{"enabled": true, "url": "/wrong"}}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Resolve(tc.settings, Responses)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("Resolve=%q,%v; want %q invalid=%v", got, err, tc.want, tc.invalid)
			}
		})
	}
}

func TestEndpointURLValidationOwnsOnlyAddressEnvelope(t *testing.T) {
	for _, uri := range []string{"", "/v1/messages", "/tenant%2Fa/responses?k=1&k=2", "https://example.com:8443/root?token=opaque", "http://127.0.0.1/test"} {
		if err := ValidateUpstreamURL(uri); err != nil {
			t.Fatalf("valid URL rejected: %q: %v", uri, err)
		}
	}
	for _, uri := range []string{"disable", "v1/messages", "//evil.example/api", "ftp://example.com/api", "https://", "https://user:password@example.com", "/api#fragment", "   ", "/bad%path"} {
		if err := ValidateUpstreamURL(uri); err == nil {
			t.Fatalf("invalid URL accepted: %q", uri)
		}
	}
}

func TestEndpointCatalogHasStableUniqueIdentifiers(t *testing.T) {
	seen := map[string]bool{}
	for _, definition := range Definitions() {
		if seen[definition.ID] || definition.ID == "" || definition.DefaultPath == "" {
			t.Fatalf("invalid definition: %+v", definition)
		}
		seen[definition.ID] = true
		if definition.ID != Messages {
			got, ok := ForRelayMode(definition.RelayMode)
			if !ok || got.ID != definition.ID {
				t.Fatalf("relay mode is ambiguous: %+v", definition)
			}
		}
	}
}
