// Package service — WorkBuddy 出站请求 prompt_cache_key 注入（费用优化）。
//
// 移植自 workbuddy2api internal/upstream/cache_key.go。
//
// 上游服务端支持 prompt_cache_key：同一段 8k token 前缀，
//   - 不带 → prompt_cache_hit_tokens=0, credit≈0.34
//   - 带   → prompt_cache_hit_tokens=7808, credit≈0.02（费用降 ~17×）
//
// 网关在此为每个出站 chat 请求注入一个稳定、按账号隔离的 cache key，让同一客户端
// 对同一账号的连续请求复用上游前缀缓存。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// injectWorkBuddyPromptCacheKey 在已改写的出站 body 上注入 prompt_cache_key 字段。
//
// 优先级：
//  1. body 已带 prompt_cache_key → 原值保留（客户端自知复用哪个键）
//  2. body 已带 conversation_id / conversationId → 用它做会话哈希源
//  3. 都没有 → 用入站 conversationID 参数（来自会话标识头解析）做哈希源
//
// 安全约束——按账号隔离：
//   - 生成键格式 `wb2a-<uid8>-<convHex>`
//   - uid8 是账号 UID 前 8 字符，跨账号绝不相同 → 跨账号缓存键绝不碰撞
//   - 跨账号复用同一 cache key 会让上游命中错账号的前缀缓存、泄露对方对话，
//     故 uid 是硬隔离因子
//
// 入参 uid 为账号 UID（空则用 "-"，但仍会注入键）。
// 两源都空时 convHex 为定值（每次新会话不复用前缀，但仍保留账号隔离段）。
func injectWorkBuddyPromptCacheKey(obj map[string]any, uid, conversationID string) {
	if obj == nil {
		return
	}
	// 优先级 1：客户端已显式带 key → 绝不覆盖。
	if existing, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(existing) != "" {
		return
	}
	// 优先级 2：body 里的 conversation_id / conversationId 做会话哈希源。
	conv := conversationID
	if v := workBuddyStrField(obj, "conversation_id"); v != "" {
		conv = v
	} else if v := workBuddyStrField(obj, "conversationId"); v != "" {
		conv = v
	}
	obj["prompt_cache_key"] = buildWorkBuddyCacheKey(uid, conv)
}

// buildWorkBuddyCacheKey 生成 `wb2a-<uid8>-<convHex>` 格式的稳定 cache key。
//
// uid8 提供账号隔离段；convHex = sha256(uid + conversationID)[:16] 的 hex 提供会话段
// （同账号同会话稳定、不同会话不同）。会话源为空时 convHex 仍由 uid 单独哈希，
// 保证跨账号绝不碰撞但同一空会话不复用（空会话 = 新会话语义）。
func buildWorkBuddyCacheKey(uid, conversation string) string {
	uid8 := uid
	if len(uid8) > 8 {
		uid8 = uid8[:8]
	}
	if uid8 == "" {
		uid8 = "-"
	}
	sum := sha256.Sum256([]byte(uid + "|" + conversation))
	convHex := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("wb2a-%s-%s", uid8, convHex)
}

// workBuddyStrField 从 map 取 string 字段，非 string 或空串返回 ""。
func workBuddyStrField(obj map[string]any, key string) string {
	v, ok := obj[key].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(v)
}
