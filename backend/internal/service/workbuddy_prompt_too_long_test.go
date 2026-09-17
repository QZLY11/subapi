package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// 上游真实形态：400 + code 11115「prompt is too long」。
const wbPromptTooLongBody = `{"code":11115,"msg":"prompt is too long: 300000 tokens > 128000 maximum"}`

// TestIsWorkBuddyPromptTooLong_RealBodies 真实上游超长响应必须被识别
// （业务码形态与纯文案形态）。
func TestIsWorkBuddyPromptTooLong_RealBodies(t *testing.T) {
	for _, body := range []string{
		wbPromptTooLongBody,
		`{"code": 11115}`,
		`{"code":"11115","msg":"too long"}`,
		`{"error":{"message":"This model's maximum context length is 128000 tokens"}}`,
		`{"msg":"请求内容过长，请缩减上下文"}`,
		`{"msg":"input is too long for this model"}`,
	} {
		require.True(t, IsWorkBuddyPromptTooLong(body), "must detect: %s", body)
	}
}

// TestIsWorkBuddyPromptTooLong_NoFalsePositive 其他上游响应不得误判。
// 特别是 111150 这类以 11115 为前缀的其他业务码（词边界锚定的意义）。
func TestIsWorkBuddyPromptTooLong_NoFalsePositive(t *testing.T) {
	for _, body := range []string{
		``,
		`{"code":111150,"msg":"other"}`,
		`{"code":14018,"msg":"额度已用尽"}`,
		`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
		`{"code":0,"msg":"ok"}`,
		`{"msg":"rate limit exceeded"}`,
	} {
		require.False(t, IsWorkBuddyPromptTooLong(body), "must not detect: %s", body)
	}
}

// TestClassifyWorkBuddyError_PromptTooLongIsRequestScoped 是本次移植最核心的
// 断言：超长必须分类为 WBErrPromptTooLong（请求级），而**不能**落进
// WBErrSoftRate —— 后者会给账号加冷却，把一次客户端上下文超限变成账号级处罚，
// 健康账号被反复误伤，表现为「回复到一半就断」。
func TestClassifyWorkBuddyError_PromptTooLongIsRequestScoped(t *testing.T) {
	got := ClassifyWorkBuddyError(http.StatusBadRequest, wbPromptTooLongBody)
	require.Equal(t, WBErrPromptTooLong, got,
		"超长请求必须分类为 WBErrPromptTooLong 才能做到不罚号不轮转")
	require.NotEqual(t, WBErrSoftRate, got, "绝不能退化成账号级软限流冷却")
	require.NotEqual(t, WBErrClient, got, "不能落进通用 4xx（会按账号级处理）")
}

// TestClassifyWorkBuddyError_PromptTooLongBeatsSoftRateSubstring 钉死判定顺序：
// 超长文案里的 "too many tokens" 命中 wbSoftRateMarkers 的 "too many" 子串，
// 判定必须在软限流之前拦截，否则会误判成软限流。
func TestClassifyWorkBuddyError_PromptTooLongBeatsSoftRateSubstring(t *testing.T) {
	body := `{"code":11115,"msg":"too many tokens in request (context overflow)"}`
	require.Equal(t, WBErrPromptTooLong, ClassifyWorkBuddyError(http.StatusBadRequest, body),
		"'too many tokens' 必须先被超长判定拦截，而非命中软限流的 'too many'")
}

// TestClassifyWorkBuddyError_ExistingKindsUnchangedAfterPromptTooLong 确认
// WBErrPromptTooLong 追加在末尾后，既有枚举数值全部不变——它们以数值形式进入
// 错误日志与调度决策记录，改变数值会污染历史数据的可比性。
func TestClassifyWorkBuddyError_ExistingKindsUnchangedAfterPromptTooLong(t *testing.T) {
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
	require.Equal(t, 10, int(WBErrPromptTooLong), "新枚举必须追加在末尾")
	require.Equal(t, "prompt_too_long", WBErrPromptTooLong.String())
}

// TestClassifyWorkBuddyError_QuotaStillBeatsPromptTooLong 额度耗尽（14018）
// 必须仍然优先判为 hard_credit：它比超长更严重（账号级），顺序不能反转。
func TestClassifyWorkBuddyError_QuotaStillBeatsPromptTooLong(t *testing.T) {
	body := `{"code":14018,"msg":"额度已用尽"}`
	require.Equal(t, WBErrHardCredit, ClassifyWorkBuddyError(http.StatusTooManyRequests, body))
}
