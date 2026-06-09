package migrate

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 6,
		Up:      addAPIKeyPIIFilterColumn,
	})
}

// 006: move privacy filtering from global setting to API keys.
func addAPIKeyPIIFilterColumn(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	dialect := db.Dialector.Name()
	if !hasPIIFilterColumn(db, dialect) {
		var sql string
		switch dialect {
		case "mysql":
			sql = "ALTER TABLE `api_keys` ADD COLUMN `pii_filter_enabled` BOOLEAN DEFAULT false"
		case "postgres":
			sql = "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS pii_filter_enabled BOOLEAN DEFAULT false"
		default:
			sql = "ALTER TABLE api_keys ADD COLUMN pii_filter_enabled BOOLEAN DEFAULT false"
		}
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("failed to add api_keys.pii_filter_enabled: %w", err)
		}
	}

	var oldValue string
	err := db.Raw("SELECT value FROM settings WHERE key = ? LIMIT 1", "privacy_filter_enabled").Scan(&oldValue).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("failed to read privacy_filter_enabled setting: %w", err)
	}
	if oldValue == "true" {
		if err := db.Exec("UPDATE api_keys SET pii_filter_enabled = true").Error; err != nil {
			return fmt.Errorf("failed to enable api key PII filters: %w", err)
		}
	}
	if err := db.Exec("DELETE FROM settings WHERE key = ?", "privacy_filter_enabled").Error; err != nil {
		return fmt.Errorf("failed to delete privacy_filter_enabled setting: %w", err)
	}
	return nil
}

func hasPIIFilterColumn(db *gorm.DB, dialect string) bool {
	switch dialect {
	case "sqlite":
		var name string
		db.Raw("SELECT name FROM pragma_table_info(?) WHERE name = ? LIMIT 1", "api_keys", "pii_filter_enabled").Scan(&name)
		return name == "pii_filter_enabled"
	case "mysql":
		var count int64
		db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?", "api_keys", "pii_filter_enabled").Scan(&count)
		return count > 0
	case "postgres":
		var count int64
		db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?", "api_keys", "pii_filter_enabled").Scan(&count)
		return count > 0
	default:
		return db.Migrator().HasColumn("api_keys", "pii_filter_enabled")
	}
}
