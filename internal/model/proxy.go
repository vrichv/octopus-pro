package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Proxy 是可复用的代理配置，渠道、渠道密钥与系统代理通过 ID 引用。
type Proxy struct {
	ID        int       `json:"id" gorm:"primaryKey"`
	Name      string    `json:"name" gorm:"unique;not null"`
	URL       string    `json:"url" gorm:"not null"`
	Remark    string    `json:"remark" gorm:"default:''"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProxyUpdateRequest 代理更新请求 - 仅包含变更的数据。
type ProxyUpdateRequest struct {
	ID     int     `json:"id" binding:"required"`
	Name   *string `json:"name,omitempty"`
	URL    *string `json:"url,omitempty"`
	Remark *string `json:"remark,omitempty"`
}

// ProxyTestRequest 代理连通性测试请求。
type ProxyTestRequest struct {
	ID int `json:"id" binding:"required"`
}

// ProxyTestResult 代理连通性测试结果，IP 与国家均来自通过该代理访问外网的出口。
type ProxyTestResult struct {
	Success   bool   `json:"success"`
	LatencyMs int64  `json:"latency_ms"`
	IP        string `json:"ip,omitempty"`
	Country   string `json:"country,omitempty"`
	Error     string `json:"error,omitempty"`
}

func (p *Proxy) Validate() error {
	p.Name = strings.TrimSpace(p.Name)
	p.URL = strings.TrimSpace(p.URL)
	p.Remark = strings.TrimSpace(p.Remark)
	if p.Name == "" {
		return fmt.Errorf("proxy name is required")
	}
	return ValidateProxyURL(p.URL)
}

// ValidateProxyURL 校验代理地址，支持 http、https、socks、socks5。
func ValidateProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("proxy url is required")
	}
	parsedURL, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("proxy url is invalid: %w", err)
	}
	validSchemes := map[string]bool{
		"http":   true,
		"https":  true,
		"socks":  true,
		"socks5": true,
	}
	if !validSchemes[parsedURL.Scheme] {
		return fmt.Errorf("proxy url scheme must be http, https, socks, or socks5")
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("proxy url must have a host")
	}
	return nil
}
