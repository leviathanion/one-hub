package model

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"gorm.io/gorm"
)

func useModelCatalogTestDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	originalDB := DB
	DB = db
	t.Cleanup(func() { DB = originalDB })
	if err := db.AutoMigrate(&ModelOwnedBy{}, &ModelInfo{}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasIndex(&ModelInfo{}, "idx_model_info_model") {
		t.Fatal("精确模型名唯一索引 idx_model_info_model 缺失")
	}
	if err := EnsureModelInfoIdentitySchema(db); err != nil {
		t.Fatal(err)
	}
}

func TestModelCatalogExactNamesAndReferences(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		useModelCatalogTestDB(t, db)
		ownedByID := 1001
		if err := CreateModelOwnedBy(&ModelOwnedBy{Id: ownedByID, Name: "自定义归属"}); err != nil {
			t.Fatal(err)
		}
		upper := &ModelInfo{Model: "Model-A", Description: "大写", OwnedByID: &ownedByID}
		lower := &ModelInfo{Model: "model-a", Description: "小写"}
		for _, info := range []*ModelInfo{upper, lower} {
			if err := CreateModelInfo(info); err != nil {
				t.Fatal(err)
			}
		}
		if err := CreateModelInfo(&ModelInfo{Model: "Model-A"}); err == nil {
			t.Fatal("精确重名必须由数据库唯一约束拒绝")
		}
		if err := CreateModelInfo(&ModelInfo{Model: " "}); err == nil {
			t.Fatal("空模型名必须拒绝")
		}
		missingID := 9999
		if err := CreateModelInfo(&ModelInfo{Model: "missing-owner", OwnedByID: &missingID}); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("非法归属引用未拒绝: %v", err)
		}
		rows, err := GetModelInfoResponses([]string{"Model-A", "model-a", "missing"})
		if err != nil || len(rows) != 2 || rows["Model-A"].Description != "大写" || rows["model-a"].Description != "小写" {
			t.Fatalf("精确目录读取错误: rows=%+v err=%v", rows, err)
		}
		if rows["Model-A"].OwnedByID == nil || *rows["Model-A"].OwnedByID != ownedByID || rows["model-a"].OwnedByID != nil {
			t.Fatal("归属关联不正确")
		}
		if err := DeleteModelOwnedBy(ownedByID); !errors.Is(err, ErrModelOwnedByInUse) {
			t.Fatalf("仍被引用的归属可被删除: %v", err)
		}
		upper.OwnedByID = &missingID
		if err := UpdateModelInfo(upper); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("更新允许非法归属引用: %v", err)
		}
		upper.OwnedByID = nil
		upper.Description = ""
		if err := UpdateModelInfo(upper); err != nil {
			t.Fatal(err)
		}
		rows, err = GetModelInfoResponses([]string{"Model-A"})
		if err != nil || rows["Model-A"].OwnedByID != nil || rows["Model-A"].Description != "" {
			t.Fatalf("清空归属或描述失败: %+v %v", rows, err)
		}
		if err := DeleteModelOwnedBy(ownedByID); err != nil {
			t.Fatal(err)
		}
		if err := UpdateModelInfo(&ModelInfo{Id: 9999, Model: "must-not-create"}); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("更新不存在目录不得变成新增: %v", err)
		}
		if err := UpdateModelOwnedBy(&ModelOwnedBy{Id: ownedByID, Name: "must-not-create"}); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("更新不存在归属不得变成新增: %v", err)
		}
	})
}

func TestModelCatalogSeedPreservesEditsAndReadErrors(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		useModelCatalogTestDB(t, db)
		first := GetDefaultModelOwnedBy()[0]
		if err := db.Create(&ModelOwnedBy{Id: first.Id, Name: "已编辑", Icon: "local-icon"}).Error; err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			if err := InitModelOwnedBys(); err != nil {
				t.Fatal(err)
			}
		}
		ownedBy, err := GetModelOwnedByMap()
		if err != nil || ownedBy[first.Id].Name != "已编辑" || ownedBy[first.Id].Icon != "local-icon" || len(ownedBy) != len(GetDefaultModelOwnedBy()) {
			t.Fatalf("种子补齐覆写管理员配置: %+v %v", ownedBy[first.Id], err)
		}
		if err := CreateModelOwnedBy(&ModelOwnedBy{Id: 1001, Name: "目录故障检查"}); err != nil {
			t.Fatal(err)
		}
		if err := db.Migrator().DropTable(&ModelInfo{}); err != nil {
			t.Fatal(err)
		}
		if _, err := GetModelInfoResponses([]string{"missing"}); err == nil {
			t.Fatal("目录查询失败不得伪装为目录缺失")
		}
		if err := DeleteModelOwnedBy(1001); err == nil {
			t.Fatal("无法验证目录引用时不得删除归属")
		}
		if err := db.Migrator().DropTable(&ModelOwnedBy{}); err != nil {
			t.Fatal(err)
		}
		if _, err := GetModelOwnedByMap(); err == nil {
			t.Fatal("归属查询失败不得返回空映射")
		}
		if err := InitModelOwnedBys(); err == nil {
			t.Fatal("种子初始化失败必须报告")
		}
	})
}

func TestModelCatalogConcurrentAssignmentAndDeletion(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		useModelCatalogTestDB(t, db)
		for i := 0; i < 12; i++ {
			id := 1001 + i
			if err := CreateModelOwnedBy(&ModelOwnedBy{Id: id, Name: "并发归属"}); err != nil {
				t.Fatal(err)
			}
			info := &ModelInfo{Model: fmt.Sprintf("concurrent-%d", i)}
			if i%2 != 0 {
				if err := CreateModelInfo(info); err != nil {
					t.Fatal(err)
				}
			}
			info.OwnedByID = &id
			var assigned, deleted error
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				if info.Id == 0 {
					assigned = CreateModelInfo(info)
				} else {
					assigned = UpdateModelInfo(info)
				}
			}()
			go func() {
				defer wg.Done()
				<-start
				deleted = DeleteModelOwnedBy(id)
			}()
			close(start)
			wg.Wait()
			if assigned == nil && deleted == nil {
				t.Fatal("并发引用与删除都成功，出现悬挂引用")
			}
			var dangling int64
			if err := db.Table("model_info AS mi").Joins("LEFT JOIN model_owned_by AS ob ON mi.owned_by_id = ob.id").
				Where("mi.owned_by_id IS NOT NULL AND ob.id IS NULL").Count(&dangling).Error; err != nil {
				t.Fatal(err)
			}
			if dangling != 0 {
				t.Fatalf("并发写删留下 %d 个悬挂引用", dangling)
			}
		}
	})
}
