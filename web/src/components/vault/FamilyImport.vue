<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { VaultApiError, api } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import type { Account, FamilyBlockResult, FamilyImportResult } from '@/types'
import {
  SECRET_INPUT_ATTRS,
  famClass,
  familyDraft,
  familyImportDone,
  familyPreview,
  groupById,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
  type VaultSubTab,
} from '@/vault'
import FamilyChanges from './FamilyChanges.vue'
import FormatText from './FormatText.vue'
import UiIcon from '../UiIcon.vue'
import UiPager from '../UiPager.vue'

// FamilyImport 为录入家庭组：按块（空行或 # 行分块，每块第一个邮箱是主邮箱）或每行一组 → 服务端预览（逐块结果，每页 50 块，
// 可按拒绝 / 无法识别 / 有提示 / 有改动筛选）→ 确认（带预览的版本：期间家庭组被别处改过则 409，换成新预览）。
// 每块原子生效；被拒绝或无法识别的块不改动任何组。草稿只在内存，录入成功后清空
// accountsLoading 为账户列表还不知道（载入中或载入失败）：这时不标“未存在”
const props = defineProps<{ byEmail: ReadonlyMap<string, Account[]>; accountsLoading?: boolean }>()
const emit = defineEmits<{ goto: [tab: VaultSubTab] }>()

const { t, tf } = useI18n()
const PAGE = 50
const busy = ref('')
const error = ref('')
const filter = ref('all')
const page = ref(1)
const pageSize = ref(PAGE)

const result = computed<FamilyImportResult | null>(() => familyPreview.value?.result ?? null)
const stale = computed(
  () => familyPreview.value !== null && familyPreview.value.version !== familyDraft.version,
)

function bump(): void {
  familyDraft.version += 1
  familyImportDone.value = null
}

function clearDraft(): void {
  familyDraft.text = ''
  familyDraft.ack = false
  familyPreview.value = null
  error.value = ''
  bump()
}

async function runPreview(): Promise<void> {
  if (familyDraft.text.trim() === '' || busy.value !== '') return
  busy.value = 'preview'
  error.value = ''
  const text = familyDraft.text
  const lines = familyDraft.lines
  try {
    const preview = await api.importFamilies({
      dry_run: true,
      text,
      mode: lines ? 'lines' : 'blocks',
    })
    familyPreview.value = { result: preview, version: familyDraft.version, text, lines }
    familyDraft.ack = false
    filter.value = 'all'
    page.value = 1
    familyImportDone.value = null
  } catch (reason) {
    error.value = vaultErrorText(reason)
  } finally {
    busy.value = ''
  }
}

const suspect = computed(() => result.value?.summary.suspect ?? [])
const applicable = computed(() => {
  const summary = result.value?.summary
  if (!summary) return 0
  return summary.created + summary.replaced + summary.primary_changed
})
const canConfirm = computed(
  () =>
    result.value !== null &&
    !stale.value &&
    busy.value === '' &&
    applicable.value > 0 &&
    (suspect.value.length === 0 || familyDraft.ack),
)

async function confirmImport(): Promise<void> {
  const preview = familyPreview.value
  if (preview === null || !canConfirm.value) return
  busy.value = 'confirm'
  error.value = ''
  try {
    const input: Parameters<typeof api.importFamilies>[0] = {
      dry_run: false,
      text: preview.text,
      mode: preview.lines ? 'lines' : 'blocks',
      base_revision: preview.result.revision,
    }
    if (familyDraft.ack) input.acknowledge_suspect = true
    const done = await vaultWrite(api.importFamilies(input))
    familyImportDone.value = done
    familyPreview.value = null
    familyDraft.text = ''
    familyDraft.ack = false
    familyDraft.version += 1
    vaultNotice(confirmText(done, 'family.importDone'))
  } catch (reason) {
    if (reason instanceof VaultApiError && reason.code === 'vault_conflict' && reason.preview) {
      familyPreview.value = {
        result: reason.preview as FamilyImportResult,
        version: familyDraft.version,
        text: preview.text,
        lines: preview.lines,
      }
      familyDraft.ack = false
      error.value = t('family.conflictRefreshed')
    } else {
      error.value = vaultErrorText(reason)
    }
  } finally {
    busy.value = ''
  }
}

// confirmText 为“确认录入（新建 N、替换 N、换主 N；移动 N 个邮箱、移出 N 个、解散 N 个组）”，为 0 的项不写
function confirmText(value: FamilyImportResult, key: TranslationKey): string {
  const summary = value.summary
  const head = [
    summary.created > 0 ? tf('family.countCreated', { count: summary.created }) : '',
    summary.replaced > 0 ? tf('family.countReplaced', { count: summary.replaced }) : '',
    summary.primary_changed > 0
      ? tf('family.countPrimaryChanged', { count: summary.primary_changed })
      : '',
  ].filter(Boolean)
  const tail = [
    summary.moved > 0 ? tf('family.countMoved', { count: summary.moved }) : '',
    summary.removed > 0 ? tf('family.countRemoved', { count: summary.removed }) : '',
    summary.dissolved > 0 ? tf('family.countDissolved', { count: summary.dissolved }) : '',
  ].filter(Boolean)
  const list = t('common.listSep')
  const detail = [head.join(list), tail.join(list)].filter(Boolean).join(t('common.clauseSep'))
  return tf(key, { detail: detail === '' ? t('family.nothingToApply') : detail })
}

type BlockFilter = 'all' | 'rejected' | 'invalid' | 'warnings' | 'changed' | string
function matches(block: FamilyBlockResult, value: BlockFilter): boolean {
  switch (value) {
    case 'all':
      return true
    case 'rejected':
      return block.status === 'rejected'
    case 'invalid':
      return block.status === 'invalid'
    case 'warnings':
      return (block.warnings?.length ?? 0) > 0
    case 'changed':
      return ['created', 'replaced', 'primary_changed', 'updated'].includes(block.status)
    default:
      return block.status === value
  }
}

const blocks = computed(() =>
  (result.value?.blocks ?? []).filter((block) => matches(block, filter.value)),
)
const pageBlocks = computed(() => blocks.value.slice((page.value - 1) * PAGE, page.value * PAGE))
watch(filter, () => {
  page.value = 1
})

const tags = computed(() => {
  const summary = result.value?.summary
  if (!summary) return []
  const warned = (result.value?.blocks ?? []).filter(
    (block) => (block.warnings?.length ?? 0) > 0,
  ).length
  return [
    {
      key: 'created',
      label: tf('family.tagCreated', { count: summary.created }),
      cls: 'text-green-300',
      count: summary.created,
    },
    {
      key: 'replaced',
      label: tf('family.tagReplaced', { count: summary.replaced }),
      cls: 'text-blue-300',
      count: summary.replaced,
    },
    {
      key: 'primary_changed',
      label: tf('family.tagPrimaryChanged', { count: summary.primary_changed }),
      cls: 'text-fuchsia-300',
      count: summary.primary_changed,
    },
    {
      key: 'unchanged',
      label: tf('family.tagUnchanged', { count: summary.unchanged }),
      cls: 'text-gray-300',
      count: summary.unchanged,
    },
    {
      key: 'rejected',
      label: tf('family.tagRejected', { count: summary.rejected }),
      cls: 'text-red-300',
      count: summary.rejected,
    },
    {
      key: 'invalid',
      label: tf('family.tagInvalid', { count: summary.invalid }),
      cls: 'text-red-300',
      count: summary.invalid,
    },
    {
      key: 'warnings',
      label: tf('family.tagWarnings', { count: warned }),
      cls: 'text-amber-300',
      count: warned,
    },
  ].filter((tag) => tag.count > 0 || ['created', 'rejected'].includes(tag.key))
})

function batchText(entry: { code: string; email: string; blocks: number[] }): string {
  const key: TranslationKey =
    entry.code === 'primary_repeated'
      ? 'family.batch.primary_repeated'
      : 'family.batch.email_in_blocks'
  return tf(key, { email: entry.email, blocks: entry.blocks.join(t('common.listSep')) })
}

function blockColor(block: FamilyBlockResult): number | undefined {
  if (block.color !== undefined) return block.color
  return block.group_id ? groupById.value.get(block.group_id)?.color : undefined
}

function missingIn(block: FamilyBlockResult): number {
  if (props.accountsLoading) return 0
  return [block.primary ?? '', ...(block.members ?? [])].filter(
    (email) => email !== '' && !props.byEmail.has(email),
  ).length
}

const done = computed(() => familyImportDone.value)
</script>

<template>
  <div class="space-y-4">
    <div class="grid gap-4 lg:grid-cols-[minmax(0,1fr)_22rem]">
      <section class="panel space-y-3 p-4">
        <label class="block">
          <span class="mb-1.5 block text-xs text-gray-400">{{ t('family.importLabel') }}</span>
          <textarea
            v-model="familyDraft.text"
            class="input h-auto min-h-[14rem] w-full resize-y py-2 font-mono text-xs leading-5"
            rows="12"
            :placeholder="t('family.importPlaceholder')"
            v-bind="SECRET_INPUT_ATTRS"
            data-testid="family-import-text"
            @input="bump"
          ></textarea>
        </label>
        <div class="flex flex-wrap items-center gap-3 border-t border-line pt-3">
          <label class="flex items-center gap-2 text-xs text-gray-300">
            <input
              v-model="familyDraft.lines"
              type="checkbox"
              data-testid="family-mode-lines"
              @change="bump"
            />
            {{ t('family.modeLines') }}
          </label>
          <span class="flex-1"></span>
          <button
            class="btn"
            type="button"
            :disabled="familyDraft.text === '' && !familyPreview"
            @click="clearDraft"
          >
            {{ t('vault.clearDraft') }}
          </button>
          <button
            class="btn btn-primary"
            type="button"
            data-testid="family-preview"
            :disabled="familyDraft.text.trim() === '' || busy !== ''"
            :aria-busy="busy === 'preview'"
            @click="runPreview"
          >
            {{ t('family.preview') }}
          </button>
        </div>
        <p
          v-if="error"
          class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-300"
          role="alert"
          data-testid="family-import-error"
        >
          {{ error }}
        </p>
      </section>
      <aside class="panel p-4 text-xs leading-5 text-gray-400">
        <details open>
          <summary
            class="mb-2 flex cursor-pointer items-center gap-2 text-sm font-semibold text-gray-200"
          >
            <UiIcon name="info" :size="14" class="text-blue-300" />
            {{ t('vault.formatTitle') }}
          </summary>
          <ul class="list-disc space-y-1.5 pl-4">
            <li>{{ t('family.help.blocks') }}</li>
            <li><FormatText :text="t('family.help.inline')" /></li>
            <li>{{ t('family.help.name') }}</li>
            <li>{{ t('family.help.conflict') }}</li>
            <li>{{ t('family.help.limits') }}</li>
          </ul>
          <p class="mt-2 text-gray-500">{{ t('vault.help.examples') }}</p>
          <pre
            class="mt-1 overflow-x-auto rounded border border-line bg-canvas p-2 font-mono text-[11px] leading-5 text-gray-300"
          >
# {{ t('family.help.nameExample') }}
main@example.com
m1@example.com
m2@example.com

x@example.com, y@example.com, z@example.com</pre>
        </details>
      </aside>
    </div>

    <section
      v-if="done"
      class="panel space-y-2 border-green-500/40 p-4 text-sm"
      data-testid="family-import-done"
    >
      <p class="flex items-center gap-2 font-semibold text-green-300">
        <UiIcon name="check" :size="14" />
        {{ confirmText(done, 'family.importDone') }}
      </p>
      <p v-if="done.summary.rejected + done.summary.invalid > 0" class="text-xs text-red-300">
        {{ tf('family.doneSkipped', { count: done.summary.rejected + done.summary.invalid }) }}
      </p>
      <button
        class="text-xs text-blue-300 hover:text-blue-200"
        type="button"
        @click="emit('goto', 'families')"
      >
        {{ t('family.viewCards') }} ›
      </button>
    </section>

    <section v-if="result" class="space-y-3" data-testid="family-import-preview">
      <div
        v-if="stale"
        class="rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
      >
        {{ t('vault.previewStale') }}
      </div>
      <div
        v-if="suspect.length > 0"
        class="space-y-2 rounded-md border border-red-500/50 bg-red-500/10 px-3 py-2.5 text-xs text-red-200"
        data-testid="family-suspect"
      >
        <p class="flex items-center gap-1.5 font-semibold text-red-300">
          <UiIcon name="warning" :size="14" />
          {{ tf('family.suspect', { count: result.summary.warnings?.maybe_missing_blank ?? 0 }) }}
        </p>
        <label class="flex items-center gap-2 font-medium text-red-100">
          <input v-model="familyDraft.ack" type="checkbox" data-testid="family-ack" />
          {{ t('vault.ackSuspect') }}
        </label>
      </div>
      <ul
        v-if="(result.batch_warnings?.length ?? 0) > 0"
        class="space-y-1 rounded-md border border-amber-500/40 bg-amber-500/10 px-3 py-2 text-xs text-amber-200"
        data-testid="family-batch-warnings"
      >
        <li v-for="entry in result.batch_warnings ?? []" :key="`${entry.code}:${entry.email}`">
          {{ batchText(entry) }}
        </li>
      </ul>
      <div class="panel space-y-3 p-3">
        <div class="flex flex-wrap items-center gap-1.5 text-xs">
          <button
            v-for="tag in tags"
            :key="tag.key"
            class="tag font-sans transition hover:border-line-strong"
            :class="[tag.cls, filter === tag.key ? 'border-blue-500 bg-blue-500/15' : '']"
            type="button"
            :aria-pressed="filter === tag.key"
            :data-tag="tag.key"
            @click="filter = filter === tag.key ? 'all' : tag.key"
          >
            {{ tag.label }}
          </button>
          <button
            class="tag font-sans text-gray-300"
            :class="filter === 'changed' ? 'border-blue-500 bg-blue-500/15' : ''"
            type="button"
            @click="filter = filter === 'changed' ? 'all' : 'changed'"
          >
            {{ t('family.tagChanged') }}
          </button>
          <span class="ml-auto text-gray-500">{{
            tf('family.previewMoves', {
              moved: result.summary.moved,
              removed: result.summary.removed,
              dissolved: result.summary.dissolved,
            })
          }}</span>
        </div>
        <div class="grid gap-2 md:grid-cols-2">
          <article
            v-for="block in pageBlocks"
            :key="block.index"
            class="min-w-0 rounded-md border border-line bg-canvas p-2.5"
            :class="[
              blockColor(block) !== undefined ? ['fam-bar', famClass(blockColor(block))] : '',
              ['rejected', 'invalid'].includes(block.status) ? 'border-red-500/40' : '',
            ]"
            data-testid="family-block"
            :data-status="block.status"
          >
            <header class="mb-1.5 flex min-w-0 items-center gap-2 text-xs">
              <span v-if="blockColor(block) !== undefined" class="fam-dot"></span>
              <span class="font-mono text-gray-500">{{
                block.first_line !== undefined && block.first_line === block.last_line
                  ? tf('family.blockLine', { index: block.index, line: block.first_line })
                  : tf('family.blockLines', {
                      index: block.index,
                      from: block.first_line ?? '?',
                      to: block.last_line ?? '?',
                    })
              }}</span>
              <span v-if="block.name" class="truncate font-semibold text-gray-200">{{
                block.name
              }}</span>
              <span
                v-if="block.primary"
                class="cell-truncate min-w-0 flex-1 font-mono text-gray-100"
              >
                <UiIcon name="crown" :size="11" class="text-amber-300" /> {{ block.primary }}
              </span>
              <span v-if="missingIn(block) > 0" class="tag tag-missing shrink-0">{{
                tf('accounts.familyHeadMissing', { count: missingIn(block) })
              }}</span>
            </header>
            <div v-if="(block.members?.length ?? 0) > 0" class="mb-1.5 flex flex-wrap gap-1">
              <span v-for="email in block.members ?? []" :key="email" class="tag text-gray-300">{{
                email
              }}</span>
            </div>
            <FamilyChanges :result="block" />
          </article>
        </div>
        <UiPager
          v-if="blocks.length > PAGE"
          v-model:page="page"
          v-model:page-size="pageSize"
          :total="blocks.length"
          fixed-size
        />
        <div class="flex flex-wrap items-center justify-end gap-2 border-t border-line pt-3">
          <button
            class="btn btn-primary"
            type="button"
            data-testid="family-confirm-import"
            :disabled="!canConfirm"
            :aria-busy="busy === 'confirm'"
            @click="confirmImport"
          >
            {{ confirmText(result, 'family.confirmImport') }}
          </button>
        </div>
      </div>
    </section>
  </div>
</template>
