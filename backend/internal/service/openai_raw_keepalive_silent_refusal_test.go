//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestShouldSuppressKeepaliveForSilentRefusal 覆盖「无报错直接断开」的根因回归。
//
// 生产证据（ops/usage 实测，claude-cli/2.1.271，key id=4）：
//
//	19:28:25 | acct 25 | first_token_ms=109160 | duration_ms=116147
//	19:28:15 | acct 22 | first_token_ms=145946 | duration_ms=146562
//
// 这两个请求体均 >=64KB，触发静默拒绝检测器；检测器保留缓冲导致首字节前完全
// 抑制 keepalive，客户端在 109–146 秒静默中自行断开，服务端仍以 200 收尾且
// 只在 Debug 级记一行日志 —— 即用户观察到的「什么报错都没有，直接断开」。
func TestShouldSuppressKeepaliveForSilentRefusal(t *testing.T) {
	// 请求体 870KB：与用户真实 payload 一致，检测器必然启用。
	detector := newOpenAIChatSilentRefusalDetector(870 * 1024)
	require.True(t, detector.Enabled(), "870KB 请求体必须启用静默拒绝检测器")

	t.Run("grace 内抑制以保住 failover 语义", func(t *testing.T) {
		require.True(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, false, 0),
			"刚发出请求时应抑制，让空响应能透明 failover")
		require.True(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, false, 4*time.Second),
			"grace 之内仍应抑制")
	})

	t.Run("grace 到期后必须放行 keepalive", func(t *testing.T) {
		require.False(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, false, openAISilentRefusalCommitGrace),
			"到达 grace 边界即应放行，否则客户端静默断开")
		// 生产实测的 109s / 146s 两个真实取值必须放行。
		require.False(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, false, 109*time.Second),
			"109s（账号 25 实测首字节）必须放行 keepalive")
		require.False(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, false, 146*time.Second),
			"146s（账号 22 实测首字节）必须放行 keepalive")
	})

	t.Run("已开始输出后不再需要保留缓冲", func(t *testing.T) {
		require.False(t,
			shouldSuppressKeepaliveForSilentRefusal(detector, true, 0),
			"客户端已收到字节，保留缓冲已无意义，应正常保活")
	})

	t.Run("检测器未启用时不受影响", func(t *testing.T) {
		small := newOpenAIChatSilentRefusalDetector(1024)
		require.False(t, small.Enabled(), "小请求体不启用检测器")
		require.False(t,
			shouldSuppressKeepaliveForSilentRefusal(small, false, 0),
			"未启用检测器时必须按原有逻辑保活，行为不变")
	})

	t.Run("nil 检测器安全", func(t *testing.T) {
		require.False(t, shouldSuppressKeepaliveForSilentRefusal(nil, false, 0))
		require.False(t, shouldSuppressKeepaliveForSilentRefusal(nil, true, 10*time.Second))
	})
}

// TestOpenAISilentRefusalCommitGrace_ShorterThanClientTolerance 保证 grace
// 远小于客户端空闲超时，否则修复无效。
func TestOpenAISilentRefusalCommitGrace_ShorterThanClientTolerance(t *testing.T) {
	require.Greater(t, openAISilentRefusalCommitGrace, time.Duration(0))
	require.LessOrEqual(t, openAISilentRefusalCommitGrace, 15*time.Second,
		"grace 必须显著短于客户端空闲超时（秒级而非分钟级）")
	// keepalive 默认间隔 10s（gateway.stream_keepalive_interval），
	// grace 需与之相当或更短，确保停顿开始后能及时发出第一帧保活。
	require.LessOrEqual(t, openAISilentRefusalCommitGrace, 10*time.Second,
		"grace 不应超过默认 keepalive 间隔，避免出现整段静默")
}
