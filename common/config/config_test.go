package config

import (
	"testing"

	"github.com/spf13/viper"
)

func TestInitConfLoadsRealtimeSessionCompatFlagAndDefaults(t *testing.T) {
	originalCompat := OpenAIRealtimeSessionCompatMode
	originalUserInvoiceMonth := UserInvoiceMonth
	originalGitHubProxy := GitHubProxy
	originalMCPEnable := MCP_ENABLE
	originalUptimeKumaEnable := UPTIMEKUMA_ENABLE
	originalUptimeKumaDomain := UPTIMEKUMA_DOMAIN
	originalUptimeKumaStatusPage := UPTIMEKUMA_STATUS_PAGE_NAME

	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
		OpenAIRealtimeSessionCompatMode = originalCompat
		UserInvoiceMonth = originalUserInvoiceMonth
		GitHubProxy = originalGitHubProxy
		MCP_ENABLE = originalMCPEnable
		UPTIMEKUMA_ENABLE = originalUptimeKumaEnable
		UPTIMEKUMA_DOMAIN = originalUptimeKumaDomain
		UPTIMEKUMA_STATUS_PAGE_NAME = originalUptimeKumaStatusPage
	})

	defaultConfig()
	if viper.GetBool("openai.realtime_session_compat") {
		t.Fatal("expected realtime session compat mode default to be disabled")
	}
	if got := viper.GetInt("codex.execution_session_revocation_timeout_ms"); got != 200 {
		t.Fatalf("expected codex execution session revocation timeout default 200ms, got %d", got)
	}

	viper.Set("openai.realtime_session_compat", true)
	viper.Set("user_invoice_month", true)
	viper.Set("github_proxy", "https://proxy.example")
	viper.Set("mcp.enable", true)
	viper.Set("uptime_kuma.enable", true)
	viper.Set("uptime_kuma.domain", "status.example.com")
	viper.Set("uptime_kuma.status_page_name", "main")

	InitConf()

	if !OpenAIRealtimeSessionCompatMode {
		t.Fatal("expected InitConf to load realtime session compat mode from viper")
	}
	if !UserInvoiceMonth {
		t.Fatal("expected InitConf to load user_invoice_month from viper")
	}
	if GitHubProxy != "https://proxy.example" {
		t.Fatalf("expected github proxy to round-trip through InitConf, got %q", GitHubProxy)
	}
	if !MCP_ENABLE || !UPTIMEKUMA_ENABLE {
		t.Fatalf("expected InitConf to load nested boolean defaults, got mcp=%v uptime=%v", MCP_ENABLE, UPTIMEKUMA_ENABLE)
	}
	if UPTIMEKUMA_DOMAIN != "status.example.com" || UPTIMEKUMA_STATUS_PAGE_NAME != "main" {
		t.Fatalf("expected InitConf to load nested uptime kuma strings, got domain=%q page=%q", UPTIMEKUMA_DOMAIN, UPTIMEKUMA_STATUS_PAGE_NAME)
	}
}
