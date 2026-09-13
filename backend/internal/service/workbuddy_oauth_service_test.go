package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWorkBuddyOAuthService_RefreshToken_PreservesIdentity 验证 refresh 端点
// 不回 uid/enterprise_id 时，RefreshToken 仍会原样带回调用方传入的身份字段，
// 避免 BuildAccountCredentials 用空字符串覆盖账号已有的 X-User-Id/X-Enterprise-Id。
func TestWorkBuddyOAuthService_RefreshToken_PreservesIdentity(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/plugin/auth/token/refresh", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"msg":  "ok",
			"data": map[string]any{
				"accessToken":  "new-access-token",
				"refreshToken": "new-refresh-token",
				"expiresIn":    3600,
				"domain":       "www.codebuddy.cn",
			},
		})
	}))
	defer srv.Close()

	svc := &WorkBuddyOAuthService{
		client:       &WorkBuddyClient{HTTP: srv.Client(), ChatBase: srv.URL},
		sessionStore: newWorkbuddySessionStore(),
	}

	auth := &WorkBuddyAuth{
		AccessToken:  "old-access-token",
		RefreshToken: "old-refresh-token",
		Domain:       "www.codebuddy.cn",
		UID:          "e68dba5d-32f0-460c-b380-5890e461a895",
		EnterpriseID: "ent-123",
	}

	info, err := svc.RefreshToken(context.Background(), auth)
	require.NoError(t, err)
	require.Equal(t, "new-access-token", info.AccessToken)
	require.Equal(t, "new-refresh-token", info.RefreshToken)
	require.Equal(t, "e68dba5d-32f0-460c-b380-5890e461a895", info.UID)
	require.Equal(t, "ent-123", info.EnterpriseID)

	creds := svc.BuildAccountCredentials(info)
	require.Equal(t, "new-access-token", creds["access_token"])
	require.Equal(t, "e68dba5d-32f0-460c-b380-5890e461a895", creds["uid"])
	require.Equal(t, "ent-123", creds["enterprise_id"])
}

// TestApplyWorkBuddyUpstreamHeaders_DoesNotInheritClientUA 验证 applyWorkBuddyUpstreamHeaders
// 不会把管线透传的客户端 UA（如 curl/x.x）当作最终 UA，否则上游按 UA 指纹校验
// issuer 会报 401 "not from a valid issuer"。
func TestApplyWorkBuddyUpstreamHeaders_DoesNotInheritClientUA(t *testing.T) {
	acct := &Account{
		Platform:    PlatformWorkBuddy,
		Type:        AccountTypeWorkBuddyOAuth,
		Credentials: map[string]any{"uid": "uid-123", "domain": "www.codebuddy.cn"},
	}

	h := http.Header{}
	// 模拟 sendCCUpstreamRequest 先透传客户端 UA。
	h.Set("User-Agent", "curl/8.5.0")

	applyWorkBuddyUpstreamHeaders(h, acct)

	require.Equal(t, (&WorkBuddyClient{}).userAgent(), h.Get("User-Agent"))
	require.Equal(t, "uid-123", h.Get("X-User-Id"))
	require.Equal(t, "www.codebuddy.cn", h.Get("X-Domain"))
	require.Equal(t, wbOriginReferer, h.Get("Origin"))
}

// TestApplyWorkBuddyUpstreamHeaders_AccountUAOverridesDefault 验证账号级 user_agent
// credential 优先于官方默认 UA。
func TestApplyWorkBuddyUpstreamHeaders_AccountUAOverridesDefault(t *testing.T) {
	acct := &Account{
		Platform:    PlatformWorkBuddy,
		Type:        AccountTypeWorkBuddyOAuth,
		Credentials: map[string]any{"user_agent": "CustomUA/1.0"},
	}

	h := http.Header{}
	applyWorkBuddyUpstreamHeaders(h, acct)

	require.Equal(t, "CustomUA/1.0", h.Get("User-Agent"))
}

// TestWorkBuddyOAuthService_BuildAccountCredentials_OmitsEmptyIdentity 验证
// uid/enterprise_id/domain 为空时被省略，避免 MergeCredentials 用空值覆盖旧值。
func TestWorkBuddyOAuthService_BuildAccountCredentials_OmitsEmptyIdentity(t *testing.T) {
	svc := &WorkBuddyOAuthService{}
	creds := svc.BuildAccountCredentials(&WorkBuddyTokenInfo{
		AccessToken: "at",
		ExpiresAt:   123,
	})
	require.Equal(t, "at", creds["access_token"])
	require.NotContains(t, creds, "uid")
	require.NotContains(t, creds, "enterprise_id")
	require.NotContains(t, creds, "domain")
	require.NotContains(t, creds, "refresh_token")
}
