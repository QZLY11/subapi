package service

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// 国产供应商（kimi/zhipu/deepseek）的响应式冷却辅助。
//
// 与 openai/anthropic 不同：
//   - 余额不足是「可恢复」状态（充值/检测恢复后自动重新调度），不能走 handleAuthError
//     永久置 status=error。这里改为 SetTempUnschedulable，由 CN 余额检测周期任务
//     （cn_provider_balance_check_service.go）在余额恢复后 ClearTempUnschedulable。
//   - Coding Plan 滚动窗口耗尽（429）的冷却终点应是真实的窗口重置时间（已由
//     CNProviderQuotaService 落入 account.Extra 快照），而非默认的秒级兜底。

// cnBalanceExtraSuffixLow 标记账号响应过「余额不足」，供余额检测任务区分
// 「确属余额不足」与「尚未探测」。
const cnBalanceExtraSuffixLow = "balance_low"

// cnBalanceLowReasonPrefix 是余额不足临时停调 reason 的稳定前缀。
// 周期余额检测任务据此识别「是我们停调的」并在余额恢复后安全清除——不会误清
// 其他子系统（阈值/限流/401）写入的临时停调。
const cnBalanceLowReasonPrefix = "cn_balance_low"

const kimiConcurrentRequestLimitMessage = "You've reached your concurrent request limit. Please wait for your ongoing requests to finish and try again."

const cnConcurrencyLimitReasonPrefix = "cn_concurrency_limit"

// wbChannelBlockedReasonPrefix 是 WorkBuddy 渠道风控临时停调 reason 的稳定前缀。
const wbChannelBlockedReasonPrefix = "workbuddy_channel_blocked"

// wbChannelBlockedCooldown WorkBuddy 渠道风控临时停调时长。
//
// 上游对该账号打上渠道标记后不会立刻解除（生产实测同一账号连续命中，
// 账号 34 在 13:57–14:06 的 9 分钟内命中 12 次），因此冷却必须显著长于
// 秒级限流，让健康账号接替，而不是在坏号上反复重试。
const wbChannelBlockedCooldown = 30 * time.Minute

// handleWorkBuddyChannelBlocked 把上游「渠道风控拦截」（HTTP 400 + code 11128，
// msg="Illegal API invocation from an unapproved channel"）标记为账号级临时停调。
//
// 为什么必须隔离而不是当作普通 4xx 轮转：
//   - 该响应对**同一账号**是持续性的，换一次号重试就能成功，但坏号若继续留在
//     调度池里，客户端会在随机的后续请求上再次命中 400 并中断会话——表现为
//     「回复一句话就断开」且服务端错误日志稀疏、难以归因。
//   - 生产实测 24 小时内 6 个账号累计命中 37 次，且全部仍 schedulable=t。
//
// 与内容审核拦截（WBErrContentBlocked）区分：后者是单次请求的语义问题，
// 换号即可，不应影响账号健康度；这里是账号渠道被标记，必须冷却。
func (s *RateLimitService) handleWorkBuddyChannelBlocked(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	if s == nil || account == nil || s.accountRepo == nil {
		return
	}
	reason := wbChannelBlockedReasonPrefix
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		reason = wbChannelBlockedReasonPrefix + ": " + msg
	}
	until := time.Now().Add(wbChannelBlockedCooldown)
	s.notifyAccountSchedulingBlocked(account, until, wbChannelBlockedReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("workbuddy_channel_blocked_set_temp_unschedulable_failed",
			"account_id", account.ID, "error", err)
		return
	}
	slog.Info("workbuddy_channel_blocked",
		"account_id", account.ID,
		"until", until.UTC(),
		"cooldown_seconds", int(wbChannelBlockedCooldown.Seconds()),
	)
}

func isCNProviderConcurrencyLimit403(account *Account, upstreamMsg string) bool {
	return account != nil && account.Platform == PlatformKimi &&
		strings.TrimSpace(upstreamMsg) == kimiConcurrentRequestLimitMessage
}

// wbWafBlockedReasonPrefix 是 WAF 拦截软冷却的稳定 reason 前缀。
// 带前缀便于运维从账号的 temp_unschedulable 原因里区分「风控窗口」与
// 「渠道风控账号级隔离」（后者前缀为 wbChannelBlockedReasonPrefix）。
const wbWafBlockedReasonPrefix = "workbuddy WAF blocked"

// wbWafBlockedCooldown 是 WAF 拦截的软冷却时长。
//
// 比渠道风控隔离短得多：WAF 是上游风控的窗口性行为，窗口过去即恢复，
// 不需要长期隔离账号。取与 429 软限流同量级，让账号在窗口后自动回到池中。
const wbWafBlockedCooldown = 3 * time.Minute

// handleWorkBuddyWafBlocked 把 WAF 拦截（403 + 无业务信封）的账号临时停调。
//
// 移植自 workbuddy2api 的 ErrWafBlock 处理（applyErrorPolicy 软冷却分支）。
// 历史实现在 sub2api 缺失：这类 403 落进通用 4xx，账号既不冷却也不隔离，
// 被上游风控拦住的账号会继续留在池里被反复调度，每次命中都返回 403 给客户端，
// 客户端看到的是「回复一句就断」。软冷却后调度器会绕开它，直到窗口过去。
func (s *RateLimitService) handleWorkBuddyWafBlocked(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	if s == nil || account == nil || s.accountRepo == nil {
		return
	}
	reason := wbWafBlockedReasonPrefix
	if msg := strings.TrimSpace(upstreamMsg); msg != "" {
		reason = wbWafBlockedReasonPrefix + ": " + msg
	}
	until := time.Now().Add(wbWafBlockedCooldown)
	s.notifyAccountSchedulingBlocked(account, until, wbWafBlockedReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("workbuddy_waf_blocked_set_temp_unschedulable_failed",
			"account_id", account.ID, "error", err)
		return
	}
	slog.Info("workbuddy_waf_blocked",
		"account_id", account.ID,
		"until", until.UTC(),
		"cooldown_seconds", int(wbWafBlockedCooldown.Seconds()),
	)
}

func (s *RateLimitService) handleCNProviderConcurrencyLimit403(
	ctx context.Context,
	account *Account,
) {
	until := time.Now().Add(time.Duration(openAI403CooldownMinutesDefault) * time.Minute)
	reason := cnConcurrencyLimitReasonPrefix + ": " + kimiConcurrentRequestLimitMessage
	s.notifyAccountSchedulingBlocked(account, until, cnConcurrencyLimitReasonPrefix)
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, reason); err != nil {
		slog.Warn("cn_concurrency_limit_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_concurrency_limited",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceLowReason 构造余额不足临时停调的 reason（带稳定前缀）。
func cnBalanceLowReason(upstreamMsg string) string {
	if upstreamMsg = strings.TrimSpace(upstreamMsg); upstreamMsg != "" {
		return cnBalanceLowReasonPrefix + ": " + upstreamMsg
	}
	return cnBalanceLowReasonPrefix + ": 余额不足，账号临时停调"
}

// cnProviderResponseIndicatesInsufficientBalance 通过响应体文案识别余额不足
// （智谱 payg 无独立余额端点，仅能靠响应文案识别）。
func cnProviderResponseIndicatesInsufficientBalance(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	s := strings.ToLower(string(body))
	// WorkBuddy 额度耗尽返回 {"code":14018,"msg":"额度已用尽，请访问以下链接，购买加量包…"}，
	// HTTP 状态码为 429，与软限流不可区分，必须靠业务码 + 文案双通道识别。
	if strings.Contains(string(body), `"code":14018`) || strings.Contains(string(body), `"code": 14018`) {
		return true
	}
	return strings.Contains(s, "余额不足") ||
		strings.Contains(s, "额度不足") ||
		strings.Contains(s, "额度已用尽") ||
		strings.Contains(s, "额度用尽") ||
		strings.Contains(s, "额度已耗尽") ||
		strings.Contains(s, "额度耗尽") ||
		strings.Contains(s, "积分不足") ||
		strings.Contains(s, "积分用完") ||
		strings.Contains(s, "购买加量包") ||
		strings.Contains(s, "insufficient balance") ||
		strings.Contains(s, "insufficient_credit") ||
		strings.Contains(s, "balance is not enough") ||
		strings.Contains(s, "no enough balance")
}

// handleCNProviderInsufficientBalance 把余额不足标记为可恢复的临时停调：
// 写入 balance_low 快照 + SetTempUnschedulable 一个余额检测周期，
// 由周期任务在余额恢复后清除。返回前已通知调度阻塞。
func (s *RateLimitService) handleCNProviderInsufficientBalance(
	ctx context.Context,
	account *Account,
	upstreamMsg string,
) {
	msg := cnBalanceLowReason(upstreamMsg)

	if err := s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		cnExtraKey(account.Platform, cnBalanceExtraSuffixLow): true,
	}); err != nil {
		slog.Warn("cn_balance_low_mark_failed", "account_id", account.ID, "error", err)
	}

	until := time.Now().Add(s.cnBalanceCooldownDuration())
	s.notifyAccountSchedulingBlocked(account, until, "cn_insufficient_balance")
	if err := s.accountRepo.SetTempUnschedulable(ctx, account.ID, until, msg); err != nil {
		slog.Warn("cn_balance_set_temp_unschedulable_failed", "account_id", account.ID, "error", err)
		return
	}
	slog.Info("cn_provider_insufficient_balance",
		"account_id", account.ID,
		"platform", account.Platform,
		"until", until.UTC(),
	)
}

// cnBalanceCooldownDuration 返回余额不足临时停调的持续时长（= 2× 余额检测周期，
// 默认 20 分钟）。周期任务会在余额恢复后提前清除，故此处只需保证冷却覆盖到下一次
// 周期检测即可。
func (s *RateLimitService) cnBalanceCooldownDuration() time.Duration {
	minutes := 10
	if s != nil && s.cfg != nil {
		if cfgMin := s.cfg.Gateway.CNProviders.BalanceCheckIntervalMinutes; cfgMin > 0 {
			minutes = cfgMin
		}
	}
	cooldown := time.Duration(minutes) * time.Minute * 2
	if cooldown < time.Minute {
		cooldown = 10 * time.Minute
	}
	return cooldown
}

// cnProviderQuotaSnapshotReset 读取 Coding Plan 账号快照中最早一个仍在未来的窗口
// 重置时间（5h / weekly）。429 多数由 5h 滚动窗口触发，取较早的重置点可避免
// 把账号冷却到 weekly 重置（可达数天）的过度停调；如果确是 weekly 窗口耗尽，
// 周期额度探测刷新快照后阈值评估会再次停调到正确的时间点。
// 无快照或均已过期返回 nil。
func cnProviderQuotaSnapshotReset(account *Account, now time.Time) *time.Time {
	if account == nil || len(account.Extra) == 0 {
		return nil
	}
	if !account.IsOpenCodeGo() && (!account.IsCNProvider() || !account.IsCodingPlan()) {
		return nil
	}
	provider := account.Platform
	suffixes := []string{cnExtraSuffix5hReset, cnExtraSuffixWeeklyReset}
	if account.IsOpenCodeGo() {
		suffixes = append(suffixes, cnExtraSuffixMonthlyReset)
	}
	var earliest *time.Time
	for _, suffix := range suffixes {
		t := parseSchedulingResetAt(account.Extra[cnExtraKey(provider, suffix)])
		if t == nil || !t.After(now) {
			continue
		}
		if earliest == nil || t.Before(*earliest) {
			earliest = t
		}
	}
	return earliest
}

// applyCNProviderReactive429 处理国产供应商的 429 响应。
// 返回 true 表示已处理（调用方应 return），false 表示未命中、继续走默认 429 逻辑。
func (s *RateLimitService) applyCNProviderReactive429(
	ctx context.Context,
	account *Account,
	headers http.Header,
	responseBody []byte,
) bool {
	if account.IsOpenCodeGo() {
		if until := cnProviderQuotaSnapshotReset(account, time.Now()); until != nil {
			s.notifyAccountSchedulingBlocked(account, *until, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("opencode_go_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", *until,
			)
			return true
		}
		if resetAt := parseOpenAIRateLimitResetTime(responseBody); resetAt != nil {
			resetTime := time.Unix(*resetAt, 0)
			s.notifyAccountSchedulingBlocked(account, resetTime, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, resetTime); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("opencode_go_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", resetTime,
			)
			return true
		}
		return false
	}
	if !account.IsCNProvider() && !account.IsWorkBuddyOAuth() && account.Platform != PlatformWorkBuddy {
		return false
	}
	// 1) 余额不足文案：可恢复临时停调（含智谱 payg 这类无余额端点的场景）。
	if cnProviderResponseIndicatesInsufficientBalance(responseBody) {
		s.handleCNProviderInsufficientBalance(ctx, account, extractUpstreamErrorMessage(responseBody))
		return true
	}
	// 2) Coding Plan 窗口耗尽：冷却到快照中最早的窗口重置点（见
	// cnProviderQuotaSnapshotReset：429 多由 5h 窗口触发，取较早点避免过度停调）。
	if account.IsCodingPlan() {
		if until := cnProviderQuotaSnapshotReset(account, time.Now()); until != nil {
			s.notifyAccountSchedulingBlocked(account, *until, "429")
			if err := s.accountRepo.SetRateLimited(ctx, account.ID, *until); err != nil {
				slog.Warn("rate_limit_set_failed", "account_id", account.ID, "error", err)
				return true
			}
			slog.Info("cn_coding_plan_rate_limited",
				"account_id", account.ID,
				"platform", account.Platform,
				"reset_at", *until,
			)
			return true
		}
	}
	return false
}
