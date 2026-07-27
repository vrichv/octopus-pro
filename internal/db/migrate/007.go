package migrate

import (
	"fmt"

	"gorm.io/gorm"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 7,
		Up:      addLlmInfosMaxContextColumn,
	})
}

// 007: add max_context to llm_infos for context window overflow detection.
func addLlmInfosMaxContextColumn(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	dialect := db.Dialector.Name()

	if HasColumn(db, "llm_infos", "max_context") {
		return nil
	}

	var sql string
	switch dialect {
	case "mysql":
		sql = "ALTER TABLE `llm_infos` ADD COLUMN `max_context` INTEGER DEFAULT 0"
	case "postgres":
		sql = "ALTER TABLE llm_infos ADD COLUMN IF NOT EXISTS max_context INTEGER DEFAULT 0"
	default:
		sql = "ALTER TABLE llm_infos ADD COLUMN max_context INTEGER DEFAULT 0"
	}
	if err := db.Exec(sql).Error; err != nil {
		return fmt.Errorf("failed to add llm_infos.max_context: %w", err)
	}
	return nil
}
