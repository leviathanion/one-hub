package catalogmigration

import (
	"fmt"
	"os"
	"testing"
	"time"

	mysqldsn "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Each external test creates and removes its own namespace. The supplied user
// must be allowed to create a schema (Postgres) or database (MySQL).
func TestMigrationSupportedDatabases(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			verifyMigration(t, openExternal(t, dialect))
		})
	}
}

func TestMigrationDatabaseFailureBoundaries(t *testing.T) {
	for _, dialect := range []string{"postgres", "mysql"} {
		t.Run(dialect, func(t *testing.T) {
			db := openExternal(t, dialect)
			seedLegacy(t, db)
			exec(t, db, "INSERT INTO model_info (id, model) VALUES (100, 'M-exact')")
			report, err := Run(db, true)
			if err == nil || len(report.Conflicts) != 1 {
				t.Fatalf("duplicate preflight: %+v %v", report, err)
			}
			if db.Migrator().HasColumn("model_info", "owned_by_id") {
				t.Fatal("duplicate preflight wrote schema")
			}
			exec(t, db, "DELETE FROM model_info WHERE id = 100")
			if dialect == "postgres" {
				exec(t, db, `CREATE FUNCTION reject_catalog_update() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'test migration failure'; END; $$`)
				exec(t, db, "CREATE TRIGGER reject_catalog_update BEFORE UPDATE ON model_info FOR EACH ROW EXECUTE FUNCTION reject_catalog_update()")
			} else {
				exec(t, db, "CREATE TRIGGER reject_catalog_update BEFORE UPDATE ON model_info FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'test migration failure'")
			}
			if _, err := Run(db, true); err == nil {
				t.Fatal("expected update failure")
			}
			if !db.Migrator().HasColumn("prices", "channel_type") {
				t.Fatal("legacy column deleted despite failed update")
			}
			hasOwner := db.Migrator().HasColumn("model_info", "owned_by_id")
			if dialect == "postgres" && hasOwner {
				t.Fatal("Postgres did not roll back schema")
			}
			if dialect == "mysql" {
				if !hasOwner {
					t.Fatal("expected MySQL nontransactional DDL")
				}
				if _, err := Run(db, true); err == nil {
					t.Fatal("partial MySQL migration resumed")
				}
			}
		})
	}
}

func openExternal(t *testing.T, dialect string) *gorm.DB {
	t.Helper()
	variable := "ONEHUB_TEST_POSTGRES_DSN"
	if dialect == "mysql" {
		variable = "ONEHUB_TEST_MYSQL_DSN"
	}
	dsn := os.Getenv(variable)
	if dsn == "" {
		t.Skip(variable + " 未设置")
	}
	config := &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)}
	var dialector gorm.Dialector
	if dialect == "postgres" {
		dialector = postgres.Open(dsn)
	} else {
		dialector = mysql.Open(dsn)
	}
	admin, err := gorm.Open(dialector, config)
	if err != nil {
		t.Fatal("无法连接外部测试数据库")
	}
	adminConn, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	adminConn.SetMaxOpenConns(1)
	adminConn.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = adminConn.Close() })
	name := fmt.Sprintf("catalog_migration_%d", time.Now().UnixNano())
	if dialect == "postgres" {
		exec(t, admin, "CREATE SCHEMA "+name)
		t.Cleanup(func() { exec(t, admin, "DROP SCHEMA "+name+" CASCADE") })
		exec(t, admin, "SET search_path TO "+name)
		return admin
	}
	exec(t, admin, "CREATE DATABASE "+name+" CHARACTER SET utf8mb4 COLLATE utf8mb4_general_ci")
	t.Cleanup(func() { exec(t, admin, "DROP DATABASE "+name) })
	parsed, err := mysqldsn.ParseDSN(dsn)
	if err != nil {
		t.Fatal("无法解析 MySQL 测试连接配置")
	}
	parsed.DBName = name
	db, err := gorm.Open(mysql.Open(parsed.FormatDSN()), config)
	if err != nil {
		t.Fatal("无法连接隔离的 MySQL 测试库")
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return db
}
