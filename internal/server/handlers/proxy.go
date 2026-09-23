package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/vrichv/octopus-pro/internal/client"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/server/middleware"
	"github.com/vrichv/octopus-pro/internal/server/resp"
	"github.com/vrichv/octopus-pro/internal/server/router"
)

func init() {
	router.NewGroupRouter("/api/v1/proxy").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listProxy),
		)
	router.NewGroupRouter("/api/v1/proxy").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createProxy),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateProxy),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteProxy),
		).
		AddRoute(
			router.NewRoute("/test", http.MethodPost).
				Handle(testProxy),
		)
}

func listProxy(c *gin.Context) {
	proxies, err := op.ProxyList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, proxies)
}

func createProxy(c *gin.Context) {
	var proxy model.Proxy
	if err := c.ShouldBindJSON(&proxy); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.ProxyCreate(&proxy, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, proxy)
}

func updateProxy(c *gin.Context) {
	var req model.ProxyUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	proxy, err := op.ProxyUpdate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, proxy)
}

func deleteProxy(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.ProxyDel(id, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusBadRequest, err.Error())
		return
	}
	resp.Success(c, nil)
}

// testProxy 通过代理访问出口 IP 服务，返回连通性、耗时与出口 IP。
func testProxy(c *gin.Context) {
	var req model.ProxyTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	proxy, err := op.ProxyGet(req.ID)
	if err != nil {
		resp.Error(c, http.StatusNotFound, err.Error())
		return
	}

	httpClient, err := client.GetHTTPClientCustomProxy(proxy.URL)
	if err != nil {
		resp.Success(c, model.ProxyTestResult{Error: err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()

	start := time.Now()
	exitIP, err := fetchExitIP(ctx, httpClient)
	latency := time.Since(start).Milliseconds()
	if err != nil {
		resp.Success(c, model.ProxyTestResult{LatencyMs: latency, Error: err.Error()})
		return
	}

	country, err := fetchIPCountry(ctx, httpClient, exitIP)
	if err != nil {
		country = ""
	}
	resp.Success(c, model.ProxyTestResult{Success: true, LatencyMs: latency, IP: exitIP, Country: country})
}

var exitIPEndpoints = []string{
	// api64 reports the proxy's IPv4 or IPv6 exit address.
	"https://api64.ipify.org?format=json",
	"https://ipinfo.io/ip",
}

var exitIPCountryEndpoint = "https://ipwho.is/"

func fetchExitIP(ctx context.Context, httpClient *http.Client) (string, error) {
	var lastErr error
	for _, endpoint := range exitIPEndpoints {
		ip, err := requestExitIP(ctx, httpClient, endpoint)
		if err == nil {
			return ip, nil
		}
		lastErr = err
	}
	return "", lastErr
}

func requestExitIP(ctx context.Context, httpClient *http.Client, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned status %d", endpoint, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}

	text := strings.TrimSpace(string(body))
	if strings.HasPrefix(text, "{") {
		var payload struct {
			IP string `json:"ip"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return "", fmt.Errorf("%s returned invalid json: %w", endpoint, err)
		}
		text = strings.TrimSpace(payload.IP)
	}
	if text == "" {
		return "", fmt.Errorf("%s returned empty exit ip", endpoint)
	}
	return text, nil
}

// fetchIPCountry resolves an exit IP's country through the tested proxy.
// A country lookup failure is non-fatal: the proxy has already proven connectivity.
func fetchIPCountry(ctx context.Context, httpClient *http.Client, ip string) (string, error) {
	endpoint := exitIPCountryEndpoint + url.PathEscape(ip)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	response, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned status %d", endpoint, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		return "", err
	}
	var payload struct {
		Success bool   `json:"success"`
		Country string `json:"country"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("%s returned invalid json: %w", endpoint, err)
	}
	if !payload.Success || strings.TrimSpace(payload.Country) == "" {
		return "", fmt.Errorf("%s returned no country for %s", endpoint, ip)
	}
	return strings.TrimSpace(payload.Country), nil
}
