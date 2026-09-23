package relay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/llm"
	"github.com/vrichv/octopus-pro/internal/db"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
)

// The same captured OpenCode request first goes directly to Zen without a key,
// then through Octopus. Opt in with ZEN_LIVE=1; no network in the default suite.
func TestZZZenLiveRelay(t *testing.T) {
	if os.Getenv("ZEN_LIVE") == "" {
		t.Skip("set ZEN_LIVE=1")
	}
	models := strings.Split(os.Getenv("ZEN_MODELS"), ",")
	if len(models) == 1 && models[0] == "" {
		models = []string{"mimo-v2.6-flash-free", "big-pickle", "muse-spark-1.3-contributor-free"}
	}
	fixture, err := os.ReadFile("../../opencode-prefixed.json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Body json.RawMessage `json:"body"`
	}
	if err := json.Unmarshal(fixture, &envelope); err != nil {
		t.Fatal(err)
	}

	if db.GetDB() != nil {
		_ = db.Close()
	}
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "zen.db"), false); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	settings := dbmodel.DefaultSettings()
	if err := db.GetDB().WithContext(ctx).Create(&settings).Error; err != nil {
		t.Fatal(err)
	}
	key := ""
	channel := dbmodel.Channel{
		ID: 900001, Name: "zen-live", Type: dbmodel.ChannelTypeOpenCodeZen, Enabled: true,
		BaseUrls:    []dbmodel.BaseUrl{{URL: "https://opencode.ai/zen/v1"}},
		Model:       models[0],
		CustomModel: strings.Join(models[1:], ","),
	}
	if err := db.GetDB().WithContext(ctx).Create(&channel).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.GetDB().WithContext(ctx).Create(&dbmodel.ChannelKey{ID: 900101, ChannelID: channel.ID, Enabled: true, ChannelKey: key}).Error; err != nil {
		t.Fatal(err)
	}
	for i, model := range models {
		group := dbmodel.Group{ID: 900200 + i, Name: model, Mode: dbmodel.GroupModeFailover}
		if err := db.GetDB().WithContext(ctx).Create(&group).Error; err != nil {
			t.Fatal(err)
		}
		item := dbmodel.GroupItem{ID: 900300 + i, GroupID: group.ID, ChannelID: channel.ID, ModelName: model, Priority: 1, Weight: 1}
		if err := db.GetDB().WithContext(ctx).Create(&item).Error; err != nil {
			t.Fatal(err)
		}
	}
	apiKey := dbmodel.APIKey{ID: 900400, Name: "zen-live", APIKey: "client-zen-live", Enabled: true}
	if err := db.GetDB().WithContext(ctx).Create(&apiKey).Error; err != nil {
		t.Fatal(err)
	}
	if err := op.InitCache(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			var body map[string]json.RawMessage
			if err := json.Unmarshal(envelope.Body, &body); err != nil {
				t.Fatal(err)
			}
			body["model"], _ = json.Marshal(model)
			clientBody, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}

			if model == "mimo-v2.6-flash-free" {
				ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
				defer cancel()
				direct, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://opencode.ai/zen/v1/chat/completions", strings.NewReader(string(clientBody)))
				if err != nil {
					t.Fatal(err)
				}
				direct.Header.Set("Content-Type", "application/json")
				direct.Header.Set("User-Agent", "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
				direct.Header.Set("x-opencode-session", "ses_f5081f13bffeEQAd7eIVRlYJoz")
				direct.Header.Set("x-opencode-request", "msg_0af7e0f00001ZIk7sqoqb8ZM6Z")
				direct.Header.Set("x-opencode-client", "cli")
				direct.Header.Set("x-opencode-project", "global")
				upstream, err := http.DefaultClient.Do(direct)
				if err != nil {
					t.Fatal(err)
				}
				content, readErr := io.ReadAll(io.LimitReader(upstream.Body, 8<<20))
				upstream.Body.Close()
				if readErr != nil {
					t.Fatal(readErr)
				}
				t.Logf("direct Mimo HTTP %d; bytes=%d; done=%t", upstream.StatusCode, len(content), strings.Contains(string(content), "data: [DONE]"))
				if upstream.StatusCode != http.StatusOK || !strings.Contains(string(content), "data: [DONE]") {
					t.Fatalf("direct Mimo failed: HTTP %d: %s", upstream.StatusCode, content)
				}
			}

			gin.SetMode(gin.TestMode)
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(clientBody)))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("User-Agent", "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14")
			c.Request.Header.Set("x-opencode-session", "ses_clientaaaaaaBBBBBBBBBBBBBB")
			c.Request.Header.Set("x-opencode-request", "msg_client000000000000000000")
			c.Request.Header.Set("x-opencode-client", "cli")
			c.Set("api_key_id", apiKey.ID)
			c.Set("pii_filter_enabled", false)
			relayHandler(llm.APIFormatOpenAIChatCompletion, 0)(c)
			out := recorder.Body.String()
			done := strings.Contains(out, "data: [DONE]")
			t.Logf("Octopus %s HTTP %d; bytes=%d; done=%t", model, recorder.Code, len(out), done)
			if recorder.Code != http.StatusOK || !done {
				preview := out
				if len(preview) > 2000 {
					preview = preview[:2000]
				}
				t.Fatalf("Octopus stream incomplete: HTTP %d, done=%t: %s", recorder.Code, done, preview)
			}
		})
	}
}
