package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/vrichv/octopus-pro/internal/db"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
)

func TestBuildExportPayloadSortsAggregateOutput(t *testing.T) {
	logs := []model.RelayLog{
		{Time: 2, ChannelId: 2, ChannelName: "beta", ActualModelName: "z-model", UseTime: 20},
		{Time: 1, ChannelId: 1, ChannelName: "alpha", ActualModelName: "a-model", UseTime: 10},
	}
	payload := buildExportPayload(logs, 0, 3, time.Unix(3, 0))
	if payload.TotalRecords != 2 || payload.Summary.TotalRequests != 2 || payload.Summary.Success != 2 {
		t.Fatalf("unexpected totals: %+v", payload)
	}
	if len(payload.Summary.Channels) != 2 || payload.Summary.Channels[0].ChannelName != "alpha" || payload.Summary.Channels[1].ChannelName != "beta" {
		t.Fatalf("channels are not deterministic: %+v", payload.Summary.Channels)
	}
	if len(payload.Summary.Models) != 2 || payload.Summary.Models[0].ModelName != "a-model" || payload.Summary.Models[1].ModelName != "z-model" {
		t.Fatalf("models are not deterministic: %+v", payload.Summary.Models)
	}
}

func TestBuildExportPayloadClassifiesResilienceSignals(t *testing.T) {
	logs := []model.RelayLog{
		{
			Time:             10,
			ChannelId:        7,
			ChannelName:      "resilience-channel",
			RequestModelName: "model-A",
			ActualModelName:  "model-A",
			Error:            "first token timeout (30s): context deadline exceeded",
			Attempts: []model.ChannelAttempt{
				{ChannelID: 7, ChannelName: "resilience-channel", ModelName: "model-A", Status: model.AttemptFailed, Msg: "Request failed: Too Many Requests (429)"},
				{ChannelID: 7, ChannelName: "resilience-channel", ModelName: "model-A", Status: model.AttemptCircuitBreak, Msg: "circuit breaker tripped"},
			},
		},
	}
	payload := buildExportPayload(logs, 0, 20, time.Unix(20, 0))
	if payload.Summary.Count429 != 1 || payload.Summary.CircuitBreakCount != 1 || payload.Summary.TimeoutCount != 1 {
		t.Fatalf("resilience summary classification=%+v", payload.Summary)
	}
	if len(payload.Summary.Channels) != 1 || payload.Summary.Channels[0].Count429 != 1 {
		t.Fatalf("resilience channel classification=%+v", payload.Summary.Channels)
	}
	if len(payload.Summary.Models) != 1 || payload.Summary.Models[0].Count429 != 1 || payload.Summary.Models[0].ChannelsAffected[0] != "resilience-channel" {
		t.Fatalf("resilience model classification=%+v", payload.Summary.Models)
	}
}

func TestExportAnalysisStreamsAttachment(t *testing.T) {
	if db.GetDB() != nil {
		_ = db.Close()
	}
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "logs.db"), false); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("InitCache: %v", err)
	}

	now := time.Now().Unix()
	if err := db.GetDB().WithContext(context.Background()).Create(&model.RelayLog{
		ID: 1, Time: now, ChannelId: 1, ChannelName: "primary", ActualModelName: "gpt-test",
	}).Error; err != nil {
		t.Fatalf("create relay log: %v", err)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/log/export-analysis?hours=1", nil)
	exportAnalysis(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if disposition := recorder.Header().Get("Content-Disposition"); disposition == "" {
		t.Fatal("missing attachment content disposition")
	}
	var payload exportPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("download is not valid JSON: %v; body=%s", err, recorder.Body.String())
	}
	if payload.TotalRecords != 1 || len(payload.Records) != 1 || payload.Records[0].ChannelName != "primary" {
		t.Fatalf("unexpected export payload: %+v", payload)
	}
}
