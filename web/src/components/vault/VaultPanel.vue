<script setup lang="ts">
import { computed, ref } from 'vue'
import { api } from '@/api'
import { confirmAction } from '@/confirm'
import { useI18n, type TranslationKey } from '@/i18n'
import type { Account } from '@/types'
import {
  familiesBySeq,
  formatShortTime,
  refreshVault,
  useAccountIndex,
  useVaultData,
  vaultData,
  vaultErrorText,
  vaultNav,
  vaultNotice,
  vaultStatus,
  vaultWrite,
  type ProfileListFilter,
  type VaultSubTab,
} from '@/vault'
import FamilyCards from './FamilyCards.vue'
import FamilyImport from './FamilyImport.vue'
import ProfileImport from './ProfileImport.vue'
import ProfileList from './ProfileList.vue'
import UiIcon from '../UiIcon.vue'

// VaultPanel 为“资料与家庭组”页签：账号资料（密码、辅助邮箱、2FA、备注）与家庭组只用于查看、复制与分类显示，不参与登录与调度。
// 数据独立于账户目录保存（删除账户不会删除它们）；“未存在”按账户列表现算。账户页的入口（资料中未存在的邮箱等）经 vaultNav 的
// 跳转请求打开这里的某个子页签与筛选，挂载时读取并清掉
defineOptions({ name: 'VaultPanel' })
const props = defineProps<{
  accounts: Account[]
  loading: boolean
}>()

const { t, tf } = useI18n()
useVaultData()
const index = useAccountIndex(() => props.accounts)
const byEmail = computed(() => index.value.byEmail)

const request = vaultNav.vault
vaultNav.vault = null
const sub = ref<VaultSubTab>(request?.sub ?? 'profiles')
// 资料列表由 KeepAlive 保留：切到家庭组、导入等子页签再回来，筛选、搜索、页码与选择都不变（跳转带来的筛选只在创建时用一次）。
// listKey 让跳转请求（页头“未存在”、导入完成卡上的“查看不在账户里的”）按新的筛选重建资料列表
const listKey = ref(0)
const listFilter = ref<ProfileListFilter | undefined>(request?.filter)
const listFamily = ref<string | undefined>(request?.family)

function goto(tab: VaultSubTab, filter?: ProfileListFilter): void {
  if (tab === 'profiles' && filter !== undefined) {
    listFilter.value = filter
    listFamily.value = undefined
    listKey.value += 1
  }
  sub.value = tab
}

// missing 为资料里、账户列表里没有的邮箱数；账户列表还没载入（或载入失败、一个账户都没有）时不知道，为 null（显示“…”）
const counts = computed(() => {
  let missing = 0
  let totp = 0
  for (const profile of vaultData.value?.profiles ?? []) {
    if (!byEmail.value.has(profile.email)) missing += 1
    if (profile.has_totp) totp += 1
  }
  return {
    profiles: vaultData.value?.profiles.length ?? 0,
    missing: props.loading ? null : missing,
    totp,
    families: familiesBySeq.value.length,
  }
})

const fileLabels: Record<string, TranslationKey> = {
  profiles: 'vault.fileProfiles',
  families: 'vault.fileFamilies',
}

const errors = computed(() => vaultData.value?.errors ?? [])
const working = ref('')

async function reload(): Promise<void> {
  working.value = 'reload'
  await refreshVault()
  working.value = ''
}

async function startEmpty(file: string): Promise<void> {
  if (file !== 'profiles' && file !== 'families') return
  const label = t(fileLabels[file] ?? 'vault.fileProfiles')
  if (!(await confirmAction(tf('vault.startEmptyConfirm', { file: label }), t('vault.startEmpty'))))
    return
  working.value = `empty:${file}`
  try {
    const result = await vaultWrite(api.vaultStartEmpty(file))
    vaultNotice(
      result.moved_backup
        ? tf('vault.startEmptyDone', { file: label, path: result.moved_backup })
        : tf('vault.startEmptyDoneNoBackup', { file: label }),
    )
  } catch (error) {
    vaultNotice(vaultErrorText(error), 'error')
  } finally {
    working.value = ''
  }
}

const backupText = computed(() =>
  (vaultData.value?.backups ?? [])
    .map((backup) =>
      tf('vault.backups', {
        file: t(fileLabels[backup.file] ?? 'vault.fileProfiles'),
        bak: backup.bak_time ? formatShortTime(backup.bak_time) : t('vault.none'),
        count: backup.snapshots,
        time: backup.snapshot_latest ? formatShortTime(backup.snapshot_latest) : t('vault.none'),
      }),
    )
    .join(' · '),
)

const tabs = computed<
  { id: VaultSubTab; label: string; count?: number; icon: 'key' | 'users' | 'upload' }[]
>(() => [
  { id: 'profiles', label: t('vault.tabProfiles'), count: counts.value.profiles, icon: 'key' },
  { id: 'families', label: t('vault.tabFamilies'), count: counts.value.families, icon: 'users' },
  { id: 'import', label: t('vault.tabImport'), icon: 'upload' },
  { id: 'familyImport', label: t('vault.tabFamilyImport'), icon: 'upload' },
])
</script>

<template>
  <section class="min-h-0 flex-1 overflow-auto">
    <div class="page space-y-4">
      <header class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <h2 class="page-title flex items-center gap-2">
            <UiIcon name="lock" :size="18" class="text-amber-300" />
            {{ t('section.vault.title') }}
          </h2>
          <p class="mt-1 text-sm text-gray-400">{{ t('section.vault.description') }}</p>
        </div>
        <div class="flex flex-wrap items-center gap-1.5 text-xs" data-testid="vault-stats">
          <span class="tag font-sans">{{
            tf('vault.statProfiles', { count: counts.profiles })
          }}</span>
          <button
            class="tag tag-missing font-sans"
            type="button"
            :disabled="counts.missing === null || counts.missing === 0"
            data-testid="vault-stat-missing"
            @click="goto('profiles', 'missing')"
          >
            {{ tf('vault.statMissing', { count: counts.missing ?? '…' }) }}
          </button>
          <span class="tag font-sans">{{ tf('vault.statTotp', { count: counts.totp }) }}</span>
          <span class="tag font-sans">{{
            tf('vault.statFamilies', { count: counts.families })
          }}</span>
        </div>
      </header>

      <div
        v-for="entry in errors"
        :key="entry.file"
        class="space-y-2 rounded-md border border-red-500/50 bg-red-500/10 px-3 py-2.5 text-xs text-red-200"
        role="alert"
        data-testid="vault-file-error"
      >
        <p class="flex items-center gap-1.5 font-semibold text-red-300">
          <UiIcon name="warning" :size="14" />
          {{
            tf('vault.fileError', {
              file: t(fileLabels[entry.file] ?? 'vault.fileProfiles'),
              message: entry.message,
            })
          }}
        </p>
        <p v-if="entry.path" class="font-mono break-all text-red-200/80">{{ entry.path }}</p>
        <p class="text-red-200/90">
          {{
            t(
              entry.reason === 'missing_with_backup'
                ? 'vault.missingWithBackup'
                : 'vault.fileErrorHelp',
            )
          }}
        </p>
        <p v-if="entry.backup" class="font-mono break-all text-red-200/70">
          {{
            tf('vault.fileErrorBackup', {
              path: entry.backup,
              time: entry.backup_time ? formatShortTime(entry.backup_time) : '',
            })
          }}
        </p>
        <div class="flex flex-wrap gap-2">
          <button class="btn btn-sm" type="button" :disabled="working !== ''" @click="reload">
            <UiIcon name="refresh" :size="12" />
            {{ t('vault.reload') }}
          </button>
          <button
            v-if="entry.reason === 'missing_with_backup'"
            class="btn btn-sm btn-danger"
            type="button"
            :disabled="working !== ''"
            @click="startEmpty(entry.file)"
          >
            {{ t('vault.startEmpty') }}
          </button>
        </div>
      </div>
      <div
        v-if="vaultStatus.error"
        class="flex flex-wrap items-center gap-2 rounded-md border border-red-500/40 bg-red-500/10 px-3 py-2 text-xs text-red-300"
        role="alert"
      >
        {{ tf('vault.loadFailed', { message: vaultStatus.error }) }}
        <button class="btn btn-sm" type="button" @click="reload">{{ t('vault.reload') }}</button>
      </div>

      <nav class="seg" :aria-label="t('section.vault.title')" data-testid="vault-tabs">
        <button
          v-for="tab in tabs"
          :key="tab.id"
          class="seg-item"
          :class="{ active: sub === tab.id }"
          type="button"
          :aria-current="sub === tab.id ? 'page' : undefined"
          :data-tab="tab.id"
          @click="sub = tab.id"
        >
          <UiIcon :name="tab.icon" :size="13" />
          {{ tab.label }}
          <span v-if="tab.count !== undefined" class="seg-count">{{ tab.count }}</span>
        </button>
      </nav>

      <div v-if="vaultStatus.loading && vaultData === null" class="py-12 text-center text-gray-500">
        {{ t('common.loading') }}
      </div>
      <template v-else>
        <KeepAlive include="ProfileList" :max="1">
          <ProfileList
            v-if="sub === 'profiles'"
            :key="listKey"
            :by-email="byEmail"
            :accounts-loading="loading"
            :initial-filter="listFilter"
            :initial-family="listFamily"
          />
          <FamilyCards
            v-else-if="sub === 'families'"
            :by-email="byEmail"
            :accounts-loading="loading"
            @import="sub = 'familyImport'"
          />
          <ProfileImport
            v-else-if="sub === 'import'"
            :by-email="byEmail"
            :accounts-loading="loading"
            @goto="goto"
          />
          <FamilyImport v-else :by-email="byEmail" :accounts-loading="loading" @goto="goto" />
        </KeepAlive>
      </template>

      <footer class="space-y-0.5 border-t border-line pt-3 text-[11px] leading-5 text-gray-500">
        <p>
          {{
            vaultData?.persistent === false
              ? t('vault.memoryOnly')
              : tf('vault.storedAt', { dir: vaultData?.dir || 'runtime/account-vault' })
          }}
        </p>
        <p v-if="backupText">{{ backupText }}</p>
        <p>{{ t('vault.privacyNote') }}</p>
      </footer>
    </div>
  </section>
</template>
