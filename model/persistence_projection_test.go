package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"
	"one-api/common"
	paytypes "one-api/payment/types"
)

func useProjectionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := paymentSchemaDB(t, true)
	if err := db.AutoMigrate(&Task{}); err != nil {
		t.Fatal(err)
	}
	previous := DB
	DB = db
	t.Cleanup(func() { DB = previous })
	return db
}

func addOldProjectionColumns(t *testing.T, db *gorm.DB) {
	t.Helper()
	for _, sql := range []string{
		"ALTER TABLE orders ADD COLUMN order_amount decimal(10,2)",
		"ALTER TABLE orders ADD COLUMN gateway_no varchar(100)",
		"ALTER TABLE orders ADD COLUMN status varchar(32)",
		"ALTER TABLE orders ADD COLUMN currency_exponent integer NOT NULL DEFAULT 2",
		"ALTER TABLE payments ADD COLUMN protocol_profile varchar(64) NOT NULL DEFAULT ''",
		"ALTER TABLE tasks ADD COLUMN prepared_at bigint",
		"ALTER TABLE tasks ADD COLUMN quota bigint",
	} {
		if err := db.Exec(sql).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func seedOldProjections(t *testing.T, db *gorm.DB) Order {
	t.Helper()
	order := insertPaymentSchemaFacts(t, db)
	addOldProjectionColumns(t, db)
	if err := db.Table("orders").Where("id = ?", order.ID).Updates(map[string]any{"order_amount": "0.29", "gateway_no": "", "status": "pending"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("payments").Where("id = ?", order.GatewayId).Update("protocol_profile", order.Identity.ProtocolProfile).Error; err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	task := Task{OwnerID: "projection-task", CreatedAt: 123, ProviderNamespace: "task-platform:suno", ProviderTaskScopeIncarnation: "provider-wide", Platform: TaskPlatformSuno, UserId: 1, RequestFingerprint: "fixture", ChargedQuota: &zero, ProviderState: TaskProviderStateClosed}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("tasks").Where("id = ?", task.ID).Updates(map[string]any{"prepared_at": 123, "quota": 0}).Error; err != nil {
		t.Fatal(err)
	}
	return order
}

func TestPersistenceProjectionMigrationPreservesFactsAndCanResume(t *testing.T) {
	db := useProjectionDB(t)
	order := seedOldProjections(t, db)
	// 模拟一个 DDL 已完成后进程退出。
	if err := db.Exec("ALTER TABLE orders DROP COLUMN gateway_no").Error; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := consolidatePersistenceProjections().Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.AutoMigrate(&Payment{}, &Order{}, &Task{}); err != nil {
		t.Fatal(err)
	}
	if err := CheckPaymentOrderSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := CheckTaskOwnerSchema(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePaymentOrderData(db); err != nil {
		t.Fatal(err)
	}
	var saved Order
	if err := db.First(&saved, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if saved.ExpectedAmountMinor != 29 || saved.Quota != 100 || !saved.Identity.Equal(order.Identity) || saved.PaymentState != paytypes.PaymentUnconfirmed {
		t.Fatalf("迁移改变了资金事实: %+v", saved)
	}
	// 旧 NOT NULL 列移除后，新 writer 无需为旧列补值。
	saved.ID = 0
	saved.TradeNo = "after-migration"
	saved.RequestKey = "after-migration"
	if err := db.Create(&saved).Error; err != nil {
		t.Fatal(err)
	}
	var binding Payment
	if err := db.First(&binding, order.GatewayId).Error; err != nil {
		t.Fatal(err)
	}
	binding.ID = 0
	binding.UUID = "after-migration"
	if err := db.Create(&binding).Error; err != nil {
		t.Fatal(err)
	}
}

func TestPersistenceProjectionDatabaseDialects(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		if err := db.AutoMigrate(&User{}, &Payment{}, &Order{}, &Redemption{}, &Task{}); err != nil {
			t.Fatal(err)
		}
		previous := DB
		DB = db
		t.Cleanup(func() { DB = previous })
		order := seedOldProjections(t, db)
		if err := consolidatePersistenceProjections().Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := CheckPaymentOrderSchema(db); err != nil {
			t.Fatal(err)
		}
		if err := CheckTaskOwnerSchema(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := ValidatePaymentOrderData(db); err != nil {
			t.Fatal(err)
		}
		result, err := GetOrderList(&SearchOrderParams{Status: "pending", PaginationParams: PaginationParams{Order: "status"}})
		if err != nil || result.TotalCount != 1 || (*result.Data)[0].ID != order.ID {
			t.Fatalf("迁移后查询: %+v %v", result, err)
		}
	})
}

func TestPersistenceProjectionMigrationRejectsConflictsBeforeDDL(t *testing.T) {
	for _, test := range []struct {
		table, column string
		value         any
	}{
		{"orders", "order_amount", "0.30"}, {"orders", "gateway_no", "unconfirmed-transaction"},
		{"orders", "status", "success"}, {"orders", "currency_exponent", 3},
		{"payments", "protocol_profile", "another-profile"}, {"tasks", "prepared_at", 124}, {"tasks", "quota", 1},
	} {
		t.Run(test.table+"_"+test.column, func(t *testing.T) {
			db := useProjectionDB(t)
			seedOldProjections(t, db)
			if err := db.Table(test.table).Where("id = 1").Update(test.column, test.value).Error; err != nil {
				t.Fatal(err)
			}
			// 软删除的支付记录也必须核对。
			if test.table == "payments" {
				if err := db.Delete(&Payment{}, 1).Error; err != nil {
					t.Fatal(err)
				}
			}
			err := consolidatePersistenceProjections().Migrate(db)
			if err == nil || !strings.Contains(err.Error(), test.column) {
				t.Fatalf("未拒绝冲突: %v", err)
			}
			for _, column := range orderProjectionColumns {
				if !db.Migrator().HasColumn("orders", column) {
					t.Fatalf("核对完成前删除了 %s", column)
				}
			}
		})
	}
}

func TestPersistenceProjectionMigrationChecksEveryBatch(t *testing.T) {
	db := useProjectionDB(t)
	order := seedOldProjections(t, db)
	for i := 2; i <= 201; i++ {
		order.ID = i
		order.TradeNo = time.Unix(int64(i), 0).String()
		order.RequestKey = order.TradeNo
		if err := db.Create(&order).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Table("orders").Where("id = 201").Update("status", "success").Error; err != nil {
		t.Fatal(err)
	}
	if err := consolidatePersistenceProjections().Migrate(db); err == nil || !strings.Contains(err.Error(), "201") {
		t.Fatalf("漏过后续批次: %v", err)
	}
}

func TestPersistenceProjectionMigrationDoesNotInventLegacyFacts(t *testing.T) {
	for _, populated := range []bool{false, true} {
		db := paymentSchemaDB(t, false)
		if err := db.Exec("CREATE TABLE tasks (id INTEGER PRIMARY KEY, quota bigint)").Error; err != nil {
			t.Fatal(err)
		}
		if populated {
			if err := db.Exec("INSERT INTO tasks (id,quota) VALUES (1,100)").Error; err != nil {
				t.Fatal(err)
			}
		}
		err := consolidatePersistenceProjections().Migrate(db)
		if populated {
			if err == nil || !db.Migrator().HasColumn("tasks", "quota") {
				t.Fatalf("丢弃了缺少新资金事实的历史字段: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
}

func TestOrderProjectionsDriveAPIQueryAndStatistics(t *testing.T) {
	db := useProjectionDB(t)
	order := insertPaymentSchemaFacts(t, db)
	order.CreatedAt = 1700000000
	if err := db.Model(&order).Update("created_at", order.CreatedAt).Error; err != nil {
		t.Fatal(err)
	}
	paid := order
	paid.ID = 2
	paid.TradeNo, paid.RequestKey = "paid", "paid"
	paid.PaymentState = paytypes.PaymentPaid
	transaction := "provider-tx"
	paid.ProviderTransactionID = &transaction
	paid.ConfirmedAmountMinor = &paid.ExpectedAmountMinor
	paid.ConfirmedCurrency, paid.EvidenceSummary = "CNY", "verified"
	if err := db.Create(&paid).Error; err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(paid)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["status"] != "success" || wire["gateway_no"] != transaction || wire["currency_exponent"] != float64(2) || wire["order_amount"] != 0.29 {
		t.Fatalf("API 投影错误: %s", raw)
	}
	for _, test := range []struct {
		status, gateway, order string
		wantID                 int
		count                  int64
	}{
		{"success", transaction, "status,-id", 2, 1}, {"pending", "", "-status,id", 1, 1},
		{"failed", "", "", 0, 0}, {"", "", "status,-id", 1, 2}, {"", "", "-status,id", 2, 2},
	} {
		result, err := GetOrderList(&SearchOrderParams{Status: test.status, GatewayNo: test.gateway, PaginationParams: PaginationParams{Order: test.order}})
		if err != nil {
			t.Fatal(err)
		}
		if result.TotalCount != test.count || (test.wantID != 0 && (*result.Data)[0].ID != test.wantID) {
			t.Fatalf("筛选/排序不一致: %+v %+v", test, result)
		}
	}
	stats, err := GetStatisticsOrder()
	if err != nil || len(stats) != 1 || stats[0].Money != 0.29 || stats[0].Quota != 100 {
		t.Fatalf("统计错误: %+v %v", stats, err)
	}
	wasSQLite, wasPostgres := common.UsingSQLite, common.UsingPostgreSQL
	common.UsingSQLite, common.UsingPostgreSQL = true, false
	t.Cleanup(func() { common.UsingSQLite, common.UsingPostgreSQL = wasSQLite, wasPostgres })
	days, err := GetStatisticsOrderByPeriod(int64(order.CreatedAt)-1, int64(order.CreatedAt)+1)
	if err != nil || len(days) != 1 || days[0].OrderAmount != 0.29 {
		t.Fatalf("按日统计错误: %+v %v", days, err)
	}
}

func TestTaskQuotaProjectionPreservesLookupOmission(t *testing.T) {
	db := useProjectionDB(t)
	charged := int64(37)
	taskID := "upstream-id"
	task := Task{OwnerID: "charged-task", Platform: TaskPlatformSuno, UserId: 1, TaskID: &taskID, ChargedQuota: &charged, ProviderNamespace: "suno", ProviderTaskScopeIncarnation: "scope", RequestFingerprint: "fixture"}
	if err := db.Create(&task).Error; err != nil {
		t.Fatal(err)
	}
	if MidjourneyFromTask(&task).Quota != 37 {
		t.Fatal("额度展示未从最终额度派生")
	}
	tasks, err := GetTaskByTaskIds(TaskPlatformSuno, 1, []string{taskID})
	if err != nil || len(tasks) != 1 {
		t.Fatalf("任务查询: %v", err)
	}
	raw, err := json.Marshal(tasks[0])
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["quota"] != float64(0) {
		t.Fatalf("原省略额度的查询暴露了最终额度: %s", raw)
	}
}

func TestPaymentProtocolProfileProjection(t *testing.T) {
	p := Payment{Identity: paytypes.GatewayIdentity{ProtocolProfile: "protocol-v1"}}
	raw, err := json.Marshal(struct {
		PaymentView
		Products []string `json:"products"`
	}{p.View(), []string{"checkout"}})
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire["protocol_profile"] != "protocol-v1" || p.Snapshot().Identity.ProtocolProfile != "protocol-v1" || wire["products"] == nil {
		t.Fatalf("协议来源不一致: %s", raw)
	}
}

func TestProjectionMigrationUnquotedSQLiteColumns(t *testing.T) {
	db := paymentSchemaDB(t, false)
	if err := db.Exec("CREATE TABLE tasks (id INTEGER PRIMARY KEY,created_at bigint,charged_quota bigint,reserved_quota bigint,quota bigint)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO tasks VALUES (1,123,0,0,0)").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := consolidatePersistenceProjections().Migrate(db); err != nil {
			t.Fatal(err)
		}
		if err := requireProjectionColumnsRemoved(db, "tasks", taskProjectionColumns); err != nil {
			t.Fatal(err)
		}
	}
	names, err := databaseColumnNames(db, "tasks")
	if err != nil || names["quota"] || !names["charged_quota"] || !names["reserved_quota"] {
		t.Fatalf("列集合错误: %v %v", names, err)
	}
}

func TestProjectionMigrationSQLiteIdentifierCase(t *testing.T) {
	for _, table := range []string{"tasks", "TASKS", "TaSkS"} {
		for _, conflict := range []bool{false, true} {
			name := table + "/matching"
			if conflict {
				name = table + "/conflicting"
			}
			t.Run(name, func(t *testing.T) {
				db := paymentSchemaDB(t, false)
				if err := db.Exec("CREATE TABLE " + table + " (ID INTEGER PRIMARY KEY, CREATED_AT bigint, CHARGED_QUOTA bigint, RESERVED_QUOTA bigint, QUOTA bigint NOT NULL)").Error; err != nil {
					t.Fatal(err)
				}
				quota := 0
				if conflict {
					quota = 1
				}
				if err := db.Exec("INSERT INTO tasks VALUES (1,123,0,0,?)", quota).Error; err != nil {
					t.Fatal(err)
				}
				if err := requireProjectionColumnsRemoved(db, "tasks", taskProjectionColumns); err == nil {
					t.Fatal("漏检大写派生列")
				}
				err := consolidatePersistenceProjections().Migrate(db)
				if conflict {
					if err == nil || !strings.Contains(err.Error(), "quota") {
						t.Fatalf("大写字段的冲突未阻止删列: %v", err)
					}
					var preserved int
					if err := db.Raw("SELECT QUOTA FROM tasks WHERE id = 1").Scan(&preserved).Error; err != nil || preserved != 1 {
						t.Fatalf("冲突历史字段被丢弃: %d %v", preserved, err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := consolidatePersistenceProjections().Migrate(db); err != nil {
					t.Fatalf("迁移不能重跑: %v", err)
				}
				if err := requireProjectionColumnsRemoved(db, "tasks", taskProjectionColumns); err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("INSERT INTO tasks (id,created_at,charged_quota,reserved_quota) VALUES (2,123,0,0)").Error; err != nil {
					t.Fatalf("新 writer 仍被旧列约束阻断: %v", err)
				}
			})
		}
	}
}

func TestProjectionMigrationSQLiteUppercasePaymentColumns(t *testing.T) {
	for _, field := range []struct {
		table, column string
		conflict      any
	}{
		{"orders", "order_amount", "0.30"},
		{"orders", "currency_exponent", 3},
		{"orders", "status", "success"},
		{"payments", "protocol_profile", "wrong-profile"},
	} {
		for _, conflict := range []bool{false, true} {
			name := field.column + "/matching"
			if conflict {
				name = field.column + "/conflicting"
			}
			t.Run(name, func(t *testing.T) {
				db := useProjectionDB(t)
				seedOldProjections(t, db)
				for _, column := range []string{field.column, "id"} {
					if err := db.Exec("ALTER TABLE " + field.table + " RENAME COLUMN " + column + " TO " + strings.ToUpper(column)).Error; err != nil {
						t.Fatal(err)
					}
				}
				if field.table == "payments" {
					if err := db.Exec("ALTER TABLE payments RENAME COLUMN identity TO IDENTITY").Error; err != nil {
						t.Fatal(err)
					}
				}
				if conflict {
					if err := db.Table(field.table).Where("id = 1").Update(field.column, field.conflict).Error; err != nil {
						t.Fatal(err)
					}
				}
				err := consolidatePersistenceProjections().Migrate(db)
				if conflict {
					if err == nil || !strings.Contains(err.Error(), field.column) {
						t.Fatalf("大写列冲突被漏过: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := consolidatePersistenceProjections().Migrate(db); err != nil {
					t.Fatal(err)
				}
				if err := CheckPaymentOrderSchema(db); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestProjectionColumnInspectionPropagatesDatabaseErrors(t *testing.T) {
	db := paymentSchemaDB(t, false)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	if err := requireProjectionColumnsRemoved(db, "tasks", taskProjectionColumns); err == nil {
		t.Fatal("数据库错误被当成列已删除")
	}
	if err := consolidatePersistenceProjections().Migrate(db); err == nil {
		t.Fatal("数据库错误被当成迁移成功")
	}
}

func TestClosedOrderMigrationPreservesLatePaymentAndPreventsPreparation(t *testing.T) {
	db := useProjectionDB(t)
	order := seedOldProjections(t, db)
	if err := db.Table("orders").Where("id = ?", order.ID).Update("status", "closed").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := consolidatePersistenceProjections().Migrate(db); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.First(&order, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if order.WindowState != paytypes.WindowLocalExpired || order.PaymentState != paytypes.PaymentUnconfirmed || order.PublicStatus() != OrderStatusClosed {
		t.Fatalf("历史关闭被误认为上游关闭或付款: %+v", order)
	}
	now := time.Now()
	if owned, err := ClaimOrderPreparation(context.Background(), order.ID, "must-not-prepare", now, now.Add(time.Minute), 1); err != nil || owned {
		t.Fatalf("关闭原单仍可发起准备: %v %v", owned, err)
	}
	for _, status := range []string{"closed", "pending"} {
		result, err := GetOrderList(&SearchOrderParams{Status: status, PaginationParams: PaginationParams{Order: "-status,id"}})
		if err != nil {
			t.Fatal(err)
		}
		want := int64(0)
		if status == "closed" {
			want = 1
		}
		if result.TotalCount != want {
			t.Fatalf("%s 筛选得到 %d 条", status, result.TotalCount)
		}
	}
	if err := db.AutoMigrate(&UserGroup{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&User{Id: 1, Username: "late-paid", Password: "password123", AccessToken: "late-paid", Quota: 10, Group: "default", Status: 1}).Error; err != nil {
		t.Fatal(err)
	}
	total := order.Money()
	evidence := paytypes.PaymentObservation{Source: paytypes.SourceVerifiedCallback, GatewayID: order.GatewayId, Identity: order.Identity, TransactionNamespace: order.TransactionNamespace, TradeNo: order.TradeNo, State: paytypes.ObservationSucceeded, ProviderTransactionID: "late-tx", OrderTotal: &total, VerificationRef: "verified"}
	for i := range 2 {
		paid, newly, err := CompleteOrderPayment(context.Background(), evidence)
		if err != nil || newly != (i == 0) || paid.PublicStatus() != OrderStatusSuccess {
			t.Fatalf("迟到支付: %+v %v %v", paid, newly, err)
		}
	}
	var user User
	if err := db.First(&user, 1).Error; err != nil {
		t.Fatal(err)
	}
	if user.Quota != 110 {
		t.Fatalf("重复入账: %d", user.Quota)
	}
}

func TestClosedOrderMappingWaitsForAllPreflightChecks(t *testing.T) {
	db := useProjectionDB(t)
	order := seedOldProjections(t, db)
	if err := db.Table("orders").Where("id = ?", order.ID).Update("status", "closed").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("tasks").Where("id = 1").Update("quota", 1).Error; err != nil {
		t.Fatal(err)
	}
	if err := consolidatePersistenceProjections().Migrate(db); err == nil {
		t.Fatal("未拒绝任务冲突")
	}
	if err := db.First(&order, order.ID).Error; err != nil {
		t.Fatal(err)
	}
	if order.WindowState != paytypes.WindowOpen {
		t.Fatal("核对全部表之前修改了订单状态")
	}
}

func TestOrderPublicStatusQueryMatchesJSON(t *testing.T) {
	db := useProjectionDB(t)
	order := insertPaymentSchemaFacts(t, db)
	for _, test := range []struct {
		payment, window, preparation string
		status                       OrderStatus
	}{
		{paytypes.PaymentUnconfirmed, paytypes.WindowOpen, paytypes.PreparationReady, OrderStatusPending},
		{paytypes.PaymentUnconfirmed, paytypes.WindowOpen, paytypes.PreparationRejected, OrderStatusFailed},
		{paytypes.PaymentUnconfirmed, paytypes.WindowProviderClosed, paytypes.PreparationReady, OrderStatusClosed},
		{paytypes.PaymentProcessing, paytypes.WindowLocalExpired, paytypes.PreparationReady, OrderStatusClosed},
	} {
		if err := db.Model(&order).Updates(map[string]any{"payment_state": test.payment, "window_state": test.window, "preparation_state": test.preparation}).Error; err != nil {
			t.Fatal(err)
		}
		for _, status := range []OrderStatus{OrderStatusPending, OrderStatusFailed, OrderStatusClosed, OrderStatusSuccess} {
			result, err := GetOrderList(&SearchOrderParams{Status: string(status), PaginationParams: PaginationParams{Order: "status"}})
			if err != nil {
				t.Fatal(err)
			}
			if status != test.status {
				if result.TotalCount != 0 {
					t.Fatalf("错误命中 %s", status)
				}
				continue
			}
			if result.TotalCount != 1 {
				t.Fatalf("未命中 %s", status)
			}
			raw, err := json.Marshal((*result.Data)[0])
			if err != nil {
				t.Fatal(err)
			}
			var wire map[string]any
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatal(err)
			}
			if wire["status"] != string(status) {
				t.Fatalf("查询和展示不一致: %s", raw)
			}
		}
	}
}
