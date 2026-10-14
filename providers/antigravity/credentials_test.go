package antigravity

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"one-api/common/config"
	"one-api/common/credentials"
	"one-api/model"
)

type credentialTestStore struct {
	mu            sync.Mutex
	row           credentials.Snapshot
	claimError    bool
	commitError   bool
	loseCommitAck bool
	beforeCommit  func(context.Context)
	afterClaim    func()
}

func (s *credentialTestStore) Load(_ context.Context, _ int) (credentials.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.row, nil
}
func (s *credentialTestStore) CompareAndSwap(ctx context.Context, expected credentials.Snapshot, data []byte, key *string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if key == nil && s.claimError {
		return false, errors.New("database unavailable")
	}
	if key != nil && s.commitError {
		return false, errors.New("commit unavailable")
	}
	if s.row.Version != expected.Version || s.row.Deleted {
		return false, nil
	}
	if key != nil && s.beforeCommit != nil {
		s.beforeCommit(ctx)
	}
	s.row.Version++
	s.row.InternalState = data
	if key == nil && s.afterClaim != nil {
		f := s.afterClaim
		s.afterClaim = nil
		f()
	}
	if key != nil {
		s.row.Key = *key
		if s.loseCommitAck {
			return false, errors.New("commit acknowledgement lost")
		}
	}
	return true, nil
}

func setupCredentialTest(t *testing.T, do func(*http.Request) (*http.Response, error)) (*credentialTestStore, *model.Channel) {
	t.Helper()
	initial := &Credentials{AccessToken: "old", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Minute).Unix(), Subject: "subject", ProjectID: "project"}
	key, err := initial.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	store := &credentialTestStore{row: credentials.Snapshot{ChannelID: 919, Type: config.ChannelTypeAntigravity, Key: key, Version: 10}}
	oldRotation, oldLoad, oldClient := CredentialRotation, loadCredentialChannel, newCredentialOAuthClient
	t.Cleanup(func() {
		CredentialRotation, loadCredentialChannel, newCredentialOAuthClient = oldRotation, oldLoad, oldClient
	})
	CredentialRotation = credentials.Service{Store: store}
	loadCredentialChannel = func(ctx context.Context, id int) (*model.Channel, error) {
		row, err := store.Load(ctx, id)
		return &model.Channel{Id: row.ChannelID, Type: row.Type, Key: row.Key, Version: row.Version, InternalState: row.InternalState}, err
	}
	newCredentialOAuthClient = func(string) (*OAuthClient, error) { return mockOAuthClient(t, do), nil }
	return store, &model.Channel{Id: store.row.ChannelID, Type: store.row.Type, Key: key, Version: 10}
}

func TestCredentialRefreshRequiresDurableClaimAndKeepsFenceAfterAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name        string
		claimError  bool
		upstreamErr bool
		commitError bool
	}{
		{"database_unavailable", true, false, false},
		{"exchange_ambiguous", false, true, false},
		{"commit_failed", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			store, ch := setupCredentialTest(t, func(*http.Request) (*http.Response, error) {
				calls++
				if tc.upstreamErr {
					return nil, errors.New("connection lost")
				}
				return oauthResponse(200, `{"access_token":"next","refresh_token":"next-refresh","expires_in":3600}`), nil
			})
			store.claimError, store.commitError = tc.claimError, tc.commitError
			_, result, err := ResolveCredentials(t.Context(), ch)
			if err == nil || result != nil {
				t.Fatal("failed refresh published credentials")
			}
			row, _ := store.Load(t.Context(), ch.Id)
			fence, _ := credentials.ReadRefresh(row.InternalState)
			if tc.claimError {
				if calls != 0 || fence != nil {
					t.Fatal("OAuth dispatched without durable claim")
				}
			} else if calls != 1 || fence == nil {
				t.Fatal("ambiguous refresh lost its fence")
			}
			if row.Key != ch.Key {
				t.Fatal("failed refresh overwrote persisted key")
			}
			_, _, _ = ResolveCredentials(t.Context(), ch)
			if !tc.claimError && calls != 1 {
				t.Fatal("fenced refresh was replayed")
			}
		})
	}
}

func TestCredentialCommitResolvesLostAcknowledgementBeforePublishing(t *testing.T) {
	calls := 0
	store, ch := setupCredentialTest(t, func(*http.Request) (*http.Response, error) {
		calls++
		return oauthResponse(200, `{"access_token":"next","expires_in":3600}`), nil
	})
	store.loseCommitAck = true
	channel, result, err := ResolveCredentials(t.Context(), ch)
	if err != nil || result.AccessToken != "next" || result.RefreshToken != "refresh" || calls != 1 {
		t.Fatalf("refresh failed: %v calls=%d", err, calls)
	}
	row, _ := store.Load(t.Context(), ch.Id)
	fence, _ := credentials.ReadRefresh(row.InternalState)
	if fence != nil || channel.Key != row.Key || channel.Version != row.Version || row.Version != 12 {
		t.Fatal("published credential differs from durable state")
	}
}

func TestCredentialCommitSurvivesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	store, ch := setupCredentialTest(t, func(*http.Request) (*http.Response, error) {
		cancel()
		return oauthResponse(200, `{"access_token":"next","expires_in":3600}`), nil
	})
	store.beforeCommit = func(ctx context.Context) {
		if ctx.Err() != nil {
			t.Fatal("commit inherited canceled caller")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("commit has no deadline")
		}
	}
	_, result, err := ResolveCredentials(ctx, ch)
	if err != nil || result.AccessToken != "next" {
		t.Fatalf("commit = %v", err)
	}
}

func TestCredentialDraftNeverRefreshesOrWritesSavedChannel(t *testing.T) {
	calls := 0
	_, ch := setupCredentialTest(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected OAuth") })
	ch.Id = 0
	if _, _, err := ResolveCredentials(t.Context(), ch); err == nil || calls != 0 {
		t.Fatal("expired draft was refreshed")
	}
	creds, _ := ParseCredentials(ch.Key)
	creds.ExpiresAt = time.Now().Add(time.Minute).Unix()
	ch.Key, _ = creds.ToJSON()
	if _, result, err := ResolveCredentials(t.Context(), ch); err != nil || result.AccessToken != "old" || calls != 0 {
		t.Fatal("valid draft cannot be used without refresh")
	}
}

func TestCredentialFenceAllowsOnlyStillValidOldAccessToken(t *testing.T) {
	calls := 0
	store, ch := setupCredentialTest(t, func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected OAuth") })
	store.row.InternalState, _ = credentials.WriteRefresh(nil, &credentials.Refresh{AttemptID: "earlier", StartedAt: 1})
	creds, _ := ParseCredentials(ch.Key)
	creds.ExpiresAt = time.Now().Add(time.Minute).Unix()
	store.row.Key, _ = creds.ToJSON()
	if _, result, err := ResolveCredentials(t.Context(), ch); err != nil || result.AccessToken != "old" || calls != 0 {
		t.Fatalf("valid old token failed: %v", err)
	}
	creds.ExpiresAt = 1
	store.row.Key, _ = creds.ToJSON()
	if _, _, err := ResolveCredentials(t.Context(), ch); !errors.Is(err, ErrCredentialRefreshUnresolved) || calls != 0 {
		t.Fatal("expired fenced credential was refreshed")
	}
}

func TestCredentialRecoveryRequiresExactSnapshotAndIdentity(t *testing.T) {
	store, ch := setupCredentialTest(t, nil)
	store.row.InternalState, _ = credentials.WriteRefresh(nil, &credentials.Refresh{AttemptID: "fenced", StartedAt: 1})
	expected := store.row
	next, _ := ParseCredentials(ch.Key)
	next.AccessToken = "recovered"
	next.ExpiresAt = time.Now().Add(time.Hour).Unix()
	next.Subject = "other"
	if err := RecoverCredentials(t.Context(), expected, "fenced", next); !errors.Is(err, credentials.ErrConflict) {
		t.Fatal("cross-account recovery accepted")
	}
	next.Subject = "subject"
	next.ProjectID = "other"
	if err := RecoverCredentials(t.Context(), expected, "fenced", next); !errors.Is(err, credentials.ErrConflict) {
		t.Fatal("cross-project recovery accepted")
	}
	next.ProjectID = "project"
	stale := expected
	stale.Version--
	if err := RecoverCredentials(t.Context(), stale, "fenced", next); !errors.Is(err, credentials.ErrConflict) {
		t.Fatal("stale recovery accepted")
	}
	if err := RecoverCredentials(t.Context(), expected, "fenced", next); err != nil {
		t.Fatal(err)
	}
	fence, _ := credentials.ReadRefresh(store.row.InternalState)
	if fence != nil || store.row.Key == ch.Key {
		t.Fatal("recovery failed to replace fenced credential")
	}
}

func TestCredentialRefreshCancellationBeforeDispatchClearsFence(t *testing.T) {
	calls := 0
	store, channel := setupCredentialTest(t, func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected exchange")
	})
	ctx, cancel := context.WithCancel(t.Context())
	store.afterClaim = cancel
	_, _, err := ResolveCredentials(ctx, channel)
	if !errors.Is(err, ErrOAuthRefreshNotDispatched) || calls != 0 {
		t.Fatalf("canceled refresh error=%v calls=%d", err, calls)
	}
	fence, _ := credentials.ReadRefresh(store.row.InternalState)
	if fence != nil || store.row.Key != channel.Key {
		t.Fatal("known unsent refresh retained a fence or changed the credential")
	}
}

func TestConcurrentCredentialRefreshUsesOneDurableExchange(t *testing.T) {
	calls := 0
	_, channel := setupCredentialTest(t, func(*http.Request) (*http.Response, error) {
		calls++ // 同渠道互斥保护实际交换。
		return oauthResponse(200, `{"access_token":"next","expires_in":3600}`), nil
	})
	var wait sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, result, err := ResolveCredentials(t.Context(), channel)
			if err == nil && (result == nil || result.AccessToken != "next") {
				err = errors.New("wrong published token")
			}
			results <- err
		}()
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("OAuth exchanges = %d", calls)
	}
}

func TestCredentialJSONRejectsAmbiguousIdentity(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"access_token":"one","access_token":"two","refresh_token":"refresh","expires_at":1,"subject":"s","project_id":"p"}`, `{"refresh_token":"only"}`} {
		if _, err := ParseCredentials(raw); err == nil {
			t.Fatal("accepted incomplete or ambiguous credential")
		}
	}
}
