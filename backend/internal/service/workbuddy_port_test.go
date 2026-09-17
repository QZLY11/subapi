package service

import (
	"encoding/json"
	"strings"
	"testing"
)

// ============================================================================
// 工具配对自愈（移植自 workbuddy2api tool_pairing.go）
// ============================================================================

// TestCleanupWorkBuddyOrphanToolCalls_PartialBatchSymmetricTrim 钉死「部分配对」
// 的对称裁剪：批 [c1,c2] 只回了 c1 时，两侧必须共用同一份 keepCalls——
// 只删 c2 并保留 c1 的调用与结果，绝不能整批删调用而留下孤儿 tool 结果
// （那会被上游判 11148 顶死整条会话）。
func TestCleanupWorkBuddyOrphanToolCalls_PartialBatchSymmetricTrim(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f1", "arguments": "{}"}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "f2", "arguments": "{}"}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r1"},
	}
	out, changed := cleanupWorkBuddyOrphanToolCalls(msgs)
	if !changed {
		t.Fatal("partial batch must be trimmed")
	}
	if len(out) != 3 {
		t.Fatalf("want 3 messages, got %d", len(out))
	}
	asst := out[1].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("want 1 kept call, got %d", len(tcs))
	}
	if id := tcs[0].(map[string]any)["id"]; id != "c1" {
		t.Fatalf("kept wrong call: %v", id)
	}
	// 关键不变量：留下的 tool 结果必须有对应调用（无孤儿）。
	ids := map[string]bool{}
	for _, tc := range tcs {
		ids[tc.(map[string]any)["id"].(string)] = true
	}
	for _, m := range out {
		msg, _ := m.(map[string]any)
		if msg["role"] == "tool" {
			if !ids[msg["tool_call_id"].(string)] {
				t.Fatal("orphan tool result survived: upstream would reject with 11148")
			}
		}
	}
}

// TestCleanupWorkBuddyOrphanToolCalls_DropsAllWhenNoResult 无任何结果时，
// assistant.tool_calls 键应被整体删除（而非留空数组）。
func TestCleanupWorkBuddyOrphanToolCalls_DropsAllWhenNoResult(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f1"}},
			},
		},
		// 工具执行失败：客户端没能写回结果消息。
		map[string]any{"role": "user", "content": "again"},
	}
	out, changed := cleanupWorkBuddyOrphanToolCalls(msgs)
	if !changed {
		t.Fatal("dangling call must be dropped")
	}
	asst := out[1].(map[string]any)
	if _, exists := asst["tool_calls"]; exists {
		t.Fatal("tool_calls key must be deleted, not left empty")
	}
}

// TestCleanupWorkBuddyOrphanToolCalls_NoToolTrafficIsZeroChange 无工具流量时
// 必须零改动（返回原 slice），避免无谓的 body 重写。
func TestCleanupWorkBuddyOrphanToolCalls_NoToolTrafficIsZeroChange(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "assistant", "content": "hello"},
	}
	out, changed := cleanupWorkBuddyOrphanToolCalls(msgs)
	if changed {
		t.Fatal("no tool traffic must be a no-op")
	}
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
}

// TestRepackWorkBuddyToolResultBlocks_MovesInjectedNoticeAfterResults 钉死
// Codex image_resize_notice 场景：插在两份 tool 结果中间的消息要被后移，
// 保证同批结果在 wire 上连续（否则上游判 11148）。
func TestRepackWorkBuddyToolResultBlocks_MovesInjectedNoticeAfterResults(t *testing.T) {
	msgs := []any{
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "c00", "type": "function", "function": map[string]any{"name": "f1"}},
				map[string]any{"id": "c01", "type": "function", "function": map[string]any{"name": "f2"}},
			},
		},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "r0"},
		map[string]any{"role": "developer", "content": "<image_resize_notice/>"},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "r1"},
	}
	out, changed := repackWorkBuddyToolResultBlocks(msgs)
	if !changed {
		t.Fatal("injected notice between tool results must be repacked")
	}
	// 期望顺序：assistant | tool c00 | tool c01 | developer
	if role := out[1].(map[string]any)["role"]; role != "tool" {
		t.Fatalf("out[1] = %v", role)
	}
	if id := out[1].(map[string]any)["tool_call_id"]; id != "c00" {
		t.Fatalf("out[1] id = %v", id)
	}
	if id := out[2].(map[string]any)["tool_call_id"]; id != "c01" {
		t.Fatalf("out[2] id = %v (results must be contiguous)", id)
	}
	if role := out[3].(map[string]any)["role"]; role != "developer" {
		t.Fatalf("out[3] = %v (notice must move after results)", role)
	}
}

// ============================================================================
// prompt_cache_key 注入（移植自 workbuddy2api cache_key.go）
// ============================================================================

// TestInjectWorkBuddyPromptCacheKey_StablePerAccountAndConversation 钉死
// 缓存键的稳定性与账号隔离：同账号同会话稳定、跨账号绝不碰撞
// （跨账号复用会命中他人前缀缓存并泄露对话）。
func TestInjectWorkBuddyPromptCacheKey_StablePerAccountAndConversation(t *testing.T) {
	build := func(uid, conv string) string {
		obj := map[string]any{"model": "deepseek-v4.1-flash"}
		injectWorkBuddyPromptCacheKey(obj, uid, conv)
		return obj["prompt_cache_key"].(string)
	}
	uidA := "846260bb-32f0-460c-b380-5890e461a895"
	uidB := "e68dba5d-a6b3-41d3-91d1-4d2763cf3036"
	conv := "conv-abc-123"

	k1 := build(uidA, conv)
	k2 := build(uidA, conv)
	if k1 != k2 {
		t.Fatalf("same account+conversation must be stable: %q vs %q", k1, k2)
	}
	if !strings.HasPrefix(k1, "wb2a-"+uidA[:8]+"-") {
		t.Fatalf("key must carry account isolation segment: %q", k1)
	}
	if other := build(uidA, "conv-other"); other == k1 {
		t.Fatal("different conversation must produce a different key")
	}
	if cross := build(uidB, conv); cross == k1 {
		t.Fatal("different account must NEVER collide (would leak cached prefix)")
	}
}

// TestInjectWorkBuddyPromptCacheKey_RespectsExplicitClientKey 客户端已带
// prompt_cache_key 时绝不覆盖。
func TestInjectWorkBuddyPromptCacheKey_RespectsExplicitClientKey(t *testing.T) {
	obj := map[string]any{"prompt_cache_key": "client-owned-key"}
	injectWorkBuddyPromptCacheKey(obj, "acct-1", "conv-1")
	if got := obj["prompt_cache_key"].(string); got != "client-owned-key" {
		t.Fatalf("explicit client key must be preserved, got %q", got)
	}
}

// TestInjectWorkBuddyPromptCacheKey_BodyConversationIDWins body 里的
// conversation_id 优先于入站参数作为哈希源。
func TestInjectWorkBuddyPromptCacheKey_BodyConversationIDWins(t *testing.T) {
	a := map[string]any{"conversation_id": "from-body"}
	injectWorkBuddyPromptCacheKey(a, "acct", "from-param")
	b := map[string]any{}
	injectWorkBuddyPromptCacheKey(b, "acct", "from-body")
	if a["prompt_cache_key"] != b["prompt_cache_key"] {
		t.Fatalf("body conversation_id must be the hash source: %v vs %v",
			a["prompt_cache_key"], b["prompt_cache_key"])
	}
}

// ============================================================================
// 端到端：改写管线接入
// ============================================================================

// TestPrepareWorkBuddyChatPayloadWithIdentity_InjectsCacheKeyAndHealsToolPairing
// 钉死完整管线的两个新增行为：注入 cache key（脱离原函数的空 uid 形态）
// 与工具配对自愈。
func TestPrepareWorkBuddyChatPayloadWithIdentity_InjectsCacheKeyAndHealsToolPairing(t *testing.T) {
	src := `{
	  "model": "deepseek-v4.1-flash",
	  "messages": [
	    {"role": "user", "content": "hi"},
	    {"role": "assistant", "tool_calls": [
	      {"id": "c1", "type": "function", "function": {"name": "f1", "arguments": "{}"}},
	      {"id": "c2", "type": "function", "function": {"name": "f2", "arguments": "{}"}}
	    ]}
	  ]
	}`
	out := prepareWorkBuddyChatPayloadWithIdentity([]byte(src), "acct-9999", "conv-xyz")

	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("output must be valid JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatal("stream must still be forced true")
	}
	key, ok := obj["prompt_cache_key"].(string)
	if !ok || !strings.HasPrefix(key, "wb2a-acct-999") {
		t.Fatalf("prompt_cache_key not injected with account isolation: %v", obj["prompt_cache_key"])
	}
	asst := obj["messages"].([]any)[1].(map[string]any)
	if _, exists := asst["tool_calls"]; exists {
		t.Fatal("dangling tool_calls (no results) must be dropped")
	}
}

// TestPrepareWorkBuddyChatPayload_BackwardCompatible 无身份参数的旧入口
// 仍能正常工作（空 uid 也注入键，隔离段退化为 "-"）。
func TestPrepareWorkBuddyChatPayload_BackwardCompatible(t *testing.T) {
	out := PrepareWorkBuddyChatPayload([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("must stay valid JSON: %v", err)
	}
	if obj["stream"] != true {
		t.Fatal("stream must be forced true")
	}
	if key, _ := obj["prompt_cache_key"].(string); !strings.HasPrefix(key, "wb2a--") {
		t.Fatalf("empty identity must still isolate by placeholder: %v", obj["prompt_cache_key"])
	}
}
