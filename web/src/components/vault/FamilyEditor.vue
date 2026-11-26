<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, shallowRef } from 'vue'
import { VaultApiError, api } from '@/api'
import { useI18n } from '@/i18n'
import type { FamilyBlockResult, FamilyCreateInput, FamilyGroup, FamilyUpdateInput } from '@/types'
import {
  PALETTE_SIZE,
  famClass,
  familiesBySeq,
  groupById,
  groupMembers,
  normEmail,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
} from '@/vault'
import ColorSwatches from './ColorSwatches.vue'
import FamilyChanges from './FamilyChanges.vue'
import UiIcon from '../UiIcon.vue'

// FamilyEditor 为新建或编辑家庭组的弹窗：名称、颜色、主邮箱、成员（一行一个，也接受同一行用 ----、逗号、顿号或空格分隔）。
// 保存先按 dry-run 预览：有移动、移出、自动解散、换主或拒绝时在框内列出结果，按钮变成“确认保存”，确认时带预览的版本（期间
// 家庭组被别处改过则 409，换成新预览）。新建时主邮箱已在某个组里（预览为替换、换主或未变化）不会新建：框内说明将更新那个
// 已有家庭组、填写的名称与颜色不采用，确认后的提示也照实写。编辑时成员删到为空 = 解散（按钮变成“解散家庭组”）。弹窗里不叠确认框
const props = defineProps<{ group?: FamilyGroup | undefined }>()
const emit = defineEmits<{ close: [] }>()

const { t, tf } = useI18n()
const editing = computed(() => props.group !== undefined)
const name = ref(props.group?.name ?? '')
// color 为 null 时新建的组由服务端自动选色（避开上一组的颜色）
const color = ref<number | null>(props.group?.color ?? null)
const primary = ref(props.group?.primary ?? '')
const membersText = ref((props.group?.members ?? []).join('\n'))
const preview = shallowRef<FamilyBlockResult | null>(null)
const saving = ref(false)
const error = ref('')

function parseMembers(text: string): string[] {
  const seen = new Set<string>()
  const list: string[] = []
  for (const token of text.split(/[\s,，;；、]+|[-‐‑‒–−﹣－—―﹘]{4,}/)) {
    const email = normEmail(token)
    if (email === '' || seen.has(email)) continue
    seen.add(email)
    list.push(email)
  }
  return list
}

const members = computed(() => parseMembers(membersText.value))
const willDissolve = computed(() => editing.value && members.value.length === 0)

const usage = computed(() => {
  const counts = Array.from({ length: PALETTE_SIZE }, () => 0)
  for (const group of familiesBySeq.value) {
    if (group.id === props.group?.id) continue
    counts[group.color % PALETTE_SIZE] = (counts[group.color % PALETTE_SIZE] ?? 0) + 1
  }
  return counts
})

// 上一组 / 下一组：按创建顺序与这个组相邻的组（新建的组排在最后，只有上一组）
const neighbors = computed(() => {
  const list = familiesBySeq.value
  if (!editing.value) return { prev: list[list.length - 1]?.color, next: undefined }
  const index = list.findIndex((group) => group.id === props.group?.id)
  return { prev: list[index - 1]?.color, next: list[index + 1]?.color }
})

function sameList(left: readonly string[], right: readonly string[]): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index])
}

function createInput(dryRun: boolean, baseRevision?: string): FamilyCreateInput {
  const input: FamilyCreateInput = {
    dry_run: dryRun,
    primary: normEmail(primary.value),
    members: members.value,
  }
  if (name.value.trim() !== '') input.name = name.value.trim()
  if (color.value !== null) input.color = color.value
  if (baseRevision !== undefined) input.base_revision = baseRevision
  return input
}

function updateInput(dryRun: boolean, baseRevision?: string): FamilyUpdateInput {
  const group = props.group!
  const input: FamilyUpdateInput = { dry_run: dryRun, rev: group.rev }
  if (name.value.trim() !== (group.name ?? '')) input.name = name.value.trim()
  if (color.value !== null && color.value !== group.color) input.color = color.value
  const nextPrimary = normEmail(primary.value)
  if (nextPrimary !== group.primary) input.primary = nextPrimary
  if (!sameList(members.value, groupMembers(group))) input.members = members.value
  if (baseRevision !== undefined) input.base_revision = baseRevision
  return input
}

function request(dryRun: boolean, baseRevision?: string): Promise<FamilyBlockResult> {
  return editing.value
    ? api.updateFamily(props.group!.id, updateInput(dryRun, baseRevision))
    : api.createFamily(createInput(dryRun, baseRevision))
}

// landsOnExisting 为新建时预览落到了已有家庭组（主邮箱已是某组的主邮箱，或换主）：不会新建，名称与颜色沿用原组
function landsOnExisting(result: FamilyBlockResult): boolean {
  return !editing.value && ['replaced', 'primary_changed', 'unchanged'].includes(result.status)
}

// needsConfirm 为预览有需要用户看一眼的结果：移动、移出、自动解散、换主、新建落到已有家庭组
function needsConfirm(result: FamilyBlockResult): boolean {
  return (
    (result.moved?.length ?? 0) > 0 ||
    (result.removed?.length ?? 0) > 0 ||
    (result.dissolved?.length ?? 0) > 0 ||
    result.status === 'primary_changed' ||
    landsOnExisting(result)
  )
}

function groupText(group: { name?: string | undefined; primary: string }): string {
  return group.name
    ? tf('family.groupNamed', { name: group.name, primary: group.primary })
    : group.primary
}

// metaIgnored 为新建落到已有家庭组时填写了与原组不同的名称或颜色（不会采用）
function metaIgnored(existing: FamilyGroup | undefined): boolean {
  const typed = name.value.trim()
  return (
    (typed !== '' && typed !== (existing?.name ?? '')) ||
    (color.value !== null && color.value !== existing?.color)
  )
}

// existingNote 为新建落到已有家庭组时框内的说明（预览时的已有组）
const existingNote = computed(() => {
  const result = preview.value
  if (result === null || !landsOnExisting(result)) return null
  const existing = result.group_id === undefined ? undefined : groupById.value.get(result.group_id)
  return {
    text: tf('family.createExisting', {
      group: existing ? groupText(existing) : (result.primary ?? ''),
    }),
    meta: metaIgnored(existing),
  }
})

// savedText 为保存后的提示：按结果写，新建落到已有家庭组时写明更新的是哪个组、名称与颜色是否没有采用
function savedText(result: FamilyBlockResult, ignored: boolean): string {
  if (result.status === 'unchanged' && editing.value) return t('family.savedUnchanged')
  if (!editing.value && result.status !== 'created') {
    const group = groupText(result.group ?? { name: result.name, primary: result.primary ?? '' })
    const text = tf(
      result.status === 'unchanged'
        ? 'family.existingUnchangedNotice'
        : 'family.updatedExistingNotice',
      { group },
    )
    return ignored ? text + t('family.metaIgnoredSuffix') : text
  }
  return t(editing.value ? 'family.savedNotice' : 'family.createdNotice')
}

const rejected = computed(
  () => preview.value !== null && ['rejected', 'invalid'].includes(preview.value.status),
)

async function apply(baseRevision: string): Promise<void> {
  const result = await vaultWrite(request(false, baseRevision))
  // 落到已有家庭组时名称与颜色不采用，写完之后那个组的名称、颜色不变，按写完的组判断有没有被忽略即可
  const target = result.group_id === undefined ? undefined : groupById.value.get(result.group_id)
  vaultNotice(savedText(result, landsOnExisting(result) && metaIgnored(target)))
  emit('close')
}

async function save(): Promise<void> {
  if (saving.value) return
  saving.value = true
  error.value = ''
  try {
    if (willDissolve.value) {
      await vaultWrite(api.dissolveFamily(props.group!.id, props.group!.rev))
      vaultNotice(t('family.dissolvedNotice'))
      emit('close')
      return
    }
    if (preview.value !== null && !rejected.value && needsConfirm(preview.value)) {
      await apply(preview.value.revision ?? '')
      return
    }
    const result = await request(true)
    if (['rejected', 'invalid'].includes(result.status) || needsConfirm(result)) {
      preview.value = result
      return
    }
    await apply(result.revision ?? '')
  } catch (reason) {
    if (reason instanceof VaultApiError && reason.code === 'vault_conflict' && reason.preview) {
      preview.value = reason.preview as FamilyBlockResult
      vaultNotice(t('family.conflictRefreshed'), 'error')
    } else {
      error.value = vaultErrorText(reason)
    }
  } finally {
    saving.value = false
  }
}

// 改了任何输入，预览作废
function touched(): void {
  preview.value = null
  error.value = ''
}

const saveLabel = computed(() => {
  if (willDissolve.value) return t('family.dissolveButton')
  if (preview.value !== null && !rejected.value && needsConfirm(preview.value))
    return t('family.confirmSave')
  return t('common.save')
})

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') emit('close')
}
onMounted(() => document.addEventListener('keydown', onKeydown))
onBeforeUnmount(() => document.removeEventListener('keydown', onKeydown))
</script>

<template>
  <Teleport to="body">
    <div class="modal-overlay" data-testid="family-editor" @click.self="emit('close')">
      <div
        class="mx-4 max-h-[92vh] w-full max-w-lg overflow-y-auto rounded-lg border border-line bg-panel p-5 shadow-xl sm:p-6"
        :class="color !== null ? famClass(color) : ''"
        role="dialog"
        aria-modal="true"
        :aria-label="t(editing ? 'family.edit' : 'family.create')"
        :style="
          color !== null ? { borderTopColor: 'var(--fam)', borderTopWidth: '3px' } : undefined
        "
      >
        <div class="mb-4 flex items-center justify-between gap-3">
          <h3 class="flex items-center gap-2 text-lg font-bold text-white">
            <UiIcon name="users" :size="16" />
            {{ t(editing ? 'family.edit' : 'family.create') }}
          </h3>
          <button
            class="rounded p-1 text-gray-500 hover:bg-raised hover:text-white"
            type="button"
            :aria-label="t('common.close')"
            @click="emit('close')"
          >
            <UiIcon name="close" :size="16" />
          </button>
        </div>
        <div class="space-y-3 text-sm">
          <label class="block">
            <span class="mb-1 block text-xs text-gray-400">{{ t('family.name') }}</span>
            <input
              v-model="name"
              class="input w-full"
              type="text"
              maxlength="40"
              :placeholder="t('family.namePlaceholder')"
              autocomplete="off"
              data-field="name"
              @input="touched"
            />
          </label>
          <div>
            <div class="mb-1 flex items-center justify-between text-xs">
              <span class="text-gray-400">{{ t('family.color') }}</span>
              <button
                v-if="!editing && color !== null"
                class="text-blue-400 hover:text-blue-300"
                type="button"
                @click="((color = null), touched())"
              >
                {{ t('family.colorAuto') }}
              </button>
            </div>
            <ColorSwatches
              :model-value="color ?? -1"
              :usage="usage"
              :prev-color="neighbors.prev"
              :next-color="neighbors.next"
              @update:model-value="((color = $event), touched())"
            />
            <p v-if="color === null" class="mt-1 text-[11px] text-gray-500">
              {{ t('family.colorAutoHint') }}
            </p>
          </div>
          <label class="block">
            <span class="mb-1 flex items-center gap-1 text-xs text-gray-400">
              <UiIcon name="crown" :size="12" class="text-amber-300" />
              {{ t('family.primaryLabel') }}
            </span>
            <input
              v-model="primary"
              class="input w-full font-mono"
              type="text"
              inputmode="email"
              autocomplete="off"
              spellcheck="false"
              data-field="primary"
              @input="touched"
            />
          </label>
          <label class="block">
            <span class="mb-1 flex items-center justify-between text-xs text-gray-400">
              {{ t('family.members') }}
              <span :class="members.length + 1 > 6 ? 'text-amber-300' : 'text-gray-500'">{{
                tf('family.people', { count: members.length + 1 })
              }}</span>
            </span>
            <textarea
              v-model="membersText"
              class="input h-auto min-h-28 w-full resize-y py-1.5 font-mono text-xs leading-5"
              rows="6"
              autocomplete="off"
              spellcheck="false"
              :placeholder="t('family.membersPlaceholder')"
              data-field="members"
              @input="touched"
            ></textarea>
            <span v-if="members.length + 1 > 6" class="mt-1 block text-[11px] text-amber-300">{{
              t('family.warn.over_five')
            }}</span>
            <span v-if="willDissolve" class="mt-1 block text-[11px] text-red-300">{{
              t('family.willDissolve')
            }}</span>
          </label>

          <div
            v-if="existingNote"
            class="rounded border border-blue-500/40 bg-blue-500/10 px-3 py-2 text-xs text-blue-200"
            data-testid="family-create-existing"
          >
            <p>{{ existingNote.text }}</p>
            <p v-if="existingNote.meta" class="mt-0.5 text-amber-300">
              {{ t('family.createExistingMeta') }}
            </p>
          </div>
          <FamilyChanges
            v-if="preview"
            :result="preview"
            :omit-warnings="existingNote ? ['name_ignored', 'over_five'] : ['over_five']"
          />
          <p
            v-if="error"
            class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-300"
          >
            {{ error }}
          </p>
        </div>
        <div class="mt-5 flex gap-2">
          <button class="btn flex-1" type="button" @click="emit('close')">
            {{ t('common.cancel') }}
          </button>
          <button
            class="btn flex-1"
            :class="willDissolve ? 'btn-danger' : 'btn-primary'"
            type="button"
            data-testid="family-save"
            :disabled="
              saving ||
              rejected ||
              (!willDissolve && (primary.trim() === '' || members.length === 0))
            "
            :aria-busy="saving"
            @click="save"
          >
            {{ saveLabel }}
          </button>
        </div>
      </div>
    </div>
  </Teleport>
</template>
