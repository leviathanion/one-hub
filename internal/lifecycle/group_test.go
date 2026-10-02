package lifecycle

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClosingGroupWaitsForAdmittedWorkAndRejectsNewWork(t *testing.T) {
	var g Group
	finish, ok := g.Start()
	if !ok {
		t.Fatal("admission")
	}
	g.Close()
	if _, ok := g.Start(); ok {
		t.Fatal("late admission")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("premature completion", err)
	}
	finish()
	finish()
	g.Close()
	if err := g.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestHandlerPreservesAdmissionAndCompletion(t *testing.T) {
	var g Group
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := g.Handler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(entered); <-release }))
	go func() { defer close(done); h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil)) }()
	<-entered
	g.Close()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 503 {
		t.Fatal(rec.Code)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if g.Wait(ctx) == nil {
		t.Fatal("handler was not tracked")
	}
	close(release)
	<-done
	if err := g.Wait(ctx); err != nil {
		t.Fatal(err)
	}
}
