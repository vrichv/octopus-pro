package migrate

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/vrichv/octopus-pro/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func init() {
	RegisterAfterAutoMigration(Migration{
		Version: 10,
		Up:      migrateProxyReferences,
	})
}

const legacyProxyURLSettingKey = model.SettingKey("proxy_url")

// 010: 将渠道、渠道密钥与系统设置中的代理地址迁移为 proxies 表的 ID 引用。
func migrateProxyReferences(db *gorm.DB) error {
	if db == nil {
		return fmt.Errorf("db is nil")
	}

	resolve := newProxyResolver(db)

	if HasColumn(db, "channels", "channel_proxy") {
		var rows []struct {
			ID           int
			ChannelProxy string
		}
		if err := db.Table("channels").
			Select("id, channel_proxy").
			Where("channel_proxy IS NOT NULL AND channel_proxy <> ''").
			Scan(&rows).Error; err != nil {
			return fmt.Errorf("failed to read legacy channel proxies: %w", err)
		}
		for _, row := range rows {
			proxyID, err := resolve(row.ChannelProxy)
			if err != nil {
				return err
			}
			if err := db.Table("channels").Where("id = ?", row.ID).Update("channel_proxy_id", proxyID).Error; err != nil {
				return fmt.Errorf("failed to set channels.channel_proxy_id for channel %d: %w", row.ID, err)
			}
		}
	}

	if HasColumn(db, "channel_keys", "key_proxy") {
		var rows []struct {
			ID       int
			KeyProxy string
		}
		if err := db.Table("channel_keys").
			Select("id, key_proxy").
			Where("key_proxy IS NOT NULL AND key_proxy <> ''").
			Scan(&rows).Error; err != nil {
			return fmt.Errorf("failed to read legacy channel key proxies: %w", err)
		}
		for _, row := range rows {
			proxyID, err := resolve(row.KeyProxy)
			if err != nil {
				return err
			}
			if err := db.Table("channel_keys").Where("id = ?", row.ID).Update("key_proxy_id", proxyID).Error; err != nil {
				return fmt.Errorf("failed to set channel_keys.key_proxy_id for key %d: %w", row.ID, err)
			}
		}
	}

	var legacySetting model.Setting
	err := db.Where(&model.Setting{Key: legacyProxyURLSettingKey}).First(&legacySetting).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("failed to read legacy proxy setting: %w", err)
	}

	if raw := strings.TrimSpace(legacySetting.Value); raw != "" {
		proxyID, err := resolve(raw)
		if err != nil {
			return err
		}
		setting := model.Setting{Key: model.SettingKeyProxyID, Value: strconv.Itoa(proxyID)}
		if err := db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "key"}},
			DoUpdates: clause.AssignmentColumns([]string{"value"}),
		}).Create(&setting).Error; err != nil {
			return fmt.Errorf("failed to set system proxy id: %w", err)
		}
	}

	if err := db.Where(&model.Setting{Key: legacyProxyURLSettingKey}).Delete(&model.Setting{}).Error; err != nil {
		return fmt.Errorf("failed to remove legacy proxy setting: %w", err)
	}
	return nil
}

// newProxyResolver 返回按代理地址查找或创建 proxies 行的函数，相同地址复用同一行。
func newProxyResolver(db *gorm.DB) func(string) (int, error) {
	return func(raw string) (int, error) {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			return 0, nil
		}

		var existing model.Proxy
		err := db.Where("url = ?", raw).First(&existing).Error
		if err == nil {
			return existing.ID, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return 0, fmt.Errorf("failed to look up proxy %q: %w", raw, err)
		}

		baseName := proxyNameFromURL(raw)
		name := baseName
		for suffix := 2; ; suffix++ {
			var count int64
			if err := db.Model(&model.Proxy{}).Where("name = ?", name).Count(&count).Error; err != nil {
				return 0, fmt.Errorf("failed to check proxy name %q: %w", name, err)
			}
			if count == 0 {
				break
			}
			name = fmt.Sprintf("%s-%d", baseName, suffix)
		}

		proxy := model.Proxy{Name: name, URL: raw}
		if err := db.Create(&proxy).Error; err != nil {
			return 0, fmt.Errorf("failed to create proxy for %q: %w", raw, err)
		}
		return proxy.ID, nil
	}
}

func proxyNameFromURL(raw string) string {
	if parsed, err := url.Parse(raw); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return raw
}
