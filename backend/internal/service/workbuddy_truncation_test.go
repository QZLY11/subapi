package service

import (
	"testing"
)

// TestIsTruncatedWorkBuddyToolCallArguments 钉死截断判定的三个边界：
// 空串是合法无参工具、解析失败才是截断、任何合法 JSON 都不算截断。
func TestIsTruncatedWorkBuddyToolCallArguments(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
		why  string
	}{
		{"", false, "空串 = 无参工具，不是截断"},
		{"   ", false, "纯空白同理"},
		{`{"city":"BJ"}`, false, "完整 JSON"},
		{`null`, false, "合法 JSON 标量"},
		{`[1,2]`, false, "合法 JSON 数组"},
		{`{"cmd":`, true, "半截对象 = 截断"},
		{`{"cmd":"ls"`, true, "缺右括号 = 截断"},
		{`not json at all`, true, "完全非法 = 截断"},
	} {
		if got := isTruncatedWorkBuddyToolCallArguments(tc.raw); got != tc.want {
			t.Fatalf("isTruncated(%q) = %v, want %v (%s)", tc.raw, got, tc.want, tc.why)
		}
	}
}

// TestAggregateCCSSE_EOFDropsTruncatedToolCalls 是本次移植的核心断言：
// 上游流被掐断（EOF 收尾、无 [DONE]、无 finish_reason）时，tool_calls 的
// arguments 只剩半截 JSON。残缺调用必须丢弃——否则客户端解析非法 JSON 卡死
// 会话（这正是「回复到一半就断」的一类根因）。
func TestAggregateCCSSE_EOFDropsTruncatedToolCalls(t *testing.T) {
	// 注意：结尾无 data: [DONE]，模拟连接中断。
	sse := "data: {\"id\":\"x1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"cmd\\\":\"},\"index\":0}]}}]}\n\n" +
		"data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"\\\"ls\\\"\"},\"index\":0}]}}]}\n\n"

	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("EOF-truncated stream must still aggregate: %v", err)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	if got := resp.Choices[0].Message.ToolCalls; len(got) != 0 {
		t.Fatalf("truncated tool call must be dropped, got %+v", got)
	}
}

// TestAggregateCCSSE_DoneKeepsCompleteToolCall 是「不误伤正例」锚：正常
// [DONE] 收尾时完整参数必须原样保留。
func TestAggregateCCSSE_DoneKeepsCompleteToolCall(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 {
		t.Fatalf("complete tool call must be kept, got %+v", got)
	}
	if got[0].Function.Arguments != `{"cmd":"ls"}` {
		t.Fatalf("arguments mangled: %q", got[0].Function.Arguments)
	}
}

// TestAggregateCCSSE_LengthFinishDropsTruncated 钉死另一个截断来源：
// finish_reason=="length"（模型因 max_tokens 提前中止）同样要丢弃残缺参数。
func TestAggregateCCSSE_LengthFinishDropsTruncated(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"bash","arguments":"{\"cmd\":"},"index":0}]}}]}

data: {"id":"x1","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	if got := resp.Choices[0].Message.ToolCalls; len(got) != 0 {
		t.Fatalf("length-truncated call must be dropped, got %+v", got)
	}
}

// TestAggregateCCSSE_EmptyArgumentsToolSurvivesTruncation 无参工具
// （arguments 为空串）不是截断，即使流被掐断也必须保留。
func TestAggregateCCSSE_EmptyArgumentsToolSurvivesTruncation(t *testing.T) {
	sse := `data: {"id":"x1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"get_time","arguments":""},"index":0}]}}]}

`
	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	got := resp.Choices[0].Message.ToolCalls
	if len(got) != 1 {
		t.Fatalf("parameterless tool must survive, got %+v", got)
	}
	if got[0].Function.Name != "get_time" {
		t.Fatalf("wrong call kept: %+v", got[0])
	}
}
