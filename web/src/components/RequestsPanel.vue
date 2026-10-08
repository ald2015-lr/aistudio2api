<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '@/api'
import { channelLabelKey, useI18n, type TranslationKey } from '@/i18n'
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

const { locale, t } = useI18n()
const cancelling = ref('')
const refreshing = ref(false)
const cooldownQuery = ref('')
const requestFilter = ref<'all' | RequestState>('all')

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

const activeCount = computed(
  () =>
    props.requests.filter((request) => request.state === 'queued' || request.state === 'running')
      .length,
)

const requestCounts = computed(() => {
  const counts: Record<RequestState, number> = {
    queued: 0,
    running: 0,
    completed: 0,
    cancelled: 0,
    failed: 0,
  }
  for (const request of props.requests) counts[request.state] += 1
  return counts
})

const filteredCooldowns = computed(() => {
  const term = cooldownQuery.value.trim().toLowerCase()
  const list = props.cooldowns.filter(
    (cooldown) =>
      term === '' ||
      [cooldown.model_id, accountLabel(cooldown.account_id, cooldown.account_label)].some((value) =>
        value.toLowerCase().includes(term),
      ),
  )
  return list.sort((left, right) => left.until.localeCompare(right.until))
})

const filteredRequests = computed(() =>
  requestFilter.value === 'all'
    ? props.requests
    : props.requests.filter((request) => request.state === requestFilter.value),
)

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
      <div class="flex items-center justify-between gap-3 border-b border-[#30363d] pb-3">
        <h2 class="page-title">{{ t('section.requests.title') }}</h2>
        <button class="btn btn-sm btn-primary" type="button" @click="refresh">
          <UiIcon name="refresh" :size="13" :class="{ 'animate-spin': refreshing }" />
          {{ t('app.refresh') }}
        </button>
      </div>

      <div class="grid grid-cols-1 gap-4 xl:grid-cols-2">
        <!-- 模型冷却 -->
        <article class="panel flex min-w-0 flex-col">
          <div class="flex flex-wrap items-center gap-2 border-b border-[#30363d] px-3 py-2">
            <h3 class="font-bold text-gray-300">{{ t('cooldowns.title') }}</h3>
            <span class="font-mono text-xs text-gray-500">{{ cooldowns.length }}</span>
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
            {{ t('cooldowns.empty') }}
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
            <span class="flex-1"></span>
            <div class="seg">
              <button
                class="seg-item"
                :class="{ active: requestFilter === 'all' }"
                type="button"
                @click="requestFilter = 'all'"
              >
                {{ t('requests.filterAll') }}
                <span class="seg-count">{{ requests.length }}</span>
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
            {{ t('requests.empty') }}
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
                          v-if="request.pool === 'ultra'"
                          v-tooltip="t('pool.ultraHelp')"
                          class="tag tag-ultra shrink-0"
                          >{{ t('pool.ultra') }}</span
                        >
                        <span class="cell-truncate text-gray-200">{{ request.model || '—' }}</span>
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
