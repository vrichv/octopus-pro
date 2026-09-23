package relay

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/bailian"
	"github.com/looplj/axonhub/llm/transformer/deepseek"
	"github.com/looplj/axonhub/llm/transformer/doubao"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/looplj/axonhub/llm/transformer/modelscope"
	"github.com/looplj/axonhub/llm/transformer/moonshot"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/opencode"
	"github.com/looplj/axonhub/llm/transformer/openrouter"
	"github.com/looplj/axonhub/llm/transformer/xai"
	"github.com/looplj/axonhub/llm/transformer/zai"
	dbmodel "github.com/vrichv/octopus-pro/internal/model"
)

func newInbound(format llm.APIFormat) transformer.Inbound {
	switch format {
	case llm.APIFormatOpenAIChatCompletion:
		return openai.NewInboundTransformer()
	case llm.APIFormatOpenAICompletion:
		return openai.NewCompletionInboundTransformer()
	case llm.APIFormatOpenAIResponse:
		return responses.NewInboundTransformer()
	case llm.APIFormatOpenAIResponseCompact:
		return responses.NewCompactInboundTransformer()
	case llm.APIFormatOpenAIEmbedding:
		return openai.NewEmbeddingInboundTransformer()
	case llm.APIFormatOpenAIImageGeneration:
		return openai.NewImageGenerationInboundTransformer()
	case llm.APIFormatOpenAIImageEdit:
		return openai.NewImageEditInboundTransformer()
	case llm.APIFormatOpenAIImageVariation:
		return openai.NewImageVariationInboundTransformer()
	case llm.APIFormatAnthropicMessage:
		return anthropic.NewInboundTransformer()
	default:
		return nil
	}
}

func newOutbound(channelType llm.APIFormat, request *llm.Request, baseURL, key string) (transformer.Outbound, error) {
	requestType := llm.RequestTypeChat
	if request != nil && request.RequestType != "" {
		requestType = request.RequestType
	}

	// 将请求类型兼容性收敛到出站适配器选择处，避免 Handler 先创建适配器再用本地规则二次拦截，
	// 这样 Doubao/Gemini 在 axonhub 已经支持的 embedding/image 能力不会被项目内旧判断挡住。
	switch requestType {
	case llm.RequestTypeEmbedding:
		switch channelType {
		case llm.APIFormatOpenAIChatCompletion,
			llm.APIFormatOpenAIResponse,
			llm.APIFormatOpenAIEmbedding,
			dbmodel.ChannelTypeDeepSeek,
			dbmodel.ChannelTypeOpenRouter,
			dbmodel.ChannelTypeBailian:
			return openai.NewOutboundTransformer(baseURL, key)
		case llm.APIFormatGeminiContents:
			return gemini.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeDoubao:
			return doubao.NewOutboundTransformer(baseURL, key)
		default:
			return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
		}
	case llm.RequestTypeImage:
		switch channelType {
		case llm.APIFormatOpenAIChatCompletion,
			llm.APIFormatOpenAIResponse,
			llm.APIFormatOpenAIImageGeneration,
			llm.APIFormatOpenAIImageEdit,
			llm.APIFormatOpenAIImageVariation,
			dbmodel.ChannelTypeDeepSeek,
			dbmodel.ChannelTypeBailian:
			return openai.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeOpenRouter:
			return openrouter.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeZAI:
			return zai.NewOutboundTransformer(baseURL, key)
		case llm.APIFormatGeminiContents:
			return gemini.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeDoubao:
			return doubao.NewOutboundTransformer(baseURL, key)
		default:
			return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
		}
	case llm.RequestTypeCompletion:
		switch channelType {
		case llm.APIFormatOpenAIChatCompletion:
			return openai.NewCompletionOutboundTransformer(&openai.Config{
				BaseURL:        baseURL,
				APIKeyProvider: auth.NewStaticKeyProvider(key),
			})
		case dbmodel.ChannelTypeDeepSeek:
			return deepseek.NewOutboundTransformer(baseURL, key)
		default:
			return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
		}
	case llm.RequestTypeCompact:
		switch channelType {
		case llm.APIFormatOpenAIResponse:
			return responses.NewOutboundTransformer(baseURL, key)
		default:
			return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
		}
	case llm.RequestTypeChat:
		switch channelType {
		case llm.APIFormatOpenAIChatCompletion:
			return openai.NewOutboundTransformer(baseURL, key)
		case llm.APIFormatOpenAIResponse:
			return responses.NewOutboundTransformer(baseURL, key)
		case llm.APIFormatAnthropicMessage:
			return anthropic.NewOutboundTransformer(baseURL, key)
		case llm.APIFormatGeminiContents:
			return gemini.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeDoubao:
			return doubao.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeDeepSeek:
			return deepseek.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeOpenRouter:
			return openrouter.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeBailian:
			return bailian.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeXAI:
			return xai.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeModelScope:
			return modelscope.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeMoonshot:
			return moonshot.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeZAI:
			return zai.NewOutboundTransformer(baseURL, key)
		case dbmodel.ChannelTypeOpenCodeZen:
			noAuth := key == ""
			outbound, err := newOpenCodeZenOutbound(request, baseURL, key)
			if err != nil {
				return nil, err
			}
			return &openCodeZenFreeOutbound{Outbound: outbound, noAuth: noAuth}, nil
		case dbmodel.ChannelTypeOpenCodeGo:
			if isOpenCodeGoResponsesModel(request.Model) {
				return responses.NewOutboundTransformer(baseURL, key)
			}
			return opencode.NewOutboundTransformer(baseURL, key)
		default:
			return nil, fmt.Errorf("channel type %s is not compatible with %s request", channelType, requestType)
		}
	default:
		return nil, fmt.Errorf("%s request is not supported by relay", requestType)
	}
}

// newOpenCodeZenOutbound follows the model API metadata when available. The
// heuristic remains a fallback for newly published or temporarily unknown models.
func newOpenCodeZenOutbound(request *llm.Request, baseURL, key string) (transformer.Outbound, error) {
	if key == "" {
		// OpenCode Free accepts requests without Authorization. The outbound
		// transformer still requires a non-empty provider token at construction;
		// the Zen Free wrapper removes the placeholder auth before sending.
		key = "public"
	}
	model := strings.ToLower(request.Model)
	provider, _ := openCodeModelProviders.lookup(model)
	switch openCodeModelProtocol(model, provider) {
	case openCodeProtocolResponses:
		return responses.NewOutboundTransformer(baseURL, key)
	case openCodeProtocolAnthropic:
		return anthropic.NewOutboundTransformer(baseURL, key)
	case openCodeProtocolGemini:
		return gemini.NewOutboundTransformerWithConfig(gemini.Config{
			BaseURL:        baseURL,
			APIVersion:     "v1",
			APIKeyProvider: auth.NewStaticKeyProvider(key),
		})
	default:
		return openai.NewOutboundTransformer(baseURL, key)
	}
}

type openCodeProtocol string

const (
	openCodeProtocolChat      openCodeProtocol = "chat"
	openCodeProtocolResponses openCodeProtocol = "responses"
	openCodeProtocolAnthropic openCodeProtocol = "anthropic"
	openCodeProtocolGemini    openCodeProtocol = "gemini"
	// OpenCode itself reads https://models.opencode.ai/api.json (core/src/models-dev.ts).
	openCodeModelCatalogURL = "https://models.opencode.ai/api.json"
	openCodeModelCatalogTTL = time.Hour
)

type openCodeModelCatalog struct {
	mu        sync.Mutex
	fetchedAt time.Time
	providers map[string]string
	models    []string
}

var openCodeModelProviders openCodeModelCatalog

func (c *openCodeModelCatalog) lookup(model string) (string, bool) {
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "opencode/")
	now := time.Now()
	c.mu.Lock()
	if now.Sub(c.fetchedAt) < openCodeModelCatalogTTL {
		canonical := c.canonicalLocked(model)
		provider, ok := c.providers[canonical]
		c.mu.Unlock()
		return provider, ok
	}
	c.mu.Unlock()

	providers, models := fetchOpenCodeModelProviders()
	c.mu.Lock()
	c.fetchedAt = now
	if providers != nil {
		c.providers = providers
		c.models = models
	}
	canonical := c.canonicalLocked(model)
	provider, ok := c.providers[canonical]
	c.mu.Unlock()
	return provider, ok
}

func (c *openCodeModelCatalog) canonical(model string) string {
	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "opencode/")
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if now.Sub(c.fetchedAt) >= openCodeModelCatalogTTL {
		return model
	}
	return c.canonicalLocked(model)
}

func (c *openCodeModelCatalog) canonicalLocked(model string) string {
	if _, ok := c.providers[model]; ok {
		return model
	}
	var match string
	for _, candidate := range c.models {
		if !strings.HasPrefix(candidate, model+"-") {
			continue
		}
		if match != "" {
			return model
		}
		match = candidate
	}
	if match != "" {
		return match
	}
	return model
}

func fetchOpenCodeModelProviders() (map[string]string, []string) {
	// models-dev.ts: USER_AGENT = `opencode/${channel}/${version}/${client}`, timeout 10s.
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequest(http.MethodGet, openCodeModelCatalogURL, nil)
	if err != nil {
		return nil, nil
	}
	request.Header.Set("User-Agent", "opencode/stable/1.18.32/cli")
	response, err := client.Do(request)
	if err != nil {
		return nil, nil
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, nil
	}
	var document struct {
		OpenCode struct {
			Models map[string]struct {
				Provider struct {
					NPM string `json:"npm"`
				} `json:"provider"`
			} `json:"models"`
		} `json:"opencode"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<20)).Decode(&document); err != nil {
		return nil, nil
	}
	providers := make(map[string]string, len(document.OpenCode.Models))
	models := make([]string, 0, len(document.OpenCode.Models))
	for id, model := range document.OpenCode.Models {
		id = strings.ToLower(id)
		providers[id] = model.Provider.NPM
		models = append(models, id)
	}
	return providers, models
}

func openCodeModelProtocol(model, provider string) openCodeProtocol {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "@ai-sdk/openai":
		return openCodeProtocolResponses
	case "@ai-sdk/anthropic":
		return openCodeProtocolAnthropic
	case "@ai-sdk/google":
		return openCodeProtocolGemini
	case "@ai-sdk/openai-compatible":
		return openCodeProtocolChat
	}

	model = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(model)), "opencode/")
	switch {
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "grok"), strings.HasPrefix(model, "muse-spark"):
		return openCodeProtocolResponses
	case strings.HasPrefix(model, "claude"), strings.HasPrefix(model, "qwen"):
		return openCodeProtocolAnthropic
	case strings.HasPrefix(model, "gemini"):
		return openCodeProtocolGemini
	default:
		return openCodeProtocolChat
	}
}

// AxonHub's OpenCode Go transformer already routes GPT and Grok. Muse Spark
// is also Responses-only in OpenCode Go but is absent from this version's map.
func isOpenCodeGoResponsesModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "muse-spark")
}
