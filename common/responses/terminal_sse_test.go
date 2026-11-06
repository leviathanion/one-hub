package responses

import "testing"

func TestTerminalSSEClassificationPreservesTypeFieldIdentity(t *testing.T) {
	for _, test := range []struct {
		event string
		want  bool
	}{
		{"data: {\"type\":\"response.completed\",\"TYPE\":\"response.output_text.delta\"}\n\n", true},
		{"data: {\"type\":\"response.output_text.delta\",\"TYPE\":\"response.completed\"}\n\n", false},
		{"data: {\"TYPE\":\"response.completed\"}\n\n", false},
		{"event: response.completed\ndata: {\"type\":\"future.event\"}\n\n", false},
		{"data: {\r\ndata: \"type\":\"response.incomplete\"}\r\n\r\n", true},
		{"data: {\"type\":\"response.failed\"}\r\r", true},
		{"data: {\"type\":\"error\"}\n\n", true},
		{"data: [DONE]\n\n", true},
	} {
		t.Run(test.event, func(t *testing.T) {
			if got := IsTerminalSSEEvent([]byte(test.event)); got != test.want {
				t.Fatalf("terminal=%v, want %v", got, test.want)
			}
		})
	}
}
