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
	if got := viper.GetInt("realtime_websocket_client_ping_interval_ms"); got != 25000 {
		t.Fatalf("expected realtime websocket client ping interval default 25000ms, got %d", got)
	}
	if got := viper.GetInt("realtime_websocket_client_pong_miss_timeout_ms"); got != 0 {
		t.Fatalf("expected realtime websocket client pong miss timeout default 0ms, got %d", got)
	}
	if got := viper.GetInt("realtime_websocket_client_inbound_activity_timeout_ms"); got != 0 {
		t.Fatalf("expected realtime websocket client inbound activity timeout default 0ms, got %d", got)
	}
	if got := viper.GetInt("responses_websocket_client_ping_interval_ms"); got != 25000 {
		t.Fatalf("expected responses websocket client ping interval default 25000ms, got %d", got)
	}
	if got := viper.GetInt("responses_websocket_client_pong_miss_timeout_ms"); got != 10000 {
		t.Fatalf("expected responses websocket client pong miss timeout default 10000ms, got %d", got)
	}
	if got := viper.GetInt("responses_websocket_client_inbound_activity_timeout_ms"); got != 60000 {
		t.Fatalf("expected responses websocket client inbound activity timeout default 60000ms, got %d", got)
	}
	if got := viper.GetInt("responses_ws.active_turn_timeout_ms"); got != 120000 {
		t.Fatalf("expected ResponsesWS active turn timeout default 120000ms, got %d", got)
	}
	if got := viper.GetInt("responses_ws.max_lifetime_ms"); got != 3600000 {
		t.Fatalf("expected ResponsesWS max lifetime default 3600000ms, got %d", got)
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
	if !UPTIMEKUMA_ENABLE {
		t.Fatal("expected InitConf to load nested boolean defaults, got uptime=false")
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

func TestConnectTimeoutUsesConfiguredSecondsWithFallback(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})

	if got := ConnectTimeout(); got != 30*time.Second {
		t.Fatalf("expected unset connect timeout to use default 30s, got %s", got)
	}

	defaultConfig()
	if got := ConnectTimeout(); got != 30*time.Second {
		t.Fatalf("expected configured default connect timeout to be 30s, got %s", got)
	}

	viper.Set("connect_timeout", 12)
	if got := ConnectTimeout(); got != 12*time.Second {
		t.Fatalf("expected connect timeout override to be 12s, got %s", got)
	}

	viper.Set("connect_timeout", 0)
	if got := ConnectTimeout(); got != 30*time.Second {
		t.Fatalf("expected non-positive connect timeout to fall back to 30s, got %s", got)
	}
}

func TestSplitWebsocketClientLivenessConfig(t *testing.T) {
	viper.Reset()
	t.Cleanup(func() {
		viper.Reset()
	})

	if got := RealtimeWebsocketClientPingInterval(); got != 25*time.Second {
		t.Fatalf("expected unset realtime client ping interval to use default 25s, got %s", got)
	}
	if got := RealtimeWebsocketClientPongMissTimeout(); got != 0 {
		t.Fatalf("expected unset realtime client pong miss timeout to default disabled, got %s", got)
	}
	if got := RealtimeWebsocketClientInboundActivityTimeout(); got != 0 {
		t.Fatalf("expected unset realtime client inbound activity timeout to default disabled, got %s", got)
	}
	if got := ResponsesWebsocketClientPingInterval(); got != 25*time.Second {
		t.Fatalf("expected unset responses client ping interval to use default 25s, got %s", got)
	}
	if got := ResponsesWebsocketClientPongMissTimeout(); got != 10*time.Second {
		t.Fatalf("expected unset responses client pong miss timeout to default to 10s, got %s", got)
	}
	if got := ResponsesWebsocketClientInboundActivityTimeout(); got != time.Minute {
		t.Fatalf("expected unset responses client inbound activity timeout to default to 1m, got %s", got)
	}

	defaultConfig()
	if got := ResponsesWebsocketClientInboundActivityTimeout(); got != time.Minute {
		t.Fatalf("expected configured responses client inbound activity timeout default 1m, got %s", got)
	}

	viper.Set("responses_websocket_client_inbound_activity_timeout_ms", 0)
	if got := ResponsesWebsocketClientInboundActivityTimeout(); got != 0 {
		t.Fatalf("expected explicit zero responses client inbound activity timeout to disable watchdog, got %s", got)
	}

	viper.Set("responses_websocket_client_inbound_activity_timeout_ms", 1500)
	if got := ResponsesWebsocketClientInboundActivityTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("expected explicit responses client inbound activity timeout to apply, got %s", got)
	}

	if got := ResponsesWSActiveTurnTimeout(); got != 2*time.Minute {
		t.Fatalf("expected responses active turn timeout to default to 2m, got %s", got)
	}
	viper.Set("responses_ws.active_turn_timeout_ms", 1500)
	if got := ResponsesWSActiveTurnTimeout(); got != 1500*time.Millisecond {
		t.Fatalf("expected explicit responses active turn timeout to apply, got %s", got)
	}
	viper.Set("responses_ws.active_turn_timeout_ms", 0)
	if got := ResponsesWSActiveTurnTimeout(); got != 0 {
		t.Fatalf("expected zero responses active turn timeout to disable watchdog, got %s", got)
	}
	viper.Reset()
	viper.Set("responses_ws.client_pong_timeout_ms", 1500)
	if got := ResponsesWebsocketClientInboundActivityTimeout(); got != time.Minute {
		t.Fatalf("expected legacy responses_ws.client_pong_timeout_ms to be ignored, got %s", got)
	}

	viper.Set("realtime_websocket_client_ping_interval_ms", 1250)
	viper.Set("realtime_websocket_client_pong_miss_timeout_ms", 2500)
	viper.Set("realtime_websocket_client_inbound_activity_timeout_ms", 5000)
	if got := RealtimeWebsocketClientPingInterval(); got != 1250*time.Millisecond {
		t.Fatalf("expected realtime client ping interval override, got %s", got)
	}
	if got := RealtimeWebsocketClientPongMissTimeout(); got != 2500*time.Millisecond {
		t.Fatalf("expected realtime client pong miss timeout override, got %s", got)
	}
	if got := RealtimeWebsocketClientInboundActivityTimeout(); got != 5*time.Second {
		t.Fatalf("expected realtime client inbound activity timeout override, got %s", got)
	}
}

func TestRealtimeWebsocketFrameQueueByteBudgets(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	if got := RealtimeWebsocketClientFrameQueueMaxBytes(); got != 64<<20 {
		t.Fatalf("expected default client frame queue budget 64 MiB, got %d", got)
	}
	if got := RealtimeWebsocketPendingFrameQueueMaxBytes(); got != 64<<20 {
		t.Fatalf("expected default pending frame queue budget 64 MiB, got %d", got)
	}
	if got := RealtimeWebsocketProviderFrameQueueMaxBytes(); got != 64<<20 {
		t.Fatalf("expected default provider frame queue budget 64 MiB, got %d", got)
	}
	if got := RealtimeWebsocketAttachmentQueueMaxBytes(); got != 64<<20 {
		t.Fatalf("expected default attachment queue budget 64 MiB, got %d", got)
	}
	viper.Set("realtime.client_frame_queue_max_bytes", 1024)
	viper.Set("realtime.pending_frame_queue_max_bytes", 2048)
	viper.Set("realtime.provider_frame_queue_max_bytes", 4096)
	viper.Set("realtime.attachment_queue_max_bytes", 8192)
	if got := RealtimeWebsocketClientFrameQueueMaxBytes(); got != 1024 {
		t.Fatalf("expected configured client frame queue budget, got %d", got)
	}
	if got := RealtimeWebsocketPendingFrameQueueMaxBytes(); got != 2048 {
		t.Fatalf("expected configured pending frame queue budget, got %d", got)
	}
	if got := RealtimeWebsocketProviderFrameQueueMaxBytes(); got != 4096 {
		t.Fatalf("expected configured provider frame queue budget, got %d", got)
	}
	if got := RealtimeWebsocketAttachmentQueueMaxBytes(); got != 8192 {
		t.Fatalf("expected configured attachment queue budget, got %d", got)
	}
}

func TestResponsesWSOptionalTimeoutsCanBeDisabled(t *testing.T) {
	viper.Reset()
	t.Cleanup(viper.Reset)

	if got := ResponsesWSFirstFrameTimeout(); got != 30*time.Second {
		t.Fatalf("expected unset first-frame timeout to use 30s, got %s", got)
	}
	if got := ResponsesWSIdleTimeout(); got != 30*time.Minute {
		t.Fatalf("expected unset idle cleanup timeout to use 30m, got %s", got)
	}
	if got := ResponsesWSActiveTurnTimeout(); got != 2*time.Minute {
		t.Fatalf("expected unset active-turn timeout to use 2m, got %s", got)
	}
	if got := ResponsesWSMaxLifetime(); got != time.Hour {
		t.Fatalf("expected unset proxy max lifetime to use 1h, got %s", got)
	}

	for _, key := range []string{
		"responses_ws.first_frame_timeout_ms",
		"responses_ws.idle_timeout_ms",
		"responses_ws.active_turn_timeout_ms",
		"responses_ws.max_lifetime_ms",
	} {
		viper.Set(key, 0)
	}
	if got := ResponsesWSFirstFrameTimeout(); got != 0 {
		t.Fatalf("expected zero first-frame timeout to disable it, got %s", got)
	}
	if got := ResponsesWSIdleTimeout(); got != 0 {
		t.Fatalf("expected zero idle timeout to disable it, got %s", got)
	}
	if got := ResponsesWSActiveTurnTimeout(); got != 0 {
		t.Fatalf("expected zero active-turn timeout to disable it, got %s", got)
	}
	if got := ResponsesWSMaxLifetime(); got != 0 {
		t.Fatalf("expected zero max lifetime to disable it, got %s", got)
	}
}
