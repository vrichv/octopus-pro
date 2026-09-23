package relay

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/vrichv/octopus-pro/internal/db"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/relay/balancer"
)

func TestPrepareAttemptUsesCandidateModelForOpenCodeProtocol(t *testing.T) {
	if db.GetDB() != nil {
		_ = db.Close()
	}
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "relay.db"), false); err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	settings := dbmodel.DefaultSettings()
	if err := db.GetDB().Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(nil)
	defer upstream.Close()
	channel := dbmodel.Channel{
		ID: 910001, Name: "zen-alias", Type: dbmodel.ChannelTypeOpenCodeZen, Enabled: true,
		BaseUrls: []dbmodel.BaseUrl{{URL: upstream.URL}},
		Keys:     []dbmodel.ChannelKey{{ID: 910002, ChannelID: 910001, Enabled: true, ChannelKey: "test-key"}},
	}
	group := dbmodel.Group{ID: 910003, Name: "muse-1.3", Mode: dbmodel.GroupModeFailover}
	item := dbmodel.GroupItem{ID: 910004, GroupID: group.ID, ChannelID: channel.ID, ModelName: "mimo-v2.6", Priority: 1, Weight: 1}
	if err := db.GetDB().Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Create(&group).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	openCodeModelProviders.mu.Lock()
	openCodeModelProviders.fetchedAt = time.Now()
	openCodeModelProviders.providers = map[string]string{"mimo-v2.6-flash-free": ""}
	openCodeModelProviders.models = []string{"mimo-v2.6-flash-free"}
	openCodeModelProviders.mu.Unlock()
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}
	resolved, err := op.GroupGetEnabledMap("muse-1.3", context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Items) != 1 {
		t.Fatalf("resolved group items = %+v", resolved.Items)
	}
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"muse-1.3"}`))
	request := &llm.Request{
		Model:       "muse-1.3",
		RequestType: llm.RequestTypeChat,
		Messages:    []llm.Message{{Role: "user", Content: llm.MessageContent{Content: new("hello")}}},
	}
	iter := balancer.NewIterator(resolved, 0, "muse-1.3")
	if !iter.Next() {
		t.Fatal("alias group iterator has no candidate")
	}
	run := &relayRun{
		c:               ctx,
		internalRequest: request,
		metrics:         &RelayMetrics{RequestModel: "muse-1.3"},
		group:           resolved,
		iter:            iter,
	}
	attempt, err := run.prepareAttempt()
	if err != nil {
		t.Fatal(err)
	}
	if attempt == nil {
		t.Fatal("prepareAttempt returned no candidate")
	}
	raw, err := attempt.outAdapter.TransformRequest(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(raw.URL, "/v1/chat/completions") {
		t.Fatalf("candidate protocol used group alias: URL=%q", raw.URL)
	}
	if request.Model != "muse-1.3" {
		t.Fatalf("client request model mutated: %q", request.Model)
	}
	if attempt.request == nil || attempt.request.Model != "mimo-v2.6-flash-free" {
		t.Fatalf("candidate request model = %v, want canonical candidate model", attempt.request)
	}
}
