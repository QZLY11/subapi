// Package service — WorkBuddy CN OAuth 设备授权流。
//
// 对齐 workbuddy2api cmd/login/main.go：
//  1. auth-url  → POST https://copilot.tencent.com/v2/plugin/auth/state?platform=CLI
//     拿上游签发的 state + authUrl（无 PKCE，state 由服务端签发）。
//  2. poll      → GET  /v2/plugin/auth/token?state=<state>
//     完成时 code=0 + token bundle；pending 时业务 code 非 0。
//  3. account   → GET  /v2/plugin/login/account?state=<state> 拿 uid/nickname/enterpriseId。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// workbuddyOAuthState TTL（设备授权流 state 有效窗口）。
const workbuddyOAuthStateTTL = 5 * time.Minute

// wbOAuthSession 内存设备授权会话。
// Token 字段在首次 poll 成功后写入并保留至 TTL 过期：create-from-oauth 需要与
// poll 共用同一 state，若 poll 时即删除会话会令建号请求报 "state not found"。
type wbOAuthSession struct {
	State       string
	UpstreamURL string
	CreatedAt   time.Time
	Token       *WorkBuddyTokenInfo
}

// workbuddySessionStore 内存 session store（state 维度，单实例足够；多实例需换 Redis）。
type workbuddySessionStore struct {
	mu       sync.Mutex
	sessions map[string]*wbOAuthSession
}

func newWorkbuddySessionStore() *workbuddySessionStore {
	return &workbuddySessionStore{sessions: make(map[string]*wbOAuthSession)}
}

func (s *workbuddySessionStore) set(state string, ss *wbOAuthSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[state] = ss
}

func (s *workbuddySessionStore) get(state string) (*wbOAuthSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[state]
	if !ok {
		return nil, false
	}
	if time.Since(ss.CreatedAt) > workbuddyOAuthStateTTL {
		delete(s.sessions, state)
		return nil, false
	}
	return ss, true
}

func (s *workbuddySessionStore) delete(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, state)
}

// consumeToken 返回已解析的 token（poll 成功后缓存）。第二个返回值为 true 表示
// 命中缓存，调用方无需再打上游。
func (s *workbuddySessionStore) consumeToken(state string) (*WorkBuddyTokenInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[state]
	if !ok || ss.Token == nil {
		return nil, false
	}
	if time.Since(ss.CreatedAt) > workbuddyOAuthStateTTL {
		delete(s.sessions, state)
		return nil, false
	}
	return ss.Token, true
}

// storeToken 在 poll 成功后缓存 token（不删除会话，供 create-from-oauth 复用）。
func (s *workbuddySessionStore) storeToken(state string, info *WorkBuddyTokenInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.sessions[state]
	if !ok {
		return
	}
	ss.Token = info
	// 刷新计时起点：授权完成后按 TTL 保留，供建号读取。
	ss.CreatedAt = time.Now()
}

// WorkBuddyOAuthService 封装 WorkBuddy CN 设备授权流程与凭证构建。
type WorkBuddyOAuthService struct {
	client       *WorkBuddyClient
	sessionStore *workbuddySessionStore
}

func NewWorkBuddyOAuthService() *WorkBuddyOAuthService {
	return &WorkBuddyOAuthService{
		client:       NewWorkBuddyClient(),
		sessionStore: newWorkbuddySessionStore(),
	}
}

// WorkBuddyAuthURLResult 设备授权 URL 结果。
type WorkBuddyAuthURLResult struct {
	AuthURL string `json:"auth_url"`
	State   string `json:"state"`
}

// GenerateAuthURL 启动设备授权：请求上游签发 state + authUrl。
func (s *WorkBuddyOAuthService) GenerateAuthURL(ctx context.Context) (*WorkBuddyAuthURLResult, error) {
	if s == nil || s.client == nil {
		return nil, infraerrors.InternalServer("WORKBUDDY_OAUTH_CLIENT_NOT_CONFIGURED", "workbuddy oauth client is not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.client.chatBase()+"/v2/plugin/auth/state?platform=CLI", bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_OAUTH_STATE_FAILED", err.Error())
	}
	s.client.CommonHeaders(req, nil)
	data, err := s.client.doJSON(req)
	if err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_FAILED", err.Error())
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_OAUTH_STATE_FAILED", "missing state or authUrl")
	}
	s.sessionStore.set(st.State, &wbOAuthSession{
		State:       st.State,
		UpstreamURL: st.AuthURL,
		CreatedAt:   time.Now(),
	})
	return &WorkBuddyAuthURLResult{AuthURL: st.AuthURL, State: st.State}, nil
}

// WorkBuddyTokenInfo 兑换/刷新后的完整 token + 账号信息。
type WorkBuddyTokenInfo struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int64  `json:"expires_in"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain,omitempty"`
	UID          string `json:"uid,omitempty"`
	EnterpriseID string `json:"enterprise_id,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
}

// PollLogin 轮询设备授权结果。pending 时返回 (nil, workbuddyPendingError 语义)。
func (s *WorkBuddyOAuthService) PollLogin(ctx context.Context, state string) (*WorkBuddyTokenInfo, error) {
	state = strings.TrimSpace(state)
	if state == "" {
		return nil, infraerrors.BadRequest("WORKBUDDY_OAUTH_STATE_REQUIRED", "oauth state is required")
	}
	if _, ok := s.sessionStore.get(state); !ok {
		// 已有解析结果时不依赖 TTL 内的原始会话存在性（state 可能刚过期）。
		if _, cached := s.sessionStore.consumeToken(state); !cached {
			return nil, infraerrors.BadRequest("WORKBUDDY_OAUTH_STATE_NOT_FOUND", "state not found or expired")
		}
	}
	// poll 已成功过一次：直接返回缓存结果，避免重复消费上游 state（上游 state 一次性）。
	if cached, ok := s.sessionStore.consumeToken(state); ok {
		return cached, nil
	}

	// GET /v2/plugin/auth/token?state=
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.client.chatBase()+"/v2/plugin/auth/token?state="+state, nil)
	if err != nil {
		return nil, infraerrors.New(http.StatusInternalServerError, "WORKBUDDY_OAUTH_POLL_FAILED", err.Error())
	}
	s.client.CommonHeaders(req, nil)
	data, err := s.client.doJSON(req)
	if err != nil {
		// pending：上游返回业务 code 非 0（"login ing"）。判定为未完成而非错误。
		if we, ok := err.(*WorkBuddyError); ok && we.Status < 500 {
			return nil, infraerrors.New(http.StatusAccepted, "WORKBUDDY_OAUTH_PENDING", "login pending: "+we.Msg)
		}
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_OAUTH_POLL_FAILED", err.Error())
	}

	var tok WorkBuddyTokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return nil, infraerrors.New(http.StatusAccepted, "WORKBUDDY_OAUTH_PENDING", "login not complete")
	}

	info := &WorkBuddyTokenInfo{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresIn:    tok.ExpiresIn,
		Domain:       tok.Domain,
	}
	if tok.ExpiresIn > 0 {
		info.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}

	// GET /v2/plugin/login/account?state= 拿 uid/nickname/enterpriseId（带 Bearer）
	acctReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.client.chatBase()+"/v2/plugin/login/account?state="+state, nil)
	if err == nil {
		auth := &WorkBuddyAuth{AccessToken: tok.AccessToken}
		s.client.CommonHeaders(acctReq, auth)
		if acctData, aErr := s.client.doJSON(acctReq); aErr == nil {
			var acct struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			}
			if json.Unmarshal(acctData, &acct) == nil {
				info.UID = acct.UID
				info.EnterpriseID = acct.EnterpriseID
				info.Nickname = acct.Nickname
			}
		}
	}

	// 缓存解析结果而非删除会话：create-from-oauth 需用同一 state 复用该结果。
	s.sessionStore.storeToken(state, info)
	return info, nil
}

// RefreshToken 用 refresh_token 换新 token（供账号凭证刷新）。
func (s *WorkBuddyOAuthService) RefreshToken(ctx context.Context, a *WorkBuddyAuth) (*WorkBuddyTokenInfo, error) {
	if err := s.client.RefreshToken(ctx, a); err != nil {
		return nil, infraerrors.New(http.StatusBadGateway, "WORKBUDDY_OAUTH_REFRESH_FAILED", err.Error())
	}
	// refresh 端点只回 accessToken/refreshToken/expiresIn/domain，不回 uid/enterprise_id。
	// 必须把调用方传入的身份字段原样带回，否则 BuildAccountCredentials 会用空字符串
	// 覆盖账号已有的 uid/enterprise_id，导致 chat 转发缺失 X-User-Id/X-Enterprise-Id
	// 而被上游判为 invalid issuer。
	return &WorkBuddyTokenInfo{
		AccessToken:  a.AccessToken,
		RefreshToken: a.RefreshToken,
		Domain:       a.Domain,
		ExpiresAt:    a.ExpiresAt,
		UID:          a.UID,
		EnterpriseID: a.EnterpriseID,
	}, nil
}

// BuildAccountCredentials 把 token info 构建为 sub2api 账号 credentials JSONB。
// 只写非空字段：uid/enterprise_id/domain 为空时省略，避免 refresh 路径用空值
// 覆盖账号已有的身份字段（chat 上游依赖 X-User-Id/X-Enterprise-Id）。
func (s *WorkBuddyOAuthService) BuildAccountCredentials(info *WorkBuddyTokenInfo) map[string]any {
	if info == nil {
		return map[string]any{}
	}
	creds := map[string]any{
		"access_token": info.AccessToken,
		"expires_at":   info.ExpiresAt,
	}
	if strings.TrimSpace(info.RefreshToken) != "" {
		creds["refresh_token"] = info.RefreshToken
	}
	if strings.TrimSpace(info.Domain) != "" {
		creds["domain"] = info.Domain
	}
	if strings.TrimSpace(info.UID) != "" {
		creds["uid"] = info.UID
	}
	if strings.TrimSpace(info.EnterpriseID) != "" {
		creds["enterprise_id"] = info.EnterpriseID
	}
	return creds
}