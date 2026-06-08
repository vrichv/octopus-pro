package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/server/middleware"
	"github.com/vrichv/octopus-pro/internal/server/resp"
	"github.com/vrichv/octopus-pro/internal/server/router"
	"github.com/gin-gonic/gin"
)

func init() {
	router.NewGroupRouter("/api/v1/log").
		Use(middleware.Auth()).
		AddRoute(
			router.NewRoute("/list", http.MethodGet).
				Handle(listLog),
		).
		AddRoute(
			router.NewRoute("/clear", http.MethodDelete).
				Handle(clearLog),
		).
		AddRoute(
			router.NewRoute("/stream-token", http.MethodGet).
				Handle(getStreamToken),
		).
		AddRoute(
			router.NewRoute("/export-analysis", http.MethodGet).
				Handle(exportAnalysis),
		)

	router.NewGroupRouter("/api/v1/log").
		AddRoute(
			router.NewRoute("/stream", http.MethodGet).
				Handle(streamLog),
		)
}

func listLog(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	startTimeStr := c.Query("start_time")
	endTimeStr := c.Query("end_time")

	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var startTime, endTime *int
	if startTimeStr != "" && endTimeStr != "" {
		st, err := strconv.Atoi(startTimeStr)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		et, err := strconv.Atoi(endTimeStr)
		if err != nil {
			resp.Error(c, http.StatusBadRequest, err.Error())
			return
		}
		startTime = &st
		endTime = &et
	}

	logs, err := op.RelayLogList(c.Request.Context(), startTime, endTime, page, pageSize)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	resp.Success(c, logs)
}

func clearLog(c *gin.Context) {
	if err := op.RelayLogClear(c.Request.Context()); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, nil)
}

func getStreamToken(c *gin.Context) {
	token, err := op.RelayLogStreamTokenCreate()
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Success(c, gin.H{"token": token})
}

func streamLog(c *gin.Context) {
	token := c.Query("token")
	if token == "" || !op.RelayLogStreamTokenVerify(token) {
		resp.Error(c, http.StatusUnauthorized, "invalid stream token")
		return
	}

	op.RelayLogStreamTokenRevoke(token)

	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	logChan := op.RelayLogSubscribe()
	defer op.RelayLogUnsubscribe(logChan)

	ctx := c.Request.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case log, ok := <-logChan:
			if !ok {
				return
			}
			data, err := json.Marshal(log)
			if err != nil {
				continue
			}
			c.Writer.Write([]byte(fmt.Sprintf("data: %s\n\n", data)))
			c.Writer.Flush()
		}
	}
}

// exportLogEntry is a simplified log entry for AI analysis export.
type exportLogEntry struct {
	Time        int64                   `json:"time"`
	ChannelID   int                     `json:"channel_id"`
	ChannelName string                  `json:"channel_name"`
	Model       string                  `json:"model"`
	UseTime     int                     `json:"use_time"`
	Ftut        int                     `json:"ftut"`
	Success     bool                    `json:"success"`
	Error       string                  `json:"error"`
	Attempts    []model.ChannelAttempt  `json:"attempts"`
}

type exportChannelSummary struct {
	ChannelID   int            `json:"channel_id"`
	ChannelName string         `json:"channel_name"`
	Total       int            `json:"total"`
	Success     int            `json:"success"`
	Failed      int            `json:"failed"`
	Count429    int            `json:"429_count"`
	AvgUseTime  int            `json:"avg_use_time_ms"`
	Hourly429   map[int]int    `json:"429_hourly"`
}

type exportModelSummary struct {
	ModelName        string   `json:"model_name"`
	Total            int      `json:"total"`
	Count429         int      `json:"429_count"`
	ChannelsAffected []string `json:"channels_affected"`
}

type exportPayload struct {
	ExportedAt   string                `json:"exported_at"`
	TimeRange    struct{ Start, End int64 } `json:"time_range"`
	TotalRecords int                   `json:"total_records"`
	Summary      struct {
		TotalRequests     int                     `json:"total_requests"`
		Success           int                     `json:"success"`
		Failed            int                     `json:"failed"`
		Count429          int                     `json:"429_count"`
		CircuitBreakCount int                     `json:"circuit_break_count"`
		TimeoutCount      int                     `json:"timeout_count"`
		Channels          []exportChannelSummary  `json:"channels"`
		Models            []exportModelSummary    `json:"models"`
	} `json:"summary"`
	Records []exportLogEntry `json:"records"`
}

func exportAnalysis(c *gin.Context) {
	hoursStr := c.DefaultQuery("hours", "24")
	hours, err := strconv.Atoi(hoursStr)
	if err != nil || hours < 1 || hours > 96 {
		resp.Error(c, http.StatusBadRequest, "invalid hours parameter")
		return
	}

	now := time.Now()
	endTime := now.Unix()
	startTime := now.Add(-time.Duration(hours) * time.Hour).Unix()

	logs, err := op.RelayLogListAll(c.Request.Context(), startTime, endTime)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	payload := buildExportPayload(logs, startTime, endTime, now)

	filename := fmt.Sprintf("data/export_analysis_%s.json", now.Format("20060102_150405"))
	os.MkdirAll("data", 0755)
	file, err := os.Create(filename)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(payload); err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}

	resp.Success(c, gin.H{
		"file":    filename,
		"records": len(logs),
	})
}

func buildExportPayload(logs []model.RelayLog, startTime, endTime int64, now time.Time) exportPayload {
	payload := exportPayload{
		ExportedAt: now.Format(time.RFC3339),
		TimeRange: struct{ Start, End int64 }{Start: startTime, End: endTime},
		TotalRecords: len(logs),
		Records: make([]exportLogEntry, 0, len(logs)),
	}

	// Channel aggregates
	type channelAgg struct {
		channelID int
		total     int
		success   int
		failed    int
		count429  int
		useTime   int
		hourly429 map[int]int
	}
	channelMap := make(map[string]*channelAgg)

	// Model aggregates
	type modelAgg struct {
		total    int
		count429 int
		channels map[string]struct{}
	}
	modelMap := make(map[string]*modelAgg)

	circuitBreakCount := 0
	timeoutCount := 0
	totalSuccess := 0
	totalFailed := 0
	total429 := 0

	for _, log := range logs {
		success := log.Error == ""
		entry := exportLogEntry{
			Time:        log.Time,
			ChannelID:   log.ChannelId,
			ChannelName: log.ChannelName,
			Model:       log.ActualModelName,
			UseTime:     log.UseTime,
			Ftut:        log.Ftut,
			Success:     success,
			Error:       log.Error,
			Attempts:    log.Attempts,
		}
		payload.Records = append(payload.Records, entry)

		if success {
			totalSuccess++
		} else {
			totalFailed++
		}

		// Channel aggregation
		chKey := log.ChannelName
		if chKey == "" {
			chKey = fmt.Sprintf("ch-%d", log.ChannelId)
		}
		ch, ok := channelMap[chKey]
		if !ok {
			ch = &channelAgg{channelID: log.ChannelId, hourly429: make(map[int]int)}
			channelMap[chKey] = ch
		}
		ch.total++
		if success {
			ch.success++
		} else {
			ch.failed++
		}
		ch.useTime += log.UseTime

		// Model aggregation
		modelName := log.ActualModelName
		if modelName == "" {
			modelName = log.RequestModelName
		}
		m, ok := modelMap[modelName]
		if !ok {
			m = &modelAgg{channels: make(map[string]struct{})}
			modelMap[modelName] = m
		}
		m.total++
		m.channels[log.ChannelName] = struct{}{}

		// Check attempts for 429 and circuit_break
		for _, a := range log.Attempts {
			if a.Status == model.AttemptCircuitBreak {
				circuitBreakCount++
				continue
			}
			if a.Status == model.AttemptFailed {
				if is429(a.Msg) {
					total429++
					ch.count429++
					m.count429++
					hour := time.Unix(log.Time, 0).Hour()
					ch.hourly429[hour]++
				}
			}
		}

		// Check for timeout
		if log.Error != "" {
			if containsTimeout(log.Error) {
				timeoutCount++
			}
		}
	}

	payload.Summary.TotalRequests = len(logs)
	payload.Summary.Success = totalSuccess
	payload.Summary.Failed = totalFailed
	payload.Summary.Count429 = total429
	payload.Summary.CircuitBreakCount = circuitBreakCount
	payload.Summary.TimeoutCount = timeoutCount

	for chKey, agg := range channelMap {
		avgUse := 0
		if agg.total > 0 {
			avgUse = agg.useTime / agg.total
		}
		payload.Summary.Channels = append(payload.Summary.Channels, exportChannelSummary{
			ChannelID:   agg.channelID,
			ChannelName: chKey,
			Total:       agg.total,
			Success:     agg.success,
			Failed:      agg.failed,
			Count429:    agg.count429,
			AvgUseTime:  avgUse,
			Hourly429:   agg.hourly429,
		})
	}

	for modelName, agg := range modelMap {
		chAffected := make([]string, 0, len(agg.channels))
		for ch := range agg.channels {
			chAffected = append(chAffected, ch)
		}
		payload.Summary.Models = append(payload.Summary.Models, exportModelSummary{
			ModelName:        modelName,
			Total:            agg.total,
			Count429:         agg.count429,
			ChannelsAffected: chAffected,
		})
	}

	return payload
}
func is429(msg string) bool {
	return len(msg) > 0 && (strings.Contains(msg, "429") || strings.Contains(msg, "Too Many Requests"))
}

func containsTimeout(s string) bool {
	return strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled")
}
