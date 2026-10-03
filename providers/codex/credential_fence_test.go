package codex

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/cache"
	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func useCodexFenceDB(t *testing.T) {
	t.Helper()
	original := model.DB
	db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Channel{}); err != nil {
		t.Fatal(err)
	}
	model.DB = db
	t.Cleanup(func() { model.DB = original })
	cache.InitCacheManager()
}

func insertCodexFenceProvider(t *testing.T, id int, credentials *OAuth2Credentials) *CodexProvider {
	t.Helper()
	key, err := credentials.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	channel := &model.Channel{Id: id, Type: config.ChannelTypeCodex, Key: key, Status: config.ChannelStatusEnabled, Name: "fence", Models: "gpt-5", Group: "default"}
	if err := model.DB.Create(channel).Error; err != nil {
		t.Fatal(err)
	}
	return CodexProviderFactory{}.Create(channel).(*CodexProvider)
}

func TestRotateOnceAmbiguousOutcomeDurablyBlocksEveryPeer(t *testing.T) {
	useCodexFenceDB(t)
	provider := insertCodexFenceProvider(t, 32001, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "one-time", ExpiresAt: time.Now().Add(-time.Minute)})

	originalRefresh := refreshOAuthCredentials
	var calls atomic.Int32
	refreshOAuthCredentials = func(*OAuth2Credentials, context.Context, string) error {
		calls.Add(1)
		return ErrOAuthRefreshOutcomeAmbiguous
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })

	if _, err := provider.refreshTokenIfNeeded(context.Background(), time.Minute); !errors.Is(err, ErrCredentialReauthorizationRequired) {
		t.Fatalf("ambiguous refresh error = %v", err)
	}
	snapshot, err := loadRotationTestSnapshot(context.Background(), 32001)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fence == nil || snapshot.Version != 1 || snapshot.Key == "" {
		t.Fatalf("ambiguous exchange did not preserve fence: %+v", snapshot)
	}

	peerChannel, err := model.GetChannelById(32001)
	if err != nil {
		t.Fatal(err)
	}
	peer := CodexProviderFactory{}.Create(peerChannel).(*CodexProvider)
	if _, err := peer.refreshTokenIfNeeded(context.Background(), time.Minute); !errors.Is(err, ErrCredentialRefreshInProgress) && !errors.Is(err, ErrCredentialRefreshUnresolved) {
		t.Fatalf("peer fence error = %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fenced peer dispatched OAuth; calls=%d", got)
	}
}

func TestRotateOncePublishesOnlyAfterDurableCommit(t *testing.T) {
	useCodexFenceDB(t)
	provider := insertCodexFenceProvider(t, 32002, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)})

	originalRefresh := refreshOAuthCredentials
	refreshOAuthCredentials = func(credentials *OAuth2Credentials, _ context.Context, _ string) error {
		credentials.AccessToken = "new-access"
		credentials.RefreshToken = "new-refresh"
		credentials.ExpiresAt = time.Now().Add(time.Hour)
		return nil
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })

	refreshed, err := provider.refreshTokenIfNeeded(context.Background(), time.Minute)
	if err != nil || !refreshed {
		t.Fatalf("refresh = %v, %v", refreshed, err)
	}
	snapshot, err := loadRotationTestSnapshot(context.Background(), 32002)
	if err != nil {
		t.Fatal(err)
	}
	durable, err := FromJSON(snapshot.Key)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Fence != nil || snapshot.Version != 2 || durable.AccessToken != "new-access" || provider.Credentials.AccessToken != "new-access" {
		t.Fatalf("rotation was not atomically published: snapshot=%+v provider=%+v", snapshot, provider.Credentials)
	}
}

func TestDurableRotationFailureBoundaries(t *testing.T) {
	for _, scenario := range []string{"claim unavailable", "not dispatched", "request cancelled after exchange", "commit unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			useCodexFenceDB(t)
			provider := insertCodexFenceProvider(t, 34010, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			originalRefresh := refreshOAuthCredentials
			calls := 0
			refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string) error {
				calls++
				if scenario == "not dispatched" {
					return ErrOAuthRefreshNotDispatched
				}
				creds.AccessToken = "new-access"
				creds.RefreshToken = "new-refresh"
				creds.ExpiresAt = time.Now().Add(time.Hour)
				if scenario == "request cancelled after exchange" {
					cancel()
				}
				return nil
			}
			t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })
			callback := "test:rotation_storage_failure"
			if err := model.DB.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if scenario == "claim unavailable" || (scenario == "commit unavailable" && calls > 0) {
					tx.AddError(errors.New("write unavailable"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = model.DB.Callback().Update().Remove(callback) })
			refreshed, err := provider.refreshTokenIfNeeded(ctx, time.Minute)
			row, loadErr := loadRotationTestSnapshot(context.Background(), 34010)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			wantCalls := 1
			if scenario == "claim unavailable" {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("OAuth calls=%d want=%d", calls, wantCalls)
			}
			switch scenario {
			case "request cancelled after exchange":
				if err != nil || !refreshed || row.Fence != nil || row.Version != 2 || provider.Credentials.AccessToken != "new-access" {
					t.Fatalf("durable commit did not survive cancellation: %v", err)
				}
			case "not dispatched":
				if !errors.Is(err, ErrOAuthRefreshNotDispatched) || row.Fence != nil || row.Version != 2 {
					t.Fatal("safe cancellation did not release claim")
				}
			case "claim unavailable":
				if err == nil || refreshed || row.Fence != nil || row.Version != 0 {
					t.Fatal("failed claim mutated or dispatched")
				}
			case "commit unavailable":
				if !errors.Is(err, errCodexCredentialPersistence) || refreshed || row.Fence == nil || row.Version != 1 || provider.Credentials.AccessToken != "old-access" {
					t.Fatal("uncommitted credential published")
				}
				if cached := provider.getCachedCredentialSnapshot(context.Background(), 0); cached.AccessToken != "" {
					t.Fatal("uncommitted token entered cache")
				}
				peerRow, _ := model.GetChannelById(34010)
				peer := CodexProviderFactory{}.Create(peerRow).(*CodexProvider)
				if _, err := peer.refreshTokenIfNeeded(context.Background(), time.Minute); err == nil || calls != 1 {
					t.Fatal("new provider repeated ambiguous OAuth exchange")
				}
			}
		})
	}
}

func TestReloadRejectsReassignedProviderBeforeOAuthOrFallback(t *testing.T) {
	for _, forced := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal with valid fallback", true: "forced"}[forced], func(t *testing.T) {
			useCodexFenceDB(t)
			provider := insertCodexFenceProvider(t, 34011, &OAuth2Credentials{AccessToken: "still-valid-old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Minute)})
			// An editable channel ID may now belong to a different provider, even
			// when its JSON credential happens to resemble the old provider's.
			if err := model.DB.Model(&model.Channel{}).Where("id = ?", 34011).Updates(map[string]any{"type": config.ChannelTypeOpenAI, "version": gorm.Expr("version + 1")}).Error; err != nil {
				t.Fatal(err)
			}
			originalRefresh := refreshOAuthCredentials
			calls := 0
			refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string) error {
				calls++
				creds.AccessToken = "wrong-provider-result"
				return nil
			}
			t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })
			var err error
			if forced {
				_, err = provider.forceRefreshToken(context.Background())
			} else {
				var token string
				token, err = provider.GetToken()
				if token != "" {
					t.Errorf("superseded provider returned a token: %q", token)
				}
			}
			if !errors.Is(err, ErrCredentialRefreshSuperseded) || calls != 0 {
				t.Fatalf("superseded provider must stop before OAuth: calls=%d error=%v", calls, err)
			}
			row, err := loadRotationTestSnapshot(context.Background(), 34011)
			if err != nil || row.Version != 1 || row.Fence != nil {
				t.Fatalf("superseded provider changed the channel: %+v, %v", row, err)
			}
		})
	}
}

func TestForcedRotationUsesOneAuthoritativeProviderSnapshot(t *testing.T) {
	useCodexFenceDB(t)
	provider := insertCodexFenceProvider(t, 34012, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour)})
	originalLoad, originalRefresh := loadLatestChannelByID, refreshOAuthCredentials
	loads, calls := 0, 0
	loadLatestChannelByID = func(ctx context.Context, id int) (*model.Channel, error) {
		loads++
		return originalLoad(ctx, id)
	}
	refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string) error {
		calls++
		creds.AccessToken = "new-access"
		creds.RefreshToken = "new-refresh"
		return nil
	}
	t.Cleanup(func() { loadLatestChannelByID, refreshOAuthCredentials = originalLoad, originalRefresh })
	if refreshed, err := provider.forceRefreshToken(context.Background()); err != nil || !refreshed {
		t.Fatalf("forced refresh = %v, %v", refreshed, err)
	}
	if loads != 1 || calls != 1 {
		t.Fatalf("one provider snapshot and one exchange required: loads=%d calls=%d", loads, calls)
	}
}

func TestConcurrentProvidersWaitAndReuseCommittedCredential(t *testing.T) {
	useCodexFenceDB(t)
	first := insertCodexFenceProvider(t, 34013, &OAuth2Credentials{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Minute)})
	peer := CodexProviderFactory{}.Create(first.codexChannel()).(*CodexProvider)
	exchanging, finish := make(chan struct{}), make(chan struct{})
	originalRefresh := refreshOAuthCredentials
	var calls atomic.Int32
	refreshOAuthCredentials = func(creds *OAuth2Credentials, _ context.Context, _ string) error {
		if calls.Add(1) == 1 {
			close(exchanging)
		}
		<-finish
		creds.AccessToken = "new-access"
		creds.RefreshToken = "new-refresh"
		creds.ExpiresAt = time.Now().Add(time.Hour)
		return nil
	}
	t.Cleanup(func() { refreshOAuthCredentials = originalRefresh })
	type result struct {
		token string
		err   error
	}
	request := func(p *CodexProvider, resultCh chan<- result) {
		token, err := p.GetToken()
		resultCh <- result{token, err}
	}
	firstResult, peerResult := make(chan result, 1), make(chan result, 1)
	go request(first, firstResult)
	select {
	case <-exchanging:
	case result := <-firstResult:
		t.Fatalf("first provider never reached OAuth: %+v", result)
	case <-time.After(5 * time.Second):
		t.Fatal("first provider did not start exchange")
	}
	go request(peer, peerResult)
	select {
	case result := <-peerResult:
		close(finish)
		<-firstResult
		t.Fatalf("peer must await the active exchange, got %+v", result)
	case <-time.After(100 * time.Millisecond):
	}
	close(finish)
	for _, results := range []<-chan result{firstResult, peerResult} {
		select {
		case result := <-results:
			if result.err != nil || result.token != "new-access" {
				t.Fatalf("peer did not reuse durable result: %+v", result)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("provider did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("concurrent providers exchanged %d times", calls.Load())
	}
}
