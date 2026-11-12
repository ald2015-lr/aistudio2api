import { reactive, type InjectionKey } from 'vue'

// expandedHelpTexts 为已展开的设置项说明（按说明文字记）：只在本次打开页面期间有效，切到其他页再回来仍保持展开
export const expandedHelpTexts = reactive(new Set<string>())

// toggleHelpText 展开或收起一段说明
export function toggleHelpText(text: string): void {
  if (expandedHelpTexts.has(text)) expandedHelpTexts.delete(text)
  else expandedHelpTexts.add(text)
}

// HelpFieldState 是 HelpField 里那段说明的状态，由其中的 HelpText 填写：text 为说明文字（还没有说明时为空），
// id 为说明正文元素的 id（展开按钮的 aria-controls），clipped 为折叠时正文超过两行，indent 为正文左边到 HelpField 内容区
// 左边的距离（像素，按钮按它缩进、与说明文字对齐；勾选框一行里勾选框的宽度随说明长短伸缩，不能写成固定值）
export interface HelpFieldState {
  text: string
  id: string
  clipped: boolean
  indent: number
}

// HelpFieldContext 由 HelpField 提供：说明放在设置项的 label 里时，HelpText 只显示正文，展开按钮由 HelpField 放到 label 之后。
// root 返回 HelpField 的根元素，用来量缩进
export interface HelpFieldContext {
  state: HelpFieldState
  root: () => HTMLElement | null
}

export const helpFieldKey: InjectionKey<HelpFieldContext> = Symbol('helpField')

// contentIndent 返回 element 左边到 container 内容区（去掉边框与内边距）左边的距离
export function contentIndent(element: HTMLElement, container: HTMLElement): number {
  const style = getComputedStyle(container)
  const left =
    container.getBoundingClientRect().left +
    (parseFloat(style.borderLeftWidth) || 0) +
    (parseFloat(style.paddingLeft) || 0)
  return Math.max(0, element.getBoundingClientRect().left - left)
}
