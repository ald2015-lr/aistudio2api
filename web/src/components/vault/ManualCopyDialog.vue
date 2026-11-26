<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from '@/i18n'
import { closeManualCopy, manualCopy } from '@/vault'
import UiIcon from '../UiIcon.vue'

// ManualCopyDialog 是浏览器复制失败时的兜底（HTTP 页面上取出明文后等得太久，execCommand 不再允许写剪贴板）：只读文本框里放要复制的
// 内容，打开即聚焦全选，用户按 Ctrl+C / ⌘C 复制。层级在弹窗遮罩（50）之上、提示（65）之下（.modal-overlay 不在样式层里，
// 工具类盖不过它，所以层级写在 style 上）；关闭即清空，2 分钟无操作自动关闭。
// 文本框加 translate="no"，浏览器整页翻译不会把明文发给翻译服务
const IDLE_CLOSE_MS = 120_000

const { t } = useI18n()
const area = ref<HTMLTextAreaElement | null>(null)
let idleTimer: number | undefined

function armIdle(): void {
  window.clearTimeout(idleTimer)
  idleTimer = window.setTimeout(closeManualCopy, IDLE_CLOSE_MS)
}

function selectAll(): void {
  area.value?.focus()
  area.value?.select()
}

watch(
  () => manualCopy.open,
  async (open) => {
    window.clearTimeout(idleTimer)
    if (!open) return
    armIdle()
    await nextTick()
    selectAll()
  },
)

onBeforeUnmount(() => window.clearTimeout(idleTimer))
</script>

<template>
  <Transition name="dialog">
    <div
      v-if="manualCopy.open"
      class="modal-overlay"
      style="z-index: 55"
      role="dialog"
      aria-modal="true"
      :aria-label="t('vault.manualCopyTitle')"
      data-testid="vault-manual-copy"
      @click.self="closeManualCopy"
      @keydown.esc="closeManualCopy"
      @pointermove="armIdle"
      @keydown="armIdle"
    >
      <div class="mx-4 w-full max-w-lg rounded-lg border border-line bg-panel p-5 shadow-xl">
        <div class="mb-3 flex items-start justify-between gap-3">
          <div class="min-w-0">
            <h3 class="text-base font-semibold text-white">{{ t('vault.manualCopyTitle') }}</h3>
            <p class="mt-1 text-xs text-gray-400">{{ t('vault.manualCopyHint') }}</p>
          </div>
          <button
            class="rounded p-1 text-gray-500 hover:bg-raised hover:text-white"
            type="button"
            :aria-label="t('common.close')"
            @click="closeManualCopy"
          >
            <UiIcon name="close" :size="16" />
          </button>
        </div>
        <p v-if="manualCopy.title" class="mb-1 text-xs text-gray-500">{{ manualCopy.title }}</p>
        <textarea
          ref="area"
          class="input h-auto max-h-[50vh] min-h-24 w-full resize-y py-2 font-mono text-xs leading-5"
          :value="manualCopy.text"
          :rows="Math.min(10, Math.max(3, manualCopy.text.split('\n').length))"
          readonly
          translate="no"
          spellcheck="false"
          autocomplete="off"
          data-1p-ignore
          data-lpignore="true"
          data-bwignore
          data-form-type="other"
          @focus="selectAll"
        ></textarea>
        <div class="mt-4 flex justify-end gap-2">
          <button class="btn" type="button" @click="selectAll">
            {{ t('vault.manualCopySelect') }}
          </button>
          <button class="btn btn-primary" type="button" @click="closeManualCopy">
            {{ t('common.close') }}
          </button>
        </div>
      </div>
    </div>
  </Transition>
</template>
