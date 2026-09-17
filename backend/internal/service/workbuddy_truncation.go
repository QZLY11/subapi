// Package service — WorkBuddy 流式聚合的截断防护。
//
// 移植自 workbuddy2api internal/upstream/truncation.go + sse.go（fix 65b2f33）。
//
// 背景：SSE 流被截断时，工具调用的 arguments 会只剩半截 JSON。若网关把脏参数
// 原样交给客户端，客户端 JSON 解析失败会卡死整条会话（生产表现为「回复到一半
// 就断」）。处置是丢弃残缺调用，而不是补成 {} 伪造合法外观。
//
// 截断有两个来源：
//   - finish_reason == "length"（模型因 max_tokens 提前中止）；
//   - 上游连接中断（EOF 收尾但未发 data: [DONE]）。
//
// 关键区分：只把「非空但无法解析」视为截断。空串是合法的无参数工具；能解析但
// 类型不对（标量 / 数组）属于模型输出错误，交给客户端 schema 校验回传即可。
package service

import (
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// isTruncatedWorkBuddyToolCallArguments 判定工具参数字符串是否因分片丢失而残缺
// （区别于「该工具本就无参数」）。
//   - 空串 / 纯空白 → false（合法无参工具）；
//   - 非空但 JSON 解析失败 → true（截断）；
//   - 能解析（含 null/标量/数组等任何合法 JSON）→ false。
func isTruncatedWorkBuddyToolCallArguments(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	var v any
	return json.Unmarshal([]byte(trimmed), &v) != nil
}

// dropTruncatedWorkBuddyToolCalls 过滤出 arguments 完整的 tool_call（返回新 slice）。
// 只依据 isTruncatedWorkBuddyToolCallArguments 判定，不改动任何保留的调用
// （正例零改动）。
func dropTruncatedWorkBuddyToolCalls(calls []apicompat.ChatToolCall) []apicompat.ChatToolCall {
	kept := make([]apicompat.ChatToolCall, 0, len(calls))
	for _, call := range calls {
		if isTruncatedWorkBuddyToolCallArguments(call.Function.Arguments) {
			continue
		}
		kept = append(kept, call)
	}
	return kept
}
