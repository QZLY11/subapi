// Package service — WorkBuddy 会话头族（官方客户端的「对话身份」）。
//
// 背景：腾讯上游对 chat/completions 做渠道校验，要求请求带官方客户端的
// 会话头族。缺失时返回 400 / code=11128（"request illegal"，displayTips 说明
// “没通过渠道校验，可能不是官方客户端发出的”）。这类失败在账号本身完全健康
// 的情况下也会发生 —— 只补 UA 与 X-IDE-* 不够，会话头族是另一条独立校验。
//
// 头族与形态（对齐 workbuddy2api-hub wb_identity.py，5.8.4 抓包实测）：
//
//	X-Conversation-ID           带连字符的 UUID；同一会话多轮复用同一个值
//	X-Conversation-Request-ID   32 位无连字符 hex；「一次 user send」内复用
//	X-Conversation-Message-ID   32 位无连字符 hex；每请求新生成
//	X-Request-ID                同上（与 Message-ID 同值）
//	X-Root-Request-ID           主对话发，值 = 本轮 Request-ID
//
// 关键形态约束：
//   - 会话 id 绝不能每请求现生成。那会把一次多轮对话拆成上千个「一次性会话」，
//     在上游反作弊看来是最明显的非人类特征。这里按「账号 + 会话键」稳定派生：
//     同账号同会话恒定、不同账号/会话互不相同，也不会成为跨账号关联键。
//   - 请求/消息 id 形态是 UUIDv7 去连字符的 32hex（时间有序），用 UUIDv7 前
//     48 位装毫秒时间戳。
package service

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// WorkBuddy 会话头族头名。
const (
	wbHeaderConversationID        = "X-Conversation-ID"
	wbHeaderConversationRequestID = "X-Conversation-Request-ID"
	wbHeaderConversationMessageID = "X-Conversation-Message-ID"
	wbHeaderRequestID             = "X-Request-ID"
	wbHeaderRootRequestID         = "X-Root-Request-ID"
)

// wbNewRequestID 生成 32 位无连字符 hex 的请求/消息 id（UUIDv7 去连字符形态）。
//
// 前 48 位是毫秒时间戳，后续位随机（版本位 7、变体位 10），与官方客户端
// 的 X-Request-ID 形态一致。上游不校验版本号，形态贴近即可。
func wbNewRequestID() string {
	var buf [16]byte
	// 48 位毫秒时间戳打到前 6 字节（大端）。
	ms := uint64(time.Now().UnixMilli()) & ((1 << 48) - 1)
	buf[0] = byte(ms >> 40)
	buf[1] = byte(ms >> 32)
	buf[2] = byte(ms >> 24)
	buf[3] = byte(ms >> 16)
	buf[4] = byte(ms >> 8)
	buf[5] = byte(ms)
	// 其余 10 字节随机。
	if _, err := rand.Read(buf[6:]); err != nil {
		// 极端情况下退化为时间+纳秒，仍保持 16 字节长度。
		binary.BigEndian.PutUint64(buf[8:], uint64(time.Now().UnixNano()))
	}
	// 版本位 7：第 6 字节高 4 位。
	buf[6] = (buf[6] & 0x0F) | 0x70
	// 变体位 10：第 8 字节高 2 位。
	buf[8] = (buf[8] & 0x3F) | 0x80
	return hex.EncodeToString(buf[:])
}

// wbNewConversationID 生成带连字符的 UUIDv7 会话 id。
func wbNewConversationID() string {
	h := wbNewRequestID()
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// wbConversationIDFor 按「账号 uid + 会话键」稳定派生会话 id（带连字符 UUID）。
//
// 同一账号的同一会话多轮复用同一个值（贴近官方「一个会话一个 id」的语义），
// 不同账号 / 不同会话互不相同。会话键为空时返回空串，由调用方决定是否
// 回退到每请求新生成。
func wbConversationIDFor(uid, sessionKey string) string {
	key := strings.TrimSpace(sessionKey)
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.TrimSpace(uid) + "|" + key))
	h := hex.EncodeToString(sum[:16])
	// 版本位 4、变体位 a，形态与官方抓包的主对话 id 一致。
	b := []byte(h)
	b[12] = '4'
	b[16] = 'a'
	h = string(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// applyWorkBuddyConversationHeaders 为出站 chat 请求补齐会话头族。
//
// 只补不覆盖：调用方（网关管线）若已从客户端请求里透传了这些头，保持原值。
// sessionKey 用于稳定派生 X-Conversation-ID；为空时每请求新生成一个
// （退化行为，但仍满足「头存在」这一渠道校验前提）。
//
// 返回值是本次请求的 X-Request-ID，供调用方在日志/错误归因里复用。
func applyWorkBuddyConversationHeaders(h interface {
	Get(string) string
	Set(string, string)
}, a *Account, sessionKey string) string {
	if h == nil {
		return ""
	}
	uid := ""
	if a != nil {
		uid = strings.TrimSpace(a.GetCredential("uid"))
	}

	// 会话 id：同账号同会话稳定复用。
	conv := strings.TrimSpace(h.Get(wbHeaderConversationID))
	if conv == "" {
		conv = wbConversationIDFor(uid, sessionKey)
		if conv == "" {
			conv = wbNewConversationID()
		}
		h.Set(wbHeaderConversationID, conv)
	}

	// 本轮请求 id（= 消息 id）。
	reqID := strings.TrimSpace(h.Get(wbHeaderConversationRequestID))
	if reqID == "" {
		reqID = wbNewRequestID()
		h.Set(wbHeaderConversationRequestID, reqID)
	}

	msgID := strings.TrimSpace(h.Get(wbHeaderConversationMessageID))
	if msgID == "" {
		msgID = wbNewRequestID()
		h.Set(wbHeaderConversationMessageID, msgID)
	}

	if strings.TrimSpace(h.Get(wbHeaderRequestID)) == "" {
		h.Set(wbHeaderRequestID, msgID)
	}
	// 主对话（无更深层调用）才发 Root-Request-ID，值 = 本轮 Request-ID。
	if strings.TrimSpace(h.Get(wbHeaderRootRequestID)) == "" {
		h.Set(wbHeaderRootRequestID, reqID)
	}
	return msgID
}
