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

export default {
  generateAuthUrl,
  pollLogin,
  createFromOAuth,
  refreshToken,
  refreshAccountToken,
}
