<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { channelLabelKey, useI18n } from '@/i18n'
import { usePaging } from '@/paging'
import type { Model, UpstreamChannel } from '@/types'
import UiIcon from './UiIcon.vue'
import UiPager from './UiPager.vue'

const props = defineProps<{
  models: Model[]
  loading: boolean
  error: string
}>()

const { locale, t } = useI18n()
const query = ref('')
const selectedMethod = ref('')
const selectedChannel = ref<'' | UpstreamChannel>('')
const pricing = ref<'' | 'free' | 'paid'>('')
const expanded = ref<Set<string>>(new Set())

const methods = computed(() =>
  [...new Set(props.models.flatMap((model) => model.methods))].sort((left, right) =>
    left.localeCompare(right),
  ),
)

const filteredModels = computed(() => {
  const term = query.value.trim().toLowerCase()
  return props.models.filter((model) => {
    if (selectedMethod.value !== '' && !model.methods.includes(selectedMethod.value)) return false
    if (selectedChannel.value !== '' && !(model.channels ?? []).includes(selectedChannel.value))
      return false
    if (pricing.value === 'paid' && model.paid !== true) return false
    if (pricing.value === 'free' && model.paid === true) return false
    if (term === '') return true
    return [
      model.id,
      model.name,
      model.description ?? '',
      ...model.methods,
      ...capabilityNames(model),
    ].some((value) => value.toLowerCase().includes(term))
  })
})

const { page, pageSize, pageItems } = usePaging(
  () => filteredModels.value,
  'aistudio2api_models_page_size',
  50,
)

watch([query, selectedMethod, selectedChannel, pricing], () => {
  page.value = 1
})

// capabilityNames 返回模型明确启用的能力名称
function capabilityNames(model: Model): string[] {
  return Object.entries(model.capabilities ?? {})
    .filter(([, enabled]) => enabled)
    .map(([name]) => name)
    .sort((left, right) => left.localeCompare(right))
}

// capabilityOptionEntries 展开模型目录返回的真实能力选项
function capabilityOptionEntries(model: Model): [string, string[]][] {
  return Object.entries(model.capability_options ?? {}).sort(([left], [right]) =>
    left.localeCompare(right),
  )
}

// tokenLimit 使用当前界面语言格式化模型限制
function tokenLimit(value: number | undefined): string {
  if (value === undefined || value === 0) return '—'
  return new Intl.NumberFormat(locale.value).format(value)
}

function isExpanded(model: Model): boolean {
  return expanded.value.has(model.id)
}

function toggleExpanded(model: Model): void {
  const next = new Set(expanded.value)
  if (next.has(model.id)) next.delete(model.id)
  else next.add(model.id)
  expanded.value = next
}

function expandPage(): void {
  const next = new Set(expanded.value)
  for (const model of pageItems.value) next.add(model.id)
  expanded.value = next
}

function collapseAll(): void {
  expanded.value = new Set()
}
</script>

<template>
  <section class="min-h-0 flex-1 overflow-auto">
    <div class="page space-y-4">
      <div class="flex flex-wrap items-baseline gap-3 border-b border-[#30363d] pb-3">
        <h2 class="page-title">{{ t('section.models.title') }}</h2>
        <span class="font-mono text-xs text-gray-500">{{ models.length }}</span>
      </div>

      <div class="flex flex-wrap items-center gap-2">
        <input
          v-model="query"
          class="input min-w-0 flex-1 sm:max-w-96"
          :placeholder="t('models.search')"
          :aria-label="t('models.search')"
          type="search"
        />
        <select v-model="selectedMethod" class="input" :aria-label="t('models.methods')">
          <option value="">{{ t('models.allMethods') }}</option>
          <option v-for="method in methods" :key="method" :value="method">{{ method }}</option>
        </select>
        <select v-model="selectedChannel" class="input" :aria-label="t('models.channels')">
          <option value="">{{ t('models.allChannels') }}</option>
          <option value="playground">{{ t('channel.playground') }}</option>
          <option value="build">{{ t('channel.build') }}</option>
        </select>
        <select v-model="pricing" class="input" :aria-label="t('models.paid')">
          <option value="">{{ t('models.pricingAll') }}</option>
          <option value="free">{{ t('models.pricingFree') }}</option>
          <option value="paid">{{ t('models.pricingPaid') }}</option>
        </select>
        <span class="flex-1"></span>
        <button class="btn btn-sm" type="button" @click="expandPage">
          {{ t('models.expandPage') }}
        </button>
        <button class="btn btn-sm" type="button" @click="collapseAll">
          {{ t('models.collapseAll') }}
        </button>
      </div>

      <div v-if="error" class="rounded border border-red-500/40 bg-red-500/10 p-4 text-red-300">
        {{ error }}
      </div>
      <div v-else-if="loading" class="py-12 text-center text-gray-500">
        {{ t('common.loading') }}
      </div>
      <div v-else class="panel">
        <div class="overflow-x-auto">
          <table class="data-table min-w-[900px]">
            <colgroup>
              <col class="w-9" />
              <col />
              <col class="w-40" />
              <col class="w-[22rem]" />
              <col class="w-28" />
              <col class="w-28" />
            </colgroup>
            <thead>
              <tr>
                <th></th>
                <th>{{ t('models.model') }}</th>
                <th>{{ t('models.channels') }}</th>
                <th>{{ t('models.methods') }}</th>
                <th class="text-right">{{ t('models.context') }}</th>
                <th class="text-right">{{ t('models.output') }}</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="filteredModels.length === 0">
                <td colspan="6" class="py-8 text-center text-gray-500">{{ t('models.empty') }}</td>
              </tr>
              <template v-for="model in pageItems" :key="model.id">
                <tr class="cursor-pointer" @click="toggleExpanded(model)">
                  <td class="text-gray-500">
                    <span
                      class="inline-block transition-transform"
                      :class="isExpanded(model) ? 'rotate-90' : ''"
                    >
                      <UiIcon name="chevronRight" :size="12" />
                    </span>
                  </td>
                  <td>
                    <div class="flex min-w-0 items-center gap-2">
                      <strong class="cell-truncate text-gray-200">{{ model.name }}</strong>
                      <span
                        v-if="model.paid"
                        class="shrink-0 rounded border border-amber-700/60 bg-amber-900/20 px-1.5 text-[10px] leading-4 text-amber-300"
                      >
                        {{ t('models.paid') }}
                      </span>
                    </div>
                    <code class="cell-truncate block text-xs text-blue-400">{{ model.id }}</code>
                  </td>
                  <td>
                    <div class="flex flex-wrap gap-1">
                      <span
                        v-for="channel in model.channels ?? []"
                        :key="channel"
                        class="tag border-green-900/60 text-green-300"
                      >
                        {{ t(channelLabelKey(channel)) }}
                      </span>
                      <span v-if="(model.channels ?? []).length === 0" class="text-gray-600">—</span>
                    </div>
                  </td>
                  <td>
                    <div class="flex flex-wrap gap-1">
                      <span
                        v-for="method in model.methods"
                        :key="method"
                        class="tag border-blue-900/60 text-blue-300"
                      >
                        {{ method }}
                      </span>
                    </div>
                  </td>
                  <td class="text-right font-mono text-xs text-gray-300">
                    {{ tokenLimit(model.input_token_limit) }}
                  </td>
                  <td class="text-right font-mono text-xs text-gray-300">
                    {{ tokenLimit(model.output_token_limit) }}
                  </td>
                </tr>
                <tr v-if="isExpanded(model)">
                  <td></td>
                  <td colspan="5" class="bg-[#0d1117]">
                    <div class="space-y-3 py-2">
                      <p v-if="model.description" class="text-xs leading-5 text-gray-400">
                        {{ model.description }}
                      </p>
                      <div>
                        <span class="mb-1 block text-xs text-gray-500">
                          {{ t('models.capabilities') }}
                        </span>
                        <div class="flex flex-wrap gap-1">
                          <span
                            v-for="capability in capabilityNames(model)"
                            :key="capability"
                            class="tag"
                          >
                            {{ capability }}
                          </span>
                          <span v-if="capabilityNames(model).length === 0" class="text-gray-600">
                            —
                          </span>
                        </div>
                      </div>
                      <div v-if="capabilityOptionEntries(model).length > 0">
                        <span class="mb-1 block text-xs text-gray-500">{{ t('models.options') }}</span>
                        <div class="space-y-1 text-xs">
                          <div v-for="[name, values] in capabilityOptionEntries(model)" :key="name">
                            <code class="text-gray-500">{{ name }}:</code>
                            <span class="ml-1 break-words text-gray-400">{{ values.join(' / ') }}</span>
                          </div>
                        </div>
                      </div>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
        <div class="border-t border-[#30363d] px-3 py-2">
          <UiPager v-model:page="page" v-model:page-size="pageSize" :total="filteredModels.length" />
        </div>
      </div>
    </div>
  </section>
</template>
