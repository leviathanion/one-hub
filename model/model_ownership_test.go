package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"gorm.io/gorm"
)

func ownershipItem(name string, owner *int) ModelOwnershipBatchItem {
	raw, _ := json.Marshal(owner)
	return ModelOwnershipBatchItem{Model: name, OwnedByID: raw}
}

func setupModelOwnershipTest(t *testing.T, db *gorm.DB) {
	t.Helper()
	useModelCatalogTestDB(t, db)
	if err := db.AutoMigrate(&Price{}, &Channel{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{1001, 1002} {
		if err := CreateModelOwnedBy(&ModelOwnedBy{Id: id, Name: "测试归属"}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestModelOwnershipCatalogCombinesExactModelsWithoutCreatingInfo(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupModelOwnershipTest(t, db)
		owner := 1001
		if err := CreateModelInfo(&ModelInfo{Model: "Catalog", Name: "目录名称", OwnedByID: &owner}); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&[]Price{{Model: "priced"}, {Model: "family*"}, {Model: "Catalog"}}).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&Channel{Name: "已禁用", Models: "catalog,channel-only,priced,other*,foo, bar", Status: 2, Key: "must-not-be-returned"}).Error; err != nil {
			t.Fatal(err)
		}
		rows, err := GetModelOwnershipCatalog(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, row := range rows {
			names = append(names, row.Model)
			if row.Model == "Catalog" {
				if !row.HasInfo || row.Name != "目录名称" || row.OwnedByID == nil || *row.OwnedByID != owner {
					t.Fatalf("目录信息丢失: %+v", row)
				}
			} else if row.HasInfo || row.OwnedByID != nil {
				t.Fatalf("无目录模型伪造了信息: %+v", row)
			}
		}
		if !reflect.DeepEqual(names, []string{"Catalog", "bar", "catalog", "channel-only", "foo", "priced"}) {
			t.Fatalf("候选模型错误: %v", names)
		}
		var count int64
		if err := db.Model(&ModelInfo{}).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("读取创建了目录: count=%d err=%v", count, err)
		}
		if err := db.Migrator().DropTable(&Price{}); err != nil {
			t.Fatal(err)
		}
		if _, err := GetModelOwnershipCatalog(context.Background()); err == nil {
			t.Fatal("来源读取失败不能返回不完整目录")
		}
	})
}

func TestModelOwnershipBatchPreservesMetadataAndSeparatesModes(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupModelOwnershipTest(t, db)
		first, second := 1001, 1002
		original := ModelInfo{Model: "existing", Name: "原名称", Description: "原描述", ContextLength: 42, MaxTokens: 7, InputModalities: `["text"]`, OutputModalities: `["audio"]`, Tags: `["tag"]`, SupportUrl: "https://example.test", CreatedAt: 11, UpdatedAt: 12}
		if err := CreateModelInfo(&original); err != nil {
			t.Fatal(err)
		}
		if err := CreateModelInfo(&ModelInfo{Model: "assigned", OwnedByID: &second}); err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&Price{Model: "existing", Input: 1.25, Output: 2.5}).Error; err != nil {
			t.Fatal(err)
		}
		request := ModelOwnershipBatchRequest{Mode: "fill_missing", Items: []ModelOwnershipBatchItem{ownershipItem("existing", &first), ownershipItem("new", &first), ownershipItem("new", &first), ownershipItem("assigned", &first)}}
		result, err := ApplyModelOwnershipBatch(context.Background(), request)
		if err != nil || result != (ModelOwnershipBatchResult{Created: 1, Updated: 1, Skipped: 1}) {
			t.Fatalf("补齐结果错误: %+v %v", result, err)
		}
		updated, err := GetModelInfoByModel("existing")
		if err != nil {
			t.Fatal(err)
		}
		if updated.OwnedByID == nil || *updated.OwnedByID != first {
			t.Fatal("没有写入归属")
		}
		updated.OwnedByID, updated.UpdatedAt = original.OwnedByID, original.UpdatedAt
		if !reflect.DeepEqual(*updated, original) {
			t.Fatalf("其他元数据被修改: got=%+v want=%+v", *updated, original)
		}
		result, err = ApplyModelOwnershipBatch(context.Background(), request)
		if err != nil || result != (ModelOwnershipBatchResult{Unchanged: 2, Skipped: 1}) {
			t.Fatalf("重复补齐结果错误: %+v %v", result, err)
		}
		request = ModelOwnershipBatchRequest{Mode: "replace", Items: []ModelOwnershipBatchItem{ownershipItem("assigned", &first), ownershipItem("existing", nil), ownershipItem("absent", nil)}}
		result, err = ApplyModelOwnershipBatch(context.Background(), request)
		if err != nil || result != (ModelOwnershipBatchResult{Updated: 2, Unchanged: 1}) {
			t.Fatalf("替换/清空结果错误: %+v %v", result, err)
		}
		if _, err := GetModelInfoByModel("absent"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("清空不存在模型不应创建目录: %v", err)
		}
		var price Price
		if err := db.Where("model = ?", "existing").Take(&price).Error; err != nil || price.Input != 1.25 || price.Output != 2.5 {
			t.Fatalf("归属修改影响了价格: %+v %v", price, err)
		}
	})
}

func TestModelOwnershipBatchRejectsInvalidInput(t *testing.T) {
	first, second := 1001, 1002
	valid := ownershipItem("valid", &first)
	tests := []ModelOwnershipBatchRequest{
		{Mode: "unknown", Items: []ModelOwnershipBatchItem{valid}},
		{Mode: "replace"},
		{Mode: "replace", Items: make([]ModelOwnershipBatchItem, ModelOwnershipBatchLimit+1)},
		{Mode: "replace", Items: []ModelOwnershipBatchItem{{Model: "missing-field"}}},
		{Mode: "fill_missing", Items: []ModelOwnershipBatchItem{ownershipItem("null", nil)}},
		{Mode: "replace", Items: []ModelOwnershipBatchItem{valid, ownershipItem("valid", &second)}},
	}
	for _, name := range []string{"", "  ", "model*", strings.Repeat("中", 101), string([]byte{0xff})} {
		tests = append(tests, ModelOwnershipBatchRequest{Mode: "replace", Items: []ModelOwnershipBatchItem{ownershipItem(name, &first)}})
	}
	for _, raw := range []string{"0", "-1", "1.5", `"1001"`, "true", "{}"} {
		tests = append(tests, ModelOwnershipBatchRequest{Mode: "replace", Items: []ModelOwnershipBatchItem{{Model: "valid", OwnedByID: json.RawMessage(raw)}}})
	}
	for i, request := range tests {
		_, err := ApplyModelOwnershipBatch(context.Background(), request)
		var validation *ModelOwnershipValidationError
		if !errors.As(err, &validation) {
			t.Fatalf("case %d 未返回输入错误: %v", i, err)
		}
	}
	names, err := ValidateModelOwnershipModels([]string{"model", "Model", "model"})
	if err != nil || !reflect.DeepEqual(names, []string{"Model", "model"}) {
		t.Fatalf("精确身份或去重错误: %v %v", names, err)
	}
}

func TestModelOwnershipValidationAcceptsFullBatchAndRejectsOverflow(t *testing.T) {
	models := make([]string, ModelOwnershipBatchLimit+1)
	for i := range models {
		models[i] = fmt.Sprintf("model-%05d", i)
	}
	names, err := ValidateModelOwnershipModels(models[:ModelOwnershipBatchLimit])
	if err != nil || !reflect.DeepEqual(names, models[:ModelOwnershipBatchLimit]) {
		t.Fatalf("达到批量上限时应保留全部模型: count=%d err=%v", len(names), err)
	}
	_, err = ValidateModelOwnershipModels(models)
	var validation *ModelOwnershipValidationError
	if !errors.As(err, &validation) || !strings.Contains(err.Error(), fmt.Sprint(ModelOwnershipBatchLimit)) {
		t.Fatalf("超过批量上限应返回当前限制: %v", err)
	}
}

func TestModelOwnershipBatchRollsBackEveryMutation(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupModelOwnershipTest(t, db)
		owner := 1001
		if err := CreateModelInfo(&ModelInfo{Model: "a-existing", Description: "保留"}); err != nil {
			t.Fatal(err)
		}
		failure := errors.New("目录写入失败")
		const callback = "test:ownership_create_failure"
		if err := db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
			if info, ok := tx.Statement.Dest.(*ModelInfo); ok && info.Model == "z-fail" {
				tx.AddError(failure)
			}
		}); err != nil {
			t.Fatal(err)
		}
		defer db.Callback().Create().Remove(callback)
		result, err := ApplyModelOwnershipBatch(context.Background(), ModelOwnershipBatchRequest{Mode: "replace", Items: []ModelOwnershipBatchItem{ownershipItem("a-existing", &owner), ownershipItem("b-created", &owner), ownershipItem("z-fail", &owner)}})
		if !errors.Is(err, failure) || result != (ModelOwnershipBatchResult{}) {
			t.Fatalf("失败返回值错误: %+v %v", result, err)
		}
		info, err := GetModelInfoByModel("a-existing")
		if err != nil || info.OwnedByID != nil || info.Description != "保留" {
			t.Fatalf("已有目录写入未回滚: %+v %v", info, err)
		}
		if _, err := GetModelInfoByModel("b-created"); !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("新增目录未回滚: %v", err)
		}
		missing := 9999
		_, err = ApplyModelOwnershipBatch(context.Background(), ModelOwnershipBatchRequest{Mode: "replace", Items: []ModelOwnershipBatchItem{ownershipItem("a-existing", &missing)}})
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			t.Fatalf("不存在归属未识别: %v", err)
		}
	})
}

func TestModelOwnershipConcurrentFillCreatesOneRow(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupModelOwnershipTest(t, db)
		owner := 1001
		request := ModelOwnershipBatchRequest{Mode: "fill_missing", Items: []ModelOwnershipBatchItem{ownershipItem("concurrent", &owner)}}
		var results [2]ModelOwnershipBatchResult
		var errs [2]error
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				results[i], errs[i] = ApplyModelOwnershipBatch(context.Background(), request)
			}(i)
		}
		close(start)
		wg.Wait()
		if errs[0] != nil || errs[1] != nil || results[0].Created+results[1].Created != 1 || results[0].Unchanged+results[1].Unchanged != 1 {
			t.Fatalf("并发补齐结果错误: %+v %v", results, errs)
		}
		if err := DeleteModelOwnedBy(owner); !errors.Is(err, ErrModelOwnedByInUse) {
			t.Fatalf("批量设置后归属仍可删除: %v", err)
		}
	})
}

func TestModelOwnershipInsertConflictPreservesExistingInfoAndTransaction(t *testing.T) {
	forEachTestDatabase(t, func(t *testing.T, db *gorm.DB) {
		setupModelOwnershipTest(t, db)
		first, second := 1001, 1002
		if err := CreateModelInfo(&ModelInfo{Model: "existing", Description: "不能覆盖", OwnedByID: &first}); err != nil {
			t.Fatal(err)
		}
		err := db.Transaction(func(tx *gorm.DB) error {
			existing, created, err := createModelOwnershipIfMissing(tx, "existing", &second)
			if err != nil {
				return err
			}
			if created {
				t.Fatal("唯一冲突错误计为新增")
			}
			if existing.Id == 0 || existing.OwnedByID == nil || *existing.OwnedByID != first {
				t.Fatalf("冲突恢复未返回已有归属: %+v", existing)
			}
			_, created, err = createModelOwnershipIfMissing(tx, "after-conflict", &second)
			if err == nil && !created {
				t.Fatal("唯一冲突后事务未继续写入")
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		info, err := GetModelInfoByModel("existing")
		if err != nil || info.OwnedByID == nil || *info.OwnedByID != first || info.Description != "不能覆盖" {
			t.Fatalf("冲突覆盖了已有目录: %+v %v", info, err)
		}
		if _, err := GetModelInfoByModel("after-conflict"); err != nil {
			t.Fatalf("冲突后写入没有提交: %v", err)
		}
	})
}
