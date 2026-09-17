package service

import (
	"testing"
)

// TestAggregateCCSSE_EmptyContentFrameDoesNotSwallowText 是「空 content 帧」的
// 回归锚点（对应 workbuddy2api issue #142）。
//
// 上游首个 chunk 常见 OpenAI 风格 role-only 帧：content 为**空串**。若聚合器把
// 「见过 content 字段」当成「已取到正文」的 latch，后续真正文就可能被静默丢弃，
// 客户端只收到空回复——这正是「回复到一半断」的一类表现。
//
// sub2api 的聚合器用累加式 Builder（不是守卫式 latch），故预期不受影响；
// 本测试把该契约钉死，防止将来有人把它改成守卫式实现而回归。
func TestAggregateCCSSE_EmptyContentFrameDoesNotSwallowText(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}

data: {"id":"x1","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":"stop"}]}

data: [DONE]

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	got := string(resp.Choices[0].Message.Content)
	if got != `"Hello world"` {
		t.Fatalf("empty first frame swallowed text: content = %s (want \"Hello world\")", got)
	}
}

// TestAggregateCCSSE_EmptyFramesWithToolCallsStillKeepsCalls 空 content 帧
// 不得妨碍同一帧内 tool_calls / reasoning 的合并。
func TestAggregateCCSSE_EmptyFramesWithToolCallsStillKeepsCalls(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"},"index":0}]},"finish_reason":null}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 || got[0].Function.Name != "f" {
		t.Fatalf("tool call lost among empty content frames: %+v", got)
	}
}

// TestAggregateCCSSE_AllEmptyFramesYieldsEmptyString 全流皆空帧时正文为空串
// 且不报错（既有行为，确认改动没有引入新的失败路径）。
func TestAggregateCCSSE_AllEmptyFramesYieldsEmptyString(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":"stop"}]}

data: [DONE]

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("all-empty stream must still aggregate: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	if got := string(resp.Choices[0].Message.Content); got != `""` {
		t.Fatalf("content = %s (want empty string)", got)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}
