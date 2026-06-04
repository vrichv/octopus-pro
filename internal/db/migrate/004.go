package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 4,
		Up:      addRateLimitAndKeyModeColumns,
	})
}

// 004: 添加限流、key 代理、key 调度模式字段
func addRateLimitAndKeyModeColumns(db *gorm.DB) error {
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

	addColumn := func(table, column, typ, def string) error {
		if hasColumn(table, column) {
			return nil
		}
		var sql string
		switch dialect {
		case "mysql":
			sql = fmt.Sprintf("ALTER TABLE `%s` ADD COLUMN `%s` %s DEFAULT %s", table, column, typ, def)
		case "postgres":
			sql = fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s DEFAULT %s", table, column, typ, def)
		default:
			sql = fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s DEFAULT %s", table, column, typ, def)
		}
		return db.Exec(sql).Error
	}

	// Channel 新增字段
	if err := addColumn("channels", "rate_limit", "TEXT", "''"); err != nil {
		return fmt.Errorf("failed to add channels.rate_limit: %w", err)
	}
	if err := addColumn("channels", "model_rate_limit", "TEXT", "''"); err != nil {
		return fmt.Errorf("failed to add channels.model_rate_limit: %w", err)
	}
	if err := addColumn("channels", "key_mode", "INTEGER", "0"); err != nil {
		return fmt.Errorf("failed to add channels.key_mode: %w", err)
	}

	// ChannelKey 新增字段
	if err := addColumn("channel_keys", "key_proxy", "TEXT", "''"); err != nil {
		return fmt.Errorf("failed to add channel_keys.key_proxy: %w", err)
	}
	if err := addColumn("channel_keys", "consecutive_auth_errors", "INTEGER", "0"); err != nil {
		return fmt.Errorf("failed to add channel_keys.consecutive_auth_errors: %w", err)
	}
	if err := addColumn("channel_keys", "last_auth_error_time", "INTEGER", "0"); err != nil {
		return fmt.Errorf("failed to add channel_keys.last_auth_error_time: %w", err)
	}

	return nil
}
