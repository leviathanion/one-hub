package channel

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func notificationResponse(body string) *http.Response {
	return &http.Response{Body: io.NopCloser(strings.NewReader(body))}
}

func TestNotificationACKDecodersRejectMalformedAndProviderFailure(t *testing.T) {
	tests := []struct {
		name   string
		decode func(*http.Response) error
		ok     string
		failed string
	}{
		{name: "dingtalk", decode: decodeDingTalkACK, ok: `{"errcode":0,"errmsg":"ok"}`, failed: `{"errcode":310000,"errmsg":"invalid"}`},
		{name: "lark", decode: decodeLarkACK, ok: `{"code":0,"msg":"ok"}`, failed: `{"code":19001,"msg":"invalid"}`},
		{name: "telegram", decode: decodeTelegramACK, ok: `{"ok":true}`, failed: `{"ok":false,"description":"invalid"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.decode(notificationResponse(test.ok)); err != nil {
				t.Fatalf("valid acknowledgement: %v", err)
			}
			for _, body := range []string{"", `{"broken":`, test.failed, test.ok + `{}`} {
				if err := test.decode(notificationResponse(body)); err == nil {
					t.Fatalf("accepted invalid acknowledgement %q", body)
				}
			}
		})
	}
}

func TestNotificationACKDecoderBoundsBody(t *testing.T) {
	body := strings.Repeat(" ", int(maxNotificationACKBytes)+1)
	if err := decodeDingTalkACK(notificationResponse(body)); err == nil {
		t.Fatal("oversized acknowledgement was accepted")
	}
}
