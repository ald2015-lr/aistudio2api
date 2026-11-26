<script setup lang="ts">
import { computed, onBeforeUnmount, onDeactivated, onMounted, ref, shallowRef, watch } from 'vue'
import { ApiError, api } from '@/api'
import { copyText } from '@/clipboard'
import { useI18n, type TranslationKey } from '@/i18n'
import { useSyncedSecondClock } from '@/serverClock'
import type { ProfileSecret, RevealField, VaultField, VaultProfile } from '@/types'
import {
  famClass,
  formatCode,
  openManualCopy,
  openProfileEditor,
  vaultErrorText,
  vaultNotice,
  type FamilyMembership,
} from '@/vault'
import FamilyBadge from './FamilyBadge.vue'
import UiIcon from '../UiIcon.vue'
import UiPopover from '../UiPopover.vue'

// SecretMenu 为一条账号资料的复制菜单（账户页的钥匙按钮、资料页每行的“复制 ▾”）。
// - 邮箱、辅助邮箱直接复制（列表里本就有）；密码、2FA 密钥、备注、验证码、整行都在点击时经取出接口只取这一项，拿到后立即复制，
//   明文只在局部变量里
// - 验证码取到后显示并倒计时；剩 5 秒以内时在下一行显示下一个码（可单独复制）；归零时菜单还开着就自动再取，最多连续 4 次
// - 眼睛按钮取出全部字段，在菜单里显示 30 秒（可提前隐藏）；菜单关闭、页面隐藏、组件卸载时立即清掉。
//   清掉时代次 epoch 加一：取出还在路上时关了菜单（或页面被隐藏），回来的明文、验证码一律丢弃、不保存；
//   用户点的复制照常复制，只是不留在菜单里
// - 复制失败（HTTP 页面上取出等得太久时浏览器不再允许写剪贴板）先关菜单，再打开手动复制框
// 明文不进提示、悬停说明、标题、aria、本地存储与控制台；显示明文的元素加 translate="no"
const props = withDefaults(
  defineProps<{
    // email 为规范化邮箱（取出接口的键）；label 为显示与复制用的邮箱（账户页用账户的 label，大小写照原样）
    email: string
    label?: string
    profile: VaultProfile
    membership?: FamilyMembership | undefined
    variant?: 'key' | 'button'
  }>(),
  { label: '', membership: undefined, variant: 'key' },
)

const REVEAL_SECONDS = 30
const CODE_AUTO_REFRESH = 4

const { t, tf } = useI18n()
const open = ref(false)
const busy = ref('')
// plain 为眼睛按钮取出的明文（显示 30 秒）；plainUntil 为自动隐藏的本机时刻
const plain = shallowRef<ProfileSecret | null>(null)
const plainUntil = ref(0)
// code 为最近取到的验证码；at 为收到时的本机时刻（按收到时的 remaining 倒数）
const code = shallowRef<{ code: string; next: string; remaining: number; at: number } | null>(null)
const codeError = ref<'' | 'missing' | 'invalid'>('')
const autoRefreshes = ref(0)
// line 为最近一次复制整行的格式说明（不保存整行本身）
const lineInfo = ref<{ format: string; lossy: string } | null>(null)

const shownEmail = computed(() => props.label || props.email)
const flags = computed(() => props.profile.totp_flags ?? [])
const { now, sync: syncClock } = useSyncedSecondClock(
  () => open.value && (code.value !== null || plain.value !== null),
)
// epoch 为清掉明文的代次（clearSecrets 加一）：取出前记下，回来时变了就丢弃结果
let epoch = 0

// 倒计时按收到时刻 at、plainUntil 计算；设置它们时同时 syncClock，now 不会早于它们（再兜底夹一次，倒计时不会多出 1 秒）
const codeLeft = computed(() => {
  const current = code.value
  if (current === null) return 0
  return Math.max(0, current.remaining - Math.floor(Math.max(0, now.value - current.at) / 1000))
})
const plainLeft = computed(() =>
  plain.value === null
    ? 0
    : Math.min(REVEAL_SECONDS, Math.max(0, Math.ceil((plainUntil.value - now.value) / 1000))),
)

function clearSecrets(): void {
  epoch += 1
  plain.value = null
  code.value = null
  codeError.value = ''
  autoRefreshes.value = 0
  lineInfo.value = null
}

watch(open, (value) => {
  if (!value) clearSecrets()
})

watch(plainLeft, (left) => {
  if (plain.value !== null && left <= 0) plain.value = null
})

watch(codeLeft, (left) => {
  if (left > 0 || code.value === null || !open.value) return
  if (autoRefreshes.value >= CODE_AUTO_REFRESH) {
    code.value = null
    return
  }
  autoRefreshes.value += 1
  void fetchCode(false)
})

function onVisibility(): void {
  if (document.visibilityState === 'hidden') clearSecrets()
}

onMounted(() => document.addEventListener('visibilitychange', onVisibility))
onBeforeUnmount(() => {
  document.removeEventListener('visibilitychange', onVisibility)
  clearSecrets()
})
// 资料列表被 KeepAlive 保留（切到家庭组等子页签）时：关菜单并清掉明文，与卸载时一样
onDeactivated(() => {
  open.value = false
  clearSecrets()
})

// copyValue 复制并提示；浏览器复制失败时关菜单、打开手动复制框（弹层层级在遮罩之上，先关掉免得盖住对话框）
async function copyValue(text: string, what: string): Promise<void> {
  if (await copyText(text)) {
    vaultNotice(tf('vault.copied', { what }))
    return
  }
  open.value = false
  openManualCopy(text, what)
}

// stillCurrent 为发请求时的代次仍然有效（之后没有清掉过）且菜单还开着
function stillCurrent(started: number): boolean {
  return started === epoch && open.value
}

async function reveal(
  fields: RevealField[] | undefined,
  action: string,
): Promise<ProfileSecret | null> {
  busy.value = action
  const started = epoch
  try {
    return await api.revealProfile(props.email, fields)
  } catch (error) {
    if (
      stillCurrent(started) &&
      fields?.length === 1 &&
      fields[0] === 'totp_code' &&
      error instanceof ApiError
    ) {
      if (error.code === 'vault_totp_missing') codeError.value = 'missing'
      else if (error.code === 'vault_totp_invalid') codeError.value = 'invalid'
    }
    vaultNotice(vaultErrorText(error), 'error')
    return null
  } finally {
    busy.value = ''
  }
}

const fieldLabels: Record<VaultField, TranslationKey> = {
  password: 'vault.fieldPassword',
  recovery_email: 'vault.fieldRecovery',
  totp: 'vault.fieldTotp',
  note: 'vault.fieldNote',
}

async function copyField(field: 'password' | 'totp' | 'note'): Promise<void> {
  const what = t(fieldLabels[field])
  let value = plain.value?.[field] ?? ''
  if (value === '') {
    const secret = await reveal([field], field)
    if (secret === null) return
    value = secret[field] ?? ''
  }
  if (value === '') {
    vaultNotice(tf('vault.fieldEmpty', { what }), 'error')
    return
  }
  await copyValue(value, what)
}

async function copyLine(): Promise<void> {
  const started = epoch
  const secret = await reveal(['line'], 'line')
  if (secret === null || !secret.line) return
  if (stillCurrent(started)) {
    lineInfo.value = { format: secret.line_format ?? 'dash', lossy: secret.line_lossy ?? '' }
  }
  await copyValue(secret.line, t('vault.lineWhat'))
}

// fetchCode 取当前验证码；copy 为真时取到后复制（自动刷新时只显示）。取出途中关了菜单时照常复制（用户点的），但不显示、不保存
async function fetchCode(copy: boolean): Promise<void> {
  const started = epoch
  const secret = await reveal(['totp_code'], 'code')
  const current = secret?.totp_code
  if (current === undefined) return
  if (stillCurrent(started)) {
    codeError.value = ''
    code.value = {
      code: current.code,
      next: current.next_code,
      remaining: current.remaining,
      at: Date.now(),
    }
    syncClock()
  }
  if (copy) await copyValue(current.code, t('vault.codeWhat'))
}

// codeSecondsLeft 为当前验证码按真实时间还剩的秒数（不按每秒跳一次的 now，免得复制一个 1 秒内就过期的码）
function codeSecondsLeft(): number {
  const current = code.value
  return current === null ? 0 : current.remaining - (Date.now() - current.at) / 1000
}

async function copyCode(): Promise<void> {
  autoRefreshes.value = 0
  if (code.value !== null && codeSecondsLeft() >= 2) {
    await copyValue(code.value.code, t('vault.codeWhat'))
    return
  }
  await fetchCode(true)
}

async function copyNextCode(): Promise<void> {
  if (code.value === null) return
  await copyValue(code.value.next, t('vault.nextCodeWhat'))
}

async function toggleReveal(): Promise<void> {
  if (plain.value !== null) {
    plain.value = null
    return
  }
  const started = epoch
  const secret = await reveal(undefined, 'reveal')
  // 取出途中关了菜单、页面被隐藏或组件已停用：丢弃，不留在页面里（否则下次打开不点眼睛就显示明文）
  if (secret === null || !stillCurrent(started)) return
  const at = Date.now()
  plain.value = secret
  plainUntil.value = at + REVEAL_SECONDS * 1000
  if (secret.totp_code !== undefined) {
    code.value = {
      code: secret.totp_code.code,
      next: secret.totp_code.next_code,
      remaining: secret.totp_code.remaining,
      at,
    }
    codeError.value = ''
  } else if (secret.totp_code_error === 'missing' || secret.totp_code_error === 'invalid') {
    codeError.value = secret.totp_code_error
  }
  syncClock()
}

function edit(): void {
  open.value = false
  openProfileEditor(props.email, 'edit')
}

const lossyKeys: Record<string, TranslationKey> = {
  edge_whitespace: 'vault.lineLossy.edge_whitespace',
  note_ambiguous: 'vault.lineLossy.note_ambiguous',
  empty_record: 'vault.lineLossy.empty_record',
  other: 'vault.lineLossy.other',
}
</script>

<template>
  <UiPopover
    v-model:open="open"
    :label="t('accounts.secretMenu')"
    :trigger-class="
      variant === 'key'
        ? 'inline-flex h-5 w-5 shrink-0 items-center justify-center rounded text-amber-300/90 transition hover:bg-raised hover:text-amber-200 data-[state=open]:bg-raised'
        : 'btn btn-sm'
    "
    content-class="w-80 max-w-[calc(100vw-16px)] p-3"
    align="end"
  >
    <template #trigger>
      <span
        class="inline-flex items-center gap-1"
        data-testid="secret-menu-trigger"
        :data-email="email"
      >
        <UiIcon v-if="variant === 'key'" name="key" :size="13" />
        <template v-else>
          <UiIcon name="copy" :size="13" />
          {{ t('vault.copyMenu') }}
          <UiIcon name="chevronDownSm" :size="11" />
        </template>
      </span>
    </template>
    <div data-testid="secret-menu" class="space-y-2 text-xs">
      <div class="flex min-w-0 items-center gap-2 border-b border-line pb-2">
        <UiIcon name="key" :size="13" class="text-amber-300/90" />
        <span class="cell-truncate min-w-0 flex-1 font-mono text-gray-200">{{ shownEmail }}</span>
        <FamilyBadge v-if="membership" :membership="membership" />
        <span
          v-if="membership?.group.name"
          class="max-w-[6rem] truncate text-[11px]"
          :class="famClass(membership.group.color)"
          style="color: var(--fam-text)"
          >{{ membership.group.name }}</span
        >
      </div>
      <div class="grid grid-cols-[4.25rem_minmax(0,1fr)_auto] items-center gap-x-2 gap-y-1.5">
        <span class="text-gray-500">{{ t('vault.fieldEmail') }}</span>
        <span class="cell-truncate font-mono text-gray-300">{{ shownEmail }}</span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="email"
          @click="copyValue(shownEmail, t('vault.fieldEmail'))"
        >
          {{ t('common.copy') }}
        </button>

        <span class="text-gray-500">{{ t('vault.fieldPassword') }}</span>
        <span
          v-if="plain?.password"
          class="break-all font-mono text-gray-100 select-all"
          translate="no"
          data-plain="password"
          >{{ plain.password }}</span
        >
        <span v-else class="font-mono text-gray-400">{{
          profile.has_password ? '●●●●●●●●' : t('vault.none')
        }}</span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="password"
          :disabled="!profile.has_password || busy !== ''"
          :aria-busy="busy === 'password'"
          @click="copyField('password')"
        >
          {{ t('common.copy') }}
        </button>

        <span class="text-gray-500">{{ t('vault.fieldRecovery') }}</span>
        <span class="cell-truncate font-mono text-gray-300">{{
          profile.recovery_email || t('vault.none')
        }}</span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="recovery"
          :disabled="!profile.recovery_email"
          @click="copyValue(profile.recovery_email ?? '', t('vault.fieldRecovery'))"
        >
          {{ t('common.copy') }}
        </button>

        <span class="text-gray-500">{{ t('vault.fieldTotp') }}</span>
        <span class="min-w-0">
          <span
            v-if="plain?.totp"
            class="block break-all font-mono text-gray-100 select-all"
            translate="no"
            data-plain="totp"
            >{{ plain.totp }}</span
          >
          <span v-else class="font-mono text-gray-400">{{
            profile.has_totp ? `●●●● …${profile.totp_last4 ?? ''}` : t('vault.none')
          }}</span>
          <span
            v-if="profile.has_totp && flags.includes('letters_only')"
            class="block text-[11px] text-amber-300"
            >{{ t('vault.totpUnsure') }}</span
          >
          <span
            v-if="profile.has_totp && !profile.totp_ok"
            class="block text-[11px] text-red-300"
            >{{ t('vault.totpBad') }}</span
          >
        </span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="totp"
          :disabled="!profile.has_totp || busy !== ''"
          :aria-busy="busy === 'totp'"
          @click="copyField('totp')"
        >
          {{ t('common.copy') }}
        </button>

        <span class="text-gray-500">{{ t('vault.fieldCode') }}</span>
        <span class="min-w-0">
          <span v-if="code" class="flex items-center gap-2">
            <span
              class="font-mono text-sm tracking-wide text-gray-100"
              translate="no"
              data-plain="code"
              >{{ formatCode(code.code) }}</span
            >
            <span
              class="rounded-full border px-1.5 font-mono text-[10px]"
              :class="
                codeLeft <= 5 ? 'border-amber-500/50 text-amber-300' : 'border-line text-gray-400'
              "
              data-testid="code-remaining"
              >{{ tf('vault.codeRemaining', { seconds: codeLeft }) }}</span
            >
          </span>
          <span v-else-if="codeError === 'missing'" class="text-gray-500">{{
            t('vault.codeMissing')
          }}</span>
          <span v-else-if="codeError === 'invalid'" class="text-red-300">{{
            t('vault.codeInvalid')
          }}</span>
          <span v-else class="text-gray-500">{{
            profile.has_totp ? t('vault.codeHint') : t('vault.none')
          }}</span>
          <span
            v-if="profile.has_totp && flags.includes('otpauth_params')"
            class="block text-[11px] text-amber-300"
            >{{ t('vault.totpParams') }}</span
          >
        </span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="code"
          :disabled="!profile.has_totp || busy !== ''"
          :aria-busy="busy === 'code'"
          @click="copyCode"
        >
          {{ t('common.copy') }}
        </button>

        <template v-if="code && codeLeft <= 5">
          <span></span>
          <span class="text-[11px] text-gray-500" translate="no" data-testid="next-code">{{
            tf('vault.nextCode', { code: formatCode(code.next), seconds: codeLeft })
          }}</span>
          <button class="btn btn-sm" type="button" data-copy="next-code" @click="copyNextCode">
            {{ t('common.copy') }}
          </button>
        </template>

        <span class="text-gray-500">{{ t('vault.fieldNote') }}</span>
        <span
          v-if="plain?.note"
          class="max-h-24 overflow-y-auto break-all text-gray-100 select-all"
          translate="no"
          data-plain="note"
          >{{ plain.note }}</span
        >
        <span v-else class="text-gray-400">{{
          profile.has_note ? t('vault.recorded') : t('vault.none')
        }}</span>
        <button
          class="btn btn-sm"
          type="button"
          data-copy="note"
          :disabled="!profile.has_note || busy !== ''"
          :aria-busy="busy === 'note'"
          @click="copyField('note')"
        >
          {{ t('common.copy') }}
        </button>
      </div>
      <p v-if="lineInfo?.format === 'tab'" class="text-[11px] text-gray-400">
        {{ t('vault.lineTab') }}
      </p>
      <p v-if="lineInfo?.lossy" class="text-[11px] text-amber-300">
        {{ t(lossyKeys[lineInfo.lossy] ?? 'vault.lineLossy.other') }}
      </p>
      <div class="flex flex-wrap items-center gap-1.5 border-t border-line pt-2">
        <button
          class="btn btn-sm btn-primary"
          type="button"
          data-copy="line"
          :disabled="busy !== ''"
          :aria-busy="busy === 'line'"
          @click="copyLine"
        >
          <UiIcon name="copy" :size="12" />
          {{ t('vault.copyLine') }}
        </button>
        <button
          class="btn btn-sm"
          type="button"
          data-testid="secret-reveal"
          :disabled="busy !== '' && busy !== 'reveal'"
          :aria-busy="busy === 'reveal'"
          :aria-pressed="plain !== null"
          @click="toggleReveal"
        >
          <UiIcon :name="plain ? 'eyeOff' : 'eye'" :size="12" />
          {{ plain ? tf('vault.hideIn', { seconds: plainLeft }) : t('vault.reveal') }}
        </button>
        <span class="flex-1"></span>
        <button class="btn btn-sm btn-ghost" type="button" @click="edit">
          <UiIcon name="pencil" :size="12" />
          {{ t('common.edit') }}
        </button>
      </div>
    </div>
  </UiPopover>
</template>
