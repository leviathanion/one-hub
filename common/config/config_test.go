package config

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestInitConfLoadsRealtimeSessionCompatFlagAndDefaults(t *testing.T) {
	originalCompat := OpenAIRealtimeSessionCompatMode
	originalDecodeEnabled := RequestBodyDecodeEnabled
	originalDecodeMaxWireBytes := RequestBodyDecodeMaxWireBytes
	originalDecodeMaxDecodedBytes := RequestBodyDecodeMaxDecodedBytes
	originalDecodeMaxDecoderWindowBytes := RequestBodyDecodeMaxDecoderWindowBytes
	originalDecodeMaxExpansionRatio := RequestBodyDecodeMaxExpansionRatio
	originalDecodeMaxLayers := RequestBodyDecodeMaxLayers
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
		RequestBodyDecodeEnabled = originalDecodeEnabled
		RequestBodyDecodeMaxWireBytes = originalDecodeMaxWireBytes
		RequestBodyDecodeMaxDecodedBytes = originalDecodeMaxDecodedBytes
		RequestBodyDecodeMaxDecoderWindowBytes = originalDecodeMaxDecoderWindowBytes
		RequestBodyDecodeMaxExpansionRatio = originalDecodeMaxExpansionRatio
		RequestBodyDecodeMaxLayers = originalDecodeMaxLayers
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
	if viper.GetBool("realtime.unsafe_allow_credential_subprotocol_any_origin") {
		t.Fatal("expected credential subprotocol unsafe origin compatibility to be disabled by default")
	}
	if got := viper.GetInt("realtime.websocket_ping_interval_ms"); got != 25000 {
		t.Fatalf("expected realtime websocket ping interval default 25000ms, got %d", got)
	}
	if !viper.GetBool("responses_ws.active_lease_redis_fail_open") {
		t.Fatal("expected ResponsesWS active lease Redis fail-open compatibility to be enabled by default")
	}
	if !viper.GetBool("request_body_decode.enabled") {
		t.Fatal("expected request body decode to be enabled by default")
	}
	if got := viper.GetInt64("request_body_decode.max_wire_bytes"); got != 64<<20 {
		t.Fatalf("expected request body decode max_wire_bytes default 64MiB, got %d", got)
	}
	if got := viper.GetInt64("request_body_decode.max_decoded_bytes"); got != 64<<20 {
		t.Fatalf("expected request body decode max_decoded_bytes default 64MiB, got %d", got)
	}
	if got := viper.GetInt64("request_body_decode.max_decoder_window_bytes"); got != 128<<20 {
		t.Fatalf("expected request body decode max_decoder_window_bytes default 128MiB, got %d", got)
	}
	if got := viper.GetInt64("request_body_decode.max_expansion_ratio"); got != 64 {
		t.Fatalf("expected request body decode max_expansion_ratio default 64, got %d", got)
	}
	if got := viper.GetInt("request_body_decode.max_layers"); got != 2 {
		t.Fatalf("expected request body decode max_layers default 2, got %d", got)
	}

	viper.Set("openai.realtime_session_compat", true)
	viper.Set("request_body_decode.enabled", false)
	viper.Set("request_body_decode.max_wire_bytes", int64(2<<20))
	viper.Set("request_body_decode.max_decoded_bytes", int64(1<<20))
	viper.Set("request_body_decode.max_decoder_window_bytes", int64(8<<20))
	viper.Set("request_body_decode.max_expansion_ratio", int64(8))
	viper.Set("request_body_decode.max_layers", 1)
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
	if RequestBodyDecodeEnabled {
		t.Fatal("expected InitConf to load request body decode enabled=false from viper")
	}
	if RequestBodyDecodeMaxWireBytes != 2<<20 || RequestBodyDecodeMaxDecodedBytes != 1<<20 || RequestBodyDecodeMaxDecoderWindowBytes != 8<<20 || RequestBodyDecodeMaxExpansionRatio != 8 || RequestBodyDecodeMaxLayers != 1 {
		t.Fatalf("expected InitConf to load request body decode limits, got wire=%d bytes=%d window=%d ratio=%d layers=%d", RequestBodyDecodeMaxWireBytes, RequestBodyDecodeMaxDecodedBytes, RequestBodyDecodeMaxDecoderWindowBytes, RequestBodyDecodeMaxExpansionRatio, RequestBodyDecodeMaxLayers)
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

func TestRealtimeWebsocketPingIntervalExplicitNonPositiveDisables(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})

	if got := RealtimeWebsocketPingInterval(); got != 25*time.Second {
		t.Fatalf("expected unset ping interval to use default 25s, got %s", got)
	}

	defaultConfig()
	if got := RealtimeWebsocketPingInterval(); got != 25*time.Second {
		t.Fatalf("expected configured default ping interval to be 25s, got %s", got)
	}

	viper.Set("realtime.websocket_ping_interval_ms", 0)
	if got := RealtimeWebsocketPingInterval(); got != 0 {
		t.Fatalf("expected explicit zero ping interval to disable pings, got %s", got)
	}

	viper.Set("realtime.websocket_ping_interval_ms", -1)
	if got := RealtimeWebsocketPingInterval(); got != 0 {
		t.Fatalf("expected explicit negative ping interval to disable pings, got %s", got)
	}

	viper.Set("realtime.websocket_ping_interval_ms", 1250)
	if got := RealtimeWebsocketPingInterval(); got != 1250*time.Millisecond {
		t.Fatalf("expected explicit ping interval to be applied, got %s", got)
	}
}

func TestResponsesWSClientPongTimeoutDefaultsAndExplicitDisable(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})

	if got := ResponsesWSClientPongTimeout(); got != 5*time.Minute {
		t.Fatalf("expected unset client pong timeout to use default 5m, got %s", got)
	}

	defaultConfig()
	if got := ResponsesWSClientPongTimeout(); got != 5*time.Minute {
		t.Fatalf("expected configured default client pong timeout to be 5m, got %s", got)
	}

	viper.Set("responses_ws.client_pong_timeout_ms", 0)
	if got := ResponsesWSClientPongTimeout(); got != 0 {
		t.Fatalf("expected explicit zero client pong timeout to disable watchdog, got %s", got)
	}

	viper.Set("responses_ws.client_pong_timeout_ms", -1)
	if got := ResponsesWSClientPongTimeout(); got != 0 {
		t.Fatalf("expected explicit negative client pong timeout to disable watchdog, got %s", got)
	}

	viper.Set("responses_ws.client_pong_timeout_ms", 1500)
	if got := ResponsesWSClientPongTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("expected explicit client pong timeout to be applied, got %s", got)
	}
}
