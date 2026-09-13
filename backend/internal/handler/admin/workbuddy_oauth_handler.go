package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// WorkBuddyOAuthHandler 处理 WorkBuddy CN（CodeBuddy）设备授权流。
// 路由（对齐 Grok/Gemini OAuth 三件套，但 WorkBuddy 是 poll 型设备流）：
//   POST /api/v1/admin/workbuddy/oauth/auth-url      → 生成授权链接 + state
//   POST /api/v1/admin/workbuddy/oauth/poll          → 轮询登录结果（拿 token）
//   POST /api/v1/admin/workbuddy/oauth/refresh-token → 刷新账号 token
//   POST /api/v1/admin/workbuddy/oauth/create-from-oauth → 建账号
type WorkBuddyOAuthHandler struct {
	workBuddyOAuthService *service.WorkBuddyOAuthService
	adminService          service.AdminService
}

func NewWorkBuddyOAuthHandler(
	workBuddyOAuthService *service.WorkBuddyOAuthService,
	adminService service.AdminService,
) *WorkBuddyOAuthHandler {
	return &WorkBuddyOAuthHandler{
		workBuddyOAuthService: workBuddyOAuthService,
		adminService:          adminService,
	}
}

// GenerateAuthURL 启动 WorkBuddy 设备授权流，返回授权链接 + state。
// POST /api/v1/admin/workbuddy/oauth/auth-url
func (h *WorkBuddyOAuthHandler) GenerateAuthURL(c *gin.Context) {
	result, err := h.workBuddyOAuthService.GenerateAuthURL(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

type WorkBuddyPollRequest struct {
	State string `json:"state" binding:"required"`
}

// PollLogin 轮询设备授权结果。pending 时返回 WORKBUDDY_OAUTH_PENDING（前端继续轮询）。
// POST /api/v1/admin/workbuddy/oauth/poll
func (h *WorkBuddyOAuthHandler) PollLogin(c *gin.Context) {
	var req WorkBuddyPollRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.workBuddyOAuthService.PollLogin(c.Request.Context(), req.State)
	if err != nil {
		if ae := infraerrors.FromError(err); ae != nil && ae.Reason == "WORKBUDDY_OAUTH_PENDING" {
			// pending 是正常中间态：前端据此继续轮询。
			response.Success(c, gin.H{"pending": true, "message": ae.Message})
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

type WorkBuddyRefreshTokenRequest struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token" binding:"required"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterprise_id"`
	DeviceToken  string `json:"device_token"`
}

// RefreshToken 用 refresh_token 换新 token。
// POST /api/v1/admin/workbuddy/oauth/refresh-token
func (h *WorkBuddyOAuthHandler) RefreshToken(c *gin.Context) {
	var req WorkBuddyRefreshTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	auth := &service.WorkBuddyAuth{
		AccessToken:  req.AccessToken,
		RefreshToken: req.RefreshToken,
		ExpiresAt:    req.ExpiresAt,
		Domain:       req.Domain,
		UID:          req.UID,
		EnterpriseID: req.EnterpriseID,
		DeviceToken:  req.DeviceToken,
	}
	tokenInfo, err := h.workBuddyOAuthService.RefreshToken(c.Request.Context(), auth)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, tokenInfo)
}

// RefreshAccountToken 刷新指定账号的凭证并写回。
// POST /api/v1/admin/workbuddy/accounts/:id/refresh-token
func (h *WorkBuddyOAuthHandler) RefreshAccountToken(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	account, err := h.adminService.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if account == nil {
		response.BadRequest(c, "Account not found")
		return
	}
	if !account.IsWorkBuddyOAuth() {
		response.BadRequest(c, "Account is not a WorkBuddy OAuth account")
		return
	}
	auth := service.WorkBuddyAuthFromAccount(account)
	tokenInfo, err := h.workBuddyOAuthService.RefreshToken(c.Request.Context(), auth)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	newCredentials := h.workBuddyOAuthService.BuildAccountCredentials(tokenInfo)
	newCredentials = service.MergeCredentials(account.Credentials, newCredentials)
	updated, err := h.adminService.UpdateAccount(c.Request.Context(), accountID, &service.UpdateAccountInput{
		Credentials: newCredentials,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(updated))
}

type WorkBuddyCreateFromOAuthRequest struct {
	State       string  `json:"state" binding:"required"`
	Name        string  `json:"name"`
	Concurrency int     `json:"concurrency"`
	Priority    int     `json:"priority"`
	GroupIDs    []int64 `json:"group_ids"`
}

// CreateAccountFromOAuth 用 poll 得到的 token 直接创建 WorkBuddy 账号。
// POST /api/v1/admin/workbuddy/oauth/create-from-oauth
func (h *WorkBuddyOAuthHandler) CreateAccountFromOAuth(c *gin.Context) {
	var req WorkBuddyCreateFromOAuthRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	tokenInfo, err := h.workBuddyOAuthService.PollLogin(c.Request.Context(), req.State)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	credentials := h.workBuddyOAuthService.BuildAccountCredentials(tokenInfo)

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = strings.TrimSpace(tokenInfo.Nickname)
	}
	if name == "" {
		name = strings.TrimSpace(tokenInfo.UID)
	}
	if name == "" {
		name = "WorkBuddy Account"
	}

	// 有 refresh_token 时不写 ExpiresAt/AutoPauseOnExpired：
	// access token 仅约 1 小时有效，若按它自动停调会让可刷新账号被误停。
	// 与 Grok grokSSOImportExpiry 的口径一致。
	var expiresAt *int64
	autoPause := false
	if strings.TrimSpace(tokenInfo.RefreshToken) == "" && tokenInfo.ExpiresAt > 0 {
		expiresAt = &tokenInfo.ExpiresAt
		autoPause = true
	}
	account, err := h.adminService.CreateAccount(c.Request.Context(), &service.CreateAccountInput{
		Name:               name,
		Platform:           service.PlatformWorkBuddy,
		Type:               service.AccountTypeWorkBuddyOAuth,
		Credentials:        credentials,
		Concurrency:        req.Concurrency,
		Priority:           req.Priority,
		GroupIDs:           req.GroupIDs,
		ExpiresAt:          expiresAt,
		AutoPauseOnExpired: &autoPause,
	})
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, dto.AccountFromService(account))
}