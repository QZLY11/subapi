/**
 * Admin WorkBuddy CN (CodeBuddy) API endpoints.
 *
 * WorkBuddy 的授权是「设备授权流」（poll 型），而非 Grok 那样的 redirect-callback：
 *   1. auth-url  → 拿上游签发的 state + 授权链接（管理员在浏览器打开并登录）
 *   2. poll      → 轮询登录结果，pending 时后端返回 { pending: true }
 *   3. create    → 用同一 state 建号（后端复用 poll 结果，不重复打上游）
 */

import { apiClient } from '../client'

export interface WorkBuddyAuthUrlResponse {
  auth_url: string
  state: string
}

export interface WorkBuddyPollPending {
  pending: true
  message?: string
}

export interface WorkBuddyTokenInfo {
  access_token: string
  refresh_token?: string
  expires_in?: number
  expires_at?: number
  domain?: string
  uid?: string
  enterprise_id?: string
  nickname?: string
}

export interface WorkBuddyCreateFromOAuthRequest {
  state: string
  name?: string
  concurrency?: number
  priority?: number
  group_ids?: number[]
}

const WORKBUDDY_POLL_TIMEOUT_MS = 30_000

export async function generateAuthUrl(): Promise<WorkBuddyAuthUrlResponse> {
  const { data } = await apiClient.post<WorkBuddyAuthUrlResponse>(
    '/admin/workbuddy/oauth/auth-url'
  )
  return data
}

/** 轮询登录结果。返回 null 表示仍在等待用户完成授权。 */
export async function pollLogin(
  state: string
): Promise<WorkBuddyTokenInfo | null> {
  const { data } = await apiClient.post<WorkBuddyTokenInfo | WorkBuddyPollPending>(
    '/admin/workbuddy/oauth/poll',
    { state },
    { timeout: WORKBUDDY_POLL_TIMEOUT_MS }
  )
  if (data && (data as WorkBuddyPollPending).pending) return null
  return data as WorkBuddyTokenInfo
}

export async function createFromOAuth(
  payload: WorkBuddyCreateFromOAuthRequest
): Promise<unknown> {
  const { data } = await apiClient.post(
    '/admin/workbuddy/oauth/create-from-oauth',
    payload
  )
  return data
}

export async function refreshToken(refreshToken: string): Promise<WorkBuddyTokenInfo> {
  const { data } = await apiClient.post<WorkBuddyTokenInfo>(
    '/admin/workbuddy/oauth/refresh-token',
    { refresh_token: refreshToken }
  )
  return data
}

export async function refreshAccountToken(id: number): Promise<unknown> {
  const { data } = await apiClient.post(
    `/admin/workbuddy/accounts/${id}/refresh-token`
  )
  return data
}

// ============================================================================
// 积分查询与签到（billing 域，走 www.codebuddy.cn）
// ============================================================================

export interface WorkBuddyCreditPackage {
  package_name: string
  remain: number
  used: number
  size: number
}

export interface WorkBuddyCredits {
  uid: string
  nickname?: string
  remain: number
  used: number
  size: number
  packages?: WorkBuddyCreditPackage[]
  total_dosage?: number
}

/** 签到结果状态：ok 成功 / already 今日已签 / fail 失败 / skipped 无凭据。 */
export type WorkBuddyCheckinStatus = 'ok' | 'already' | 'fail' | 'skipped'

export interface WorkBuddyCheckinResult {
  uid: string
  nickname?: string
  status: WorkBuddyCheckinStatus
  detail?: string
  credits?: WorkBuddyCredits
}

export interface WorkBuddyAccountCredits {
  account_id: number
  name: string
  uid?: string
  nickname?: string
  remain: number
  used: number
  size: number
  ok: boolean
  error?: string
  checked_in_today: boolean
  checkin_status?: string
}

export interface WorkBuddyCreditsSummary {
  /** WorkBuddy 账号总数 */
  account_count: number
  /** 总计分额度 */
  total_size: number
  /** 剩余总积分 */
  total_remain: number
  /** 已用总积分 */
  total_used: number
  /** 今日已签到账号数 */
  today_checkin_count: number
  today_checkin_date: string
  accounts: WorkBuddyAccountCredits[]
  ok_count: number
  fail_count: number
  fetched_at: number
}

export interface WorkBuddyCheckinAllResponse {
  results: WorkBuddyCheckinResult[]
  total: number
  ok: number
  already: number
  fail: number
  skipped: number
  summary: string
  /** 签到后立即回读的汇总统计（可能因查询失败而缺失）。 */
  credits?: WorkBuddyCreditsSummary
  credits_error?: string
}

/** 查询单账号积分（会落 Extra 快照）。 */
export async function queryCredits(id: number): Promise<WorkBuddyCredits> {
  const { data } = await apiClient.get<WorkBuddyCredits>(
    `/admin/workbuddy/accounts/${id}/credits`
  )
  return data
}

/** 单账号签到。 */
export async function checkinAccount(id: number): Promise<WorkBuddyCheckinResult> {
  const { data } = await apiClient.post<WorkBuddyCheckinResult>(
    `/admin/workbuddy/accounts/${id}/checkin`
  )
  return data
}

/** 全量积分汇总（账号总数 / 总计分额度 / 剩余总积分 / 今日已签到数）。 */
export async function queryCreditsSummary(): Promise<WorkBuddyCreditsSummary> {
  const { data } = await apiClient.get<WorkBuddyCreditsSummary>(
    '/admin/workbuddy/credits/summary',
    { timeout: 120_000 }
  )
  return data
}

/** 一键全量签到。耗时较长（逐账号串行 + 节流），故放宽超时。 */
export async function checkinAll(): Promise<WorkBuddyCheckinAllResponse> {
  const { data } = await apiClient.post<WorkBuddyCheckinAllResponse>(
    '/admin/workbuddy/checkin/all',
    {},
    { timeout: 300_000 }
  )
  return data
}

export default {
  generateAuthUrl,
  pollLogin,
  createFromOAuth,
  refreshToken,
  refreshAccountToken,
  queryCredits,
  checkinAccount,
  queryCreditsSummary,
  checkinAll,
}
