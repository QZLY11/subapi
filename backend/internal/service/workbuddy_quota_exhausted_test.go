//go:build unit

package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// wbQuotaExhaustedBody 是 WorkBuddy 上游在额度耗尽时返回的真实响应体。
//
// 关键点：HTTP 状态码是 429（与软限流相同），只有 body 里的业务码 14018
// 和中文文案「额度已用尽」能把它与「请求过于频繁」区分开。早期实现只收录
// 了「额度用尽」，而实际文案是「额度已用尽」，strings.Contains 判定失败，
// 该响应被误判为 WBErrSoftRate → 只进入数秒短冷却 → 坏账号立刻复活、
// 再次被调度、再次 429，用户侧表现为「回复一句就断」的反复断开。
const wbQuotaExhaustedBody = `{"error":{"data":{"code":14018,` +
	`"msg":"额度已用尽，请访问以下链接，购买加量包以获取更多额度：https://www.codebuddy.cn/profile/usage ",` +
	`"requestId":"a5051a0a-4b4e-4b3f-96fb-5e5f7468f83b"}}}`

func TestClassifyWorkBuddyError_QuotaExhaustedIsHardCredit(t *testing.T) {
	// 真实抓包：429 + 14018 + 「额度已用尽」必须识别为硬额度耗尽。
	got := ClassifyWorkBuddyError(http.StatusTooManyRequests, wbQuotaExhaustedBody)
	require.Equal(t, WBErrHardCredit, got,
		"额度耗尽（429+14018）必须分类为 WBErrHardCredit 才能获得长冷却，否则会退化成短冷却循环")
}

func TestClassifyWorkBuddyError_QuotaExhaustedChineseVariants(t *testing.T) {
	// 上游文案随版本变化，任一中文变体都必须命中硬额度耗尽。
	for _, msg := range []string{
		"额度已用尽",
		"额度用尽",
		"额度已耗尽",
		"额度耗尽",
		"额度用完",
		"额度不足",
		"余额不足",
		"积分不足",
	} {
		t.Run(msg, func(t *testing.T) {
			body := `{"error":{"data":{"code":14018,"msg":"` + msg + `，请购买加量包"}}}`
			require.Equal(t, WBErrHardCredit,
				ClassifyWorkBuddyError(http.StatusTooManyRequests, body))
		})
	}
}

func TestClassifyWorkBuddyError_QuotaExhaustedByCodeOnly(t *testing.T) {
	// 文案完全变化时，业务码 14018 仍须兜底命中。
	body := `{"error":{"data":{"code":14018,"msg":"unexpected upstream wording"}}}`
	require.Equal(t, WBErrHardCredit,
		ClassifyWorkBuddyError(http.StatusTooManyRequests, body))
}

func TestClassifyWorkBuddyError_SoftRateStaysSoft(t *testing.T) {
	// 回归保护：真正的软限流不能被误升级为硬额度耗尽（否则账号会被过度停调）。
	for _, body := range []string{
		`{"error":{"data":{"code":6004,"msg":"请求过于频繁，请稍后再试"}}}`,
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"data":{"code":14001,"msg":"too many requests"}}}`,
	} {
		got := ClassifyWorkBuddyError(http.StatusTooManyRequests, body)
		require.Equal(t, WBErrSoftRate, got,
			"软限流应保持 WBErrSoftRate，body=%s", body)
	}
}

func TestCNProviderResponseIndicatesInsufficientBalance_WorkBuddy(t *testing.T) {
	// 该 helper 是 429 可恢复路径的判定入口；WorkBuddy 额度耗尽的响应体
	// 必须在此命中，否则 handle429 会落入默认短冷却逻辑。
	require.True(t,
		cnProviderResponseIndicatesInsufficientBalance([]byte(wbQuotaExhaustedBody)),
		"WorkBuddy 额度耗尽响应体必须被识别为余额不足")

	require.True(t,
		cnProviderResponseIndicatesInsufficientBalance(
			[]byte(`{"error":{"data":{"code":14018,"msg":"额度已用尽"}}}`)))

	// 回归保护：软限流与普通错误不得命中，避免账号被过度停调。
	for _, body := range []string{
		`{"error":{"data":{"code":6004,"msg":"请求过于频繁"}}}`,
		`{"error":{"message":"rate limit exceeded"}}`,
		`{"error":{"data":{"code":14001,"msg":"model not found"}}}`,
		``,
	} {
		require.False(t,
			cnProviderResponseIndicatesInsufficientBalance([]byte(body)),
			"非余额不足响应不应命中，body=%s", body)
	}
}
