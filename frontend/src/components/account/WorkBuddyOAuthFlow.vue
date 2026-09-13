<template>
  <div class="space-y-5">
    <!-- 步骤 1：生成设备授权链接 -->
    <div class="rounded-lg border border-gray-200 p-4 dark:border-dark-600">
      <div class="flex items-start gap-3">
        <span
          class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-sky-600 text-xs font-semibold text-white"
        >
          1
        </span>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.oauth.workbuddy.step1GenerateUrl') }}
          </p>
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.oauth.workbuddy.step1Desc') }}
          </p>
          <button
            type="button"
            class="btn btn-secondary mt-3"
            :disabled="generating || polling"
            @click="startFlow"
          >
            <svg
              v-if="generating"
              class="-ml-1 mr-2 h-4 w-4 animate-spin"
              fill="none"
              viewBox="0 0 24 24"
            >
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
              <path
                class="opacity-75"
                fill="currentColor"
                d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
              />
            </svg>
            {{
              authUrl
                ? t('admin.accounts.oauth.workbuddy.regenerate')
                : t('admin.accounts.oauth.workbuddy.generateAuthUrl')
            }}
          </button>
        </div>
      </div>
    </div>

    <!-- 步骤 2：打开链接并登录 -->
    <div
      v-if="authUrl"
      class="rounded-lg border border-gray-200 p-4 dark:border-dark-600"
    >
      <div class="flex items-start gap-3">
        <span
          class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-sky-600 text-xs font-semibold text-white"
        >
          2
        </span>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.oauth.workbuddy.step2OpenUrl') }}
          </p>
          <p class="mt-1 text-xs text-gray-500 dark:text-gray-400">
            {{ t('admin.accounts.oauth.workbuddy.step2Desc') }}
          </p>

          <div class="mt-3 flex flex-wrap items-center gap-2">
            <a
              :href="authUrl"
              target="_blank"
              rel="noreferrer"
              class="btn btn-primary"
            >
              {{ t('admin.accounts.oauth.workbuddy.openUrl') }}
            </a>
            <button
              type="button"
              class="btn btn-secondary"
              @click="copyAuthUrl"
            >
              {{ copied ? t('admin.accounts.oauth.workbuddy.copied') : t('admin.accounts.oauth.workbuddy.copyUrl') }}
            </button>
          </div>

          <p class="mt-3 break-all rounded bg-gray-50 p-2 font-mono text-xs text-gray-600 dark:bg-dark-700 dark:text-gray-300">
            {{ authUrl }}
          </p>

          <!-- 轮询状态 -->
          <div class="mt-3 flex items-center gap-2 text-xs">
            <template v-if="polling">
              <svg class="h-3.5 w-3.5 animate-spin text-sky-600" fill="none" viewBox="0 0 24 24">
                <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
                <path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4z" />
              </svg>
              <span class="text-gray-500 dark:text-gray-400">
                {{ t('admin.accounts.oauth.workbuddy.waitingLogin') }}
              </span>
            </template>
            <span v-else-if="tokenInfo" class="font-medium text-green-600 dark:text-green-400">
              {{ t('admin.accounts.oauth.workbuddy.loginSuccess') }}
            </span>
          </div>
        </div>
      </div>
    </div>

    <!-- 步骤 3：确认账号信息 -->
    <div
      v-if="tokenInfo"
      class="rounded-lg border border-gray-200 p-4 dark:border-dark-600"
    >
      <div class="flex items-start gap-3">
        <span
          class="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-sky-600 text-xs font-semibold text-white"
        >
          3
        </span>
        <div class="min-w-0 flex-1">
          <p class="text-sm font-medium text-gray-900 dark:text-white">
            {{ t('admin.accounts.oauth.workbuddy.step3Confirm') }}
          </p>

          <dl class="mt-3 grid grid-cols-1 gap-2 text-xs sm:grid-cols-2">
            <div v-if="tokenInfo.nickname">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.workbuddy.nickname') }}</dt>
              <dd class="font-medium text-gray-900 dark:text-white">{{ tokenInfo.nickname }}</dd>
            </div>
            <div v-if="tokenInfo.uid">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.workbuddy.uid') }}</dt>
              <dd class="break-all font-mono text-gray-900 dark:text-white">{{ tokenInfo.uid }}</dd>
            </div>
            <div v-if="tokenInfo.enterprise_id">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.workbuddy.enterpriseId') }}</dt>
              <dd class="break-all font-mono text-gray-900 dark:text-white">{{ tokenInfo.enterprise_id }}</dd>
            </div>
            <div v-if="tokenInfo.domain">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.workbuddy.domain') }}</dt>
              <dd class="break-all font-mono text-gray-900 dark:text-white">{{ tokenInfo.domain }}</dd>
            </div>
            <div v-if="tokenInfo.expires_at">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('admin.accounts.oauth.workbuddy.expiresAt') }}</dt>
              <dd class="font-medium text-gray-900 dark:text-white">{{ formatExpiresAt(tokenInfo.expires_at) }}</dd>
            </div>
          </dl>

          <button
            type="button"
            class="btn btn-primary mt-4"
            :disabled="creating"
            @click="createAccount"
          >
            <svg
              v-if="creating"
              class="-ml-1 mr-2 h-4 w-4 animate-spin"
              fill="none"
              viewBox="0 0 24 24"
            >
              <circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4" />
              <path
                class="opacity-75"
                fill="currentColor"
                d="M4 12a8 8 0 018-8V0C5.373 0 0 5.373 0 12h4zm2 5.291A7.962 7.962 0 014 12H0c0 3.042 1.135 5.824 3 7.938l3-2.647z"
              />
            </svg>
            {{ creating ? t('admin.accounts.creating') : t('admin.accounts.oauth.workbuddy.createAccount') }}
          </button>
        </div>
      </div>
    </div>

    <!-- 错误 -->
    <div
      v-if="error"
      class="rounded-lg border border-red-200 bg-red-50 p-3 text-sm text-red-700 dark:border-red-800 dark:bg-red-900/20 dark:text-red-400"
    >
      {{ error }}
    </div>

    <p class="text-xs text-gray-500 dark:text-gray-400">
      {{ t('admin.accounts.oauth.workbuddy.notice') }}
    </p>
  </div>
</template>

<script setup lang="ts">
import { onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useAppStore } from '@/stores/app'
import workbuddyAPI from '@/api/admin/workbuddy'
import type { WorkBuddyTokenInfo } from '@/api/admin/workbuddy'

const props = withDefaults(
  defineProps<{
    /** 账号名称；留空时后端回退为昵称 / uid */
    name?: string
    concurrency?: number
    priority?: number
    groupIds?: number[]
  }>(),
  {
    name: '',
    concurrency: 10,
    priority: 1,
    groupIds: () => []
  }
)

const emit = defineEmits<{
  (e: 'created'): void
}>()

const { t } = useI18n()
const appStore = useAppStore()

/** 轮询间隔：设备流通常 2~5 秒内完成授权。 */
const POLL_INTERVAL_MS = 3_000

const generating = ref(false)
const polling = ref(false)
const creating = ref(false)
const copied = ref(false)
const error = ref('')
const authUrl = ref('')
const state = ref('')
const tokenInfo = ref<WorkBuddyTokenInfo | null>(null)

let pollTimer: ReturnType<typeof setTimeout> | null = null

const stopPolling = () => {
  polling.value = false
  if (pollTimer !== null) {
    clearTimeout(pollTimer)
    pollTimer = null
  }
}

onBeforeUnmount(stopPolling)

const startFlow = async () => {
  stopPolling()
  error.value = ''
  tokenInfo.value = null
  copied.value = false
  generating.value = true
  try {
    const res = await workbuddyAPI.generateAuthUrl()
    authUrl.value = res.auth_url
    state.value = res.state
    generating.value = false
    // 直接进入轮询：管理员在新标签页完成登录后前端自动感知。
    polling.value = true
    schedulePoll()
  } catch (e: any) {
    generating.value = false
    error.value = extractError(e, t('admin.accounts.oauth.workbuddy.failedToGenerateUrl'))
  }
}

const schedulePoll = () => {
  pollTimer = setTimeout(pollOnce, POLL_INTERVAL_MS)
}

const pollOnce = async () => {
  pollTimer = null
  if (!polling.value || !state.value) return
  try {
    const info = await workbuddyAPI.pollLogin(state.value)
    if (info) {
      tokenInfo.value = info
      stopPolling()
      return
    }
    // null = 仍在等待：继续轮询。
    schedulePoll()
  } catch (e: any) {
    stopPolling()
    error.value = extractError(e, t('admin.accounts.oauth.workbuddy.pollFailed'))
  }
}

/** apiClient 拦截器 reject 的是普通对象（{status,message,...}），优先读 .message。 */
const extractError = (e: any, fallback: string): string =>
  e?.message || e?.response?.data?.detail || e?.response?.data?.message || fallback

const copyAuthUrl = async () => {
  try {
    await navigator.clipboard.writeText(authUrl.value)
    copied.value = true
    setTimeout(() => (copied.value = false), 2_000)
  } catch {
    error.value = t('admin.accounts.oauth.workbuddy.copyFailed')
  }
}

const formatExpiresAt = (unixSeconds: number) => new Date(unixSeconds * 1000).toLocaleString()

const createAccount = async () => {
  if (!state.value) return
  creating.value = true
  error.value = ''
  try {
    await workbuddyAPI.createFromOAuth({
      state: state.value,
      name: props.name.trim() || undefined,
      concurrency: props.concurrency,
      priority: props.priority,
      group_ids: props.groupIds
    })
    appStore.showSuccess(t('admin.accounts.oauth.workbuddy.created'))
    emit('created')
  } catch (e: any) {
    error.value = extractError(e, t('admin.accounts.oauth.workbuddy.createFailed'))
  } finally {
    creating.value = false
  }
}
</script>
