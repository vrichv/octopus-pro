package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 8,
		Up:      addCircuitBreakerColumns,
	})
}

// 008: add per-channel circuit breaker override columns to channels table.
func addCircuitBreakerColumns(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	dialect := db.Dialector.Name()

	columns := []struct {
		name    string
		colType string
	}{
		{"circuit_breaker_threshold", "INTEGER"},
		{"circuit_breaker_cooldown", "INTEGER"},
		{"circuit_breaker_max_cooldown", "INTEGER"},
	}

	for _, col := range columns {
		if HasColumn(db, "channels", col.name) {
			continue
		}

		var sql string
		switch dialect {
		case "mysql":
			sql = fmt.Sprintf("ALTER TABLE `channels` ADD COLUMN `%s` %s DEFAULT NULL", col.name, col.colType)
		case "postgres":
			sql = fmt.Sprintf("ALTER TABLE channels ADD COLUMN IF NOT EXISTS %s %s", col.name, col.colType)
		default:
			sql = fmt.Sprintf("ALTER TABLE channels ADD COLUMN %s %s", col.name, col.colType)
		}
		if err := db.Exec(sql).Error; err != nil {
			return fmt.Errorf("failed to add channels.%s: %w", col.name, err)
		}
	}
	return nil
}
