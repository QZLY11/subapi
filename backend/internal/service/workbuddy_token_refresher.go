package service

import (
	"context"
	"errors"
	"hash/fnv"
	"strings"
	"time"
)

// WorkBuddy access token 通常 ~1h（expiresIn 由上游返回）。提前 1h 预热刷新，
// 与 Grok 的窗口口径一致，保证请求路径缓存未命中时账号池仍是热的。
const workBuddyTokenRefreshSkew = time.Hour

// Stampede 分散：按 account id 派生确定性抖动，避免同批导入的账号在同一个
// TokenRefreshService 周期内集中刷新。
const workBuddyTokenRefreshJitterMax = 3 * time.Minute

// 抖动下限：保证窗口不会缩到无意义的阈值以下。
const workBuddyTokenRefreshSkewMin = 30 * time.Minute

// WorkBuddyTokenRefresher 处理 WorkBuddy CN OAuth 账号的后台 token 刷新。
type WorkBuddyTokenRefresher struct {
	workbuddyOAuthService *WorkBuddyOAuthService
}

func NewWorkBuddyTokenRefresher(workbuddyOAuthService *WorkBuddyOAuthService) *WorkBuddyTokenRefresher {
	return &WorkBuddyTokenRefresher{workbuddyOAuthService: workbuddyOAuthService}
}

// CacheKey 返回用于分布式锁的缓存键。
func (r *WorkBuddyTokenRefresher) CacheKey(account *Account) string {
	return WorkBuddyTokenCacheKey(account)
}

// CanRefresh 判断是否能处理该账号：workbuddy 平台 + workbuddy_oauth 类型 + 有 refresh_token。
func (r *WorkBuddyTokenRefresher) CanRefresh(account *Account) bool {
	if account == nil || !account.IsWorkBuddyOAuth() {
		return false
	}
	return strings.TrimSpace(account.GetCredential("refresh_token")) != ""
}

// NeedsRefresh 基于 expires_at（Unix 秒）判断是否进入刷新窗口。
func (r *WorkBuddyTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	if account == nil || strings.TrimSpace(account.GetCredential("refresh_token")) == "" {
		return false
	}
	if strings.TrimSpace(account.GetCredential("access_token")) == "" {
		return true
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		// 无过期时间：保守判定为需要刷新，避免 token 静默过期。
		return true
	}
	if refreshWindow < workBuddyTokenRefreshSkew {
		refreshWindow = workBuddyTokenRefreshSkew
	}
	refreshWindow = workBuddyTokenRefreshWindowWithJitter(account.ID, refreshWindow)
	return time.Until(*expiresAt) < refreshWindow
}

// workBuddyTokenRefreshWindowWithJitter 返回 refreshWindow 减去一个基于 accountID
// 的稳定偏移（落在 [0, jitterMax]）。结果不会低于 workBuddyTokenRefreshSkewMin。
func workBuddyTokenRefreshWindowWithJitter(accountID int64, refreshWindow time.Duration) time.Duration {
	if accountID <= 0 || refreshWindow <= workBuddyTokenRefreshSkewMin {
		return refreshWindow
	}
	h := fnv.New32a()
	var b [8]byte
	id := uint64(accountID)
	for i := 0; i < 8; i++ {
		b[i] = byte(id >> (8 * i))
	}
	_, _ = h.Write(b[:])
	jitter := time.Duration(h.Sum32()%uint32(workBuddyTokenRefreshJitterMax/time.Second)) * time.Second
	out := refreshWindow - jitter
	if out < workBuddyTokenRefreshSkewMin {
		return workBuddyTokenRefreshSkewMin
	}
	return out
}

// Refresh 执行 token 刷新，返回合并后的 credentials（保留原有 base_url 等字段）。
func (r *WorkBuddyTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.workbuddyOAuthService == nil {
		return nil, errors.New("workbuddy oauth service is not configured")
	}
	auth := WorkBuddyAuthFromAccount(account)
	if auth == nil || strings.TrimSpace(auth.RefreshToken) == "" {
		return nil, errors.New("workbuddy account missing refresh_token")
	}
	tokenInfo, err := r.workbuddyOAuthService.RefreshToken(ctx, auth)
	if err != nil {
		return nil, err
	}
	newCredentials := r.workbuddyOAuthService.BuildAccountCredentials(tokenInfo)
	newCredentials = MergeCredentials(account.Credentials, newCredentials)
	// 保留账号显式配置的 base_url（若存在），避免刷新后丢失自定义上游地址。
	if baseURL := strings.TrimSpace(account.GetCredential("base_url")); baseURL != "" {
		newCredentials["base_url"] = baseURL
	}
	return newCredentials, nil
}
