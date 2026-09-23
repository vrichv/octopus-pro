package helper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dlclark/regexp2"
	"github.com/vrichv/octopus-pro/internal/client"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/utils/log"
	"github.com/vrichv/octopus-pro/internal/utils/xstrings"
)

func ChannelHttpClient(channel *model.Channel) (*http.Client, error) {
	if channel == nil {
		return nil, errors.New("channel is nil")
	}
	if !channel.Proxy {
		return client.GetHTTPClientSystemProxy(false)
	} else if channel.ChannelProxyID == nil || *channel.ChannelProxyID == 0 {
		return client.GetHTTPClientSystemProxy(true)
	} else {
		proxyURL, err := op.ProxyGetURL(*channel.ChannelProxyID)
		if err != nil {
			return nil, err
		}
		return client.GetHTTPClientCustomProxy(proxyURL)
	}
}

// KeyHttpClient 返回针对指定 ChannelKey 的 HTTP 客户端。
// 代理优先级：key.KeyProxyID > channel.ChannelProxyID > 系统代理 > 直连
func KeyHttpClient(channel *model.Channel, key *model.ChannelKey) (*http.Client, error) {
	if channel == nil {
		return nil, errors.New("channel is nil")
	}
	if key != nil && key.KeyProxyID > 0 {
		proxyURL, err := op.ProxyGetURL(key.KeyProxyID)
		if err != nil {
			return nil, err
		}
		return client.GetHTTPClientCustomProxy(proxyURL)
	}
	return ChannelHttpClient(channel)
}

// ProxyDesc 返回用于日志的代理描述。
func ProxyDesc(channel *model.Channel, key *model.ChannelKey) string {
	if key != nil && key.KeyProxyID > 0 {
		return proxyName(key.KeyProxyID)
	}
	if channel != nil && channel.ChannelProxyID != nil && *channel.ChannelProxyID > 0 {
		return proxyName(*channel.ChannelProxyID)
	}
	if channel != nil && channel.Proxy {
		return "system"
	}
	return "none"
}

func proxyName(id int) string {
	proxy, err := op.ProxyGet(id)
	if err != nil || strings.TrimSpace(proxy.Name) == "" {
		return fmt.Sprintf("proxy#%d", id)
	}
	return proxy.Name
}

// MaskKeySuffix 脱敏 Key，仅保留 *{最后3位}。
// 若 Key 长度 <= 3 则直接返回 * 掩码。
func MaskKeySuffix(key string) string {
	if len(key) <= 3 {
		return "*" + key
	}
	return "*" + key[len(key)-3:]
}

func ChannelBaseUrlDelayUpdate(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	newBaseUrls := make([]model.BaseUrl, 0, len(channel.BaseUrls))
	for _, baseUrl := range channel.BaseUrls {
		if baseUrl.URL == "" {
			continue
		}
		httpClient, err := ChannelHttpClient(channel)
		if err != nil {
			log.Warnf("failed to get http client (channel=%d): %v", channel.ID, err)
			continue
		}
		delay, err := GetUrlDelay(httpClient, baseUrl.URL, ctx)
		if err != nil {
			log.Warnf("failed to get url delay (channel=%d): %v", channel.ID, err)
			continue
		}
		newBaseUrls = append(newBaseUrls, model.BaseUrl{
			URL:   baseUrl.URL,
			Delay: delay,
		})
	}
	if len(newBaseUrls) > 0 {
		op.ChannelBaseUrlUpdate(channel.ID, newBaseUrls)
	}
}

func ChannelAutoGroup(channel *model.Channel, ctx context.Context) {
	if channel == nil {
		return
	}
	if channel.AutoGroup == model.AutoGroupTypeNone {
		return
	}
	groups, err := op.GroupList(ctx)
	if err != nil {
		log.Warnf("get group list failed: %v", err)
		return
	}

	channelModelNames := xstrings.SplitTrimCompact(",", channel.Model, channel.CustomModel)
	if len(channelModelNames) == 0 {
		return
	}

	for _, group := range groups {
		matchedModelNames := make([]string, 0, len(channelModelNames))

		switch channel.AutoGroup {
		case model.AutoGroupTypeExact:
			for _, modelName := range channelModelNames {
				if strings.EqualFold(modelName, group.Name) {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}

		case model.AutoGroupTypeFuzzy:
			groupNameLower := strings.ToLower(strings.TrimSpace(group.Name))
			if groupNameLower == "" {
				continue
			}
			for _, modelName := range channelModelNames {
				if strings.Contains(strings.ToLower(modelName), groupNameLower) {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}

		case model.AutoGroupTypeRegex:
			if group.MatchRegex == "" {
				for _, modelName := range channelModelNames {
					if strings.EqualFold(modelName, group.Name) {
						matchedModelNames = append(matchedModelNames, modelName)
					}
				}
				break
			}

			re, err := regexp2.Compile(group.MatchRegex, regexp2.ECMAScript)
			if err != nil {
				log.Warnf("compile regex failed (channel=%d group=%d regex=%q): %v", channel.ID, group.ID, group.MatchRegex, err)
				continue
			}
			for _, modelName := range channelModelNames {
				matched, err := re.MatchString(modelName)
				if err != nil {
					log.Warnf("match regex failed (channel=%d group=%d regex=%q model=%q): %v", channel.ID, group.ID, group.MatchRegex, modelName, err)
					continue
				}
				if matched {
					matchedModelNames = append(matchedModelNames, modelName)
				}
			}
		}

		if len(matchedModelNames) > 0 {
			items := make([]model.GroupIDAndLLMName, 0, len(matchedModelNames))
			for _, modelName := range matchedModelNames {
				items = append(items, model.GroupIDAndLLMName{
					ChannelID: channel.ID,
					ModelName: modelName,
				})
			}
			if err := op.GroupItemBatchAdd(group.ID, items, ctx); err != nil {
				log.Warnf("group item batch add failed (channel=%d group=%d): %v", channel.ID, group.ID, err)
			}
		}
	}
}
