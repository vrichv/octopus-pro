package privacyfilter

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestSamplesFromTestdata 跑 testdata/cases.json 里的回归样本。
// 每次有新反馈 case，直接追加到 JSON 里即可，无需改 Go 代码。
func TestSamplesFromTestdata(t *testing.T) {
	type tc struct {
		ID        int    `json:"id"`
		Desc      string `json:"desc"`
		Input     string `json:"input"`
		ExpectHit bool   `json:"expect_hit"`
	}
	raw, err := os.ReadFile("testdata/cases.json")
	if err != nil {
		t.Fatalf("read cases.json: %v", err)
	}
	var cases []tc
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatalf("parse cases.json: %v", err)
	}
	f := newFilter(t)
	for _, c := range cases {
		r := f.Redact(c.Input)
		if r.Hit != c.ExpectHit {
			t.Errorf("#%d %s\n  input : %q\n  expect hit=%v\n  got    hit=%v out=%q",
				c.ID, c.Desc, c.Input, c.ExpectHit, r.Hit, r.Redacted)
		} else if c.ExpectHit && !strings.Contains(r.Redacted, "[") {
			t.Errorf("#%d %s: expected to contain [xxx] label but got %q", c.ID, c.Desc, r.Redacted)
		}
	}
}
