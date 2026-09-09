package model

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common"
	"one-api/common/config"
)

func execStartupFixture(t *testing.T, db *gorm.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func TestStartupMigrationsRunThroughInitDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "startup.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.AutoMigrate(&User{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&User{Id: 1, Username: "existing-root", Password: "fixture", Quota: 100, UsedQuota: 17}).Error; err != nil {
		t.Fatal(err)
	}
	execStartupFixture(t, db,
		"CREATE TABLE tasks (id INTEGER PRIMARY KEY, created_at bigint, task_id text, platform text, user_id integer, channel_id integer, status text, quota bigint)",
		"INSERT INTO tasks VALUES (1,123,'suno-1','suno',1,2,'SUCCESS',17)",
		"CREATE TABLE midjourneys (id INTEGER PRIMARY KEY, mj_id text, user_id integer, channel_id integer, status text, progress text, quota integer, submit_time bigint)",
		"INSERT INTO midjourneys VALUES (1,'mj-1',1,2,'SUCCESS','100%',19,123000)",
		"CREATE TABLE prices (model text, type text, input real, output real)",
		"INSERT INTO prices VALUES ('same','tokens',1,2)",
		"INSERT INTO prices VALUES ('same','tokens',1,2)",
		"CREATE TABLE abilities (channel_id integer)",
		"CREATE TABLE responses_ws_settlement_intents (id integer)",
	)
	previousDB, previousMaster := DB, config.IsMasterNode
	previousSQLite, previousPostgres := common.UsingSQLite, common.UsingPostgreSQL
	viper.Reset()
	viper.Set("sqlite_path", path)
	config.IsMasterNode = true
	t.Cleanup(func() {
		if DB != nil && DB != previousDB {
			if pool, err := DB.DB(); err == nil {
				_ = pool.Close()
			}
		}
		DB, config.IsMasterNode = previousDB, previousMaster
		common.UsingSQLite, common.UsingPostgreSQL = previousSQLite, previousPostgres
		viper.Reset()
	})
	config.IsMasterNode = false
	if err := InitDB(); err == nil {
		t.Fatal("从节点应拒绝未迁移的数据库")
	}
	if DB.Migrator().HasColumn("tasks", "charged_quota") || DB.Migrator().HasTable("migrations") {
		t.Fatal("从节点执行了数据迁移")
	}
	if pool, err := DB.DB(); err == nil {
		_ = pool.Close()
	}
	config.IsMasterNode = true
	for i := 0; i < 2; i++ {
		if i > 0 {
			if pool, err := DB.DB(); err == nil {
				_ = pool.Close()
			}
		}
		if err := InitDB(); err != nil {
			t.Fatalf("第 %d 次启动迁移: %v", i+1, err)
		}
		if !DB.Config.PrepareStmt {
			t.Fatal("迁移完成后应恢复预编译缓存")
		}
		if err := CheckPaymentOrderSchema(DB); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePaymentOrderData(DB); err != nil {
			t.Fatal(err)
		}
		if err := CheckTaskOwnerSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"midjourneys", "abilities", "responses_ws_settlement_intents"} {
			if DB.Migrator().HasTable(table) {
				t.Fatalf("启动迁移后仍保留旧表 %s", table)
			}
		}
	}
	var taskCount, priceCount int64
	if err := DB.Model(&Task{}).Count(&taskCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := DB.Model(&Price{}).Count(&priceCount).Error; err != nil {
		t.Fatal(err)
	}
	if taskCount != 2 || priceCount != 1 {
		t.Fatalf("启动重复导入或丢失数据: tasks=%d prices=%d", taskCount, priceCount)
	}
	var user User
	if err := DB.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100 || user.UsedQuota != 17 {
		t.Fatal("启动改变了历史余额")
	}
}

func TestStartupTokenLimitsMigrationRetriesSkippedRows(t *testing.T) {
	db := paymentSchemaDB(t, false)
	execStartupFixture(t, db, "CREATE TABLE tokens (id INTEGER PRIMARY KEY, name text, setting text)", `INSERT INTO tokens VALUES (1,'token','{"limits":{"enabled":true,"models":["test-model"]},"unrelated":42}')`, `INSERT INTO tokens VALUES (2,'bad','{broken')`)
	if err := retryTokenLimitsMigration().Migrate(db); err == nil || !strings.Contains(err.Error(), "记录 2") {
		t.Fatalf("错误记录被跳过: %v", err)
	}
	execStartupFixture(t, db, `UPDATE tokens SET setting='{}' WHERE id=2`)
	if err := retryTokenLimitsMigration().Migrate(db); err != nil {
		t.Fatal(err)
	}
	var setting string
	if err := db.Table("tokens").Where("id=1").Select("setting").Scan(&setting).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(setting, "limit_model_setting") || !strings.Contains(setting, `"unrelated":42`) {
		t.Fatalf("配置转换丢失内容: %s", setting)
	}
}

func TestStartupPaymentObservationColumnsAreAutomatic(t *testing.T) {
	db := paymentSchemaDB(t, true)
	insertPaymentSchemaFacts(t, db)
	for column := range paymentOrderObservationColumns {
		if err := db.Migrator().DropColumn(&Order{}, column); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidatePaymentUpgradePrerequisites(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Order{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPaymentOrderSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaymentOrderData(db); err != nil {
		t.Fatal(err)
	}
}

func TestStartupTaskMigrationPreservesFactsAndDoesNotSettle(t *testing.T) {
	db := paymentSchemaDB(t, false)
	execStartupFixture(t, db,
		"CREATE TABLE tasks (id INTEGER PRIMARY KEY, created_at bigint, task_id varchar(50), platform varchar(30), user_id integer, channel_id integer, token_id integer, status varchar(20), quota bigint, properties text)",
		"INSERT INTO tasks VALUES (1,123,'upstream-1','suno',1,2,3,'SUCCESS',17,NULL)",
		`INSERT INTO tasks VALUES (2,123,'upstream-2','kling',1,2,3,'FAILURE',0,'{"status":"rolled_back","envelope":{"command":{"user_id":1,"channel_id":2,"token_id":3,"final_quota":0}}}')`,
		"CREATE TABLE users (id INTEGER PRIMARY KEY, quota bigint, used_quota bigint)",
		"INSERT INTO users VALUES (1,100,17)",
	)
	for i := 0; i < 2; i++ {
		if err := migrateHistoricalTasks().Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := consolidatePersistenceProjections().Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Task{}); err != nil {
		t.Fatal(err)
	}
	var tasks []Task
	if err := db.Order("id").Find(&tasks).Error; err != nil {
		t.Fatal(err)
	}
	for index, quota := range []int64{17, 0} {
		task := tasks[index]
		if task.OwnerID == "" || task.ProviderState != TaskProviderStateClosed || task.ChargedQuota == nil || *task.ChargedQuota != quota || task.NextActionAt != 0 {
			t.Fatalf("历史任务事实丢失: %+v", task)
		}
	}
	old := DB
	DB = db
	t.Cleanup(func() { DB = old })
	if err := CheckTaskOwnerSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 已迁移终态再次结算不会触及余额。
	if _, err := FinalizeTaskBillingOwner(context.Background(), &tasks[0], 0, "cancel"); err != nil {
		t.Fatal(err)
	}
	var user struct{ Quota, UsedQuota int64 }
	if err := db.Table("users").Take(&user).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 100 || user.UsedQuota != 17 {
		t.Fatalf("迁移重放了资金操作: %+v", user)
	}
}

func TestStartupTaskMigrationRejectsAmbiguousRowsAtomically(t *testing.T) {
	for _, state := range []string{"IN_PROGRESS", "FAILURE", "UNKNOWN"} {
		t.Run(state, func(t *testing.T) {
			db := paymentSchemaDB(t, false)
			execStartupFixture(t, db,
				"CREATE TABLE tasks (id INTEGER PRIMARY KEY, created_at bigint, task_id text, platform text, user_id integer, channel_id integer, status text, quota bigint)",
				"INSERT INTO tasks VALUES (1,123,'upstream-1','suno',1,2,'SUCCESS',17)",
				fmt.Sprintf("INSERT INTO tasks VALUES (2,123,'upstream-2','suno',1,2,'%s',17)", state),
			)
			if err := migrateHistoricalTasks().Migrate(db); err == nil || !strings.Contains(err.Error(), "记录 2") {
				t.Fatalf("未定位歧义旧任务: %v", err)
			}
			var changed int64
			if err := db.Table("tasks").Where("owner_id IS NOT NULL").Count(&changed).Error; err != nil || changed != 0 {
				t.Fatalf("失败未回滚数据: changed=%d err=%v", changed, err)
			}
		})
	}
}

func TestStartupMidjourneyMigrationIsAtomicAndResumable(t *testing.T) {
	db := paymentSchemaDB(t, false)
	if err := db.AutoMigrate(&Task{}); err != nil {
		t.Fatal(err)
	}
	execStartupFixture(t, db,
		"CREATE TABLE midjourneys (id INTEGER PRIMARY KEY, mj_id text, user_id integer, channel_id integer, token_id integer, status text, progress text, quota integer, submit_time bigint, prompt text, image_url text)",
		"INSERT INTO midjourneys VALUES (1,'mj-1',1,2,3,'SUCCESS','100%',17,123000,'original prompt','https://example.com/image')",
		"INSERT INTO midjourneys VALUES (2,'mj-2',1,2,3,'IN_PROGRESS','50%',17,123000,'second prompt','')",
	)
	if err := migrateHistoricalMidjourney().Migrate(db); err == nil {
		t.Fatal("在途任务不应被猜成已结算")
	}
	var count int64
	if err := db.Model(&Task{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("未回滚 MJ 迁移: %d %v", count, err)
	}
	execStartupFixture(t, db, "UPDATE midjourneys SET status='SUCCESS',progress='100%' WHERE id=2")
	for i := 0; i < 2; i++ {
		if err := migrateHistoricalMidjourney().Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	var tasks []Task
	if err := db.Order("id").Find(&tasks).Error; err != nil || len(tasks) != 2 {
		t.Fatalf("历史 MJ 丢失或重复: %d %v", len(tasks), err)
	}
	view := MidjourneyFromTask(&tasks[0])
	if view.MjId != "mj-1" || view.Prompt != "original prompt" || view.ImageUrl != "https://example.com/image" || view.Quota != 17 || view.SubmitTime != 123000 || tasks[0].CreatedAt != 123 {
		t.Fatalf("MJ 展示事实改变: %+v", view)
	}
	if !db.Migrator().HasTable("midjourneys") {
		t.Fatal("导入阶段应保留旧表，等最终清理阶段核对后再删除")
	}
}

func TestStartupPriceMigrationMergesOnlyIdenticalRows(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			db := paymentSchemaDB(t, false)
			execStartupFixture(t, db, "CREATE TABLE prices (model text, type text, input real, output real, legacy_note text)",
				"INSERT INTO prices VALUES ('same','tokens',1,2,'keep')", "INSERT INTO prices VALUES ('same','tokens',1,2,'keep')")
			if conflict {
				execStartupFixture(t, db, "INSERT INTO prices VALUES ('same','tokens',3,2,'keep')")
			}
			err := migrateIdenticalPrices().Migrate(db)
			if conflict {
				if err == nil {
					t.Fatal("不应选择冲突价格")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := migrateIdenticalPrices().Migrate(db); err != nil {
				t.Fatal(err)
			}
			var rows []map[string]any
			if err := db.Table("prices").Find(&rows).Error; err != nil || len(rows) != 1 || rows[0]["legacy_note"] != "keep" {
				t.Fatalf("价格或未知字段丢失: %+v %v", rows, err)
			}
			if err := db.AutoMigrate(&Price{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStartupPaymentRepresentationMigration(t *testing.T) {
	for _, amount := range []string{"0.29", "0.291", "92233720368547758.08"} {
		t.Run(amount, func(t *testing.T) {
			db := paymentSchemaDB(t, false)
			execStartupFixture(t, db, "CREATE TABLE orders (id INTEGER PRIMARY KEY, order_amount text, order_currency text, gateway_no text)", fmt.Sprintf("INSERT INTO orders VALUES (1,'%s','CNY','upstream-transaction')", amount))
			err := migrateHistoricalPaymentRepresentations().Migrate(db)
			if amount != "0.29" {
				if err == nil {
					t.Fatal("不可无损转换的金额被接受")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := migrateHistoricalPaymentRepresentations().Migrate(db); err != nil {
				t.Fatal(err)
			}
			var order struct {
				ExpectedAmountMinor   int64
				ProviderTransactionID string
			}
			if err := db.Table("orders").Take(&order).Error; err != nil {
				t.Fatal(err)
			}
			if order.ExpectedAmountMinor != 29 || order.ProviderTransactionID != "upstream-transaction" {
				t.Fatalf("付款事实丢失: %+v", order)
			}
			if err := ValidatePaymentUpgradePrerequisites(db); err == nil {
				t.Fatal("表示转换不能冒充缺失的商户身份")
			}
		})
	}
}

func TestStartupTaskMigrationChecksLaterBatchesAndResumes(t *testing.T) {
	db := paymentSchemaDB(t, false)
	execStartupFixture(t, db, "CREATE TABLE tasks (id INTEGER PRIMARY KEY, created_at bigint, task_id text, platform text, user_id integer, channel_id integer, status text, quota bigint)")
	for id := 1; id <= 205; id++ {
		if err := db.Exec("INSERT INTO tasks VALUES (?,123,?,'suno',1,2,'SUCCESS',17)", id, fmt.Sprintf("task-%d", id)).Error; err != nil {
			t.Fatal(err)
		}
	}
	execStartupFixture(t, db, "UPDATE tasks SET status='IN_PROGRESS' WHERE id=205")
	if err := migrateHistoricalTasks().Migrate(db); err == nil || !strings.Contains(err.Error(), "记录 205") {
		t.Fatalf("漏检后续批次: %v", err)
	}
	var count int64
	if err := db.Table("tasks").Where("owner_id IS NOT NULL").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("未回滚之前批次: %d %v", count, err)
	}
	execStartupFixture(t, db, "UPDATE tasks SET status='SUCCESS' WHERE id=205")
	if err := migrateHistoricalTasks().Migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("tasks").Where("owner_id IS NOT NULL AND charged_quota=17").Count(&count).Error; err != nil || count != 205 {
		t.Fatalf("未完整恢复迁移: %d %v", count, err)
	}
}

func TestStartupTaskMigrationRejectsReservedSettlement(t *testing.T) {
	quota := int64(17)
	row := startupLegacyTask{Task: Task{Status: TaskStatusSuccess, UserId: 1, ChannelId: 2, TokenID: 3}, Quota: &quota}
	row.Properties = []byte(`{"status":"reserved","envelope":{"command":{"user_id":1,"channel_id":2,"token_id":3,"final_quota":17}}}`)
	if _, err := historicalTaskCharge(row); err == nil {
		t.Fatal("上游成功不能替代本地结算证据")
	}
	row.Properties = []byte(`{"status":"committed","envelope":{"command":{"user_id":99,"channel_id":2,"token_id":3,"final_quota":17}}}`)
	if _, err := historicalTaskCharge(row); err == nil {
		t.Fatal("不能继承另一用户的结算证据")
	}
}

func TestStartupMigrationsDatabaseDialects(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		legacy := struct {
			ID        int64 `gorm:"primaryKey;autoIncrement"`
			CreatedAt int64
			TaskID    string `gorm:"size:50"`
			Platform  string `gorm:"size:30"`
			UserId    int
			ChannelId int
			Status    string `gorm:"size:20"`
			Quota     int64
		}{CreatedAt: 123, TaskID: "old-task", Platform: "suno", UserId: 1, ChannelId: 2, Status: "SUCCESS", Quota: 17}
		if err := db.Table("tasks").AutoMigrate(&legacy); err != nil {
			t.Fatal(err)
		}
		if err := db.Table("tasks").Create(&legacy).Error; err != nil {
			t.Fatal(err)
		}
		if err := migrateHistoricalTasks().Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := consolidatePersistenceProjections().Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&Task{}); err != nil {
			t.Fatal(err)
		}
		var task Task
		if err := db.First(&task).Error; err != nil {
			t.Fatal(err)
		}
		if task.ChargedQuota == nil || *task.ChargedQuota != 17 || task.ProviderState != TaskProviderStateClosed {
			t.Fatalf("历史任务转换错误: %+v", task)
		}
	})
}
