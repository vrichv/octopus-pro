package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 5,
		Up:      addChannelExcludedModelColumn,
	})
}

// 005: add excluded_model to remember auto models disabled by users.
func addChannelExcludedModelColumn(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	dialect := db.Dialector.Name()

	hasColumn := func(table, column string) bool {
		switch dialect {
		case "sqlite":
			var name string
			db.Raw("SELECT name FROM pragma_table_info(?) WHERE name = ? LIMIT 1", table, column).Scan(&name)
			return name == column
		case "mysql":
			var count int64
			db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?", table, column).Scan(&count)
			return count > 0
		case "postgres":
			var count int64
			db.Raw("SELECT COUNT(*) FROM information_schema.columns WHERE table_name = ? AND column_name = ?", table, column).Scan(&count)
			return count > 0
		default:
			return db.Migrator().HasColumn(table, column)
		}
	}

	if hasColumn("channels", "excluded_model") {
		return nil
	}

	var sql string
	switch dialect {
	case "mysql":
		sql = "ALTER TABLE `channels` ADD COLUMN `excluded_model` TEXT DEFAULT ''"
	case "postgres":
		sql = "ALTER TABLE channels ADD COLUMN IF NOT EXISTS excluded_model TEXT DEFAULT ''"
	default:
		sql = "ALTER TABLE channels ADD COLUMN excluded_model TEXT DEFAULT ''"
	}
	if err := db.Exec(sql).Error; err != nil {
		return fmt.Errorf("failed to add channels.excluded_model: %w", err)
	}
	return nil
}
