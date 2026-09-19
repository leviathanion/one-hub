package wire

import "testing"

func TestCodexClientRecognition(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"codex_cli_rs/0.144.1", true},
		{"codex-tui/0.154.0 (Linux; x64)", true},
		{"Codex Desktop/0.146.0-alpha.3", true},
		{"wrapper/1 codex_vscode_copilot/1.0", true},
		{"wrapper/1 codex-cli/1.0", true},
		{"wrapper/1 Codex Desktop/1.0", true},
		{"cccc/0.141.0 (Mac OS 14.6.1; arm64) Apple_Terminal/453 (codex-tui; 0.141.0)", true},
		{"cccc/1.0 (Mac OS; arm64) (Codex Desktop; 2026.9)", true},
		{"curl/8", false},
		{"pi (linux 6.12; x64)", false},
		{"codexcanary/1", false},
		{"evil-codex_cli_rs/1", false},
		{"my-codex-client/1", false},
		{"wrapper/1 (not-codex-tui; 1)", false},
		{"codex", false},
	} {
		if got := isCodexUserAgent(tc.value); got != tc.want {
			t.Errorf("UA %q: got %v want %v", tc.value, got, tc.want)
		}
	}
	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"CODEX_VSCODE", true}, {"Codex Desktop", true}, {"codex_atlas", true},
		{"codex_exec", true}, {"codex_sdk_ts", true}, {"codex", false}, {"codex-unknown", false}, {"my-codex", false},
	} {
		if got := isCodexClientName(tc.value); got != tc.want {
			t.Errorf("originator %q: got %v want %v", tc.value, got, tc.want)
		}
	}
}
