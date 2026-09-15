package service

import (
	"net/http"
	"testing"
)

// TestIsMultiProtocolAPIKeyProvider_WorkBuddyExempt 钉死 workbuddy 平台在
// /v1/messages 调度闸门上的豁免：cc-switch / Claude Code 的辅助流量
// （模型列表、usage 统计）走 /v1/messages 时若被 allow_messages_dispatch
// 拦成 403，客户端本地熔断器累积失败后会拉黑整个 provider（生产事故）。
func TestIsMultiProtocolAPIKeyProvider_WorkBuddyExempt(t *testing.T) {
	if !IsMultiProtocolAPIKeyProvider(PlatformWorkBuddy) {
		t.Fatal("workbuddy must be treated as multi-protocol API key provider")
	}
	for _, p := range []string{PlatformKimi, PlatformZhipu, PlatformDeepseek, PlatformMiniMax, PlatformOpenCodeGo} {
		if !IsMultiProtocolAPIKeyProvider(p) {
			t.Fatalf("%s must remain exempt", p)
		}
	}
	if IsMultiProtocolAPIKeyProvider(PlatformOpenAI) || IsMultiProtocolAPIKeyProvider(PlatformAnthropic) {
		t.Fatal("openai/anthropic must not be exempt")
	}
}

// TestAggregateCCSSEToChatCompletionsResponse 钉死 WorkBuddy 强制流式上游的
// SSE→JSON 聚合：客户端请求非流式时，网关必须把 SSE 流聚合为单个
// chat.completion JSON，而不是把 SSE 文本当 JSON 解析导致 502。
func TestAggregateCCSSEToChatCompletionsResponse(t *testing.T) {
	sse := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"role":"assistant","content":"Hello"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-1","object":"chat.completion.chunk","model":"deepseek-v4.1-flash","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}

data: [DONE]

`

	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	if resp == nil || resp.Object != "chat.completion" {
		t.Fatalf("bad response object: %+v", resp)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("want 1 choice, got %d", len(resp.Choices))
	}
	if got := string(resp.Choices[0].Message.Content); got != `"Hello world"` {
		t.Fatalf("content = %s", got)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 12 || resp.Usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

// TestAggregateCCSSEToChatCompletionsResponse_ToolCalls 钉死流内 tool_calls
// 按 index 拼接为完整的 function call。
func TestAggregateCCSSEToChatCompletionsResponse_ToolCalls(t *testing.T) {
	sse := `data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"BJ\"}"}}]},"finish_reason":null}]}

data: {"object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

	resp, err := aggregateCCSSEToChatCompletionsResponse([]byte(sse))
	if err != nil {
		t.Fatalf("aggregate failed: %v", err)
	}
	if len(resp.Choices) != 1 || len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("choices = %+v", resp.Choices)
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "get_weather" || tc.Function.Arguments != `{"city":"BJ"}` {
		t.Fatalf("tool call = %+v", tc)
	}
	if resp.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
}

// TestLooksLikeSSEBody 钉死 SSE 检测：Content-Type 或 data: 前缀任一命中即可。
func TestLooksLikeSSEBody(t *testing.T) {
	if !looksLikeSSEBody(http.Header{"Content-Type": []string{"text/event-stream"}}, []byte("data: {}")) {
		t.Fatal("sse content-type must be detected")
	}
	if !looksLikeSSEBody(http.Header{}, []byte("data: {}")) {
		t.Fatal("data: prefix must be detected")
	}
	if looksLikeSSEBody(http.Header{"Content-Type": []string{"application/json"}}, []byte(`{"id":"x"}`)) {
		t.Fatal("json body must not be detected as sse")
	}
}
