// Package service — WorkBuddy CN（CodeBuddy / copilot.tencent.com）上游 client。
//
// 本文件把 workbuddy2api 的 upstream 层（client/headers/payload 改写/错误分类）内嵌为
// sub2api 的 service，实现 platform=workbuddy + account_type=workbuddy_oauth 的原生支持。
//
// 上游端点（CN only，对齐 workbuddy2api internal/upstream）：
//   - chat:        POST https://copilot.tencent.com/v2/chat/completions
//   - refresh:     POST https://copilot.tencent.com/v2/plugin/auth/token/refresh
//   - models:      GET  https://copilot.tencent.com/console/enterprises/personal/models
//
// 账号凭证（credentials JSONB，对齐 workbuddy2api internal/auth.Auth）：
//   access_token / refresh_token / expires_at / uid / enterprise_id / domain / device_token
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ============================================================================
// 常量
// ============================================================================

const (
	wbUpstreamBaseCN = "https://copilot.tencent.com"
	wbOriginReferer  = "https://www.codebuddy.cn"

	// wbDefaultClientVersion 对齐官方 WorkBuddy 桌面端版本（workbuddy2api headers.go）。
	wbDefaultClientVersion = "5.5.4"
	// wbDefaultCliVersion 对齐官方内置 CLI 版本。
	wbDefaultCliVersion = "2.137.1"

	// DefaultWorkBuddyTestModel 是后台「测试连接」在 UI 未选模型时的回退值。
	// 必须是 CodeBuddy CN 目录中的 Chat Completions 模型 ID。
	DefaultWorkBuddyTestModel = "glm-5.2"
)

// DefaultWorkBuddyModelIDs 返回 CodeBuddy CN 当前公开的模型目录，
// 供 /v1/models 在尚未同步上游列表时回退，以及账号白名单预填。
// 与 workbuddy2api internal/server/handler.go staticModels 保持一致。
func DefaultWorkBuddyModelIDs() []string {
	return []string{
		"glm-5.2",
		"glm-5.1",
		"glm-5v-turbo",
		"kimi-k2.7",
		"minimax-m3",
		"hy3",
		"hy3-preview",
		"hy3-preview-agent",
		"deepseek-v4-pro",
		"deepseek-v4-flash",
	}
}

// ============================================================================
// 错误分类（对齐 workbuddy2api client.go Classify）
// ============================================================================

// WorkBuddyErrKind 上游错误分类，供调度器 map 到冷却/熔断/session 失效策略。
type WorkBuddyErrKind int

const (
	WBErrNone           WorkBuddyErrKind = iota // 成功
	WBErrHardCredit                             // 余额不足（402 或 body 关键词）→ 长冷却
	WBErrSoftRate                               // 429 软限流 → 短冷却
	WBErrSessionDead                            // 401 + 12153 offline session 失效 → 禁用
	WBErrNotFound                               // 404 上游偶发 → 短冷却，不计错
	WBErrServer                                 // 5xx 上游故障
	WBErrContentBlocked                         // 400 + 审核文案 → 不罚号，降级重试
	WBErrBadParams                              // 400 + 请求体解析失败 → 不罚号，仍轮转
	WBErrClient                                 // 其他 4xx / 业务错误
)

func (k WorkBuddyErrKind) String() string {
	switch k {
	case WBErrHardCredit:
		return "hard_credit"
	case WBErrSoftRate:
		return "soft_rate"
	case WBErrSessionDead:
		return "session_dead"
	case WBErrNotFound:
		return "not_found"
	case WBErrServer:
		return "server"
	case WBErrContentBlocked:
		return "content_blocked"
	case WBErrBadParams:
		return "bad_params"
	case WBErrClient:
		return "client"
	default:
		return "none"
	}
}

// 余额不足关键词（小写 + 中文原文双通道）。
var wbHardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// 限流/节流关键词（非 429 状态码也可能带限流文案）。
var wbSoftRateMarkers = []string{
	"rate limit", "rate-limiting", "rate-limited",
	"too many requests", "too many",
	"usage limit",
	"请求过于频繁", "限流",
}

// session 失效关键词。
var wbSessionDeadMarkers = []string{"Offline user session not found", "12153"}

// 内容策略拦截关键词（误报信号，不罚号）。
var wbContentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// 请求体解析失败关键词（发给上游的 body 有问题，不罚号）。
var wbBadParamsMarkerMsg = "Unmarshal chat params failed"
var wbBadParamsMarkerCode = `"code":11101`

// wbModelRateLimitCode 模型级 429 限流业务 code。
const wbModelRateLimitCode = "6004"

var wbCode6004Re = regexp.MustCompile(`"code"\s*:\s*"?` + wbModelRateLimitCode + `"?`)

// IsModelRateLimit 报告 429 body 是否明确指向模型级限流（业务 code 6004）。
func IsModelRateLimit(body string) bool {
	return wbCode6004Re.MatchString(body)
}

// ClassifyWorkBuddyError 按 HTTP 状态码 + body 分类上游错误。
// 判定顺序自严到宽（对齐 workbuddy2api Classify）。
func ClassifyWorkBuddyError(status int, body string) WorkBuddyErrKind {
	if status == http.StatusPaymentRequired {
		return WBErrHardCredit
	}
	lower := strings.ToLower(body)
	for _, m := range wbHardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return WBErrHardCredit
		}
	}
	for _, m := range wbSessionDeadMarkers {
		if strings.Contains(body, m) {
			return WBErrSessionDead
		}
	}
	for _, m := range wbSoftRateMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return WBErrSoftRate
		}
	}
	if status == http.StatusTooManyRequests {
		return WBErrSoftRate
	}
	if status == http.StatusNotFound {
		return WBErrNotFound
	}
	if status >= 500 {
		return WBErrServer
	}
	if status >= 400 {
		for _, m := range wbContentBlockedMarkers {
			if strings.Contains(lower, m) {
				return WBErrContentBlocked
			}
		}
		if strings.Contains(body, wbBadParamsMarkerMsg) || strings.Contains(body, wbBadParamsMarkerCode) {
			return WBErrBadParams
		}
		return WBErrClient
	}
	return WBErrNone
}

// ============================================================================
// 账号凭证
// ============================================================================

// WorkBuddyAuth 归一化账号凭证（对齐 workbuddy2api internal/auth.Auth）。
type WorkBuddyAuth struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	DeviceToken  string
}

// applyWorkBuddyUpstreamHeaders 在 OpenAI CC 转发管线上为 workbuddy 账号注入
// 专用身份头（X-User-Id / X-Enterprise-Id / X-Tenant-Id / X-Domain / X-Device-Token）
// 与官方 WorkBuddy 三段式 UA。Authorization 由管线统一以 Bearer <access_token> 注入，
// 不在此重复设置。
func applyWorkBuddyUpstreamHeaders(h http.Header, a *Account) {
	if a == nil {
		return
	}
	auth := WorkBuddyAuthFromAccount(a)
	if auth.UID != "" {
		h.Set("X-User-Id", auth.UID)
	}
	if auth.EnterpriseID != "" {
		h.Set("X-Enterprise-Id", auth.EnterpriseID)
		h.Set("X-Tenant-Id", auth.EnterpriseID)
	}
	if auth.Domain != "" {
		h.Set("X-Domain", auth.Domain)
	}
	if auth.DeviceToken != "" {
		h.Set("X-Device-Token", auth.DeviceToken)
	}
	// UA 优先级：账号 credentials.user_agent > 官方 WorkBuddy 三段式默认值。
	// 注意：不能 fallback 到 h.Get("User-Agent") —— 那可能是管线透传的客户端 UA
	// （openaiCCRawAllowedHeaders 放行了 user-agent，如 curl/x.x），上游会按 UA
	// 指纹校验 issuer，客户端 UA 会导致 401 "not from a valid issuer"。
	// GetOpenAIUserAgent() 只认 openai 平台，故 workbuddy 的账号级 UA 必须在这里读取。
	// Origin/Referer 必须为 CodeBuddy 白名单域，上游会校验，故无条件补齐。
	h.Set("Origin", wbOriginReferer)
	h.Set("Referer", wbOriginReferer+"/")
	ua := strings.TrimSpace(a.GetCredential("user_agent"))
	if ua == "" {
		ua = (&WorkBuddyClient{}).userAgent()
	}
	if ua != "" {
		h.Set("User-Agent", ua)
	}
}

// FromAccount 从 sub2api Account.Credentials 解析 WorkBuddyAuth。
func WorkBuddyAuthFromAccount(a *Account) *WorkBuddyAuth {
	if a == nil {
		return nil
	}
	return &WorkBuddyAuth{
		AccessToken:  a.GetCredential("access_token"),
		RefreshToken: a.GetCredential("refresh_token"),
		ExpiresAt:    a.GetCredentialAsInt64("expires_at"),
		Domain:       a.GetCredential("domain"),
		UID:          a.GetCredential("uid"),
		EnterpriseID: a.GetCredential("enterprise_id"),
		DeviceToken:  a.GetCredential("device_token"),
	}
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
func (a *WorkBuddyAuth) NeedsRefresh(within time.Duration) bool {
	if a == nil || a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// ============================================================================
// 上游 client
// ============================================================================

// WorkBuddyClient 封装对 CodeBuddy CN 上游的 HTTP 调用。Base 字段可覆盖便于测试。
type WorkBuddyClient struct {
	HTTP *http.Client

	// UserAgent 出站 UA 显式覆盖（空 = 默认官方三段式）。
	UserAgent string
	// ClientVersion / CliVersion 版本段覆盖（空 = 内置默认）。
	ClientVersion string
	CliVersion    string

	// ChatBase 上游基址覆盖（空 = copilot.tencent.com）。
	ChatBase string
	// BillingBase billing 域基址覆盖（空 = www.codebuddy.cn）；积分/签到/上报走此域。
	BillingBase string
}

// NewWorkBuddyClient 生产默认值（连接池减少 TLS 握手）。
func NewWorkBuddyClient() *WorkBuddyClient {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second,
	}
	return &WorkBuddyClient{
		HTTP: &http.Client{Timeout: 120 * time.Second, Transport: tr},
	}
}

func (c *WorkBuddyClient) clientVersion() string {
	if c != nil && c.ClientVersion != "" {
		return c.ClientVersion
	}
	return wbDefaultClientVersion
}

func (c *WorkBuddyClient) cliVersion() string {
	if c != nil && c.CliVersion != "" {
		return c.CliVersion
	}
	return wbDefaultCliVersion
}

func (c *WorkBuddyClient) chatBase() string {
	if c != nil && c.ChatBase != "" {
		return strings.TrimRight(c.ChatBase, "/")
	}
	return wbUpstreamBaseCN
}

// userAgent 返回默认官方三段式 UA（WorkBuddy/<ver> WorkBuddy/<ver> CLI/<ver>）。
func (c *WorkBuddyClient) userAgent() string {
	if c != nil && c.UserAgent != "" {
		return c.UserAgent
	}
	v := c.clientVersion()
	return fmt.Sprintf("WorkBuddy/%s WorkBuddy/%s CLI/%s", v, v, c.cliVersion())
}

// CommonHeaders 设置所有上游共享头。
func (c *WorkBuddyClient) CommonHeaders(req *http.Request, a *WorkBuddyAuth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", wbOriginReferer)
	req.Header.Set("Referer", wbOriginReferer+"/")
	req.Header.Set("User-Agent", c.userAgent())
	if a == nil {
		return
	}
	if a.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+a.AccessToken)
	}
	if a.UID != "" {
		req.Header.Set("X-User-Id", a.UID)
	}
	if a.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		req.Header.Set("X-Tenant-Id", a.EnterpriseID)
	}
	if a.Domain != "" {
		req.Header.Set("X-Domain", a.Domain)
	}
	if a.DeviceToken != "" {
		req.Header.Set("X-Device-Token", a.DeviceToken)
	}
}

// RefreshHeaders refresh 端点专属头（X-Refresh-Token 只出现在这里）。
func (c *WorkBuddyClient) RefreshHeaders(req *http.Request, a *WorkBuddyAuth) {
	c.CommonHeaders(req, a)
	if a != nil {
		req.Header.Set("X-Refresh-Token", a.RefreshToken)
		if a.EnterpriseID != "" {
			req.Header.Set("X-Enterprise-Id", a.EnterpriseID)
		}
	}
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
}

// wbEnvelope 上游统一信封 {code,msg,data}。
type wbEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// WorkBuddyError 带分类的上游错误。
type WorkBuddyError struct {
	Kind   WorkBuddyErrKind
	Status int
	Msg    string
}

func (e *WorkBuddyError) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// IsWorkBuddySessionDead 报告 err 是否为 session 失效（供调度器禁用账号）。
func IsWorkBuddySessionDead(err error) bool {
	var we *WorkBuddyError
	return errors.As(err, &we) && we.Kind == WBErrSessionDead
}

func truncateWB(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

// doJSON 发请求并解信封；HTTP 非 2xx 或业务 code != 0 返回带 body 片段的 *WorkBuddyError。
func (c *WorkBuddyClient) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := ClassifyWorkBuddyError(resp.StatusCode, string(raw))
		return nil, &WorkBuddyError{Kind: kind, Status: resp.StatusCode, Msg: truncateWB(string(raw), 300)}
	}
	var env wbEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncateWB(string(raw), 120))
	}
	if env.Code != 0 {
		kind := ClassifyWorkBuddyError(resp.StatusCode, env.Msg)
		if kind == WBErrNone {
			kind = WBErrClient
		}
		return nil, &WorkBuddyError{Kind: kind, Status: resp.StatusCode, Msg: fmt.Sprintf("code=%d msg=%s", env.Code, truncateWB(env.Msg, 200))}
	}
	return env.Data, nil
}

// WorkBuddyTokenBundle 刷新/兑换后的 token 结果（上游响应为驼峰形）。
type WorkBuddyTokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// RefreshToken 刷新 access token；成功时更新 a 字段（缺省保留旧值）。
func (c *WorkBuddyClient) RefreshToken(ctx context.Context, a *WorkBuddyAuth) error {
	if a == nil || strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase()+"/v2/plugin/auth/token/refresh", nil)
	if err != nil {
		return err
	}
	c.RefreshHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return err
	}
	var tok WorkBuddyTokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	a.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		a.Domain = tok.Domain
	}
	if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	return nil
}

// ChatStream 发 chat 请求并返回原始 SSE body 流（调用方负责 Close）。
// 非 2xx 时 rc 为 nil、respBody 为上游响应体（供调用方分类）、err 为 nil；
// 只有传输层失败才返回 err。
func (c *WorkBuddyClient) ChatStream(a *WorkBuddyAuth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.chatBase()+"/v2/chat/completions", bytes.NewReader(PrepareWorkBuddyChatPayload(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	c.ChatHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, nil
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ChatHeaders 设置 chat 端点专属头（与 CommonHeaders 一致，独立便于将来扩展设备风控头）。
func (c *WorkBuddyClient) ChatHeaders(req *http.Request, a *WorkBuddyAuth) {
	c.CommonHeaders(req, a)
}

// WorkBuddyModelInfo 动态模型信息（含 maxInputTokens/maxOutputTokens）。
type WorkBuddyModelInfo struct {
	ID            string
	Name          string
	ContextWindow int64 // = maxInputTokens
	MaxTokens     int64 // = maxOutputTokens
	Efforts       []string
}

// FetchModels 调上游动态模型接口。
func (c *WorkBuddyClient) FetchModels(ctx context.Context, a *WorkBuddyAuth) ([]WorkBuddyModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.chatBase()+"/console/enterprises/personal/models", nil)
	if err != nil {
		return nil, err
	}
	c.CommonHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models api status %d: %s", resp.StatusCode, truncateWB(string(raw), 120))
	}
	var env struct {
		Code int `json:"code"`
		Data struct {
			Models []struct {
				ID              string `json:"id"`
				Name            string `json:"name"`
				MaxInputTokens  int64  `json:"maxInputTokens"`
				MaxOutputTokens int64  `json:"maxOutputTokens"`
				Disabled        bool   `json:"disabled"`
				Reasoning       struct {
					Effort           string   `json:"effort"`
					SupportedEfforts []string `json:"supportedEfforts"`
				} `json:"reasoning"`
			} `json:"models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api code=%d", env.Code)
	}
	out := make([]WorkBuddyModelInfo, 0, len(env.Data.Models))
	for _, m := range env.Data.Models {
		if m.Disabled {
			continue
		}
		out = append(out, WorkBuddyModelInfo{
			ID:            m.ID,
			Name:          m.Name,
			ContextWindow: m.MaxInputTokens,
			MaxTokens:     m.MaxOutputTokens,
			Efforts:       m.Reasoning.SupportedEfforts,
		})
	}
	return out, nil
}