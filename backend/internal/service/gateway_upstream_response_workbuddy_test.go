package service

import (
	"regexp"
	"strings"
	"testing"
)

// dshStructuredContextOverflow 是 DSH（deepseek-harness）dsh-llm 包中
// isContextWindowExceededError 使用的结构化超限正则的等价复刻。
// 下游据此判定「上下文超限」并触发压缩+重试的自动恢复。
// 这里直接内联，保证改写结果与该判定长期对齐（正则一旦变动，本测试先失败）。
var dshStructuredContextOverflow = regexp.MustCompile(
	`(?i)(?:^|[^a-z0-9])context[\s_-](?:length|window)[\s_-](?:exceed(?:ed|s)?|overflow(?:ed)?|limit[\s_-]exceeded)(?:$|[^a-z0-9])`)

// TestNormalizeWorkBuddyOverflowWording 断言腾讯超限文案被补全为下游可识别句式，
// 且其他错误文本不受污染、重复调用不叠加后缀。
func TestNormalizeWorkBuddyOverflowWording(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantSuffix  bool // true = 应被追加后缀
		unchanged   bool // true = 应原样返回
	}{
		{
			name:       "tencent_prompt_too_long_gets_suffix",
			in:         "prompt is too long: 1048632 tokens > 1048576 maximum",
			wantSuffix: true,
		},
		{
			name:       "english_variant_gets_suffix",
			in:         "request is too long",
			wantSuffix: true,
		},
		{
			name:      "already_has_context_window_left_alone",
			in:        "prompt is too long for this model context window",
			unchanged: true,
		},
		{
			name:      "structured_wording_left_alone",
			in:        "context window exceeded",
			unchanged: true,
		},
		{
			name:      "unrelated_error_untouched",
			in:        "Invalid API key",
			unchanged: true,
		},
		{
			name:      "waf_channel_error_untouched",
			in:        "Illegal API invocation from an unapproved channel",
			unchanged: true,
		},
		{
			name:      "empty_stays_empty",
			in:        "",
			unchanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizeWorkBuddyOverflowWording(tc.in)
			if tc.unchanged && got != tc.in {
				t.Fatalf("expected unchanged %q, got %q", tc.in, got)
			}
			if tc.wantSuffix {
				if !strings.HasSuffix(got, "(context window exceeded)") {
					t.Fatalf("expected normalized suffix, got %q", got)
				}
				// 原句必须完整保留在前缀里（含精确 token 计数）。
				if !strings.HasPrefix(got, tc.in) {
					t.Fatalf("original text must be preserved, got %q", got)
				}
				// 核心断言：改写后必须能被 DSH 的溢出判定命中。
				if !dshStructuredContextOverflow.MatchString(got) {
					t.Fatalf("normalized text must match DSH overflow pattern, got %q", got)
				}
			}
			// 幂等：再跑一次不应叠加。
			again := normalizeWorkBuddyOverflowWording(got)
			if again != got {
				t.Fatalf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestExtractUpstreamErrorMessage_WorkBuddy 覆盖腾讯 WorkBuddy / CodeBuddy 的
// 非标准错误结构：不含 error/detail/message 任一标准字段，必须靠
// extError.message / msg / displayMsg.zh 提取，否则会被兜底成
// "Upstream error: <status>"，使下游无法判定上下文超限。
func TestExtractUpstreamErrorMessage_WorkBuddy(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "prompt_too_long_full_shape",
			body: `{"actions":["EDIT_INPUT"],"code":11115,` +
				`"displayMsg":{"en":"Content too long","zh":"内容过长，请精简或新建任务"},` +
				`"extError":{"StatusCode":400,"code":"invalid_request_error",` +
				`"message":"prompt is too long: 1048668 tokens > 1048576 maximum"},` +
				`"msg":"prompt is too long: 1048668 tokens > 1048576 maximum"}`,
			want: "prompt is too long: 1048668 tokens > 1048576 maximum (context window exceeded)",
		},
		{
			name: "msg_only",
			body: `{"code":11115,"msg":"prompt is too long: 300000 tokens > 128000 maximum"}`,
			want: "prompt is too long: 300000 tokens > 128000 maximum (context window exceeded)",
		},
		{
			name: "displaymsg_zh_fallback",
			body: `{"code":11115,"displayMsg":{"en":"Content too long","zh":"内容过长，请精简或新建任务"}}`,
			want: "内容过长，请精简或新建任务 (context window exceeded)",
		},
		{
			name: "standard_shape_still_wins",
			body: `{"error":{"message":"exceeds the context window"},"msg":"should not win"}`,
			want: "exceeds the context window",
		},
		{
			name: "unrelated_code_not_claimed",
			body: `{"code":14018,"status":"error"}`,
			want: "",
		},
		{
			name: "waf_error_not_polluted",
			body: `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
			want: "Illegal API invocation from an unapproved channel",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractUpstreamErrorMessage([]byte(tc.body))
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
