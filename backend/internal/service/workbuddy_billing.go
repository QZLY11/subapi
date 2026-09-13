// Package service — WorkBuddy CN 积分（credits）查询、每日签到与活跃上报。
//
// 本文件把 workbuddy2api 的 billing 域能力（cmd/credit + cmd/signin + upstream/report.go）
// 内嵌为 sub2api 的 service，与 workbuddy_client.go（chat/refresh/models）并列。
//
// 上游端点（billing 域走 codebuddy.cn，而非 copilot.tencent.com）：
//   - 积分:   POST https://www.codebuddy.cn/v2/billing/meter/get-user-resource
//   - 签到:   POST https://www.codebuddy.cn/v2/billing/meter/daily-checkin
//   - 活跃上报: POST https://www.codebuddy.cn/v2/report
//
// 积分聚合口径与 workbuddy2api cmd/credit 的 packageRemainUsed 一致：
// 优先 CyclicCapacity*（周期包），退化到 Capacity*（总量包），负值钳 0。
package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// wbBillingBaseCN billing 域基址（与 chat 域的 copilot.tencent.com 不同）。
	wbBillingBaseCN = "https://www.codebuddy.cn"

	wbBillingMeterPath   = "/v2/billing/meter/get-user-resource"
	wbDailyCheckinPath   = "/v2/billing/meter/daily-checkin"
	wbReportActivityPath = "/v2/report"

	// wbCreditsProductCode CodeBuddy 个人版产品码（积分包查询固定值）。
	wbCreditsProductCode = "p_tcaca"
)

// alreadyCheckinMarkers 上游「今日已签到」文案标记（等价拒绝，视为正常）。
// 对齐 workbuddy2api client.go IsAlreadyCheckin。
var alreadyCheckinMarkers = []string{
	"已签到", "already", "checkin", "code=400",
}

// WorkBuddyCreditPackage 单个积分包（一个账号可能同时持有多个）。
type WorkBuddyCreditPackage struct {
	PackageName string `json:"package_name"`
	Remain      int64  `json:"remain"`
	Used        int64  `json:"used"`
	Size        int64  `json:"size"`
}

// WorkBuddyCredits 单账号积分汇总。
type WorkBuddyCredits struct {
	UID      string                    `json:"uid"`
	Nickname string                    `json:"nickname"`
	Remain   int64                     `json:"remain"`
	Used     int64                     `json:"used"`
	Size     int64                     `json:"size"`
	Packages []WorkBuddyCreditPackage  `json:"packages,omitempty"`

	// TotalDosage 上游给出的大小下限（用于修正 size 小于实际已用量的情况）。
	TotalDosage int64 `json:"total_dosage,omitempty"`
}

// WorkBuddyCheckinResult 单账号签到结果。
type WorkBuddyCheckinResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	// Status: ok（签到成功）/ already（今日已签）/ fail（失败）/ skipped（无凭据）
	Status  string `json:"status"`
	Detail  string `json:"detail,omitempty"`
	Credits *WorkBuddyCredits `json:"credits,omitempty"`
}

// workBuddyResourcePackage 上游积分包原始结构（PascalCase，腾讯云 meter 口径）。
type workBuddyResourcePackage struct {
	PackageName         string `json:"PackageName"`
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// billingBase 返回 billing 域基址（测试可覆盖）。
func (c *WorkBuddyClient) billingBaseURL() string {
	if c != nil && strings.TrimSpace(c.BillingBase) != "" {
		return strings.TrimRight(c.BillingBase, "/")
	}
	return wbBillingBaseCN
}

// billingHeaders 设置 billing 域请求头。
// 与 workbuddy2api BillingHeaders 对齐：只带 Authorization/Accept/Content-Type/UA
// 与身份头，不带 Origin/Referer（billing 域不校验来源白名单）。
func (c *WorkBuddyClient) billingHeaders(req *http.Request, a *WorkBuddyAuth) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
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

// billingJSON 向 billing 域发请求并解 {code,msg,data} 信封。
// body 为 nil 时不带请求体。错误语义与 doJSON 一致。
func (c *WorkBuddyClient) billingJSON(ctx context.Context, a *WorkBuddyAuth, method, path string, body any) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.billingBaseURL()+path, rdr)
	if err != nil {
		return nil, err
	}
	c.billingHeaders(req, a)
	return c.doJSON(req)
}

// packageRemainUsed 计算单个积分包的 remain/used/size。
// 优先周期口径（CycleCapacity*），无周期包时退回总量口径；remain 钳在 [0,size]。
// 与 workbuddy2api cmd/credit packageRemainUsed 逐行对齐。
func packageRemainUsed(p workBuddyResourcePackage) (remain, used, size int64) {
	if p.CycleCapacitySize > 0 {
		remain = p.CycleCapacityRemain
		size = p.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if p.CycleCapacityUsed > used {
			used = p.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = p.CapacityRemain
	used = p.CapacityUsed
	size = p.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// FetchCredits 查询单个账号的积分余额（聚合所有积分包）。
func (c *WorkBuddyClient) FetchCredits(ctx context.Context, a *WorkBuddyAuth) (*WorkBuddyCredits, error) {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("no accessToken")
	}
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              wbCreditsProductCode,
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	data, err := c.billingJSON(ctx, a, http.MethodPost, wbBillingMeterPath, body)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Response struct {
			Data struct {
				TotalDosage int64                      `json:"TotalDosage"`
				Accounts    []workBuddyResourcePackage `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("resource parse: %w", err)
	}

	out := &WorkBuddyCredits{
		UID:         a.UID,
		TotalDosage: resp.Response.Data.TotalDosage,
	}
	for _, p := range resp.Response.Data.Accounts {
		r, u, s := packageRemainUsed(p)
		out.Remain += r
		out.Used += u
		out.Size += s
		out.Packages = append(out.Packages, WorkBuddyCreditPackage{
			PackageName: p.PackageName,
			Remain:      r,
			Used:        u,
			Size:        s,
		})
	}
	// TotalDosage 是上游给出的大小下限：实际包总和偏小时以它为准。
	if out.TotalDosage > out.Size {
		out.Size = out.TotalDosage
	}
	if derived := out.Size - out.Remain; derived > out.Used {
		out.Used = derived
	}
	return out, nil
}

// DailyCheckin 执行每日签到。今日已签到时上游返回业务 code != 0，
// 调用方用 IsWorkBuddyAlreadyCheckin 区分（视作正常，不算失败）。
func (c *WorkBuddyClient) DailyCheckin(ctx context.Context, a *WorkBuddyAuth) error {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("no accessToken")
	}
	_, err := c.billingJSON(ctx, a, http.MethodPost, wbDailyCheckinPath, map[string]any{})
	return err
}

// IsWorkBuddyAlreadyCheckin 报告 err 是否表示「今日已签到」（上游幂等拒绝）。
// 只认带分类的 *WorkBuddyError：网络层/解析层错误不得当作"已签到"，
// 否则抖动会被误判为已完成。
func IsWorkBuddyAlreadyCheckin(err error) bool {
	if err == nil {
		return false
	}
	we, ok := err.(*WorkBuddyError)
	if !ok {
		return false
	}
	msg := strings.ToLower(we.Msg)
	for _, m := range alreadyCheckinMarkers {
		if strings.Contains(msg, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// ReportChatActivity 向上游发送一条对话活跃上报（chat_request_send）。
// 一条上报同时点亮 growth 连续登录并解锁 first_buddy 任务（领养前置）。
// 风控口径：每号每天 1 次即可，不做多时点高频上报。
func (c *WorkBuddyClient) ReportChatActivity(ctx context.Context, a *WorkBuddyAuth) error {
	if a == nil || strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("no accessToken")
	}
	now := time.Now().UnixMilli()
	// 会话 ID 无需真实存在——服务端不校验一致性，但 userId 必须为账号 uid，
	// 缺失则上游 200 静默丢弃。
	convID := fmt.Sprintf("sub2api-%d", now)
	ev := map[string]any{
		"eventCode":             "chat_request_send",
		"timestamp":             now,
		"reportDelay":           0,
		"mode":                  "craft",
		"conversationId":        convID,
		"requestId":             convID,
		"inputLength":           12,
		"requestModelId":        "deepseek-v4-flash",
		"requestModelName":      "DeepSeek V4 Flash",
		"isPlan":                false,
		"isAutoExecuteTerminal": false,
		"isAutoModify":          false,
		"codebaseEnable":        false,
		"maxToken":              0,
		"maxSteps":              0,
		"temperature":           0,
		"maxRetries":            0,
		"mentionContexts":       []any{},
		"knowledgeId":           []any{},
		"knowledgeName":         []any{},
		"codebaseId":            "",
		"mentionContextCount":   0,
		"command":               "",
		"expertId":              "",
		"recommendId":           "",
		"skillId":               "",
		"skillCount":            0,
		"totalCount":            0,
		"fileUri":               "",
		"presentAt":             now,
		"traceId":               "",
		"rootRequestId":         convID,
		"parentConversationId":  convID,
		"agentName":             "default",
		"agentType":             "conversation",
		"userId":                a.UID,
	}
	payload, err := json.Marshal([]map[string]any{ev})
	if err != nil {
		return err
	}
	_, err = c.billingJSON(ctx, a, http.MethodPost, wbReportActivityPath, json.RawMessage(payload))
	return err
}
