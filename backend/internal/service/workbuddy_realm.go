// Package service — WorkBuddy 双区域（国内版 CodeBuddy / 国际版 WorkBuddy）出站参数。
//
// 背景：腾讯把同一套服务拆成两个区域端点，账号、协议、端点路径一致，但
// 「往哪发」和「以什么身份发」不同。硬编码单一区域会让另一个区域完全不可用
// （表现为 401 not from a valid issuer 或域名解析失败）。
//
// 取值来源（对齐 workbuddy2api-hub v1.7.19 的 REALM_CONFIGS 与 wb_identity）：
//
//	                国内版 CN                        国际版 INTL
//	chat 上游        https://copilot.tencent.com      https://www.workbuddy.ai
//	billing 上游     https://www.codebuddy.cn         https://www.workbuddy.ai
//	Origin/Referer   https://www.codebuddy.cn         https://www.workbuddy.ai
//	X-Domain 兜底     copilot.tencent.com              www.workbuddy.ai
//	chat UA          WorkBuddy/{ver} WorkBuddy/{ver} CLI/{cli}
//	                 WorkBuddy/{ver} workbuddy-ai/{ver} CLI/{cli}   ← 第二段不同
//	Accept-Language  zh-CN                            en-US
//	签到             支持（daily-checkin）             不支持（只有网页活跃打卡）
//
// 注意：国际版官方 product.json 写的是 www.codebuddy.ai，但该域不可解析；
// hub 实测走 www.workbuddy.ai 可用，此处沿用（已知 endpoint 差异）。
package service

import "strings"

// wbRealmName 区域标识。
const (
	wbRealmCN   = "cn"
	wbRealmIntl = "intl"
)

// wbRealm 一个区域的完整出站参数集合。
type wbRealm struct {
	// ChatBase chat 协议上游基址（/v2/chat/completions 挂在其后）。
	ChatBase string
	// BillingBase billing 域基址（积分/签到/活跃上报）。
	BillingBase string
	// Origin Origin 与 Referer 的基址（上游校验来源白名单）。
	Origin string
	// Domain X-Domain 的兜底值（账号自身 domain 优先）。
	Domain string
	// ChatUA 对话请求 User-Agent（三段式，第二段区分区域）。
	ChatUA string
	// BillingUA billing 请求 User-Agent（两段式）。
	BillingUA string
	// AcceptLanguage 出站 Accept-Language。
	AcceptLanguage string
	// HasCheckin 该区域是否有可调用的签到接口。
	HasCheckin bool
	// ClientVersion 桌面端版本号（用于其他头部的版本字段）。
	ClientVersion string
}

// wbRealms 区域参数表。新增区域只需在此登记。
var wbRealms = map[string]wbRealm{
	wbRealmCN: {
		ChatBase:       "https://copilot.tencent.com",
		BillingBase:    "https://www.codebuddy.cn",
		Origin:         "https://www.codebuddy.cn",
		Domain:         "copilot.tencent.com",
		ChatUA:         "WorkBuddy/5.8.3 WorkBuddy/5.8.3 CLI/2.161.0",
		BillingUA:      "WorkBuddy/5.8.3",
		AcceptLanguage: "zh-CN",
		HasCheckin:     true,
		ClientVersion:  "5.8.3",
	},
	wbRealmIntl: {
		ChatBase:       "https://www.workbuddy.ai",
		BillingBase:    "https://www.workbuddy.ai",
		Origin:         "https://www.workbuddy.ai",
		Domain:         "www.workbuddy.ai",
		// 国际版第二段是 workbuddy-ai（不是 WorkBuddy），写错会被判非官方客户端。
		ChatUA:         "WorkBuddy/5.8.4 workbuddy-ai/5.8.4 CLI/2.161.0",
		BillingUA:      "WorkBuddy/5.8.4",
		AcceptLanguage: "en-US",
		HasCheckin:     false,
		ClientVersion:  "5.8.4",
	},
}

// wbRealmFor 判定区域。
//
// 优先级：
//  1. 账号 platform 明确为国际版 → intl
//  2. 账号 credentials.domain 含 workbuddy.ai / codebuddy.ai → intl
//  3. 其余（含 domain 为空）→ cn（与既有部署形态一致，保证国内版零回归）
func wbRealmFor(platform string, a *Account) string {
	if platform == PlatformWorkBuddyIntl {
		return wbRealmIntl
	}
	if a != nil {
		d := strings.ToLower(strings.TrimSpace(a.GetCredential("domain")))
		if strings.Contains(d, "workbuddy.ai") || strings.Contains(d, "codebuddy.ai") {
			return wbRealmIntl
		}
	}
	return wbRealmCN
}

// wbRealmOf 同 wbRealmFor，但直接返回参数集。
func wbRealmOf(platform string, a *Account) wbRealm {
	return wbRealmByName(wbRealmFor(platform, a))
}

// wbRealmByName 按名字取参数集；未知名字回退国内版。
func wbRealmByName(name string) wbRealm {
	if r, ok := wbRealms[name]; ok {
		return r
	}
	return wbRealms[wbRealmCN]
}

// wbRealmForAccount 账号自身所属区域（平台未知时用）。
func wbRealmForAccount(a *Account) wbRealm {
	if a == nil {
		return wbRealms[wbRealmCN]
	}
	return wbRealmOf(a.Platform, a)
}

// wbOriginRefererFor 返回 Origin/Referer 基址（供出站头使用）。
func wbOriginRefererFor(a *Account) string {
	return wbRealmForAccount(a).Origin
}
