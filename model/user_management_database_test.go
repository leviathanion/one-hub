package model

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"one-api/common"
	"one-api/common/config"
)

func openUserManagementTestDB(t *testing.T, dialect, dsn string) *gorm.DB {
	t.Helper()
	var driver gorm.Dialector
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(dsn)
	case "postgres":
		driver = postgres.Open(dsn)
	case "mysql":
		driver = mysql.Open(dsn)
	default:
		t.Fatal("未知测试数据库")
	}
	db, err := gorm.Open(driver, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(8)
	t.Cleanup(func() { _ = pool.Close() })
	return db
}

// 子进程不共享 Go 锁、缓存或连接池，只共享数据库；Redis 故意没有客户端。
func TestUserVerificationProcess(t *testing.T) {
	dialect := os.Getenv("ONEHUB_VERIFICATION_CHILD_DIALECT")
	if dialect == "" {
		t.Skip("仅由数据库集成测试启动")
	}
	DB = openUserManagementTestDB(t, dialect, os.Getenv("ONEHUB_VERIFICATION_CHILD_DSN"))
	config.RedisEnabled = os.Getenv("ONEHUB_VERIFICATION_CHILD_REDIS") == "true"
	config.SessionSecret = os.Getenv("ONEHUB_VERIFICATION_CHILD_SECRET")
	id, err := ConsumeUserVerification(context.Background(), "process@example.com", common.PasswordResetPurpose, "process-token")
	if err != nil || id != 1 {
		t.Fatalf("凭据未被该进程取得: id=%d err=%v", id, err)
	}
}

func TestUserManagementDatabases(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			dsn := os.Getenv("ONEHUB_USER_" + strings.ToUpper(dialect) + "_DSN")
			if dialect == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "users.db") + "?_busy_timeout=10000&_txlock=immediate"
			} else {
				if dsn == "" {
					t.Skip("未提供隔离数据库 DSN")
				}
				if !strings.Contains(dsn, "onehub_user_test") {
					t.Fatal("只能在 onehub_user_test 隔离数据库执行破坏性夹具初始化")
				}
			}
			db := openUserManagementTestDB(t, dialect, dsn)
			for _, table := range []any{&UserVerification{}, &User{}, &UserGroup{}, &Option{}} {
				if err := db.Migrator().DropTable(table); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.AutoMigrate(&User{}, &UserGroup{}, &Option{}, &UserVerification{}); err != nil {
				t.Fatal(err)
			}
			if err := ensureUserIdentityCollations(db); err != nil {
				t.Fatal(err)
			}
			if err := ValidateUserManagementSchema(db); err != nil {
				t.Fatal(err)
			}
			oldDB := DB
			DB = db
			t.Cleanup(func() { DB = oldDB })
			for _, u := range []User{{Id: 1, Username: "target", Password: "old-hash", Quota: 200, AccessToken: "target-access", AffCode: "target-aff"}, {Id: 2, Username: "other", Password: "old-hash", AccessToken: "other-access", AffCode: "other-aff"}, {Id: 99, Username: "root", Password: "old-hash", Role: config.RoleRootUser, AccessToken: "root-access", AffCode: "root-aff"}} {
				if err := db.Create(&u).Error; err != nil {
					t.Fatal(err)
				}
			}

			t.Run("身份唯一归属", func(t *testing.T) {
				id := 731
				start := make(chan struct{})
				out := make(chan error, 2)
				for _, userID := range []int{1, 2} {
					go func(userID int) { <-start; out <- UpdateUserIdentity(userID, UserIdentityPatch{GitHubIDNew: &id}) }(userID)
				}
				close(start)
				successes := 0
				for range 2 {
					err := <-out
					if err == nil {
						successes++
					} else if !errors.Is(err, ErrIdentityOccupied) {
						t.Fatal(err)
					}
				}
				if successes != 1 {
					t.Fatalf("绑定成功次数=%d", successes)
				}
			})
			t.Run("空值不争用身份", func(t *testing.T) {
				for _, id := range []int{1, 2} {
					if err := UnbindUserIdentity(id, "github"); err != nil {
						t.Fatal(err)
					}
				}
				var count int64
				if err := db.Model(&User{}).Where("github_id_new IS NULL").Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 3 {
					t.Fatalf("未解绑为空值: %d", count)
				}
			})
			t.Run("身份大小写不能混同", func(t *testing.T) {
				for id, value := range map[int]string{1: "CaseSensitive", 2: "casesensitive"} {
					if err := UpdateUserIdentity(id, UserIdentityPatch{WeChatId: &value}); err != nil {
						t.Fatal(err)
					}
				}
				var u User
				if err := db.Where("wechat_id = ?", "casesensitive").First(&u).Error; err != nil || u.Id != 2 {
					t.Fatalf("身份大小写错误: id=%d err=%v", u.Id, err)
				}
			})
			t.Run("两个进程一次消费", func(t *testing.T) {
				if err := StoreUserVerification(context.Background(), "process@example.com", common.PasswordResetPurpose, "process-token", 1); err != nil {
					t.Fatal(err)
				}
				start := make(chan struct{})
				out := make(chan error, 2)
				for _, redis := range []string{"false", "true"} {
					go func(redis string) {
						ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
						defer cancel()
						cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestUserVerificationProcess$")
						cmd.Env = append(os.Environ(), "ONEHUB_VERIFICATION_CHILD_DIALECT="+dialect, "ONEHUB_VERIFICATION_CHILD_DSN="+dsn, "ONEHUB_VERIFICATION_CHILD_REDIS="+redis, "ONEHUB_VERIFICATION_CHILD_SECRET="+config.SessionSecret)
						<-start
						output, err := cmd.CombinedOutput()
						if err != nil && !strings.Contains(string(output), ErrVerificationInvalid.Error()) {
							t.Errorf("凭据竞争子进程异常: %v\n%s", err, output)
						}
						out <- err
					}(redis)
				}
				close(start)
				successes := 0
				for range 2 {
					if err := <-out; err == nil {
						successes++
					}
				}
				if successes != 1 {
					t.Fatalf("独立进程消费成功次数=%d", successes)
				}
				if _, err := ConsumeUserVerification(context.Background(), "process@example.com", common.PasswordResetPurpose, "process-token"); !errors.Is(err, ErrVerificationInvalid) {
					t.Fatalf("凭据仍可复用: %v", err)
				}
			})
			t.Run("错误码与新旧凭据隔离", func(t *testing.T) {
				for _, code := range []string{"old", "new"} {
					if err := StoreUserVerification(context.Background(), "reset@example.com", common.PasswordResetPurpose, code, 1); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := ConsumeUserVerification(context.Background(), "reset@example.com", common.PasswordResetPurpose, "old"); !errors.Is(err, ErrVerificationInvalid) {
					t.Fatal(err)
				}
				if id, err := ConsumeUserVerification(context.Background(), "reset@example.com", common.PasswordResetPurpose, "new"); err != nil || id != 1 {
					t.Fatalf("新码被旧请求删除: %v", err)
				}
			})
			t.Run("恢复地址转移不修改另一用户", func(t *testing.T) {
				if err := SetUserEmail(1, "recover@example.com"); err != nil {
					t.Fatal(err)
				}
				if err := StoreUserVerification(context.Background(), "recover@example.com", common.PasswordResetPurpose, "transfer-token", 1); err != nil {
					t.Fatal(err)
				}
				if err := SetUserEmail(1, ""); err != nil {
					t.Fatal(err)
				}
				if err := SetUserEmail(2, "recover@example.com"); err != nil {
					t.Fatal(err)
				}
				id, err := ConsumeUserVerification(context.Background(), "recover@example.com", common.PasswordResetPurpose, "transfer-token")
				if err != nil {
					t.Fatal(err)
				}
				if err := ResetUserPassword(context.Background(), id, "recover@example.com", "new-password123"); !errors.Is(err, ErrVerificationInvalid) {
					t.Fatalf("地址转移后仍重置: %v", err)
				}
				u, _ := GetUserById(2, true)
				if u.Password != "old-hash" {
					t.Fatal("另一用户密码被修改")
				}
			})
			t.Run("管理检查期间禁止并发改写目标", func(t *testing.T) {
				const hook = "test:hold-user-admin-target"
				read := make(chan struct{})
				release := make(chan struct{})
				var once sync.Once
				if err := db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
					if tx.Statement.Table == "users" {
						once.Do(func() { close(read); <-release })
					}
				}); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = db.Callback().Query().Remove(hook) })
				name := "updated"
				managed := make(chan error, 1)
				go func() {
					managed <- UpdateUserByAdmin(context.Background(), 99, UserAdminPatch{Id: 1, UserProfilePatch: UserProfilePatch{DisplayName: &name}})
				}()
				<-read
				writer := make(chan error, 1)
				go func() { writer <- db.Exec("UPDATE users SET role = ? WHERE id = 1", config.RoleRootUser).Error }()
				select {
				case err := <-writer:
					close(release)
					t.Fatalf("并发写穿透管理事务: %v", err)
				case <-time.After(100 * time.Millisecond):
				}
				close(release)
				if err := <-managed; err != nil {
					t.Fatal(err)
				}
				if err := <-writer; err != nil {
					t.Fatal(err)
				}
			})
			t.Run("签发方作用域与绑定共同受保护", func(t *testing.T) {
				issuer, subject := "https://issuer-a.example", "shared-subject"
				if err := db.Create(&Option{Key: "OIDCIssuer", Value: issuer}).Error; err != nil {
					t.Fatal(err)
				}
				if err := UpdateUserIdentity(1, UserIdentityPatch{OIDCId: &subject, OIDCIssuer: issuer}); err != nil {
					t.Fatal(err)
				}
				if _, err := FindUserByOIDC(context.Background(), "https://issuer-b.example", subject); err == nil {
					t.Fatal("不同签发方进入旧账户")
				}
				if user, err := FindUserByOIDC(context.Background(), issuer, subject); err != nil || user.Id != 1 {
					t.Fatalf("正常身份登录失败: %v", err)
				}
				if err := db.Transaction(func(tx *gorm.DB) error { return validateOIDCIssuerMutation(tx, "https://issuer-b.example", false) }); err == nil {
					t.Fatal("存在绑定仍允许更换签发方")
				}
				if dialect != "sqlite" {
					if err := UnbindUserIdentity(1, "oidc"); err != nil {
						t.Fatal(err)
					}
					tx := db.Begin()
					if tx.Error != nil {
						t.Fatal(tx.Error)
					}
					defer tx.Rollback()
					var count int64
					if err := tx.Model(&User{}).Where("oidc_id IS NOT NULL").Count(&count).Error; err != nil || count != 0 {
						t.Fatalf("未建立空绑定快照: %d %v", count, err)
					}
					if err := UpdateUserIdentity(2, UserIdentityPatch{OIDCId: &subject, OIDCIssuer: issuer}); err != nil {
						t.Fatal(err)
					}
					if err := validateOIDCIssuerMutation(tx, "https://issuer-b.example", false); err == nil {
						t.Fatal("旧事务快照漏掉了刚提交的绑定")
					}
				}
			})
			t.Run("旧数据迁移先拒绝冲突再建立唯一约束", func(t *testing.T) {
				if err := db.Migrator().DropTable(&User{}); err != nil {
					t.Fatal(err)
				}
				if err := db.AutoMigrate(&legacyIdentityUser{}); err != nil {
					t.Fatal(err)
				}
				for _, user := range []legacyIdentityUser{{Id: 1, Username: "old-one", Password: "old-hash", Email: "legacy@example.com"}, {Id: 2, Username: "old-two", Password: "old-hash", Email: "LEGACY@example.com"}, {Id: 3, Username: "old-three", Password: "old-hash"}} {
					if err := db.Create(&user).Error; err != nil {
						t.Fatal(err)
					}
				}
				migration := migrateUserIdentityUniqueness()
				if err := migration.Migrate(db); err == nil {
					t.Fatal("重复邮箱没有阻止升级")
				}
				if err := db.Table("users").Where("id = ?", 2).Update("email", "").Error; err != nil {
					t.Fatal(err)
				}
				if err := migration.Migrate(db); err != nil {
					t.Fatal(err)
				}
				if err := db.AutoMigrate(&User{}); err != nil {
					t.Fatal(err)
				}
				if err := ensureUserIdentityCollations(db); err != nil {
					t.Fatal(err)
				}
				if err := ValidateUserManagementSchema(db); err != nil {
					t.Fatal(err)
				}
				var count int64
				if err := db.Table("users").Where("email IS NULL").Count(&count).Error; err != nil {
					t.Fatal(err)
				}
				if count != 2 {
					t.Fatalf("旧空邮箱未转换: %d", count)
				}
				if err := db.Table("users").Where("id = ?", 2).Update("email", "legacy@example.com").Error; err == nil {
					t.Fatal("迁移后仍可产生重复邮箱")
				}
			})
		})
	}
}

type legacyIdentityUser struct {
	Id          int
	Username    string
	Password    string
	Email       string `gorm:"size:191"`
	OidcId      string `gorm:"size:255"`
	GitHubIdNew int
	WeChatId    string `gorm:"size:255"`
	TelegramId  int64
	LarkId      string `gorm:"size:255"`
}

func (legacyIdentityUser) TableName() string { return "users" }
