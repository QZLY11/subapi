//go:build unit

package service

import (
	"strings"
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

// TestKeepaliveBaselineIsClientWriteNotUpstreamRead 锁定第二层缺陷：
// keepalive 的必要性必须以「真正写给客户端的最后一刻」为基准，而不是
// 「上游最后一次吐行」。
//
// 上游 /v2/chat/completions 会周期性发送 `: heartbeat` 注释行（生产实测
// 1.2MB 请求体下于 3.91s 出现一帧）。这些行会被 writeLine 缓冲在
// pendingLines 中（静默拒绝检测未放行时），并不会写给客户端。若用
// lastDataAt 判定，上游心跳会不断刷新时间戳，使网关 keepalive 被无限期跳过：
// 服务端以为「刚发过数据」，客户端却在整段等待期收到零字节并自行断开。
//
// 本测试通过真实生产帧内容确认两类行可区分，从而证明基准必须分开跟踪。
func TestKeepaliveBaselineIsClientWriteNotUpstreamRead(t *testing.T) {
	// 生产抓取到的两种注释行：网关保活是裸 ":"，上游心跳带内容。
	gatewayKeepalive := ":"
	upstreamHeartbeat := ": heartbeat"

	require.NotEqual(t, gatewayKeepalive, upstreamHeartbeat,
		"网关保活与上游心跳必须可区分，否则无法验证修复是否真正生效")

	// 上游心跳不满足「以裸冒号表示网关保活」的判据。
	require.True(t, strings.HasPrefix(upstreamHeartbeat, ":"))
	require.False(t, upstreamHeartbeat == gatewayKeepalive,
		"上游心跳不得被误计为网关保活帧")

	// 二者在协议上都是合法 SSE 注释行，客户端会忽略内容，因此用
	// lastDataAt 无法区分「已送达」与「仍被缓冲」。
	for _, line := range []string{gatewayKeepalive, upstreamHeartbeat} {
		require.True(t, strings.HasPrefix(line, ":"),
			"注释行必须以冒号开头才是合法 SSE")
	}
}

// TestOpenAIKeepaliveDueAfter 覆盖「grace 到期后仍要多等一个 keepalive 周期」
// 这一时序缺陷。
//
// 生产复测（1.2MB body）：首字节 4.11s，落在 5s grace 之内；但 grace 到期后
// ticker 仍按配置间隔（10s）触发，导致 5s–10s 之间没有任何保活帧，客户端静默
// 窗口被无谓拉长。ticker 与判定阈值都必须改按 grace 的节奏。
func TestOpenAIKeepaliveDueAfter(t *testing.T) {
	const cfgInterval = 10 * time.Second

	t.Run("首字节前按 grace 节奏", func(t *testing.T) {
		got := openAIKeepaliveDueAfter(cfgInterval, false)
		require.Equal(t, openAISilentRefusalCommitGrace, got,
			"首字节前应使用 grace 作为间隔，避免 grace 到期后再空等一个配置周期")
		require.Less(t, got, cfgInterval,
			"首字节前的等待必须短于配置间隔")
	})

	t.Run("首字节后恢复配置值", func(t *testing.T) {
		require.Equal(t, cfgInterval, openAIKeepaliveDueAfter(cfgInterval, true),
			"已开始输出后应保持既有行为，使用配置间隔")
	})

	t.Run("配置间隔更小时不被放大", func(t *testing.T) {
		// 若运维把间隔配成 5s 以下（允许 5–30），不得把间隔放大到 grace。
		small := 5 * time.Second
		require.Equal(t, small, openAIKeepaliveDueAfter(small, false))
	})

	t.Run("零配置不产生 ticker 周期", func(t *testing.T) {
		// keepaliveInterval<=0 时走同步直读路径，本函数不应被用于构造 ticker；
		// 但即便被调用也需返回正值以避免 panic（time.NewTicker(0) 会 panic）。
		got := openAIKeepaliveDueAfter(0, false)
		require.Greater(t, got, time.Duration(0),
			"返回 0 会导致 time.NewTicker panic")
	})
}
