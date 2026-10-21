package service

import (
	"encoding/json"
	"testing"
)

// TestEnsureWorkBuddyLeadingSystem_InjectsWhenFirstIsUser 校验首条不是 system 时
// 补一条前置 system。
//
// 上游规则：首条消息必须是 system prompt。缺失时返回 400 code=11128
// 「first message is not system prompt」，而该错误码的 displayTips 会把原因
// 误导成「渠道校验失败 / 不是官方客户端」，排查时容易误判成请求头问题。
func TestEnsureWorkBuddyLeadingSystem_InjectsWhenFirstIsUser(t *testing.T) {
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"hi"}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (system + user), got %d: %s", len(msgs), out)
	}
	first := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("first role = %q, want system", role)
	}
	if content, _ := first["content"].(string); content == "" {
		t.Fatalf("leading system content must not be empty")
	}
	// 用户消息必须原样保留在原位（不能被覆盖或丢弃）。
	second := msgs[1].(map[string]any)
	if role, _ := second["role"].(string); role != "user" {
		t.Fatalf("second role = %q, want user", role)
	}
	if content, _ := second["content"].(string); content != "hi" {
		t.Fatalf("user content altered: %q", content)
	}
}

// TestEnsureWorkBuddyLeadingSystem_KeepsExistingSystem 校验首条已是 system 时
// 不重复注入（否则会污染客户端/用户自带的提示词，并改变缓存前缀）。
func TestEnsureWorkBuddyLeadingSystem_KeepsExistingSystem(t *testing.T) {
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"system","content":"custom prompt"},{"role":"user","content":"hi"}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %s", len(msgs), out)
	}
	first := msgs[0].(map[string]any)
	if content, _ := first["content"].(string); content != "custom prompt" {
		t.Fatalf("existing system prompt was overwritten: %q", content)
	}
}

// TestEnsureWorkBuddyLeadingSystem_DeveloperBecomesSystem 校验 developer 角色
// 归一为 system 后不再额外注入前置 system（一次 developer 就够了）。
func TestEnsureWorkBuddyLeadingSystem_DeveloperBecomesSystem(t *testing.T) {
	body := `{"model":"deepseek-v4-pro","messages":[{"role":"developer","content":"dev prompt"},{"role":"user","content":"hi"}]}`
	out := PrepareWorkBuddyChatPayload([]byte(body))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (developer→system, no extra), got %d: %s", len(msgs), out)
	}
	first := msgs[0].(map[string]any)
	if role, _ := first["role"].(string); role != "system" {
		t.Fatalf("first role = %q, want system", role)
	}
	if content, _ := first["content"].(string); content != "dev prompt" {
		t.Fatalf("developer content lost: %q", content)
	}
}

// TestEnsureWorkBuddyLeadingSystem_EmptyMessagesUntouched 空 messages 不介入
// （交给上游自行报错，不伪造消息）。
func TestEnsureWorkBuddyLeadingSystem_EmptyMessagesUntouched(t *testing.T) {
	obj := map[string]any{"messages": []any{}}
	ensureWorkBuddyLeadingSystem(obj)
	if msgs, ok := obj["messages"].([]any); !ok || len(msgs) != 0 {
		t.Fatalf("empty messages must stay empty, got %v", obj["messages"])
	}
}
