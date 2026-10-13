<template>
  <div
    v-if="visible"
    class="rounded-lg border border-gray-200 bg-white dark:border-gray-700 dark:bg-gray-800"
  >
    <!-- 常驻头部：折叠开关 + 标题 + 折叠态紧凑摘要 + 操作按钮 -->
    <div class="flex flex-wrap items-center justify-between gap-2 px-4 py-2.5">
      <div class="flex min-w-0 flex-1 items-center gap-2">
        <button
          type="button"
          data-test="workbuddy-panel-toggle"
          class="inline-flex shrink-0 items-center justify-center rounded p-1 text-gray-500 transition-colors hover:bg-gray-100 hover:text-gray-700 dark:text-gray-400 dark:hover:bg-gray-700 dark:hover:text-gray-200"
          :title="collapsed ? t('admin.accounts.workbuddyCredits.expand') : t('admin.accounts.workbuddyCredits.collapse')"
          :aria-expanded="!collapsed"
          @click="collapsed = !collapsed"
        >
          <svg
            class="h-4 w-4 transition-transform"
            :class="collapsed ? '-rotate-90' : 'rotate-0'"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 9l-7 7-7-7" />
          </svg>
        </button>

        <h3 class="shrink-0 text-sm font-semibold text-gray-900 dark:text-white">
          {{ t('admin.accounts.workbuddyCredits.summaryTitle') }}
        </h3>

        <!-- 折叠态：单行紧凑摘要，不占版面 -->
        <span
          v-if="collapsed && summary"
          data-test="workbuddy-compact-summary"
          class="truncate text-[11px] text-gray-500 dark:text-gray-400"
        >
          {{ compactSummary }}
        </span>
      </div>

      <div class="flex shrink-0 items-center gap-2">
        <!-- 自动查询开关：切换即写库，后台刷新服务下一轮生效，无需重启 -->
        <label
          class="inline-flex cursor-pointer select-none items-center gap-1.5 text-xs text-gray-600 dark:text-gray-300"
          :title="t('admin.accounts.workbuddyCredits.autoRefreshHint')"
          data-test="workbuddy-auto-refresh"
        >
          <Toggle
            v-model="autoRefreshEnabled"
            :disabled="settingsSaving"
            @update:model-value="handleAutoRefreshChange"
          />
          <span>{{ t('admin.accounts.workbuddyCredits.autoRefresh') }}</span>
        </label>

        <button
          type="button"
          data-test="workbuddy-summary-refresh"
          :title="t('admin.accounts.workbuddyCredits.queryAllHint')"
          class="inline-flex items-center gap-1 rounded-md border border-gray-300 px-2.5 py-1.5 text-xs font-medium text-gray-700 transition-colors hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50 dark:border-gray-600 dark:text-gray-200 dark:hover:bg-gray-700"
          :disabled="loading || checkinRunning"
          @click="loadSummary"
        >
          <svg
            class="h-3.5 w-3.5"
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
          {{
            loading
              ? t('admin.accounts.workbuddyCredits.queryAllRunning')
              : t('admin.accounts.workbuddyCredits.queryAll')
          }}
        </button>

        <button
          type="button"
          data-test="workbuddy-checkin-all"
          class="inline-flex items-center gap-1 rounded-md bg-emerald-600 px-2.5 py-1.5 text-xs font-medium text-white transition-colors hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-50"
          :disabled="checkinRunning || loading || accountCount === 0"
          @click="handleCheckinAll"
        >
          <svg
            class="h-3.5 w-3.5"
            :class="{ 'animate-spin': checkinRunning }"
            fill="none"
            stroke="currentColor"
            viewBox="0 0 24 24"
          >
            <path
              stroke-linecap="round"
              stroke-linejoin="round"
              stroke-width="2"
              d="M9 12l2 2 4-4m6 2a9 9 0 11-18 0 9 9 0 0118 0z"
            />
          </svg>
          {{
            checkinRunning
              ? t('admin.accounts.workbuddyCredits.checkinAllRunning')
              : t('admin.accounts.workbuddyCredits.checkinAll')
          }}
        </button>
      </div>
    </div>

    <!-- 可折叠正文 -->
    <div v-show="!collapsed" class="border-t border-gray-100 px-4 pb-4 pt-3 dark:border-gray-700">
      <!-- Four headline stats: accounts / total credits / remaining / checked in today -->
      <div class="grid grid-cols-2 gap-3 sm:grid-cols-4">
        <div
          v-for="stat in stats"
          :key="stat.key"
          :data-test="`workbuddy-stat-${stat.key}`"
          class="rounded-md bg-gray-50 px-3 py-2 dark:bg-gray-900/40"
        >
          <div class="text-[11px] text-gray-500 dark:text-gray-400">{{ stat.label }}</div>
          <div class="mt-0.5 text-base font-semibold text-gray-900 dark:text-white">
            {{ stat.value }}
          </div>
        </div>
      </div>

      <!-- Remaining credits ratio bar -->
      <div v-if="summary && summary.total_size > 0" class="mt-3">
        <div class="mb-1 flex justify-between text-[11px] text-gray-500 dark:text-gray-400">
          <span>{{ t('admin.accounts.workbuddyCredits.totalRemain') }}</span>
          <span>{{ percentRemainLabel }}</span>
        </div>
        <div class="h-1.5 w-full overflow-hidden rounded-full bg-gray-200 dark:bg-gray-700">
          <div
            class="h-full rounded-full bg-emerald-500 transition-all"
            :style="{ width: `${remainPercent}%` }"
          />
        </div>
      </div>

      <div v-if="error" class="mt-3 text-xs text-red-600 dark:text-red-400">{{ error }}</div>
      <div
        v-if="checkinMessage"
        class="mt-3 text-xs text-emerald-700 dark:text-emerald-400"
        data-test="workbuddy-checkin-message"
      >
        {{ checkinMessage }}
      </div>
      <div
        v-if="summary?.today_checkin_date"
        class="mt-2 text-[11px] text-gray-400 dark:text-gray-500"
      >
        {{ t('admin.accounts.workbuddyCredits.todayCheckin') }} ·
        {{ summary.today_checkin_date }}
      </div>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { adminAPI } from '@/api/admin'
import Toggle from '@/components/common/Toggle.vue'
import type { WorkBuddyCreditsSummary } from '@/api/admin/workbuddy'

const { t } = useI18n()

// 默认折叠，避免占用列表空间遮挡其他账号。
const collapsed = ref(true)

const loading = ref(false)
const checkinRunning = ref(false)
const error = ref<string | null>(null)
const checkinMessage = ref<string | null>(null)
const summary = ref<WorkBuddyCreditsSummary | null>(null)

// 后台自动查询开关：与后端 settings 同步，切换即写库（下一轮生效，无需重启）。
const autoRefreshEnabled = ref(true)
const settingsSaving = ref(false)

// 仅在确有 WorkBuddy 账号时显示（避免给未使用该平台的部署增加噪音）。
const accountCount = computed(() => summary.value?.account_count ?? 0)
const visible = computed(() => summary.value !== null && accountCount.value > 0)

const formatCredits = (v: number): string => {
  if (!Number.isFinite(v)) return '--'
  if (Math.abs(v) >= 1000) return v.toLocaleString('en-US', { maximumFractionDigits: 0 })
  return Number.isInteger(v) ? String(v) : v.toFixed(2)
}

const stats = computed(() => [
  {
    key: 'account-count',
    label: t('admin.accounts.workbuddyCredits.accountCount'),
    value: summary.value ? `${accountCount.value} ${t('admin.accounts.workbuddyCredits.accountsUnit')}`.trim() : '--'
  },
  {
    key: 'total-size',
    label: t('admin.accounts.workbuddyCredits.totalSize'),
    value: summary.value ? formatCredits(summary.value.total_size) : '--'
  },
  {
    key: 'total-remain',
    label: t('admin.accounts.workbuddyCredits.totalRemain'),
    value: summary.value ? formatCredits(summary.value.total_remain) : '--'
  },
  {
    key: 'today-checkin',
    label: t('admin.accounts.workbuddyCredits.todayCheckin'),
    value: summary.value
      ? `${summary.value.today_checkin_count}/${accountCount.value}`
      : '--'
  }
])

/** 折叠态单行摘要：账号数 / 总积分 / 剩余 / 已签。 */
const compactSummary = computed(() => {
  const s = summary.value
  if (!s) return ''
  return t('admin.accounts.workbuddyCredits.compactSummary', {
    count: accountCount.value,
    size: formatCredits(s.total_size),
    remain: formatCredits(s.total_remain),
    checkin: `${s.today_checkin_count}/${accountCount.value}`
  })
})

const remainPercent = computed(() => {
  const s = summary.value
  if (!s || s.total_size <= 0) return 0
  const pct = (s.total_remain / s.total_size) * 100
  return Math.max(0, Math.min(100, Math.round(pct)))
})

const percentRemainLabel = computed(() =>
  t('admin.accounts.workbuddyCredits.percentRemain', { percent: remainPercent.value })
)

const extractErrorMessage = (e: unknown): string => {
  const err = e as { message?: string; reason?: string }
  return err?.message || err?.reason || t('common.error')
}

/**
 * 实时全量刷新：逐个打上游 meter 接口，账号多时需要数十秒。
 * 只由「刷新积分」按钮显式触发——绝不在页面加载路径上调用。
 */
const loadSummary = async () => {
  if (loading.value) return
  loading.value = true
  error.value = null
  try {
    summary.value = await adminAPI.workbuddy.queryCreditsSummary()
  } catch (e) {
    error.value = t('admin.accounts.workbuddyCredits.summaryFailed', {
      error: extractErrorMessage(e)
    })
  } finally {
    loading.value = false
  }
}

/**
 * 读取积分快照：后端只读 Extra 缓存，毫秒级返回，不打上游。
 * 页面加载走这里，数据由后端后台错开刷新维护（每账号约 10 分钟一次），
 * 所以可能略旧，但页面绝不会被它拖住。要最新值就点「刷新积分」。
 */
const loadSnapshot = async () => {
  try {
    summary.value = await adminAPI.workbuddy.queryCreditsSnapshot()
  } catch (e) {
    error.value = t('admin.accounts.workbuddyCredits.summaryFailed', {
      error: extractErrorMessage(e)
    })
  }
}

const handleCheckinAll = async () => {
  if (checkinRunning.value) return
  checkinRunning.value = true
  error.value = null
  checkinMessage.value = null
  try {
    const res = await adminAPI.workbuddy.checkinAll()
    checkinMessage.value = t('admin.accounts.workbuddyCredits.checkinDone', {
      ok: res.ok,
      already: res.already,
      fail: res.fail,
      skipped: res.skipped
    })
    // 后端在签到后回读汇总；缺失时自行再拉一次。
    if (res.credits) {
      summary.value = res.credits
    } else {
      await loadSummary()
    }
    // 签到后自动展开，展示结果与最新统计。
    collapsed.value = false
  } catch (e) {
    error.value = extractErrorMessage(e) || t('admin.accounts.workbuddyCredits.checkinFailed')
  } finally {
    checkinRunning.value = false
  }
}

/**
 * 读取后台自动查询设置。与快照并行加载、互不阻塞；
 * 失败时保持默认（开启），不打断面板渲染。
 */
const loadSettings = async () => {
  try {
    const s = await adminAPI.workbuddy.getCreditsSettings()
    autoRefreshEnabled.value = s.background_refresh_enabled
  } catch {
    // 设置读取失败不影响积分展示；用户仍可手动切换触发一次写入
  }
}

/**
 * 切换自动查询开关，即时写库。
 * 写失败时回滚开关位置，避免 UI 与后端状态不一致。
 */
const handleAutoRefreshChange = async (value: boolean) => {
  settingsSaving.value = true
  error.value = null
  try {
    const s = await adminAPI.workbuddy.updateCreditsSettings({
      background_refresh_enabled: value
    })
    autoRefreshEnabled.value = s.background_refresh_enabled
  } catch (e) {
    autoRefreshEnabled.value = !value
    error.value = t('admin.accounts.workbuddyCredits.autoRefreshFailed', {
      error: extractErrorMessage(e)
    })
  } finally {
    settingsSaving.value = false
  }
}

// 页面挂载：并行读快照与设置。快照毫秒级返回；实时全量查询交给「查询全部积分」按钮。
onMounted(() => {
  void loadSnapshot()
  void loadSettings()
})

defineExpose({ loadSummary, loadSnapshot, loadSettings })
</script>
