package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ModelOwnershipBatchLimit = 500

type ModelOwnershipCandidate struct {
	Model     string `json:"model"`
	Name      string `json:"name"`
	OwnedByID *int   `json:"owned_by_id"`
	HasInfo   bool   `json:"has_info"`
}

type ModelOwnershipBatchItem struct {
	Model     string          `json:"model"`
	OwnedByID json.RawMessage `json:"owned_by_id"`
}

type ModelOwnershipBatchRequest struct {
	Mode  string                    `json:"mode"`
	Items []ModelOwnershipBatchItem `json:"items"`
}

type ModelOwnershipBatchResult struct {
	Created   int `json:"created"`
	Updated   int `json:"updated"`
	Unchanged int `json:"unchanged"`
	Skipped   int `json:"skipped"`
}

type ModelOwnershipValidationError struct {
	Message string
}

func (e *ModelOwnershipValidationError) Error() string { return e.Message }

func ownershipValidationError(message string) error {
	return &ModelOwnershipValidationError{Message: message}
}

// ValidateModelOwnershipModels 保留精确模型身份，去重仅影响本次操作范围。
func ValidateModelOwnershipModels(models []string) ([]string, error) {
	if len(models) == 0 || len(models) > ModelOwnershipBatchLimit {
		return nil, ownershipValidationError("每次必须指定 1 至 500 个模型")
	}
	seen := make(map[string]struct{}, len(models))
	for _, name := range models {
		if strings.TrimSpace(name) == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > 100 || strings.Contains(name, "*") {
			return nil, ownershipValidationError("模型标识不能为空、不能超过 100 个字符且不能包含通配符 *")
		}
		seen[name] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for name := range seen {
		result = append(result, name)
	}
	sort.Strings(result)
	return result, nil
}

// GetModelOwnershipCatalog 只组合管理视图，不为尚未维护信息的模型创建目录。
func GetModelOwnershipCatalog(ctx context.Context) ([]ModelOwnershipCandidate, error) {
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	db := DB.WithContext(ctx)
	var infos []ModelInfo
	if err := db.Select("model", "name", "owned_by_id").Find(&infos).Error; err != nil {
		return nil, err
	}
	entries := make(map[string]ModelOwnershipCandidate, len(infos))
	add := func(name string) {
		if strings.TrimSpace(name) == "" || strings.Contains(name, "*") {
			return
		}
		if _, exists := entries[name]; !exists {
			entries[name] = ModelOwnershipCandidate{Model: name}
		}
	}
	for _, info := range infos {
		add(info.Model)
		if _, exists := entries[info.Model]; exists {
			entries[info.Model] = ModelOwnershipCandidate{Model: info.Model, Name: info.Name, OwnedByID: info.OwnedByID, HasInfo: true}
		}
	}
	var prices []string
	if err := db.Model(&Price{}).Pluck("model", &prices).Error; err != nil {
		return nil, err
	}
	for _, name := range prices {
		add(name)
	}
	var channels []struct{ Models string }
	if err := db.Model(&Channel{}).Select("models").Find(&channels).Error; err != nil {
		return nil, err
	}
	for _, channel := range channels {
		for _, name := range strings.Split(channel.Models, ",") {
			add(strings.TrimSpace(name))
		}
	}
	result := make([]ModelOwnershipCandidate, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Model < result[j].Model })
	return result, nil
}

func ApplyModelOwnershipBatch(ctx context.Context, request ModelOwnershipBatchRequest) (ModelOwnershipBatchResult, error) {
	var result ModelOwnershipBatchResult
	if request.Mode != "fill_missing" && request.Mode != "replace" {
		return result, ownershipValidationError("归属修改模式必须为 fill_missing 或 replace")
	}
	names := make([]string, len(request.Items))
	for i, item := range request.Items {
		names[i] = item.Model
	}
	names, err := ValidateModelOwnershipModels(names)
	if err != nil {
		return result, err
	}
	targets := make(map[string]*int, len(names))
	ownerSet := make(map[int]struct{})
	for _, item := range request.Items {
		if len(item.OwnedByID) == 0 {
			return result, ownershipValidationError("每个模型必须显式提供 owned_by_id；清空归属请传 null")
		}
		var owner *int
		if err := json.Unmarshal(item.OwnedByID, &owner); err != nil || (owner != nil && *owner <= 0) {
			return result, ownershipValidationError("owned_by_id 必须为正整数或 null")
		}
		if request.Mode == "fill_missing" && owner == nil {
			return result, ownershipValidationError("仅补齐归属时不能使用 null")
		}
		if previous, exists := targets[item.Model]; exists && !sameModelOwnership(previous, owner) {
			return result, ownershipValidationError("同一模型不能指定不同的目标归属")
		}
		targets[item.Model] = owner
		if owner != nil {
			ownerSet[*owner] = struct{}{}
		}
	}
	if DB == nil {
		return result, errors.New("database is not initialized")
	}
	ownerIDs := make([]int, 0, len(ownerSet))
	for id := range ownerSet {
		ownerIDs = append(ownerIDs, id)
	}
	sort.Ints(ownerIDs)
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 与单条编辑和归属删除保持相同锁顺序，批次内部再按 ID 和模型名排序。
		for _, id := range ownerIDs {
			if err := lockModelOwnedBy(tx, id); err != nil {
				return fmt.Errorf("无法使用归属 %d: %w", id, err)
			}
		}
		for _, name := range names {
			owner := targets[name]
			var current ModelInfo
			err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "owned_by_id").Where("model = ?", name).Take(&current).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				if owner == nil {
					result.Unchanged++
					continue
				}
				var created bool
				current, created, err = createModelOwnershipIfMissing(tx, name, owner)
				if err != nil {
					return err
				}
				if created {
					result.Created++
					continue
				}
				// 唯一冲突恢复已锁定真实行，复用其归属判断 fill_missing。
			} else if err != nil {
				return err
			}
			if sameModelOwnership(current.OwnedByID, owner) {
				result.Unchanged++
				continue
			}
			if request.Mode == "fill_missing" && current.OwnedByID != nil {
				result.Skipped++
				continue
			}
			query := tx.Model(&ModelInfo{}).Where("id = ?", current.Id)
			if request.Mode == "fill_missing" {
				query = query.Where("owned_by_id IS NULL")
			}
			if err := query.Updates(map[string]any{"owned_by_id": owner, "updated_at": time.Now().Unix()}).Error; err != nil {
				return err
			}
			result.Updated++
		}
		return nil
	})
	if err != nil {
		return ModelOwnershipBatchResult{}, err
	}
	return result, nil
}

func sameModelOwnership(left, right *int) bool {
	return (left == nil && right == nil) || (left != nil && right != nil && *left == *right)
}

func createModelOwnershipIfMissing(tx *gorm.DB, name string, owner *int) (ModelInfo, bool, error) {
	// 唯一键冲突可能发生在另一归属的并发批次中；保存点使 PostgreSQL 的
	// 冲突不会中止整个事务，也不依赖 MySQL 的 found-rows 选项判断是否创建。
	const savepoint = "model_ownership_insert"
	if err := tx.SavePoint(savepoint).Error; err != nil {
		return ModelInfo{}, false, err
	}
	created := ModelInfo{Model: name, OwnedByID: owner}
	err := tx.Create(&created).Error
	if err != nil {
		if rollbackErr := tx.RollbackTo(savepoint).Error; rollbackErr != nil {
			return ModelInfo{}, false, rollbackErr
		}
	}
	if releaseErr := tx.Exec("RELEASE SAVEPOINT " + savepoint).Error; releaseErr != nil {
		return ModelInfo{}, false, releaseErr
	}
	if err != nil && !IsUniqueConstraintError(err) {
		return ModelInfo{}, false, err
	}
	if err != nil {
		// 仅在相同模型确实已存在时恢复；其他约束错误仍是数据库失败。
		var existing ModelInfo
		lookupErr := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "owned_by_id").Where("model = ?", name).Take(&existing).Error
		if errors.Is(lookupErr, gorm.ErrRecordNotFound) {
			return ModelInfo{}, false, err
		}
		if lookupErr != nil {
			return ModelInfo{}, false, lookupErr
		}
		return existing, false, nil
	}
	return created, true, nil
}
