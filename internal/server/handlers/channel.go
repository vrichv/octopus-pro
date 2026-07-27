package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/vrichv/octopus-pro/internal/helper"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
	"github.com/vrichv/octopus-pro/internal/relay/plugins"
	"github.com/vrichv/octopus-pro/internal/server/middleware"
	"github.com/vrichv/octopus-pro/internal/server/resp"
	"github.com/vrichv/octopus-pro/internal/server/router"
	"github.com/vrichv/octopus-pro/internal/task"
)

func init() {
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		Use(middleware.RequireJSON()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listChannel),
		).
		AddRoute(
			router.NewRoute("/create", http.MethodPost).
				Handle(createChannel),
		).
		AddRoute(
			router.NewRoute("/update", http.MethodPost).
				Handle(updateChannel),
		).
		AddRoute(
			router.NewRoute("/enable", http.MethodPost).
				Handle(enableChannel),
		).
		AddRoute(
			router.NewRoute("/delete/:id", http.MethodDelete).
				Handle(deleteChannel),
		).
		AddRoute(
			router.NewRoute("/fetch-model", http.MethodPost).
				Handle(fetchModel),
		)
	router.NewGroupRouter("/api/v1/channel").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/sync", http.MethodPost).
				Handle(syncChannel),
		).
		AddRoute(
			router.NewRoute("/last-sync-time", http.MethodGet).
				Handle(getLastSyncTime),
		).
		AddRoute(
			router.NewRoute("/:id/scheduling-status", http.MethodGet).
				Handle(getSchedulingStatus),
		)
}

func listChannel(c *gin.Context) {
	channels, err := op.ChannelList(c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	for i, channel := range channels {
		stats := op.StatsChannelGet(channel.ID)
		channels[i].Stats = &stats
	}
	resp.Success(c, channels)
}

func createChannel(c *gin.Context) {
	var channel model.Channel
	if err := c.ShouldBindJSON(&channel); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.ChannelCreate(&channel, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	stats := op.StatsChannelGet(channel.ID)
	channel.Stats = &stats
	go func(channel *model.Channel) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		modelStr := channel.Model + "," + channel.CustomModel
		modelArray := strings.Split(modelStr, ",")
		helper.LLMPriceAddToDB(modelArray, ctx)
		helper.ChannelBaseUrlDelayUpdate(channel, ctx)
		helper.ChannelAutoGroup(channel, ctx)
	}(&channel)
	resp.Success(c, channel)
}

func updateChannel(c *gin.Context) {
	var req model.ChannelUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	channel, err := op.ChannelUpdate(&req, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	stats := op.StatsChannelGet(channel.ID)
	channel.Stats = &stats
	go func(channel *model.Channel) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		modelStr := channel.Model + "," + channel.CustomModel
		modelArray := strings.Split(modelStr, ",")
		helper.LLMPriceAddToDB(modelArray, ctx)
		helper.ChannelBaseUrlDelayUpdate(channel, ctx)
		helper.ChannelAutoGroup(channel, ctx)
	}(channel)
	resp.Success(c, channel)
}

func enableChannel(c *gin.Context) {
	var request struct {
		ID      int  `json:"id"`
		Enabled bool `json:"enabled"`
	}
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	if err := op.ChannelEnabled(request.ID, request.Enabled, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func deleteChannel(c *gin.Context) {
	id := c.Param("id")
	idNum, err := strconv.Atoi(id)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}
	if err := op.ChannelDel(idNum, c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}
func fetchModel(c *gin.Context) {
	var request model.Channel
	if err := c.ShouldBindJSON(&request); err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidJSON)
		return
	}
	models, err := helper.FetchModels(c.Request.Context(), request)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, models)
}

func syncChannel(c *gin.Context) {
	task.SyncModelsTask()
	resp.Success(c, nil)
}

func getLastSyncTime(c *gin.Context) {
	time := task.GetLastSyncModelsTime()
	resp.Success(c, time)
}

// schedulingStatusResponse 调度状态查询响应。
type schedulingStatusResponse struct {
	ChannelID int                  `json:"channel_id"`
	Keys      []keyStatusEntry     `json:"keys"`
}

type keyStatusEntry struct {
	KeyID     int                  `json:"key_id"`
	KeySuffix string               `json:"key_suffix"`
	Models    []modelStatusEntry   `json:"models"`
}

type modelStatusEntry struct {
	Model           string                       `json:"model"`
	CircuitBreaker  balancer.CircuitBreakerStatus `json:"circuit_breaker"`
	RateLimit       rateLimitStatus              `json:"rate_limit"`
	Cooldown        model.CooldownStatus          `json:"cooldown"`
}

type rateLimitStatus struct {
	KeyLimit   *plugins.RateLimiterStatus `json:"key_limit,omitempty"`
	ModelLimit *plugins.RateLimiterStatus `json:"model_limit,omitempty"`
}

func getSchedulingStatus(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		resp.Error(c, http.StatusBadRequest, resp.ErrInvalidParam)
		return
	}

	channel, err := op.ChannelGet(id, c.Request.Context())
	if err != nil {
		resp.Error(c, http.StatusNotFound, "channel not found")
		return
	}

	// Collect all model names for this channel
	modelNames := collectModelNames(channel.Model, channel.CustomModel)

	// Parse model rate limit config
	modelLimits := plugins.ParseModelRateLimit(channel.ModelRateLimit)

	// Parse key rate limit config
	var keyLimitCount int
	var keyLimitInterval time.Duration
	if channel.RateLimit != "" {
		keyLimitCount, keyLimitInterval, _ = plugins.ParseRateSpec(channel.RateLimit)
	}

	var keys []keyStatusEntry
	for _, k := range channel.Keys {
		if k.ID == 0 || k.ChannelKey == "" || !k.Enabled {
			continue
		}

		keySuffix := k.ChannelKey
		if len(keySuffix) > 8 {
			keySuffix = "***" + keySuffix[len(keySuffix)-4:]
		}

		var models []modelStatusEntry
		for _, modelName := range modelNames {
			entry := modelStatusEntry{
				Model:          modelName,
				CircuitBreaker: balancer.GetCircuitBreakerStatus(channel.ID, k.ID, modelName),
				Cooldown:       model.GetKeyModelCooldownStatus(channel.ID, k.ID, modelName),
			}

			// Key-level rate limit
			if keyLimitCount > 0 {
				key := fmt.Sprintf("ch:%d:k:%d", channel.ID, k.ID)
				status := plugins.GetRateLimiterStatus(key, keyLimitCount, keyLimitInterval)
				entry.RateLimit.KeyLimit = &status
			}

			// Model-level rate limit
			if modelSpec, ok := modelLimits[modelName]; ok {
				if mc, md, err := plugins.ParseRateSpec(modelSpec); err == nil {
					key := fmt.Sprintf("ch:%d:m:%s", channel.ID, modelName)
					status := plugins.GetRateLimiterStatus(key, mc, md)
					entry.RateLimit.ModelLimit = &status
				}
			}

			models = append(models, entry)
		}

		keys = append(keys, keyStatusEntry{
			KeyID:     k.ID,
			KeySuffix: keySuffix,
			Models:    models,
		})
	}

	resp.Success(c, schedulingStatusResponse{
		ChannelID: channel.ID,
		Keys:      keys,
	})
}

func collectModelNames(model, customModel string) []string {
	var names []string
	for _, m := range strings.Split(model+","+customModel, ",") {
		m = strings.TrimSpace(m)
		if m != "" {
			names = append(names, m)
		}
	}
	return names
}
