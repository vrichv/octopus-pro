package price

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/vrichv/octopus-pro/internal/client"
	"github.com/vrichv/octopus-pro/internal/model"
	"github.com/vrichv/octopus-pro/internal/op"
	"github.com/vrichv/octopus-pro/internal/utils/log"
)

const llmPriceUrl = "https://models.dev/api.json"

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"

// developerFamilies 定义研发商及其自研模型系列前缀。
var developerFamilies = map[string][]string{
	"openai":     {"gpt", "o"},
	"anthropic":  {"claude"},
	"google":     {"gemini", "gemma", "lyria", "veo"},
	"deepseek":   {"deepseek"},
	"xai":        {"grok"},
	"alibaba":    {"qwen", "qvq"},
	"zhipuai":    {"glm"},
	"minimax":    {"minimax"},
	"moonshotai": {"kimi"},
	"v0":         {"v0"},
	"xiaomi":     {"mimo"},
}

var (
	claudeTypeFirstPattern = regexp.MustCompile(`^claude-(opus|sonnet|haiku)-(\d)-(\d)(-.*)?$`)
	claudeVersionFirstPattern = regexp.MustCompile(`^claude-(\d)-(\d)-(opus|sonnet|haiku)(-.*)?$`)
)

// claudeAliases mirrors scripts/updatePrice.py so a runtime refresh preserves
// the generated names accepted by existing channels.
func claudeAliases(modelID string) []string {
	if matches := claudeTypeFirstPattern.FindStringSubmatch(modelID); matches != nil {
		modelType, major, minor, suffix := matches[1], matches[2], matches[3], matches[4]
		return []string{
			fmt.Sprintf("claude-%s-%s.%s%s", modelType, major, minor, suffix),
			fmt.Sprintf("claude-%s.%s-%s%s", major, minor, modelType, suffix),
			fmt.Sprintf("claude-%s-%s-%s%s", major, minor, modelType, suffix),
		}
	}
	if matches := claudeVersionFirstPattern.FindStringSubmatch(modelID); matches != nil {
		major, minor, modelType, suffix := matches[1], matches[2], matches[3], matches[4]
		return []string{
			fmt.Sprintf("claude-%s.%s-%s%s", major, minor, modelType, suffix),
			fmt.Sprintf("claude-%s-%s.%s%s", modelType, major, minor, suffix),
		}
	}
	return nil
}

func snapshotPrices() map[string]model.LLMPrice {
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()

	prices := make(map[string]model.LLMPrice, len(llmPrice))
	for modelID, price := range llmPrice {
		prices[modelID] = price
	}
	return prices
}

var lastUpdateTime time.Time

func fetchLLMPriceBody(ctx context.Context, useProxy bool) ([]byte, error) {
	httpClient, err := client.GetHTTPClientSystemProxy(useProxy)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llmPriceUrl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to fetch LLM info: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}
	return body, nil
}

func UpdateLLMPrice(ctx context.Context) error {
	log.Debugf("update LLM price task started")
	startTime := time.Now()
	defer func() {
		log.Debugf("update LLM price task finished, update time: %s", time.Since(startTime))
	}()

	body, err := fetchLLMPriceBody(ctx, false)
	if err != nil {
		log.Warnf("direct request failed, trying with proxy: %v", err)
		body, err = fetchLLMPriceBody(ctx, true)
		if err != nil {
			return err
		}
	}

	var rawPrice map[string]struct {
		Models map[string]struct {
			ID         string `json:"id"`
			Family     string `json:"family"`
			Modalities struct {
				Output []string `json:"output"`
			} `json:"modalities"`
			Cost model.LLMPrice `json:"cost"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &rawPrice); err != nil {
		return fmt.Errorf("failed to parse LLM info: %w", err)
	}

	updatedPrices := snapshotPrices()
	for provider, familyPrefixes := range developerFamilies {
		for _, priceModel := range rawPrice[provider].Models {
			modelID := strings.ToLower(priceModel.ID)
			modelFamily := strings.ToLower(priceModel.Family)

			if modelID == "" || !slices.Contains(priceModel.Modalities.Output, "text") || strings.Contains(modelID, "embed") || strings.Contains(modelFamily, "embed") {
				continue
			}

			isDeveloperModel := false
			for _, familyPrefix := range familyPrefixes {
				if strings.HasPrefix(modelFamily, familyPrefix) {
					isDeveloperModel = true
					break
				}
			}
			if !isDeveloperModel {
				continue
			}

			updatedPrices[modelID] = priceModel.Cost
			for _, alias := range claudeAliases(modelID) {
				updatedPrices[alias] = priceModel.Cost
			}
		}
	}

	llmPriceLock.Lock()
	llmPrice = updatedPrices
	lastUpdateTime = time.Now()
	llmPriceLock.Unlock()
	return nil
}

func GetLastUpdateTime() time.Time {
	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()
	return lastUpdateTime
}

func GetLLMPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(modelName)
	if dbPrice, err := op.LLMGet(modelName); err == nil {
		return &dbPrice
	}
	return LookupCalibratedPrice(modelName)
}

// LookupCalibratedPrice 只查 models.dev 校准表，不读数据库手工价。
func LookupCalibratedPrice(modelName string) *model.LLMPrice {
	modelName = strings.ToLower(modelName)

	llmPriceLock.RLock()
	defer llmPriceLock.RUnlock()

	if price, ok := llmPrice[modelName]; ok {
		return &price
	}

	modelNameSegments := strings.FieldsFunc(modelName, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.'
	})

	matchedModelID := ""
	matchedSegmentCount := 0
	ambiguous := false
	for modelID, price := range llmPrice {
		modelIDSegments := strings.Split(modelID, "-")
		for start := 0; start+len(modelIDSegments) <= len(modelNameSegments); start++ {
			matched := true
			for i, segment := range modelIDSegments {
				if modelNameSegments[start+i] != segment {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			if len(modelIDSegments) > matchedSegmentCount {
				matchedModelID = modelID
				matchedSegmentCount = len(modelIDSegments)
				ambiguous = false
			} else if len(modelIDSegments) == matchedSegmentCount && llmPrice[matchedModelID] != price {
				ambiguous = true
			}
			break
		}
	}
	if matchedModelID == "" || ambiguous {
		return nil
	}
	price := llmPrice[matchedModelID]
	return &price
}
