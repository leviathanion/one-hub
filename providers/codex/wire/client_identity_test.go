package wire

import (
	"net/http"
	"strings"
	"testing"

	"one-api/common/requestctx"
)

func TestResolveClientIdentityPriority(t *testing.T) {
	configured := ChannelPolicy{DefaultUserAgent: "configured/1", DefaultOriginator: "configured"}
	for _, tc := range []struct {
		name, ua, originator   string
		policy                 ChannelPolicy
		wantUA, wantOriginator string
	}{
		{"both client values", "codex-tui/1", "custom client/next", configured, "codex-tui/1", "custom client/next"},
		{"only client UA", "codex_cli_rs/1", "", configured, "codex_cli_rs/1", ""},
		{"only client originator", "", "CODEX_VSCODE", configured, "", "CODEX_VSCODE"},
		{"blank client originator", "codex-tui/8", " \t", configured, "codex-tui/8", ""},
		{"configured pair", "", "", configured, "configured/1", "configured"},
		{"only configured originator", "", "", ChannelPolicy{DefaultOriginator: "custom"}, DefaultUserAgent(), "custom"},
		{"only configured UA", "", "", ChannelPolicy{DefaultUserAgent: "custom/1"}, "custom/1", "pi"},
		{"non Codex values use config", "curl/8", "random", configured, "configured/1", "configured"},
		{"non Codex values use PI defaults", "Mozilla/5.0", "random", ChannelPolicy{}, DefaultUserAgent(), "pi"},
		{"PI defaults", "", "", ChannelPolicy{}, DefaultUserAgent(), "pi"},
		{"whitespace is missing", " \t", " ", ChannelPolicy{}, DefaultUserAgent(), "pi"},
		{"long client UA", "codex-tui/" + strings.Repeat("x", 300), "", configured, "codex-tui/" + strings.Repeat("x", 300), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"User-Agent": {tc.ua}, "Originator": {tc.originator}}
			identity, _, err := ResolveClientIdentity(requestctx.NewHeaderSnapshot(headers), tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			if identity.UserAgent != tc.wantUA || identity.Originator != tc.wantOriginator {
				t.Fatalf("got UA=%q originator=%q, want %q %q", identity.UserAgent, identity.Originator, tc.wantUA, tc.wantOriginator)
			}
		})
	}
}

func TestResolveClientIdentityRejectsUnsafeAndAmbiguousHeaders(t *testing.T) {
	for _, name := range []string{"User-Agent", "originator"} {
		for _, values := range [][]string{{"a", "b"}, {"good\r\nInjected: value"}, {"bad\x00value"}, {"bad\n"}, {strings.Repeat("x", 16*1024+1)}} {
			headers := http.Header{"User-Agent": {"codex-tui/1"}, "originator": {"codex-tui"}}
			headers[name] = values
			_, _, err := ResolveClientIdentity(requestctx.NewHeaderSnapshot(headers), ChannelPolicy{})
			if err == nil {
				t.Fatalf("accepted unsafe or ambiguous %s", name)
			}
		}
	}
}

func TestPiUserAgentPlatformNames(t *testing.T) {
	for _, tc := range []struct{ platform, release, arch, want string }{
		{"linux", "6.12.1", "amd64", "pi (linux 6.12.1; x64)"},
		{"darwin", "24.0.0", "arm64", "pi (darwin 24.0.0; arm64)"},
		{"windows", "10.0.26100", "386", "pi (win32 10.0.26100; ia32)"},
	} {
		if got := formatPiUserAgent(tc.platform, tc.release, tc.arch); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
}
