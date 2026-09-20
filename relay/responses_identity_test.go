package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"one-api/model"
	"one-api/types"
)

func TestStoredResponsesIdentityAliasesNeverReachProvider(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	for _, id := range []string{" " + owner.ResponseID + " ", strings.ToUpper(owner.ResponseID), owner.ResponseID + " "} {
		for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			path := "/v1/responses/" + url.PathEscape(id)
			if method == http.MethodPost {
				path += "/cancel"
			}
			c.Request = httptest.NewRequest(method, path, nil)
			c.Params = gin.Params{{Key: "response_id", Value: id}}
			c.Set("id", owner.UserID)
			c.Set("token_id", owner.TokenID)
			StoredResponses(c)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("alias %q method=%s status=%d body=%s", id, method, rec.Code, rec.Body.String())
			}
		}
	}
	if atomic.LoadInt32(calls) != 0 {
		t.Fatalf("alias authorization reached upstream: %d", atomic.LoadInt32(calls))
	}
}

func TestStoredResponsesPreservesGenuineOpaqueIDThroughTombstone(t *testing.T) {
	id := " resp-Opaque "
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/"+id {
			t.Errorf("provider ID changed: %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"deleted":true}`)
	})
	exact, err := model.NewResponseOwner(id, owner.UserID, owner.TokenID, owner.ChannelID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = model.CreateResponseOwner(context.Background(), exact); err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{http.MethodDelete, http.MethodGet, http.MethodDelete} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/v1/responses/"+url.PathEscape(id), nil)
		c.Params = gin.Params{{Key: "response_id", Value: id}}
		c.Set("id", owner.UserID)
		c.Set("token_id", owner.TokenID)
		StoredResponses(c)
		if rec.Code != 200 {
			t.Fatalf("opaque ID %q status=%d body=%s", id, rec.Code, rec.Body.String())
		}
	}
	got, err := model.GetResponseOwner(context.Background(), id, owner.UserID)
	if err != nil || got.ResponseID != id || got.State != model.ResponseOwnerStateDeleted {
		t.Fatalf("owner=%+v err=%v", got, err)
	}
	if atomic.LoadInt32(calls) != 3 {
		t.Fatalf("opaque ID did not reach provider: %d", atomic.LoadInt32(calls))
	}
}

func TestResponsesContinuationUsesExactDurableAndEphemeralIdentity(t *testing.T) {
	setupRelayTestDB(t, &model.ResponseOwner{})
	owner, err := model.NewResponseOwner("resp-exact", 11, 12, 13, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = model.CreateResponseOwner(context.Background(), owner); err != nil {
		t.Fatal(err)
	}
	c, _ := responsesOwnerTestContext(11, 12)
	recordResponsesEphemeralProof(c, "resp-proof", 13)
	t.Cleanup(func() { clearResponsesEphemeralProof(c, "resp-proof", 13) })
	for _, id := range []string{" resp-exact ", "RESP-EXACT", " resp-proof ", "RESP-PROOF"} {
		fresh, _ := responsesOwnerTestContext(11, 12)
		if _, err := prepareResponsesContinuationOwnership(fresh, &types.OpenAIResponsesRequest{PreviousResponseID: id}); err == nil {
			t.Fatalf("alias %q accepted", id)
		}
	}
	exact := " resp-proof-raw "
	recordResponsesEphemeralProof(c, exact, 13)
	t.Cleanup(func() { clearResponsesEphemeralProof(c, exact, 13) })
	fresh, _ := responsesOwnerTestContext(11, 12)
	route, err := prepareResponsesContinuationOwnership(fresh, &types.OpenAIResponsesRequest{PreviousResponseID: exact})
	if err != nil || route.ChannelID != 13 {
		t.Fatalf("raw proof lost: %+v %v", route, err)
	}
	if _, ok := lookupResponsesEphemeralProof(c, strings.TrimSpace(exact)); ok {
		t.Fatal("raw proof authorized trimmed alias")
	}
}

func TestBackgroundResponsesPreservesOpaqueIDAcrossOwnerAndPoll(t *testing.T) {
	id := " resp-background-raw "
	var polls atomic.Int32
	r, rec := backgroundFixture(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if req.Method == http.MethodPost {
			_, _ = io.WriteString(w, `{"id":" resp-background-raw ","status":"queued"}`)
			return
		}
		polls.Add(1)
		if req.URL.Path != "/v1/responses/"+id {
			t.Errorf("poll identity changed: %q", req.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":" resp-background-raw ","status":"completed"}`)
	}, `{"model":"gpt-5","background":true,"store":false,"input":"hello"}`)
	if apiErr, _ := RelayHandler(r); apiErr != nil {
		t.Fatalf("create=%+v raw=%q", apiErr, rec.Body.String())
	}
	waitBackgroundObserver(t, r.c)
	owner, err := model.GetResponseOwner(context.Background(), id, 1)
	if err != nil || owner.ResponseID != id {
		t.Fatalf("owner=%+v err=%v", owner, err)
	}
	task, err := model.GetBackgroundResponseTask(context.Background(), *owner.TaskOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if model.TaskProviderID(task) != id {
		t.Fatalf("task ID changed: %q", model.TaskProviderID(task))
	}
	if err = pollBackgroundResponse(context.Background(), task); err != nil {
		t.Fatal(err)
	}
	if polls.Load() != 1 || task.ProviderState != model.TaskProviderStateClosed {
		t.Fatalf("poll=%d task=%+v", polls.Load(), task)
	}
}

func TestStoredResponsesOpaqueIDDoesNotSelectChildOperation(t *testing.T) {
	owner, _, calls := setupStoredResponsesHandlerTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses/input_items" {
			t.Errorf("ID interpreted as operation: %q", r.URL.Path)
		}
		w.WriteHeader(200)
	})
	exact, err := model.NewResponseOwner("input_items", owner.UserID, owner.TokenID, owner.ChannelID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = model.CreateResponseOwner(context.Background(), exact); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses/input_items", nil)
	c.Params = gin.Params{{Key: "response_id", Value: exact.ResponseID}}
	c.Set("id", owner.UserID)
	c.Set("token_id", owner.TokenID)
	StoredResponses(c)
	if rec.Code != 200 || atomic.LoadInt32(calls) != 1 {
		t.Fatalf("status=%d calls=%d", rec.Code, atomic.LoadInt32(calls))
	}
}
