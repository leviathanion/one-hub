package relay

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

// 普通 HTTP 上下文没有长连接标记，也必须执行当前 SQL 凭据校验。
func TestFixI055_StoredHTTPChecksCurrentPrincipalWithoutLongLivedMarker(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if err := model.DB.Model(&model.Token{}).Where("id = ?", owner.TokenID).Update("status", config.TokenStatusDisabled).Error; err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(method, "/v1/responses/"+owner.ResponseID, nil)
			ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
			ctx.Set("id", owner.UserID)
			ctx.Set("token_id", owner.TokenID)
			StoredResponses(ctx)
			if recorder.Code != http.StatusUnauthorized || atomic.LoadInt32(calls) != 0 {
				t.Fatalf("撤销凭据必须在上游前拒绝: status=%d calls=%d body=%s", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
			}
			got, err := model.GetResponseOwner(nil, owner.ResponseID, owner.UserID)
			if err != nil || got.State != model.ResponseOwnerStateActive {
				t.Fatalf("拒绝后 owner 必须保持 active: owner=%+v err=%v", got, err)
			}
		})
	}
}

func TestFixI055_InputTokensChecksCurrentPrincipalWithoutLongLivedMarker(t *testing.T) {
	for _, previous := range []bool{false, true} {
		name := "new"
		if previous {
			name = "previous_response_id"
		}
		t.Run(name, func(t *testing.T) {
			owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			if err := model.DB.Model(&model.Token{}).Where("id = ?", owner.TokenID).Update("status", config.TokenStatusDisabled).Error; err != nil {
				t.Fatal(err)
			}
			body := `{"model":"gpt-test"}`
			if previous {
				body = `{"model":"gpt-test","previous_response_id":"` + owner.ResponseID + `"}`
			}
			ctx, _ := responsesOwnerTestContext(owner.UserID, owner.TokenID)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/input_tokens", strings.NewReader(body))
			relayer := NewRelayResponses(ctx)
			if err := relayer.setRequest(); err != nil {
				t.Fatal(err)
			}
			err := relayer.setProvider("gpt-test")
			var apiErr *types.OpenAIErrorWithStatusCode
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized || atomic.LoadInt32(calls) != 0 {
				t.Fatalf("无标记 input_tokens 必须执行 SQL 撤销校验: err=%v calls=%d", err, atomic.LoadInt32(calls))
			}
		})
	}
}
