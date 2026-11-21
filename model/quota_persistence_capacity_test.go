package model

import (
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var quotaCapacityModels = []struct {
	value  any
	fields []string
}{
	{&User{}, []string{"quota", "used_quota", "request_count", "aff_quota", "aff_history"}},
	{&Token{}, []string{"remain_quota", "used_quota"}},
	{&Channel{}, []string{"used_quota"}},
	{&Order{}, []string{"quota"}},
}

func openQuotaCapacityTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "capacity.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return db
}
func TestQuotaPersistenceDialectTypesOn64BitBuilds(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("capacity audit covers supported amd64/arm64 builds")
	}
	local := openQuotaCapacityTestDB(t)
	pool, err := local.DB()
	if err != nil {
		t.Fatal(err)
	}
	// Connections stay local: dialect metadata only, no MySQL/PostgreSQL server calls.
	my, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	pg, err := gorm.Open(postgres.New(postgres.Config{Conn: pool}), &gorm.Config{DisableAutomaticPing: true})
	if err != nil {
		t.Fatal(err)
	}
	dialects := []struct {
		name string
		db   *gorm.DB
		want string
	}{{"mysql", my, "bigint"}, {"postgres", pg, "bigint"}, {"sqlite", local, "integer"}}
	for _, item := range quotaCapacityModels {
		parsed, err := schema.Parse(item.value, &sync.Map{}, schema.NamingStrategy{})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range item.fields {
			field := parsed.LookUpField(name)
			if field == nil {
				t.Fatal(name)
			}
			for _, dialect := range dialects {
				actual := dialect.db.Migrator().FullDataTypeOf(field).SQL
				t.Logf("%s %s.%s: %s (Go %s, %d bits)", dialect.name, parsed.Table, name, actual, field.FieldType, field.Size)
				if !strings.HasPrefix(strings.ToLower(actual), dialect.want) {
					t.Fatalf("%s %s.%s generates %s; expected %s", dialect.name, parsed.Table, name, actual, dialect.want)
				}
			}
		}
	}
}
func TestQuotaPersistenceSQLiteLargeValuesAndIncrement(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("capacity audit covers supported 64-bit builds")
	}
	db := openQuotaCapacityTestDB(t)
	for _, item := range quotaCapacityModels {
		if err := db.AutoMigrate(item.value); err != nil {
			t.Fatal(err)
		}
	}
	for _, value64 := range []int64{1 << 31, 1 << 40, (1 << 63) - 1 - 8} {
		value := int(value64)
		user := &User{Username: "capacity-" + strconv.Itoa(value), AccessToken: "capacity-" + strconv.Itoa(value), AffCode: "capacity-" + strconv.Itoa(value), Quota: value, UsedQuota: value, RequestCount: value, AffQuota: value, AffHistoryQuota: value}
		token := &Token{UserId: 1, Key: "capacity-" + strconv.Itoa(value), RemainQuota: value, UsedQuota: value}
		channel := &Channel{UsedQuota: int64(value)}
		order := &Order{UserId: value, TradeNo: "capacity-" + strconv.Itoa(value), RequestKey: "capacity", Quota: value, PaymentState: "unpaid"}
		for _, row := range []any{user, token, channel, order} {
			if err := db.Session(&gorm.Session{SkipHooks: true}).Create(row).Error; err != nil {
				t.Fatal(err)
			}
		}
		rows := []struct {
			item   any
			id     int
			fields []string
		}{{user, user.Id, quotaCapacityModels[0].fields}, {token, token.Id, quotaCapacityModels[1].fields}, {channel, channel.Id, quotaCapacityModels[2].fields}, {order, order.ID, quotaCapacityModels[3].fields}}
		for _, row := range rows {
			columns, err := db.Migrator().ColumnTypes(row.item)
			if err != nil {
				t.Fatal(err)
			}
			actualTypes := map[string]string{}
			for _, column := range columns {
				actualTypes[column.Name()] = column.DatabaseTypeName()
			}
			for _, field := range row.fields {
				if strings.ToLower(actualTypes[field]) != "integer" {
					t.Fatalf("actual SQLite column %s=%s", field, actualTypes[field])
				}
				var got int64
				if err := db.Model(row.item).Select(field).Where("id = ?", row.id).Scan(&got).Error; err != nil || got != int64(value) {
					t.Fatalf("%s before=%d want=%d err=%v", field, got, value, err)
				}
				if err := db.Model(row.item).Where("id = ?", row.id).UpdateColumn(field, gorm.Expr(field+" + ?", 4)).Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Model(row.item).Select(field).Where("id = ?", row.id).Scan(&got).Error; err != nil || got != int64(value)+4 {
					t.Fatalf("%s after=%d want=%d err=%v", field, got, int64(value)+4, err)
				}
			}
		}
	}
}
