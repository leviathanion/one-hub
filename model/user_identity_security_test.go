package model

import (
	"context"
	"one-api/common/config"
	"sync"
	"testing"
)

func TestOIDCIssuerChangeCannotReassignExistingSubjects(t *testing.T) {
	db := useUserCreditDB(t)
	if err := db.AutoMigrate(&Option{}, &PublicationVersion{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsurePublicationVersionRows(db); err != nil {
		t.Fatal(err)
	}
	issuer := "https://issuer-a.example"
	config.GlobalOption.RegisterString("OIDCIssuer", &issuer)
	if _, err := config.GlobalOption.PublishRuntimeOverrides(2, map[string]string{"QuotaPerUnit": "1", "OIDCIssuer": issuer}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&Option{Key: "OIDCIssuer", Value: issuer}).Error; err != nil {
		t.Fatal(err)
	}
	subject := "stable-subject"
	if err := UpdateUserIdentity(1, UserIdentityPatch{OIDCId: &subject, OIDCIssuer: issuer}); err != nil {
		t.Fatal(err)
	}
	other := "https://issuer-b.example"
	for _, mutation := range []OptionMutation{{Key: "OIDCIssuer", Value: &other}, {Key: "OIDCIssuer", Inherit: true}} {
		if _, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{mutation}); err == nil {
			t.Fatal("已有绑定仍允许变更/移除签发方")
		}
	}
	if _, err := FindUserByOIDC(context.Background(), other, subject); err == nil {
		t.Fatal("不同签发方同名 subject 登录了旧账户")
	}
	if user, err := FindUserByOIDC(context.Background(), issuer, subject); err != nil || user.Id != 1 {
		t.Fatalf("原签发方登录失败: %+v %v", user, err)
	}
	if err := UnbindUserIdentity(1, "oidc"); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyOptionMutations(context.Background(), 1, []OptionMutation{{Key: "OIDCIssuer", Value: &other}}); err != nil {
		t.Fatal(err)
	}
	if err := UpdateUserIdentity(1, UserIdentityPatch{OIDCId: &subject, OIDCIssuer: issuer}); err == nil {
		t.Fatal("在途旧签发方请求仍可新建绑定")
	}
	if err := UpdateUserIdentity(2, UserIdentityPatch{OIDCId: &subject, OIDCIssuer: other}); err != nil {
		t.Fatal(err)
	}
}

func TestUserSecurityConcurrentBindingHasSingleOwner(t *testing.T) {
	useUserCreditDB(t)
	id, login := 987654, "binding-user"
	ready := make(chan struct{}, 2)
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, userID := range []int{1, 2} {
		wg.Add(1)
		go func(userID int) {
			defer wg.Done()
			if IsGitHubIdAlreadyTaken(login) {
				t.Error("测试起始身份已被占用")
			}
			ready <- struct{}{}
			<-start
			errs <- UpdateUserIdentity(userID, UserIdentityPatch{GitHubID: &login, GitHubIDNew: &id})
		}(userID)
	}
	<-ready
	<-ready
	close(start)
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	var count int64
	if err := DB.Model(&User{}).Where("github_id_new = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("同一第三方身份产生多个 owner: successful_bindings=%d owners=%d", successes, count)
	}
}
