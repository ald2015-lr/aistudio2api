import type { TranslationKey } from '@/i18n'
import type { AccountState } from '@/types'

// 账户状态的显示：状态名的翻译键、状态点与文字的颜色。账户页与“资料与家庭组”页共用（从 AccountsPanel.vue 原样移出）

export const stateKeys: Record<AccountState, TranslationKey> = {
  ready: 'state.ready',
  busy: 'state.busy',
  cooldown: 'state.cooldown',
  auth_required: 'state.auth_required',
  unavailable: 'state.unavailable',
  disabled: 'state.disabled',
  org_disabled: 'state.org_disabled',
  proxy_paused: 'state.proxy_paused',
}

export const stateDotClass: Record<AccountState, string> = {
  ready: 'bg-green-500',
  busy: 'bg-blue-500',
  cooldown: 'bg-yellow-500',
  auth_required: 'bg-red-500',
  unavailable: 'bg-red-500',
  disabled: 'bg-gray-600',
  org_disabled: 'bg-orange-500',
  proxy_paused: 'bg-purple-500',
}

export const stateTextClass: Record<AccountState, string> = {
  ready: 'text-green-400',
  busy: 'text-blue-400',
  cooldown: 'text-yellow-400',
  auth_required: 'text-red-400',
  unavailable: 'text-red-400',
  disabled: 'text-gray-500',
  org_disabled: 'text-orange-400',
  proxy_paused: 'text-purple-300',
}
