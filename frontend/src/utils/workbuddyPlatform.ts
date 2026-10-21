/**
 * WorkBuddy 平台族判定。
 *
 * 腾讯把同一套服务拆成两个区域：
 *   - `workbuddy`      国内版，品牌 CodeBuddy，上游 copilot.tencent.com / www.codebuddy.cn
 *   - `workbuddy_intl` 国际版，品牌 WorkBuddy，上游 www.workbuddy.ai
 *
 * 两者协议同源、共用同一套转发管线与积分逻辑，差异只在区域出站参数
 * （上游基址 / Origin / UA / 语种）与「国际版没有签到接口」。
 *
 * 底部 platform 标识保持不变：`workbuddy` 已落库到账号、分组与历史用量，
 * 改标识会破坏兼容性；品牌差异只体现在 UI 显示名（见 constants/platforms.ts）。
 */

/** 国内版平台标识（历史沿用 workbuddy）。 */
export const PLATFORM_CODEBUDDY = 'workbuddy' as const
/** 国际版平台标识。 */
export const PLATFORM_WORKBUDDY = 'workbuddy_intl' as const

/** 平台是否属于 WorkBuddy 家族（国内版或国际版）。 */
export function isWorkBuddyPlatform(platform?: string | null): boolean {
  return platform === PLATFORM_CODEBUDDY || platform === PLATFORM_WORKBUDDY
}

/** 平台是否属于国际版。 */
export function isWorkBuddyIntlPlatform(platform?: string | null): boolean {
  return platform === PLATFORM_WORKBUDDY
}

/**
 * 平台显示名。国内版显示 CodeBuddy，国际版显示 WorkBuddy。
 * 未匹配时回退原值，避免把未知平台渲染成空。
 */
export function workBuddyPlatformLabel(platform?: string | null): string {
  if (platform === PLATFORM_WORKBUDDY) return 'WorkBuddy'
  if (platform === PLATFORM_CODEBUDDY) return 'CodeBuddy'
  return platform ?? ''
}
