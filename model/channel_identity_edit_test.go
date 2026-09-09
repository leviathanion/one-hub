package model

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"one-api/common/config"
)

func editChannelJSON(t *testing.T, body string) error {
	t.Helper()
	var request ChannelEditRequest
	if err := json.Unmarshal([]byte(body), &request); err != nil {
		return err
	}
	return request.Update()
}

func TestChannelAdminEditKeepsIDAndRuntimeState(t *testing.T) {
	useTestChannelDB(t) // 没有任务表，管理编辑不依赖占用查询。
	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeOpenAI, Key: "original-key", BaseURL: stringPtr("https://old.example"), Name: "before", UsedQuota: 42, CreatedTime: 123, Status: config.ChannelStatusManuallyDisabled})
	if err := editChannelJSON(t, `{"id":1,"base_url":"https://new.example","key":"replacement","name":"after","used_quota":999}`); err != nil {
		t.Fatal(err)
	}
	row, err := GetChannelById(1)
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	DB.Model(&Channel{}).Count(&count)
	if count != 1 || row.Id != 1 || row.GetBaseURL() != "https://new.example" || row.Name != "after" || row.Key != "replacement" || row.UsedQuota != 42 || row.CreatedTime != 123 || row.CredentialRevision != 1 || row.Status != config.ChannelStatusManuallyDisabled {
		t.Fatal("edit must update the same channel without cloning, enabling or resetting state")
	}
	if err := editChannelJSON(t, `{"id":1,"base_url":null}`); err != nil {
		t.Fatal(err)
	}
	row, _ = GetChannelById(1)
	if row.BaseURL != nil || row.Key != "replacement" {
		t.Fatal("explicit null URL or omitted key not preserved")
	}
}

func TestChannelAdminEditPreservesCredentialFence(t *testing.T) {
	useTestChannelDB(t)
	insertCredentialRotationChannel(t, 1, "credential-a")
	ticket := CredentialRotationTicket{ChannelID: 1, AttemptID: "refresh", ExpectedRevision: 0}
	if outcome, err := ClaimCredentialRotation(context.Background(), ticket, time.Now()); err != nil || outcome != CredentialRotationClaimAcquired {
		t.Fatalf("claim: %v/%v", outcome, err)
	}
	if err := editChannelJSON(t, `{"id":1,"key":"replacement"}`); err == nil {
		t.Fatal("edit must not bypass credential fence")
	}
	if err := editChannelJSON(t, `{"id":1,"name":"updated"}`); err != nil {
		t.Fatal(err)
	}
	if outcome, err := CommitCredentialRotation(context.Background(), ticket, "credential-b"); err != nil || outcome != CredentialRotationCommitApplied {
		t.Fatalf("commit: %v/%v", outcome, err)
	}
	if err := editChannelJSON(t, `{"id":1,"base_url":"https://new.example"}`); err != nil {
		t.Fatal(err)
	}
	row, _ := GetChannelById(1)
	if row.Key != "credential-b" || row.CredentialRevision != 2 {
		t.Fatal("edit must preserve latest credential and supersede old tickets")
	}
	if outcome, err := CommitCredentialRotation(context.Background(), ticket, "stale"); err != nil || outcome != CredentialRotationCommitSuperseded {
		t.Fatalf("stale commit: %v/%v", outcome, err)
	}
	if err := editChannelJSON(t, `{"id":1,"key":""}`); err == nil {
		t.Fatal("explicit empty key must fail")
	}
}

func TestChannelAdminEditSupportsExistingKeylessConnection(t *testing.T) {
	useTestChannelDB(t)
	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeOpenAI, BaseURL: stringPtr("https://old.example")})
	if err := editChannelJSON(t, `{"id":1,"base_url":"https://new.example"}`); err != nil {
		t.Fatalf("changing a keyless endpoint must not require a new credential: %v", err)
	}
	row, _ := GetChannelById(1)
	if row.Key != "" || row.GetBaseURL() != "https://new.example" {
		t.Fatal("keyless connection was not preserved")
	}
}

func TestChannelAdminEditCanRepairInvalidHeaders(t *testing.T) {
	useTestChannelDB(t)
	headers := `{"OpenAI-Project":123}`
	insertTestChannel(t, &Channel{Id: 1, Type: config.ChannelTypeOpenAI, Key: "key", ModelHeaders: &headers})
	if err := editChannelJSON(t, `{"id":1,"model_headers":"{\"OpenAI-Project\":\"valid-project\"}"}`); err != nil {
		t.Fatalf("administrator must be able to repair invalid source configuration: %v", err)
	}
	row, _ := GetChannelById(1)
	identity, err := row.HeaderIdentity()
	if err != nil || identity.Project != "valid-project" || row.CredentialRevision != 1 {
		t.Fatalf("repair did not save a valid configuration: %v", err)
	}
	if err := editChannelJSON(t, `{"id":1,"model_headers":"{\"OpenAI-Project\":false}"}`); err == nil {
		t.Fatal("new configuration must still be validated")
	}
}

func TestChannelAdminTagEditRollsBackOnCredentialFence(t *testing.T) {
	useTestChannelDB(t)
	for _, id := range []int{1, 2} {
		insertTestChannel(t, &Channel{Id: id, Type: config.ChannelTypeOpenAI, Key: "key", Tag: "team", BaseURL: stringPtr("https://old.example")})
	}
	if err := DB.Model(&Channel{}).Where("id = 2").Update("credential_refresh_fence", "pending").Error; err != nil {
		t.Fatal(err)
	}
	fields := ChannelTagSubmittedFields{"base_url": {}, "models": {}}
	update := &Channel{BaseURL: stringPtr("https://new.example"), Models: "gpt-new"}
	if err := UpdateChannelsTagWithSubmittedFields("team", update, fields, ChannelUpdateOptions{AllowIdentityChange: true}); err == nil {
		t.Fatal("tag edit must not override a member's credential fence")
	}
	for _, id := range []int{1, 2} {
		row, _ := GetChannelById(id)
		if row.GetBaseURL() != "https://old.example" || row.Models != "" {
			t.Fatal("tag edit partially committed")
		}
	}
}
