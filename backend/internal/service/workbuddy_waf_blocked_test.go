package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIsWorkBuddyWafBlocked_RealBodies 真实 WAF 拦截形态必须被识别。
// 上游风控收紧时返回的拦截页没有业务信封，与业务 403 形态可区分。
func TestIsWorkBuddyWafBlocked_RealBodies(t *testing.T) {
	for _, body := range []string{
		"", // 空体
		"<html><body>403 Forbidden</body></html>",
		"403 Forbidden",
		"blocked by WAF",
	} {
		require.True(t, IsWorkBuddyWafBlocked(http.StatusForbidden, body),
			"403 无业务信封必须判为 WAF 拦截: %q", body)
	}
}

// TestIsWorkBuddyWafBlocked_BusinessEnvelopeNotWaf 带业务信封的 403 不是
// WAF 拦截——它们有各自的权威分类（11128 渠道风控 / 11140 request illegal），
// 宁可漏判 WAF 也不误罚业务 403。
func TestIsWorkBuddyWafBlocked_BusinessEnvelopeNotWaf(t *testing.T) {
	for _, body := range []string{
		`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
		`{"code":11140,"msg":"request illegal"}`,
		`{"code": 11128, "msg": "x"}`,
		`{"msg":"some business error"}`,
	} {
		require.False(t, IsWorkBuddyWafBlocked(http.StatusForbidden, body),
			"带业务信封的 403 不得判为 WAF: %s", body)
	}
}

// TestIsWorkBuddyWafBlocked_NonForbiddenStatus 只有 403 才是 WAF 形态；
// 其他状态码（尤其 400/429）不得进入本判定，避免抢占既有分类。
func TestIsWorkBuddyWafBlocked_NonForbiddenStatus(t *testing.T) {
	for _, status := range []int{200, 400, 401, 404, 429, 500, 502} {
		require.False(t, IsWorkBuddyWafBlocked(status, "<html>403</html>"),
			"status %d 不得判为 WAF", status)
	}
}

// TestClassifyWorkBuddyError_WafBlockedIsAccountScoped 是本次移植的核心断言：
// WAF 形态必须分类为 WBErrWafBlock（账号软冷却），而**不能**落进
// WBErrClient —— 后者既不冷却也不隔离，被拦住的风控账号会继续留在池里被
// 反复调度，每次命中都把 403 抛给客户端（表现为「回复一句就断」）。
func TestClassifyWorkBuddyError_WafBlockedIsAccountScoped(t *testing.T) {
	got := ClassifyWorkBuddyError(http.StatusForbidden, "<html><body>403 Forbidden</body></html>")
	require.Equal(t, WBErrWafBlock, got,
		"WAF 拦截必须分类为 WBErrWafBlock 才能触发账号软冷却")
	require.NotEqual(t, WBErrClient, got, "不能落进通用 4xx（账号不会被冷却）")

	require.Equal(t, WBErrWafBlock, ClassifyWorkBuddyError(http.StatusForbidden, ""),
		"空体 403 同样是 WAF 形态")
}

// TestClassifyWorkBuddyError_Business403StillClassified 确认移植没有抢占
// 既有业务 403 分类。
func TestClassifyWorkBuddyError_Business403StillClassified(t *testing.T) {
	require.Equal(t, WBErrChannelBlocked,
		ClassifyWorkBuddyError(http.StatusForbidden, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`),
		"渠道风控 403 必须仍判为账号级隔离，不得被 WAF 判定抢占")
	require.Equal(t, WBErrClient,
		ClassifyWorkBuddyError(http.StatusForbidden, `{"code":11140,"msg":"request illegal"}`),
		"其他业务 403 仍落既有分类链")
}

// TestClassifyWorkBuddyError_EnumsUnchangedAfterWafBlock 确认 WBErrWafBlock
// 追加在末尾后既有枚举数值全部不变。
func TestClassifyWorkBuddyError_EnumsUnchangedAfterWafBlock(t *testing.T) {
	require.Equal(t, 0, int(WBErrNone))
	require.Equal(t, 1, int(WBErrHardCredit))
	require.Equal(t, 2, int(WBErrSoftRate))
	require.Equal(t, 3, int(WBErrSessionDead))
	require.Equal(t, 4, int(WBErrNotFound))
	require.Equal(t, 5, int(WBErrServer))
	require.Equal(t, 6, int(WBErrContentBlocked))
	require.Equal(t, 7, int(WBErrBadParams))
	require.Equal(t, 8, int(WBErrClient))
	require.Equal(t, 9, int(WBErrChannelBlocked))
	require.Equal(t, 10, int(WBErrPromptTooLong))
	require.Equal(t, 11, int(WBErrWafBlock), "新枚举必须追加在末尾")
	require.Equal(t, "waf_block", WBErrWafBlock.String())
}

// TestWorkBuddyWafBlockedCooldownShorterThanChannelBlocked 确认冷却时长口径：
// WAF 是窗口性风控（分钟级），渠道风控是账号级长期隔离。若两者时长口径颠倒，
// 要么把窗口性问题长期化（浪费账号），要么让账号级问题过早复活。
func TestWorkBuddyWafBlockedCooldownShorterThanChannelBlocked(t *testing.T) {
	require.Less(t, wbWafBlockedCooldown, wbChannelBlockedCooldown,
		"WAF 软冷却必须短于渠道风控隔离")
	require.Greater(t, wbWafBlockedCooldown.Seconds(), float64(0))
}
