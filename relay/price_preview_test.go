package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPricePreviewRunsDraftRulesWithoutPricePublication(t *testing.T) {
	router := gin.New()
	router.POST("/preview", PreviewPriceRules)
	request := `{"price":{"model":"unsaved-preview","type":"tokens","input":2.5,"output":15,"extra_ratios":{"cached_tokens":0.1},"rate_rules":{"version":2,"schedule":{"rules":[{"id":"free-input","when":{},"multipliers":{"all":0,"extra_multipliers":{"cached_tokens":1}}}]}}},"facts":{"service_tier":"default"},"usage":{"prompt_tokens":100,"completion_tokens":10},"extra_tokens":{"cached_tokens":80}}`
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(request)))
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	var response struct {
		Success bool `json:"success"`
		Data    struct {
			Decision struct {
				FinalQuota int64
				Confirm    bool
			}
			Metadata map[string]any
		}
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.True(t, response.Data.Decision.Confirm)
	assert.Equal(t, int64(20), response.Data.Decision.FinalQuota)
	assert.Equal(t, float64(0), response.Data.Metadata["input_ratio"])
	bad := strings.Replace(request, `"prompt_tokens":100`, `"prompt_tokens":-1`, 1)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/preview", strings.NewReader(bad)))
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
}
