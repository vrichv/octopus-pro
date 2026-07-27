package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 9,
		Up:      addRateLimitRetryWaitMaxColumn,
	})
}

// 009: add rate_limit_retry_wait_max to groups table.
func addRateLimitRetryWaitMaxColumn(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	dialect := db.Dialector.Name()

	if HasColumn(db, "groups", "rate_limit_retry_wait_max") {
		return nil
	}

	var sql string
	switch dialect {
	case "mysql":
		sql = "ALTER TABLE `groups` ADD COLUMN `rate_limit_retry_wait_max` INTEGER DEFAULT NULL"
	case "postgres":
		sql = "ALTER TABLE groups ADD COLUMN IF NOT EXISTS rate_limit_retry_wait_max INTEGER"
	default:
		sql = "ALTER TABLE groups ADD COLUMN rate_limit_retry_wait_max INTEGER"
	}
	if err := db.Exec(sql).Error; err != nil {
		return fmt.Errorf("failed to add groups.rate_limit_retry_wait_max: %w", err)
	}
	return nil
}
