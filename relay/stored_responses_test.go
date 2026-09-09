package relay

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"
	"one-api/types"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func seedStoredResponsesPrincipal(t *testing.T, userID, tokenID int) {
	t.Helper()
	if err := model.DB.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
		t.Fatal(err)
	}
	user := &model.User{Id: userID, Username: fmt.Sprintf("stored-user-%d", userID), AccessToken: fmt.Sprintf("stored-access-%d", userID), AffCode: fmt.Sprintf("stored-aff-%d", userID), Status: config.UserStatusEnabled}
	if err := model.DB.Create(user).Error; err != nil {
		t.Fatal(err)
	}
	token := &model.Token{Id: tokenID, UserId: userID, Key: fmt.Sprintf("stored-token-%d", tokenID), Status: config.TokenStatusEnabled, ExpiredTime: -1}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
}

func setupStoredResponsesHandlerTest(t *testing.T, handler http.HandlerFunc) (*model.ResponseOwner, string, *int32) {
	t.Helper()
	setupRelayTestDB(t, &model.Channel{}, &model.ResponseOwner{})
	seedStoredResponsesPrincipal(t, 101, 201)
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	originalHTTPClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })
	originalLogConsume := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = originalLogConsume })

	proxy := ""
	channel := &model.Channel{
		Id:      301,
		Type:    config.ChannelTypeOpenAI,
		Key:     "sk-test",
		Status:  config.ChannelStatusEnabled,
		Name:    "stored-responses-test",
		BaseURL: &server.URL,
		Proxy:   &proxy,
		Other:   `{"responses_stored_lifecycle":true}`,
	}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	owner, err := model.NewResponseOwner("resp_lifecycle", 101, 201, channel.Id, time.Now())
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	return owner, server.URL, &calls
}

func TestStoredResponsesRetrieveUsesOwnerChannelAndPreservesStatus(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/responses/resp_lifecycle" || r.URL.RawQuery != "include=usage&stream=false" {
			t.Errorf("unexpected upstream request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream"}}`))
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle?include=usage&stream=false", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	if recorder.Code != http.StatusTeapot || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected exact upstream status through owner channel, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
}

func TestStoredResponsesLifecycleDeadlineReachesProviderRequest(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "retrieve", method: http.MethodGet, path: "/v1/responses/resp_lifecycle"},
		{name: "delete", method: http.MethodDelete, path: "/v1/responses/resp_lifecycle"},
		{name: "input items", method: http.MethodGet, path: "/v1/responses/resp_lifecycle/input_items"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var deadlineSeen atomic.Bool
			owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream failed"}}`)
			})
			originalHTTPClient := requester.HTTPClient
			baseTransport, ok := originalHTTPClient.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("server transport=%T, want *http.Transport", originalHTTPClient.Transport)
			}
			capturingTransport := baseTransport.Clone()
			capturingTransport.DisableKeepAlives = true
			capturingTransport.Proxy = func(req *http.Request) (*url.URL, error) {
				ctx := req.Context()
				deadline, hasDeadline := ctx.Deadline()
				remaining := time.Until(deadline)
				deadlineSeen.Store(hasDeadline && remaining > 0 && remaining <= responsesLifecycleIOTimeout)
				return nil, nil
			}
			capturingClient := *originalHTTPClient
			capturingClient.Transport = capturingTransport
			requester.HTTPClient = &capturingClient
			t.Cleanup(func() { requester.HTTPClient = originalHTTPClient })

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(test.method, test.path, nil)
			ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
			ctx.Set("id", owner.UserID)
			ctx.Set("token_id", owner.TokenID)
			StoredResponses(ctx)

			if atomic.LoadInt32(calls) != 1 || !deadlineSeen.Load() {
				t.Fatalf("stored lifecycle request did not inherit its bounded context: calls=%d deadline=%v", atomic.LoadInt32(calls), deadlineSeen.Load())
			}
		})
	}
}

func TestStoredResponsesUsesDisabledOrSoftDeletedOwnerChannel(t *testing.T) {
	for _, test := range []struct {
		name       string
		disable    bool
		softDelete bool
	}{
		{name: "disabled", disable: true},
		{name: "soft-deleted", softDelete: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_lifecycle","status":"completed"}`))
			})
			if test.disable {
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", owner.ChannelID).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatalf("disable owner channel: %v", err)
				}
			}
			if test.softDelete {
				if err := model.DB.Delete(&model.Channel{}, owner.ChannelID).Error; err != nil {
					t.Fatalf("soft delete owner channel: %v", err)
				}
			}

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/"+owner.ResponseID, nil)
			ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
			ctx.Set("id", owner.UserID)
			ctx.Set("token_id", owner.TokenID)
			StoredResponses(ctx)

			if recorder.Code != http.StatusOK || atomic.LoadInt32(calls) != 1 {
				t.Fatalf("durable owner lifecycle must ignore route status/deletion: status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
			}
		})
	}
}

func TestStoredResponsesProviderFailureUsesSharedHealthProjection(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid provider credential","type":"authentication_error"}}`))
	})

	originalProcess := processChannelRelayErrorFunc
	processed := make(chan int, 1)
	processChannelRelayErrorFunc = func(_ context.Context, channelID int, _ string, apiErr *types.OpenAIErrorWithStatusCode, _ int) {
		if apiErr != nil && apiErr.StatusCode == http.StatusUnauthorized {
			processed <- channelID
		}
	}
	t.Cleanup(func() { processChannelRelayErrorFunc = originalProcess })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/"+owner.ResponseID, nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	if atomic.LoadInt32(calls) != 1 {
		t.Fatalf("stored response provider calls=%d, want 1", atomic.LoadInt32(calls))
	}
	select {
	case channelID := <-processed:
		if channelID != owner.ChannelID {
			t.Fatalf("health projection channel=%d, want %d", channelID, owner.ChannelID)
		}
	case <-time.After(time.Second):
		t.Fatal("stored response provider failure skipped shared health projection")
	}
}

func TestStoredResponsesRetrievePreservesConditionalNotModified(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `"response-v1"` {
			t.Errorf("If-None-Match=%q", got)
		}
		w.Header().Set("ETag", `"response-v1"`)
		w.Header().Set("Last-Modified", "Wed, 19 Aug 2026 00:00:00 GMT")
		w.WriteHeader(http.StatusNotModified)
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle", nil)
	ctx.Request.Header.Set("If-None-Match", `"response-v1"`)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	if recorder.Code != http.StatusNotModified || atomic.LoadInt32(calls) != 1 || recorder.Body.Len() != 0 {
		t.Fatalf("expected raw 304, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
	if recorder.Header().Get("ETag") != `"response-v1"` || recorder.Header().Get("Last-Modified") == "" {
		t.Fatalf("conditional headers were lost: %#v", recorder.Header())
	}
}

func TestStoredResponsesPreservesExactWireRedirectWithoutFollowing(t *testing.T) {
	setupRelayTestDB(t, &model.Channel{}, &model.ResponseOwner{})
	seedStoredResponsesPrincipal(t, 101, 201)
	originalHTTPClient := requester.HTTPClient
	originalLogConsume := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	var calls int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Location", "https://redirect.example/v1/responses/target")
		w.Header().Set("X-Request-Id", "req-redirect")
		w.Header().Set("Etag", `"redirect-v1"`)
		w.Header().Set("Last-Modified", "Sun, 23 Aug 2026 00:00:00 GMT")
		w.Header().Set("Set-Cookie", "provider_session=secret")
		w.Header().Set("Openai-Organization", "org-secret")
		w.WriteHeader(http.StatusTemporaryRedirect)
		_, _ = io.WriteString(w, "redirect body\n")
	}))
	t.Cleanup(server.Close)
	baseTransport, ok := server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("TLS server transport=%T, want *http.Transport", server.Client().Transport)
	}
	transport := baseTransport.Clone()
	serverAddress := server.Listener.Addr().String()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, serverAddress)
	}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	} else {
		transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	}
	transport.TLSClientConfig.InsecureSkipVerify = true
	transport.ForceAttemptHTTP2 = false
	requester.HTTPClient = &http.Client{Transport: transport}
	t.Cleanup(func() {
		requester.HTTPClient = originalHTTPClient
		config.LogConsumeEnabled = originalLogConsume
	})

	proxy := ""
	channel := &model.Channel{
		Id:     302,
		Type:   config.ChannelTypeOpenAI,
		Key:    "sk-test",
		Status: config.ChannelStatusEnabled,
		Name:   "stored-responses-exact-wire",
		Proxy:  &proxy,
		Other:  `{"responses_stored_lifecycle":true}`,
	}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatalf("create channel: %v", err)
	}
	owner, err := model.NewResponseOwner("resp_redirect", 101, 201, channel.Id, time.Now())
	if err != nil {
		t.Fatalf("new owner: %v", err)
	}
	if err := model.CreateResponseOwner(nil, owner); err != nil {
		t.Fatalf("create owner: %v", err)
	}

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "retrieve", method: http.MethodGet, path: "/v1/responses/resp_redirect"},
		{name: "delete", method: http.MethodDelete, path: "/v1/responses/resp_redirect"},
		{name: "input items", method: http.MethodGet, path: "/v1/responses/resp_redirect/input_items"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(test.method, test.path, nil)
			ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
			ctx.Set("id", owner.UserID)
			ctx.Set("token_id", owner.TokenID)

			StoredResponses(ctx)

			if recorder.Code != http.StatusTemporaryRedirect || recorder.Body.String() != "redirect body\n" {
				t.Fatalf("exact redirect changed: status=%d body=%q", recorder.Code, recorder.Body.String())
			}
			for name, want := range map[string]string{
				"Content-Type": "text/plain; charset=utf-8",
				"Location":     "https://redirect.example/v1/responses/target",
				"X-Request-Id": "req-redirect",
			} {
				if got := recorder.Header().Get(name); got != want {
					t.Fatalf("%s=%q, want %q in %#v", name, got, want, recorder.Header())
				}
			}
			for _, name := range []string{"Set-Cookie", "Openai-Organization"} {
				if got := recorder.Header().Get(name); got != "" {
					t.Fatalf("unsafe redirect header %s leaked as %q", name, got)
				}
			}
			if test.method == http.MethodGet {
				if recorder.Header().Get("Etag") != `"redirect-v1"` || recorder.Header().Get("Last-Modified") == "" {
					t.Fatalf("safe conditional headers were lost: %#v", recorder.Header())
				}
			} else if recorder.Header().Get("Etag") != "" || recorder.Header().Get("Last-Modified") != "" {
				t.Fatalf("delete exposed read-only conditional headers: %#v", recorder.Header())
			}
		})
	}

	if got := atomic.LoadInt32(&calls); got != int32(len(tests)) {
		t.Fatalf("redirect target was followed or an operation was skipped: calls=%d want=%d", got, len(tests))
	}
	storedOwner, err := model.GetResponseOwner(context.Background(), owner.ResponseID, owner.UserID)
	if err != nil || storedOwner.State != model.ResponseOwnerStateActive {
		t.Fatalf("redirected delete must not tombstone the owner: owner=%+v err=%v", storedOwner, err)
	}
}

func TestStoredResponsesRetrieveAppliesProviderResponseHeaderPolicy(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_lifecycle")
		w.Header().Set("Retry-After", "2")
		w.Header().Set("ETag", `"response-v1"`)
		w.Header().Set("Set-Cookie", "provider_session=secret")
		w.Header().Set("OpenAI-Organization", "org_shared")
		w.Header().Set("X-Ratelimit-Remaining-Tokens", "42")
		w.Header().Set("X-Provider-Debug", "private")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	if recorder.Code != http.StatusTooManyRequests || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected upstream response, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
	for name, want := range map[string]string{
		"Content-Type": "application/json; charset=utf-8",
		"X-Request-Id": "req_lifecycle",
		"Retry-After":  "2",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Fatalf("expected %s=%q, got %q in %#v", name, want, got, recorder.Header())
		}
	}
	for _, name := range []string{"Etag", "Set-Cookie", "OpenAI-Organization", "X-Ratelimit-Remaining-Tokens", "X-Provider-Debug"} {
		if got := recorder.Header().Get(name); got != "" {
			t.Fatalf("expected %s to be filtered, got %q in %#v", name, got, recorder.Header())
		}
	}
}

func TestStoredResponsesRetrieveRejectsRecoveryStreamBeforeProviderWork(t *testing.T) {
	tests := []string{
		"stream=true",
		"stream=TRUE",
		"stream=1",
		"stream=yes",
		"stream=",
		"starting_after=7",
	}
	for _, rawQuery := range tests {
		t.Run(rawQuery, func(t *testing.T) {
			owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle?"+rawQuery, nil)
			ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
			ctx.Set("id", owner.UserID)
			ctx.Set("token_id", owner.TokenID)

			StoredResponses(ctx)

			if recorder.Code != http.StatusBadRequest || atomic.LoadInt32(calls) != 0 {
				t.Fatalf("expected local capability rejection before provider work, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), unsupportedCapabilityCode) {
				t.Fatalf("expected unsupported capability error, body=%q", recorder.Body.String())
			}
		})
	}
}

func TestStoredResponsesInputItemsPreservesPaginationQuery(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/responses/resp_lifecycle/input_items" || r.URL.RawQuery != "limit=7&after=item_1" {
			t.Errorf("unexpected upstream request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle/input_items?limit=7&after=item_1", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)

	StoredResponses(ctx)

	if recorder.Code != http.StatusOK || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected input-items pagination to reach the owner channel, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
}

func TestStoredResponsesCrossUserIsUniform404WithoutUpstreamCall(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_lifecycle", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	seedStoredResponsesPrincipal(t, owner.UserID+1, owner.TokenID+1)
	ctx.Set("id", owner.UserID+1)
	ctx.Set("token_id", owner.TokenID+1)
	StoredResponses(ctx)

	if recorder.Code != http.StatusNotFound || atomic.LoadInt32(calls) != 0 {
		t.Fatalf("expected local uniform 404, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
}

func TestStoredResponsesDeleteTombstonesBeforeReply(t *testing.T) {
	owner, _, _ := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_lifecycle","deleted":true}`))
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_lifecycle", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	got, err := model.GetResponseOwner(ctx.Request.Context(), owner.ResponseID, owner.UserID)
	if recorder.Code != http.StatusOK || err != nil || got.State != model.ResponseOwnerStateDeleted {
		t.Fatalf("expected delete tombstone before success, status=%d owner=%+v err=%v", recorder.Code, got, err)
	}
}

func TestStoredResponsesDeleteFailureKeepsActiveOwner(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("expected DELETE, got %s", r.Method)
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"delete failed"}}`))
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_lifecycle", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	got, err := model.GetResponseOwner(ctx.Request.Context(), owner.ResponseID, owner.UserID)
	if recorder.Code != http.StatusBadGateway || atomic.LoadInt32(calls) != 1 || err != nil || got.State != model.ResponseOwnerStateActive {
		t.Fatalf("expected failed upstream delete to preserve active owner, status=%d calls=%d owner=%+v err=%v", recorder.Code, atomic.LoadInt32(calls), got, err)
	}
}

func TestStoredResponsesDeleteKeepsCommittedProviderSuccessWhenTombstoneFails(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) {
		model.DB = nil
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_lifecycle","deleted":true}`))
	})
	ownerDB := model.DB
	t.Cleanup(func() { model.DB = ownerDB })

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/v1/responses/resp_lifecycle", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: owner.ResponseID}}
	ctx.Set("id", owner.UserID)
	ctx.Set("token_id", owner.TokenID)
	StoredResponses(ctx)

	model.DB = ownerDB
	got, err := model.GetResponseOwner(context.Background(), owner.ResponseID, owner.UserID)
	if recorder.Code != http.StatusOK || recorder.Body.String() != `{"id":"resp_lifecycle","deleted":true}` || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("expected committed provider delete response, status=%d calls=%d body=%q", recorder.Code, atomic.LoadInt32(calls), recorder.Body.String())
	}
	if err != nil || got.State != model.ResponseOwnerStateActive {
		t.Fatalf("expected failed tombstone to leave an observable stale owner for operator recovery, owner=%+v err=%v", got, err)
	}
}

func TestStoredResponsesOwnerStoreFailureReturns503BeforeProviderWork(t *testing.T) {
	setupStoredResponsesHandlerTest(t, func(http.ResponseWriter, *http.Request) {
		t.Error("owner store failure reached provider")
	})
	if err := model.DB.Callback().Query().Before("gorm:query").Register("stored:owner_read_failure", func(tx *gorm.DB) {
		if tx.Statement.Table == "response_owners" {
			tx.AddError(errors.New("injected owner store failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer model.DB.Callback().Query().Remove("stored:owner_read_failure")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/resp_unknown", nil)
	ctx.Params = gin.Params{{Key: "response_id", Value: "resp_unknown"}}
	ctx.Set("id", 101)
	ctx.Set("token_id", 201)
	StoredResponses(ctx)

	if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "responses_owner_store_unavailable") {
		t.Fatalf("expected owner store failure to return a local 503, status=%d body=%q", recorder.Code, recorder.Body.String())
	}
}
