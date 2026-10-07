// Package catalogmigration upgrades model attribution before application startup.
package catalogmigration

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"one-api/internal/catalogschema"

	"gorm.io/gorm"
)

var ErrAlreadyMigrated = errors.New("数据库已是目标结构，无需重复迁移")

type Conflict struct {
	Model string `json:"model"`
	IDs   []int  `json:"ids"`
}

type Report struct {
	Models           int        `json:"models"`
	ExistingMetadata int        `json:"existing_metadata"`
	Assigned         int        `json:"assigned"`
	Created          int        `json:"created"`
	Unassigned       int        `json:"unassigned"`
	Dangling         []string   `json:"dangling_models,omitempty"`
	Conflicts        []Conflict `json:"conflicts,omitempty"`
	Applied          bool       `json:"applied"`
}

type metadata struct {
	ID    int
	Model string
}

type legacyPrice struct {
	Model       string
	ChannelType int
}

type assignment struct {
	id    int
	model string
	owner int
}

type plan struct {
	report      Report
	assignments []assignment
}

// Run performs a complete preflight before its first write. The application must
// not serve requests or start workers until the master finishes this migration.
// Existing writers must be stopped and backed up before deploying the new version.
func Run(db *gorm.DB, apply bool) (Report, error) {
	if err := checkSourceSchema(db); err != nil {
		return Report{}, err
	}
	if !apply {
		p, err := inspect(db)
		return p.report, err
	}
	var result Report
	writeAttempted := false
	execute := func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec("LOCK TABLE model_info, model_owned_by, prices, channels IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				return err
			}
		}
		// Recheck under the transaction lock rather than applying a stale preview.
		if err := checkSourceSchema(tx); err != nil {
			return err
		}
		p, err := inspect(tx)
		result = p.report
		if err != nil {
			return err
		}
		writeAttempted = true
		if err := tx.Exec("ALTER TABLE model_info ADD COLUMN owned_by_id bigint").Error; err != nil {
			return err
		}
		if err := catalogschema.EnsureIdentity(tx); err != nil {
			return err
		}
		for _, item := range p.assignments {
			if item.id != 0 {
				if err := tx.Table("model_info").Where("id = ?", item.id).Update("owned_by_id", item.owner).Error; err != nil {
					return err
				}
			} else {
				now := time.Now().Unix()
				if err := tx.Table("model_info").Create(map[string]interface{}{
					"model": item.model, "owned_by_id": item.owner, "created_at": now, "updated_at": now,
				}).Error; err != nil {
					return err
				}
			}
		}
		if err := tx.Exec("ALTER TABLE prices DROP COLUMN channel_type").Error; err != nil {
			return err
		}
		return catalogschema.Validate(tx)
	}
	var err error
	if db.Dialector.Name() == "mysql" {
		// MySQL DDL commits implicitly. A failed run must be restored, never resumed.
		err = execute(db)
		if err != nil && writeAttempted {
			err = fmt.Errorf("迁移失败；MySQL DDL 不可事务回滚，请恢复停机备份后重新执行，禁止续跑: %w", err)
		}
	} else {
		err = db.Transaction(execute)
	}
	result.Applied = err == nil
	return result, err
}

func checkSourceSchema(db *gorm.DB) error {
	for _, table := range []string{"model_info", "model_owned_by", "prices", "channels"} {
		if !db.Migrator().HasTable(table) {
			return fmt.Errorf("缺少表 %s；空库无需迁移，部分迁移数据库必须恢复备份", table)
		}
	}
	hasOwner := db.Migrator().HasColumn("model_info", "owned_by_id")
	hasLegacy := db.Migrator().HasColumn("prices", "channel_type")
	if hasOwner && !hasLegacy {
		if err := catalogschema.Validate(db); err != nil {
			return err
		}
		return ErrAlreadyMigrated
	}
	if hasOwner || !hasLegacy {
		return errors.New("检测到部分迁移结构，请恢复停机备份后重新执行，禁止自动续跑")
	}
	return nil
}

func inspect(db *gorm.DB) (plan, error) {
	p := plan{}
	var infos []metadata
	if err := db.Table("model_info").Select("id, model").Order("id").Find(&infos).Error; err != nil {
		return p, err
	}
	p.report.ExistingMetadata = len(infos)
	byName := make(map[string][]int, len(infos))
	models := make(map[string]struct{})
	for _, info := range infos {
		byName[info.Model] = append(byName[info.Model], info.ID)
		if isExact(info.Model) {
			models[info.Model] = struct{}{}
		}
	}
	for name, ids := range byName {
		if len(ids) > 1 {
			p.report.Conflicts = append(p.report.Conflicts, Conflict{Model: name, IDs: ids})
		}
	}
	sort.Slice(p.report.Conflicts, func(i, j int) bool { return p.report.Conflicts[i].Model < p.report.Conflicts[j].Model })
	if len(p.report.Conflicts) > 0 {
		return p, errors.New("存在重复模型目录；请先根据冲突模型与记录 ID 明确合并，再重新盘点")
	}
	var prices []legacyPrice
	if err := db.Table("prices").Select("model, channel_type").Find(&prices).Error; err != nil {
		return p, err
	}
	exact := make(map[string]int)
	priceOwners := make(map[string]int)
	var patterns []legacyPrice
	for _, price := range prices {
		if owner, exists := priceOwners[price.Model]; exists && owner != price.ChannelType {
			return p, fmt.Errorf("价格规则 %q 存在不同归属的重复记录，请先明确解决冲突后重启", price.Model)
		}
		priceOwners[price.Model] = price.ChannelType
		if strings.HasSuffix(price.Model, "*") {
			patterns = append(patterns, price)
		} else {
			exact[price.Model] = price.ChannelType
			if isExact(price.Model) {
				models[price.Model] = struct{}{}
			}
		}
	}
	sort.Slice(patterns, func(i, j int) bool {
		if len(patterns[i].Model) != len(patterns[j].Model) {
			return len(patterns[i].Model) > len(patterns[j].Model)
		}
		return patterns[i].Model < patterns[j].Model
	})
	var channels []struct{ Models string }
	if err := db.Table("channels").Select("models").Find(&channels).Error; err != nil {
		return p, err
	}
	for _, channel := range channels {
		for _, name := range strings.Split(channel.Models, ",") {
			if isExact(name) {
				models[name] = struct{}{}
			}
		}
	}
	var ownerIDs []int
	if err := db.Table("model_owned_by").Pluck("id", &ownerIDs).Error; err != nil {
		return p, err
	}
	owners := make(map[int]bool, len(ownerIDs))
	for _, id := range ownerIDs {
		if id > 0 {
			owners[id] = true
		}
	}
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sort.Strings(names)
	p.report.Models = len(names)
	for _, name := range names {
		owner, matched := exact[name]
		if !matched {
			for _, pattern := range patterns {
				if strings.HasPrefix(name, strings.TrimRight(pattern.Model, "*")) {
					owner, matched = pattern.ChannelType, true
					break
				}
			}
		}
		if !owners[owner] {
			p.report.Unassigned++
			if matched && owner != 0 {
				p.report.Dangling = append(p.report.Dangling, name)
			}
			continue
		}
		if strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 100 {
			return p, fmt.Errorf("模型 %q 无法写入目录：标识必须非空且不超过 100 个字符", name)
		}
		item := assignment{model: name, owner: owner}
		if ids := byName[name]; len(ids) > 0 {
			item.id = ids[0]
		} else {
			p.report.Created++
		}
		p.report.Assigned++
		p.assignments = append(p.assignments, item)
	}
	if db.Dialector.Name() == "mysql" {
		// utf8mb4_bin distinguishes case but ignores trailing spaces. Detect
		// collisions before MySQL's first non-transactional DDL statement.
		identities := make(map[string][]int)
		for _, info := range infos {
			key := strings.TrimRight(info.Model, " ")
			identities[key] = append(identities[key], info.ID)
		}
		for _, item := range p.assignments {
			if item.id == 0 {
				key := strings.TrimRight(item.model, " ")
				identities[key] = append(identities[key], 0)
			}
		}
		for key, ids := range identities {
			if len(ids) > 1 {
				p.report.Conflicts = append(p.report.Conflicts, Conflict{Model: key, IDs: ids})
			}
		}
		if len(p.report.Conflicts) > 0 {
			sort.Slice(p.report.Conflicts, func(i, j int) bool { return p.report.Conflicts[i].Model < p.report.Conflicts[j].Model })
			return p, errors.New("模型名称仅尾部空格不同，无法满足 MySQL 目标唯一约束；请先解决冲突，ID 0 表示待补建目录")
		}
	}
	return p, nil
}

func isExact(name string) bool { return name != "" && !strings.HasSuffix(name, "*") }
