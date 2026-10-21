// Package service — WorkBuddy 账号积分查询与签到编排服务。
//
// 与 workbuddy_billing.go（无状态上游调用）分层：本文件负责账号加载、token 预刷新、
// 结果落 extra 快照，以及全量批量签到。对齐 CNProviderBalanceService 的结构。
package service

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	// wbCreditsProbeTimeout 单次积分查询/签到上游超时。
	wbCreditsProbeTimeout = 20 * time.Second

	// wbCreditsRefreshSkew 查询/签到的 token 预刷新窗口。
	// access token 约 1h 有效，提前 10 分钟刷新（与 workbuddy2api checkinRefreshSkew 一致）。
	wbCreditsRefreshSkew = 10 * time.Minute

	// 批量调用之间的节流间隔，避免同一出口 IP 短时高频打上游 meter 接口。
	wbCreditsBatchDelay = 150 * time.Millisecond

	// 落 extra 的快照键（平台前缀，与 CN provider 的 <platform>_<suffix> 口径一致）。
	wbExtraCredits        = "workbuddy_credits"
	wbExtraCreditsUsed    = "workbuddy_credits_used"
	wbExtraCreditsSize    = "workbuddy_credits_size"
	wbExtraCreditsUpdated = "workbuddy_credits_updated"
	wbExtraCheckinAt      = "workbuddy_checkin_at"
	wbExtraCheckinDate    = "workbuddy_checkin_date"
	wbExtraCheckinStatus  = "workbuddy_checkin_status"

	// wbExtraCreditsNextAt 该账号下次允许由后台刷新的时刻（RFC3339 UTC）。
	// 读取端只看快照，不触发上游；后台按这个时刻错开逐个刷新。
	wbExtraCreditsNextAt = "workbuddy_credits_next_at"
)

// 后台错开刷新参数。
//
// 语义约定（务必区分，避免理解偏差）：
//   - wbCreditsRefreshInterval 是【同一个账号】两次查询之间的间隔，与账号总数无关。
//   - 全体账号的到期时刻由 wbStableOffset 用账号 ID 哈希铺在周期内，互不重叠。
//   - 每轮扫描的处理量由 wbRefreshQuotaFor 按账号数算出，而不是固定值；
//     这样才能做到「N 个账号都保持 10 分钟周期」，而不是「N 个账号挤在一个周期里」。
const (
	// wbCreditsRefreshInterval 单个账号的目标刷新周期：同一账号每 10 分钟查一次。
	wbCreditsRefreshInterval = 10 * time.Minute

	// wbCreditsRefreshTick 后台扫描周期：每分钟检查一次哪些账号到期。
	wbCreditsRefreshTick = time.Minute

	// wbCreditsRefreshJitterMax 相位抖动上限。
	// 取【完整周期】而非一个小值：账号 ID 的稳定哈希把到期时刻均匀铺在
	// [0, interval) 上，N 个账号就自然均分成 interval/N 的间隔逐个到期。
	//   62 个账号  → 约每 10 秒到期一个
	//   1000 个账号 → 约每 0.6 秒到期一个（受下方 maxPerTick 封顶）
	// 若只给一个小抖动值（如 2 分钟），1000 个账号会挤在 2 分钟里同时到期。
	wbCreditsRefreshJitterMax = wbCreditsRefreshInterval

	// wbCreditsRefreshMaxPerTick 单轮扫描允许处理的账号数硬上限。
	//
	// 存在的理由：无条件保证 N 个账号都在 interval 内刷一遍，每轮处理量
	// 就等于 N/(interval/tick)，上游压力随账号数线性增长 —— 1000 个账号
	// 相当于每 6 秒打一次 meter 接口，必然触发风控。
	//
	// 封顶后的实际周期 = max(interval, 账号数/maxPerTick × tick)：
	//   账号数 ≤ 300 → 10 分钟（300/10 = 30，恰为上限）
	//   账号数 = 1000 → 约 34 分钟（自动降频，保护上游）
	//   账号数 = 5000 → 约 167 分钟
	// 账号再多也不碰上游更快，这是刻意的：宁可数据旧一点，也不打死账号池。
	wbCreditsRefreshMaxPerTick = 30

	// wbCreditsRefreshConcurrency 单轮内部的并发查询上限。
	wbCreditsRefreshConcurrency = 4
)

// wbRefreshQuotaFor 计算单轮扫描应处理的账号数。
//
// 目标：让每个账号都在 interval 内被刷到一次。
// 所需吞吐 = 账号数 / 每周期包含的 tick 数；再受 maxPerTick 封顶。
// 返回 0 表示无需处理（账号为空或上限非法）。
func wbRefreshQuotaFor(totalAccounts int, interval time.Duration, maxPerTick int) int {
	if totalAccounts <= 0 || maxPerTick <= 0 {
		return 0
	}
	if interval <= 0 {
		interval = wbCreditsRefreshInterval
	}
	ticksPerCycle := int(interval / wbCreditsRefreshTick)
	if ticksPerCycle < 1 {
		ticksPerCycle = 1
	}
	// 向上取整，避免长期欠账导致周期悄悄漂移。
	quota := (totalAccounts + ticksPerCycle - 1) / ticksPerCycle
	if quota > maxPerTick {
		quota = maxPerTick
	}
	if quota < 1 {
		quota = 1
	}
	return quota
}

// wbCheckinDateLayout 签到日期键格式（本地时区的自然日，用于「今日已签到数」判定）。
const wbCheckinDateLayout = "2006-01-02"

// wbTodayCheckinDate 返回服务器本地时区的今日日期串。
// 签到调度按自然日（CST 9:00 / 21:00）计，故必须用本地日而非 UTC 日。
func wbTodayCheckinDate() string {
	return time.Now().Format(wbCheckinDateLayout)
}

// WorkBuddyCreditsService 查询 WorkBuddy 账号积分并执行签到。
type WorkBuddyCreditsService struct {
	accountRepo  AccountRepository
	oauthService *WorkBuddyOAuthService
	client       *WorkBuddyClient

	// checkinMu 串行化签到，避免并发批量签到对同一账号重复打上游。
	checkinMu sync.Mutex
}

// NewWorkBuddyCreditsService 构造积分/签到服务。
func NewWorkBuddyCreditsService(
	accountRepo AccountRepository,
	oauthService *WorkBuddyOAuthService,
) *WorkBuddyCreditsService {
	return &WorkBuddyCreditsService{
		accountRepo:  accountRepo,
		oauthService: oauthService,
		client:       NewWorkBuddyClient(),
	}
}

// wbLoadWorkBuddyAccount 加载并校验 WorkBuddy 账号。
func (s *WorkBuddyCreditsService) wbLoadWorkBuddyAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_NOT_CONFIGURED", "workbuddy credits service is not configured")
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "WORKBUDDY_CREDITS_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "WORKBUDDY_CREDITS_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsWorkBuddy() {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKBUDDY_CREDITS_INVALID_PLATFORM", "account is not a WorkBuddy account")
	}
	return account, nil
}

// wbEnsureFreshAuth 返回可用凭证，必要时先刷新 token。
// 刷新的新凭证会写回账号（access token 约 1h 有效，否则查询/签到会 401）。
func (s *WorkBuddyCreditsService) wbEnsureFreshAuth(ctx context.Context, account *Account) *WorkBuddyAuth {
	auth := WorkBuddyAuthFromAccount(account)
	if auth == nil {
		return nil
	}
	if auth.AccessToken != "" && !auth.NeedsRefresh(wbCreditsRefreshSkew) {
		return auth
	}
	if strings.TrimSpace(auth.RefreshToken) == "" || s.oauthService == nil {
		// 无 refresh_token：只能用现有 access_token（可能已过期，交给上游报错）。
		return auth
	}
	if err := s.client.RefreshToken(ctx, auth); err != nil {
		slog.Warn("workbuddy_credits_refresh_failed", "account_id", account.ID, "error", err)
		return auth
	}
	// 刷新成功：写回账号，避免下次再用旧 token。
	newCredentials := MergeCredentials(account.Credentials, map[string]any{
		"access_token":  auth.AccessToken,
		"refresh_token": auth.RefreshToken,
		"expires_at":    auth.ExpiresAt,
		"domain":        auth.Domain,
	})
	if err := persistAccountCredentials(ctx, s.accountRepo, account, newCredentials); err != nil {
		slog.Warn("workbuddy_credits_credentials_persist_failed", "account_id", account.ID, "error", err)
	}
	return auth
}

// QueryCredits 查询指定账号积分并落 extra 快照。
func (s *WorkBuddyCreditsService) QueryCredits(ctx context.Context, accountID int64) (*WorkBuddyCredits, error) {
	account, err := s.wbLoadWorkBuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return s.QueryCreditsForAccount(ctx, account)
}

// QueryCreditsForAccount 查询已加载账号的积分（批量入口复用，避免二次 GetByID）。
func (s *WorkBuddyCreditsService) QueryCreditsForAccount(ctx context.Context, account *Account) (*WorkBuddyCredits, error) {
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "WORKBUDDY_CREDITS_ACCOUNT_NOT_FOUND", "account not found")
	}
	if !account.IsWorkBuddy() {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKBUDDY_CREDITS_INVALID_PLATFORM", "account is not a WorkBuddy account")
	}
	auth := s.wbEnsureFreshAuth(ctx, account)
	if auth == nil || strings.TrimSpace(auth.AccessToken) == "" {
		return nil, infraerrors.New(http.StatusBadRequest, "WORKBUDDY_CREDITS_NO_TOKEN", "account has no access_token")
	}

	callCtx, cancel := context.WithTimeout(ctx, wbCreditsProbeTimeout)
	defer cancel()
	credits, err := s.client.FetchCredits(callCtx, auth)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_CREDITS_QUERY_FAILED", err.Error())
	}
	credits.Nickname = account.Name
	s.persistCreditsSnapshot(ctx, account.ID, credits)
	return credits, nil
}

// persistCreditsSnapshot 把积分结果写入 account.Extra（UI 免探测展示）。
func (s *WorkBuddyCreditsService) persistCreditsSnapshot(ctx context.Context, accountID int64, credits *WorkBuddyCredits) {
	if s == nil || s.accountRepo == nil || credits == nil {
		return
	}
	updates := map[string]any{
		wbExtraCredits:        credits.Remain,
		wbExtraCreditsUsed:    credits.Used,
		wbExtraCreditsSize:    credits.Size,
		wbExtraCreditsUpdated: time.Now().UTC().Format(time.RFC3339),
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, updates); err != nil {
		slog.Warn("workbuddy_credits_persist_failed", "account_id", accountID, "error", err)
	}
}

// CheckinAccount 对单个账号执行签到，返回结果（含签到后积分）。
func (s *WorkBuddyCreditsService) CheckinAccount(ctx context.Context, accountID int64) (*WorkBuddyCheckinResult, error) {
	account, err := s.wbLoadWorkBuddyAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()
	return s.checkinOneAccount(ctx, account), nil
}

// checkinOneAccount 执行单账号签到。
//
// 并发安全说明：批量路径（CheckinAllAccounts）并发调用本方法，每个账号
// 写自己的数据行，互不冲突；单账号路径（CheckinAccount）持有 checkinMu
// 防同一账号重入。上游「今日已签到」视为正常（already），不算失败。
func (s *WorkBuddyCreditsService) checkinOneAccount(ctx context.Context, account *Account) *WorkBuddyCheckinResult {
	result := &WorkBuddyCheckinResult{
		UID:      account.GetCredential("uid"),
		Nickname: account.Name,
	}
	// 国际版没有签到接口（只有网页侧每日活跃打卡），直接跳过而不是打上游。
	if r := wbRealmForAccount(account); !r.HasCheckin {
		result.Status = "skipped"
		result.Detail = "international realm has no checkin endpoint"
		return result
	}
	auth := s.wbEnsureFreshAuth(ctx, account)
	if auth == nil || strings.TrimSpace(auth.AccessToken) == "" {
		result.Status = "skipped"
		result.Detail = "no credentials"
		return result
	}

	callCtx, cancel := context.WithTimeout(ctx, wbCreditsProbeTimeout)
	defer cancel()
	// 先上报一条活跃事件：解锁 first_buddy 任务（领养前置），与签到配合。
	if err := s.client.ReportChatActivity(callCtx, auth); err != nil {
		slog.Warn("workbuddy_activity_report_failed", "account_id", account.ID, "error", err)
	}

	if err := s.client.DailyCheckin(callCtx, auth); err != nil {
		if IsWorkBuddyAlreadyCheckin(err) {
			result.Status = "already"
			result.Detail = "today already checked in"
		} else {
			result.Status = "fail"
			result.Detail = err.Error()
		}
	} else {
		result.Status = "ok"
	}

	// 签到后顺带查积分，返回给 UI 直接刷新展示。
	s.persistCheckinSnapshot(ctx, account.ID, result.Status)
	if credits, err := s.client.FetchCredits(callCtx, auth); err == nil {
		result.Credits = credits
		s.persistCreditsSnapshot(ctx, account.ID, credits)
	}
	return result
}

// persistCheckinSnapshot 记录签到时间与状态到 extra。
func (s *WorkBuddyCreditsService) persistCheckinSnapshot(ctx context.Context, accountID int64, status string) {
	if s == nil || s.accountRepo == nil {
		return
	}
	if err := s.accountRepo.UpdateExtra(ctx, accountID, map[string]any{
		wbExtraCheckinAt:     time.Now().UTC().Format(time.RFC3339),
		wbExtraCheckinDate:   wbTodayCheckinDate(),
		wbExtraCheckinStatus: status,
	}); err != nil {
		slog.Warn("workbuddy_checkin_persist_failed", "account_id", accountID, "error", err)
	}
}

// WorkBuddyCreditsSummary 全量积分汇总。
type WorkBuddyCreditsSummary struct {
	// AccountCount WorkBuddy 账号总数（全部，含查询失败）。
	AccountCount int `json:"account_count"`
	// TotalSize 总计分额度（所有账号 size 之和）。
	TotalSize int64 `json:"total_size"`
	// TotalRemain 剩余总积分（所有账号 remain 之和）。
	TotalRemain int64 `json:"total_remain"`
	// TotalUsed 已用总积分。
	TotalUsed int64 `json:"total_used"`
	// TodayCheckinCount 今日已签到账号数（按本地自然日，来自 extra 快照）。
	TodayCheckinCount int `json:"today_checkin_count"`
	// TodayCheckinDate 统计所用的本地日期（供 UI 显示"今天"）。
	TodayCheckinDate string `json:"today_checkin_date"`

	Accounts  []*WorkBuddyAccountCredits `json:"accounts"`
	OKCount   int                        `json:"ok_count"`
	FailCount int                        `json:"fail_count"`
	FetchedAt int64                      `json:"fetched_at"`
}

// WorkBuddyAccountCredits 单账号积分（含失败信息）。
type WorkBuddyAccountCredits struct {
	AccountID int64  `json:"account_id"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`
	Nickname  string `json:"nickname,omitempty"`
	Remain    int64  `json:"remain"`
	Used      int64  `json:"used"`
	Size      int64  `json:"size"`
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`

	// CheckedInToday 今日是否已签到（来自 extra 快照，不额外打上游）。
	CheckedInToday bool   `json:"checked_in_today"`
	CheckinStatus  string `json:"checkin_status,omitempty"`
}

// ListWorkBuddyAccounts 列出全部 WorkBuddy 账号（国内版 + 国际版）。
//
// 两个平台的积分/签到协议同源，只是上游基址与 UA 不同（由账号自身 realm
// 决定）。统一列出使管理端一次看到全部账号，各账号按自己的区域打上游。
func (s *WorkBuddyCreditsService) ListWorkBuddyAccounts(ctx context.Context) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_NOT_CONFIGURED", "workbuddy credits service is not configured")
	}
	var out []Account
	for _, platform := range []string{PlatformWorkBuddy, PlatformWorkBuddyIntl} {
		accounts, err := s.accountRepo.ListByPlatform(ctx, platform)
		if err != nil {
			return nil, infraerrors.Newf(http.StatusInternalServerError, "WORKBUDDY_CREDITS_LIST_FAILED", "list accounts (%s): %v", platform, err)
		}
		out = append(out, accounts...)
	}
	return out, nil
}

// QueryAllCredits 查询全部 workbuddy 账号积分并汇总
//（账号总数 / 总计分额度 / 剩余总积分 / 今日已签到数）。
//
// 这是【实时全量查询】，会逐个打上游（62 个账号约需数十秒），因此只应由
// 管理端的显式动作触发（例如"刷新积分"按钮），不要放在页面加载路径上。
// 页面加载请用 QueryCreditsSnapshot 读快照。
//
// 与旧实现（串行 for + 每轮 sleep）相比改为受限并发：上游 meter 接口对同一
// 出口 IP 有频率敏感度，故用信号量限制并发数，并保留每账号之间的节流间隔。
func (s *WorkBuddyCreditsService) QueryAllCredits(ctx context.Context) (*WorkBuddyCreditsSummary, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return nil, err
	}
	started := time.Now()
	today := wbTodayCheckinDate()
	summary := &WorkBuddyCreditsSummary{
		AccountCount:     len(accounts),
		TodayCheckinDate: today,
		Accounts:         make([]*WorkBuddyAccountCredits, 0, len(accounts)),
		FetchedAt:        started.Unix(),
	}

	// 结果按原账号顺序落位，保证并发下输出顺序稳定。
	rows := make([]*WorkBuddyAccountCredits, len(accounts))
	sem := make(chan struct{}, wbCreditsRefreshConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for i := range accounts {
		account := &accounts[i]
		rows[i] = &WorkBuddyAccountCredits{
			AccountID:      account.ID,
			Name:           account.Name,
			CheckedInToday: wbExtraString(account.Extra, wbExtraCheckinDate) == today,
			CheckinStatus:  wbExtraString(account.Extra, wbExtraCheckinStatus),
		}
		if rows[i].CheckedInToday {
			mu.Lock()
			summary.TodayCheckinCount++
			mu.Unlock()
		}

		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, acc *Account, row *WorkBuddyAccountCredits) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					row.Error = fmt.Sprintf("panic: %v", r)
					summary.FailCount++
					mu.Unlock()
				}
			}()

			credits, qErr := s.QueryCreditsForAccount(ctx, acc)
			mu.Lock()
			defer mu.Unlock()
			if qErr != nil {
				row.Error = qErr.Error()
				summary.FailCount++
				return
			}
			row.UID = credits.UID
			row.Nickname = credits.Nickname
			row.Remain = credits.Remain
			row.Used = credits.Used
			row.Size = credits.Size
			row.OK = true
			summary.TotalRemain += credits.Remain
			summary.TotalUsed += credits.Used
			summary.TotalSize += credits.Size
			summary.OKCount++
		}(i, account, rows[i])

		// 受限并发下的起步节流：避免 62 个 goroutine 在同一瞬间齐发。
		if i < len(accounts)-1 {
			select {
			case <-ctx.Done():
				wg.Wait()
				summary.Accounts = append(summary.Accounts, rows...)
				return summary, nil
			case <-time.After(wbCreditsBatchDelay):
			}
		}
	}
	wg.Wait()

	summary.Accounts = append(summary.Accounts, rows...)
	return summary, nil
}

// QueryCreditsSnapshot 只读 extra 中的积分快照，不打上游（毫秒级返回）。
//
// 页面加载走这里：数据可能略旧（由后台错开刷新维护，见 RefreshDueAccounts），
// 但绝不阻塞 UI。需要最新值时再显式调用 QueryAllCredits。
func (s *WorkBuddyCreditsService) QueryCreditsSnapshot(ctx context.Context) (*WorkBuddyCreditsSummary, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return nil, err
	}
	today := wbTodayCheckinDate()
	summary := &WorkBuddyCreditsSummary{
		AccountCount:     len(accounts),
		TodayCheckinDate: today,
		Accounts:         make([]*WorkBuddyAccountCredits, 0, len(accounts)),
	}
	var newest int64
	for i := range accounts {
		account := &accounts[i]
		row := &WorkBuddyAccountCredits{
			AccountID:      account.ID,
			Name:           account.Name,
			CheckedInToday: wbExtraString(account.Extra, wbExtraCheckinDate) == today,
			CheckinStatus:  wbExtraString(account.Extra, wbExtraCheckinStatus),
		}
		if row.CheckedInToday {
			summary.TodayCheckinCount++
		}
		// 快照缺字段时 remain/used/size 为 0，OK=false，由前端提示"未采集"。
		if remain, ok := wbExtraInt64(account.Extra, wbExtraCredits); ok {
			row.Remain = remain
			row.Used = wbExtraInt64Default(account.Extra, wbExtraCreditsUsed)
			row.Size = wbExtraInt64Default(account.Extra, wbExtraCreditsSize)
			row.OK = true
			summary.TotalRemain += row.Remain
			summary.TotalUsed += row.Used
			summary.TotalSize += row.Size
			summary.OKCount++
		} else {
			summary.FailCount++
		}
		if updated, err := time.Parse(time.RFC3339, wbExtraString(account.Extra, wbExtraCreditsUpdated)); err == nil {
			if u := updated.Unix(); u > newest {
				newest = u
			}
		}
		summary.Accounts = append(summary.Accounts, row)
	}
	summary.FetchedAt = newest
	return summary, nil
}

// RefreshDueAccounts 后台错开刷新一批到期的账号，返回成功刷新的数量。
//
// 处理量不是固定值，而由 wbRefreshQuotaFor 按当前账号总数与 settings 算出：
//   - 账号数不超过 maxPerTick×tick周期 时，保证每账号都在 interval 内刷一次；
//   - 超出后自动降频（周期 = 账号数/maxPerTick × tick），保护上游。
//
// settings 为 nil 时取默认值。到期判定 = extra.workbuddy_credits_next_at 缺失
// 或已过期；缺失表示从未采集（冷启动），纳入本轮后由配额自然摊开。
func (s *WorkBuddyCreditsService) RefreshDueAccounts(ctx context.Context, settings *WorkBuddyCreditsSettings) (int, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return 0, err
	}
	if settings == nil {
		settings = defaultWorkBuddyCreditsSettings()
	}
	interval := time.Duration(settings.RefreshIntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = wbCreditsRefreshInterval
	}
	quota := wbRefreshQuotaFor(len(accounts), interval, settings.MaxPerTick)
	if quota <= 0 {
		return 0, nil
	}
	now := time.Now()
	type due struct {
		acc *Account
		at  time.Time
	}
	var dues []due
	for i := range accounts {
		account := &accounts[i]
		nextAt, ok := wbNextAtFor(account, now, interval)
		if !ok {
			// 从未刷新过：立刻纳入本轮（冷启动由配额摊开，见函数注释）。
			dues = append(dues, due{acc: account, at: time.Time{}})
			continue
		}
		if !nextAt.After(now) {
			dues = append(dues, due{acc: account, at: nextAt})
		}
	}
	if len(dues) == 0 {
		return 0, nil
	}
	// 最久未刷新的优先。
	sort.Slice(dues, func(a, b int) bool { return dues[a].at.Before(dues[b].at) })
	if len(dues) > quota {
		dues = dues[:quota]
	}

	sem := make(chan struct{}, wbCreditsRefreshConcurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	refreshed := 0
	for _, d := range dues {
		wg.Add(1)
		sem <- struct{}{}
		go func(acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { _ = recover() }()
			if _, qErr := s.QueryCreditsForAccount(ctx, acc); qErr != nil {
				slog.Warn("workbuddy_credits_background_refresh_failed",
					"account_id", acc.ID, "error", qErr.Error())
			} else {
				mu.Lock()
				refreshed++
				mu.Unlock()
			}
			// 无论成功失败都推进 next_at，避免失败账号被每轮反复重试打爆上游。
			next := wbNextAtForWrite(acc.ID, time.Now(), interval)
			if err := s.accountRepo.UpdateExtra(ctx, acc.ID, map[string]any{
				wbExtraCreditsNextAt: next.UTC().Format(time.RFC3339),
			}); err != nil {
				slog.Warn("workbuddy_credits_next_at_persist_failed", "account_id", acc.ID, "error", err)
			}
		}(d.acc)
	}
	wg.Wait()
	return refreshed, nil
}

// wbNextAtFor 读取账号的下次刷新时刻。ok=false 表示从未写入过（视为立即到期，
// 但实际首次由配额摊开，不会一次性齐发上游）。interval 为当前生效的单账号周期。
func wbNextAtFor(account *Account, now time.Time, interval time.Duration) (time.Time, bool) {
	if account == nil {
		return time.Time{}, false
	}
	raw := wbExtraString(account.Extra, wbExtraCreditsNextAt)
	if raw == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, true
	}
	// 格式损坏：打散到未来一个周期内的稳定位置，避免坏数据让账号永久饿死。
	return now.Add(wbStableOffset(account.ID, interval)), true
}

// wbNextAtForWrite 计算写入用的下次刷新时刻：一个周期 + 账号 ID 的确定性相位偏移。
func wbNextAtForWrite(accountID int64, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = wbCreditsRefreshInterval
	}
	return now.Add(interval).Add(wbStableOffset(accountID, interval))
}

// wbStableOffset 返回基于账号 ID 的稳定偏移 [0, jitterMax)。
// 用 FNV-1a 哈希而非随机数：同一账号每次算出的偏移一致，测试可复现，
// 且新增账号会自然落进周期内的不同位置。
// jitterMax 取完整周期时，N 个账号的到期时刻就均匀铺在周期内。
func wbStableOffset(accountID int64, jitterMax time.Duration) time.Duration {
	if accountID <= 0 || jitterMax <= 0 {
		return 0
	}
	// 取整到秒；不足 1 秒时退化为无偏移（避免对 0 取模 panic）。
	secs := uint32(jitterMax / time.Second)
	if secs == 0 {
		return 0
	}
	h := fnv.New32a()
	var b [8]byte
	id := uint64(accountID)
	for i := 0; i < 8; i++ {
		b[i] = byte(id >> (8 * i))
	}
	_, _ = h.Write(b[:])
	return time.Duration(h.Sum32()%secs) * time.Second
}

// wbExtraInt64 读取 int64 快照值（JSON 反序列化后可能是 float64）。
func wbExtraInt64(extra map[string]any, key string) (int64, bool) {
	if extra == nil {
		return 0, false
	}
	v, ok := extra[key]
	if !ok || v == nil {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		if out, err := n.Int64(); err == nil {
			return out, true
		}
	}
	return 0, false
}

// wbExtraInt64Default 读取 int64 快照值，缺失返回 0。
func wbExtraInt64Default(extra map[string]any, key string) int64 {
	v, _ := wbExtraInt64(extra, key)
	return v
}

// wbExtraString 从 extra 读取字符串字段（缺失/类型不符返回空串）。
func wbExtraString(extra map[string]any, key string) string {
	if extra == nil {
		return ""
	}
	v, ok := extra[key]
	if !ok {
		return ""
	}
	s, _ := v.(string)
	return s
}

// CheckinAllAccounts 对全部 workbuddy 账号执行签到（手动「一键签到」）。
//
// 与 QueryAllCredits 一致的受限并发策略：每个账号需打 3 次上游
// （活跃上报 + 签到 + 积分回读），串行 62 个账号会顶满 5 分钟超时。
// 信号量限并发（wbCreditsRefreshConcurrency）并保留起步节流，把总耗时
// 压到约 1/并发数。不同账号写不同数据行，并发安全；checkinMu 仍由
// CheckinAccount（单账号接口）持有，防同一账号重入。
func (s *WorkBuddyCreditsService) CheckinAllAccounts(ctx context.Context) ([]*WorkBuddyCheckinResult, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return nil, err
	}

	// 结果按原账号顺序落位，保证并发下输出顺序稳定。
	results := make([]*WorkBuddyCheckinResult, len(accounts))
	sem := make(chan struct{}, wbCreditsRefreshConcurrency)
	var wg sync.WaitGroup

	for i := range accounts {
		account := &accounts[i]
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, acc *Account) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() { _ = recover() }()
			results[idx] = s.checkinOneAccount(ctx, acc)
		}(i, account)

		// 受限并发下的起步节流：避免 N 个 goroutine 同一瞬间齐发上游。
		if i < len(accounts)-1 {
			select {
			case <-ctx.Done():
				wg.Wait()
				slog.Info("workbuddy_checkin_all_done", "summary", WorkBuddyCheckinSummaryText(results))
				return results, nil
			case <-time.After(wbCreditsBatchDelay):
			}
		}
	}
	wg.Wait()
	slog.Info("workbuddy_checkin_all_done", "summary", WorkBuddyCheckinSummaryText(results))
	return results, nil
}

// WorkBuddyCheckinSummaryText 生成人类可读的签到汇总（日志/响应复用）。
func WorkBuddyCheckinSummaryText(results []*WorkBuddyCheckinResult) string {
	var ok, already, fail, skipped int
	for _, r := range results {
		if r == nil {
			continue
		}
		switch r.Status {
		case "ok":
			ok++
		case "already":
			already++
		case "fail":
			fail++
		default:
			skipped++
		}
	}
	return fmt.Sprintf("total=%d ok=%d already=%d fail=%d skipped=%d", len(results), ok, already, fail, skipped)
}
