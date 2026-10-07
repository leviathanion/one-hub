package model

import (
	"context"
	"path/filepath"
	"testing"

	"one-api/common"
	"one-api/common/config"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func catalogStartupDB(t *testing.T, master bool) *gorm.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.db")
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	require.NoError(t, err)
	pool, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	previousDB, previousMaster := DB, config.IsMasterNode
	previousSQLite, previousPostgres := common.UsingSQLite, common.UsingPostgreSQL
	viper.Reset()
	viper.Set("sqlite_path", path)
	config.IsMasterNode = master
	t.Cleanup(func() {
		if DB != nil && DB != previousDB {
			if current, err := DB.DB(); err == nil {
				_ = current.Close()
			}
		}
		DB, config.IsMasterNode = previousDB, previousMaster
		common.UsingSQLite, common.UsingPostgreSQL = previousSQLite, previousPostgres
		viper.Reset()
	})
	return db
}

func seedOldCatalogForStartup(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.AutoMigrate(&Price{}, &ModelOwnedBy{}, &Channel{}, &PublicationVersion{}))
	execStartupFixture(t, db,
		"ALTER TABLE prices ADD COLUMN channel_type INTEGER DEFAULT 0",
		"CREATE TABLE model_info (id INTEGER PRIMARY KEY, model varchar(100), name varchar(100), description text, created_at bigint, updated_at bigint)",
		"CREATE INDEX idx_model_info_model ON model_info (model)",
		"INSERT INTO model_info (id, model, name, description, created_at, updated_at) VALUES (1, 'alpha-exact', '模型描述', '原样保留', 11, 12)",
		"INSERT INTO model_owned_by (id, name) VALUES (1, '内置分类'), (1001, '自定义托管商')",
		"INSERT INTO prices (model, type, input, output, locked, channel_type) VALUES ('alpha-exact', 'tokens', 1.25, 2.5, true, 1), ('alpha*', 'tokens', 2, 3, false, 1001)",
		"INSERT INTO publication_versions (owner, version) VALUES ('price', 42)",
	)
	require.NoError(t, db.Create(&Channel{Id: 1, Name: "禁用渠道", Key: "fixture", Models: "alpha-disabled,no-owner", Status: 2}).Error)
}

func TestStartupAutomaticallyMigratesModelCatalogOnce(t *testing.T) {
	db := catalogStartupDB(t, true)
	seedOldCatalogForStartup(t, db)
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			require.NoError(t, CloseDB())
		}
		require.NoError(t, InitDB())
		require.NoError(t, ValidateModelCatalogSchema(DB))
		require.False(t, DB.Migrator().HasColumn("prices", "channel_type"))
		var infos []ModelInfo
		require.NoError(t, DB.Order("model").Find(&infos).Error)
		require.Len(t, infos, 2)
		require.Equal(t, "alpha-disabled", infos[0].Model)
		require.NotNil(t, infos[0].OwnedByID)
		require.Equal(t, 1001, *infos[0].OwnedByID)
		require.Equal(t, "alpha-exact", infos[1].Model)
		require.NotNil(t, infos[1].OwnedByID)
		require.Equal(t, 1, *infos[1].OwnedByID)
		require.Equal(t, "原样保留", infos[1].Description)
		require.EqualValues(t, 11, infos[1].CreatedAt)
		require.EqualValues(t, 12, infos[1].UpdatedAt)
		var price Price
		require.NoError(t, DB.Where("model = ?", "alpha-exact").Take(&price).Error)
		require.Equal(t, 1.25, price.Input)
		require.Equal(t, 2.5, price.Output)
		require.True(t, price.Locked)
		version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
		require.NoError(t, err)
		require.EqualValues(t, 42, version)
	}
	require.NoError(t, CloseDB())
	config.IsMasterNode = false
	require.NoError(t, InitDB())
	require.NoError(t, ValidateModelCatalogSchema(DB))
	version, err := ReadPublicationVersion(context.Background(), DB, PublicationOwnerPrice)
	require.NoError(t, err)
	require.EqualValues(t, 42, version)
}

func TestStartupInitializesNewModelCatalog(t *testing.T) {
	catalogStartupDB(t, true)
	require.NoError(t, InitDB())
	require.NoError(t, ValidateModelCatalogSchema(DB))
	require.True(t, DB.Migrator().HasColumn("model_info", "owned_by_id"))
	require.False(t, DB.Migrator().HasColumn("prices", "channel_type"))
}

func TestReplicaStartupRequiresCompletedModelCatalog(t *testing.T) {
	for _, old := range []bool{false, true} {
		name := "empty"
		if old {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			db := catalogStartupDB(t, false)
			if old {
				seedOldCatalogForStartup(t, db)
			}
			require.Error(t, InitDB())
			require.False(t, db.Migrator().HasTable("migrations"))
			require.False(t, db.Migrator().HasTable("users"))
			require.False(t, db.Migrator().HasColumn("model_info", "owned_by_id"))
			if old {
				require.True(t, db.Migrator().HasColumn("prices", "channel_type"))
			}
		})
	}
}

func TestStartupCatalogFailureStopsFurtherInitialization(t *testing.T) {
	for _, failure := range []string{"duplicate", "partial", "missing_table", "duplicate_price_owner"} {
		t.Run(failure, func(t *testing.T) {
			db := catalogStartupDB(t, true)
			seedOldCatalogForStartup(t, db)
			switch failure {
			case "duplicate":
				execStartupFixture(t, db, "INSERT INTO model_info (id, model) VALUES (2, 'alpha-exact')")
			case "partial":
				execStartupFixture(t, db, "ALTER TABLE model_info ADD COLUMN owned_by_id bigint")
			case "missing_table":
				execStartupFixture(t, db, "DROP TABLE model_owned_by")
			case "duplicate_price_owner":
				execStartupFixture(t, db, "DROP INDEX idx_prices_model_unique", "INSERT INTO prices (model, type, input, output, locked, channel_type) VALUES ('alpha-exact', 'tokens', 1.25, 2.5, true, 1001)")
			}
			require.Error(t, InitDB())
			require.False(t, db.Migrator().HasTable("migrations"))
			require.False(t, db.Migrator().HasTable("users"))
			require.Equal(t, failure == "partial", db.Migrator().HasColumn("model_info", "owned_by_id"))
			require.True(t, db.Migrator().HasColumn("prices", "channel_type"))
		})
	}
}
