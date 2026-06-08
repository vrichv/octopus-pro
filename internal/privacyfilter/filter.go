// Package privacyfilter filters PII and secrets from text before it is sent to an LLM.
//
// The filter is regex-only, model-free, O(n), and safe for concurrent reuse after construction.
package privacyfilter

import (
	"sort"
	"strings"
)

// Entity 是一个被脱敏的命中项。Start/End 为 UTF-8 字节偏移。
type Entity struct {
	Type  string `json:"type"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

// Result 是一次脱敏的结果。
type Result struct {
	Redacted string   `json:"redacted"`
	Hit      bool     `json:"hit"`
	Count    int      `json:"count"`
	Entities []Entity `json:"entities"`
}

// span 是各检测层内部产出的区间。
type span struct {
	start, end int
	label      string
}

// Filter 持有编译好的规则。创建后只读，可并发安全地复用。
type Filter struct {
	secrets *secretDetector
}

// New creates a Filter from a gitleaks rules file path.
// Passing an empty path uses built-in fallback rules. Existing but invalid files return an error.
func New(gitleaksTOML string) (*Filter, error) {
	sd, err := newSecretDetectorFromFile(gitleaksTOML)
	if err != nil {
		return nil, err
	}
	return &Filter{secrets: sd}, nil
}

// NewFromBytes creates a Filter from embedded gitleaks TOML bytes.
// Passing an empty slice uses built-in fallback rules.
func NewFromBytes(gitleaksTOML []byte) (*Filter, error) {
	sd, err := newSecretDetectorFromBytes(gitleaksTOML)
	if err != nil {
		return nil, err
	}
	return &Filter{secrets: sd}, nil
}

// Stats 返回已加载的规则数，以及因语法不兼容被跳过的规则数。
func (f *Filter) Stats() (rules, skipped int) {
	return len(f.secrets.rules), f.secrets.skipped
}

// Redact 检测并脱敏文本。并发安全。
func (f *Filter) Redact(text string) Result {
	var spans []span
	spans = append(spans, detectPII(text)...)        // 结构化 PII
	spans = append(spans, f.secrets.detect(text)...) // 密钥 / 凭证

	merged := mergeSpans(spans)

	// 单遍扫描重建文本：merged 已按起点升序且互不重叠，O(n)
	var b strings.Builder
	prev := 0
	for _, s := range merged {
		b.WriteString(text[prev:s.start])
		b.WriteString(s.label)
		prev = s.end
	}
	b.WriteString(text[prev:])
	out := b.String()

	entities := make([]Entity, len(merged))
	for i, s := range merged {
		entities[i] = Entity{Type: s.label, Start: s.start, End: s.end, Text: text[s.start:s.end]}
	}
	return Result{
		Redacted: out,
		Hit:      len(merged) > 0,
		Count:    len(merged),
		Entities: entities,
	}
}

// mergeSpans 丢弃无效与重叠区间：按起点升序、同起点取更长者，
// 贪心保留互不重叠的区间。
func mergeSpans(spans []span) []span {
	var valid []span
	for _, s := range spans {
		if s.start >= 0 && s.start < s.end {
			valid = append(valid, s)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool {
		if valid[i].start != valid[j].start {
			return valid[i].start < valid[j].start
		}
		return valid[i].end > valid[j].end
	})
	merged := make([]span, 0, len(valid))
	lastEnd := -1
	for _, s := range valid {
		if s.start >= lastEnd {
			merged = append(merged, s)
			lastEnd = s.end
		}
	}
	return merged
}
