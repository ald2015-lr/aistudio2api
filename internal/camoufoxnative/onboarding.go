package camoufoxnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// OnboardingFailedPrefix 为没能自动完成 AI Studio 首次使用页的错误文案前缀，完整文案为
// "AI Studio 首次使用页未能完成: <原因>[ | 已做=…][ | 页面=…][ | 结构=…]"；与缓存无关，不算页面状态类（见 PageStateFailure）
const OnboardingFailedPrefix = "AI Studio 首次使用页未能完成"

// OnboardingProgressMarker 为首次使用页进度记录的开头：接在没能完成的原因之后、页面快照之前，单行、不含“ | ”，
// 按顺序列出这次做过的每一步（见 OnboardingResult.Progress）；这次什么都没做时为“-”
const OnboardingProgressMarker = " | 已做="

// OnboardingStructureMarker 为首次使用页结构摘要的开头：接在页面快照之后，单行 JSON、不含“ | ”，列出查找范围、当前一步所在的 ms- 组件、
// 范围里可见的 ms- 组件、整页可见与全部勾选框数、各勾选框（是否必选、是否已勾、是否不可勾、必选依据、关键词标记、是否用途调查选项、类别、
// 标签开头与结尾）、各按钮（本身是勾选框的除外；文字前 40 字、是否启用、不点的原因、是否不在当前一步）、范围内的链接与其他表单控件、
// 范围外的按钮，文字已打码（见 onboardingSummary）
const OnboardingStructureMarker = " | 结构="

const (
	// onboardingCompletionLimit 为一次启动（或一次验证、登录）里自动完成首次使用页的次数上限，防止来回跳转死循环
	onboardingCompletionLimit = 2
	// onboardingMaxBoxClicks 为一次完成里勾选的次数上限：勾了仍显示未勾选时不无限重试
	onboardingMaxBoxClicks = 12
	// onboardingMaxButtonClicks 为一次完成里点击继续按钮的次数上限（多步向导每一步各点一次）
	onboardingMaxButtonClicks = 8
	// onboardingMaxSkipClicks 为一次完成里点击跳过的次数上限（只在不涉及同意的步骤里点，单独计数，不占点击继续的名额）
	onboardingMaxSkipClicks = 4
	// onboardingProgressItems 为进度记录写进日志的步数上限：超过时保留开头与结尾几步（见 OnboardingResult.Progress）
	onboardingProgressItems = 12
	// onboardingStructureLimit 为结构摘要 JSON 的长度上限（按字符计）
	onboardingStructureLimit = 900
)

// onboardingTiming 为自动完成首次使用页的各项时限（测试里缩短）：total 为一次完成的总时限（多步向导要连续走完几步，仍受 ctx 约束）；
// poll 为两步之间的间隔，settle 为点击继续或跳过之后等页面响应的间隔；retry 为两次点击（继续或跳过）的最短间隔；relax 为必选项都勾上后
// 按钮一直禁用、开始把标签含服务条款或年龄的项视为必选的等待（从最后一次勾选算起；点过继续仍停在首次使用页时同样放宽）；stuck 为找不到
// 可点的东西持续多久算失败；skip 为不涉及同意的一步里继续按钮不存在或一直禁用、持续多久才点跳过（页面渲染稳定，刚渲染时短暂禁用的
// 继续按钮不被跳过抢先）；saved 为勾选或点击过、离开首次使用页之后再等的时长：官网可能先切路由、再异步提交服务条款与年龄确认，
// 立刻整页重新导航或关闭浏览器会中断这个请求
var onboardingTiming = struct {
	total, poll, settle, retry, relax, stuck, skip, saved time.Duration
}{
	total:  75 * time.Second,
	poll:   500 * time.Millisecond,
	settle: time.Second,
	retry:  3 * time.Second,
	relax:  5 * time.Second,
	stuck:  10 * time.Second,
	skip:   2 * time.Second,
	saved:  2 * time.Second,
}

// onboardingDetectHelpers 定义首次使用页的识别与勾选框的可见性（依赖 pageDOMHelpers 的 visible）
const onboardingDetectHelpers = `
  const onboardingBoxSelector = 'input[type=checkbox], [role=checkbox]';
  const boxHostSelector = 'mat-checkbox, mat-mdc-checkbox, .mat-mdc-checkbox, .mat-checkbox';
  // boxHost 返回勾选框所在的组件（mat-checkbox 等，原生 input 外层的 [role=checkbox]），没有时为 null
  const boxHost = element => element.closest(boxHostSelector) || (element.localName === 'input' ? element.closest('[role=checkbox]') : null);
  // boxLabels 返回原生 input 关联的 label（element.labels 与包着它的 label），其他勾选框为空
  const boxLabels = element => {
    if (element.localName !== 'input') return [];
    const labels = element.labels ? [...element.labels] : [];
    const wrapping = element.closest('label');
    if (wrapping && !labels.includes(wrapping)) labels.push(wrapping);
    return labels;
  };
  // boxShown 返回勾选框是否算可见：自己或所在组件可见；原生 input 被隐藏（display:none，只显示自绘的样式）时关联的 label 可见也算
  // （点隐藏的 input 照样切换状态并派发 change）
  const boxShown = element => {
    if (visible(element)) return true;
    const host = boxHost(element);
    return Boolean(host && visible(host)) || boxLabels(element).some(visible);
  };
  // onboardingPage 判断页面是否停在 AI Studio 首次使用页：路径以 /onboarding 开头（服务端直接落到，或 SPA 路由过去），
  // 或没有可见的提示词输入框、却有带可见勾选框的首次使用组件（ms-onboarding、ms-tos-disclaimer）。
  // 对话页上同名的引导或条款说明组件不带勾选框、或与输入框同时可见，不算，免得在正常页面上点按钮
  const onboardingPage = () => {
    if (location.hostname !== 'aistudio.google.com') return false;
    if (location.pathname.startsWith('/onboarding')) return true;
    if ([...document.querySelectorAll('ms-prompt-box textarea')].some(visible)) return false;
    return [...document.querySelectorAll('ms-onboarding, ms-tos-disclaimer')]
      .some(element => visible(element) && [...element.querySelectorAll(onboardingBoxSelector)].some(boxShown));
  };
`

// onboardingPageExpression 只读判断页面是否停在首次使用页
const onboardingPageExpression = `(() => {` + pageDOMHelpers + onboardingDetectHelpers + `
  return onboardingPage();
})()`

// onboardingStepHelpers 定义首次使用页里勾选框与继续按钮的查找与判定（依赖 pageDOMHelpers 与 onboardingDetectHelpers）。
// 页面文字先打码再截短（与快照表达式的 mask、short 规则一致）
const onboardingStepHelpers = `
  const clean = value => String(value ?? '').replace(/\s+/g, ' ').trim();
  const mask = text => text.replace(/([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}/g, '$1***@***');
  const short = (value, limit) => {
    const text = mask(clean(value));
    return text.length > limit ? text.slice(0, limit - 1) + '…' : text;
  };
  // clip 打码后保留开头 head 个与结尾 tail 个字符、中间以“…”省略（标签结尾的“*”与关键词不会被截掉）
  const clip = (value, head, tail) => {
    const chars = [...mask(clean(value))];
    return chars.length > head + tail + 1 ? chars.slice(0, head).join('') + '…' + chars.slice(-tail).join('') : chars.join('');
  };
  const textOf = element => element ? clean(element.innerText || element.textContent) : '';
  // 图标（mat-icon 等图标字体，文字是 ligature，如“close”“check_box_outline_blank”）
  const iconSelector = 'mat-icon, .material-icons, .material-symbols-outlined, .material-symbols-rounded, .google-symbols';
  const iconText = element => clean([...element.querySelectorAll(iconSelector)].map(textOf).join(' '));
  // plainText 返回元素去掉图标 ligature 之后的文字：自绘勾选框里的“check_box_outline_blank”不算标签
  const plainText = element => {
    if (!element) return '';
    let text = textOf(element);
    for (const icon of element.querySelectorAll(iconSelector)) {
      const ligature = textOf(icon);
      if (ligature) text = text.replace(ligature, ' ');
    }
    return clean(text);
  };
  // inside 返回 node 是否在 root 里（含 root 自己）
  const inside = (node, root) => {
    for (; node; node = node.parentElement) if (node === root) return true;
    return false;
  };
  const requiredMarkerSelector = '.required, .required-marker, .mat-mdc-form-field-required-marker, .mat-form-field-required-marker, .mdc-floating-label--required';
  // 必填标记：标签文字去空白后以“*”开头或结尾，或带括号的“必填”字样
  const starred = text => /^[*＊]|[*＊]$/.test(text);
  const requiredWord = /[(（]\s*(required|必須|必须|必填|必需)\s*[)）]/i;
  const marked = text => starred(text) || requiredWord.test(text);
  // termsWord 为服务条款与年龄确认的关键词，optionalWord 为邮件订阅、调研邀请、通知类的关键词：标签含这类词的勾选框一律不勾
  // （订阅类选项常用尾部“*”引出脚注，带星号也不算必选），按钮名含这类词的按钮也不点
  const termsWord = /terms|利用規約|規約|条款|條款|약관|nutzungsbedingungen|términos|termos|(^|[^0-9])18([^0-9]|$)/i;
  const optionalWord = /e[-\s]?mail|newsletter|subscri|research|survey|feedback|notif|updates|offers|marketing|promotion|\bnews\b|\btips\b|メール|ニュース|調査|アンケート|通知|最新情報|お知らせ|配信|購読|受け取る|邮件|郵件|订阅|訂閱|调研|調研|调查|調查|资讯|資訊|推广|推廣/i;
  // subscribeWord 为选择加入、注册类按钮名（可选的订阅步骤里的主按钮）：这类按钮不点
  const subscribeWord = /\b(sign me up|sign up|opt in)\b|登録|注册|註冊/i;
  // 关闭、返回、取消、拒绝、不同意、跳过、稍后、退出登录与切换账号类按钮不当作继续按钮（按 aria-label、title 与文字判断，见 buttonWords）；
  // 含没有 aria-label 的图标按钮的 ligature（close_small、arrow_back、chevron_left 等，下划线已换成空格）。skipWord 里的跳过类词都在这里
  // （不会被当作唯一剩下的按钮点击），只在不涉及同意、有用途调查证据的一步里、继续按钮不存在或一直禁用时另按 skipButton 点（见一步式表达式）
  const negativeWord = /\b(close|back|cancel|decline|reject|dismiss|skip|no thanks|not now|not you|later|disagree|do not agree|don'?t agree|not agree|do not accept|don'?t accept|not accept|sign out|log out|logout|switch account|add account|manage account|google account|use another account|use a different account|chevron left|navigate before|arrow left|keyboard backspace)\b|閉じる|戻る|キャンセル|拒否|しない|しません|後で|スキップ|ログアウト|アカウントを切り替え|アカウントを追加|別のアカウント|Google アカウント|关闭|關閉|返回|取消|拒绝|拒絕|不同意|不接受|稍后|稍後|跳过|跳過|退出|登出|切换账号|切换帐号|切換帳號|切换账户|其他账号|其他帐号|其他帳號|其他账户|Google 账号|Google 帐号|Google 帳戶|略过|略過|건너뛰기/i;
  // 继续类关键词：多步向导结尾页的“Start building”“开始使用”“开始构建”“Let's go”“完成”“Done”也算（跳过类词不算）
  const positiveWord = /\b(continue|get started|accept|agree|i agree|try|next|start|confirm|done|finish|let[’']?s go)\b|続行|続ける|同意|次へ|開始|始める|始めよう|始めましょう|はじめる|はじめよう|試す|使ってみる|完了|继续|繼續|开始|接受|下一步|确认|確認|完成/i;
  // skipWord 为跳过类按钮名（规范化后整个名字是其中之一，见 skipButton）：只在不涉及同意、有用途调查证据的一步里点
  const skipWord = /^(skip|skip for now|skip this step|跳过|暂时跳过|先跳过|略过|跳過|略過|スキップ|今はスキップ|건너뛰기)$/;
  // mainContentWord 为“Skip to main content”“跳到主要内容”这类无障碍跳转的文字：不算跳过按钮
  const mainContentWord = /main content|主要内容|主要內容|メイン ?コンテンツ|본문/i;
  // onboardingComponents 返回可见的首次使用组件 [元素, 名字]：ms-onboarding 在前，ms-tos-disclaimer 在后
  const onboardingComponents = () => ['ms-onboarding', 'ms-tos-disclaimer']
    .flatMap(name => [...document.querySelectorAll(name)].filter(visible).map(element => [element, name]));
  // onboardingScope 返回查找的作用域、名字与作用域之外其余可见的首次使用组件。可见的首次使用组件里优先取含可见勾选框的
  // （ms-onboarding 与 ms-tos-disclaimer 可能是兄弟节点，必选项在后者里），都不含勾选框时取第一个（ms-onboarding 优先）；
  // 没有首次使用组件时为 document.body
  const onboardingScope = () => {
    const components = onboardingComponents();
    const chosen = components.find(([element]) => boxOwners(element).length) || components[0];
    if (!chosen) return [document.body, 'body', []];
    const others = components.map(([element]) => element).filter(element => !inside(element, chosen[0]) && !inside(chosen[0], element));
    return [chosen[0], chosen[1], others];
  };
  // boxOwners 收集 root 里可见的勾选框（见 boxShown；all 为真时含不可见的）：原生 input[type=checkbox]（含 mat-checkbox / mat-mdc-checkbox 里的）
  // 与 [role=checkbox]；[role=checkbox] 里包着原生 input 时以 input 为准，同一组件只算一个
  const boxOwners = (root, all = false) => {
    const owners = [];
    for (const element of root.querySelectorAll(onboardingBoxSelector)) {
      if (element.localName !== 'input' && element.querySelector('input[type=checkbox]')) continue;
      const host = boxHost(element);
      const owner = host || element;
      if (owners.some(item => item.owner === owner)) continue;
      if (!all && !boxShown(element)) continue;
      owners.push({element, host, owner});
    }
    return owners;
  };
  // describeBox 读出勾选框的状态、标签文字、是否必选与必选依据 basis。显式属性：input.required 或所在组件带 required 属性为 required，
  // aria-required="true" 为 aria。文字判定：标签文字（aria-labelledby 指向的文字、aria-label、label、所在组件的文字，都去掉图标 ligature）
  // 带必填标记为 star，含必填标记元素为 marker；标签没有关联到勾选框（文字是勾选框的兄弟元素，或勾选框里只有图标）时，往上找第一个
  // 有文字的容器，只在它只包着这一个勾选框时采用其文字，带必填标记为 ctx。文字判定不采用标签含邮件、订阅、调研、通知类关键词的项
  // （见 optionalWord）。另返回结构摘要用的标记：terms 为标签含服务条款或年龄关键词，optional 为含邮件通知类关键词，hidden 为勾选框自己
  // 不可见（靠所在组件或 label 算可见），kind 为 input 或 role 加所在组件的标签名；own 为标签取自勾选框自己（aria-labelledby、aria-label、
  // 关联的 label、自己与所在组件的文字），不是往上找的容器文字
  const describeBox = ({element, host}) => {
    const isInput = element.localName === 'input';
    const checked = isInput ? Boolean(element.checked) : element.getAttribute('aria-checked') === 'true';
    const disabled = (isInput && Boolean(element.disabled)) || Boolean(element.closest('[aria-disabled="true"]'));
    const owners = host && host !== element ? [element, host] : [element];
    const containers = owners.filter(owner => owner !== element || !isInput);
    const direct = [];
    const add = value => {
      const text = clean(value);
      if (text && !direct.includes(text)) direct.push(text);
    };
    for (const owner of owners) {
      for (const id of clean(owner.getAttribute('aria-labelledby')).split(' ').filter(Boolean)) add(plainText(document.getElementById(id)));
      add(owner.getAttribute('aria-label'));
    }
    const labels = boxLabels(element);
    for (const label of labels) add(plainText(label));
    for (const container of containers) add(plainText(container));
    let context = '';
    if (!direct.length) {
      // 不可见的勾选框（整页查是否涉及同意时）按容器里的全部勾选框数判断
      const all = !boxShown(element);
      for (let node = (host || element).parentElement, depth = 0; node && depth < 4; node = node.parentElement, depth++) {
        const text = plainText(node);
        if (!text) continue;
        if (boxOwners(node, all).length === 1) context = text;
        break;
      }
    }
    const text = direct.join(' ') || context;
    const optional = optionalWord.test(text);
    const explicit = isInput && Boolean(element.required) ? 'required'
      : owners.some(owner => owner.getAttribute('aria-required') === 'true') ? 'aria'
      : owners.some(owner => owner !== element && owner.hasAttribute('required') && owner.getAttribute('required') !== 'false') ? 'required' : '';
    const textual = optional ? ''
      : direct.some(marked) ? 'star'
      : labels.concat(containers).some(root => root.querySelector(requiredMarkerSelector)) ? 'marker'
      : context && marked(context) ? 'ctx' : '';
    const basis = explicit || textual;
    const kind = (isInput ? 'input' : 'role') + (host && host !== element ? '/' + host.localName : isInput ? '' : '/' + element.localName);
    return {element, checked, disabled, required: Boolean(basis), basis, label: direct[0] || context,
      terms: termsWord.test(text), optional, hidden: !visible(element), kind, own: direct.length > 0};
  };
  // intentOption 返回勾选框是否是用途调查的选项：ms-intent-selection 里、标签取自按钮自己且非空的 button[role=checkbox]（线上为带
  // add_circle 图标的按钮）。不在 ms-intent-selection 里的，或标签认不出（按钮里只有图标、aria-labelledby 指向的标签还没渲染、
  // 只能往上取容器文字）的 button[role=checkbox] 不算：条款、年龄勾选框也可能写成这种按钮，按涉及同意处理
  const intentOption = box => box.kind === 'role/button' && box.own && Boolean(box.label) && Boolean(box.element.closest('ms-intent-selection'));
  // 图标按钮（文字只有图标的 ligature，如“close”）的文字取 aria-label 或 title
  const buttonText = button => {
    const text = textOf(button);
    const label = clean(button.getAttribute('aria-label') || button.getAttribute('title'));
    return label && (!text || text === iconText(button)) ? label : text;
  };
  const buttonName = button => clean([button.getAttribute('aria-label'), button.getAttribute('title'), textOf(button)].filter(Boolean).join(' '));
  // actionText 为写进进度记录的按钮文字：去掉图标 ligature（“继续 arrow_forward”记为“继续”），只有图标时取 aria-label 或 title
  const actionText = button => short(plainText(button) || buttonText(button), 40);
  // buttonWords 为按关键词判断用的按钮名：图标 ligature 的下划线与标点换成空格（close_small、arrow_back、“No, thanks”才能按词匹配）
  const buttonWords = button => clean(buttonName(button).replace(/[_\-,.!?;:、，。！？；：]/g, ' '));
  // unnamedButton 返回按钮是否认不出是什么：没有 aria-label、title，文字为空或只有图标的 ligature（如 chevron_left）
  const unnamedButton = button => {
    if (clean(button.getAttribute('aria-label') || button.getAttribute('title'))) return false;
    const text = textOf(button);
    return !text || text === iconText(button);
  };
  // 勾选框内部与标签里的按钮、页头导航与全局横幅里的按钮、开关不点
  const excludedButtonArea = 'mat-checkbox, mat-mdc-checkbox, .mat-mdc-checkbox, .mat-checkbox, [role=checkbox], [role=switch], label, header, nav, [role=navigation], [role=banner], mat-toolbar, ms-global-banner';
  // cookieArea 返回按钮是否在 Cookie 同意横幅里（Google 的 glue-cookie-notification-bar 等：按钮或祖先的 id、class、aria-label 含 cookie）
  const cookieArea = button => {
    for (let node = button; node && node !== document.body && node !== document.documentElement; node = node.parentElement) {
      if (/cookie/i.test([node.getAttribute('id'), node.getAttribute('class'), node.getAttribute('aria-label')].join(' '))) return true;
    }
    return false;
  };
  // popupButton 返回按钮是否是菜单与展开类（aria-haspopup、aria-expanded）
  const popupButton = button => (button.hasAttribute('aria-haspopup') && button.getAttribute('aria-haspopup') !== 'false') || button.hasAttribute('aria-expanded');
  // normalizedName 返回规范化的按钮名：小写，下划线与标点换成空格
  const normalizedName = value => clean(String(value ?? '').replace(/[_\-,.!?;:、，。！？；：…]/g, ' ')).toLowerCase();
  // previewArea 返回按钮是否在轮播或卡片预览里：名字含 carousel、slide、cards、preview 的 ms- 组件（如 ms-stacked-cards-preview），
  // aria-roledescription 为 carousel、slide 的容器，标签页（role=tab、tablist）
  const previewTag = /^ms-[\w-]*(carousel|slide|cards|preview)|(^|-)(carousel|swiper)(-|$)/;
  const previewArea = button => {
    for (let node = button; node && node !== document.body && node !== document.documentElement; node = node.parentElement) {
      if (previewTag.test(node.localName) || /carousel|slide/i.test(node.getAttribute('aria-roledescription') || '') ||
        ['tab', 'tablist'].includes(node.getAttribute('role'))) return true;
    }
    return false;
  };
  // continueName 为整个名字就是继续类短语（规范化、去掉图标之后）；previewContinue 返回轮播与卡片预览里的按钮是否仍照常参与挑选：
  // 只有文字（不算 aria-label）整句是继续类短语的 button 元素。卡片（div[role=button] 等）与介绍文字里常带 try、start、开始、完成，不算
  const continueName = /^(continue|next|get started|start|start building|start now|done|finish|let[’']?s go|继续|繼續|下一步|开始|开始使用|开始构建|立即开始|完成|続行|続ける|次へ|開始|始める|始めよう|完了)$/;
  const previewContinue = button => button.localName === 'button' && continueName.test(normalizedName(plainText(button)));
  // skipReason 返回按钮不点的原因（写进结构摘要），可以点时为空串：link 为链接（a[href]）；area 为勾选框、开关、标签、页头导航与全局横幅里的；
  // cookie 为 Cookie 同意横幅里的；popup 为菜单与展开类；card 为轮播与卡片预览里的卡片、“前往第 N 张幻灯片”类按钮（整句是继续类短语的
  // button 照常参与，见 previewContinue）；
  // neg 为关闭、返回、取消、拒绝、跳过、稍后、退出登录与切换账号类，以及“跳到主要内容”这类无障碍跳转；opt 为订阅、邮件、调研、通知与
  // 注册类（按 aria-label、title 与文字判断）；icon 为认不出是什么的按钮（只有图标、没有文字与 aria-label）
  const skipReason = button => {
    if (button.closest('a[href]')) return 'link';
    if (button.closest(excludedButtonArea)) return 'area';
    if (cookieArea(button)) return 'cookie';
    if (popupButton(button)) return 'popup';
    if (previewArea(button) && !previewContinue(button)) return 'card';
    const words = buttonWords(button);
    if (negativeWord.test(words) || mainContentWord.test(words)) return 'neg';
    if (optionalWord.test(words) || subscribeWord.test(words)) return 'opt';
    if (unnamedButton(button)) return 'icon';
    return '';
  };
  const buttonSkipped = button => Boolean(skipReason(button));
  // skipButton 返回按钮是否是可点的跳过按钮：可见、启用，aria-label、title 或去掉图标后的文字规范化后是跳过类词（见 skipWord）；
  // 链接（a[href]）、勾选框与页头横幅里的、Cookie 横幅里的、菜单与展开类、轮播与卡片预览里的（见 previewArea：如跳过卡片动画，
  // 点了向导不前进）、无障碍跳转（名字含 main content、主要内容等）不算
  const skipButton = button => {
    if (!visible(button) || !buttonEnabled(button) || button.closest('a[href]') || button.closest(excludedButtonArea) ||
      cookieArea(button) || popupButton(button) || previewArea(button)) return false;
    const names = [button.getAttribute('aria-label'), button.getAttribute('title'), plainText(button)].map(normalizedName).filter(Boolean);
    return !names.some(name => mainContentWord.test(name)) && names.some(name => skipWord.test(name));
  };
  // visibleButtons 收集 root 里可见的按钮（button 与 [role=button]，包着 button 的 [role=button] 以里面的为准）
  const visibleButtons = root => [...root.querySelectorAll('button, [role=button]')]
    .filter(button => visible(button) && !(button.localName !== 'button' && button.querySelector('button')));
  const primaryClass = /(^|\s)(mat-flat-button|mat-mdc-unelevated-button|mat-raised-button|mat-mdc-raised-button|mat-primary|ms-button-primary|ms-button-filled|primary|primary-button|button-primary)(\s|$)/;
  const primaryStyle = button => primaryClass.test(button.getAttribute('class') || '') || button.getAttribute('color') === 'primary' ||
    ['mat-flat-button', 'mat-raised-button', 'mat-unelevated-button'].some(name => button.hasAttribute(name)) ||
    /^(primary|filled)$/i.test(button.getAttribute('variant') || '');
  const continueButton = button => positiveWord.test(buttonWords(button));
  // offscreen 返回元素是否整个在视口左右之外（多步向导把其余各步平移到视口外时）：只在取得到非零尺寸与视口宽度时判断
  const offscreen = element => {
    if (typeof element.getBoundingClientRect !== 'function') return false;
    const rect = element.getBoundingClientRect();
    const width = Number(globalThis.innerWidth) || 0;
    return width > 0 && rect.width > 0 && rect.height > 0 && (rect.right <= 0 || rect.left >= width);
  };
  // shownComponent 返回组件是否显示出来：visibility 不是 hidden、collapse，自己与祖先都没被 display:none 隐藏。宿主为 display:contents 时
  // checkVisibility 为假（没有自己的盒子），这里照样算；上一步留在 DOM 里、已隐藏的组件不算
  const shownComponent = element => {
    if (['hidden', 'collapse'].includes(getComputedStyle(element).visibility)) return false;
    for (let node = element; node; node = node.parentElement) if (getComputedStyle(node).display === 'none') return false;
    return true;
  };
  // currentStep 返回元素是否在多步向导的当前一步里：不在 aria-hidden、inert 容器里，也不整个在视口左右之外
  // （多步向导可能把各步都渲染出来、只把其余各步平移到视口外或放进 aria-hidden 容器，checkVisibility 仍算可见）
  const currentStep = element => !element.closest('[aria-hidden="true"], [inert]') && !offscreen(element);
  // keywordButton 只按文字关键词找第一个没被排除的继续按钮（候选已按当前一步收窄，见一步式表达式）
  const keywordButton = buttons => buttons.find(button => !buttonSkipped(button) && continueButton(button)) || null;
  // pickButton 从没被排除的候选（已按当前一步收窄）里挑继续按钮：有文字含继续类关键词的候选时只在它们里面挑（主按钮样式的“詳細”
  // 不抢在普通样式的“続行”前面），依次取 type=submit、主按钮样式、唯一剩下的按钮（认不出是什么的图标按钮已在 skipReason 里排除）；
  // 同一级有多个时看下一级，都挑不出时取第一个含关键词的
  const pickButton = buttons => {
    const keyword = buttons.filter(continueButton);
    const pool = keyword.length ? keyword : buttons;
    const tiers = [
      pool.filter(button => (button.getAttribute('type') || '').toLowerCase() === 'submit'),
      pool.filter(primaryStyle),
      pool.length === 1 ? pool : [],
    ];
    for (const tier of tiers) {
      if (tier.length === 1) return tier[0];
    }
    return keyword[0] || null;
  };
  // 作用域所在的对话框（首次使用组件渲染在 Material 对话框里时，操作按钮可能在组件之外、对话框之内）
  const dialogSelector = 'mat-dialog-container, .mat-mdc-dialog-container, .mat-dialog-container, cdk-dialog-container, .cdk-overlay-pane, [role=dialog], [role=alertdialog]';
  // 勾选框以外的表单控件（结构摘要用：继续按钮可能被必填的单选、下拉、日期或开关挡住）
  const controlSelector = 'input, select, textarea, mat-select, [role=radio], [role=switch], [role=combobox], [role=listbox], [role=slider], [role=spinbutton]';
  const formControl = element => !(element.localName === 'input' && ['checkbox', 'hidden'].includes((element.getAttribute('type') || '').toLowerCase())) &&
    !element.matches('[role=checkbox]');
  // controlKind 返回表单控件的类别：input 带 type，自定义组件取标签名，其余带 role 的取 role
  const controlKind = element => element.localName === 'input' ? 'input:' + ((element.getAttribute('type') || 'text').toLowerCase())
    : element.localName.includes('-') ? element.localName
    : element.getAttribute('role') ? 'role:' + element.getAttribute('role') : element.localName;
`

// onboardingStepOptions 为一步的参数：Click 为可以点击继续按钮（距上一次点击继续或跳过已超过 retry）；Relaxed 为距最后一次勾选
// 已超过 relax，必选项都勾上后按钮仍禁用、找不到按钮，或点过继续（Clicked：距上一次点击已超过 retry）仍停在首次使用页时，可以把
// 标签含服务条款或年龄的项视为必选；Skip 为可以点跳过（这一步不涉及同意、有用途调查类的证据、继续按钮不存在或禁用、当前一步有跳过按钮
// 已持续 skip，且距上一次点击已超过 retry，由 Go 侧计时）；Observe 为只读取结构摘要、不点击任何东西
type onboardingStepOptions struct {
	Click   bool `json:"click"`
	Relaxed bool `json:"relaxed"`
	Clicked bool `json:"clicked"`
	Skip    bool `json:"skip"`
	Observe bool `json:"observe"`
}

// onboardingStepExpression 在首次使用页上做一小步并返回 JSON 状态：已离开首次使用页为 left；勾了一个必选项为 checked
// （一步只勾一个，让页面处理完 change 再看下一个）；点了继续按钮为 clicked；在不涉及同意的一步里点了跳过为 skipped；页面正在载入、
// 首次使用页尚未渲染、必选项不可勾、按钮禁用或刚点过为 waiting；找不到可点的继续按钮为 stuck。left 与页面正在载入之外都附结构摘要 summary；
// 不涉及同意、有用途调查类证据的一步里继续按钮不存在或禁用、当前一步有可点的跳过按钮时，stuck 与 waiting 另附跳过按钮的文字 skip
// （Go 侧据此计时，见 Skip）。可选项（邮件订阅、调研邀请等）与用途调查的选项一律不碰，链接、Cookie 横幅、关闭与订阅类按钮、轮播与卡片、
// 认不出是什么的图标按钮不点；多步向导把各步都渲染出来时只在当前一步里挑（见 currentStep）
func onboardingStepExpression(options onboardingStepOptions) string {
	encoded, _ := json.Marshal(options)
	// 不用 fmt.Sprintf：辅助脚本的正则里有“%”
	return `(() => {` + pageDOMHelpers + onboardingDetectHelpers + onboardingStepHelpers + `
  const options = ` + string(encoded) + `;` + onboardingStepBody
}

// onboardingStepBody 为一步表达式在 options 之后的部分（见 onboardingStepExpression）
const onboardingStepBody = `
  if (!onboardingPage()) return JSON.stringify({state: 'left'});
  // 新文档还在解析 head（body 为 null）：点击继续后整页跳到下一步、登录与验证轮询时都可能碰上，等它载入
  if (!document.body) return JSON.stringify({state: 'waiting', reason: '页面正在载入'});
  const [scope, scopeName, others] = onboardingScope();
  const boxes = boxOwners(scope).map(describeBox);
  const scoped = visibleButtons(scope);
  const links = [...scope.querySelectorAll('a[href]')].filter(link => visible(link) && !scoped.includes(link));
  // 作用域里有当前一步（见 currentStep）的按钮或链接时只在当前一步里挑继续按钮；一个都没有时（取不到尺寸等）照常用全部候选
  const narrow = (list, any) => any ? list.filter(currentStep) : list;
  const hasCurrent = scoped.concat(links).some(currentStep);
  let shown = scoped;
  let button = null;
  if (scope === document.body) {
    // 作用域退到整页时（没有首次使用组件）只按关键词找：整页上还有侧栏等无关按钮，不按样式或唯一性猜
    button = keywordButton(narrow(shown, hasCurrent));
  } else {
    button = pickButton(narrow(shown.filter(item => !buttonSkipped(item)), hasCurrent));
    if (!button) {
      // 作用域里挑不出：在其余首次使用组件与作用域所在的对话框里按同样的规则找，不到整页上找（整页上有 Cookie 横幅、侧栏等无关按钮）
      const dialog = scope.closest(dialogSelector);
      const extra = [];
      for (const area of dialog ? others.concat([dialog]) : others) {
        for (const item of visibleButtons(area)) if (!shown.includes(item) && !extra.includes(item)) extra.push(item);
      }
      button = pickButton(narrow(extra.filter(item => !buttonSkipped(item)), hasCurrent || extra.some(currentStep)));
      shown = shown.concat(extra);
    }
  }
  // 这一步是否涉及同意（涉及时绝不点跳过），下面任一条成立就算：作用域为 ms-tos-disclaimer；作用域里当前一步有不是用途调查选项
  // （见 intentOption）的勾选框（原生 input、mat-checkbox、role=checkbox 的 div，ms-intent-selection 之外或标签认不出的
  // button[role=checkbox] 等，标签认不出也算，认出是邮件通知类的除外）；整页当前一步里可见的勾选框有必选或标签含服务条款、年龄关键词的
  // （不论是否已勾）；整页全部勾选框（含不可见的：条款内容可能正在显示过渡中）有未勾选的必选或服务条款、年龄类的；整页上有显示出来
  // （见 shownComponent，宿主为 display:contents 时 checkVisibility 为假也算）、在当前一步里的服务条款组件（ms-tos-disclaimer）
  const consentBox = box => box.required || box.terms;
  const consent = scopeName === 'ms-tos-disclaimer' ||
    boxes.some(box => currentStep(box.element) && !intentOption(box) && !box.optional) ||
    boxOwners(document.body, true).map(describeBox)
      .some(box => consentBox(box) && (!box.checked || (boxShown(box.element) && currentStep(box.element)))) ||
    [...document.querySelectorAll('ms-tos-disclaimer')].some(element => shownComponent(element) && currentStep(element));
  // 点跳过还要有这一步是用途调查类的正向证据：当前一步里有可见的 ms-intent-selection，或有非必选、标签不含服务条款与年龄关键词的
  // 用途调查选项（ms-intent-selection 为 display:contents、自己不算可见时）。只有页脚（跳过与继续）、步骤内容还没渲染出来时不跳过
  // （条款内容可能晚于页脚渲染）
  const survey = [...scope.querySelectorAll('ms-intent-selection')].some(element => visible(element) && currentStep(element)) ||
    boxes.some(box => intentOption(box) && !box.required && !box.terms && currentStep(box.element));
  // 不涉及同意、有用途调查证据的一步里继续按钮不存在或禁用时，可以点作用域里当前一步的跳过按钮（见 skipButton；不到其余首次使用组件、
  // 对话框与整页上找，当前一步里没有时不点），不替账号选用途。作用域退到整页（整页上有无障碍跳转等无关按钮）、整页载入未完成时不点
  const skipTarget = consent || !survey || scope === document.body || document.readyState !== 'complete' ? null
    : scoped.find(item => skipButton(item) && currentStep(item)) || null;
  const skipField = skipTarget && !(button && buttonEnabled(button)) ? {skip: actionText(skipTarget)} : {};
  // 当前一步所在的 ms- 组件（cur，停在多步向导的哪一步以它为准）：当前一步的勾选框、挑出的继续按钮与要报告的跳过按钮（都没有时取当前一步
  // 第一个没被排除的按钮，再没有时取当前一步第一个按钮或链接）各自往上最近的 ms- 组件（作用域之内，没有时为作用域名），再接当前一步
  // 可见标题（h1、h2、h3、[role=heading]）里第一个在 ms- 组件里的标题所在的组件（直接在作用域里的标题跳过：介绍卡片、结尾页的继续按钮
  // 在共用的页脚 ms-horizontal-stepper 里时，靠标题分出停在哪一步），按此顺序去重
  const nearestPart = element => {
    for (let node = element.parentElement; node && node !== scope; node = node.parentElement) {
      if (node.localName.startsWith('ms-')) return node.localName;
    }
    return scopeName;
  };
  const anchors = boxes.filter(box => currentStep(box.element)).map(box => box.element)
    .concat([button, skipField.skip ? skipTarget : null].filter(Boolean));
  if (!anchors.length) {
    const present = narrow(scoped.concat(links), hasCurrent);
    const first = present.find(item => !buttonSkipped(item)) || present[0];
    if (first) anchors.push(first);
  }
  const headingPart = [...scope.querySelectorAll('h1, h2, h3, [role=heading]')]
    .filter(element => visible(element) && currentStep(element)).map(nearestPart).find(name => name !== scopeName);
  const cur = [...new Set(anchors.map(nearestPart).concat(headingPart ? [headingPart] : []))];
  const describeButton = item => {
    const skip = skipReason(item);
    return Object.assign({text: short(buttonText(item), 40), on: buttonEnabled(item)}, skip ? {skip} : {}, currentStep(item) ? {} : {off: true});
  };
  // 按钮先列当前一步的、再列其余各步的（off），截取前 8 个时当前一步的不被挤掉；本身就是勾选框的按钮（用途调查的 button[role=checkbox]
  // 选项，已在 boxes 里按 kind 为 role/button 列出）不再列，免得四个用途选项占满名额、把跳过与继续挤掉。作用域外可见的按钮（继续按钮
  // 不在作用域、其余首次使用组件或对话框里时的线索）与作用域里勾选框以外的表单控件
  const boxButtons = new Set(boxes.map(box => box.element));
  const listable = shown.filter(item => !boxButtons.has(item));
  const listed = listable.filter(currentStep).concat(listable.filter(item => !currentStep(item)));
  const outside = scope === document.body ? [] : visibleButtons(document.body).filter(item => !shown.includes(item));
  const controls = [...scope.querySelectorAll(controlSelector)].filter(element => formControl(element) && visible(element));
  // 作用域里可见、在当前一步里的 ms- 组件，按文档顺序去重：可能包括同时渲染的其他各步与常驻的卡片预览（线上用途调查一步时
  // ms-stacked-cards-preview、ms-outro 也算可见），停在哪一步以 cur 为准
  const parts = scope === document.body ? [] : [...new Set([...scope.querySelectorAll('*')]
    .filter(element => element.localName.startsWith('ms-') && visible(element) && currentStep(element)).map(element => element.localName))];
  const summary = {
    scope: scopeName,
    cur: cur.length ? cur.slice(0, 4) : undefined,
    parts: parts.length ? parts.slice(0, 6) : undefined,
    bodyBoxes: scope === document.body ? boxes.length : boxOwners(document.body).length,
    allBoxes: document.querySelectorAll(onboardingBoxSelector).length,
    boxes: boxes.slice(0, 6).map(box => Object.assign({req: box.required, on: box.checked}, box.disabled ? {dis: true} : {},
      box.basis ? {by: box.basis} : {}, box.terms ? {t: true} : {}, box.optional ? {o: true} : {}, box.hidden ? {hid: true} : {},
      intentOption(box) ? {intent: true} : {}, {kind: box.kind, label: clip(box.label, 40, 15)})),
    buttons: listed.slice(0, 8).map(describeButton),
    links: links.slice(0, 3).map(link => short(textOf(link) || link.getAttribute('aria-label'), 40)),
    controls: controls.length ? {n: controls.length, kinds: [...new Set(controls.map(controlKind))].slice(0, 4)} : undefined,
    outside: outside.length ? outside.slice(0, 3).map(describeButton) : undefined,
  };
  const reply = (state, extra) => JSON.stringify(Object.assign({state, summary}, extra));
  if (options.observe) return reply('observe', {});
  // 一步只勾一个未勾选、可点的必选项。必选项都勾上后按钮仍禁用、找不到按钮，或点过继续仍停在首次使用页时（relaxed），
  // 再勾标签含服务条款或年龄的项，邮件、订阅、调研、通知类不勾
  const pending = boxes.filter(box => box.required && !box.checked);
  const stalled = options.relaxed && (!button || !buttonEnabled(button) || options.clicked);
  const next = pending.find(box => !box.disabled) ||
    (stalled ? boxes.find(box => !box.required && !box.checked && !box.disabled && box.terms && !box.optional) : undefined);
  if (next) {
    next.element.click();
    return reply('checked', {label: clip(next.label, 40, 15), relaxed: !next.required});
  }
  if (pending.length) return reply('waiting', {reason: '必选项不可勾选'});
  // 首次使用页还没渲染出来（整页载入未完成，或作用域退到整页、既没有勾选框也没有按钮）：继续等，不算找不到，由总时限兜底
  if (!button && (document.readyState !== 'complete' || (scope === document.body && !boxes.length && !shown.length))) {
    return reply('waiting', {reason: '首次使用页尚未渲染'});
  }
  if (skipField.skip && options.skip) {
    skipTarget.click();
    return reply('skipped', {button: skipField.skip});
  }
  if (!button) return reply('stuck', Object.assign({reason: '找不到可点的继续按钮'}, skipField));
  const text = actionText(button);
  if (!buttonEnabled(button)) return reply('waiting', Object.assign({reason: '继续按钮未启用', button: text}, skipField));
  if (!options.click) return reply('waiting', {reason: '等待上一次点击生效', button: text});
  button.click();
  return reply('clicked', {button: text});
})()`

// OnboardingResult 为一次自动完成 AI Studio 首次使用页的结果（写进启动日志）
type OnboardingResult struct {
	// Checked 为勾选的次数（只勾必选项，以及按钮一直禁用时标签含服务条款或年龄的项）
	Checked int
	// Labels 为各次勾选的标签（已打码，保留开头与结尾几个字，见 onboardingLogLabel）；按放宽规则补勾的项（没有必选标记、
	// 标签含服务条款或年龄关键词，按钮一直禁用或点过继续仍停在首次使用页时才勾）前面带“[补]”
	Labels []string
	// Button 为最后一次点击的继续按钮文字（已打码、截短）；没有点击时为空
	Button string
	// Skipped 为点击跳过的次数（只在不涉及同意的步骤里点，如用途调查）
	Skipped int
	// Steps 为按顺序做过的每一步：“勾选:<标签>”（同 Labels）、“点击:<按钮>”、“跳过:<按钮>”，标签与按钮文字已打码截短（见 Progress）
	Steps []string
	// Elapsed 为从开始处理到离开首次使用页的时长
	Elapsed time.Duration
	// LocalStorage 为完成之后 AI Studio 页面 origin 的 localStorage（格式同 Worker.StorageLocalStorage）。只在 Worker 启动时导出，
	// 导出失败时为空：首次使用状态可能记在 localStorage 里，完成之后启动仍失败时浏览器随即关闭，页面写入的值只有这一次机会写回账户
	LocalStorage []byte
}

// acted 返回这次是否在页面上勾选或点击过：页面自己离开首次使用页时什么都没做，不报告
func (result OnboardingResult) acted() bool {
	return len(result.Steps) > 0 || result.Checked > 0 || result.Button != ""
}

// 进度记录（OnboardingResult.Steps）里各步的前缀
const (
	onboardingStepChecked = "勾选:"
	onboardingStepClicked = "点击:"
	onboardingStepSkipped = "跳过:"
)

// Progress 返回写进日志的进度记录：各步以“；”分隔，如“勾选:* 私は 18 歳以上です；点击:続行；跳过:跳过；点击:开始使用”；
// 什么都没做时为“-”。超过 onboardingProgressItems 步时保留开头 4 步与结尾 7 步，中间写“…（省略 N 步）”
func (result OnboardingResult) Progress() string {
	steps := result.Steps
	if len(steps) == 0 {
		return "-"
	}
	if len(steps) > onboardingProgressItems {
		head, tail := steps[:4:4], steps[len(steps)-7:]
		steps = append(append(head, fmt.Sprintf("…（省略 %d 步）", len(result.Steps)-len(head)-len(tail))), tail...)
	}
	return strings.Join(steps, "；")
}

// onboardingLogButton 为写进进度记录的按钮文字：打码、“|”换成“¦”、前 20 字，没有文字时为“-”
func onboardingLogButton(text string) string {
	text = snapshotText(strings.Join(strings.Fields(text), " "), 20)
	if text == "" {
		return "-"
	}
	return text
}

// onboardingStepReply 为一步表达式的返回值：Button 为点击（clicked、skipped）或等待的按钮文字；Skip 为不涉及同意的一步里
// 继续按钮不存在或禁用时可点的跳过按钮文字（没有时为空）
type onboardingStepReply struct {
	State   string             `json:"state"`
	Reason  string             `json:"reason"`
	Label   string             `json:"label"`
	Button  string             `json:"button"`
	Skip    string             `json:"skip"`
	Relaxed bool               `json:"relaxed"`
	Summary *onboardingSummary `json:"summary"`
}

// onboardingSummary 为首次使用页的结构摘要（写进失败的错误，见 OnboardingStructureMarker）：Scope 为查找范围（ms-onboarding、
// ms-tos-disclaimer 或 body），Cur 为当前一步所在的 ms- 组件（当前一步的勾选框、挑出的继续与跳过按钮、第一个标题各自最近的 ms- 组件，
// 前 4 个；停在多步向导的哪一步以它为准），Parts 为范围里可见、不在 aria-hidden 容器与视口外的 ms- 组件（前 6 个，可能包括同时渲染的
// 其他各步与常驻的卡片预览），BodyBoxes 为整页可见勾选框数
// （多于 Boxes 时有勾选框在范围之外），AllBoxes 为整页全部勾选框数（含不可见的，多于 BodyBoxes 时有勾选框没被当作可见），
// Buttons 为看到的按钮（本身是勾选框的 button[role=checkbox] 不列，已在 Boxes 里），
// Links 为范围内可见链接的文字，Controls 为范围内勾选框以外的可见表单控件，Outside 为范围之外的可见按钮（前 3 个）
type onboardingSummary struct {
	Scope     string              `json:"scope,omitempty"`
	Cur       []string            `json:"cur,omitempty"`
	Parts     []string            `json:"parts,omitempty"`
	BodyBoxes int                 `json:"bodyBoxes"`
	AllBoxes  int                 `json:"allBoxes"`
	Boxes     []onboardingBox     `json:"boxes"`
	Buttons   []onboardingButton  `json:"buttons"`
	Links     []string            `json:"links,omitempty"`
	Controls  *onboardingControls `json:"controls,omitempty"`
	Outside   []onboardingButton  `json:"outside,omitempty"`
}

// onboardingBox 为一个勾选框：By 为必选依据（required、aria、star、marker、ctx，见 describeBox），Disabled 为不可勾；Terms 为标签含
// 服务条款或年龄关键词，Optional 为含邮件、订阅、调研、通知类关键词（不按文字判定为必选），Hidden 为勾选框自己不可见（靠所在组件或
// label 算可见），Intent 为用途调查的选项（ms-intent-selection 里、标签取自按钮自己的 button[role=checkbox]，见 intentOption；
// 其余勾选框都按涉及同意处理），Kind 为 input 或 role 加所在组件的标签名；Label 为标签开头 40 字与结尾 15 字
type onboardingBox struct {
	Required bool   `json:"req"`
	Checked  bool   `json:"on"`
	Disabled bool   `json:"dis,omitempty"`
	By       string `json:"by,omitempty"`
	Terms    bool   `json:"t,omitempty"`
	Optional bool   `json:"o,omitempty"`
	Hidden   bool   `json:"hid,omitempty"`
	Intent   bool   `json:"intent,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Label    string `json:"label"`
}

// onboardingControls 为勾选框以外的表单控件：Count 为数量，Kinds 为类别（input:date、mat-select、role:radio 等，前 4 种）
type onboardingControls struct {
	Count int      `json:"n"`
	Kinds []string `json:"kinds,omitempty"`
}

// onboardingButton 为一个按钮：Skip 为不点的原因（link、area、cookie、popup、card、neg、opt、icon，见 skipReason）；Off 为不在
// 当前一步（在 aria-hidden、inert 容器里，或整个在视口左右之外，见 currentStep），列在当前一步的按钮之后
type onboardingButton struct {
	Text    string `json:"text"`
	Enabled bool   `json:"on"`
	Skip    string `json:"skip,omitempty"`
	Off     bool   `json:"off,omitempty"`
}

// onboardingFailure 为没能自动完成首次使用页：文案以 OnboardingFailedPrefix 开头，原因之后依次接进度记录（这次做过的每一步，
// 见 OnboardingProgressMarker；次数用完、没有开始完成时没有）、页面快照与结构摘要；cause 为底层错误（ctx 结束、页面脚本异常），可为 nil
type onboardingFailure struct {
	reason    string
	cause     error
	progress  string
	snapshot  string
	structure string
}

func (failure *onboardingFailure) Error() string {
	return OnboardingFailedPrefix + ": " + failure.reason + failure.progress + failure.snapshot + failure.structure
}

func (failure *onboardingFailure) Unwrap() error {
	return failure.cause
}

// onboardingInterrupted 为 bootstrap 订阅网络事件之后的页面类失败、失败时页面停在首次使用页：原错误文字不变，
// 末尾补一句是否已就地完成（没能完成时再接进度记录），再接页面快照（失败那一刻的首次使用页）与结构摘要；错误链保留原错误
type onboardingInterrupted struct {
	err  error
	text string
}

func (interrupted *onboardingInterrupted) Error() string {
	return interrupted.text
}

func (interrupted *onboardingInterrupted) Unwrap() error {
	return interrupted.err
}

// onboardingRelated 返回错误是否发生在首次使用页上（没能完成，或失败时页面停在首次使用页）：清缓存对它们没用
func onboardingRelated(err error) bool {
	var failure *onboardingFailure
	var interrupted *onboardingInterrupted
	return errors.As(err, &failure) || errors.As(err, &interrupted)
}

// errOnboardingPage 为 bootstrap 等到页面就绪后发现停在首次使用页（由 bootstrap 自己处理，不会返回给调用方）
var errOnboardingPage = errors.New("页面停在 AI Studio 首次使用页")

// onboardingPage 返回页面是否停在首次使用页。识别用短期限（pageSnapshotTimeout）：浏览器卡住时不把调用方拖到 ctx 到期；
// 读取失败（页面正在跳转、浏览器断开、超时、ctx 已结束）按不在处理
func (client *bidiClient) onboardingPage(ctx context.Context, contextID string) bool {
	if ctx.Err() != nil {
		return false
	}
	checkCtx, cancel := context.WithTimeout(ctx, pageSnapshotTimeout)
	defer cancel()
	value, err := client.evaluateBool(checkCtx, contextID, onboardingPageExpression)
	return err == nil && value
}

// startupDiagnosis 为启动时页面类失败处的诊断结果（见 startupFailureDiagnosis）：onboarding 为停在首次使用页（此时不读地址与快照）；
// pageURL 为读到的地址（withURL 时），suffix 为页面快照；blocked 为页面停在组织停用页（access.workspace.google.com）时的地址，否则为空
type startupDiagnosis struct {
	onboarding bool
	pageURL    string
	suffix     string
	blocked    string
}

// serviceNotAllowed 在页面停在组织停用页时返回 ServiceNotAllowedError（附同一份快照），否则为 nil：
// 失败的原因是账号所属组织停用了 AI Studio，原来的页面类错误（输入框未就绪、填写提示词失败等）只是表象
func (diagnosis startupDiagnosis) serviceNotAllowed() error {
	if diagnosis.blocked == "" {
		return nil
	}
	return &ServiceNotAllowedError{URL: serviceNotAllowedPath(diagnosis.blocked), suffix: diagnosis.suffix}
}

// startupFailureDiagnosis 为启动时页面类失败处的诊断：先识别首次使用页，不在时再读地址（withURL）与页面快照（同 pageDiagnosis），
// 并按读到的地址或快照里的 href 识别组织停用页（不多做 evaluate）。
// 识别、地址与快照共用一个从 context.Background 派生的短上下文：浏览器卡住时与原来只采快照一样最多多等 pageSnapshotTimeout，
// 识别没有回答时快照随即以“快照失败”结束，不会因为识别把失败拖到 INIT_TIMEOUT。ctx 已结束时不识别（来不及完成首次使用，
// 按不在处理），调用方已取消时什么都不读（同 pageDiagnosis）。在首次使用页时不读地址与快照（需要时由调用方另读）
func (client *bidiClient) startupFailureDiagnosis(ctx context.Context, contextID string, withURL bool) startupDiagnosis {
	if errors.Is(ctx.Err(), context.Canceled) {
		return startupDiagnosis{}
	}
	diagnosisCtx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	if ctx.Err() == nil {
		if value, err := client.evaluateBool(diagnosisCtx, contextID, onboardingPageExpression); err == nil && value {
			return startupDiagnosis{onboarding: true}
		}
	}
	pageURL, suffix, blocked := client.pageDiagnosisWithin(diagnosisCtx, contextID, withURL)
	return startupDiagnosis{pageURL: pageURL, suffix: suffix, blocked: blocked}
}

// onboardingLocalStorage 导出完成首次使用页之后 AI Studio 页面的 localStorage（见 OnboardingResult.LocalStorage），用短期限；
// 导出失败（页面正在跳转、不在 AI Studio、超时）时返回 nil
func (client *bidiClient) onboardingLocalStorage(ctx context.Context, contextID string) []byte {
	if ctx.Err() != nil {
		return nil
	}
	exportCtx, cancel := context.WithTimeout(ctx, pageSnapshotTimeout)
	defer cancel()
	encoded, err := client.exportAIStudioLocalStorage(exportCtx, contextID)
	if err != nil {
		return nil
	}
	return encoded
}

// onboardingBudget 限制一次启动（或一次验证、登录）里自动完成首次使用页的次数（见 onboardingCompletionLimit）
type onboardingBudget struct {
	completed int
}

// finish 自动完成首次使用页一次；次数用完时不再点击，以 onboardingFailure 失败（附只读的结构摘要与页面快照）
func (budget *onboardingBudget) finish(ctx context.Context, client *bidiClient, contextID string) (OnboardingResult, error) {
	if budget.completed >= onboardingCompletionLimit {
		reason := fmt.Sprintf("已自动完成 %d 次仍回到首次使用页", budget.completed)
		return OnboardingResult{}, client.onboardingFailed(ctx, contextID, reason, nil, "", client.observeOnboarding(ctx, contextID))
	}
	result, err := client.completeOnboarding(ctx, contextID)
	if err != nil {
		return result, err
	}
	budget.completed++
	return result, nil
}

// completeOnboarding 逐步完成首次使用页：每次 evaluate 做一小步（见 onboardingStepExpression），由这里轮询，
// 总时限 onboardingTiming.total、受 ctx 约束。只勾必选项（服务条款、年满 18 岁等带星号或标为必选的项），
// 必选项都勾上、按钮启用才点继续；不涉及同意的一步（用途调查等，见一步式表达式）里继续按钮不存在或禁用、有跳过按钮持续
// onboardingTiming.skip 时点跳过（不替账号选用途）。多步向导每一步换一次作用域里的内容，轮询连续走完各步；离开首次使用页即完成（勾选或点击过时再等
// onboardingTiming.saved，让官网发完保存请求）。按顺序做过的每一步记在 OnboardingResult.Steps。
// 失败时错误以 OnboardingFailedPrefix 开头，附进度记录、页面快照与结构摘要
func (client *bidiClient) completeOnboarding(ctx context.Context, contextID string) (OnboardingResult, error) {
	timing := onboardingTiming
	started := time.Now()
	stepCtx, cancel := context.WithTimeout(ctx, timing.total)
	defer cancel()
	var (
		result      OnboardingResult
		summary     *onboardingSummary
		reason      = "页面没有回应"
		lastChecked = started
		// lastClick 为最后一次点击继续，lastAction 为最后一次点击继续或跳过
		lastClick, lastAction time.Time
		stuckSince            time.Time
		// skipSince 为这一步开始可以点跳过的时刻（不涉及同意、继续按钮不存在或禁用、有跳过按钮），不能点时为零
		skipSince    time.Time
		buttonClicks int
	)
	failed := func(reason string, cause error) (OnboardingResult, error) {
		return result, client.onboardingFailed(ctx, contextID, reason, cause, OnboardingProgressMarker+result.Progress(), summary)
	}
	for {
		if stepCtx.Err() != nil {
			if err := ctx.Err(); err != nil {
				// 启动（或验证、登录）的剩余时间先于总时限用完：原因带上最后的状态，看得出停在什么状态
				return failed(err.Error()+"（"+reason+"）", err)
			}
			return failed("等待超时（"+reason+"）", context.DeadlineExceeded)
		}
		now := time.Now()
		// 点击继续的次数用完、距最后一次点击已超过 retry：这一轮读到的页面仍在首次使用页才失败（点击之后的等待在负载高时可能比 retry 长，
		// 页面已离开时不误判）
		clicksUsed := buttonClicks >= onboardingMaxButtonClicks && now.Sub(lastClick) >= timing.retry
		settled := lastAction.IsZero() || now.Sub(lastAction) >= timing.retry
		skippable := !skipSince.IsZero() && now.Sub(skipSince) >= timing.skip && settled
		if skippable && result.Skipped >= onboardingMaxSkipClicks {
			return failed(fmt.Sprintf("跳过 %d 次后仍停在首次使用页", result.Skipped), nil)
		}
		options := onboardingStepOptions{
			Click:   buttonClicks < onboardingMaxButtonClicks && settled,
			Relaxed: now.Sub(lastChecked) >= timing.relax,
			Clicked: buttonClicks > 0 && now.Sub(lastClick) >= timing.retry,
			Skip:    skippable,
		}
		raw, err := client.evaluateString(stepCtx, contextID, onboardingStepExpression(options))
		delay := timing.poll
		switch {
		case err != nil && (stepCtx.Err() != nil || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)):
			// 时限到期或 ctx 结束：命令自己的计时器（期限同 stepCtx）可能比 ctx 先到，等 stepCtx 结束后回到循环开头按原因失败
			<-stepCtx.Done()
			continue
		case err != nil && retryablePageEvaluation(err):
			// 点击继续后整页跳转，旧文档已销毁：等新文档
			reason = "页面正在跳转"
			skipSince = time.Time{}
		case err != nil:
			return failed("页面脚本失败: "+err.Error(), err)
		default:
			var reply onboardingStepReply
			if decodeErr := json.Unmarshal([]byte(raw), &reply); decodeErr != nil {
				return failed("页面脚本返回值无效", decodeErr)
			}
			if reply.Summary != nil {
				summary = reply.Summary
			}
			if clicksUsed && reply.State != "left" {
				return failed(fmt.Sprintf("点击继续 %d 次后仍停在首次使用页", buttonClicks), nil)
			}
			if reply.State != "stuck" {
				stuckSince = time.Time{}
			}
			// 跳过从“可以点”持续 skip 起才点：继续按钮刚渲染时短暂禁用、随即变为可点时不被跳过抢先
			if reply.Skip == "" || (reply.State != "waiting" && reply.State != "stuck") {
				skipSince = time.Time{}
			} else if skipSince.IsZero() {
				skipSince = time.Now()
			}
			switch reply.State {
			case "left":
				result.Elapsed = time.Since(started)
				if result.acted() {
					// 官网可能先切路由、再异步提交服务条款与年龄确认：稍等再返回，调用方随后整页重新导航或关闭浏览器时不中断这个请求
					_ = waitContext(ctx, timing.saved)
				}
				return result, nil
			case "checked":
				label := onboardingLogLabel(reply.Label, reply.Relaxed)
				result.Checked++
				result.Labels = append(result.Labels, label)
				result.Steps = append(result.Steps, onboardingStepChecked+label)
				lastChecked = time.Now()
				reason = "勾选后页面没有变化"
				if result.Checked > onboardingMaxBoxClicks {
					return failed(fmt.Sprintf("勾选 %d 次后仍有必选项未勾上", onboardingMaxBoxClicks), nil)
				}
			case "clicked":
				buttonClicks++
				result.Button = snapshotText(reply.Button, 40)
				result.Steps = append(result.Steps, onboardingStepClicked+onboardingLogButton(reply.Button))
				lastClick = time.Now()
				lastAction = lastClick
				reason = "点击继续后仍停在首次使用页"
				delay = timing.settle
			case "skipped":
				result.Skipped++
				result.Steps = append(result.Steps, onboardingStepSkipped+onboardingLogButton(reply.Button))
				lastAction = time.Now()
				reason = "跳过后仍停在首次使用页"
				delay = timing.settle
			case "waiting":
				reason = reply.Reason
			case "stuck":
				reason = reply.Reason
				if stuckSince.IsZero() {
					stuckSince = time.Now()
				} else if time.Since(stuckSince) >= timing.stuck {
					return failed(reply.Reason, nil)
				}
			default:
				return failed("页面脚本返回未知状态 "+reply.State, nil)
			}
		}
		_ = waitContext(stepCtx, delay)
	}
}

// observeOnboarding 只读取首次使用页的结构摘要（不点击），失败时返回 nil。与页面快照一样用独立的短上下文（ctx 可能已经过期），
// 调用方已取消时不读取
func (client *bidiClient) observeOnboarding(ctx context.Context, contextID string) *onboardingSummary {
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	observeCtx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	raw, err := client.evaluateString(observeCtx, contextID, onboardingStepExpression(onboardingStepOptions{Observe: true}))
	if err != nil {
		return nil
	}
	var reply onboardingStepReply
	if json.Unmarshal([]byte(raw), &reply) != nil {
		return nil
	}
	return reply.Summary
}

// onboardingFailed 组装没能完成首次使用页的错误：原因之后接进度记录 progress（“ | 已做=…”，没有开始完成时为空串）、
// 失败那一刻的页面快照与结构摘要。快照显示页面已被跳到组织停用页（access.workspace.google.com）时改为返回 ServiceNotAllowedError
// （附同一份快照）：没能完成的原因是账号所属组织停用了 AI Studio，不是首次使用页本身
func (client *bidiClient) onboardingFailed(ctx context.Context, contextID string, reason string, cause error, progress string, summary *onboardingSummary) error {
	_, snapshot, blocked := client.pageDiagnosisBlocked(ctx, contextID, false)
	if blocked != "" {
		return &ServiceNotAllowedError{URL: serviceNotAllowedPath(blocked), suffix: snapshot}
	}
	return &onboardingFailure{
		reason:    strings.Join(strings.Fields(reason), " "),
		cause:     cause,
		progress:  progress,
		snapshot:  snapshot,
		structure: formatOnboardingStructure(summary),
	}
}

// formatOnboardingStructure 把结构摘要整理成“ | 结构={…}”：文字打码、“|”换成“¦”，总长度不超过 onboardingStructureLimit；
// 超长时依次缩短标签与范围外的按钮、减少链接与范围外的按钮、范围里的 ms- 组件（parts，保留前 3 个）、按钮（按 keepButtons 的优先级
// 保留 4 个：继续与跳过不被用途选项、卡片挤掉）与勾选框条目，再缩短全部文字、去掉链接与范围外的按钮，仍超长时按字符截断。
// 标签保留开头与结尾（结尾的“*”与关键词不丢）。范围、当前一步所在的组件（cur）、勾选框数、表单控件计数与各条目的标记
// （必选、已勾、不可勾、必选依据、关键词标记、用途选项、类别、启用、不点的原因、不在当前一步）都很短，缩减时一直保留。没有摘要时返回空串
func formatOnboardingStructure(summary *onboardingSummary) string {
	if summary == nil {
		return ""
	}
	cleaned := onboardingSummary{
		Scope:     snapshotText(summary.Scope, 20),
		Cur:       shortenTexts(headItems(summary.Cur, 4), 30),
		Parts:     shortenTexts(headItems(summary.Parts, 6), 30),
		BodyBoxes: summary.BodyBoxes,
		AllBoxes:  summary.AllBoxes,
		Boxes:     make([]onboardingBox, 0, len(summary.Boxes)),
		Buttons:   make([]onboardingButton, 0, len(summary.Buttons)),
	}
	for _, box := range headItems(summary.Boxes, 6) {
		box.By = snapshotText(box.By, 10)
		box.Kind = snapshotText(box.Kind, 30)
		box.Label = clipLabel(box.Label, 40, 15)
		cleaned.Boxes = append(cleaned.Boxes, box)
	}
	cleanButtons := func(buttons []onboardingButton, limit int) []onboardingButton {
		var cleanedButtons []onboardingButton
		for _, button := range headItems(buttons, limit) {
			button.Skip = snapshotText(button.Skip, 10)
			button.Text = snapshotText(button.Text, 40)
			cleanedButtons = append(cleanedButtons, button)
		}
		return cleanedButtons
	}
	cleaned.Buttons = append(cleaned.Buttons, cleanButtons(summary.Buttons, 8)...)
	cleaned.Outside = cleanButtons(summary.Outside, 3)
	for _, link := range headItems(summary.Links, 3) {
		cleaned.Links = append(cleaned.Links, snapshotText(link, 40))
	}
	if summary.Controls != nil {
		controls := &onboardingControls{Count: summary.Controls.Count}
		for _, kind := range headItems(summary.Controls.Kinds, 4) {
			controls.Kinds = append(controls.Kinds, snapshotText(kind, 20))
		}
		cleaned.Controls = controls
	}
	shortenButtons := func(buttons []onboardingButton, limit int) {
		for index := range buttons {
			buttons[index].Text = truncateRunes(buttons[index].Text, limit)
		}
	}
	reductions := []func(*onboardingSummary){
		func(summary *onboardingSummary) {
			for index := range summary.Boxes {
				summary.Boxes[index].Label = clipLabel(summary.Boxes[index].Label, 20, 9)
			}
			shortenButtons(summary.Outside, 20)
		},
		func(summary *onboardingSummary) {
			summary.Links = headItems(summary.Links, 1)
			for index := range summary.Links {
				summary.Links[index] = truncateRunes(summary.Links[index], 20)
			}
			summary.Outside = headItems(summary.Outside, 1)
		},
		func(summary *onboardingSummary) { summary.Parts = headItems(summary.Parts, 3) },
		func(summary *onboardingSummary) { summary.Buttons = keepButtons(summary.Buttons, 4) },
		func(summary *onboardingSummary) { summary.Boxes = headItems(summary.Boxes, 4) },
		func(summary *onboardingSummary) {
			for index := range summary.Boxes {
				summary.Boxes[index].Label = clipLabel(summary.Boxes[index].Label, 9, 5)
			}
			shortenButtons(summary.Buttons, 15)
			summary.Links = nil
			summary.Outside = nil
		},
	}
	text := marshalCompact(cleaned)
	for _, reduce := range reductions {
		if utf8.RuneCountInString(text) <= onboardingStructureLimit {
			break
		}
		reduce(&cleaned)
		text = marshalCompact(cleaned)
	}
	return OnboardingStructureMarker + truncateRunes(text, onboardingStructureLimit)
}

// keepButtons 从按钮里保留 limit 个、按原顺序输出（不修改原切片）：当前一步的优先于其余各步的（off）；同在当前一步（或同在其余各步）时，
// 可以点的（没有不点的原因，继续按钮在这里）优先，其次 neg（跳过按钮在这里），再次其他不点的原因（area、card 等：旧版摘要里的用途选项、
// 卡片与轮播）；同一级按原顺序
func keepButtons(buttons []onboardingButton, limit int) []onboardingButton {
	if len(buttons) <= limit {
		return buttons
	}
	rank := func(button onboardingButton) int {
		value := 2
		switch button.Skip {
		case "":
			value = 0
		case "neg":
			value = 1
		}
		if button.Off {
			value += 3
		}
		return value
	}
	order := make([]int, len(buttons))
	for index := range order {
		order[index] = index
	}
	slices.SortStableFunc(order, func(a, b int) int { return rank(buttons[a]) - rank(buttons[b]) })
	keep := order[:limit]
	slices.Sort(keep)
	kept := make([]onboardingButton, 0, limit)
	for _, index := range keep {
		kept = append(kept, buttons[index])
	}
	return kept
}

// clipLabel 对标签打码、把“|”换成“¦”，超过 head+tail+1 个字符时保留开头 head 个与结尾 tail 个、中间以“…”省略
func clipLabel(text string, head, tail int) string {
	// 先整段打码（打码可能让文字变长，这里不截断），再按开头与结尾裁剪
	runes := []rune(snapshotText(text, math.MaxInt32))
	if len(runes) <= head+tail+1 {
		return string(runes)
	}
	return string(runes[:head]) + "…" + string(runes[len(runes)-tail:])
}

// onboardingLogLabel 为写进日志的一个勾选标签（见 OnboardingResult.Labels）：打码、保留开头 20 字与结尾 9 字，
// 按放宽规则补勾的前面带“[补]”，没有标签时为“-”
func onboardingLogLabel(label string, relaxed bool) string {
	label = clipLabel(strings.Join(strings.Fields(label), " "), 20, 9)
	if label == "" {
		label = "-"
	}
	if relaxed {
		label = "[补]" + label
	}
	return label
}

// marshalCompact 编码为单行 JSON；不转义 <、>、&，日志里保持可读（同 pageSnapshot.marshal）
func marshalCompact(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// headItems 返回前 limit 项（不修改原切片）
func headItems[T any](items []T, limit int) []T {
	if len(items) <= limit {
		return items
	}
	return items[:limit:limit]
}
