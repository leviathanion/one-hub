package wire

import (
	"errors"
	"strings"
	"testing"
)

func TestChatAccumulatedToolIndexMustMatchHistoryPosition(t *testing.T) {
	base := `{"messages":[{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}}]}]}`
	indexed := strings.ReplaceAll(strings.ReplaceAll(base, `"id":"a"`, `"index":0,"id":"a"`), `"id":"b"`, `"index":1,"id":"b"`)
	assertSameJSON(t, mustRequest(t, DialectChat, base).Body, mustRequest(t, DialectChat, indexed).Body)
	for _, value := range []string{"0", "-1", "null", `"1"`, "2", "1.5"} {
		bad := strings.Replace(indexed, `"index":1`, `"index":`+value, 1)
		_, err := ConvertRequest(DialectChat, []byte(bad), "model")
		var failure *CapabilityError
		if !errors.As(err, &failure) || failure.Param != "messages[0].tool_calls[1].index" {
			t.Fatalf("inconsistent index accepted: %s, %v", value, err)
		}
	}
}
