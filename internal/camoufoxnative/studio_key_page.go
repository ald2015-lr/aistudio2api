package camoufoxnative

import (
	"encoding/json"
)

// 创建 Key 流程在页面里执行的表达式。studioKeyInspectExpression 只读（不点击、不写值、不派发事件、不导航），
// 把页面状态整理成 studioKeyPage 的 JSON；studioKeyTargetExpression 按同一套枚举（序号一致）找出要点的元素，
// 核对文字与可用状态后交给 loginSession.pointerClick 用真实指针点击。页面文字先打码（Key 换成 <key>、邮箱留前两位）再截短。

// studioKeyDOMHelpers 为两类表达式共用的查找函数（依赖 pageDOMHelpers 的 visible 与 buttonEnabled）
var studioKeyDOMHelpers = `
  const clean = value => String(value ?? '').replace(/\s+/g, ' ').trim();
  const mask = text => text
    .replace(/AIza[0-9A-Za-z_-]{4,}|AQ\.[0-9A-Za-z_-]{4,}/g, '<key>')
    .replace(/([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}/g, '$1***@***');
  const short = (value, limit) => {
    const text = mask(clean(value));
    return text.length > limit ? text.slice(0, limit - 1) + '…' : text;
  };
  const pattern = source => new RegExp(source, 'i');
  const keyButtonPattern = ` + studioKeyButtonText + `;
  const createProjectPattern = pattern(` + jsString(studioKeyPatternCreateProject) + `);
  const verifyPattern = pattern(` + jsString(studioKeyPatternVerify) + `);
  const iconSelector = 'mat-icon, .material-icons, .material-symbols-outlined, .material-symbols-rounded, .google-symbols';
  const textOf = element => element ? clean(element.innerText || element.textContent) : '';
  const iconText = element => clean([...element.querySelectorAll(iconSelector)].map(textOf).join(' '));
  // plainText 返回去掉图标 ligature（如 add、close）之后的文字
  const plainText = element => {
    if (!element) return '';
    let text = textOf(element);
    for (const icon of element.querySelectorAll(iconSelector)) {
      const value = textOf(icon);
      if (value) text = text.replace(value, ' ');
    }
    return clean(text);
  };
  // nameOf 为控件的名字：去掉图标后的文字，加上不重复的 aria-label 或 title
  const nameOf = element => {
    const text = plainText(element);
    const aria = clean(element.getAttribute('aria-label') || element.getAttribute('title') || '');
    if (!text) return aria;
    if (!aria || text.includes(aria) || aria.includes(text)) return text;
    return text + ' ' + aria;
  };
  const within = (element, root) => {
    for (let node = element; node; node = node.parentElement) if (node === root) return true;
    return false;
  };
  const dialogSelector = 'mat-dialog-container, [role=dialog], [role=alertdialog]';
  const controlSelector = 'button, a[href], [role=button], [role=link], [role=menuitem]';
  const optionSelector = 'mat-option, [role=option]';
  const panelSelector = optionSelector + ', [role=listbox] button, .mat-mdc-select-panel button, .mat-mdc-autocomplete-panel button, .mat-select-panel button';
  const listPanelSelector = '[role=listbox], .mat-mdc-select-panel, .mat-mdc-autocomplete-panel, .mat-select-panel';
  const spinnerSelector = 'mat-spinner, mat-progress-spinner, mat-progress-bar, [role=progressbar]';
  const selectSelector = 'mat-select, [role=combobox], select';
  const boxSelector = 'input[type=checkbox], [role=checkbox]';
  const boxHostSelector = 'mat-checkbox, mat-mdc-checkbox, .mat-mdc-checkbox, .mat-checkbox';
  // currentDialog 为最上层（文档里最后一个）可见的对话框
  const currentDialog = () => {
    const dialogs = [...document.querySelectorAll(dialogSelector)].filter(visible);
    return dialogs[dialogs.length - 1] || null;
  };
  // dialogControls 为对话框里可见的按钮与链接（文档顺序；嵌在另一个控件、下拉框、下拉选项、勾选框里的不单列）
  const dialogControls = dialog => [...dialog.querySelectorAll(controlSelector)].filter(visible).filter(element => {
    const outer = element.parentElement && element.parentElement.closest(controlSelector);
    if (outer && within(outer, dialog)) return false;
    if (element.closest(selectSelector) || element.closest(optionSelector) || element.closest(boxHostSelector)) return false;
    return !element.matches(boxSelector);
  });
  const optionEnabled = element => buttonEnabled(element) &&
    !element.matches('.mdc-list-item--disabled, .mat-mdc-option-disabled, .mat-option-disabled');
  // panelOptions 为展开着的下拉列表里可见的选项与按钮（嵌在选项里的按钮不单列）
  const panelOptions = () => [...document.querySelectorAll(panelSelector)].filter(visible).filter(element => {
    const parent = element.parentElement;
    return !(parent && parent.closest(optionSelector));
  });
  const dialogSelect = dialog => [...dialog.querySelectorAll(selectSelector)].filter(visible)[0] || null;
  const inputValue = element => typeof element.value === 'string' ? element.value : (element.getAttribute('value') || '');
  const fieldLabel = element => {
    const field = element.closest('mat-form-field, .mat-mdc-form-field');
    return field ? textOf(field.querySelector('mat-label, label')) : '';
  };
  const dialogInputs = dialog => [...dialog.querySelectorAll('input, textarea')].filter(visible).filter(element => {
    const type = (element.getAttribute('type') || 'text').toLowerCase();
    return !['checkbox', 'radio', 'hidden', 'button', 'submit', 'search'].includes(type) && !element.closest(selectSelector);
  });
  // dialogBoxes 为对话框里的勾选框（[role=checkbox] 里包着原生 input 时以 input 为准）；target 为点击位置：原生 input 可见时点它，
  // 否则点所在组件
  const dialogBoxes = dialog => [...dialog.querySelectorAll(boxSelector)]
    .filter(element => !(element.localName !== 'input' && element.querySelector('input[type=checkbox]')))
    .map(element => {
      const host = element.closest(boxHostSelector) || element;
      return {element, host, target: visible(element) ? element : host};
    })
    .filter(item => visible(item.host));
  const boxLabel = item => {
    const parts = [];
    const add = value => {
      const text = clean(value);
      if (text && !parts.includes(text)) parts.push(text);
    };
    for (const id of clean(item.element.getAttribute('aria-labelledby')).split(' ').filter(Boolean)) add(plainText(document.getElementById(id)));
    add(item.element.getAttribute('aria-label'));
    for (const label of item.element.labels || []) add(plainText(label));
    if (item.host !== item.element) add(plainText(item.host));
    return short(parts[0] || '', 80);
  };
  const boxChecked = element => element.localName === 'input' ? Boolean(element.checked) : element.getAttribute('aria-checked') === 'true';
  const boxRequired = item => Boolean(item.element.required) || item.element.getAttribute('aria-required') === 'true' ||
    item.host.hasAttribute('required') || /\*\s*$|^\s*\*/.test(boxLabel(item));
  const boxDisabled = element => Boolean(element.disabled) || Boolean(element.closest('[aria-disabled="true"]'));
  const outsideOverlays = element => !element.closest(dialogSelector + ', .cdk-overlay-container');
  const sameSitePath = element => {
    try {
      const target = new URL(element.getAttribute('href'), location.href);
      return target.hostname === 'aistudio.google.com' ? target.pathname + target.search : '';
    } catch (error) {
      return '';
    }
  };
  // offSite 判断元素是指向站外（不是 aistudio.google.com，如 console.cloud.google.com）的链接：这类链接一律不点
  const offSite = element => {
    if (element.localName !== 'a' || !element.getAttribute('href')) return false;
    try {
      const target = new URL(element.getAttribute('href'), location.href);
      return target.protocol !== 'javascript:' && target.hostname !== 'aistudio.google.com';
    } catch (error) {
      return true;
    }
  };
  // pageCreateButtons 为页面主区域（不在对话框与弹层里）文字像“Create API key”的可见按钮（站外链接除外）
  const pageCreateButtons = () => [...document.querySelectorAll('button, a[role=button], [role=button]')]
    .filter(visible).filter(outsideOverlays).filter(element => !offSite(element))
    .filter(element => keyButtonPattern.test(((element.innerText || '') + ' ' + (element.getAttribute('aria-label') || '')).replace(/\s+/g, ' ').trim()));
  // pageProjectButton 为页面主区域可点的新建项目按钮（Projects 页；站外链接除外）
  const pageProjectButton = () => [...document.querySelectorAll('button, a[role=button], [role=button]')]
    .filter(visible).filter(outsideOverlays).filter(buttonEnabled).filter(element => !offSite(element))
    .find(element => createProjectPattern.test(nameOf(element))) || null;
`

// studioKeyInspectExpression 只读整理页面状态（见 studioKeyPage）
var studioKeyInspectExpression = `(() => {` + pageDOMHelpers + studioKeyDOMHelpers + `
  const dialogs = [...document.querySelectorAll(dialogSelector)].filter(visible);
  const dialog = dialogs[dialogs.length - 1] || null;
  const describe = (element, index, enabled) => {
    const out = {i: index, t: short(nameOf(element), 80), e: enabled};
    const icon = iconText(element);
    if (icon) out.ic = short(icon, 30);
    if (element.localName === 'a' && element.getAttribute('href')) {
      const path = sameSitePath(element);
      if (path) out.href = path;
      else if (offSite(element)) out.ext = true;
    }
    return out;
  };
  const view = {host: location.hostname, path: location.pathname, dialogs: dialogs.length, dialog: null, options: [], alerts: [],
    create: '', project: '', verify: false};
  if (dialog) {
    const heading = [...dialog.querySelectorAll('h1, h2, h3, [mat-dialog-title], .mat-mdc-dialog-title, [role=heading]')]
      .filter(visible).map(textOf).find(Boolean) || '';
    const select = dialogSelect(dialog);
    let selectView = null;
    if (select) {
      let value = '';
      let empty = false;
      let nativeOptions = [];
      let placeholder = clean(select.getAttribute('placeholder') || '');
      if (select.localName === 'input') {
        value = inputValue(select);
        empty = !clean(value);
      } else if (select.localName === 'select') {
        const option = select.selectedOptions && select.selectedOptions[0];
        value = option ? textOf(option) : '';
        empty = !value || !select.value;
        // 原生 select 的下拉列表不在页面 DOM 里：只读出选项文字，用来判断有没有已有项目
        nativeOptions = [...select.querySelectorAll('option')].slice(0, 20)
          .map((item, index) => ({i: index, t: short(item.label || textOf(item) || item.textContent, 80), e: !item.disabled}));
      } else {
        const valueText = select.querySelector('.mat-mdc-select-value-text, .mat-select-value-text');
        const holder = select.querySelector('.mat-mdc-select-placeholder, .mat-select-placeholder');
        if (holder) placeholder = placeholder || textOf(holder);
        value = valueText ? textOf(valueText) : (holder ? '' : plainText(select));
        empty = select.matches('.mat-mdc-select-empty, .mat-select-empty') || !clean(value);
      }
      selectView = {v: short(value, 80), empty, ph: short(placeholder || clean(select.getAttribute('aria-label') || '') || fieldLabel(select), 80),
        dis: Boolean(select.disabled) || select.getAttribute('aria-disabled') === 'true' || select.matches('.mat-mdc-select-disabled, .mat-select-disabled'),
        exp: select.getAttribute('aria-expanded') === 'true'};
      if (nativeOptions.length) selectView.opts = nativeOptions;
    }
    view.dialog = {
      title: short(heading, 80),
      text: short(textOf(dialog), 300),
      buttons: dialogControls(dialog).slice(0, 16).map((element, index) => describe(element, index, buttonEnabled(element))),
      select: selectView,
      inputs: dialogInputs(dialog).slice(0, 6).map((element, index) => ({i: index, empty: !clean(inputValue(element)),
        l: short(element.getAttribute('aria-label') || element.getAttribute('placeholder') || fieldLabel(element), 60),
        req: Boolean(element.required) || element.getAttribute('aria-required') === 'true'})),
      boxes: dialogBoxes(dialog).slice(0, 6).map((item, index) => ({i: index, l: boxLabel(item), on: boxChecked(item.element),
        req: boxRequired(item), dis: boxDisabled(item.element)})),
      error: short([...dialog.querySelectorAll('mat-error, .mat-mdc-form-field-error, [role=alert]')].filter(visible).map(textOf)
        .filter(Boolean).join(' '), 160),
      busy: [...dialog.querySelectorAll(spinnerSelector)].some(visible),
    };
  }
  view.listBusy = [...document.querySelectorAll(listPanelSelector)].filter(visible)
    .some(panel => [...panel.querySelectorAll(spinnerSelector)].some(visible));
  view.options = panelOptions().slice(0, 20).map((element, index) => {
    const out = describe(element, index, optionEnabled(element));
    if (!element.matches(optionSelector)) out.btn = true;
    if (element.getAttribute('aria-selected') === 'true') out.sel = true;
    return out;
  });
  const alerts = [];
  for (const element of document.querySelectorAll('mat-snack-bar-container, .mat-mdc-snack-bar-container, simple-snack-bar, [role=alert]')) {
    if (!visible(element) || (dialog && within(element, dialog))) continue;
    const text = short(textOf(element), 160);
    if (text && !alerts.some(item => item.includes(text) || text.includes(item))) alerts.push(text);
  }
  view.alerts = alerts.slice(0, 4);
  const creates = pageCreateButtons();
  view.create = creates.some(buttonEnabled) ? 'enabled' : creates.length ? 'disabled' : '';
  const projectButton = pageProjectButton();
  view.project = projectButton ? short(nameOf(projectButton), 80) : '';
  view.verify = [...document.querySelectorAll('a[href*="verify-account"]')].some(visible) ||
    [...document.querySelectorAll('[role=alert], [role=status], ms-global-banner, mat-banner')].filter(visible).some(element => verifyPattern.test(textOf(element)));
  return JSON.stringify(view);
})()`

// studioKeyTargetExpression 返回 target 指向的元素：按与 studioKeyInspectExpression 相同的枚举取序号，文字一致、仍可用、不是站外链接时
// 才返回，否则为 null（页面变了就不点）
func studioKeyTargetExpression(target studioKeyTarget) string {
	encoded, _ := json.Marshal(target)
	return `((target) => {` + pageDOMHelpers + studioKeyDOMHelpers + `
  const dialog = currentDialog();
  switch (target.scope) {
    case 'select':
      return dialog ? dialogSelect(dialog) : null;
    case 'page-create':
      return pageCreateButtons().find(buttonEnabled) || null;
    case 'page-project': {
      const button = pageProjectButton();
      return button && short(nameOf(button), 80) === target.text ? button : null;
    }
    case 'dialog': {
      const element = dialog ? dialogControls(dialog)[target.index] : null;
      return element && !offSite(element) && buttonEnabled(element) && short(nameOf(element), 80) === target.text ? element : null;
    }
    case 'option': {
      const element = panelOptions()[target.index];
      return element && !offSite(element) && optionEnabled(element) && short(nameOf(element), 80) === target.text ? element : null;
    }
    case 'box': {
      const item = dialog ? dialogBoxes(dialog)[target.index] : null;
      return item && !boxChecked(item.element) && !boxDisabled(item.element) && boxLabel(item) === target.text ? item.target : null;
    }
  }
  return null;
})(` + string(encoded) + `)`
}

// jsString 把字符串编码成 JS 字符串字面量
func jsString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
