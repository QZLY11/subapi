//go:build unit

package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// wbChannelBlockedBody 是生产环境真实抓到的上游渠道风控响应体
// （ops_error_logs id 424-434，账号 20/24/27/34/35 累计命中 37 次）。
//
// 关键点：HTTP 400 且 msg 同时包含 "Illegal API invocation" 与
// "unapproved channel"，会与 wbContentBlockedMarkers（内容审核）重合，
// 因此判定顺序必须是渠道风控优先。
const wbChannelBlockedBody = `{"code":11128,"displayMsg":{"en":"The request was blocked by security policy. Please retry later or contact support.","zh":"请求被安全策略拦截，请稍后重试或联系支持。","zh-hant":"請求已被安全策略攔截，請稍後重試或聯繫支援。"},"msg":"Illegal API invocation from an unapproved channel","requestId":"7e58445f-97f8-42c4-9f2a-f11d233e2e3d"}`

// TestIsWorkBuddyChannelBlocked_RealBody 真实渠道风控响应必须被识别。
func TestIsWorkBuddyChannelBlocked_RealBody(t *testing.T) {
	require.True(t, IsWorkBuddyChannelBlocked(wbChannelBlockedBody))
	// 上游 JSON 序列化可能带空格。
	require.True(t, IsWorkBuddyChannelBlocked(`{"code": 11128,"msg":"Illegal API invocation from an unapproved channel"}`))
	// 裸 code 无 msg 时也应命中（文案随版本变化，业务码更稳定）。
	require.True(t, IsWorkBuddyChannelBlocked(`{"code":11128}`))
}

// TestIsWorkBuddyChannelBlocked_NoFalsePositive 其他上游响应不得误判。
func TestIsWorkBuddyChannelBlocked_NoFalsePositive(t *testing.T) {
	cases := map[string]string{
		"额度耗尽":   `{"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包。"}`,
		"软限流":    `{"code":6004,"msg":"rate limit exceeded"}`,
		"会话失效":   `{"code":12153,"msg":"Offline user session not found"}`,
		"模型不可用":  `{"code":11102,"msg":"model not found"}`,
		"请求体解析失败": `{"code":11101,"msg":"Unmarshal chat params failed"}`,
		"空体":     ``,
		"包含11128数字": `{"code":111280,"msg":"unrelated"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			require.False(t, IsWorkBuddyChannelBlocked(body),
				"不得把 %s 误判为渠道风控", name)
		})
	}
}

// TestClassifyWorkBuddyError_ChannelBlockedBeatsContentBlocked 是最核心的
// 回归点：渠道风控的英文文案同时命中内容审核关键词，若顺序反转就会被降级为
// 「不罚号」的 WBErrContentBlocked，账号继续被调度并持续返回 400 —— 这正是
// 用户「回复一句话就断」长时间未被修复的原因。
func TestClassifyWorkBuddyError_ChannelBlockedBeatsContentBlocked(t *testing.T) {
	got := ClassifyWorkBuddyError(http.StatusBadRequest, wbChannelBlockedBody)
	require.Equal(t, WBErrChannelBlocked, got,
		"code=11128 必须分类为渠道风控，而不是可轮转的内容审核拦截")
	require.NotEqual(t, WBErrContentBlocked, got)
	require.Equal(t, "channel_blocked", got.String())
}

// TestClassifyWorkBuddyError_ContentBlockedStillContentBlocked 确保修复没有
// 破坏原有语义：不含 11128 的纯内容审核响应仍走不罚号分支。
func TestClassifyWorkBuddyError_ContentBlockedStillContentBlocked(t *testing.T) {
	body := `{"code":11001,"msg":"Content blocked by security policy"}`
	got := ClassifyWorkBuddyError(http.StatusBadRequest, body)
	require.Equal(t, WBErrContentBlocked, got,
		"无 11128 业务码的内容审核必须保持「不罚号」语义")
}

// TestClassifyWorkBuddyError_ExistingKindsUnchanged 确认新增枚举追加在末尾，
// 既有分类值未发生位移。
func TestClassifyWorkBuddyError_ExistingKindsUnchanged(t *testing.T) {
	require.Equal(t, 0, int(WBErrNone))
	require.Equal(t, 1, int(WBErrHardCredit))
	require.Equal(t, 2, int(WBErrSoftRate))
	require.Equal(t, 3, int(WBErrSessionDead))
	require.Equal(t, 4, int(WBErrNotFound))
	require.Equal(t, 5, int(WBErrServer))
	require.Equal(t, 6, int(WBErrContentBlocked))
	require.Equal(t, 7, int(WBErrBadParams))
	require.Equal(t, 8, int(WBErrClient))
	require.Equal(t, 9, int(WBErrChannelBlocked),
		"新枚举必须追加在末尾，避免改变已持久化到日志/调度记录中的既有值")
}

// TestWBChannelBlockedCooldown_NotSeconds 冷却时长必须显著大于秒级限流，
// 否则坏号会在下一次调度中立刻复活，重现「回复一句就断」的循环。
func TestWBChannelBlockedCooldown_NotSeconds(t *testing.T) {
	require.GreaterOrEqual(t, wbChannelBlockedCooldown, 10*time.Minute,
		"渠道风控冷却不得低于 10 分钟")
}
