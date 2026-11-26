<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, reactive, ref, watch } from 'vue'
import { api } from '@/api'
import { useI18n, type TranslationKey } from '@/i18n'
import type { ProfileUpdateInput, VaultField } from '@/types'
import {
  SECRET_INPUT_ATTRS,
  closeProfileEditor,
  hasControl,
  lettersOnlyTOTP,
  normEmail,
  passwordNeedsTab,
  profileByEmail,
  profileEditor,
  vaultErrorText,
  vaultNotice,
  vaultWrite,
} from '@/vault'
import UiIcon from '../UiIcon.vue'

// ProfileEditor 为新增或编辑一条账号资料的弹窗（App 里挂一次，单独的异步块）。为了不让浏览器密码管理器与扩展抓取第三方密码：
// 不用 <form>（保存是按钮的点击，Enter 不提交），不用 type=password（密码与 2FA 是普通文本框，用 -webkit-text-security 遮挡，
// 眼睛按钮切换），并加各家扩展的忽略属性。只提交改过的字段；要清空某一项必须点它的“清空”。
// 明文只在这个弹窗打开期间留在输入框里，关闭即清空
const { t, tf } = useI18n()

type SecretField = 'password' | 'totp' | 'note'
const fields: VaultField[] = ['password', 'recovery_email', 'totp', 'note']
const labels: Record<VaultField, TranslationKey> = {
  password: 'vault.fieldPassword',
  recovery_email: 'vault.fieldRecovery',
  totp: 'vault.fieldTotp',
  note: 'vault.fieldNote',
}

const email = ref(profileEditor.email)
const values = reactive<Record<VaultField, string>>({
  password: '',
  recovery_email: '',
  totp: '',
  note: '',
})
// revealed 为“显示当前值”取到的原值：输入框里还是这个值时不算改动
const revealed = reactive<Partial<Record<SecretField, string>>>({})
const clearing = reactive(new Set<VaultField>())
const masked = reactive({ password: true, totp: true })
const saving = ref(false)
const loadingField = ref('')
const error = ref('')
const emailInput = ref<HTMLInputElement | null>(null)

const key = computed(() => normEmail(email.value))
const existing = computed(() => profileByEmail.value.get(key.value))
const locked = computed(() => profileEditor.mode === 'edit')
const originalRecovery = computed(() => existing.value?.recovery_email ?? '')

function has(field: VaultField): boolean {
  const profile = existing.value
  if (profile === undefined) return false
  switch (field) {
    case 'password':
      return profile.has_password
    case 'recovery_email':
      return (profile.recovery_email ?? '') !== ''
    case 'totp':
      return profile.has_totp
    default:
      return profile.has_note
  }
}

// 新增时输入的邮箱已有资料：辅助邮箱预填成已记录的值（与编辑同样的规则）
watch(
  originalRecovery,
  (value) => {
    values.recovery_email = value
  },
  { immediate: true },
)

function changed(field: VaultField): boolean {
  if (clearing.has(field)) return false
  if (field === 'recovery_email') {
    const value = values.recovery_email.trim()
    return value !== '' && value.toLowerCase() !== originalRecovery.value
  }
  const value = values[field]
  return value !== '' && value !== (revealed[field] ?? null)
}

// recoveryCleared 为用户把预填的辅助邮箱删空了：保存时清空（与点“清空”相同）
const recoveryCleared = computed(
  () => originalRecovery.value !== '' && values.recovery_email.trim() === '',
)

const clearList = computed<VaultField[]>(() => {
  const list = fields.filter((field) => clearing.has(field) && has(field))
  if (recoveryCleared.value && !list.includes('recovery_email')) list.push('recovery_email')
  return list
})

const controlFields = computed(() =>
  fields.filter((field) => !clearing.has(field) && hasControl(values[field])),
)

const payload = computed<ProfileUpdateInput | null>(() => {
  if (key.value === '') return null
  const input: ProfileUpdateInput = { email: key.value }
  let count = 0
  if (changed('password')) {
    input.password = values.password
    count += 1
  }
  if (changed('recovery_email')) {
    input.recovery_email = values.recovery_email.trim()
    count += 1
  }
  if (changed('totp')) {
    input.totp = values.totp.trim()
    count += 1
  }
  if (changed('note')) {
    input.note = values.note.trim()
    count += 1
  }
  if (clearList.value.length > 0) input.clear = [...clearList.value]
  // 新增一条只有邮箱的资料也允许（之后再补字段）
  if (count === 0 && clearList.value.length === 0 && existing.value !== undefined) return null
  return input
})

const canSave = computed(
  () => !saving.value && payload.value !== null && controlFields.value.length === 0,
)

const willBeEmpty = computed(() => {
  if (existing.value === undefined) return false
  return fields.every(
    (field) => clearList.value.includes(field) || (!has(field) && !changed(field)),
  )
})

function toggleClear(field: VaultField): void {
  if (clearing.has(field)) {
    clearing.delete(field)
    if (field === 'recovery_email') values.recovery_email = originalRecovery.value
    return
  }
  clearing.add(field)
  values[field] = ''
  if (field !== 'recovery_email') delete revealed[field]
}

async function showCurrent(field: SecretField): Promise<void> {
  loadingField.value = field
  error.value = ''
  try {
    const secret = await api.revealProfile(key.value, [field])
    const value = secret[field] ?? ''
    values[field] = value
    revealed[field] = value
    clearing.delete(field)
  } catch (reason) {
    error.value = vaultErrorText(reason)
  } finally {
    loadingField.value = ''
  }
}

async function save(): Promise<void> {
  const input = payload.value
  if (input === null || !canSave.value) return
  saving.value = true
  error.value = ''
  try {
    const result = await vaultWrite(api.updateProfile(input))
    const message = result.created ? t('vault.addedNotice') : t('vault.savedNotice')
    vaultNotice(willBeEmpty.value ? `${message} ${t('vault.allClearedHint')}` : message)
    close()
  } catch (reason) {
    error.value = vaultErrorText(reason)
  } finally {
    saving.value = false
  }
}

function close(): void {
  for (const field of fields) values[field] = ''
  for (const field of Object.keys(revealed) as SecretField[]) delete revealed[field]
  closeProfileEditor()
}

function onKeydown(event: KeyboardEvent): void {
  if (event.key === 'Escape') close()
}

onMounted(async () => {
  document.addEventListener('keydown', onKeydown)
  await nextTick()
  if (!locked.value && email.value === '') emailInput.value?.focus()
})
onBeforeUnmount(() => {
  document.removeEventListener('keydown', onKeydown)
  for (const field of fields) values[field] = ''
})

function placeholder(field: VaultField): string {
  if (clearing.has(field)) return t('vault.willClear')
  return has(field) ? t('vault.keepHint') : ''
}
</script>

<template>
  <Teleport to="body">
    <div class="modal-overlay" data-testid="profile-editor" @click.self="close">
      <div
        class="mx-4 max-h-[92vh] w-full max-w-lg overflow-y-auto rounded-lg border border-line bg-panel p-5 shadow-xl sm:p-6"
        role="dialog"
        aria-modal="true"
        :aria-label="t(existing ? 'vault.editTitle' : 'vault.addTitle')"
      >
        <div class="mb-4 flex items-center justify-between gap-3">
          <h3 class="flex items-center gap-2 text-lg font-bold text-white">
            <UiIcon name="key" :size="16" class="text-amber-300" />
            {{ t(existing ? 'vault.editTitle' : 'vault.addTitle') }}
          </h3>
          <button
            class="rounded p-1 text-gray-500 hover:bg-raised hover:text-white"
            type="button"
            :aria-label="t('common.close')"
            @click="close"
          >
            <UiIcon name="close" :size="16" />
          </button>
        </div>

        <div class="space-y-3 text-sm">
          <label class="block">
            <span class="mb-1 block text-xs text-gray-400">{{ t('vault.fieldEmail') }}</span>
            <input
              ref="emailInput"
              v-model="email"
              class="input w-full font-mono"
              type="text"
              inputmode="email"
              :readonly="locked"
              :class="locked ? 'text-gray-400' : ''"
              v-bind="SECRET_INPUT_ATTRS"
              data-field="email"
            />
            <span v-if="!locked && existing" class="mt-1 block text-[11px] text-gray-500">{{
              t('vault.emailExists')
            }}</span>
          </label>

          <div v-for="field in fields" :key="field">
            <div class="mb-1 flex items-center justify-between gap-2 text-xs">
              <span class="flex items-center gap-1.5 text-gray-400">
                {{ t(labels[field]) }}
                <span
                  v-if="clearing.has(field) || (field === 'recovery_email' && recoveryCleared)"
                  class="rounded border border-red-500/40 px-1 text-[10px] text-red-300 line-through"
                  >{{ t('vault.willClear') }}</span
                >
              </span>
              <span class="flex items-center gap-2">
                <button
                  v-if="field !== 'recovery_email' && has(field) && !clearing.has(field)"
                  class="text-blue-400 hover:text-blue-300 disabled:opacity-50"
                  type="button"
                  :disabled="loadingField !== ''"
                  :aria-busy="loadingField === field"
                  :data-show="field"
                  @click="showCurrent(field as SecretField)"
                >
                  {{ t('vault.showCurrent') }}
                </button>
                <button
                  v-if="has(field)"
                  class="hover:text-red-300"
                  :class="clearing.has(field) ? 'text-gray-300' : 'text-gray-500'"
                  type="button"
                  :data-clear="field"
                  @click="toggleClear(field)"
                >
                  {{ clearing.has(field) ? t('vault.undoClear') : t('vault.clearField') }}
                </button>
              </span>
            </div>
            <div v-if="field === 'note'" translate="no">
              <textarea
                v-model="values.note"
                class="input h-auto min-h-16 w-full resize-y py-1.5"
                rows="2"
                :disabled="clearing.has('note')"
                :placeholder="placeholder('note')"
                v-bind="SECRET_INPUT_ATTRS"
                data-field="note"
              ></textarea>
            </div>
            <div v-else class="relative" translate="no">
              <input
                v-model="values[field]"
                class="input w-full font-mono"
                :class="{
                  'secret-mask': (field === 'password' || field === 'totp') && masked[field],
                  'pr-9': field === 'password' || field === 'totp',
                }"
                type="text"
                :disabled="clearing.has(field)"
                :placeholder="placeholder(field)"
                v-bind="SECRET_INPUT_ATTRS"
                :data-field="field"
              />
              <button
                v-if="field === 'password' || field === 'totp'"
                class="absolute top-1/2 right-1.5 -translate-y-1/2 rounded p-1 text-gray-500 hover:text-gray-200"
                type="button"
                :aria-label="t(masked[field] ? 'vault.unmask' : 'vault.mask')"
                :aria-pressed="!masked[field]"
                :data-mask="field"
                @click="masked[field] = !masked[field]"
              >
                <UiIcon :name="masked[field] ? 'eye' : 'eyeOff'" :size="14" />
              </button>
            </div>
            <p v-if="controlFields.includes(field)" class="mt-1 text-[11px] text-red-300">
              {{ t('vault.fieldControlChar') }}
            </p>
            <template v-if="field === 'password' && values.password !== ''">
              <p
                v-if="values.password !== values.password.trim()"
                class="mt-1 text-[11px] text-amber-300"
              >
                {{ t('vault.passwordEdgeSpace') }}
              </p>
              <p v-if="passwordNeedsTab(values.password)" class="mt-1 text-[11px] text-gray-400">
                {{ t('vault.passwordTabLine') }}
              </p>
            </template>
            <p
              v-if="field === 'totp' && lettersOnlyTOTP(values.totp)"
              class="mt-1 text-[11px] text-amber-300"
            >
              {{ t('vault.totpLettersHint') }}
            </p>
            <p
              v-if="field === 'recovery_email' && recoveryCleared && !clearing.has(field)"
              class="mt-1 text-[11px] text-gray-400"
            >
              {{ t('vault.recoveryClearedHint') }}
            </p>
          </div>

          <p
            v-if="willBeEmpty"
            class="rounded border border-line bg-canvas px-3 py-2 text-xs text-gray-400"
          >
            {{ t('vault.allClearedHint') }}
          </p>
          <p
            v-if="error"
            class="rounded border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-300"
          >
            {{ error }}
          </p>
          <p class="text-[11px] leading-5 text-gray-500">{{ t('vault.editorPrivacy') }}</p>
        </div>

        <div class="mt-5 flex gap-2">
          <button class="btn flex-1" type="button" @click="close">{{ t('common.cancel') }}</button>
          <button
            class="btn btn-primary flex-1"
            type="button"
            data-testid="profile-save"
            :disabled="!canSave"
            :aria-busy="saving"
            @click="save"
          >
            {{ t('common.save') }}
          </button>
        </div>
        <p v-if="payload === null && existing" class="mt-2 text-center text-[11px] text-gray-500">
          {{ t('vault.noChanges') }}
        </p>
        <p
          v-else-if="payload && (payload.clear?.length ?? 0) > 0"
          class="mt-2 text-center text-[11px] text-red-300"
        >
          {{
            tf('vault.clearSummary', {
              fields: (payload.clear ?? [])
                .map((field) => t(labels[field]))
                .join(t('common.listSep')),
            })
          }}
        </p>
      </div>
    </div>
  </Teleport>
</template>
