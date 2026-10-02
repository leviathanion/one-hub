package check_channel

import (
	"context"
	"one-api/providers/base"
	"one-api/types"
	"sync/atomic"
	"testing"
	"time"
)

type cancelCheckProvider struct {
	base.ChatInterface
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (p *cancelCheckProvider) CreateChatCompletion(*types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	p.calls.Add(1)
	close(p.entered)
	<-p.release
	return nil, &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "canceled"}}
}
func TestCheckStreamJoinsCurrentWorkAndStopsFurtherChecksOnCancellation(t *testing.T) {
	p := &cancelCheckProvider{entered: make(chan struct{}), release: make(chan struct{})}
	check := &CheckChannel{Models: []string{"gpt-test", "next"}, ChatInterface: p}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan *ModelResult)
	done := make(chan struct{})
	go func() { defer close(done); check.RunStream(ctx, results) }()
	<-p.entered
	cancel()
	select {
	case <-done:
		t.Fatal("worker abandoned in-flight provider work")
	default:
	}
	close(p.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker blocked after client left")
	}
	if p.calls.Load() != 1 {
		t.Fatal("started another check after cancellation")
	}
	if _, ok := <-results; ok {
		t.Fatal("producer did not close results")
	}
}
func TestCheckStreamDoesNotBlockPublishingAfterClientLeaves(t *testing.T) {
	check := &CheckChannel{Models: []string{"gpt-test"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results := make(chan *ModelResult)
	done := make(chan struct{})
	go func() { defer close(done); check.RunStream(ctx, results) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled stream blocked")
	}
}
