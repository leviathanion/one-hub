package codex

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var errCodexCredentialCASConflict = errors.New("codex credential compare-and-set conflict")

var ErrOAuthCredentialsRequireReauthorization = errors.New("OAuth credentials require reauthorization")

// pendingCredentialCommit is a process-local recovery journal, not a cache.
// Consequently it has no TTL: elapsed time can never make a rotated one-time
// refresh token safe to discard. An entry leaves the journal only after the
// rotated value is durable or a different durable manual value supersedes it.
//
// Trade-off: a hard process/host loss can still destroy this journal. Closing
// that last OAuth/DB atomicity gap requires a separate encrypted durable secret
// store that remains writable when the channel database is unavailable. We keep
// the normal transient-failure path small and lossless in-process, and surface a
// reauthorization requirement when the remote rotation can no longer be recovered
// instead of treating a timer as durability. Memory is bounded to one entry per
// channel: token paths must reconcile that entry under the channel lock before
// they are allowed to start another exchange.
type pendingCredentialCommit struct {
	expectedKey string
	rotatedKey  string
}

var pendingCredentialCommits = struct {
	sync.Mutex
	items map[int]pendingCredentialCommit
}{items: make(map[int]pendingCredentialCommit)}

// ambiguousCredentialRefreshes is a non-expiring circuit breaker for outcomes
// where the replacement refresh token is unknowable. It is keyed by the durable
// credential value used for the ambiguous exchange; an explicit manual key change
// clears the block on the next attempt. This breaker is intentionally process-local:
// persisting the fact that a secret is unknown cannot recover that secret and would
// add a durable operator-state protocol. After a restart the remote will either
// still accept the old token or reject it definitively; until then, this process
// trades automatic recovery attempts for protection against consuming a one-time
// token twice.
var ambiguousCredentialRefreshes = struct {
	sync.Mutex
	expectedKeys map[int]string
}{expectedKeys: make(map[int]string)}

func rememberAmbiguousCredentialRefresh(channelID int, expectedKey string) {
	if channelID <= 0 || expectedKey == "" {
		return
	}
	ambiguousCredentialRefreshes.Lock()
	ambiguousCredentialRefreshes.expectedKeys[channelID] = expectedKey
	ambiguousCredentialRefreshes.Unlock()
}

func requireUnambiguousCredentialRefresh(channelID int, currentKey string) error {
	if channelID <= 0 {
		return nil
	}
	ambiguousCredentialRefreshes.Lock()
	defer ambiguousCredentialRefreshes.Unlock()
	expectedKey, blocked := ambiguousCredentialRefreshes.expectedKeys[channelID]
	if !blocked {
		return nil
	}
	if expectedKey != currentKey {
		delete(ambiguousCredentialRefreshes.expectedKeys, channelID)
		return nil
	}
	return ErrOAuthCredentialsRequireReauthorization
}

func rememberPendingCredentialCommit(channelID int, expectedKey, rotatedKey string) error {
	if channelID <= 0 || expectedKey == "" || rotatedKey == "" {
		return fmt.Errorf("invalid codex pending credential commit")
	}

	pendingCredentialCommits.Lock()
	defer pendingCredentialCommits.Unlock()
	if pending, ok := pendingCredentialCommits.items[channelID]; ok {
		if pending.expectedKey == expectedKey && pending.rotatedKey == rotatedKey {
			return nil
		}
		return fmt.Errorf("channel %d already has a different pending credential rotation", channelID)
	}
	pendingCredentialCommits.items[channelID] = pendingCredentialCommit{
		expectedKey: expectedKey,
		rotatedKey:  rotatedKey,
	}
	return nil
}

func getPendingCredentialCommit(channelID int) (pendingCredentialCommit, bool) {
	pendingCredentialCommits.Lock()
	defer pendingCredentialCommits.Unlock()
	pending, ok := pendingCredentialCommits.items[channelID]
	return pending, ok
}

func clearPendingCredentialCommit(channelID int, rotatedKey string) {
	pendingCredentialCommits.Lock()
	defer pendingCredentialCommits.Unlock()
	if pending, ok := pendingCredentialCommits.items[channelID]; ok && pending.rotatedKey == rotatedKey {
		delete(pendingCredentialCommits.items, channelID)
	}
}

func pendingCredentialChannelIDs() []int {
	pendingCredentialCommits.Lock()
	defer pendingCredentialCommits.Unlock()
	ids := make([]int, 0, len(pendingCredentialCommits.items))
	for channelID := range pendingCredentialCommits.items {
		ids = append(ids, channelID)
	}
	return ids
}

// ReconcilePendingCredentials is the single scheduled recovery owner. It is run
// before due-time decisions, so credential durability never depends on a request
// missing a usage cache or a token becoming close to expiry.
func ReconcilePendingCredentials(ctx context.Context) error {
	ctx = ensureContext(ctx)
	var reconcileErr error
	for _, channelID := range pendingCredentialChannelIDs() {
		if err := ctx.Err(); err != nil {
			return errors.Join(reconcileErr, err)
		}
		channel, err := loadLatestChannelByID(ctx, channelID)
		if err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("load channel %d for pending credential recovery: %w", channelID, err))
			continue
		}
		provider, ok := CodexProviderFactory{}.Create(channel).(*CodexProvider)
		if !ok || provider == nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("create provider for pending credential channel %d", channelID))
			continue
		}
		if err := provider.commitPendingCredentials(ctx); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("commit pending credentials for channel %d: %w", channelID, err))
		}
	}
	return reconcileErr
}

// commitPendingCredentials is also the synchronous credential gateway used by
// token operations. Scheduled reconciliation provides liveness; this guard
// prevents a request from consuming the stale durable refresh token while a
// rotated successor still awaits commit.
func (p *CodexProvider) commitPendingCredentials(ctx context.Context) error {
	if p == nil || p.channelID() <= 0 {
		return nil
	}
	channelID := p.channelID()
	if _, ok := getPendingCredentialCommit(channelID); !ok {
		return nil
	}

	release, err := acquireChannelRefreshLock(ctx, channelID)
	if err != nil {
		return fmt.Errorf("waiting for channel refresh lock: %w", err)
	}
	defer release()
	return p.commitPendingCredentialsLocked(ctx)
}

// commitPendingCredentialsLocked requires the caller to hold the per-channel
// refresh lock. Keeping this check inside the same critical section as due-time
// evaluation closes the gap where another provider could create a new pending
// rotation between an optimistic recovery check and the next OAuth exchange.
func (p *CodexProvider) commitPendingCredentialsLocked(ctx context.Context) error {
	channelID := p.channelID()
	pending, ok := getPendingCredentialCommit(channelID)
	if !ok {
		return nil
	}
	if _, err := parseRotatedCredentials(pending.rotatedKey); err != nil {
		// Do not silently discard a corrupt journal entry. It is safer to keep the
		// channel blocked and require explicit operator repair.
		return fmt.Errorf("invalid pending credentials for channel %d: %w", channelID, err)
	}
	if err := p.loadLatestCredentialsFromDatabase(ctx); err != nil {
		return fmt.Errorf("reload credentials before pending commit: %w", err)
	}
	latest := p.codexChannel()
	if latest == nil {
		return fmt.Errorf("channel %d disappeared before pending commit", channelID)
	}

	switch latest.Key {
	case pending.rotatedKey:
		clearPendingCredentialCommit(channelID, pending.rotatedKey)
		return nil
	case pending.expectedKey:
		p.credentialPersistenceMu.Lock()
		p.credentialExpectedKey = pending.expectedKey
		p.credentialRotatedKey = pending.rotatedKey
		p.credentialDirty = true
		p.credentialPersistenceMu.Unlock()
		return p.persistRefreshedCredentials(ctx)
	default:
		// A durable manual update is an explicit winner.
		clearPendingCredentialCommit(channelID, pending.rotatedKey)
		return nil
	}
}
