package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexOfficialStaticContract(t *testing.T) {
	officialFiles := []string{
		"responses.go",
		"responses_ws_upstream.go",
	}
	for _, name := range officialFiles {
		body := readSourceFile(t, name)
		for _, forbidden := range []string{
			"applyDefaultHeaders",
			"getRequestHeaderBag",
			"ensureStablePromptCacheKey",
			"adaptCodexCLI",
			"Conversation_id",
			"\"x-session-id\"",
			"\"session_id\"",
		} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s contains forbidden Codex Official source fragment %q", name, forbidden)
			}
		}
	}

	wireFiles, err := filepath.Glob(filepath.Join("wire", "*.go"))
	if err != nil {
		t.Fatalf("glob wire files: %v", err)
	}
	for _, name := range wireFiles {
		body := readSourceFile(t, name)
		for _, forbiddenImport := range []string{
			`"github.com/gin-gonic/gin"`,
			`"one-api/model"`,
			`"one-api/common/requester"`,
		} {
			if strings.Contains(body, forbiddenImport) {
				t.Fatalf("%s imports forbidden dependency %s", name, forbiddenImport)
			}
		}
	}

	base := readSourceFile(t, filepath.Join("..", "base", "interface.go"))
	if strings.Contains(base, "*types.OpenAIResponsesRequest") {
		t.Fatal("providers/base must expose raw commonresponses.Request for Responses, not typed OpenAIResponsesRequest")
	}
}

func readSourceFile(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}
