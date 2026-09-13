# 方案 B：把 workbuddy2api 内嵌为 sub2api 原生平台「WorkBuddy」

> 目标：让管理员在 sub2api 后台「新建账号 → 平台」下拉框里直接选 **WorkBuddy / CodeBuddy**，
> 账号类型为 **OAuth（accessToken + refreshToken）**，账号池、token 刷新、上游转发全部由 sub2api
> 自己接管，不再需要单独跑 workbuddy2api 进程。
>
> 已确认的源码事实（两个仓库均已 clone 到本地）：
> - sub2api：`D:\Trae_code\杂乱会话\sub2api`
> - workbuddy2api：`D:\Trae_code\杂乱会话\workbuddy2api`

---

## 0. 核心差异（为何不能只加一个下拉项）

sub2api 内置的 `deepseek/zhipu/kimi` 是「OpenAI Chat Completions 兼容 + api_key/base_url 透传」。
workbuddy2api 要内嵌，两者差异集中在：

| 维度 | deepseek（现有范式） | workbuddy2api（要新增） |
|---|---|---|
| 上游端点 | `{base_url}/chat/completions` | `https://copilot.tencent.com/v2/chat/completions` |
| 账号类型 | `apikey` / `upstream` | `oauth`（accessToken + refreshToken + uid + enterpriseId） |
| 鉴权头 | `Authorization: Bearer sk-xxx` | `Authorization: Bearer <accessToken>` + `X-User-Id` + `X-Enterprise-Id` + `X-Domain` + UA 指纹 |
| token 生命周期 | 无过期 | accessToken 有 `expiresAt`，需自动 refresh（`/v2/plugin/auth/token/refresh`） |
| 请求改写 | 基本透传 | 强制 `stream:true`、tool_choice 归一化、developer→system、thinking 注入、reasoning 降级 |
| 错误处理 | 429/402 标准 | 402 余额、429 软限流、401+12153 session 失效、400 内容拦截/坏参数 分类冷却 |

**结论**：workbuddy2api 本质是一个「OAuth 账号池 + 专用上游 client」的完整实现。
内嵌到 sub2api，需要新增：1 个平台常量 + 1 个账号类型 + 1 套 OAuth 登录/刷新 + 1 套上游 client 适配 + 前端下拉/图标/白名单。

---

## 1. 后端改动清单（backend/）

### 1.1 平台常量 —— `backend/internal/domain/constants.go`

在「国产 OpenAI 兼容供应商」块后新增：

```go
// WorkBuddy CN（CodeBuddy / copilot.tencent.com）：
// 走 OAuth 设备授权 + /v2/chat/completions 端点，协议上兼容 OpenAI Chat Completions。
PlatformWorkBuddy = "workbuddy"
```

### 1.2 账号类型常量（同文件 Account type 块）

```go
// AccountTypeWorkBuddyOAuth 是 WorkBuddy CN 的 OAuth 账号类型。
// credentials 结构：access_token / refresh_token / expires_at / uid / enterprise_id / domain / device_token
AccountTypeWorkBuddyOAuth = "workbuddy_oauth"
```

> 说明：不复用通用 `oauth` 类型，是因为 WorkBuddy 的 token 存储、刷新端点、header 组
> 与 Gemini OAuth 完全不同，独立类型避免污染既有 Gemini OAuth 逻辑。

### 1.3 账号平台判定 —— `backend/internal/service/account.go`

在 `IsMiniMax()` 之后新增：

```go
func (a *Account) IsWorkBuddy() bool {
	return a != nil && a.Platform == PlatformWorkBuddy
}

func (a *Account) IsWorkBuddyOAuth() bool {
	return a != nil && a.Platform == PlatformWorkBuddy && a.Type == AccountTypeWorkBuddyOAuth
}
```

并把 `IsOpenAICompatible()` 加进 `PlatformWorkBuddy`（它走 OpenAI gateway 转发）：

```go
func (a *Account) IsOpenAICompatible() bool {
	return a != nil && (a.Platform == PlatformOpenAI || a.Platform == PlatformGrok ||
		a.IsCNProvider() || a.IsOpenCodeGo() || a.Platform == PlatformWorkBuddy)
}
```

### 1.4 平台白名单数组

搜索并同步所有「平台枚举」出现的位置（grep `PlatformDeepseek` 已确认约 235 处），
需要把 `PlatformWorkBuddy` 加入以下**关键白名单**（其余按测试/监控逐个补）：

1. `backend/internal/service/admin_group.go` 第 319 行的平台遍历数组
2. `backend/internal/service/account_service.go` 第 518 行附近（`IsOpenAICompatible` 相关分支）
3. `backend/internal/service/account.go` 的 `platformOrder` / `AllPlatforms` 类数组（若有）
4. `backend/internal/model/error_passthrough_rule.go`（若需透传上游错误）

### 1.5 gateway 路由 —— `backend/internal/server/routes/gateway.go`

第 51、61 行的 `isOpenAIResponsesCompatibleGatewayPlatform` 与 `countTokensHandler` 的
switch 里，把 `service.PlatformWorkBuddy` 加入 OpenAI 兼容分支：

```go
case service.PlatformOpenAI, service.PlatformGrok,
	service.PlatformKimi, service.PlatformZhipu, service.PlatformDeepseek,
	service.PlatformMiniMax, service.PlatformOpenCodeGo, service.PlatformWorkBuddy:
	return true
```

### 1.6 上游 client 适配（新文件）—— `backend/internal/service/workbuddy_client.go`

这是本次集成的**核心**。把 workbuddy2api 的 `internal/upstream` 逻辑迁移为 sub2api 的 service：

```go
package service

// WorkBuddy 上游常量
const (
	wbUpstreamBase    = "https://copilot.tencent.com"
	wbChatEndpoint    = wbUpstreamBase + "/v2/chat/completions"
	wbRefreshEndpoint = wbUpstreamBase + "/v2/plugin/auth/token/refresh"
	wbModelsEndpoint  = wbUpstreamBase + "/console/enterprises/personal/models"
	wbOriginReferer   = "https://www.codebuddy.cn"
	wbClientVersion   = "5.5.4"   // WorkBuddy 桌面端版本
	wbCliVersion      = "2.137.1" // 内置 CLI 版本
)

// WorkBuddyClient 封装对 CodeBuddy CN 上游的全部调用与错误分类。
type WorkBuddyClient struct {
	HTTPClient *http.Client
	// 可覆盖字段（从 config 读）
	ClientVersion string
	CliVersion    string
	UserAgent     string
}

// WorkBuddyAuth 归一化账号凭证（对应 workbuddy2api internal/auth.Auth）
type WorkBuddyAuth struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	UID          string
	EnterpriseID string
	DeviceToken  string
}

// NeedsRefresh 报告 token 是否将在 within 内过期。
func (a *WorkBuddyAuth) NeedsRefresh(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// CommonHeaders 设置 WorkBuddy 上游共享头（对齐 workbuddy2api headers.go）。
func (c *WorkBuddyClient) CommonHeaders(req *http.Request, a *WorkBuddyAuth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", wbOriginReferer)
	req.Header.Set("Referer", wbOriginReferer+"/")
	req.Header.Set("User-Agent", c.userAgent())
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
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

func (c *WorkBuddyClient) userAgent() string {
	if c.UserAgent != "" {
		return c.UserAgent
	}
	return fmt.Sprintf("WorkBuddy/%s WorkBuddy/%s CLI/%s",
		c.clientVersion(), c.clientVersion(), c.cliVersion())
}

// RefreshToken 刷新并返回新 access token（对齐 workbuddy2api refresh 逻辑）。
func (c *WorkBuddyClient) RefreshToken(ctx context.Context, a *WorkBuddyAuth) error {
	body, _ := json.Marshal(map[string]any{"refresh_token": a.RefreshToken})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, wbRefreshEndpoint, bytes.NewReader(body))
	c.CommonHeaders(req, a)
	req.Header.Set("X-Refresh-Token", a.RefreshToken)
	req.Header.Set("X-Auth-Refresh-Source", "workbuddy")
	// ... 解析 {code,msg,data:{accessToken,refreshToken,expiresIn}} 信封，
	// 更新 a.AccessToken / a.RefreshToken / a.ExpiresAt，超时/401 返回 error 供调度器标记 session_dead
	return nil
}

// PrepareChatPayload 改写发往上游的请求体（对齐 workbuddy2api payload.go）：
// 强制 stream:true、tool_choice 归一化、developer→system、thinking 注入、reasoning 降级。
func PrepareChatPayload(src []byte) []byte {
	// 逻辑照搬 workbuddy2api/internal/upstream/payload.go PrepareBodyOptWithEfforts
	return src
}

// ClassifyUpstreamError 把上游响应分类为冷却策略（对齐 workbuddy2api client.go ErrKind）。
// 返回 ErrKind 枚举，供 sub2api 调度器 map 到 rate_limited_at / session_dead / 熔断。
func ClassifyUpstreamError(status int, body string) wbErrKind {
	// 402 + hardMarkers → hard_credit
	// 429 或 softRateMarkers → soft_rate
	// 401 + "12153" → session_dead
	// 400 + contentBlockedMarkers → content_blocked（不罚号，降级重试）
	// 400 + "Unmarshal chat params failed" → bad_params
	// 5xx → server
	return wbErrServer
}
```

### 1.7 token 刷新调度 —— `backend/internal/service/account_service.go`

复用 sub2api 已有的「账号定时刷新」（Gemini OAuth 已有 token 刷新协程），为
`AccountTypeWorkBuddyOAuth` 接入同一刷新管道：当 `account.schedulable` 且
`expires_at` 临近，调用 `WorkBuddyClient.RefreshToken` 并写回 `credentials` 的
`access_token`/`refresh_token`/`expires_at`。

### 1.8 上游模型列表 —— `backend/internal/service/upstream_models.go`

第 628 / 795 行的 `IsOpenAICompatible` 分支已覆盖（因 1.3 已把 WorkBuddy 并入），
`/v1/models` 会转发到 WorkBuddy 上游模型端点。

### 1.9 错误透传规则 —— `backend/internal/model/error_passthrough_rule.go`

若希望上游「内容拦截 / 余额不足」文案原样透传给客户端，把 `PlatformWorkBuddy`
加入透传白名单（参考 `PlatformDeepseek` 的既有写法）。

---

## 2. 数据库迁移（backend/migrations/）

新增迁移 `XXX_add_workbuddy_platform.sql`，放宽所有含平台 CHECK 约束的表：

```sql
-- user_platform_quotas / composite_routes / channel_monitor_* 等表
ALTER TABLE user_platform_quotas DROP CONSTRAINT ...;
ALTER TABLE user_platform_quotas ADD CONSTRAINT ...
  CHECK (platform IN ('anthropic','openai','gemini','antigravity','grok',
                      'kimi','zhipu','deepseek','minimax','opencode_go','workbuddy'));
```

> 逐表确认：grep 迁移测试里的 `CHECK (platform IN (...))` 已列出全部需要改的表
> （`user_platform_quota_cn_providers_migration_test.go`、`opencode_go_platform_migration_test.go`、
> `minimax_platform_migration_test.go`、`composite_routes_cn_providers_migration_test.go` 等）。
>
> 同时更新 `backend/ent/channelmonitor/channelmonitor.go` 第 184 行的 Provider 枚举、
> `backend/ent/channelmonitorrequesttemplate/...go` 第 110 行的 Provider 枚举、
> `backend/ent/migrate/schema.go` 第 626/779 行的 `provider` 枚举（新增 `workbuddy_oauth` 或 `workbuddy`）。

---

## 3. 前端改动清单（frontend/）

### 3.1 平台枚举 —— `frontend/src/types/index.ts`

```ts
// 第 541 行 GroupPlatform 与第 921 行 AccountPlatform
export type GroupPlatform = 'anthropic' | 'openai' | 'gemini' | 'antigravity' | 'grok'
  | 'kimi' | 'zhipu' | 'deepseek' | 'minimax' | 'opencode_go' | 'composite' | 'workbuddy'
export type AccountPlatform = '...' | 'workbuddy'
```

### 3.2 平台下拉框 —— `frontend/src/constants/platforms.ts`

```ts
{ value: 'workbuddy', label: 'WorkBuddy (CodeBuddy)' },
```

### 3.3 平台图标 —— `frontend/src/components/common/PlatformIcon.vue`

新增 `workbuddy` 分支的 SVG（复用 CodeBuddy logo 或 deepseek 风格占位）。

### 3.4 模型白名单 —— `frontend/src/composables/useModelWhitelist.ts`

新增 `workBuddyModels` 数组（与上游 `/v1/models` 返回一致），并接入 platform 分支。

### 3.5 新建账号对话框 —— 「平台=WorkBuddy」时的表单

账号类型固定为 **OAuth**，要求管理员粘贴 `accessToken` / `refreshToken` /
`uid` / `enterpriseId`（可提供「从 auth JSON 一键导入」），或直接在 UI 里跑 OAuth
设备授权流（`url`/`poll` 两步，参考 `cmd/login/main.go`）。

### 3.6 channelMonitor 枚举 —— `frontend/src/constants/channelMonitor.ts`、`src/api/admin/channelMonitor.ts`

新增 `PROVIDER_WORKBUDDY = 'workbuddy'` 与类型联合项。

---

## 4. OAuth 登录 UI（可选，增强项）

若要在 sub2api 后台「一键 OAuth 登录」而非手动粘贴 token，需在后端加两个管理端点：

```go
// 对齐 workbuddy2api cmd/login/main.go 的设备授权流
// POST /admin/workbuddy/oauth/start → 返回 {state, authUrl}
// GET  /admin/workbuddy/oauth/poll?state= → 返回完整 token bundle
```

前端在「新建账号 → WorkBuddy → OAuth」里展示二维码/链接 + 轮询按钮。

---

## 5. 验证闭环

```bash
# 1. 后端编译 + 迁移
cd sub2api/backend
go build ./...
# 应用 XXX_add_workbuddy_platform.sql 迁移

# 2. 直连上游验证（用真实 CodeBuddy token）
curl -sN https://copilot.tencent.com/v2/chat/completions \
  -H "Authorization: Bearer <accessToken>" \
  -H "X-User-Id: <uid>" -H "X-Enterprise-Id: <enterpriseId>" \
  -H "User-Agent: WorkBuddy/5.5.4 WorkBuddy/5.5.4 CLI/2.137.1" \
  -H "Origin: https://www.codebuddy.cn" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}],"stream":true}'

# 3. 经 sub2api 网关（平台=workbuddy 分组 + 用户 key）
curl -sN https://<sub2api域名>/v1/chat/completions \
  -H "Authorization: Bearer sk-xxx" \
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}],"stream":true}'
```

---

## 6. 改动规模评估

| 层 | 文件数（约） | 难度 |
|---|---|---|
| 后端平台常量 + 判定 + 白名单 | 6~8 | 低（机械枚举） |
| 后端上游 client（核心） | 1 新增 + 2~3 接入 | **高** |
| token 刷新调度接入 | 1~2 | 中 |
| 数据库迁移 + ent 枚举 | 3~5 | 中 |
| 前端枚举 + 下拉 + 图标 + 白名单 | 5~7 | 低 |
| OAuth 登录 UI（可选） | 2~4 | 中 |

**总计约 18~30 个文件。**

---

## 7. 建议的最小可用版本（MVP）

第一阶段先落地「手动粘贴 token + 单账号」，砍掉账号池轮转和 OAuth 一键登录：

1. `PlatformWorkBuddy` + `AccountTypeWorkBuddyOAuth` 常量
2. `WorkBuddyClient`（CommonHeaders + RefreshToken + PrepareChatPayload + ClassifyUpstreamError）
3. `account.go` 判定 + gateway.go 路由分支
4. 迁移 + 前端下拉框 + 图标 + 白名单
5. 单账号：调度器不轮转，token 快过期时用 RefreshToken 刷新后重试

跑通后，第二阶段再把 workbuddy2api 的「多账号池 + 冷却状态机」逻辑（`internal/pool/` /
`internal/scheduler/`）迁移进 sub2api 的调度器，实现和 workbuddy2api 同等的多账号轮转能力。