<template>
  <div v-if="visible" class="space-y-1">
    <!-- Credits value row: probe result, else persisted extra snapshot -->
    <div class="flex flex-wrap items-center gap-1.5">
      <span
        data-test="workbuddy-credits-value"
        class="text-[10px] font-medium leading-4 text-gray-700 dark:text-gray-200"
        :title="creditsTooltip"
      >
        {{ creditsLabel }}
      </span>
      <span
        v-if="checkedInToday"
        class="inline-flex items-center rounded bg-green-100 px-1 py-0.5 text-[10px] font-medium text-green-700 dark:bg-green-900/30 dark:text-green-300"
        :title="t('admin.accounts.workbuddyCredits.statusAlready')"
      >
        {{ statusLabel }}
      </span>
    </div>

    <!-- Actions: query credits + single-account check-in -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        data-test="workbuddy-credits-query"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-blue-600 transition-colors hover:bg-blue-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-blue-400 dark:hover:bg-blue-900/30"
        :disabled="loading"
        :title="t('admin.accounts.workbuddyCredits.creditsTooltip')"
        @click="handleQuery"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': loading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path
            stroke-linecap="round"
            stroke-linejoin="round"
            stroke-width="2"
            d="M4 4v5h.582m15.356 2A8.001 8.001 0 004.582 9m0 0H9m11 11v-5h-.581m0 0a8.003 8.003 0 01-15.357-2m15.357 2H15"
          />
        </svg>
        {{ t('admin.accounts.workbuddyCredits.query') }}
      </button>

      <button
        type="button"
        data-test="workbuddy-credits-checkin"
        class="inline-flex items-center gap-0.5 whitespace-nowrap rounded px-1.5 py-0.5 text-[10px] font-medium leading-4 text-emerald-600 transition-colors hover:bg-emerald-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-emerald-400 dark:hover:bg-emerald-900/30"
        :disabled="checkinLoading || loading"
        :title="t('admin.accounts.workbuddyCredits.checkinTooltip')"
        @click="handleCheckin"
      >
        <svg
          class="h-2.5 w-2.5"
          :class="{ 'animate-spin': checkinLoading }"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
        >
          <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />
        </svg>
        {{ t('admin.accounts.workbuddyCredits.checkin') }}
      </button>
    </div>

    <div v-if="error" class="truncate text-[10px] text-red-600 dark:text-red-400" :title="error">
      {{ truncatedError }}
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import type { WorkBuddyCredits } from '@/api/admin/workbuddy'
import type { Account } from '@/types'

const props = defineProps<{
  account: Account
}>()

const emit = defineEmits<{
  (e: 'account-updated', account: Account): void
}>()

const { t } = useI18n()

// 仅 WorkBuddy 平台账号展示。
const visible = computed(() => props.account.platform === 'workbuddy')

const loading = ref(false)
const checkinLoading = ref(false)
const error = ref<string | null>(null)
const data = ref<WorkBuddyCredits | null>(null)

// 落库快照（后端查询/签到后写入 account.Extra）。
const snapshotRemain = computed<number | null>(() => {
  const v = props.account.extra?.workbuddy_credits
  return typeof v === 'number' ? v : null
})
const snapshotSize = computed<number | null>(() => {
  const v = props.account.extra?.workbuddy_credits_size
  return typeof v === 'number' ? v : null
})

const currentRemain = computed<number | null>(() => {
  if (data.value) return data.value.remain
  return snapshotRemain.value
})
const currentSize = computed<number | null>(() => {
  if (data.value) return data.value.size
  return snapshotSize.value
})

/** 今日是否已签到（后端按本地自然日写 workbuddy_checkin_date）。 */
const checkedInToday = computed(() => {
  const status = props.account.extra?.workbuddy_checkin_status
  const date = props.account.extra?.workbuddy_checkin_date
  if (typeof status !== 'string' || typeof date !== 'string') return false
  if (status !== 'ok' && status !== 'already') return false
  return date === localDateString()
})

const statusLabel = computed(() => {
  const status = props.account.extra?.workbuddy_checkin_status
  if (status === 'ok') return t('admin.accounts.workbuddyCredits.statusOk')
  return t('admin.accounts.workbuddyCredits.statusAlready')
})

const creditsTooltip = computed(() => t('admin.accounts.workbuddyCredits.creditsTooltip'))

const creditsLabel = computed(() => {
  if (currentRemain.value == null) return t('admin.accounts.workbuddyCredits.creditsPlaceholder')
  const remain = formatCredits(currentRemain.value)
  if (currentSize.value == null || currentSize.value <= 0) return remain
  return t('admin.accounts.workbuddyCredits.remainOfTotal', {
    remain,
    size: formatCredits(currentSize.value)
  })
})

/** 积分取整展示：大额省略小数，小额保留两位。 */
const formatCredits = (v: number): string => {
  if (!Number.isFinite(v)) return '--'
  if (Math.abs(v) >= 1000) return v.toFixed(0)
  return Number.isInteger(v) ? String(v) : v.toFixed(2)
}

/** 本地自然日（与后端 wbTodayCheckinDate 口径一致）。 */
const localDateString = (): string => {
  const d = new Date()
  const m = String(d.getMonth() + 1).padStart(2, '0')
  const day = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${m}-${day}`
}

const extractErrorMessage = (e: unknown): string => {
  const err = e as {
    message?: string
    reason?: string
    response?: { data?: { message?: string; error?: string } }
  }
  return (
    err?.message ||
    err?.reason ||
    err?.response?.data?.message ||
    err?.response?.data?.error ||
    t('common.error')
  )
}

const truncatedError = computed(() => {
  if (!error.value) return ''
  return error.value.length > 80 ? `${error.value.slice(0, 80)}...` : error.value
})

/** 重读账号，使 extra 快照（积分/签到状态）回填到列表行。 */
const refreshAccount = async () => {
  try {
    const fresh = await adminAPI.accounts.getById(props.account.id)
    emit('account-updated', fresh)
  } catch {
    // 回填失败不影响本次操作结果，静默即可。
  }
}

const handleQuery = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    data.value = await adminAPI.workbuddy.queryCredits(props.account.id)
    await refreshAccount()
  } catch (e) {
    error.value = extractErrorMessage(e) || t('admin.accounts.workbuddyCredits.queryFailed')
  } finally {
    loading.value = false
  }
}

const handleCheckin = async () => {
  if (checkinLoading.value) return
  checkinLoading.value = true
  error.value = null
  try {
    const result = await adminAPI.workbuddy.checkinAccount(props.account.id)
    if (result.credits) data.value = result.credits
    if (result.status === 'fail') {
      error.value = result.detail || t('admin.accounts.workbuddyCredits.checkinFailed')
    }
    await refreshAccount()
  } catch (e) {
    error.value = extractErrorMessage(e) || t('admin.accounts.workbuddyCredits.checkinFailed')
  } finally {
    checkinLoading.value = false
  }
}

watch(
  () => props.account.id,
  () => {
    data.value = null
    error.value = null
    loading.value = false
    checkinLoading.value = false
  }
)
</script>
