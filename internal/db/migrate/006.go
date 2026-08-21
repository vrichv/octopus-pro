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

	if !HasColumn(db, "api_keys", "pii_filter_enabled") {
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
