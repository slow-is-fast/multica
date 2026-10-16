package summary

import (
	"testing"
)

// block 是一个构造辅助：把 JSON 内容包成 ```multica-run-summary 围栏块。
func block(json string) string {
	return "```multica-run-summary\n" + json + "\n```"
}

func TestExtractRunSummary(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantOK    bool
		wantReason string
		wantDecis int
		wantOpen  int
	}{
		{
			name:       "无块：纯散文评论",
			content:    "我改了登录页，详见 diff。",
			wantOK:     false,
			wantReason: "no_block",
		},
		{
			name:       "块内不是合法 JSON",
			content:    block("{ this is not json"),
			wantOK:     false,
			wantReason: "malformed",
		},
		{
			name:       "版本未知",
			content:    block(`{"version":2,"decisions":[]}`),
			wantOK:     false,
			wantReason: "version_unknown",
		},
		{
			name:       "合法：一条决定、一个未完成项",
			content:    block(`{"version":1,"decisions":[{"what":"缓存用 Redis 而非本地 Map","why":"多实例下本地缓存不一致","refs":["c-123"]}],"open_items":[{"what":"灰度开关未接","why_left":"等运维排期"}]}`),
			wantOK:     true,
			wantDecis:  1,
			wantOpen:   1,
		},
		{
			name:       "合法：决定列表为空（仍采信，只是不产生条目）",
			content:    block(`{"version":1,"decisions":[],"open_items":[{"what":"x","why_left":"y"}]}`),
			wantOK:     true,
			wantDecis:  0,
			wantOpen:   1,
		},
		{
			name:       "块前后有散文与别的围栏块，仍能定位",
			content:    "结果见下。\n```html\n<div>chart</div>\n```\n```multica-run-summary\n{\"version\":1,\"decisions\":[{\"what\":\"a\",\"why\":\"b\"}]}\n```\n完。",
			wantOK:     true,
			wantDecis:  1,
		},
		{
			name:       "info string 大小写/拼写不符：不认",
			content:    "```multica-run-Summary\n{\"version\":1,\"decisions\":[]}\n```",
			wantOK:     false,
			wantReason: "no_block",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, ok, reason := ExtractRunSummary(tt.content)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v (reason=%q)", ok, tt.wantOK, reason)
			}
			if !tt.wantOK {
				if reason != tt.wantReason {
					t.Errorf("reason = %q, want %q", reason, tt.wantReason)
				}
				return
			}
			if len(s.Decisions) != tt.wantDecis {
				t.Errorf("decisions = %d, want %d", len(s.Decisions), tt.wantDecis)
			}
			if len(s.OpenItems) != tt.wantOpen {
				t.Errorf("open_items = %d, want %d", len(s.OpenItems), tt.wantOpen)
			}
			// round-trip 一致性：重序列化应无信息丢失。
			if !roundTripOK([]byte(extractFencedBlock(tt.content)), s) {
				t.Errorf("round-trip 校验未通过，块可能被静默改写了")
			}
		})
	}
}

func TestDecisionRationale(t *testing.T) {
	tests := []struct {
		name string
		d    decision
		want string
	}{
		{"只有 why", decision{What: "a", Why: "因为 X"}, "因为 X"},
		{"只有 refs", decision{What: "a", Refs: []string{"c-1", "c-2"}}, "引用: c-1, c-2"},
		{"why + refs", decision{What: "a", Why: "因为 X", Refs: []string{"c-1"}}, "因为 X 引用: c-1"},
		{"都没有", decision{What: "a"}, ""},
		{"why 带首尾空白被压平", decision{What: "a", Why: "  因为 X  "}, "因为 X"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decisionRationale(tt.d); got != tt.want {
				t.Errorf("decisionRationale() = %q, want %q", got, tt.want)
			}
		})
	}
}
