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
