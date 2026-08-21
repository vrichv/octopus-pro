package handlers

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/server/middleware"
	"github.com/vrichv/octopus-pro/internal/server/resp"
	"github.com/vrichv/octopus-pro/internal/server/router"
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

const (
	exportPageSize   = 1000
	exportMaxRecords = 100000
)

// exportLogEntry is a simplified log entry for AI analysis export.
type exportLogEntry struct {
	Time        int64                  `json:"time"`
	ChannelID   int                    `json:"channel_id"`
	ChannelName string                 `json:"channel_name"`
	Model       string                 `json:"model"`
	UseTime     int                    `json:"use_time"`
	Ftut        int                    `json:"ftut"`
	Success     bool                   `json:"success"`
	Error       string                 `json:"error"`
	Attempts    []model.ChannelAttempt `json:"attempts"`
}

type exportChannelSummary struct {
	ChannelID   int         `json:"channel_id"`
	ChannelName string      `json:"channel_name"`
	Total       int         `json:"total"`
	Success     int         `json:"success"`
	Failed      int         `json:"failed"`
	Count429    int         `json:"429_count"`
	AvgUseTime  int         `json:"avg_use_time_ms"`
	Hourly429   map[int]int `json:"429_hourly"`
}

type exportModelSummary struct {
	ModelName        string   `json:"model_name"`
	Total            int      `json:"total"`
	Count429         int      `json:"429_count"`
	ChannelsAffected []string `json:"channels_affected"`
}

type exportPayload struct {
	ExportedAt   string                     `json:"exported_at"`
	TimeRange    struct{ Start, End int64 } `json:"time_range"`
	TotalRecords int                        `json:"total_records"`
	Summary      struct {
		TotalRequests     int                    `json:"total_requests"`
		Success           int                    `json:"success"`
		Failed            int                    `json:"failed"`
		Count429          int                    `json:"429_count"`
		CircuitBreakCount int                    `json:"circuit_break_count"`
		TimeoutCount      int                    `json:"timeout_count"`
		Channels          []exportChannelSummary `json:"channels"`
		Models            []exportModelSummary   `json:"models"`
	} `json:"summary"`
	Records []exportLogEntry `json:"records"`
}

type exportChannelAggregate struct {
	channelID int
	total     int
	success   int
	failed    int
	count429  int
	useTime   int
	hourly429 map[int]int
}

type exportModelAggregate struct {
	total    int
	count429 int
	channels map[string]struct{}
}

type exportAccumulator struct {
	payload           exportPayload
	channels          map[string]*exportChannelAggregate
	models            map[string]*exportModelAggregate
	success           int
	failed            int
	count429          int
	circuitBreakCount int
	timeoutCount      int
}

func newExportAccumulator(startTime, endTime int64, now time.Time) *exportAccumulator {
	accumulator := &exportAccumulator{
		channels: make(map[string]*exportChannelAggregate),
		models:   make(map[string]*exportModelAggregate),
	}
	accumulator.payload.ExportedAt = now.Format(time.RFC3339)
	accumulator.payload.TimeRange = struct{ Start, End int64 }{Start: startTime, End: endTime}
	return accumulator
}

func (a *exportAccumulator) add(relayLog model.RelayLog) exportLogEntry {
	success := relayLog.Error == ""
	if success {
		a.success++
	} else {
		a.failed++
	}
	channelName := relayLog.ChannelName
	if channelName == "" {
		channelName = fmt.Sprintf("ch-%d", relayLog.ChannelId)
	}
	channel, ok := a.channels[channelName]
	if !ok {
		channel = &exportChannelAggregate{channelID: relayLog.ChannelId, hourly429: make(map[int]int)}
		a.channels[channelName] = channel
	}
	channel.total++
	if success {
		channel.success++
	} else {
		channel.failed++
	}
	channel.useTime += relayLog.UseTime

	modelName := relayLog.ActualModelName
	if modelName == "" {
		modelName = relayLog.RequestModelName
	}
	modelAggregate, ok := a.models[modelName]
	if !ok {
		modelAggregate = &exportModelAggregate{channels: make(map[string]struct{})}
		a.models[modelName] = modelAggregate
	}
	modelAggregate.total++
	modelAggregate.channels[relayLog.ChannelName] = struct{}{}
	for _, attempt := range relayLog.Attempts {
		if attempt.Status == model.AttemptCircuitBreak {
			a.circuitBreakCount++
			continue
		}
		if attempt.Status == model.AttemptFailed && is429(attempt.Msg) {
			a.count429++
			channel.count429++
			modelAggregate.count429++
			channel.hourly429[time.Unix(relayLog.Time, 0).Hour()]++
		}
	}
	if relayLog.Error != "" && containsTimeout(relayLog.Error) {
		a.timeoutCount++
	}
	return exportLogEntry{
		Time: relayLog.Time, ChannelID: relayLog.ChannelId, ChannelName: relayLog.ChannelName,
		Model: relayLog.ActualModelName, UseTime: relayLog.UseTime, Ftut: relayLog.Ftut,
		Success: success, Error: relayLog.Error, Attempts: relayLog.Attempts,
	}
}

func (a *exportAccumulator) finalize(recordCount int) exportPayload {
	payload := a.payload
	payload.TotalRecords = recordCount
	payload.Summary.TotalRequests = recordCount
	payload.Summary.Success = a.success
	payload.Summary.Failed = a.failed
	payload.Summary.Count429 = a.count429
	payload.Summary.CircuitBreakCount = a.circuitBreakCount
	payload.Summary.TimeoutCount = a.timeoutCount
	for channelName, aggregate := range a.channels {
		average := 0
		if aggregate.total > 0 {
			average = aggregate.useTime / aggregate.total
		}
		payload.Summary.Channels = append(payload.Summary.Channels, exportChannelSummary{
			ChannelID: aggregate.channelID, ChannelName: channelName, Total: aggregate.total,
			Success: aggregate.success, Failed: aggregate.failed, Count429: aggregate.count429,
			AvgUseTime: average, Hourly429: aggregate.hourly429,
		})
	}
	sort.Slice(payload.Summary.Channels, func(i, j int) bool {
		return payload.Summary.Channels[i].ChannelName < payload.Summary.Channels[j].ChannelName
	})
	for modelName, aggregate := range a.models {
		channels := make([]string, 0, len(aggregate.channels))
		for channelName := range aggregate.channels {
			channels = append(channels, channelName)
		}
		sort.Strings(channels)
		payload.Summary.Models = append(payload.Summary.Models, exportModelSummary{
			ModelName: modelName, Total: aggregate.total, Count429: aggregate.count429, ChannelsAffected: channels,
		})
	}
	sort.Slice(payload.Summary.Models, func(i, j int) bool { return payload.Summary.Models[i].ModelName < payload.Summary.Models[j].ModelName })
	return payload
}

func exportAnalysis(c *gin.Context) {
	hours, err := strconv.Atoi(c.DefaultQuery("hours", "24"))
	if err != nil || hours < 1 || hours > 96 {
		resp.Error(c, http.StatusBadRequest, "invalid hours parameter")
		return
	}
	now := time.Now()
	startTime := now.Add(-time.Duration(hours) * time.Hour).Unix()
	endTime := now.Unix()
	persistedCount, err := op.RelayLogCountInRange(c.Request.Context(), startTime, endTime)
	if err != nil {
		resp.Error(c, http.StatusInternalServerError, err.Error())
		return
	}
	cachedLogs := op.RelayLogCachedInRange(startTime, endTime)
	if persistedCount+int64(len(cachedLogs)) > exportMaxRecords {
		resp.Error(c, http.StatusRequestEntityTooLarge, "export exceeds 100000 records; reduce the time range")
		return
	}

	c.Header("Content-Type", "application/json; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=export_analysis_%s.json", now.Format("20060102_150405")))
	accumulator := newExportAccumulator(startTime, endTime, now)
	writer := c.Writer
	exportedAt, _ := json.Marshal(accumulator.payload.ExportedAt)
	timeRange, _ := json.Marshal(accumulator.payload.TimeRange)
	if _, err := fmt.Fprintf(writer, `{"exported_at":%s,"time_range":%s,"records":[`, exportedAt, timeRange); err != nil {
		return
	}
	firstRecord := true
	recordCount := 0
	emit := func(relayLog model.RelayLog) error {
		entry, err := json.Marshal(accumulator.add(relayLog))
		if err != nil {
			return err
		}
		if !firstRecord {
			if _, err := io.WriteString(writer, ","); err != nil {
				return err
			}
		}
		firstRecord = false
		if _, err := writer.Write(entry); err != nil {
			return err
		}
		recordCount++
		return nil
	}
	cacheIDs := make(map[int64]struct{}, len(cachedLogs))
	for _, relayLog := range cachedLogs {
		cacheIDs[relayLog.ID] = struct{}{}
		if err := emit(relayLog); err != nil {
			return
		}
	}
	var beforeID int64
	for {
		page, err := op.RelayLogListPage(c.Request.Context(), startTime, endTime, beforeID, exportPageSize)
		if err != nil {
			return
		}
		if len(page) == 0 {
			break
		}
		beforeID = page[len(page)-1].ID
		for _, relayLog := range page {
			if _, cached := cacheIDs[relayLog.ID]; cached {
				continue
			}
			if err := emit(relayLog); err != nil {
				return
			}
		}
		if len(page) < exportPageSize {
			break
		}
	}
	payload := accumulator.finalize(recordCount)
	summary, err := json.Marshal(payload.Summary)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(writer, `],"summary":%s,"total_records":%d}`, summary, payload.TotalRecords)
}

func buildExportPayload(logs []model.RelayLog, startTime, endTime int64, now time.Time) exportPayload {
	accumulator := newExportAccumulator(startTime, endTime, now)
	payload := exportPayload{Records: make([]exportLogEntry, 0, len(logs))}
	for _, relayLog := range logs {
		payload.Records = append(payload.Records, accumulator.add(relayLog))
	}
	finalized := accumulator.finalize(len(logs))
	finalized.Records = payload.Records
	return finalized
}

func is429(msg string) bool {
	return len(msg) > 0 && (strings.Contains(msg, "429") || strings.Contains(msg, "Too Many Requests"))
}

func containsTimeout(s string) bool {
	return strings.Contains(s, "timeout") || strings.Contains(s, "deadline") || strings.Contains(s, "context canceled")
}
