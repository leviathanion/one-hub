package providerresponse

import "testing"

func TestI017SSEPayloadContainsErrorDoesNotLetMalformedTypeHideError(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventName string
		payload   string
		want      bool
	}{
		{
			name:    "object type with nonnull error",
			payload: `{"type":{},"error":{"message":"failed"},"future":{"kept":true}}`,
			want:    true,
		},
		{
			name:    "array type with nonnull error",
			payload: `{"type":[],"error":{"message":"failed"}}`,
			want:    true,
		},
		{
			name:    "error type with null error",
			payload: `{"type":"error","error":null}`,
			want:    true,
		},
		{
			name:    "done type with null error",
			payload: `{"type":"speech.audio.done","error":null}`,
			want:    false,
		},
		{
			name:      "event name remains authoritative",
			eventName: "error",
			payload:   `{"type":{},"future":{"kept":true}}`,
			want:      true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := SSEPayloadContainsError(test.eventName, []byte(test.payload)); got != test.want {
				t.Fatalf("SSEPayloadContainsError(%q, %s)=%v want %v", test.eventName, test.payload, got, test.want)
			}
		})
	}
}
