package admin

import (
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// WorkBuddyCreditsHandler 处理 WorkBuddy 积分查询与签到。
//
// 路由：
//   GET  /api/v1/admin/workbuddy/accounts/:id/credits   → 单账号积分
//   POST /api/v1/admin/workbuddy/accounts/:id/checkin   → 单账号签到
//   GET  /api/v1/admin/workbuddy/credits/summary        → 全量积分汇总
//   POST /api/v1/admin/workbuddy/checkin/all            → 全量一键签到
type WorkBuddyCreditsHandler struct {
	creditsService *service.WorkBuddyCreditsService
	settingService *service.SettingService
}

func NewWorkBuddyCreditsHandler(
	creditsService *service.WorkBuddyCreditsService,
	settingService *service.SettingService,
) *WorkBuddyCreditsHandler {
	return &WorkBuddyCreditsHandler{
		creditsService: creditsService,
		settingService: settingService,
	}
}

// QueryCredits 查询单账号积分（并落 extra 快照）。
// GET /api/v1/admin/workbuddy/accounts/:id/credits
func (h *WorkBuddyCreditsHandler) QueryCredits(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	result, err := h.creditsService.QueryCredits(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// Checkin 对单账号执行签到（返回签到后积分）。
// POST /api/v1/admin/workbuddy/accounts/:id/checkin
func (h *WorkBuddyCreditsHandler) Checkin(c *gin.Context) {
	accountID, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid account ID")
		return
	}
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	result, err := h.creditsService.CheckinAccount(c.Request.Context(), accountID)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, result)
}

// QueryCreditsSummary 实时查询全部 WorkBuddy 账号积分并汇总总额。
//
// 注意：这是【实时全量】查询，会逐个打上游 meter 接口，账号多时需数十秒。
// 页面加载不要用它——用 QueryCreditsSnapshot 读快照；本接口只应由
// 管理端显式动作（"刷新积分"按钮）触发。
// GET /api/v1/admin/workbuddy/credits/summary
func (h *WorkBuddyCreditsHandler) QueryCreditsSummary(c *gin.Context) {
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	summary, err := h.creditsService.QueryAllCredits(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, summary)
}

// QueryCreditsSnapshot 读取积分快照（不打上游，毫秒级返回）。
//
// 数据由后台错开刷新服务维护（默认每账号约 10 分钟一次），可能略旧但绝不阻塞
// 页面。响应中的 fetched_at 是该批快照里最新的一次更新时间，供 UI 显示"数据截至"。
// GET /api/v1/admin/workbuddy/credits/snapshot
func (h *WorkBuddyCreditsHandler) QueryCreditsSnapshot(c *gin.Context) {
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	summary, err := h.creditsService.QueryCreditsSnapshot(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, summary)
}

// GetCreditsSettings 读取积分后台刷新设置：自动查询开关 / 单账号周期 / 单轮上限。
// GET /api/v1/admin/workbuddy/credits/settings
func (h *WorkBuddyCreditsHandler) GetCreditsSettings(c *gin.Context) {
	if h == nil || h.settingService == nil {
		response.BadRequest(c, "workbuddy credits settings service is not enabled")
		return
	}
	settings, err := h.settingService.GetWorkBuddyCreditsSettings(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// UpdateCreditsSettings 更新积分后台刷新设置。
//
// 部分更新：只传要改的字段即可（例如前端只翻开关、不动周期）。
// 改动写入 settings 表，后台刷新服务下一轮立即生效，无需重启。
// PUT /api/v1/admin/workbuddy/credits/settings
func (h *WorkBuddyCreditsHandler) UpdateCreditsSettings(c *gin.Context) {
	if h == nil || h.settingService == nil {
		response.BadRequest(c, "workbuddy credits settings service is not enabled")
		return
	}
	var req struct {
		BackgroundRefreshEnabled *bool `json:"background_refresh_enabled"`
		RefreshIntervalMinutes   *int  `json:"refresh_interval_minutes"`
		MaxPerTick               *int  `json:"max_per_tick"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}
	ctx := c.Request.Context()
	settings, err := h.settingService.GetWorkBuddyCreditsSettings(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if req.BackgroundRefreshEnabled != nil {
		settings.BackgroundRefreshEnabled = *req.BackgroundRefreshEnabled
	}
	if req.RefreshIntervalMinutes != nil {
		settings.RefreshIntervalMinutes = *req.RefreshIntervalMinutes
	}
	if req.MaxPerTick != nil {
		settings.MaxPerTick = *req.MaxPerTick
	}
	if err := h.settingService.SetWorkBuddyCreditsSettings(ctx, settings); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, settings)
}

// CheckinAll 对全部 WorkBuddy 账号一键签到，并返回签到后的汇总统计。
// POST /api/v1/admin/workbuddy/checkin/all
func (h *WorkBuddyCreditsHandler) CheckinAll(c *gin.Context) {
	if h == nil || h.creditsService == nil {
		response.BadRequest(c, "workbuddy credits service is not enabled")
		return
	}
	ctx := c.Request.Context()
	results, err := h.creditsService.CheckinAllAccounts(ctx)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	summary := summarizeCheckinResults(results)

	// 签到本身已把每个账号的积分写回 extra 快照，这里直接读快照即可
	// （不再触发一次全量上游查询，避免签到接口再叠加数十秒耗时）。
	credits, cErr := h.creditsService.QueryCreditsSnapshot(ctx)
	if cErr == nil {
		summary["credits"] = credits
	} else {
		summary["credits_error"] = cErr.Error()
	}
	response.Success(c, summary)
}

// summarizeCheckinResults 汇总签到结果计数。
func summarizeCheckinResults(results []*service.WorkBuddyCheckinResult) gin.H {
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
	return gin.H{
		"results": results,
		"total":   len(results),
		"ok":      ok,
		"already": already,
		"fail":    fail,
		"skipped": skipped,
		"summary": service.WorkBuddyCheckinSummaryText(results),
	}
}
