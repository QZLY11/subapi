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
}

func NewWorkBuddyCreditsHandler(creditsService *service.WorkBuddyCreditsService) *WorkBuddyCreditsHandler {
	return &WorkBuddyCreditsHandler{creditsService: creditsService}
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

// QueryCreditsSummary 查询全部 WorkBuddy 账号积分并汇总总额。
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

	// 签到会写 extra 快照，故随后再取一次汇总，让前端一次调用即可刷新
	// 账号总数 / 总计分额度 / 剩余总积分 / 今日已签到数。
	credits, cErr := h.creditsService.QueryAllCredits(ctx)
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
