package model

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// 外部数据库仅使用显式测试 DSN，并为每次测试创建独立数据库或 schema。
func forEachTestDatabase(t *testing.T, run func(*testing.T, *gorm.DB)) {
	t.Helper()
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var dialector gorm.Dialector
			if dialect == "sqlite" {
				dialector = sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
			} else {
				envKey := "ONEHUB_TEST_" + strings.ToUpper(dialect) + "_DSN"
				dsn := os.Getenv(envKey)
				if dsn == "" {
					t.Skipf("真实 %s 集成测试需要 %s", dialect, envKey)
				}
				name := fmt.Sprintf("onehub_test_%d", time.Now().UnixNano())
				var createSQL, dropSQL string
				if dialect == "mysql" {
					cfg, err := drivermysql.ParseDSN(dsn)
					if err != nil {
						t.Fatal("测试 MySQL DSN 无效")
					}
					if cfg.Net != "unix" || !strings.HasPrefix(cfg.Addr, "/tmp/") {
						host, _, err := net.SplitHostPort(cfg.Addr)
						if cfg.Net != "tcp" || err != nil || !isLoopbackTestHost(host) {
							t.Fatal("测试 MySQL 必须使用 loopback 或 /tmp 下的 socket")
						}
					}
					dialector = mysql.Open(dsn)
					createSQL = "CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4"
					dropSQL = "DROP DATABASE `" + name + "`"
					cfg.DBName = name
					dsn = cfg.FormatDSN()
				} else {
					parsed, err := url.Parse(dsn)
					if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || !isLoopbackTestHost(parsed.Hostname()) {
						t.Fatal("测试 PostgreSQL 必须使用 loopback URL")
					}
					dialector = postgres.Open(dsn)
					createSQL = `CREATE SCHEMA "` + name + `"`
					dropSQL = `DROP SCHEMA "` + name + `" CASCADE`
					query := parsed.Query()
					query.Set("search_path", name)
					parsed.RawQuery = query.Encode()
					dsn = parsed.String()
				}
				admin, err := gorm.Open(dialector, &gorm.Config{})
				if err != nil {
					t.Fatal(err)
				}
				adminSQL, err := admin.DB()
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = adminSQL.Close() })
				if err := admin.Exec(createSQL).Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := admin.Exec(dropSQL).Error; err != nil {
						t.Errorf("清理隔离测试数据库失败: %v", err)
					}
				})
				if dialect == "mysql" {
					dialector = mysql.Open(dsn)
				} else {
					dialector = postgres.Open(dsn)
				}
			}
			db, err := gorm.Open(dialector, &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sqlDB.Close() })
			run(t, db)
		})
	}
}

func isLoopbackTestHost(host string) bool {
	return host == "localhost" || net.ParseIP(host).IsLoopback()
}
