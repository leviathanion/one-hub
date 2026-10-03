package check_channel

import (
	"context"
	"one-api/common/cache"
	"one-api/common/config"
	"one-api/providers/base"
	"one-api/types"
	"sync"
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

type publishCancelCheckProvider struct {
	base.ChatInterface
	calls            atomic.Int32
	entered, release chan struct{}
}

func (p *publishCancelCheckProvider) CreateChatCompletion(*types.ChatCompletionRequest) (*types.ChatCompletionResponse, *types.OpenAIErrorWithStatusCode) {
	if p.calls.Add(1) == 5 {
		close(p.entered)
		<-p.release
	}
	return nil, &types.OpenAIErrorWithStatusCode{OpenAIError: types.OpenAIError{Message: "test provider error"}}
}
func TestCheckStreamDoesNotBlockPublishingAfterClientLeaves(t *testing.T) {
	oldRedis := config.RedisEnabled
	config.RedisEnabled = false
	cache.InitCacheManager()
	t.Cleanup(func() { config.RedisEnabled = oldRedis })
	provider := &publishCancelCheckProvider{entered: make(chan struct{}), release: make(chan struct{})}
	check := &CheckChannel{Models: []string{"gpt-test"}, ChatInterface: provider}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan *ModelResult)
	done := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(provider.release) }) }
	defer func() {
		cancel()
		unblock()
		for range results {
		}
		<-done
	}()
	go func() { defer close(done); check.RunStream(ctx, results) }()
	select {
	case <-provider.entered:
	case <-time.After(time.Second):
		t.Fatal("last check never started")
	}
	cancel()
	unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled stream blocked publishing final result")
	}
	if provider.calls.Load() != 5 {
		t.Fatalf("wanted all five checks before publish, got %d", provider.calls.Load())
	}
}
