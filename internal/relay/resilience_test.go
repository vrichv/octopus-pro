package relay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/vrichv/octopus-pro/internal/db"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
	"github.com/vrichv/octopus-pro/internal/relay/plugins"
)

const (
	faultJSONSuccess        = "json_success"
	faultSSESuccess         = "sse_success"
	faultHTTPStatus         = "http_status"
	faultDelayHeaders       = "delay_headers"
	faultDelayFirstEvent    = "delay_first_event"
	faultIdleAfterEvent     = "idle_after_event"
	faultNeverEnd           = "never_end"
	faultDisconnectBefore   = "disconnect_before_event"
	faultDisconnectAfter    = "disconnect_after_event"
	faultSuccessShapedError = "success_shaped_error"
	faultContentFilter      = "content_filter"
	resilienceRequestHeader = "X-Resilience-Trace"
	resilienceAPIKeyBase    = "resilience-api-key-"
)

type harnessOptions struct {
	DurationUnit              time.Duration
	GroupMode                 dbmodel.GroupMode
	KeyMode                   int
	FirstTokenTimeOut         int
	UpstreamTimeOut           int
	StreamIdleTimeOut         int
	StreamHardTimeOut         int
	RateLimitRetryWaitMax     *int
	RateLimit                 string
	ModelRateLimit            string
	CircuitBreakerThreshold   *int
	CircuitBreakerCooldown    *int
	CircuitBreakerMaxCooldown *int
}

type faultStep struct {
	Kind         string   `json:"kind"`
	Status       int      `json:"status,omitempty"`
	DelaySeconds int      `json:"delay_seconds,omitempty"`
	RetryAfter   string   `json:"retry_after,omitempty"`
	Chunks       []string `json:"chunks,omitempty"`
}

type upstreamRequest struct {
	TraceID   string    `json:"trace_id"`
	Channel   string    `json:"channel"`
	ChannelID int       `json:"channel_id"`
	Key       string    `json:"key"`
	Model     string    `json:"model"`
	URL       string    `json:"url"`
	Stream    bool      `json:"stream"`
	Action    string    `json:"action"`
	Status    int       `json:"status"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
}

type resilienceUpstream struct {
	name         string
	channelID    int
	durationUnit time.Duration

	mu       sync.Mutex
	faults   map[string][]faultStep
	scripts  map[string][]faultStep
	requests []upstreamRequest
	server   *httptest.Server
}

type resilienceChannel struct {
	channel dbmodel.Channel
	keys    []dbmodel.ChannelKey
}

type resilienceHarness struct {
	t          *testing.T
	name       string
	opts       harnessOptions
	models     []string
	apiKey     dbmodel.APIKey
	channels   [2]resilienceChannel
	upstreams  [2]*resilienceUpstream
	requestSeq atomic.Uint64
}

type relayObservation struct {
	TraceID      string
	Model        string
	Stream       bool
	Status       int
	Headers      http.Header
	Body         string
	Calls        []upstreamRequest
	RelayLog     dbmodel.RelayLog
	HasRelayLog  bool
	Reproduction string
}

var resilienceScenarioSeq atomic.Uint64

func newResilienceHarness(t *testing.T, name string, opts harnessOptions) *resilienceHarness {
	t.Helper()
	if opts.DurationUnit <= 0 {
		opts.DurationUnit = 10 * time.Millisecond
	}
	if opts.GroupMode == 0 {
		opts.GroupMode = dbmodel.GroupModeFailover
	}
	if opts.CircuitBreakerThreshold == nil {
		v := 5
		opts.CircuitBreakerThreshold = &v
	}
	if opts.CircuitBreakerCooldown == nil {
		v := 60
		opts.CircuitBreakerCooldown = &v
	}
	if opts.CircuitBreakerMaxCooldown == nil {
		v := 600
		opts.CircuitBreakerMaxCooldown = &v
	}

	id := int(resilienceScenarioSeq.Add(1))
	base := 100000 + id*10000
	h := &resilienceHarness{
		t:      t,
		name:   fmt.Sprintf("%s-%d", name, id),
		opts:   opts,
		models: []string{"model-A", "model-B", "model-C"},
	}
	for i := range h.upstreams {
		channelID := base + i + 1
		u := &resilienceUpstream{
			name:         fmt.Sprintf("%s-channel-%d", h.name, i+1),
			channelID:    channelID,
			durationUnit: opts.DurationUnit,
			faults:       make(map[string][]faultStep),
			scripts:      make(map[string][]faultStep),
		}
		u.server = httptest.NewServer(http.HandlerFunc(u.serveHTTP))
		h.upstreams[i] = u
	}

	if db.GetDB() != nil {
		_ = db.Close()
	}
	dsn := filepath.Join(t.TempDir(), "resilience.db")
	if err := db.InitDB("sqlite", dsn, false); err != nil {
		t.Fatalf("init resilience sqlite: %v", err)
	}
	ctx := context.Background()
	settings := dbmodel.DefaultSettings()
	if err := db.GetDB().WithContext(ctx).Create(&settings).Error; err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	for i := range h.channels {
		channelID := base + i + 1
		channel := dbmodel.Channel{
			ID:                        channelID,
			Name:                      h.upstreams[i].name,
			Type:                      llm.APIFormatOpenAIChatCompletion,
			Enabled:                   true,
			BaseUrls:                  []dbmodel.BaseUrl{{URL: h.upstreams[i].server.URL}},
			Model:                     h.models[0],
			CustomModel:               strings.Join(h.models[1:], ","),
			KeyMode:                   opts.KeyMode,
			RateLimit:                 opts.RateLimit,
			ModelRateLimit:            opts.ModelRateLimit,
			CircuitBreakerThreshold:   opts.CircuitBreakerThreshold,
			CircuitBreakerCooldown:    opts.CircuitBreakerCooldown,
			CircuitBreakerMaxCooldown: opts.CircuitBreakerMaxCooldown,
		}
		if err := db.GetDB().WithContext(ctx).Create(&channel).Error; err != nil {
			t.Fatalf("seed channel %d: %v", i+1, err)
		}
		keys := make([]dbmodel.ChannelKey, 0, 10)
		for keyIndex := 0; keyIndex < 10; keyIndex++ {
			key := dbmodel.ChannelKey{
				ID:         base*100 + i*10 + keyIndex + 1,
				ChannelID:  channelID,
				Enabled:    true,
				ChannelKey: fmt.Sprintf("%s-%d-key-%d", resilienceAPIKeyBase, id, i*10+keyIndex),
				TotalCost:  float64(keyIndex),
			}
			if keyIndex > 0 {
				key.TotalCost = float64(100 + keyIndex)
			}
			if err := db.GetDB().WithContext(ctx).Create(&key).Error; err != nil {
				t.Fatalf("seed channel %d key %d: %v", i+1, keyIndex, err)
			}
			keys = append(keys, key)
		}
		h.channels[i] = resilienceChannel{channel: channel, keys: keys}
	}

	for modelIndex, modelName := range h.models {
		groupID := base + 100 + modelIndex
		group := dbmodel.Group{
			ID:                    groupID,
			Name:                  modelName,
			Mode:                  opts.GroupMode,
			FirstTokenTimeOut:     opts.FirstTokenTimeOut,
			UpstreamTimeOut:       opts.UpstreamTimeOut,
			StreamIdleTimeOut:     opts.StreamIdleTimeOut,
			StreamHardTimeOut:     opts.StreamHardTimeOut,
			RateLimitRetryWaitMax: opts.RateLimitRetryWaitMax,
		}
		if err := db.GetDB().WithContext(ctx).Create(&group).Error; err != nil {
			t.Fatalf("seed group %s: %v", modelName, err)
		}
		items := []dbmodel.GroupItem{
			{ID: base + 1000 + modelIndex*10 + 1, GroupID: groupID, ChannelID: h.channels[0].channel.ID, ModelName: modelName, Priority: 1, Weight: 1},
			{ID: base + 1000 + modelIndex*10 + 2, GroupID: groupID, ChannelID: h.channels[1].channel.ID, ModelName: modelName, Priority: 2, Weight: 1},
		}
		if err := db.GetDB().WithContext(ctx).Create(&items).Error; err != nil {
			t.Fatalf("seed group items %s: %v", modelName, err)
		}
	}

	h.apiKey = dbmodel.APIKey{
		ID:              base + 3000,
		Name:            "resilience-api-key-" + h.name,
		APIKey:          "client-" + h.name,
		Enabled:         true,
		SupportedModels: strings.Join(h.models, ","),
	}
	if err := db.GetDB().WithContext(ctx).Create(&h.apiKey).Error; err != nil {
		t.Fatalf("seed API key: %v", err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatalf("init resilience cache: %v", err)
	}

	t.Cleanup(func() {
		for _, u := range h.upstreams {
			if u != nil && u.server != nil {
				u.server.Close()
			}
		}
		if db.GetDB() != nil {
			_ = db.Close()
		}
	})
	return h
}

func validFaultKind(kind string) bool {
	switch kind {
	case faultJSONSuccess, faultSSESuccess, faultHTTPStatus, faultDelayHeaders,
		faultDelayFirstEvent, faultIdleAfterEvent, faultNeverEnd,
		faultDisconnectBefore, faultDisconnectAfter, faultSuccessShapedError,
		faultContentFilter:
		return true
	default:
		return false
	}
}

func (h *resilienceHarness) setFault(channelIndex, keyIndex int, modelName string, steps ...faultStep) {
	h.t.Helper()
	if channelIndex < 1 || channelIndex > len(h.upstreams) {
		h.t.Fatalf("invalid channel index %d", channelIndex)
	}
	if keyIndex < 0 || keyIndex >= len(h.channels[channelIndex-1].keys) {
		h.t.Fatalf("invalid key index %d", keyIndex)
	}
	for _, step := range steps {
		if !validFaultKind(step.Kind) {
			h.t.Fatalf("invalid fault kind %q", step.Kind)
		}
	}
	key := h.channels[channelIndex-1].keys[keyIndex].ChannelKey
	u := h.upstreams[channelIndex-1]
	queue := make([]faultStep, len(steps))
	copy(queue, steps)
	history := make([]faultStep, len(steps))
	copy(history, steps)
	u.mu.Lock()
	u.faults[faultKey(key, modelName)] = queue
	u.scripts[faultKey(key, modelName)] = history
	u.mu.Unlock()
}

func faultKey(key, modelName string) string {
	return key + "\x00" + modelName
}

func (u *resilienceUpstream) nextFault(key, modelName string, stream bool) faultStep {
	u.mu.Lock()
	defer u.mu.Unlock()
	queue := u.faults[faultKey(key, modelName)]
	if len(queue) == 0 {
		if stream {
			return faultStep{Kind: faultSSESuccess}
		}
		return faultStep{Kind: faultJSONSuccess}
	}
	step := queue[0]
	u.faults[faultKey(key, modelName)] = queue[1:]
	return step
}

func (u *resilienceUpstream) serveHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	body, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	traceID := r.Header.Get(resilienceRequestHeader)
	step := u.nextFault(key, request.Model, request.Stream)
	if step.Kind == "" {
		if request.Stream {
			step.Kind = faultSSESuccess
		} else {
			step.Kind = faultJSONSuccess
		}
	}
	recordStatus := http.StatusOK
	if step.Kind == faultHTTPStatus {
		recordStatus = step.Status
		if recordStatus == 0 {
			recordStatus = http.StatusInternalServerError
		}
	}
	if r.URL.Path != "/v1/chat/completions" {
		recordStatus = http.StatusNotFound
	}
	record := upstreamRequest{
		TraceID:   traceID,
		Channel:   u.name,
		ChannelID: u.channelID,
		Key:       key,
		Model:     request.Model,
		URL:       u.server.URL + r.URL.RequestURI(),
		Stream:    request.Stream,
		Action:    step.Kind,
		Status:    recordStatus,
		Started:   started,
	}
	u.mu.Lock()
	recordIndex := len(u.requests)
	u.requests = append(u.requests, record)
	u.mu.Unlock()
	defer func() {
		u.mu.Lock()
		u.requests[recordIndex].Ended = time.Now()
		u.mu.Unlock()
	}()

	if r.URL.Path != "/v1/chat/completions" {
		record.Status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}

	switch step.Kind {
	case faultHTTPStatus:
		u.writeHTTPStatus(w, request.Model, step)
	case faultDelayHeaders:
		if !waitResilienceDelay(r.Context(), step.DelaySeconds, u.durationUnit) {
			return
		}
		if request.Stream {
			u.writeSSE(w, r, request.Model, defaultSSEChunks(request.Model))
		} else {
			u.writeJSON(w, request.Model)
		}
	case faultDelayFirstEvent:
		if !request.Stream {
			if !waitResilienceDelay(r.Context(), step.DelaySeconds, u.durationUnit) {
				return
			}
			u.writeJSON(w, request.Model)
			break
		}
		if !u.flushSSEHeaders(w) {
			return
		}
		if !waitResilienceDelay(r.Context(), step.DelaySeconds, u.durationUnit) {
			return
		}
		u.writeSSEChunks(w, r, step.Chunks, request.Model)
	case faultIdleAfterEvent:
		if !request.Stream {
			u.writeJSON(w, request.Model)
			break
		}
		chunks := step.Chunks
		if len(chunks) == 0 {
			chunks = defaultSSEChunks(request.Model)
		}
		if !u.flushSSEHeaders(w) {
			return
		}
		u.writeSSEChunk(w, chunks[0])
		if !waitResilienceDelay(r.Context(), step.DelaySeconds, u.durationUnit) {
			return
		}
		u.writeSSEChunks(w, r, chunks[1:], request.Model)
	case faultNeverEnd:
		if !request.Stream {
			u.writeJSON(w, request.Model)
			break
		}
		chunks := step.Chunks
		if len(chunks) == 0 {
			chunks = defaultSSEChunks(request.Model)
		}
		if !u.flushSSEHeaders(w) {
			return
		}
		u.writeSSEChunk(w, chunks[0])
		<-r.Context().Done()
	case faultDisconnectBefore:
		u.writeTruncated(w, nil)
	case faultDisconnectAfter:
		if request.Stream {
			chunks := step.Chunks
			if len(chunks) == 0 {
				chunks = defaultSSEChunks(request.Model)
			}
			u.writeTruncated(w, []byte("data: "+chunks[0]+"\n\n"))
		} else {
			u.writeTruncated(w, []byte(`{"id":"truncated","object":"chat.completion"}`))
		}
	case faultSuccessShapedError:
		if request.Stream {
			if !u.flushSSEHeaders(w) {
				return
			}
			u.writeSSEChunk(w, `{"error":{"message":"model is temporarily unavailable"}}`)
		} else {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"error":{"message":"model is temporarily unavailable"}}`)
		}
	case faultContentFilter:
		if request.Stream {
			if !u.flushSSEHeaders(w) {
				return
			}
			chunks := step.Chunks
			if len(chunks) == 0 {
				chunks = []string{fmt.Sprintf(`{"id":"filter","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":"blocked"},"finish_reason":"content_filter"}]}`, request.Model), "[DONE]"}
			}
			u.writeSSEChunks(w, r, chunks, request.Model)
		} else {
			u.writeJSON(w, request.Model)
		}
	case faultSSESuccess:
		if request.Stream {
			u.writeSSEDelayed(w, r, request.Model, step.Chunks, step.DelaySeconds)
		} else {
			u.writeJSON(w, request.Model)
		}
	case faultJSONSuccess:
		if request.Stream {
			u.writeSSE(w, r, request.Model, step.Chunks)
		} else {
			u.writeJSON(w, request.Model)
		}
	default:
		if request.Stream {
			u.writeSSE(w, r, request.Model, nil)
		} else {
			u.writeJSON(w, request.Model)
		}
	}
}

func waitResilienceDelay(ctx context.Context, seconds int, unit time.Duration) bool {
	if seconds <= 0 {
		return true
	}
	if unit <= 0 {
		unit = time.Second
	}
	timer := time.NewTimer(time.Duration(seconds) * unit)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (u *resilienceUpstream) writeHTTPStatus(w http.ResponseWriter, modelName string, step faultStep) {
	status := step.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	if step.RetryAfter != "" {
		w.Header().Set("Retry-After", step.RetryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"message":"fault status %d","type":"resilience_fault"}}`, status)
}

func (u *resilienceUpstream) writeJSON(w http.ResponseWriter, modelName string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, fmt.Sprintf(`{"id":"resilience","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, modelName))
}

func (u *resilienceUpstream) flushSSEHeaders(w http.ResponseWriter) bool {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		return false
	}
	flusher.Flush()
	return true
}

func (u *resilienceUpstream) writeSSE(w http.ResponseWriter, r *http.Request, modelName string, chunks []string) {
	u.writeSSEDelayed(w, r, modelName, chunks, 0)
}

func (u *resilienceUpstream) writeSSEDelayed(w http.ResponseWriter, r *http.Request, modelName string, chunks []string, delaySeconds int) {
	if !u.flushSSEHeaders(w) {
		return
	}
	u.writeSSEChunksDelayed(w, r, chunks, modelName, delaySeconds)
}

func (u *resilienceUpstream) writeSSEChunks(w http.ResponseWriter, r *http.Request, chunks []string, modelName string) {
	u.writeSSEChunksDelayed(w, r, chunks, modelName, 0)
}

func (u *resilienceUpstream) writeSSEChunksDelayed(w http.ResponseWriter, r *http.Request, chunks []string, modelName string, delaySeconds int) {
	if len(chunks) == 0 {
		chunks = defaultSSEChunks(modelName)
	}
	for index, chunk := range chunks {
		if index > 0 {
			if !waitResilienceDelay(r.Context(), delaySeconds, u.durationUnit) {
				return
			}
		}
		u.writeSSEChunk(w, chunk)
	}
}

func (u *resilienceUpstream) writeSSEChunk(w http.ResponseWriter, chunk string) {
	if chunk == "[DONE]" {
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	} else {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", chunk)
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (u *resilienceUpstream) writeTruncated(w http.ResponseWriter, body []byte) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 100000\r\nConnection: close\r\n\r\n")
	if len(body) > 0 {
		_, _ = conn.Write(body)
	}
}

func defaultSSEChunks(modelName string) []string {
	return []string{
		fmt.Sprintf(`{"id":"resilience","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{"role":"assistant","content":"ok"},"finish_reason":null}]}`, modelName),
		fmt.Sprintf(`{"id":"resilience","object":"chat.completion.chunk","created":1,"model":%q,"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, modelName),
		"[DONE]",
	}
}

func (u *resilienceUpstream) snapshot(traceID string) []upstreamRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	requests := make([]upstreamRequest, 0, len(u.requests))
	for _, request := range u.requests {
		if traceID == "" || request.TraceID == traceID {
			requests = append(requests, request)
		}
	}
	return requests
}

func (h *resilienceHarness) traceCalls(traceID string) []upstreamRequest {
	calls := make([]upstreamRequest, 0)
	for _, upstream := range h.upstreams {
		calls = append(calls, upstream.snapshot(traceID)...)
	}
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].Started.Before(calls[j].Started) })
	return calls
}

func (h *resilienceHarness) faultSnapshot() map[string][]faultStep {
	result := make(map[string][]faultStep)
	for _, upstream := range h.upstreams {
		upstream.mu.Lock()
		for key, steps := range upstream.scripts {
			copied := make([]faultStep, len(steps))
			copy(copied, steps)
			result[upstream.name+":"+key] = copied
		}
		upstream.mu.Unlock()
	}
	return result
}

func (h *resilienceHarness) latestRelayLog() (dbmodel.RelayLog, bool) {
	logs, err := op.RelayLogList(context.Background(), nil, nil, 1, 1000)
	if err != nil {
		return dbmodel.RelayLog{}, false
	}
	var latest dbmodel.RelayLog
	found := false
	for _, logEntry := range logs {
		if logEntry.RequestAPIKeyName != h.apiKey.Name {
			continue
		}
		if !found || logEntry.ID > latest.ID {
			latest = logEntry
			found = true
		}
	}
	return latest, found
}

func (h *resilienceHarness) doChat(modelName string, stream bool) relayObservation {
	traceID := fmt.Sprintf("%s-%d", h.name, h.requestSeq.Add(1))
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}],"stream":%t}`, modelName, stream)
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set(resilienceRequestHeader, traceID)
	c.Set("api_key_id", h.apiKey.ID)
	c.Set("supported_models", h.apiKey.SupportedModels)
	c.Set("pii_filter_enabled", false)
	relayHandler(llm.APIFormatOpenAIChatCompletion, h.opts.DurationUnit)(c)
	observation := relayObservation{
		TraceID: traceID,
		Model:   modelName,
		Stream:  stream,
		Status:  recorder.Code,
		Headers: recorder.Header().Clone(),
		Body:    recorder.Body.String(),
		Calls:   h.traceCalls(traceID),
	}
	observation.RelayLog, observation.HasRelayLog = h.latestRelayLog()
	trace := struct {
		Scenario     string                   `json:"scenario"`
		DurationUnit string                   `json:"duration_unit"`
		FaultSteps   map[string][]faultStep   `json:"fault_steps"`
		Calls        []upstreamRequest        `json:"upstream_calls"`
		Status       int                      `json:"client_status"`
		Body         string                   `json:"client_body"`
		Attempts     []dbmodel.ChannelAttempt `json:"relay_attempts,omitempty"`
	}{
		Scenario:     h.name,
		DurationUnit: h.opts.DurationUnit.String(),
		FaultSteps:   h.faultSnapshot(),
		Calls:        observation.Calls,
		Status:       observation.Status,
		Body:         observation.Body,
	}
	if observation.HasRelayLog {
		trace.Attempts = observation.RelayLog.Attempts
	}
	encoded, _ := json.Marshal(trace)
	observation.Reproduction = string(encoded)
	return observation
}

func assertObservation(t *testing.T, observation relayObservation, condition bool, format string, args ...any) {
	t.Helper()
	if !condition {
		args = append(args, observation.Reproduction)
		t.Fatalf(format+" reproduction=%s", args...)
	}
}

func TestResilienceSuite(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("TopologyRoundRobin", func(t *testing.T) {
		h := newResilienceHarness(t, "topology", harnessOptions{GroupMode: dbmodel.GroupModeRoundRobin, KeyMode: 1})
		seen := make(map[string]int)
		for _, modelName := range h.models {
			for i := 0; i < 20; i++ {
				observation := h.doChat(modelName, false)
				assertObservation(t, observation, observation.Status == http.StatusOK, "baseline request status=%d want 200", observation.Status)
				assertObservation(t, observation, len(observation.Calls) == 1, "baseline calls=%d want 1", len(observation.Calls))
				if len(observation.Calls) == 1 {
					call := observation.Calls[0]
					seen[fmt.Sprintf("%s/%s/%s", call.Channel, call.Key, modelName)]++
				}
			}
		}
		assertObservation(t, relayObservation{Reproduction: fmt.Sprintf("topology combinations=%d", len(seen))}, len(seen) == 60, "topology combinations=%d want 60")
		for _, channel := range h.channels {
			count := 0
			for key := range seen {
				if strings.HasPrefix(key, channel.channel.Name+"/") {
					count++
				}
			}
			if count != 30 {
				t.Fatalf("channel %s combinations=%d want 30", channel.channel.Name, count)
			}
		}
	})

	for _, testCase := range []struct {
		name   string
		status int
	}{
		{name: "HTTP400", status: 400},
		{name: "HTTP401", status: 401},
		{name: "HTTP403", status: 403},
		{name: "HTTP404", status: 404},
		{name: "HTTP408", status: 408},
		{name: "HTTP429", status: 429},
		{name: "HTTP500", status: 500},
		{name: "HTTP502", status: 502},
		{name: "HTTP503", status: 503},
		{name: "HTTP504", status: 504},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var waitMax *int
			if testCase.status == 429 {
				v := 0
				waitMax = &v
			}
			h := newResilienceHarness(t, "status-"+testCase.name, harnessOptions{RateLimitRetryWaitMax: waitMax})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultHTTPStatus, Status: testCase.status})
			if testCase.status == 400 {
				observation := h.doChat(h.models[0], false)
				assertObservation(t, observation, observation.Status == 400, "400 status=%d", observation.Status)
				assertObservation(t, observation, len(observation.Calls) == 1, "400 calls=%d", len(observation.Calls))
				return
			}
			if testCase.status == http.StatusUnauthorized || testCase.status == http.StatusForbidden {
				h.setFault(1, 1, h.models[0], faultStep{Kind: faultJSONSuccess})
				observation := h.doChat(h.models[0], false)
				assertObservation(t, observation, observation.Status == http.StatusOK, "%d status=%d", testCase.status, observation.Status)
				assertObservation(t, observation, len(observation.Calls) == 2 && observation.Calls[0].Key != observation.Calls[1].Key, "%d calls=%v", testCase.status, observation.Calls)
				return
			}
			if testCase.status == 404 {
				observation := h.doChat(h.models[0], false)
				assertObservation(t, observation, observation.Status == 200, "404 failover status=%d", observation.Status)
				assertObservation(t, observation, len(observation.Calls) == 2 && observation.Calls[0].ChannelID == h.channels[0].channel.ID && observation.Calls[1].ChannelID == h.channels[1].channel.ID, "404 call order=%v", observation.Calls)
				return
			}
			if testCase.status == 429 {
				observation := h.doChat(h.models[0], false)
				assertObservation(t, observation, observation.Status == 200, "429 status=%d", observation.Status)
				assertObservation(t, observation, len(observation.Calls) == 2, "429 calls=%d", len(observation.Calls))
				return
			}
			h.setFault(1, 1, h.models[0], faultStep{Kind: faultJSONSuccess})
			observation := h.doChat(h.models[0], false)
			assertObservation(t, observation, observation.Status == 200, "%d status=%d", testCase.status, observation.Status)
			assertObservation(t, observation, len(observation.Calls) == 2, "%d calls=%d", testCase.status, len(observation.Calls))
		})
	}

	t.Run("StreamingHTTP401RetriesNextKey", func(t *testing.T) {
		h := newResilienceHarness(t, "streaming-http-401", harnessOptions{})
		h.setFault(1, 0, h.models[0], faultStep{Kind: faultHTTPStatus, Status: http.StatusUnauthorized})

		observation := h.doChat(h.models[0], true)
		assertObservation(t, observation, observation.Status == http.StatusOK, "streaming 401 status=%d", observation.Status)
		assertObservation(t, observation, len(observation.Calls) == 2 && observation.Calls[0].Key != observation.Calls[1].Key, "streaming 401 calls=%v", observation.Calls)
		assertObservation(t, observation, strings.Contains(observation.Body, "[DONE]"), "streaming 401 body=%q", observation.Body)
	})

	t.Run("AllKeys429FastFailure", func(t *testing.T) {
		zero := 0
		h := newResilienceHarness(t, "all-429", harnessOptions{RateLimitRetryWaitMax: &zero})
		for channelIndex := range 2 {
			for keyIndex := range 10 {
				h.setFault(channelIndex+1, keyIndex, h.models[0], faultStep{Kind: faultHTTPStatus, Status: http.StatusTooManyRequests})
			}
		}
		started := time.Now()
		observation := h.doChat(h.models[0], false)
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("all-429 disabled wait elapsed=%v", elapsed)
		}
		assertObservation(t, observation, observation.Status == http.StatusTooManyRequests, "all-429 status=%d", observation.Status)
		assertObservation(t, observation, len(observation.Calls) == 20, "all-429 upstream calls=%d", len(observation.Calls))
	})

	t.Run("SingleKeyCooldownInformsClient", func(t *testing.T) {
		zero := 0
		h := newResilienceHarness(t, "single-key-cooldown", harnessOptions{RateLimitRetryWaitMax: &zero})
		onlyKey := h.channels[0].keys[0]
		if err := db.GetDB().Model(&dbmodel.ChannelKey{}).Where("id <> ?", onlyKey.ID).Update("enabled", false).Error; err != nil {
			t.Fatalf("disable fallback keys: %v", err)
		}
		if err := op.InitCache(); err != nil {
			t.Fatalf("reload channel cache: %v", err)
		}
		h.setFault(1, 0, h.models[0], faultStep{Kind: faultHTTPStatus, Status: http.StatusTooManyRequests, RetryAfter: "4"})

		first := h.doChat(h.models[0], false)
		assertObservation(t, first, first.Status == http.StatusTooManyRequests, "first 429 status=%d", first.Status)
		assertObservation(t, first, first.Headers.Get("Retry-After") == "4", "first retry-after=%q", first.Headers.Get("Retry-After"))
		assertObservation(t, first, len(first.Calls) == 1, "first upstream calls=%d", len(first.Calls))

		blocked := h.doChat(h.models[0], false)
		assertObservation(t, blocked, blocked.Status == http.StatusTooManyRequests, "cooldown status=%d", blocked.Status)
		assertObservation(t, blocked, blocked.Headers.Get("Retry-After") != "", "cooldown retry-after=%q", blocked.Headers.Get("Retry-After"))
		assertObservation(t, blocked, len(blocked.Calls) == 0, "cooldown unexpectedly reached upstream: calls=%v", blocked.Calls)
	})

	t.Run("SingleKeyStreamCooldownUsesServiceRetry", func(t *testing.T) {
		h := newResilienceHarness(t, "single-key-stream-cooldown", harnessOptions{})
		onlyKey := h.channels[0].keys[0]
		if err := db.GetDB().Model(&dbmodel.ChannelKey{}).Where("id <> ?", onlyKey.ID).Update("enabled", false).Error; err != nil {
			t.Fatalf("disable fallback keys: %v", err)
		}
		if err := op.InitCache(); err != nil {
			t.Fatalf("reload channel cache: %v", err)
		}
		h.setFault(1, 0, h.models[0], faultStep{Kind: faultDisconnectAfter})

		first := h.doChat(h.models[0], true)
		assertObservation(t, first, len(first.Calls) == 1, "stream failure upstream calls=%d", len(first.Calls))

		blocked := h.doChat(h.models[0], false)
		assertObservation(t, blocked, blocked.Status == http.StatusServiceUnavailable, "stream cooldown status=%d", blocked.Status)
		assertObservation(t, blocked, blocked.Headers.Get("Retry-After") != "", "stream cooldown retry-after=%q", blocked.Headers.Get("Retry-After"))
		assertObservation(t, blocked, len(blocked.Calls) == 0, "stream cooldown unexpectedly reached upstream: calls=%v", blocked.Calls)
	})

	t.Run("CircuitOverrideTripsAfterTwoFailures", func(t *testing.T) {
		threshold := 2
		h := newResilienceHarness(t, "circuit-override", harnessOptions{CircuitBreakerThreshold: &threshold})
		h.setFault(1, 0, h.models[0],
			faultStep{Kind: faultHTTPStatus, Status: http.StatusBadGateway},
			faultStep{Kind: faultHTTPStatus, Status: http.StatusBadGateway},
		)
		for range 2 {
			observation := h.doChat(h.models[0], false)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "circuit warmup status=%d calls=%v", observation.Status, observation.Calls)
		}
		third := h.doChat(h.models[0], false)
		assertObservation(t, third, third.Status == 200, "circuit probe status=%d", third.Status)
		circuitBreak := false
		for _, attempt := range third.RelayLog.Attempts {
			circuitBreak = circuitBreak || attempt.Status == dbmodel.AttemptCircuitBreak
		}
		assertObservation(t, third, circuitBreak, "circuit attempt audit=%v", third.RelayLog.Attempts)
		status := balancer.GetCircuitBreakerStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0])
		if status.State != "open" {
			t.Fatalf("circuit status=%+v", status)
		}
	})

	t.Run("AuthFailureDisablesKey", func(t *testing.T) {
		h := newResilienceHarness(t, "auth-disable", harnessOptions{})
		h.setFault(1, 0, h.models[0], faultStep{Kind: faultHTTPStatus, Status: http.StatusUnauthorized})

		observation := h.doChat(h.models[0], false)
		assertObservation(t, observation, observation.Status == http.StatusOK, "auth failure status=%d", observation.Status)
		assertObservation(t, observation, len(observation.Calls) == 2 && observation.Calls[0].Key != observation.Calls[1].Key, "auth failure calls=%v", observation.Calls)
		channel, err := op.ChannelGet(h.channels[0].channel.ID, context.Background())
		if err != nil {
			t.Fatalf("get auth channel: %v", err)
		}
		if channel.Keys[0].Enabled {
			t.Fatalf("key-0 remained enabled after authentication failure: %+v", channel.Keys[0])
		}
	})

	t.Run("RateLimitCooldownIsolation", func(t *testing.T) {
		retryAfterDate := time.Now().Add(time.Second).UTC().Format(http.TimeFormat)
		for _, retryAfter := range []string{"1", retryAfterDate, ""} {
			t.Run(fmt.Sprintf("retry-after-%q", retryAfter), func(t *testing.T) {
				h := newResilienceHarness(t, "retry-after", harnessOptions{})
				h.setFault(1, 0, h.models[0], faultStep{Kind: faultHTTPStatus, Status: 429, RetryAfter: retryAfter})
				observation := h.doChat(h.models[0], false)
				assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "429 failover status=%d calls=%d", observation.Status, len(observation.Calls))
				status := dbmodel.GetKeyModelCooldownStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0])
				if !status.Active || status.Consecutive429s != 1 {
					t.Fatalf("429 cooldown status=%+v", status)
				}
				otherModel := h.doChat(h.models[1], false)
				assertObservation(t, otherModel, otherModel.Status == 200 && len(otherModel.Calls) == 1 && otherModel.Calls[0].Key == h.channels[0].keys[0].ChannelKey, "429 model isolation status=%d calls=%v", otherModel.Status, otherModel.Calls)
				second := h.doChat(h.models[0], false)
				assertObservation(t, second, second.Status == 200 && len(second.Calls) == 1 && second.Calls[0].Key != h.channels[0].keys[0].ChannelKey, "429 skip cooldown calls=%v", second.Calls)
			})
		}
	})

	t.Run("ModelAndChannelIsolation", func(t *testing.T) {
		h := newResilienceHarness(t, "model-isolation", harnessOptions{})
		h.setFault(1, 0, h.models[1], faultStep{Kind: faultHTTPStatus, Status: 404})
		b := h.doChat(h.models[1], false)
		assertObservation(t, b, b.Status == 200 && len(b.Calls) == 2 && b.Calls[1].ChannelID == h.channels[1].channel.ID, "model B failover calls=%v", b.Calls)
		a := h.doChat(h.models[0], false)
		c := h.doChat(h.models[2], false)
		assertObservation(t, a, a.Status == 200 && len(a.Calls) == 1 && a.Calls[0].ChannelID == h.channels[0].channel.ID, "model A isolation calls=%v", a.Calls)
		assertObservation(t, c, c.Status == 200 && len(c.Calls) == 1 && c.Calls[0].ChannelID == h.channels[0].channel.ID, "model C isolation calls=%v", c.Calls)
	})

	t.Run("PreEventTimeoutSwitchesChannel", func(t *testing.T) {
		h := newResilienceHarness(t, "pre-event-timeout", harnessOptions{FirstTokenTimeOut: 30, UpstreamTimeOut: 30})
		h.setFault(1, 0, h.models[0], faultStep{Kind: faultDelayFirstEvent, DelaySeconds: 60})
		observation := h.doChat(h.models[0], true)
		assertObservation(t, observation, observation.Status == 200, "pre-event timeout status=%d", observation.Status)
		assertObservation(t, observation, len(observation.Calls) == 2 && observation.Calls[0].ChannelID == h.channels[0].channel.ID && observation.Calls[1].ChannelID == h.channels[1].channel.ID, "pre-event timeout call order=%v", observation.Calls)
		hasTimeoutAttempt := false
		for _, attempt := range observation.RelayLog.Attempts {
			if strings.Contains(attempt.Msg, "first token timeout") {
				hasTimeoutAttempt = true
			}
		}
		assertObservation(t, observation, hasTimeoutAttempt, "pre-event timeout attempts=%v", observation.RelayLog.Attempts)
	})

	t.Run("SSEWireResilience", func(t *testing.T) {
		for _, testCase := range []struct {
			name         string
			firstTimeout int
			delaySeconds int
			wantSwitch   bool
		}{
			{name: "first-event-5-within-10", firstTimeout: 10, delaySeconds: 5, wantSwitch: false},
			{name: "first-event-60-over-30", firstTimeout: 30, delaySeconds: 60, wantSwitch: true},
			{name: "first-event-90-over-60", firstTimeout: 60, delaySeconds: 90, wantSwitch: true},
		} {
			t.Run(testCase.name, func(t *testing.T) {
				h := newResilienceHarness(t, "sse-"+testCase.name, harnessOptions{FirstTokenTimeOut: testCase.firstTimeout})
				h.setFault(1, 0, h.models[0], faultStep{Kind: faultDelayFirstEvent, DelaySeconds: testCase.delaySeconds})
				observation := h.doChat(h.models[0], true)
				assertObservation(t, observation, observation.Status == 200, "first-event status=%d", observation.Status)
				assertObservation(t, observation, strings.Contains(observation.Body, "[DONE]"), "first-event body=%q", observation.Body)
				wantCalls := 1
				if testCase.wantSwitch {
					wantCalls = 2
				}
				assertObservation(t, observation, len(observation.Calls) == wantCalls, "first-event calls=%d want=%d", len(observation.Calls), wantCalls)
			})
		}

		t.Run("response-headers-timeout", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-response-headers", harnessOptions{FirstTokenTimeOut: 30})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultDelayHeaders, DelaySeconds: 60})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "header timeout status=%d calls=%v", observation.Status, observation.Calls)
		})

		t.Run("normal-slow-stream", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-normal-slow", harnessOptions{FirstTokenTimeOut: 5, StreamIdleTimeOut: 5, StreamHardTimeOut: 10})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultSSESuccess, DelaySeconds: 1, Chunks: defaultSSEChunks(h.models[0])})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 1, "normal slow status=%d calls=%v", observation.Status, observation.Calls)
			assertObservation(t, observation, strings.Contains(observation.Body, "[DONE]"), "normal slow body=%q", observation.Body)
		})

		t.Run("idle-timeout-after-event", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-idle", harnessOptions{FirstTokenTimeOut: 5, StreamIdleTimeOut: 2, StreamHardTimeOut: 20})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultIdleAfterEvent, DelaySeconds: 10, Chunks: defaultSSEChunks(h.models[0])})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 1, "idle status=%d calls=%v", observation.Status, observation.Calls)
			assertObservation(t, observation, strings.Contains(observation.Body, "content"), "idle prefix body=%q", observation.Body)
			status := dbmodel.GetKeyModelCooldownStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0])
			if !status.Active {
				t.Fatalf("idle timeout did not cool key: %+v", status)
			}
		})

		t.Run("hard-timeout-after-write", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-hard", harnessOptions{FirstTokenTimeOut: 5, StreamIdleTimeOut: 50, StreamHardTimeOut: 3})
			chunks := make([]string, 8)
			for index := range chunks {
				chunks[index] = fmt.Sprintf(`{"id":"hard-%d","object":"chat.completion.chunk","model":%q,"choices":[{"index":0,"delta":{"content":"part"},"finish_reason":null}]}`, index, h.models[0])
			}
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultSSESuccess, DelaySeconds: 1, Chunks: chunks})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 1, "hard status=%d calls=%v", observation.Status, observation.Calls)
			assertObservation(t, observation, strings.Contains(observation.Body, "part"), "hard prefix body=%q", observation.Body)
			status := dbmodel.GetKeyModelCooldownStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0])
			if !status.Active {
				t.Fatalf("hard timeout did not cool key: %+v", status)
			}
		})

		t.Run("disconnect-before-event-switches", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-disconnect-before", harnessOptions{FirstTokenTimeOut: 10})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultDisconnectBefore})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "disconnect-before status=%d calls=%v", observation.Status, observation.Calls)
		})

		t.Run("disconnect-after-event-stops", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-disconnect-after", harnessOptions{FirstTokenTimeOut: 10})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultDisconnectAfter, Chunks: defaultSSEChunks(h.models[0])})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 1, "disconnect-after status=%d calls=%v", observation.Status, observation.Calls)
			assertObservation(t, observation, strings.Contains(observation.Body, "data:"), "disconnect-after body=%q", observation.Body)
			passiveFailure := false
			for _, attempt := range observation.RelayLog.Attempts {
				if attempt.Status == dbmodel.AttemptCanceled {
					t.Fatalf("passive upstream disconnect was misclassified as client cancellation: %+v", observation.RelayLog.Attempts)
				}
				if attempt.Status == dbmodel.AttemptFailed && strings.Contains(attempt.Msg, "failed to read stream event") {
					passiveFailure = true
				}
			}
			assertObservation(t, observation, passiveFailure, "passive disconnect attempts=%v", observation.RelayLog.Attempts)
		})

		t.Run("empty-stream-switches", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-empty", harnessOptions{FirstTokenTimeOut: 10})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultSSESuccess, Chunks: []string{"[DONE]"}})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "empty stream status=%d calls=%v", observation.Status, observation.Calls)
		})

		t.Run("success-shaped-stream-error", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-shaped-error", harnessOptions{})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultSuccessShapedError})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "shaped stream status=%d calls=%v", observation.Status, observation.Calls)
			failed := false
			for _, attempt := range observation.RelayLog.Attempts {
				failed = failed || attempt.Status == dbmodel.AttemptFailed
			}
			assertObservation(t, observation, failed, "shaped stream attempts=%v", observation.RelayLog.Attempts)
		})

		t.Run("content-filter-is-failure", func(t *testing.T) {
			h := newResilienceHarness(t, "sse-content-filter", harnessOptions{})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultContentFilter})
			observation := h.doChat(h.models[0], true)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 1, "content filter status=%d calls=%v", observation.Status, observation.Calls)
			failed := false
			for _, attempt := range observation.RelayLog.Attempts {
				failed = failed || attempt.Status == dbmodel.AttemptFailed
			}
			assertObservation(t, observation, failed, "content filter attempts=%v", observation.RelayLog.Attempts)
		})

		t.Run("success-shaped-json-error", func(t *testing.T) {
			h := newResilienceHarness(t, "json-shaped-error", harnessOptions{})
			h.setFault(1, 0, h.models[0], faultStep{Kind: faultSuccessShapedError})
			observation := h.doChat(h.models[0], false)
			assertObservation(t, observation, observation.Status == 200 && len(observation.Calls) == 2, "shaped json status=%d calls=%v", observation.Status, observation.Calls)
		})
	})

	t.Run("UnstableConcurrent", func(t *testing.T) {
		h := newResilienceHarness(t, "unstable-concurrent", harnessOptions{})
		for keyIndex := range 10 {
			steps := make([]faultStep, 67)
			for index := range steps {
				switch index % 3 {
				case 0:
					steps[index] = faultStep{Kind: faultHTTPStatus, Status: 429}
				case 1:
					steps[index] = faultStep{Kind: faultHTTPStatus, Status: 502}
				default:
					steps[index] = faultStep{Kind: faultDisconnectBefore}
				}
			}
			h.setFault(1, keyIndex, h.models[0], steps...)
		}
		observations := make(chan relayObservation, 64)
		var wg sync.WaitGroup
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				observations <- h.doChat(h.models[0], false)
			}()
		}
		wg.Wait()
		close(observations)
		for observation := range observations {
			assertObservation(t, observation, observation.Status == 200, "unstable status=%d calls=%v", observation.Status, observation.Calls)
			assertObservation(t, observation, len(observation.Calls) <= 11, "unstable attempts=%d calls=%v", len(observation.Calls), observation.Calls)
			seenFailed := make(map[string]bool)
			for _, call := range observation.Calls {
				failed := call.Action == faultDisconnectBefore || call.Action == faultDisconnectAfter ||
					(call.Action == faultHTTPStatus && call.Status >= 400)
				if !failed {
					continue
				}
				pair := fmt.Sprintf("%d:%s", call.ChannelID, call.Key)
				assertObservation(t, observation, !seenFailed[pair], "unstable repeated failed pair=%s calls=%v", pair, observation.Calls)
				seenFailed[pair] = true
			}
		}
		start := time.Now().Add(-time.Minute).Unix()
		end := time.Now().Add(time.Minute).Unix()
		logs, err := op.RelayLogListAll(context.Background(), start, end)
		if err != nil {
			t.Fatalf("list unstable relay logs: %v", err)
		}
		count := 0
		for _, logEntry := range logs {
			if logEntry.RequestAPIKeyName != h.apiKey.Name {
				continue
			}
			count++
			if logEntry.TotalAttempts > 66 {
				t.Fatalf("unstable relay total attempts=%d log=%+v", logEntry.TotalAttempts, logEntry)
			}
		}
		if count != 64 {
			t.Fatalf("unstable relay logs=%d want 64", count)
		}
		callsByChannel := make(map[int][2]int)
		for _, call := range h.traceCalls("") {
			counts := callsByChannel[call.ChannelID]
			failed := call.Action == faultDisconnectBefore || call.Action == faultDisconnectAfter ||
				(call.Action == faultHTTPStatus && call.Status >= 400)
			if failed {
				counts[1]++
			} else {
				counts[0]++
			}
			callsByChannel[call.ChannelID] = counts
		}
		for _, channel := range h.channels {
			stats := op.StatsChannelGet(channel.channel.ID)
			counts := callsByChannel[channel.channel.ID]
			if stats.RequestSuccess != int64(counts[0]) || stats.RequestFailed != int64(counts[1]) {
				t.Fatalf("unstable channel %d stats=%+v simulated success=%d failure=%d", channel.channel.ID, stats, counts[0], counts[1])
			}
		}
	})

	t.Run("LimiterKeyAndModel", func(t *testing.T) {
		t.Run("key-limit-skips-only-key", func(t *testing.T) {
			h := newResilienceHarness(t, "limiter-key", harnessOptions{RateLimit: "1/1s"})
			first := h.doChat(h.models[0], false)
			second := h.doChat(h.models[0], false)
			assertObservation(t, first, first.Status == 200 && len(first.Calls) == 1, "key limiter first status=%d calls=%v", first.Status, first.Calls)
			assertObservation(t, second, second.Status == 200 && len(second.Calls) == 1 && second.Calls[0].ChannelID == h.channels[0].channel.ID, "key limiter second status=%d calls=%v", second.Status, second.Calls)
			if second.Calls[0].Key == h.channels[0].keys[0].ChannelKey {
				t.Fatalf("key limiter reused key-0 upstream: %+v", second.Calls)
			}
			if status := balancer.GetCircuitBreakerStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0]); status.State != "closed" {
				t.Fatalf("local key limiter opened breaker: %+v", status)
			}
			keyStatus := plugins.GetRateLimiterStatus(fmt.Sprintf("ch:%d:k:%d", h.channels[0].channel.ID, h.channels[0].keys[0].ID), 1, time.Second)
			if keyStatus.Used == 0 {
				t.Fatalf("key limiter status did not record first request: %+v", keyStatus)
			}
		})

		t.Run("model-limit-crosses-channel", func(t *testing.T) {
			h := newResilienceHarness(t, "limiter-model", harnessOptions{ModelRateLimit: "model-A=1/1s"})
			first := h.doChat(h.models[0], false)
			second := h.doChat(h.models[0], false)
			assertObservation(t, first, first.Status == 200 && len(first.Calls) == 1 && first.Calls[0].ChannelID == h.channels[0].channel.ID, "model limiter first status=%d calls=%v", first.Status, first.Calls)
			assertObservation(t, second, second.Status == 200 && len(second.Calls) == 1 && second.Calls[0].ChannelID == h.channels[1].channel.ID, "model limiter second status=%d calls=%v", second.Status, second.Calls)
			modelStatus := plugins.GetRateLimiterStatus(fmt.Sprintf("ch:%d:m:%s", h.channels[0].channel.ID, h.models[0]), 1, time.Second)
			if modelStatus.Used == 0 {
				t.Fatalf("model limiter status did not record first request: %+v", modelStatus)
			}
			if status := balancer.GetCircuitBreakerStatus(h.channels[0].channel.ID, h.channels[0].keys[0].ID, h.models[0]); status.State != "closed" {
				t.Fatalf("local model limiter opened breaker: %+v", status)
			}
		})
	})

	t.Run("LimiterRoundWait", func(t *testing.T) {
		zero := 0
		h := newResilienceHarness(t, "limiter-round", harnessOptions{GroupMode: dbmodel.GroupModeRoundRobin, KeyMode: 1, RateLimit: "1/1s", RateLimitRetryWaitMax: &zero})
		for i := 0; i < 20; i++ {
			observation := h.doChat(h.models[0], false)
			assertObservation(t, observation, observation.Status == 200, "fill request %d status=%d", i, observation.Status)
		}
		before := len(h.traceCalls(""))
		blocked := h.doChat(h.models[0], false)
		assertObservation(t, blocked, blocked.Status == http.StatusTooManyRequests, "disabled wait status=%d", blocked.Status)
		if len(h.traceCalls("")) != before {
			t.Fatalf("disabled wait unexpectedly reached upstream: before=%d after=%d", before, len(h.traceCalls("")))
		}
		waitMax := 120
		h2 := newResilienceHarness(t, "limiter-round-wait", harnessOptions{GroupMode: dbmodel.GroupModeRoundRobin, KeyMode: 1, RateLimit: "1/1s", RateLimitRetryWaitMax: &waitMax})
		for i := 0; i < 20; i++ {
			observation := h2.doChat(h2.models[0], false)
			assertObservation(t, observation, observation.Status == 200, "wait fill request %d status=%d", i, observation.Status)
		}
		started := time.Now()
		released := h2.doChat(h2.models[0], false)
		if elapsed := time.Since(started); elapsed < 900*time.Millisecond || elapsed > 3*time.Second {
			t.Fatalf("round wait elapsed=%v", elapsed)
		}
		assertObservation(t, released, released.Status == 200, "round wait status=%d", released.Status)
	})
}
