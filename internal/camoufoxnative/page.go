package camoufoxnative

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// pageDOMHelpers 定义官网页面共用的可见目标与按钮状态判断
const pageDOMHelpers = `
  const visible = element => element.checkVisibility({visibilityProperty: true});
  const uniqueVisible = (selector, label) => {
    const items = [...document.querySelectorAll(selector)].filter(visible);
    if (items.length > 1) throw new Error(label + ' 匹配多个可见目标');
    return items[0];
  };
  const buttonEnabled = button => !button.matches(':disabled') && !button.closest('[aria-disabled="true"]');
`

// promptReadyExpression 检查登录与生成流程使用的同一个可见输入框
const promptReadyExpression = `(() => {` + pageDOMHelpers + `
  return Boolean(uniqueVisible('ms-prompt-box textarea', '提示词输入框'));
})()`

// workerPageReadyExpression 等待输入框出现，或页面跳转到登录入口、组织停用页（access.workspace.google.com 的 ServiceNotAllowed 等）、
// 停在首次使用页（都立刻结束等待，由 bootstrap 分别处理）
const workerPageReadyExpression = `(location.hostname === 'accounts.google.com' || location.hostname === '` + serviceNotAllowedHost + `' || ` +
	onboardingPageExpression + ` || ` + promptReadyExpression + `)`

// normalizePromptNewlines 把 CRLF/CR 统一成 LF，官网 textarea 会把写入值规范化为 LF
func normalizePromptNewlines(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "\r\n", "\n"), "\r", "\n")
}

// fillPromptExpression 向当前可见提示框写入文本并通知页面表单
func fillPromptExpression(prompt string) string {
	encoded, _ := json.Marshal(prompt)
	return fmt.Sprintf(`(() => {`+pageDOMHelpers+`
  const textarea = uniqueVisible('ms-prompt-box textarea', '提示词输入框');
  if (!textarea) throw new Error('提示词输入框不存在');
  const setter = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set;
  setter.call(textarea, %s);
  textarea.dispatchEvent(new InputEvent('input', {bubbles: true, inputType: 'insertText', data: %s}));
  textarea.dispatchEvent(new Event('change', {bubbles: true}));
  return textarea.value;
})()`, encoded, encoded)
}

// runButtonEnabledExpression 检查官网 Run 按钮是否可见且已启用
const runButtonEnabledExpression = `(() => {` + pageDOMHelpers + `
  const button = uniqueVisible('ms-run-button button', '官网 Run 按钮');
  return Boolean(button && buttonEnabled(button));
})()`

// submitPromptExpression 点击官网当前可见且启用的提交按钮
const submitPromptExpression = `(() => {` + pageDOMHelpers + `
  const button = uniqueVisible('ms-run-button button', '官网 Run 按钮');
  if (!button) throw new Error('官网 Run 按钮不存在');
  if (!buttonEnabled(button)) throw new Error('官网 Run 按钮已禁用');
  button.click();
  return true;
})()`

// dismissOverlaysExpression 关闭官网已知且可交互的启动弹层，返回点击数；不依赖界面语言（原先只认英文 aria-label，中文界面一个都点不到）：
//   - G1 欢迎对话框（ms-g1-welcome-dialog）：每个对话框取第一个可见、可点的关闭按钮（mat-dialog-close 属性，或名字、图标含
//     close、dismiss、关闭、關閉、閉じる）；认不出关闭按钮时，对话框里只有一个可见可点的按钮才点它，有多个（如关闭与开始使用）不猜；
//   - 引导浏览浮层：退出按钮的类名 xap-tour-exit-button 不随语言变化，英文 aria-label 的写法保留；
//   - Cookie 告知条：只在 glue-cookie 条内点，有拒绝按钮时点拒绝，条内没有拒绝按钮时点确认。
//
// 上游改成 'ms-g1-welcome-dialog button' 加 uniqueVisible：对话框里不止一个可见按钮时抛错，经 pageFailure 变成 Worker 启动失败。
// 这里每一步都不抛错，找不到就跳过；bootstrap 用 JS 点击 Run，弹层没关掉也不挡程序化点击
const dismissOverlaysExpression = `(() => {` + pageDOMHelpers + `
  let clicked = 0;
  const clickable = button => {
    try { return visible(button) && buttonEnabled(button); } catch (error) { return false; }
  };
  const name = button => [button.getAttribute('aria-label'), button.getAttribute('title'), button.textContent]
    .filter(Boolean).join(' ').replace(/_/g, ' ').toLowerCase();
  const closeName = /\b(close|dismiss)\b|关闭|關閉|閉じる/;
  const closeButton = button => button.hasAttribute('mat-dialog-close') || closeName.test(name(button));
  const press = button => {
    if (!button) return;
    try { button.click(); clicked++; } catch (error) {}
  };
  const all = (root, selector) => {
    try { return [...root.querySelectorAll(selector)]; } catch (error) { return []; }
  };
  for (const dialog of all(document, 'ms-g1-welcome-dialog')) {
    const buttons = all(dialog, 'button').filter(clickable);
    const close = buttons.find(closeButton);
    press(close || (buttons.length === 1 ? buttons[0] : null));
  }
  const tourExits = [
    ...all(document, 'button.xap-tour-exit-button'),
    ...all(document, 'button[aria-label]').filter(button => button.getAttribute('aria-label') === 'Close guided tour'),
  ];
  press(tourExits.find(clickable));
  const bars = '[id*="glue-cookie-notification-bar"], .glue-cookie-notification-bar';
  for (const bar of all(document, bars)) {
    // 条内带同样 id 前缀的子元素不当作另一条
    if (bar.parentElement && bar.parentElement.closest(bars)) continue;
    const reject = all(bar, '.glue-cookie-notification-bar__reject');
    if (reject.length > 0) {
      press(reject.find(clickable));
    } else {
      press(all(bar, '.glue-cookie-notification-bar__accept').find(clickable));
    }
  }
  return clicked;
})()`

// dismissKnownOverlays 关闭官网已知启动弹层
func dismissKnownOverlays(ctx context.Context, client *bidiClient, contextID string) error {
	if _, err := client.evaluate(ctx, contextID, dismissOverlaysExpression); err != nil {
		return fmt.Errorf("处理 AI Studio 启动覆盖层: %w", err)
	}
	return nil
}
