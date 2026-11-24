<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '@/api'
import {
  channelLabelKey,
  poolTagClass,
  poolTagged,
  poolTagHelpKey,
  poolTagLabelKey,
  useI18n,
  type TranslationKey,
} from '@/i18n'
import { usePaging } from '@/paging'
import type { Account, Cooldown, RequestState, RequestSummary } from '@/types'
import UiIcon from './UiIcon.vue'
import UiPager from './UiPager.vue'

const props = defineProps<{
  accounts: Account[]
  cooldowns: Cooldown[]
  requests: RequestSummary[]
  loading: boolean
  cooldownError: string
  requestError: string
}>()

const emit = defineEmits<{
  refresh: []
  notice: [message: string, tone: 'success' | 'error']
}>()

const { locale, t, tf } = useI18n()
const cancelling = ref('')
const refreshing = ref(false)
// accountQuery 为两个列表共用的账户筛选（邮箱或账户 ID 子串，不区分大小写）；cooldownQuery 只在冷却列表里再按模型筛
const accountQuery = ref('')
const cooldownQuery = ref('')
const requestFilter = ref<'all' | RequestState>('all')
// extraOnly 为只看正在由补充池服务的请求：只在列表里有这样的请求（或已经打开）时显示这个开关
const extraOnly = ref(false)

const requestStateKeys: Record<RequestState, TranslationKey> = {
  queued: 'state.queued',
  running: 'state.running',
  completed: 'state.completed',
  cancelled: 'state.cancelled',
  failed: 'state.failed',
}
const requestStates: RequestState[] = ['queued', 'running', 'completed', 'failed', 'cancelled']
const requestStateClass: Record<RequestState, string> = {
  queued: 'text-cyan-300',
  running: 'text-blue-400',
  completed: 'text-green-400',
  cancelled: 'text-yellow-400',
  failed: 'text-red-400',
}

const accountLabels = computed(() => {
  const labels = new Map<string, string>()
  for (const account of props.accounts) labels.set(account.id, account.label)
  return labels
})

// matchesAccount 按邮箱或账户 ID 子串匹配账户筛选（不区分大小写）；没有填写时都匹配
function matchesAccount(id: string, label: string): boolean {
  const term = accountQuery.value.trim().toLowerCase()
  if (term === '') return true
  return accountLabel(id, label).toLowerCase().includes(term) || id.toLowerCase().includes(term)
}

// searchedCooldowns、searchedRequests 为账户筛选之后的两个列表：请求的状态数量、冷却的模型搜索与翻页都基于它们
const searchedCooldowns = computed(() =>
  props.cooldowns.filter((cooldown) => matchesAccount(cooldown.account_id, cooldown.account_label)),
)
const searchedRequests = computed(() =>
  props.requests.filter((request) => matchesAccount(request.account_id, request.account_label)),
)

const activeCount = computed(
  () =>
    searchedRequests.value.filter(
      (request) => request.state === 'queued' || request.state === 'running',
    ).length,
)

const requestCounts = computed(() => {
  const counts: Record<RequestState, number> = {
    queued: 0,
    running: 0,
    completed: 0,
    cancelled: 0,
    failed: 0,
  }
  for (const request of searchedRequests.value) counts[request.state] += 1
  return counts
})

const filteredCooldowns = computed(() => {
  const term = cooldownQuery.value.trim().toLowerCase()
  const list = searchedCooldowns.value.filter(
    (cooldown) => term === '' || cooldown.model_id.toLowerCase().includes(term),
  )
  return list.sort((left, right) => left.until.localeCompare(right.until))
})

const extraCount = computed(() => searchedRequests.value.filter((request) => request.extra).length)
const filteredRequests = computed(() =>
  searchedRequests.value.filter(
    (request) =>
      (requestFilter.value === 'all' || request.state === requestFilter.value) &&
      (!extraOnly.value || request.extra === true),
  ),
)

// accountFiltered 为账户筛选已填写：列表标题旁写出筛选后与全部的条数，列表为空时说明是筛选的结果
const accountFiltered = computed(() => accountQuery.value.trim() !== '')

const cooldownPaging = usePaging(
  () => filteredCooldowns.value,
  'aistudio2api_cooldowns_page_size',
  20,
)
const cooldownPage = cooldownPaging.page
const cooldownPageSize = cooldownPaging.pageSize
const cooldownItems = cooldownPaging.pageItems

const requestPaging = usePaging(() => filteredRequests.value, 'aistudio2api_requests_page_size', 20)
const requestPage = requestPaging.page
const requestPageSize = requestPaging.pageSize
const requestItems = requestPaging.pageItems

watch(cooldownQuery, () => {
  cooldownPage.value = 1
})
watch(accountQuery, () => {
  cooldownPage.value = 1
  requestPage.value = 1
})
watch(requestFilter, () => {
  requestPage.value = 1
})

// accountLabel 将稳定账户 ID 映射为用户显示名称
function accountLabel(id: string, label: string): string {
  return label || accountLabels.value.get(id) || '—'
}

// formatTime 根据当前语言显示时间
function formatTime(value: string): string {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return new Intl.DateTimeFormat(locale.value, {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
  }).format(date)
}

// refresh 请求刷新数据，图标短暂旋转确认点击
function refresh(): void {
  refreshing.value = true
  emit('refresh')
  window.setTimeout(() => (refreshing.value = false), 600)
}

// cancelRequest 停止活动请求并刷新摘要
async function cancelRequest(request: RequestSummary): Promise<void> {
  cancelling.value = request.id
  try {
    await api.cancelRequest(request.id)
    emit('refresh')
  } catch (error) {
    emit('notice', error instanceof Error ? error.message : t('common.error'), 'error')
  } finally {
    cancelling.value = ''
  }
}
</script>

<template>
  <section class="min-h-0 flex-1 overflow-auto">
    <div class="page space-y-4">
      <div
        class="flex flex-wrap items-center justify-between gap-x-3 gap-y-2 border-b border-[#30363d] pb-3"
      >
        <h2 class="page-title">{{ t('section.requests.title') }}</h2>
        <div class="flex w-full min-w-0 items-center gap-2 sm:w-auto">
          <input
            v-model="accountQuery"
            class="input h-[1.75rem] min-w-0 flex-1 text-xs sm:w-64 sm:flex-none"
            type="search"
            :placeholder="t('requests.accountFilter')"
            :aria-label="t('requests.accountFilter')"
          />
          <button class="btn btn-sm btn-primary shrink-0" type="button" @click="refresh">
            <UiIcon name="refresh" :size="13" :class="{ 'animate-spin': refreshing }" />
            {{ t('app.refresh') }}
          </button>
        </div>
      </div>

      <div class="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <!-- 模型冷却 -->
        <article class="panel flex min-w-0 flex-col">
          <div class="flex flex-wrap items-center gap-2 border-b border-[#30363d] px-3 py-2">
            <h3 class="font-bold text-gray-300">{{ t('cooldowns.title') }}</h3>
            <span class="font-mono text-xs text-gray-500">{{
              accountFiltered || cooldownQuery.trim() !== ''
                ? `${filteredCooldowns.length} / ${cooldowns.length}`
                : cooldowns.length
            }}</span>
            <span class="flex-1"></span>
            <input
              v-model="cooldownQuery"
              class="input h-[1.75rem] w-56 text-xs"
              type="search"
              :placeholder="t('cooldowns.search')"
              :aria-label="t('cooldowns.search')"
            />
          </div>
          <div v-if="cooldownError" class="m-3 rounded border border-red-500/40 bg-red-500/10 p-3 text-red-300">
            {{ cooldownError }}
          </div>
          <div v-else-if="loading" class="py-8 text-center text-gray-500">
            {{ t('common.loading') }}
          </div>
          <div v-else-if="filteredCooldowns.length === 0" class="py-8 text-center text-xs text-gray-600">
            {{
              cooldowns.length > 0 && (accountFiltered || cooldownQuery.trim() !== '')
                ? t('cooldowns.noMatch')
                : t('cooldowns.empty')
            }}
          </div>
          <template v-else>
            <div class="overflow-x-auto">
              <table class="data-table min-w-[560px]">
                <colgroup>
                  <col />
                  <col class="w-48" />
                  <col class="w-28" />
                </colgroup>
                <thead>
                  <tr>
                    <th>{{ t('requests.model') }} / {{ t('requests.account') }}</th>
                    <th>{{ t('cooldowns.reason') }}</th>
                    <th class="text-right">{{ t('cooldowns.until') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="cooldown in cooldownItems"
                    :key="`${cooldown.account_id}:${cooldown.channel}:${cooldown.model_id}`"
                  >
                    <td>
                      <div class="cell-truncate text-gray-200">{{ cooldown.model_id }}</div>
                      <div class="cell-truncate text-xs text-gray-500">
                        {{ accountLabel(cooldown.account_id, cooldown.account_label) }} ·
                        {{ t(channelLabelKey(cooldown.channel)) }}
                      </div>
                    </td>
                    <td>
                      <div v-tooltip="cooldown.reason ?? ''" class="cell-truncate text-xs text-gray-400">
                        {{ cooldown.reason || '—' }}
                      </div>
                    </td>
                    <td class="text-right font-mono text-xs text-yellow-400">
                      {{ formatTime(cooldown.until) }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <div class="border-t border-[#30363d] px-3 py-2">
              <UiPager
                v-model:page="cooldownPage"
                v-model:page-size="cooldownPageSize"
                :total="filteredCooldowns.length"
              />
            </div>
          </template>
        </article>

        <!-- 请求摘要 -->
        <article class="panel flex min-w-0 flex-col">
          <div class="flex flex-wrap items-center gap-2 border-b border-[#30363d] px-3 py-2">
            <h3 class="font-bold text-gray-300">{{ t('requests.history') }}</h3>
            <span class="text-xs text-green-400">{{ t('requests.live') }}: {{ activeCount }}</span>
            <span v-if="accountFiltered" class="font-mono text-xs text-gray-500"
              >{{ searchedRequests.length }} / {{ requests.length }}</span
            >
            <span class="flex-1"></span>
            <button
              v-if="extraOnly || extraCount > 0"
              v-tooltip="t('requests.filterExtraHelp')"
              class="btn btn-sm"
              :class="extraOnly ? 'border-emerald-500/50 text-emerald-300' : ''"
              type="button"
              :aria-pressed="extraOnly"
              data-testid="requests-extra-filter"
              @click="extraOnly = !extraOnly"
            >
              {{ t('requests.filterExtra') }}
              <span class="seg-count">{{ extraCount }}</span>
            </button>
            <div class="seg">
              <button
                class="seg-item"
                :class="{ active: requestFilter === 'all' }"
                type="button"
                @click="requestFilter = 'all'"
              >
                {{ t('requests.filterAll') }}
                <span class="seg-count">{{ searchedRequests.length }}</span>
              </button>
              <button
                v-for="state in requestStates"
                :key="state"
                class="seg-item"
                :class="{ active: requestFilter === state }"
                type="button"
                @click="requestFilter = state"
              >
                {{ t(requestStateKeys[state]) }}
                <span class="seg-count">{{ requestCounts[state] }}</span>
              </button>
            </div>
          </div>
          <div v-if="requestError" class="m-3 rounded border border-red-500/40 bg-red-500/10 p-3 text-red-300">
            {{ requestError }}
          </div>
          <div v-else-if="loading" class="py-8 text-center text-gray-500">
            {{ t('common.loading') }}
          </div>
          <div v-else-if="filteredRequests.length === 0" class="py-8 text-center text-xs text-gray-600">
            {{ accountFiltered && requests.length > 0 ? t('requests.noMatch') : t('requests.empty') }}
          </div>
          <template v-else>
            <div class="overflow-x-auto">
              <table class="data-table min-w-[560px]">
                <colgroup>
                  <col />
                  <col class="w-24" />
                  <col class="w-32" />
                  <col class="w-20" />
                </colgroup>
                <thead>
                  <tr>
                    <th>{{ t('requests.model') }} / {{ t('requests.account') }}</th>
                    <th>{{ t('accounts.state') }}</th>
                    <th>{{ t('requests.started') }}</th>
                    <th class="text-right">{{ t('requests.action') }}</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="request in requestItems" :key="request.id">
                    <td>
                      <div class="flex min-w-0 items-center gap-1.5">
                        <span
                          v-if="poolTagged(request.pool)"
                          v-tooltip="t(poolTagHelpKey(request.pool))"
                          class="tag shrink-0"
                          :class="poolTagClass(request.pool)"
                          >{{ t(poolTagLabelKey(request.pool)) }}</span
                        >
                        <span
                          v-if="request.extra"
                          v-tooltip="t('pool.extraHelp')"
                          class="tag tag-extra shrink-0"
                          >{{ t('pool.extra') }}</span
                        >
                        <span class="cell-truncate text-gray-200">{{ request.model || '—' }}</span>
                        <span
                          v-if="request.fallback_from"
                          v-tooltip="tf('logs.fallbackHelp', { model: request.fallback_from })"
                          class="tag tag-fallback shrink-0"
                          >{{ t('logs.fallback') }}</span
                        >
                      </div>
                      <div class="cell-truncate text-xs text-gray-500">
                        {{ accountLabel(request.account_id, request.account_label)
                        }}<template v-if="request.channel">
                          · {{ t(channelLabelKey(request.channel)) }}</template
                        >
                      </div>
                    </td>
                    <td class="text-xs" :class="requestStateClass[request.state]">
                      {{ t(requestStateKeys[request.state]) }}
                    </td>
                    <td class="font-mono text-xs text-gray-400">{{ formatTime(request.started_at) }}</td>
                    <td class="text-right">
                      <button
                        v-if="request.state === 'queued' || request.state === 'running'"
                        class="btn btn-sm btn-danger"
                        type="button"
                        :disabled="cancelling !== ''"
                        :aria-busy="cancelling === request.id"
                        @click="cancelRequest(request)"
                      >
                        {{ t('requests.stop') }}
                      </button>
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <div class="border-t border-[#30363d] px-3 py-2">
              <UiPager
                v-model:page="requestPage"
                v-model:page-size="requestPageSize"
                :total="filteredRequests.length"
              />
            </div>
          </template>
        </article>
      </div>
    </div>
  </section>
</template>
