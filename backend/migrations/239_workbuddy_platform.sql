-- 把 WorkBuddy（CodeBuddy / copilot.tencent.com）加入 user_platform_quotas.platform CHECK。
--
-- 与 237/238 同型：DROP IF EXISTS 后重建超集约束（必须保留 opencode_go），
-- DROP + 幂等保证可重入，存量行瞬时校验通过。
--
-- 刻意不动的三处约束（都是"DB 放宽 → 出现 API 层/服务层必拒的假选项"）：
--   1. composite_model_routes.target_platform：workbuddy 未纳入 composite 成员
--      （isConcreteRequestPlatform / matchingPlatforms / matchingPlatforms 均不含它），
--      且 CompositeRouteRequest 的 oneof 白名单与之同步收紧。
--      多账号轮转走 workbuddy 自身的平台分组调度路径，不依赖 composite。
--   2. channel_monitors.provider / channel_monitor_request_templates.provider：
--      workbuddy 既无探活 adapter（providerAdapters）也无配额数据源分支
--      （channel_monitor_quota_fetcher 的 switch 会落到 fetchUsage，对 OAuth 账号无效），
--      放开只会得到一个可选中但创建必失败的 provider。
--   3. accounts 表本身没有 platform/type CHECK 约束，workbuddy 账号写入无需迁移。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go', 'workbuddy'));
