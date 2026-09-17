package service

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func wbFingerprintAccount(uid, domain string) *Account {
	return &Account{
		ID:       1,
		Platform: PlatformWorkBuddy,
		Credentials: map[string]any{
			"uid":    uid,
			"domain": domain,
		},
	}
}

// TestApplyWorkBuddyClientFingerprintHeaders_InjectsAll 钉死官方客户端指纹头
// 必须齐全。缺任一项上游都可能按「非官方客户端」判 401
// "not from a valid issuer"——账号看起来健康却始终无法对话。
func TestApplyWorkBuddyClientFingerprintHeaders_InjectsAll(t *testing.T) {
	h := http.Header{}
	applyWorkBuddyClientFingerprintHeaders(h, wbFingerprintAccount("uid-abc-123", "www.codebuddy.cn"))

	require.Equal(t, "1", h.Get("X-CodeBuddy-Request"), "客户端标记头必须注入")
	require.Equal(t, "zh-CN", h.Get("Accept-Language"), "国内域必须 zh-CN")
	require.NotEmpty(t, h.Get("X-Machine-ID"), "设备标识必须注入")
	require.NotEmpty(t, h.Get("X-Session-ID"), "会话标识必须注入")
	require.Len(t, h.Get("X-Machine-ID"), 36, "标识形态须为 36 hex")
	require.Len(t, h.Get("X-Session-ID"), 36)
}

// TestDeriveWorkBuddyAccountStableID_StableAndIsolated 钉死派生的两个契约：
// 同账号稳定（跨进程/跨重启上游视为同一设备）、跨账号绝不碰撞。
func TestDeriveWorkBuddyAccountStableID_StableAndIsolated(t *testing.T) {
	a := deriveWorkBuddyAccountStableID("uid-A", "machine")
	b := deriveWorkBuddyAccountStableID("uid-A", "machine")
	require.Equal(t, a, b, "同账号同用途必须稳定")

	require.NotEqual(t, a, deriveWorkBuddyAccountStableID("uid-B", "machine"),
		"不同账号绝不碰撞（否则所有账号被上游视为同一设备）")
	require.NotEqual(t, a, deriveWorkBuddyAccountStableID("uid-A", "session"),
		"不同用途必须相互独立")
}

// TestWorkBuddyAcceptLanguageFor_RealmSplit 钉死语种按 realm 切：
// 写错语种会被上游按地域不符处理。
func TestWorkBuddyAcceptLanguageFor_RealmSplit(t *testing.T) {
	require.Equal(t, "zh-CN", workBuddyAcceptLanguageFor(wbFingerprintAccount("u", "www.codebuddy.cn")))
	require.Equal(t, "zh-CN", workBuddyAcceptLanguageFor(wbFingerprintAccount("u", "workbuddy.cn")))
	require.Equal(t, "en-US", workBuddyAcceptLanguageFor(wbFingerprintAccount("u", "workbuddy.ai")),
		"国际版域必须 en-US")
	require.Equal(t, "zh-CN", workBuddyAcceptLanguageFor(wbFingerprintAccount("u", "")),
		"domain 缺失按国内口径（默认部署形态）")
}

// TestApplyWorkBuddyClientFingerprintHeaders_DoesNotOverwrite 只补不覆盖：
// 调用方显式设置过的值优先。
func TestApplyWorkBuddyClientFingerprintHeaders_DoesNotOverwrite(t *testing.T) {
	h := http.Header{}
	h.Set("Accept-Language", "ja-JP")
	h.Set("X-CodeBuddy-Request", "9")
	applyWorkBuddyClientFingerprintHeaders(h, wbFingerprintAccount("uid-x", "www.codebuddy.cn"))

	require.Equal(t, "ja-JP", h.Get("Accept-Language"), "显式值不得被覆盖")
	require.Equal(t, "9", h.Get("X-CodeBuddy-Request"))
}

// TestApplyWorkBuddyClientFingerprintHeaders_NoUIDSkipsDerivedIDs 无 uid 时
// 不得注入派生标识——注入跨账号一致的固定值会让上游把所有账号视为同一设备，
// 反而放大风控。
func TestApplyWorkBuddyClientFingerprintHeaders_NoUIDSkipsDerivedIDs(t *testing.T) {
	h := http.Header{}
	applyWorkBuddyClientFingerprintHeaders(h, wbFingerprintAccount("", "www.codebuddy.cn"))

	require.Empty(t, h.Get("X-Machine-ID"), "无 uid 时宁可不注入")
	require.Empty(t, h.Get("X-Session-ID"))
	require.Equal(t, "1", h.Get("X-CodeBuddy-Request"), "与 uid 无关的标记头仍须注入")
}

// TestWorkBuddyUpstreamHeaders_IncludesFingerprint 端到端：主注入函数必须
// 带上指纹头（确认接线没有遗漏）。
func TestWorkBuddyUpstreamHeaders_IncludesFingerprint(t *testing.T) {
	h := http.Header{}
	applyWorkBuddyUpstreamHeaders(h, wbFingerprintAccount("uid-e2e", "www.codebuddy.cn"))

	require.Equal(t, "1", h.Get("X-CodeBuddy-Request"))
	require.NotEmpty(t, h.Get("X-Machine-ID"))
	require.Equal(t, "uid-e2e", h.Get("X-User-Id"))
}

// TestWbAuthRefreshSourceIsPlugin 钉死 refresh 渠道标识用官方取值 "plugin"。
// sub2api 此前写的是自造词 "workbuddy"，上游按白名单校验该头取值。
func TestWbAuthRefreshSourceIsPlugin(t *testing.T) {
	require.Equal(t, "plugin", wbAuthRefreshSourcePlugin)
	require.NotEqual(t, "workbuddy", wbAuthRefreshSourcePlugin,
		"自造词会被上游判为非官方客户端")
	require.True(t, strings.HasPrefix(wbAuthRefreshSourcePlugin, "p"))
}
