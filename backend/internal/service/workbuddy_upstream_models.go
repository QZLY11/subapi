package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// wbModelsPath 是 WorkBuddy CN 的动态模型列表端点（不在 /v2 下，故独立于 chat base）。
const wbModelsPath = "/console/enterprises/personal/models"

// buildWorkBuddyUpstreamModelsRequest 构造 WorkBuddy 动态模型列表请求。
// 端点 https://copilot.tencent.com/console/enterprises/personal/models，
// 使用账号 access_token 作 Bearer 并注入 WorkBuddy 身份头。
func (s *AccountTestService) buildWorkBuddyUpstreamModelsRequest(ctx context.Context, account *Account) (*http.Request, error) {
	if account == nil {
		return nil, newUpstreamModelSyncConfigError("Account is required", nil)
	}
	if !account.IsWorkBuddy() {
		return nil, newUpstreamModelSyncUnsupportedError(
			fmt.Sprintf("Unsupported WorkBuddy account: %s", account.Platform), nil,
		)
	}
	accessToken := strings.TrimSpace(account.GetCredential("access_token"))
	if accessToken == "" {
		return nil, newUpstreamModelSyncConfigError("No WorkBuddy access token is available", nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wbUpstreamBaseCN+wbModelsPath, nil)
	if err != nil {
		return nil, newUpstreamModelSyncConfigError("Invalid WorkBuddy model list URL", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	applyWorkBuddyUpstreamHeaders(req.Header, account)
	// 账号级请求头覆写：模型列表探测与真实转发保持一致的最终头。
	account.ApplyHeaderOverrides(req.Header)
	return req, nil
}

// extractWorkBuddyUpstreamModelIDs 解析 WorkBuddy 模型列表响应。
// 形如 {"code":0,"data":{"models":[{"id":"...","disabled":false}]}}；
// 兼容 data 直接为数组的简化形态。
func extractWorkBuddyUpstreamModelIDs(body []byte) ([]string, error) {
	var envelope struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("workbuddy models: invalid JSON: %w", err)
	}

	entries := make([]json.RawMessage, 0)
	// 形态一：data.models[]（官方）
	var nested struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(envelope.Data, &nested); err == nil && len(nested.Models) > 0 {
		entries = nested.Models
	} else {
		// 形态二：data[]（简化/兼容）
		var flat []json.RawMessage
		if err := json.Unmarshal(envelope.Data, &flat); err == nil {
			entries = flat
		}
	}
	if len(entries) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}

	models := make([]string, 0, len(entries))
	efforts := make(map[string][]string, len(entries))
	for _, raw := range entries {
		var entry struct {
			ID       string `json:"id"`
			Model    string `json:"model"`
			Disabled bool   `json:"disabled"`
			Reasoning struct {
				SupportedEfforts []string `json:"supportedEfforts"`
			} `json:"reasoning"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.Disabled {
			continue
		}
		id := strings.TrimSpace(entry.ID)
		if id == "" {
			id = strings.TrimSpace(entry.Model)
		}
		if id == "" {
			continue
		}
		models = append(models, id)
		if len(entry.Reasoning.SupportedEfforts) > 0 {
			efforts[id] = entry.Reasoning.SupportedEfforts
		}
	}
	if len(models) == 0 {
		return nil, newUpstreamModelSyncUpstreamError("Upstream returned no supported models", nil)
	}
	// 同步返回的 supportedEfforts 落到进程内缓存，供请求改写做 reasoning_effort 降级。
	cacheWorkBuddyModelEfforts(efforts)
	return dedupeAndSortModelIDs(models), nil
}
