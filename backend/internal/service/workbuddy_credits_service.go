// Package service — WorkBuddy 账号积分查询与签到编排服务。
//
// 与 workbuddy_billing.go（无状态上游调用）分层：本文件负责账号加载、token 预刷新、
// 结果落 extra 快照，以及全量批量签到。对齐 CNProviderBalanceService 的结构。
package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
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
)

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

// checkinOneAccount 执行单账号签到（调用方持锁）。
// 上游「今日已签到」视为正常（already），不算失败。
func (s *WorkBuddyCreditsService) checkinOneAccount(ctx context.Context, account *Account) *WorkBuddyCheckinResult {
	result := &WorkBuddyCheckinResult{
		UID:      account.GetCredential("uid"),
		Nickname: account.Name,
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

// ListWorkBuddyAccounts 列出全部 workbuddy 账号。
func (s *WorkBuddyCreditsService) ListWorkBuddyAccounts(ctx context.Context) ([]Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_CREDITS_NOT_CONFIGURED", "workbuddy credits service is not configured")
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformWorkBuddy)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusInternalServerError, "WORKBUDDY_CREDITS_LIST_FAILED", "list accounts: %v", err)
	}
	return accounts, nil
}

// QueryAllCredits 查询全部 workbuddy 账号积分并汇总
//（账号总数 / 总计分额度 / 剩余总积分 / 今日已签到数）。
func (s *WorkBuddyCreditsService) QueryAllCredits(ctx context.Context) (*WorkBuddyCreditsSummary, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return nil, err
	}
	today := wbTodayCheckinDate()
	summary := &WorkBuddyCreditsSummary{
		AccountCount:     len(accounts),
		TodayCheckinDate: today,
		Accounts:         make([]*WorkBuddyAccountCredits, 0, len(accounts)),
		FetchedAt:        time.Now().Unix(),
	}
	for i := range accounts {
		account := &accounts[i]
		row := &WorkBuddyAccountCredits{
			AccountID: account.ID,
			Name:      account.Name,
		}
		// 今日签到状态取自 extra 快照（签到/查询时写入），不额外打上游。
		row.CheckedInToday = wbExtraString(account.Extra, wbExtraCheckinDate) == today
		row.CheckinStatus = wbExtraString(account.Extra, wbExtraCheckinStatus)
		if row.CheckedInToday {
			summary.TodayCheckinCount++
		}

		credits, qErr := s.QueryCreditsForAccount(ctx, account)
		if qErr != nil {
			row.Error = qErr.Error()
			summary.FailCount++
		} else {
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
		}
		summary.Accounts = append(summary.Accounts, row)
		if i < len(accounts)-1 {
			select {
			case <-ctx.Done():
				return summary, nil
			case <-time.After(wbCreditsBatchDelay):
			}
		}
	}
	return summary, nil
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
func (s *WorkBuddyCreditsService) CheckinAllAccounts(ctx context.Context) ([]*WorkBuddyCheckinResult, error) {
	accounts, err := s.ListWorkBuddyAccounts(ctx)
	if err != nil {
		return nil, err
	}
	s.checkinMu.Lock()
	defer s.checkinMu.Unlock()

	results := make([]*WorkBuddyCheckinResult, 0, len(accounts))
	for i := range accounts {
		account := &accounts[i]
		results = append(results, s.checkinOneAccount(ctx, account))
		if i < len(accounts)-1 {
			select {
			case <-ctx.Done():
				return results, nil
			case <-time.After(wbCreditsBatchDelay):
			}
		}
	}
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
