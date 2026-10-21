// Package service — WorkBuddy 出站请求头对齐官方客户端指纹。
//
// 移植自 workbuddy2api internal/upstream/headers.go 的 D1/D3/D5 与
// X-Machine-ID/X-Session-ID 派生（提交 3682b3e / 0ba1951 / e78a1af / 3b87c14）。
//
// 背景：腾讯上游对 WorkBuddy / CodeBuddy 的出站请求做客户端指纹校验。缺少
// 官方客户端会带的头，或头值形态不符（例如 refresh 渠道标识写成自造词而非
// 官方 "plugin"），会被判为「非官方客户端」并返回 401
// "not from a valid issuer"，表现为账号看起来健康却始终无法对话。
package service

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
)

// wbClientRequestMarkerValue 是 X-CodeBuddy-Request 的固定值（D1）。
// 官方客户端所有请求都带该标记，缺失会被上游按非客户端流量处理。
const wbClientRequestMarkerValue = "1"

// wbAuthRefreshSourcePlugin 是 X-Auth-Refresh-Source 的官方取值（D3）。
//
// 必须用官方的 "plugin"，而不是自造词（sub2api 此前写的是 "workbuddy"）。
// 该头的语义是「refresh token 来源渠道」，上游按白名单校验取值。
const wbAuthRefreshSourcePlugin = "plugin"

// applyWorkBuddyClientFingerprintHeaders 为出站 chat 请求补齐官方客户端指纹头。
//
// 补齐范围（只补不覆盖——调用方显式设置过的值优先）：
//   - X-CodeBuddy-Request: 1（客户端标记，D1）
//   - Accept-Language: 按账号 realm 切（CN → zh-CN，global → en-US，D5）
//   - X-Machine-ID: 按 uid 稳定派生的设备标识（跨会话稳定）
//   - X-Session-ID: 按 uid 稳定派生的账号固定会话标识（跨重启稳定）
//
// realm 判定依据账号 domain：含 codebuddy.cn / workbuddy.cn 等国内域为 cn，
// 其余为 global。domain 为空时按国内口径（与 sub2api 默认部署形态一致）。
func applyWorkBuddyClientFingerprintHeaders(h http.Header, a *Account) {
	if h == nil || a == nil {
		return
	}
	setIfEmpty(h, "X-CodeBuddy-Request", wbClientRequestMarkerValue)
	setIfEmpty(h, "Accept-Language", workBuddyAcceptLanguageFor(a))

	uid := a.GetCredential("uid")
	if uid = strings.TrimSpace(uid); uid == "" {
		// 无 uid 时无法派生稳定标识：宁可不注入，也不注入跨账号一致的固定值——
		// 那会让上游把所有账号视为同一设备，反而放大风控。
		return
	}
	setIfEmpty(h, "X-Machine-ID", deriveWorkBuddyAccountStableID(uid, "machine"))
	setIfEmpty(h, "X-Session-ID", deriveWorkBuddyAccountStableID(uid, "session"))
}

// workBuddyAcceptLanguageFor 按账号 realm 返回 Accept-Language（D5）。
// 官方客户端按账号域切：国内 zh-CN，国际版 en-US。写错语种会被上游按
// 地域不符处理。
func workBuddyAcceptLanguageFor(a *Account) string {
	if a == nil {
		return "zh-CN"
	}
	domain := strings.ToLower(strings.TrimSpace(a.GetCredential("domain")))
	if domain == "" {
		return "zh-CN"
	}
	// 国内域：codebuddy.cn / workbuddy.cn / tencent 系。其余（含 .ai / .com
	// 的国际版端点）按 global 口径返回 en-US。
	if strings.HasSuffix(domain, ".cn") || strings.Contains(domain, "tencent") {
		return "zh-CN"
	}
	return "en-US"
}

// deriveWorkBuddyAccountStableID 按 uid + 用途盐稳定派生设备/会话标识。
//
// 形态对齐官方客户端 5.8.4 抓包：machineId / sessionId 都是**带连字符的
// UUIDv4 形状**（8-4-4-4-12）。此前这里输出 36 位无连字符 hex，形态本身
// 就与官方不符，是一条可被识别的「网关特征」。
//
// 派生是纯函数：同账号跨进程/跨重启稳定（上游视为同一设备/会话），
// 不同账号绝不碰撞（盐里含 uid），不同用途（machine/session）相互独立。
// 取值必须按账号派生，不能在进程维度取一次 —— 那样所有账号会共用同一个
// 设备标识，反而把它们关联到同一台机器。
func deriveWorkBuddyAccountStableID(uid, purpose string) string {
	sum := sha256.Sum256([]byte("wb2a:" + purpose + ":" + uid))
	h := hex.EncodeToString(sum[:16])
	// 版本位 4、变体位 a，与官方 machineId 的 UUIDv4 形状一致。
	b := []byte(h)
	b[12] = '4'
	b[16] = 'a'
	h = string(b)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// setIfEmpty 仅在头缺失或为空时写入（调用方显式设置的值优先）。
func setIfEmpty(h http.Header, key, value string) {
	if value == "" {
		return
	}
	if strings.TrimSpace(h.Get(key)) != "" {
		return
	}
	h.Set(key, value)
}
