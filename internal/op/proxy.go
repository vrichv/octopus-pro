package op

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/utils/cache"
)

var proxyCache = cache.New[int, model.Proxy](8)

var ErrProxyNotFound = errors.New("proxy not found")

func ProxyList(ctx context.Context) ([]model.Proxy, error) {
	proxies := make([]model.Proxy, 0, proxyCache.Len())
	for _, p := range proxyCache.GetAll() {
		proxies = append(proxies, p)
	}
	sort.Slice(proxies, func(i, j int) bool { return proxies[i].ID < proxies[j].ID })
	return proxies, nil
}

func ProxyGet(id int) (*model.Proxy, error) {
	proxy, ok := proxyCache.Get(id)
	if !ok {
		return nil, ErrProxyNotFound
	}
	return &proxy, nil
}

// ProxyGetURL 返回代理 ID 对应的地址，id 为 0 表示未配置代理。
func ProxyGetURL(id int) (string, error) {
	if id == 0 {
		return "", nil
	}
	proxy, err := ProxyGet(id)
	if err != nil {
		return "", err
	}
	return proxy.URL, nil
}

func ProxyCreate(proxy *model.Proxy, ctx context.Context) error {
	if err := proxy.Validate(); err != nil {
		return err
	}
	if err := ensureProxyNameAvailable(proxy.Name, 0); err != nil {
		return err
	}
	if err := db.GetDB().WithContext(ctx).Create(proxy).Error; err != nil {
		return err
	}
	proxyCache.Set(proxy.ID, *proxy)
	return nil
}

func ProxyUpdate(req *model.ProxyUpdateRequest, ctx context.Context) (*model.Proxy, error) {
	current, ok := proxyCache.Get(req.ID)
	if !ok {
		return nil, fmt.Errorf("proxy not found")
	}

	updated := current
	if req.Name != nil {
		updated.Name = strings.TrimSpace(*req.Name)
	}
	if req.URL != nil {
		updated.URL = strings.TrimSpace(*req.URL)
	}
	if req.Remark != nil {
		updated.Remark = strings.TrimSpace(*req.Remark)
	}
	if err := updated.Validate(); err != nil {
		return nil, err
	}
	if err := ensureProxyNameAvailable(updated.Name, req.ID); err != nil {
		return nil, err
	}

	if err := db.GetDB().WithContext(ctx).
		Model(&model.Proxy{}).
		Where("id = ?", req.ID).
		Select("name", "url", "remark").
		Updates(&updated).Error; err != nil {
		return nil, fmt.Errorf("failed to update proxy: %w", err)
	}
	proxyCache.Set(updated.ID, updated)
	return &updated, nil
}

// ProxyDel 删除代理；仍被渠道、渠道密钥或系统代理引用时拒绝删除。
func ProxyDel(id int, ctx context.Context) error {
	if _, ok := proxyCache.Get(id); !ok {
		return fmt.Errorf("proxy not found")
	}

	conn := db.GetDB().WithContext(ctx)
	var channelCount, keyCount int64
	if err := conn.Model(&model.Channel{}).Where("channel_proxy_id = ?", id).Count(&channelCount).Error; err != nil {
		return fmt.Errorf("failed to check channel references: %w", err)
	}
	if err := conn.Model(&model.ChannelKey{}).Where("key_proxy_id = ?", id).Count(&keyCount).Error; err != nil {
		return fmt.Errorf("failed to check channel key references: %w", err)
	}
	systemProxyID, err := SettingGetInt(model.SettingKeyProxyID)
	if err != nil {
		return err
	}
	if systemProxyID == id {
		return fmt.Errorf("proxy is used as the system proxy")
	}
	if channelCount > 0 || keyCount > 0 {
		return fmt.Errorf("proxy is in use by %d channel(s) and %d key(s)", channelCount, keyCount)
	}

	if err := conn.Delete(&model.Proxy{}, id).Error; err != nil {
		return fmt.Errorf("failed to delete proxy: %w", err)
	}
	proxyCache.Del(id)
	return nil
}

func ensureProxyNameAvailable(name string, excludeID int) error {
	var count int64
	query := db.GetDB().Model(&model.Proxy{}).Where("name = ?", name)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	if err := query.Count(&count).Error; err != nil {
		return fmt.Errorf("failed to check proxy name: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("proxy name already exists")
	}
	return nil
}

func proxyRefreshCache(ctx context.Context) error {
	proxies := []model.Proxy{}
	if err := db.GetDB().WithContext(ctx).Find(&proxies).Error; err != nil {
		return fmt.Errorf("failed to get proxies: %w", err)
	}
	proxyCache.Clear()
	for _, proxy := range proxies {
		proxyCache.Set(proxy.ID, proxy)
	}
	return nil
}
