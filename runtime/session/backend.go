package session

import (
	"context"
	"time"
)

type bindingBackend interface {
	ResolveBinding(ctx context.Context, bindingKey string) (*Binding, ResolveStatus)
	RevocationStatus(ctx context.Context, sessionKey string) RevocationStatus
	CreateBindingIfAbsent(ctx context.Context, binding *Binding, ttl time.Duration) BindingWriteStatus
	ReplaceBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, replacement *Binding, ttl time.Duration) BindingWriteStatus
	DeleteBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string) BindingWriteStatus
	TouchBindingIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, ttl time.Duration) BindingWriteStatus
	DeleteBindingAndRevokeIfSessionMatches(ctx context.Context, bindingKey, expectedSessionKey string, revokeTTL time.Duration) BindingWriteStatus
	CountBindings(ctx context.Context) int64
}

type bulkRevocationBackend interface {
	RevocationStatuses(ctx context.Context, sessionKeys []string) ([]RevocationStatus, error)
}
