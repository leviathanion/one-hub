package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"one-api/common/config"

	"gorm.io/gorm"
)

func TestChannelMetadataEditDoesNotOverwriteConcurrentRotation(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 32010, "credential-a")
	readComplete, continueEdit := make(chan struct{}), make(chan struct{})
	var paused atomic.Bool
	callback := "test:pause_metadata_snapshot"
	if err := DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" && paused.CompareAndSwap(false, true) {
			close(readComplete)
			<-continueEdit
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Query().Remove(callback) })
	done := make(chan error, 1)
	go func() { done <- (&Channel{Id: 32010, Name: "updated-name"}).UpdateRaw(false) }()
	<-readComplete
	ticket := CredentialRotationTicket{ChannelID: 32010, AttemptID: "refresh-b", ExpectedRevision: 0}
	claim, claimErr := ClaimCredentialRotation(context.Background(), ticket, time.Now())
	commit, commitErr := CommitCredentialRotation(context.Background(), ticket, "credential-b")
	close(continueEdit)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if claimErr != nil || claim != CredentialRotationClaimAcquired || commitErr != nil || commit != CredentialRotationCommitApplied {
		t.Fatalf("rotation: claim=%v/%v commit=%v/%v", claim, claimErr, commit, commitErr)
	}
	persisted, err := GetChannelById(32010)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Key != "credential-b" || persisted.CredentialRevision != 1 || persisted.CredentialRefreshFence != nil || persisted.Name != "updated-name" {
		t.Fatalf("metadata edit changed rotated credentials: %+v", persisted)
	}
}

func TestChannelEditWhitelistSupportsZeroValues(t *testing.T) {
	useTestChannelDB(t)
	weight, priority := uint(4), int64(3)
	insertTestChannel(t, &Channel{Id: 32011, Type: config.ChannelTypeOpenAI, Key: "key-a", Name: "before", Group: "default", Models: "gpt-5", Weight: &weight, Priority: &priority, OnlyChat: true, PreCost: 3, UsedQuota: 20, CreatedTime: 123})
	var request ChannelEditRequest
	if err := json.Unmarshal([]byte(`{"id":32011,"name":"","weight":0,"priority":0,"only_chat":false,"pre_cost":0,"used_quota":999,"created_time":999,"test_time":999,"balance":999}`), &request); err != nil {
		t.Fatal(err)
	}
	var updateSQL string
	callback := "test:capture_metadata_sql"
	if err := DB.Callback().Update().After("gorm:update").Register(callback, func(tx *gorm.DB) {
		updateSQL = tx.Statement.SQL.String()
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = DB.Callback().Update().Remove(callback) })
	if err := request.Update(); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"key", "credential_revision", "credential_refresh_fence", "type", "base_url", "used_quota", "created_time", "test_time", "balance"} {
		if strings.Contains(updateSQL, "`"+forbidden+"`") {
			t.Errorf("ordinary SQL contains %s: %s", forbidden, updateSQL)
		}
	}
	persisted, err := GetChannelById(32011)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Name != "" || *persisted.Weight != 0 || *persisted.Priority != 0 || persisted.OnlyChat || persisted.PreCost != 0 || persisted.UsedQuota != 20 || persisted.CreatedTime != 123 || persisted.Models != "gpt-5" {
		t.Fatalf("zero-value edit did not preserve field boundaries: %+v", persisted)
	}
}

func TestChannelIdentityHeadersCannotBeChangedByOrdinaryEdit(t *testing.T) {
	for _, header := range []string{
		`{"openai-organization":"other-org","OpenAI-Project":"project-a"}`,
		`{"OpenAI-Organization":"org-a","OPENAI-PROJECT":"other-project"}`,
		`{"OpenAI-Organization":"org-a"}`,
		`{}`,
		`{"gpt-5":{"OpenAI-Organization":"other-org"}}`,
		`{"OpenAI-Organization":"org-a","openai-organization":"other-org","OpenAI-Project":"project-a"}`,
	} {
		t.Run(header, func(t *testing.T) {
			useTestChannelDB(t)
			original := `{"OpenAI-Organization":"org-a","OpenAI-Project":"project-a","x-business-tag":"old"}`
			insertTestChannel(t, &Channel{Id: 32012, Type: config.ChannelTypeOpenAI, Key: "key-a", ModelHeaders: &original})
			candidate := &Channel{Id: 32012, ModelHeaders: &header}
			if err := candidate.UpdateRaw(false); err == nil {
				t.Fatal("identity header edit succeeded")
			}
			persisted, err := GetChannelById(32012)
			if err != nil || persisted.ModelHeaders == nil || *persisted.ModelHeaders != original {
				t.Fatalf("identity mutated: %+v err=%v", persisted, err)
			}
		})
	}
}

func TestChannelIdentityHeadersAllowBusinessEditsAndCaseNormalization(t *testing.T) {
	useTestChannelDB(t)
	original := `{"OpenAI-Organization":"org-a","OpenAI-Project":"project-a","x-business-tag":"old"}`
	insertTestChannel(t, &Channel{Id: 32013, Type: config.ChannelTypeOpenAI, Key: "key-a", ModelHeaders: &original})
	changed := `{"openai-organization":" org-a ","OPENAI-PROJECT":"project-a","x-business-tag":"new"}`
	if err := (&Channel{Id: 32013, ModelHeaders: &changed}).UpdateRaw(false); err != nil {
		t.Fatal(err)
	}
	persisted, err := GetChannelById(32013)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := persisted.HeaderIdentity()
	if err != nil || identity.Organization != "org-a" || identity.Project != "project-a" {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	headers, _ := persisted.GetModelHeadersMap()
	if headers["x-business-tag"] != "new" {
		t.Fatalf("business header not updated: %+v", headers)
	}
}

func TestCredentialRotationSameRevisionHasOnlyOneWinner(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 32014, "original")
	first := CredentialRotationTicket{ChannelID: 32014, AttemptID: "first", ExpectedRevision: 0}
	second := CredentialRotationTicket{ChannelID: 32014, AttemptID: "second", ExpectedRevision: 0}
	if outcome, err := ClaimCredentialRotation(context.Background(), first, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("first claim=%v err=%v", outcome, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), first, "winner"); err != nil || outcome != CredentialRotationCommitApplied {
		t.Fatalf("first commit=%v err=%v", outcome, err)
	}
	if outcome, err := ClaimCredentialRotation(context.Background(), second, time.Now()); err != nil || outcome != CredentialRotationClaimSuperseded {
		t.Fatalf("stale claim=%v err=%v", outcome, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), second, "loser"); err != nil || outcome != CredentialRotationCommitSuperseded {
		t.Fatalf("stale commit=%v err=%v", outcome, err)
	}
	snapshot, err := LoadCredentialRotationSnapshot(context.Background(), 32014)
	if err != nil || snapshot.Key != "winner" || snapshot.Revision != 1 {
		t.Fatalf("winner overwritten: %+v err=%v", snapshot, err)
	}
}

func TestChannelReauthorizationConcurrentCASHasOnlyOneWinner(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 32015, "original")
	sqlDB, err := DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite 以单连接串行执行两条竞争 CAS，测试不依赖 SQLITE_BUSY 的调度。
	sqlDB.SetMaxOpenConns(1)
	start := make(chan struct{})
	results := make(chan error, 2)
	ticket := CredentialRotationTicket{ChannelID: 32015, ExpectedRevision: 0, AttemptID: "authorized-claim"}
	if outcome, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("claim=%v err=%v", outcome, err)
	}
	for _, key := range []string{"authorized-first", "authorized-second"} {
		go func(key string) {
			<-start
			results <- ReplaceChannelCredentialWithContext(context.Background(), ticket, key)
		}(key)
	}
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			wins++
		} else if errors.Is(err, ErrChannelCredentialConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	snapshot, err := LoadCredentialRotationSnapshot(context.Background(), 32015)
	if err != nil || snapshot.Revision != 1 || snapshot.Fence != nil || (snapshot.Key != "authorized-first" && snapshot.Key != "authorized-second") {
		t.Fatalf("unexpected committed credentials: %+v err=%v", snapshot, err)
	}
}

func TestChannelTagMetadataPreservesRefreshedMemberAndRejectsOldKeyList(t *testing.T) {
	useTestChannelDB(t)
	channel := &Channel{Id: 32016, Type: config.ChannelTypeCodex, Key: "credential-a", Name: "member", Tag: "refresh-team", Models: "old-model"}
	insertTestChannel(t, channel)
	view, err := GetChannelsTag("refresh-team")
	if err != nil {
		t.Fatal(err)
	}
	ticket := CredentialRotationTicket{ChannelID: channel.Id, ExpectedRevision: 0, AttemptID: "refresh-b"}
	if outcome, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("claim=%v err=%v", outcome, err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), ticket, "credential-b"); err != nil || outcome != CredentialRotationCommitApplied {
		t.Fatalf("commit=%v err=%v", outcome, err)
	}
	if err := UpdateChannelsTagWithSubmittedFields("refresh-team", &Channel{Models: "new-model"}, ChannelTagSubmittedFields{"models": {}}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateChannelsTagWithSubmittedFields("refresh-team", &Channel{Key: view.Key, Models: "stale-edit"}, ChannelTagSubmittedFields{"key": {}, "models": {}}); err == nil {
		t.Fatal("stale credential list was accepted by ordinary tag edit")
	}
	var members []Channel
	if err := DB.Unscoped().Where("tag = ?", "refresh-team").Find(&members).Error; err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Id != channel.Id || members[0].DeletedAt.Valid || members[0].Key != "credential-b" || members[0].CredentialRevision != 1 || members[0].CredentialRefreshFence != nil || members[0].Models != "new-model" {
		t.Fatalf("tag metadata replaced a refreshed member: %+v", members)
	}
}

func TestAddChannelToTagStartsIndependentCredentialLifecycle(t *testing.T) {
	useTestChannelDB(t)
	insertTestChannel(t, &Channel{Id: 32017, Type: config.ChannelTypeCodex, Key: "original", Name: "member", Tag: "independent-team", CredentialRevision: 4})
	ticket := CredentialRotationTicket{ChannelID: 32017, ExpectedRevision: 4, AttemptID: "original-refresh"}
	if outcome, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("claim=%v err=%v", outcome, err)
	}
	added, err := AddChannelToTag("independent-team", &Channel{Key: "new-account"})
	if err != nil {
		t.Fatal(err)
	}
	if added.Id == 32017 || added.CredentialRevision != 0 || added.CredentialRefreshFence != nil || added.CredentialRefreshStartedAt != nil {
		t.Fatalf("new member inherited another channel's credential lifecycle: %+v", added)
	}
	original, err := LoadCredentialRotationSnapshot(context.Background(), 32017)
	if err != nil || original.Revision != 4 || original.Fence == nil || *original.Fence != ticket.AttemptID {
		t.Fatalf("adding member changed original credential lifecycle: %+v err=%v", original, err)
	}
}
