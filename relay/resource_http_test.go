package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/requester"
	"one-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func setupResourceHTTPTest(t *testing.T, handler http.HandlerFunc) (*model.Channel, *atomic.Int32) {
	t.Helper()
	setupRelayTestDB(t, &model.Channel{}, &model.ResourceOwner{}, &model.ResponseOwner{}, &model.PublicationVersion{})
	originalGroups := model.GlobalUserGroupRatio
	model.GlobalUserGroupRatio = &model.UserGroupRatio{}
	t.Cleanup(func() { model.GlobalUserGroupRatio = originalGroups })
	enabled := true
	if err := model.DB.Create(&model.UserGroup{Symbol: "default", Name: "Default", Ratio: 1, Enable: &enabled}).Error; err != nil {
		t.Fatal(err)
	}
	if err := model.EnsurePublicationVersionRows(model.DB); err != nil {
		t.Fatal(err)
	}
	if err := model.GlobalUserGroupRatio.Load(); err != nil {
		t.Fatal(err)
	}
	seedStoredResponsesPrincipal(t, 101, 201)
	if err := model.DB.Model(&model.User{}).Where("id = ?", 101).Update("group", "default").Error; err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int32{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); handler(w, r) }))
	t.Cleanup(server.Close)
	oldClient := requester.HTTPClient
	requester.HTTPClient = server.Client()
	t.Cleanup(func() { requester.HTTPClient = oldClient })
	oldLog := config.LogConsumeEnabled
	config.LogConsumeEnabled = false
	t.Cleanup(func() { config.LogConsumeEnabled = oldLog })
	proxy := ""
	channel := &model.Channel{Id: 301, Type: config.ChannelTypeOpenAI, Key: "provider-secret-resource", Status: config.ChannelStatusEnabled, Name: "resource-test", BaseURL: &server.URL, Proxy: &proxy, Group: "default"}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	setting := model.TokenSetting{ResourceChannelID: 301}
	// A model-limited token still manages resources without an artificial model.
	setting.Limits.LimitModelSetting.Enabled = true
	setting.Limits.LimitModelSetting.Models = []string{"gpt-5"}
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 201).Update("setting", datatypes.NewJSONType(setting)).Error; err != nil {
		t.Fatal(err)
	}
	return channel, calls
}
func resourceHTTPContext(method, path string, body io.Reader) (*gin.Context, *httptest.ResponseRecorder) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(method, path, body)
	enableResponsesTestDeadline(ctx)
	ctx.Set("id", 101)
	ctx.Set("token_id", 201)
	return ctx, recorder
}
func seedResourceHTTPOwner(t *testing.T, channel *model.Channel, kind, id string, userID int) *model.ResourceOwner {
	t.Helper()
	namespace, scope, err := model.ConservativeResponseOwnerIdentity(channel)
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := model.ReserveResourceOwner(context.Background(), model.ResourceOwnerReservation{Kind: kind, UserID: userID, TokenID: 201, ChannelID: channel.Id, ProviderNamespace: namespace, ProviderScope: scope, SubmitDeadline: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := model.BindResourceOwner(context.Background(), reserved.ID, userID, id)
	if err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestResourceHTTPFileCreatePreservesMultipartAndCommitsOwner(t *testing.T) {
	raw := "--boundary\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nfuture-purpose\r\n--boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"x\"\r\n\r\nabc\x00xyz\r\n--boundary--\r\n"
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if string(body) != raw || r.Header.Get("Content-Type") != "multipart/form-data; boundary=boundary" || r.Header.Get("Authorization") != "Bearer provider-secret-resource" || r.URL.RawQuery != "future=1" {
			t.Errorf("wire changed: %q %v %s", body, r.Header, r.URL)
		}
		if r.Header.Get("X-Future") != "keep" {
			t.Error("future header lost")
		}
		w.Header().Set("Location", "/v1/files/file_new")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "provider-secret-resource")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{ "id": "file_new", "future": {"unknown":true} }`)
	})
	ctx, recorder := resourceHTTPContext("POST", "/v1/files?future=1", strings.NewReader(raw))
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary=boundary")
	ctx.Request.Header.Set("Authorization", "Bearer client-secret")
	ctx.Request.Header.Set("X-Future", "keep")
	ResourceRelay(ctx)
	if recorder.Code != 201 || calls.Load() != 1 || recorder.Body.String() != `{ "id": "file_new", "future": {"unknown":true} }` {
		t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
	owner, err := model.GetResourceOwner(context.Background(), "file", "file_new", 101)
	if err != nil || owner.ChannelID != 301 {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	if recorder.Header().Get("Location") != "/v1/files/file_new" || recorder.Header().Get("X-Request-Id") != "" {
		t.Fatalf("headers=%v", recorder.Header())
	}
}

func TestResourceHTTPDeleteObservationDoesNotBlockSubsequentAccess(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "future=opaque" {
			t.Error("query changed")
		}
		if r.Method == http.MethodDelete {
			io.WriteString(w, `{"id":"file_old","deleted":true}`)
			return
		}
		w.WriteHeader(http.StatusGone)
		io.WriteString(w, `{"error":{"message":"upstream decides"}}`)
	})
	seedResourceHTTPOwner(t, channel, "file", "file_old", 101)
	for _, method := range []string{"DELETE", "GET", "DELETE"} {
		ctx, recorder := resourceHTTPContext(method, "/v1/files/file_old?future=opaque", nil)
		ResourceRelay(ctx)
		want := 200
		if method == "GET" {
			want = 410
		}
		if recorder.Code != want {
			t.Fatalf("%s response=%d %s", method, recorder.Code, recorder.Body)
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestResourceHTTPRejectsUnknownOwnerAndAccountListsBeforeUpstream(t *testing.T) {
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("unauthorized upstream work") })
	for _, path := range []string{"/v1/files", "/v1/files/file_foreign", "/v1/conversations", "/v1/conversations/conv_foreign/items", "/v1/files/file%2Fother", "/v1/files/../other"} {
		ctx, recorder := resourceHTTPContext("GET", path, nil)
		ResourceRelay(ctx)
		if recorder.Code < 400 {
			t.Fatalf("path=%s response=%d", path, recorder.Code)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("calls=%d", calls.Load())
	}
}

func TestResourceHTTPUploadCompleteBindsDerivedFile(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/uploads/upload_1/complete" {
			t.Errorf("request=%s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"id":"upload_1","file":{"id":"file_done","unknown":1},"status":"completed"}`)
	})
	parent := seedResourceHTTPOwner(t, channel, "upload", "upload_1", 101)
	ctx, recorder := resourceHTTPContext("POST", "/v1/uploads/upload_1/complete", strings.NewReader(`{"part_ids":["part_1"],"unknown":true}`))
	ResourceRelay(ctx)
	if recorder.Code != 200 || calls.Load() != 1 {
		t.Fatalf("response=%d %s", recorder.Code, recorder.Body)
	}
	derived, err := model.GetResourceOwner(context.Background(), "file", "file_done", 101)
	if err != nil || derived.ParentID == nil || *derived.ParentID != parent.ID {
		t.Fatalf("derived=%+v err=%v", derived, err)
	}
}

func TestResourceHTTPConversationReferencesChooseOwnerChannel(t *testing.T) {
	raw := `{"items":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_input"}]},{"type":"tool_search_output","tools":[{"type":"function","name":"f","parameters":{"properties":{"file_id":{"type":"string"}}}}]}],"future":null}`
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != raw {
			t.Errorf("body changed: %s", body)
		}
		io.WriteString(w, `{"id":"conv_new"}`)
	})
	seedResourceHTTPOwner(t, channel, "file", "file_input", 101)
	// The referenced owner wins over a deliberately invalid default channel.
	if err := model.DB.Model(&model.Token{}).Where("id = ?", 201).Update("setting", datatypes.NewJSONType(model.TokenSetting{ResourceChannelID: 999})).Error; err != nil {
		t.Fatal(err)
	}
	ctx, recorder := resourceHTTPContext("POST", "/v1/conversations", strings.NewReader(raw))
	ResourceRelay(ctx)
	if recorder.Code != 200 || calls.Load() != 1 {
		t.Fatalf("response=%d %s", recorder.Code, recorder.Body)
	}
	owner, err := model.GetResourceOwner(context.Background(), "conversation", "conv_new", 101)
	if err != nil || owner.ChannelID != 301 {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
}

func TestResourceHTTPCreateBindConflictNeverDeliversIDOrRetries(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":"file_collision"}`) })
	seedStoredResponsesPrincipal(t, 102, 202)
	seedResourceHTTPOwner(t, channel, "file", "file_collision", 102)
	ctx, recorder := resourceHTTPContext("POST", "/v1/files", strings.NewReader("multipart bytes"))
	ResourceRelay(ctx)
	if recorder.Code != http.StatusConflict || strings.Contains(recorder.Body.String(), "file_collision") || calls.Load() != 1 {
		t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
	var reserved int64
	model.DB.Model(&model.ResourceOwner{}).Where("user_id = ? AND phase = ?", 101, model.ResourceOwnerReserved).Count(&reserved)
	if reserved != 0 {
		t.Fatalf("leaked reservations=%d", reserved)
	}
}

type resourceZeroReader struct{}

func (resourceZeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestResourceHTTPFilesStreamBeyondCommonBodyCacheLimit(t *testing.T) {
	const size = int64(65 << 20)
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		received, err := io.Copy(io.Discard, r.Body)
		if err != nil || received != size {
			t.Errorf("received=%d err=%v", received, err)
		}
		io.WriteString(w, `{"id":"file_large"}`)
	})
	ctx, recorder := resourceHTTPContext("POST", "/v1/files", io.LimitReader(resourceZeroReader{}, size))
	ctx.Request.ContentLength = size
	ctx.Request.Header.Set("Content-Type", "multipart/form-data; boundary=raw")
	ResourceRelay(ctx)
	if recorder.Code != 200 || calls.Load() != 1 {
		t.Fatalf("response=%d %s", recorder.Code, recorder.Body)
	}
}

func TestResourceHTTPTransferLimitsAndAmbiguousCreate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		size  int64
		want  int
		calls int32
	}{
		{"known request limit", "", resourceFileBytes + 1, 413, 0},
		{"successful response without identity", `{"future":true}`, 0, 502, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) })
			ctx, recorder := resourceHTTPContext("POST", "/v1/files", nil)
			ctx.Request.ContentLength = tc.size
			ResourceRelay(ctx)
			if recorder.Code != tc.want || calls.Load() != tc.calls {
				t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
			}
		})
	}
	reader := &resourceBoundedReadCloser{ReadCloser: io.NopCloser(strings.NewReader("abcd")), remaining: 3}
	content, err := io.ReadAll(reader)
	if string(content) != "abc" || err == nil {
		t.Fatalf("content=%q err=%v", content, err)
	}
}

func TestResourceHTTPMethodsAreExplicitAndDoNotConflateChildDelete(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/v1/files/x/content", true}, {"POST", "/v1/uploads/x/parts", true}, {"POST", "/v1/uploads/x/cancel", true},
		{"DELETE", "/v1/conversations/x/items/y", true}, {"POST", "/v1/conversations/x", true},
		{"PATCH", "/v1/files/x", false}, {"POST", "/v1/uploads/x/unknown", false}, {"GET", "/v1/files/x/", false},
	} {
		t.Run(fmt.Sprintf("%s %s", tc.method, tc.path), func(t *testing.T) {
			op, ok := parseResourceHTTPOperation(tc.method, tc.path)
			if ok != tc.want {
				t.Fatalf("ok=%v", ok)
			}
			if strings.Contains(tc.path, "/items/") && op.observeDelete {
				t.Fatal("child deletion would tombstone parent")
			}
		})
	}
}

func TestResourceHTTPParentItemsRejectCrossChannelReferences(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { t.Error("must not send cross-channel references") })
	seedResourceHTTPOwner(t, channel, "conversation", "conv_parent", 101)
	second := *channel
	second.Id = 302
	if err := model.DB.Create(&second).Error; err != nil {
		t.Fatal(err)
	}
	seedResourceHTTPOwner(t, &second, "file", "file_other_channel", 101)
	ctx, recorder := resourceHTTPContext("POST", "/v1/conversations/conv_parent/items", strings.NewReader(`{"items":[{"type":"message","role":"user","content":[{"type":"input_file","file_id":"file_other_channel"}]}]}`))
	ResourceRelay(ctx)
	if recorder.Code != 400 || calls.Load() != 0 {
		t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
}

func TestResourceHTTPPreservesAdminRawWildcardAndRechecksRole(t *testing.T) {
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/v1/files/admin_only/new_action" {
			t.Errorf("request=%s %s", r.Method, r.URL)
		}
		io.WriteString(w, `{"future":true}`)
	})
	if err := model.DB.Model(&model.User{}).Where("id = ?", 101).Update("role", config.RoleAdminUser).Error; err != nil {
		t.Fatal(err)
	}
	ctx, recorder := resourceHTTPContext("PATCH", "/v1/files/admin_only/new_action", strings.NewReader(`{}`))
	ctx.Set("specific_channel_id", 301)
	ResourceRelay(ctx)
	if recorder.Code != 200 || calls.Load() != 1 {
		t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
	if err := model.DB.Model(&model.User{}).Where("id = ?", 101).Update("role", config.RoleCommonUser).Error; err != nil {
		t.Fatal(err)
	}
	ctx, recorder = resourceHTTPContext("PATCH", "/v1/files/admin_only/new_action", strings.NewReader(`{}`))
	ctx.Set("specific_channel_id", 301)
	ctx.Set("role", config.RoleAdminUser)
	ResourceRelay(ctx)
	if recorder.Code != 403 || calls.Load() != 1 {
		t.Fatalf("revoked response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
}

func TestResourceHTTPFileContentIsUserScopedAcrossTokens(t *testing.T) {
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=2-4" || r.URL.RawQuery != "future=1" {
			t.Errorf("request=%s headers=%v", r.URL, r.Header)
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 2-4/8")
		w.WriteHeader(http.StatusPartialContent)
		w.Write([]byte{0, 1, 255})
	})
	seedResourceHTTPOwner(t, channel, "file", "file_binary", 101)
	seedStoredResponsesPrincipal(t, 102, 202)
	foreign, denied := resourceHTTPContext("GET", "/v1/files/file_binary/content", nil)
	foreign.Set("id", 102)
	foreign.Set("token_id", 202)
	ResourceRelay(foreign)
	if denied.Code != 404 || calls.Load() != 0 {
		t.Fatalf("foreign response=%d calls=%d", denied.Code, calls.Load())
	}
	// Another valid token of the same user can manage the previously created file.
	token := &model.Token{Id: 203, UserId: 101, Key: "resource-rotated-token", Status: config.TokenStatusEnabled, ExpiredTime: -1}
	if err := model.DB.Session(&gorm.Session{SkipHooks: true}).Create(token).Error; err != nil {
		t.Fatal(err)
	}
	ctx, recorder := resourceHTTPContext("GET", "/v1/files/file_binary/content?future=1", nil)
	ctx.Set("token_id", 203)
	ctx.Request.Header.Set("Range", "bytes=2-4")
	ResourceRelay(ctx)
	if recorder.Code != 206 || recorder.Header().Get("Content-Range") != "bytes 2-4/8" || recorder.Body.String() != string([]byte{0, 1, 255}) || calls.Load() != 1 {
		t.Fatalf("response=%d %q headers=%v calls=%d", recorder.Code, recorder.Body.String(), recorder.Header(), calls.Load())
	}
}

func TestResourceHTTPDeletionObservationDoesNotInferFromStatus(t *testing.T) {
	channel, _ := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"file_pending","future":"delete accepted"}`)
	})
	seedResourceHTTPOwner(t, channel, "file", "file_pending", 101)
	ctx, recorder := resourceHTTPContext("DELETE", "/v1/files/file_pending", nil)
	ResourceRelay(ctx)
	owner, err := model.GetResourceOwner(context.Background(), "file", "file_pending", 101)
	if recorder.Code != 200 || err != nil || owner.DeleteObservedAt != nil {
		t.Fatalf("response=%d owner=%+v err=%v", recorder.Code, owner, err)
	}
}

func TestResourceHTTPNewCreationRejectsDisabledOrDeletedChannelButOwnerReadsRemain(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprint(deleted), func(t *testing.T) {
			channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":"file_existing"}`) })
			seedResourceHTTPOwner(t, channel, "file", "file_existing", 101)
			if deleted {
				if err := model.DB.Delete(&model.Channel{}, channel.Id).Error; err != nil {
					t.Fatal(err)
				}
			} else {
				if err := model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", config.ChannelStatusManuallyDisabled).Error; err != nil {
					t.Fatal(err)
				}
			}
			ctx, recorder := resourceHTTPContext("POST", "/v1/files", strings.NewReader("multipart"))
			ResourceRelay(ctx)
			if recorder.Code != 403 || calls.Load() != 0 {
				t.Fatalf("create=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
			}
			ctx, recorder = resourceHTTPContext("GET", "/v1/files/file_existing", nil)
			ResourceRelay(ctx)
			if recorder.Code != 200 || calls.Load() != 1 {
				t.Fatalf("read=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
			}
		})
	}
}

func TestResourceHTTPLocationCannotExposeConflictingNewIdentity(t *testing.T) {
	_, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/v1/files/file_other")
		io.WriteString(w, `{"id":"file_created"}`)
	})
	ctx, recorder := resourceHTTPContext("POST", "/v1/files", strings.NewReader("multipart"))
	ResourceRelay(ctx)
	if recorder.Code != 502 || recorder.Header().Get("Location") != "" || strings.Contains(recorder.Body.String(), "file_other") || calls.Load() != 1 {
		t.Fatalf("response=%d %s headers=%v calls=%d", recorder.Code, recorder.Body, recorder.Header(), calls.Load())
	}
}

func TestResourceHTTPCompressedConversationKeepsWireAfterAuthorization(t *testing.T) {
	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	io.WriteString(writer, `{"items":[{"role":"user","content":[{"type":"input_file","file_id":"file_gzip"}]}]}`)
	writer.Close()
	wire := append([]byte(nil), encoded.Bytes()...)
	channel, calls := setupResourceHTTPTest(t, func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		if !bytes.Equal(received, wire) || r.Header.Get("Content-Encoding") != "gzip" {
			t.Error("compressed request was rebuilt")
		}
		io.WriteString(w, `{"id":"conv_gzip"}`)
	})
	seedResourceHTTPOwner(t, channel, "file", "file_gzip", 101)
	ctx, recorder := resourceHTTPContext("POST", "/v1/conversations", bytes.NewReader(wire))
	ctx.Request.Header.Set("Content-Encoding", "gzip")
	ResourceRelay(ctx)
	if recorder.Code != 200 || calls.Load() != 1 {
		t.Fatalf("response=%d %s calls=%d", recorder.Code, recorder.Body, calls.Load())
	}
}
