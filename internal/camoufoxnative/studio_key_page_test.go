package camoufoxnative

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeStudioKeySite 在首次使用页的最小 DOM 替身上搭一个合成的 AI Studio API Keys 页（照新版创建对话框的结构：mat-dialog-container、
// mat-select、cdk-overlay 里的 mat-option、mat-checkbox），由 variant 选场景：
//   - create-project：账户没有项目，下拉框里只有导入、“No Cloud Projects Available”与“+ Create project”；新建项目对话框的项目名已预填，
//     服务条款必勾（另有邮件订阅不该勾），确认后转圈一会儿才建好，建好后不自动选上（要重新展开下拉框选它）；
//   - existing：已有项目（列表响应里也有），下拉框里另有新建项目的选项（不该点）；banner 为真时页面上有需要验证账号的横幅与链接；
//   - empty-name：同 create-project，但新建项目对话框的项目名为空；
//   - verify-redirect：打开 API Keys 页总被跳到验证账号页；
//   - slow-list：已有项目，但页面加载时的响应里没有项目列表，下拉框展开后要过一会儿才列出它（先只有导入与“Create project”）；
//   - no-create：账户没有项目时 API Keys 页上没有“Create API key”，只有“Create project”（空状态）；Projects 页上有新建项目的按钮，
//     建好项目后 API Keys 页才出现“Create API key”。defaultKey 为真时新项目自带一个 Key（回到 API Keys 页 300ms 后才出现在 Key 列表与
//     它的响应里）；shownKey 为真时回到 API Keys 页后页面上显示着 Key 的末几位、响应里却没有；
//   - externalEntry 为真时（配合 create-project）下拉框里没有“Create project”，创建 Key 的对话框里只有指向 Google Cloud 控制台的
//     站外新建项目链接（不该点）；
//   - table：账户已有 Key（rows 里每行一条：key 为 saved 时是 fakeKey、other 时是 otherKey），Key 列表的响应里没有项目编号
//     （keyRecordID 为真时 Key 的记录里带项目 ID），项目列表的响应里是 listProjects；页面上是 mat-table（表头 Key、Project、
//     Created on、Quota tier 与操作列；行里有 Key 名称按钮、打码的 Key、项目列文字、站外的“Set up billing”、复制、删除、更多操作），
//     点 Key 名称打开“API key details”对话框（details 的项目名与编号，标签按 lang，结构按 detailsLayout：dl、form、inline、table、
//     spans），对话框里另有复制、删除按钮与关闭按钮。keyResource 为真时 Key 列表的每条记录前面是 Key 的资源名
//     projects/<行的 number>/locations/global/keys/k<序号>；projectsFirst 为真时项目列表先于 Key 列表响应；detailsReply（number、id、
//     name）为点开详情时页面再发出的一条只含一个项目的项目查询的响应。maskedList 为真时 Key 列表的响应里只有打码的 Key（…末四位）；
//     detailsMasked 为真时详情对话框里也只显示打码的 Key，detailsKeyReply 为真时点开详情时页面发出一条带这条完整 Key 的响应；
//     detailsWrongKey 为真时详情里显示的是另一条（末四位不同的）完整 Key；detailsTwin 为真时显示的是末四位相同的另一条完整 Key；
//     detailsPrefix 为真时详情里显示 Key 的前 24 位加“…”加末四位。
//     rowMask 为 none 时行里不显示打码的 Key（认不出列表）；prefix 时显示前 24 位加“…”加末四位（prefix-split 时两段分在两个元素里）；
//     full 时显示完整 Key；glued 时打码的 Key 后面紧挨着一个带复制图标的按钮（aria-label 为 Copy，不该点）。clientSample 为真时主区域
//     另有一段带完整公共 Key 的示例代码（列表项里）。detailsLayout 为 glued 时详情里每项是标签、值与带复制图标的按钮紧挨着的三个行内元素。
//
// glue 为真时页面的 innerText 照真实浏览器（见 fakeOnboardingDOMLib 的 gluedText）：相邻的行内元素之间没有空白，文字直接连在一起。
//
// 页面自己的公共 Key（clientKey，假的）照真实页面出现在：每次导航的 HTML 文档（WIZ_global_data）、同域脚本、发给 MakerSuite 接口的
// 每个请求的 X-Goog-Api-Key 请求头、一条带 ?key= 与 base64 编码请求头的 generativelanguage 请求，以及一条配置接口的 JSON 响应
// （整段字符串就是它）。noClientHeaders 为真时请求的地址与请求头里都不带它（只在文档、脚本与配置接口的正文里）。emptyReplyKey 为真时
// 页面是空状态、Key 列表接口的响应里却有一条完整的 Key（otherKey）。创建 Key 后：createdRow 为真时列表里新出现一行打码的新 Key；createdMasked 为真时创建结果对话框里只显示打码的 Key；
// createdNoReply 为真时创建接口的响应里没有完整 Key；createExtra 为真时创建接口的响应里另有公共 Key 与一条不相干的 Key；createdShow 为
// prefix 时创建结果对话框里显示新 Key 的前 24 位加“…”加末四位，为 client 时显示打码的新 Key 与一段带完整公共 Key 的示例代码，为 classic
// 时显示打码的新 Key 与另一条不在拒绝集合里的完整 Key，为 glued 时完整的新 Key 后面紧挨着带复制图标的按钮（配合 glue）。
//
// 页面自己的“请求”只是排进 responses，由 Go 侧假浏览器在下一条命令时以 network.responseCompleted 事件发出（带请求头、MIME、
// 导航编号）、network.getData 回答正文。真实指针点击模拟为：点击表达式里的 scrollIntoView 记下目标，input.performActions 时点它（__click）
const fakeStudioKeySite = `
const texts = {
  en: {create: 'Create API key', keyTitle: 'Create API key', name: 'Name your key', choose: 'Choose an imported project', cancel: 'Cancel',
    confirm: 'Create key', imp: 'Import project', none: 'No Cloud Projects Available', newProject: 'Create project',
    projectTitle: 'Create a new project', projectName: 'Project name', tos: 'I agree to the Google Cloud Terms of Service *',
    mail: 'Email me with product updates and offers', projectConfirm: 'Create project', created: 'API key created',
    detailsTitle: 'API key details', projectNameLabel: 'Project name', projectNumberLabel: 'Project number', close: 'Close'},
  zh: {create: '创建 API 密钥', keyTitle: '创建 API 密钥', name: '为密钥命名', choose: '选择已导入的项目', cancel: '取消', confirm: '创建密钥',
    imp: '导入项目', none: '没有可用的 Cloud 项目', newProject: '创建项目', projectTitle: '创建新项目', projectName: '项目名称',
    tos: '我同意 Google Cloud 服务条款 *', mail: '通过电子邮件接收产品动态和优惠', projectConfirm: '创建', created: '已创建 API 密钥',
    detailsTitle: 'API 密钥详情', projectNameLabel: '项目名称', projectNumberLabel: '项目编号', close: '关闭'},
  'zh-TW': {create: '建立 API 金鑰', keyTitle: '建立 API 金鑰', name: '為金鑰命名', choose: '選擇已匯入的專案', cancel: '取消', confirm: '建立金鑰',
    imp: '匯入專案', none: '沒有可用的 Cloud 專案', newProject: '建立專案', projectTitle: '建立新專案', projectName: '專案名稱',
    tos: '我同意 Google Cloud 服務條款 *', mail: '透過電子郵件接收產品更新', projectConfirm: '建立', created: '已建立 API 金鑰',
    detailsTitle: 'API 金鑰詳細資料', projectNameLabel: '專案名稱', projectNumberLabel: '專案編號', close: '關閉'},
  ja: {create: 'API キーを作成', keyTitle: 'API キーを作成', name: 'キーの名前', choose: 'インポートしたプロジェクトを選択', cancel: 'キャンセル',
    confirm: 'キーを作成', imp: 'プロジェクトをインポート', none: 'プロジェクトがありません', newProject: 'プロジェクトを作成',
    projectTitle: '新しいプロジェクトを作成', projectName: 'プロジェクト名', tos: 'Google Cloud 利用規約に同意します *', mail: 'メールで最新情報を受け取る',
    projectConfirm: '作成', created: 'API キーを作成しました',
    detailsTitle: 'API キーの詳細', projectNameLabel: 'プロジェクト名', projectNumberLabel: 'プロジェクト番号', close: '閉じる'},
  ko: {create: 'API 키 만들기', keyTitle: 'API 키 만들기', name: '키 이름', choose: '가져온 프로젝트 선택', cancel: '취소', confirm: '키 만들기',
    imp: '프로젝트 가져오기', none: '프로젝트가 없습니다', newProject: '프로젝트 만들기', projectTitle: '새 프로젝트 만들기', projectName: '프로젝트 이름',
    tos: 'Google Cloud 서비스 약관에 동의합니다 *', mail: '이메일로 소식 받기', projectConfirm: '만들기', created: 'API 키가 생성됨',
    detailsTitle: 'API 키 세부정보', projectNameLabel: '프로젝트 이름', projectNumberLabel: '프로젝트 번호', close: '닫기'},
};
const T = texts[variant.lang || 'en'];
gluedText = Boolean(variant.glue);
const log = [];
let responses = [];
let requestSeq = 0;
const state = {navigations: [], projectCreates: 0, keyCreates: 0, project: null, selected: null, pendingProject: 0, tos: null, mail: null,
  projectPane: null, listReadyAt: 0, listLoaded: variant.scenario !== 'slow-list', keysAt: 0, detailsOpens: 0,
  rows: variant.scenario === 'table' ? (variant.rows || []).slice() : []};
if (variant.scenario === 'existing' || variant.scenario === 'slow-list') state.project = {number: '210987654321', name: 'Default Gemini Project', id: 'gen-lang-client-0987654321'};
Element.prototype.scrollIntoView = function () { globalThis.__target = this; };
const nameOf = element => element.getAttribute('data-name') || element.localName;
const respond = (url, body, options = {}) => responses.push({id: 'req-' + (++requestSeq), url, body, mime: options.mime || '',
  headers: options.headers || [], navigation: options.navigation || '', destination: options.destination || ''});
const apiHeaders = [...(variant.noClientHeaders ? [] : [['X-Goog-Api-Key', clientKey]]), ['X-User-Agent', 'grpc-web-javascript/0.1'],
  ['Cookie', 'SID=fake']];
globalThis.__click = () => {
  const target = globalThis.__target;
  globalThis.__target = null;
  if (!target) return '';
  log.push(nameOf(target));
  target.click();
  return nameOf(target);
};
globalThis.__drain = () => {
  const out = responses;
  responses = [];
  return JSON.stringify(out);
};
const rpcURL = name => 'https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/' + name;
// rpcRespond 为页面发给 MakerSuite 接口的请求的响应：请求头带着页面自己的公共 Key
const rpcRespond = (name, body) => respond(rpcURL(name), body, {mime: 'application/json+protobuf', headers: apiHeaders});
// pageLoad 为每次导航时页面自己的文档、脚本与几条带公共 Key 的请求
const pageLoad = path => {
  respond('https://aistudio.google.com' + path, '<!doctype html><html><head><script nonce="n">window.WIZ_global_data = {"WIu0Nc":"' + clientKey +
    '","PeqOqb":"' + clientKey + '","cfb2h":"boq_alkali-makersuite"};</script></head><body><ms-app></ms-app></body></html>',
    {mime: 'text/html; charset=utf-8', navigation: 'nav-' + requestSeq, destination: 'document'});
  respond('https://aistudio.google.com/_/mss/boq-makersuite/_/js/k=boq-makersuite.MakerSuite.en.app.js',
    'var k="' + clientKey + '";function g(){return "AQ.getInstanceForTestingOnlyFooBarBaz"}', {mime: 'text/javascript', destination: 'script'});
  if (!variant.noClientHeaders) {
    respond('https://generativelanguage.googleapis.com/v1beta/models?key=' + clientKey, '{"models":[]}',
      {mime: 'application/json', headers: [['X-Goog-Api-Key', clientKey, 'base64']]});
  }
  rpcRespond('GetUserConfig', ")]}'\n" + JSON.stringify([[clientKey, 'boq_alkali-makersuite', null, 1]]));
};
let overlay = null;
let keyDialog = null;
let keySelect = null;
let keyConfirm = null;
let panel = null;
const selectValue = () => state.selected ? h('span', {class: 'mat-mdc-select-value-text'}, state.selected.name)
  : h('span', {class: 'mat-mdc-select-placeholder'}, T.choose);
const setSelected = project => {
  state.selected = project;
  replace(keySelect, selectValue());
  keySelect.setAttribute('class', 'mat-mdc-select' + (project ? '' : ' mat-mdc-select-empty'));
  keyConfirm.disabled = !project;
};
const closePanel = () => {
  if (!panel) return;
  panel.setAttribute('hidden', '');
  panel = null;
  keySelect.setAttribute('aria-expanded', 'false');
};
const option = (text, onClick, name, iconName) => {
  const element = h('mat-option', {role: 'option', class: 'mat-mdc-option', 'data-name': name}, ...(iconName ? [icon(iconName)] : []), text);
  element.addEventListener('click', onClick);
  return element;
};
const togglePanel = () => {
  if (panel) {
    closePanel();
    return;
  }
  keySelect.setAttribute('aria-expanded', 'true');
  const options = [];
  if (state.project && state.listLoaded) options.push(projectOption());
  // 列表展开 100ms 后才加载完（短于测试里的 listSettle，长于一轮轮询）
  if (state.project && !state.listLoaded && !state.listReadyAt) state.listReadyAt = Date.now() + 100;
  options.push(option(T.imp, () => { closePanel(); }, 'opt-import', 'download'));
  if (!state.project) {
    options.push(h('mat-option', {role: 'option', 'aria-disabled': 'true', class: 'mat-mdc-option mdc-list-item--disabled', 'data-name': 'opt-none'},
      T.none));
  }
  if (!variant.externalEntry) options.push(option(T.newProject, () => { closePanel(); openProjectDialog(); }, 'opt-create', 'add'));
  panel = h('div', {class: 'cdk-overlay-pane'}, h('div', {class: 'mat-mdc-select-panel', role: 'listbox'}, ...options));
  append(overlay, panel);
};
const projectOption = () => option(state.project.name + ' ' + state.project.id, () => { setSelected(state.project); closePanel(); }, 'opt-project');
const openProjectDialog = () => {
  const nameInput = h('input', {type: 'text', required: '', value: variant.scenario === 'empty-name' ? '' : 'Gemini Project', 'data-name': 'project-name'});
  const tos = h('input', {type: 'checkbox', id: 'tos', required: '', 'data-name': 'tos'});
  const mail = h('input', {type: 'checkbox', id: 'mail', 'data-name': 'mail'});
  state.tos = tos;
  state.mail = mail;
  const confirm = h('button', {disabled: '', 'data-name': 'project-confirm'}, T.projectConfirm);
  tos.addEventListener('change', () => { confirm.disabled = !tos.checked; });
  const dialog = h('mat-dialog-container', {role: 'dialog', 'data-name': 'project-dialog'},
    h('h2', {}, T.projectTitle),
    h('mat-form-field', {}, h('mat-label', {}, T.projectName), nameInput),
    h('mat-checkbox', {class: 'mat-mdc-checkbox'}, tos, h('label', {for: 'tos'}, T.tos)),
    h('mat-checkbox', {class: 'mat-mdc-checkbox'}, mail, h('label', {for: 'mail'}, T.mail)),
    h('button', {'data-name': 'project-cancel'}, T.cancel), confirm);
  confirm.addEventListener('click', () => {
    state.projectCreates++;
    confirm.disabled = true;
    append(dialog, h('mat-progress-bar', {role: 'progressbar'}));
    state.pendingProject = 12;
  });
  state.projectPane = h('div', {class: 'cdk-overlay-pane'}, dialog);
  append(overlay, state.projectPane);
};
const masked = value => '…' + value.slice(-4);
// prefixMasked 为 Key 的前 24 位加“…”加末四位（AQ. 新格式 Key 的前缀也像完整 Key）
const prefixMasked = value => value.slice(0, 24) + '…' + value.slice(-4);
const createKey = () => {
  state.keyCreates++;
  const project = state.selected;
  const extra = variant.createExtra ? [[null, null, clientKey], ['projects/999999999999/locations/global/keys/kx', null, classicKey]] : [];
  rpcRespond('CreateApiKey', ")]}'\n" + JSON.stringify([[['projects/' + project.number + '/locations/global/keys/k1', null,
    variant.createdNoReply ? masked(fakeKey) : fakeKey, ['projects/' + project.number, project.name]], ...extra]]));
  const done = h('button', {'data-name': 'done'}, 'Done');
  const pane = keyDialog.parentElement;
  done.addEventListener('click', () => pane.setAttribute('hidden', ''));
  // createdShow：prefix 时显示 Key 的长前缀加“…”加末四位；client 时显示打码的新 Key，另有一段带完整公共 Key 的示例代码；
  // classic 时显示打码的新 Key 与另一条不在拒绝集合里的完整 Key（末四位与新行对不上）
  const createdText = variant.createdShow === 'prefix' ? prefixMasked(fakeKey) : variant.createdShow === 'glued' ? fakeKey
    : variant.createdMasked || variant.createdShow ? masked(fakeKey) : fakeKey;
  const sample = variant.createdShow === 'client' ? [h('pre', {}, 'curl "https://generativelanguage.googleapis.com/v1beta/models?key=' + clientKey + '"')]
    : variant.createdShow === 'classic' ? [h('code', {}, classicKey)] : [];
  // glued：Key 与后面带复制图标的按钮紧挨着（innerText 里连成“Key 加 content_copy”）
  const shownText = variant.createdShow === 'glued' ? h('p', {}, h('span', {class: 'key-value'}, createdText),
    h('button', {'aria-label': 'Copy', 'data-name': 'copy-icon'}, icon('content_copy'))) : h('p', {}, createdText);
  replace(keyDialog, h('h2', {}, T.created), shownText, ...sample, h('button', {'data-name': 'copy'}, 'Copy'), done);
  if (variant.createdRow) {
    // 页面在列表里加上新 Key 的一行（打码）
    state.rows.push({name: 'Gemini API Key', key: 'saved', project: project.name});
    renderList();
  }
};
const openKeyDialog = () => {
  keySelect = h('mat-select', {role: 'combobox', 'aria-expanded': 'false', class: 'mat-mdc-select mat-mdc-select-empty', 'data-name': 'select'}, selectValue());
  keySelect.addEventListener('click', togglePanel);
  keyConfirm = h('button', {disabled: '', 'data-name': 'key-confirm'}, T.confirm);
  keyConfirm.addEventListener('click', createKey);
  keyDialog = h('mat-dialog-container', {role: 'dialog', 'data-name': 'key-dialog'},
    h('h2', {}, T.keyTitle),
    h('mat-form-field', {}, h('mat-label', {}, T.name), h('input', {type: 'text', value: 'Gemini API Key'})),
    h('mat-form-field', {}, h('mat-label', {}, T.choose), keySelect),
    ...(variant.externalEntry ? [h('a', {href: 'https://console.cloud.google.com/projectcreate', 'data-name': 'cloud-link'},
      icon('add'), 'Create a project in Google Cloud console')] : []),
    h('button', {'data-name': 'key-cancel'}, T.cancel), keyConfirm);
  append(overlay, h('div', {class: 'cdk-overlay-pane'}, keyDialog));
};
const show = (body, path) => {
  // 新页面：上一页展开着的下拉列表随页面一起没了
  panel = null;
  mount(body, path);
  document.querySelector = selector => document.querySelectorAll(selector)[0] || null;
};
// keysTable 为已有 Key 的列表（照 mat-table 的结构）；点 Key 名称打开这条 Key 的详情对话框
const keyValue = item => item.key === 'other' ? otherKey : item.key === 'client' ? clientKey : fakeKey;
// shownKey 为行里显示的 Key（rowMask）：默认打码（…末四位）；none 不显示；prefix 为前 24 位加“…”加末四位；prefix-split 同 prefix、
// 前缀与“…末四位”分在两个元素里；full 为完整 Key；glued 为打码的 Key 后面紧挨着带复制图标的按钮
const shownKey = (value, index) => {
  switch (variant.rowMask) {
    case 'none': return [];
    case 'glued': return [h('span', {class: 'masked-key'}, masked(value)),
      h('button', {'aria-label': 'Copy', 'data-name': 'cell-copy-icon-' + index}, icon('content_copy'))];
    case 'prefix': return [h('span', {class: 'masked-key'}, prefixMasked(value))];
    case 'prefix-split': return [h('span', {class: 'masked-key'}, h('span', {}, value.slice(0, 24)), h('span', {}, '…' + value.slice(-4)))];
    case 'full': return [h('span', {class: 'masked-key'}, value)];
  }
  return [h('span', {class: 'masked-key'}, masked(value))];
};
const keysTable = () => {
  const row = (item, index) => {
    const value = keyValue(item);
    const name = h('button', {class: 'mdc-button name-link', 'data-name': 'key-name-' + index}, item.name);
    name.addEventListener('click', () => openDetails(item, value));
    // keyCellActions：Key 这一格里在 Key 名称前面另有带文字的复制、删除按钮，aria-label 为 Copy、文字是打码 Key 的按钮，
    // 提示（mattooltip）为 Delete、文字是打码 Key 的按钮，以及带复制图标、文字是打码 Key 的按钮（都不该点）
    const cellActions = variant.keyCellActions ? [h('button', {'data-name': 'cell-copy-' + index}, 'Copy'),
      h('button', {'data-name': 'cell-delete-' + index}, 'Delete'),
      h('button', {'aria-label': 'Copy', 'data-name': 'cell-copy-key-' + index}, '…' + value.slice(-4)),
      h('button', {mattooltip: 'Delete', 'data-name': 'cell-tooltip-key-' + index}, '…' + value.slice(-4)),
      h('button', {'data-name': 'cell-icon-key-' + index}, icon('content_copy'), '…' + value.slice(-4))] : [];
    return h('tr', {class: 'mat-mdc-row'},
      h('td', {class: 'mat-mdc-cell'}, ...cellActions, name, ...shownKey(value, index)),
      h('td', {class: 'mat-mdc-cell'}, item.project || ''),
      h('td', {class: 'mat-mdc-cell'}, 'Nov 1, 2026'),
      h('td', {class: 'mat-mdc-cell'}, h('a', {href: 'https://console.cloud.google.com/billing', 'data-name': 'billing-' + index}, 'Set up billing')),
      h('td', {class: 'mat-mdc-cell'},
        h('button', {'aria-label': 'Copy API key', 'data-name': 'copy-' + index}, icon('content_copy')),
        h('button', {'data-name': 'delete-' + index}, icon('delete'), 'Delete'),
        h('button', {'aria-label': 'More actions', 'data-name': 'more-' + index}, icon('more_vert'))));
  };
  const headers = variant.noHeader ? [] : [h('thead', {}, h('tr', {class: 'mat-mdc-header-row'},
    ...['Key', 'Project', 'Created on', 'Quota tier', ''].map(text => h('th', {class: 'mat-mdc-header-cell'}, text))))];
  return h('table', {class: 'mat-mdc-table'}, ...headers, h('tbody', {}, ...state.rows.map(row)));
};
// detailsFields 按 detailsLayout 搭详情对话框里的项目名与项目编号（另有 API key 一项，显示完整 Key：表达式应打码）
const detailsFields = (item, value) => {
  const details = variant.details || {};
  const twin = value.slice(0, 10) + 'q'.repeat(value.length - 14) + value.slice(-4);
  const shownValue = variant.detailsWrongKey ? classicKey : variant.detailsMasked ? masked(value) : variant.detailsPrefix ? prefixMasked(value)
    : variant.detailsTwin ? twin : value;
  const pairs = [['API key', shownValue], ['Name', item.name], [T.projectNameLabel, details.name || ''], [T.projectNumberLabel, details.number || '']];
  switch (variant.detailsLayout) {
    case 'form':
      return pairs.map(([label, text]) => h('mat-form-field', {class: 'mat-mdc-form-field'}, h('mat-label', {}, label),
        h('input', {type: 'text', readonly: '', value: text})));
    case 'inline':
      return pairs.map(([label, text]) => h('p', {}, label + '：' + text));
    case 'table':
      return [h('table', {}, ...pairs.map(([label, text]) => h('tr', {}, h('th', {}, label), h('td', {}, text, icon('content_copy')))))];
    case 'spans':
      return pairs.map(([label, text]) => h('div', {class: 'field'}, h('span', {class: 'label'}, label), h('span', {class: 'value'}, text)));
    case 'glued':
      return pairs.map(([label, text]) => h('div', {class: 'field'}, h('span', {class: 'label'}, label), h('span', {class: 'value'}, text),
        h('button', {'aria-label': 'Copy'}, icon('content_copy'))));
  }
  return [h('dl', {}, ...pairs.flatMap(([label, text]) => [h('dt', {}, label), h('dd', {}, text)]))];
};
const openDetails = (item, value) => {
  state.detailsOpens++;
  const reply = variant.detailsReply;
  if (reply) rpcRespond('GetCloudProject', ")]}'\n" + JSON.stringify([['projects/' + reply.number, reply.id, ...(reply.name ? [reply.name] : [])]]));
  if (variant.detailsKeyReply) rpcRespond('GetCloudApiKey', ")]}'\n" + JSON.stringify([['k' + state.rows.indexOf(item), item.name, value]]));
  const close = h('button', {'data-name': 'details-close'}, T.close);
  const pane = h('div', {class: 'cdk-overlay-pane'}, h('mat-dialog-container', {role: 'dialog', 'data-name': 'details-dialog'},
    h('h2', {}, T.detailsTitle), ...detailsFields(item, value),
    h('button', {'data-name': 'details-copy'}, icon('content_copy'), 'Copy key'),
    h('button', {'data-name': 'details-delete'}, icon('delete'), 'Delete key'), close));
  close.addEventListener('click', () => pane.setAttribute('hidden', ''));
  append(overlay, pane);
};
const respondProjects = () => rpcRespond('ListCloudProjects', ")]}'\n" +
  JSON.stringify([(variant.listProjects || []).map(project => ['projects/' + project.number, project.id, project.name])]));
let listHost = null;
// renderList 画 Key 列表：有行时是表格，没有时是空状态的文字（shownKey 时回到 API Keys 页后显示着打码的 Key）
const renderList = () => {
  if (!listHost) return;
  const created = state.project && state.projectCreates > 0;
  // clientSample：主区域另有一段带完整公共 Key 的示例代码（列表项里）
  const sample = variant.clientSample ? [h('ul', {}, h('li', {}, 'curl https://generativelanguage.googleapis.com/v1beta/models?key=' + clientKey))] : [];
  replace(listHost, ...(state.rows.length ? [keysTable()] : [h('p', {}, created && variant.shownKey ? 'Gemini API Key …WXYZ' : 'No API keys yet')]), ...sample);
};
const apikeyPage = () => {
  let create = h('button', {'data-name': 'create-key'}, icon('key'), T.create);
  create.addEventListener('click', openKeyDialog);
  if (variant.scenario === 'no-create' && !state.project) {
    // 没有项目的空状态：只有新建项目的按钮（本流程不点它，改去 Projects 页）
    create = h('button', {'data-name': 'apikey-project'}, icon('add'), T.newProject);
    create.addEventListener('click', openProjectDialog);
  }
  const created = state.project && state.projectCreates > 0;
  listHost = h('div', {class: 'key-list'});
  renderList();
  const children = [h('h1', {}, 'API Keys'), listHost, create];
  if (variant.banner) {
    children.unshift(h('div', {role: 'alert', 'data-name': 'banner'}, 'Your account must be verified before it can continue using the Gemini API.',
      h('a', {href: '/verify-account', 'data-name': 'verify-link'}, 'Verify')));
  }
  overlay = h('div', {class: 'cdk-overlay-container'});
  show(h('body', {}, h('ms-app', {}, h('main', {}, ...children)), overlay), '/apikey');
  if (created && variant.defaultKey) state.keysAt = Date.now() + 300;
  else if (variant.scenario === 'table') {
    if (variant.projectsFirst) respondProjects();
    rpcRespond('ListApiKeys', ")]}'\n" + JSON.stringify([state.rows.map((item, index) => [
      ...(variant.keyResource ? ['projects/' + item.number + '/locations/global/keys/k' + index] : []), item.name,
      variant.maskedList ? masked(keyValue(item)) : keyValue(item), ...(variant.keyRecordID ? [variant.keyRecordID] : []), [1762000000, 0]])]));
    if (!variant.projectsFirst) respondProjects();
  } else rpcRespond('ListApiKeys', ")]}'\n" + (variant.emptyReplyKey ? JSON.stringify([[['Gemini API Key', otherKey]]]) : '[]'));
  if (state.project && variant.scenario !== 'slow-list') {
    rpcRespond('ListCloudProjects', ")]}'\n" + JSON.stringify([[['projects/' + state.project.number, state.project.id, state.project.name]]]));
  }
};
globalThis.__navigate = url => {
  const path = new URL(url).pathname;
  state.navigations.push(path);
  pageLoad(path);
  if (variant.scenario === 'verify-redirect') {
    show(h('body', {}, h('main', {}, h('h1', {}, 'Verify your account'), h('button', {'data-name': 'verify'}, 'Verify'))), '/verify-account');
    return path;
  }
  if (path === '/apikey') apikeyPage();
  if (path === '/projects') projectsPage();
  return path;
};
const projectsPage = () => {
  const create = h('button', {'data-name': 'projects-create'}, icon('add'), T.newProject);
  create.addEventListener('click', openProjectDialog);
  overlay = h('div', {class: 'cdk-overlay-container'});
  show(h('body', {}, h('ms-app', {}, h('main', {}, h('h1', {}, 'Projects'), create)), overlay), '/projects');
};
show(h('body', {}), '/');
const beforeEvaluate = () => {
  if (state.keysAt && Date.now() >= state.keysAt) {
    // 新项目自带的 Key 晚一点才出现在 Key 列表与它的响应里
    state.keysAt = 0;
    rpcRespond('ListApiKeys', ")]}'\n" + JSON.stringify([[['projects/' + state.project.number + '/locations/global/keys/k0', null, fakeKey,
      ['projects/' + state.project.number, state.project.name]]]]));
    state.rows.push({name: 'Default Gemini API Key', key: 'saved', project: state.project.name});
    renderList();
  }
  if (state.listReadyAt && Date.now() >= state.listReadyAt) {
    state.listReadyAt = 0;
    // 项目列表加载完：展开着的列表里补上已有项目
    state.listLoaded = true;
    if (panel) prepend(panel.children[0], projectOption());
  }
  if (state.pendingProject > 0 && --state.pendingProject === 0) {
    state.projectPane.setAttribute('hidden', '');
    state.project = {number: '123456789012', name: 'Gemini Project', id: 'gen-lang-client-0123456789'};
    respond('https://cloudresourcemanager.googleapis.com/v3/operations/cp.1', JSON.stringify({name: 'operations/cp.1', done: true,
      response: {name: 'projects/123456789012', projectId: 'gen-lang-client-0123456789', displayName: 'Gemini Project', state: 'ACTIVE'}}));
  }
};
const report = () => ({log, navigations: state.navigations, projectCreates: state.projectCreates, keyCreates: state.keyCreates,
  tos: Boolean(state.tos && state.tos.checked), mail: Boolean(state.mail && state.mail.checked), detailsOpens: state.detailsOpens,
  dialogs: document.querySelectorAll('mat-dialog-container').filter(element => element.checkVisibility()).length});
`

// studioKeyNodeReport 为合成页面的记录
type studioKeyNodeReport struct {
	Log            []string
	Navigations    []string
	ProjectCreates int
	KeyCreates     int
	Tos            bool
	Mail           bool
	DetailsOpens   int
	// Dialogs 为结束时仍可见的对话框个数
	Dialogs int
}

// fastStudioKey 把创建流程的时限调短，测试结束时恢复
func fastStudioKey(t *testing.T) {
	t.Helper()
	saved := studioKeyTiming
	studioKeyTiming.poll = 20 * time.Millisecond
	studioKeyTiming.settle = 200 * time.Millisecond
	studioKeyTiming.createWait = 5 * time.Second
	studioKeyTiming.projectWait = 5 * time.Second
	studioKeyTiming.stuck = 3 * time.Second
	studioKeyTiming.noDialog = time.Second
	studioKeyTiming.retry = 100 * time.Millisecond
	studioKeyTiming.listSettle = 150 * time.Millisecond
	studioKeyTiming.projectName = 300 * time.Millisecond
	t.Cleanup(func() { studioKeyTiming = saved })
}

// runStudioKeyInNode 在合成页面上跑完整的获取流程（Go 侧轮询、读响应、真实指针点击、导航都经假浏览器转到 node）；
// tweaks 在调短时限之后再改个别时限
func runStudioKeyInNode(t *testing.T, variant map[string]any, tweaks ...func()) (StudioKeyResult, error, studioKeyNodeReport) {
	t.Helper()
	return runStudioKeyInNodeWith(t, variant, StudioKeyOptions{}, tweaks...)
}

// runStudioKeyInNodeWith 同 runStudioKeyInNode，按 options（只读、已保存的 Key）获取
func runStudioKeyInNodeWith(t *testing.T, variant map[string]any, options StudioKeyOptions, tweaks ...func()) (StudioKeyResult, error, studioKeyNodeReport) {
	t.Helper()
	fastStudioKey(t)
	for _, tweak := range tweaks {
		tweak()
	}
	encodedVariant, _ := json.Marshal(variant)
	process := startOnboardingNode(t, "const variant = "+string(encodedVariant)+";\nconst fakeKey = "+jsString(fakeNewKey)+";\nconst otherKey = "+
		jsString(fakeOtherKey)+";\nconst clientKey = "+jsString(fakeClientKey)+";\nconst classicKey = "+jsString(fakeClassicKey)+";\n"+fakeStudioKeySite)
	var mu sync.Mutex
	bodies := map[string]string{}
	drain := func() []map[string]any {
		answer := process.evaluate("__drain()")
		text, _ := answer.value.(string)
		var items []struct {
			ID          string     `json:"id"`
			URL         string     `json:"url"`
			Body        string     `json:"body"`
			Mime        string     `json:"mime"`
			Headers     [][]string `json:"headers"`
			Navigation  string     `json:"navigation"`
			Destination string     `json:"destination"`
		}
		_ = json.Unmarshal([]byte(text), &items)
		var events []map[string]any
		mu.Lock()
		defer mu.Unlock()
		for _, item := range items {
			bodies[item.ID] = item.Body
			events = append(events, studioKeyResponseEvent(item.ID, item.URL, item.Mime, item.Headers, item.Navigation, item.Destination))
		}
		return events
	}
	hooks := fakeBiDiHooks{
		command: func(method string, params map[string]any) []map[string]any {
			switch method {
			case "input.performActions":
				process.evaluate("__click()")
			case "browsingContext.navigate":
				target, _ := params["url"].(string)
				process.evaluate("__navigate(" + jsString(target) + ")")
			}
			return drain()
		},
		result: func(method string, params map[string]any) map[string]any {
			if method != "network.getData" {
				return nil
			}
			requestID, _ := params["request"].(string)
			mu.Lock()
			defer mu.Unlock()
			body, ok := bodies[requestID]
			if !ok {
				return nil
			}
			return map[string]any{"bytes": map[string]any{"type": "string", "value": body}}
		},
	}
	page := newFakeBiDiPageWithHooks(t, process.evaluate, fakeEvaluate{value: fakeSnapshot}, hooks)
	session := &loginSession{client: page.client, contextID: "ctx-1"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := session.fetchStudioKey(ctx, options)
	var report studioKeyNodeReport
	reported := process.evaluate("__report")
	if text, ok := reported.value.(string); !ok || json.Unmarshal([]byte(text), &report) != nil {
		t.Fatalf("读取页面记录失败: %+v", reported)
	}
	return result, err, report
}

// studioKeyResponseEvent 照 BiDi network.responseCompleted 的格式编一条事件：请求头为 [名字, 值, 编码]（编码为 base64 时值按 base64 发出），
// navigation 不为空时是导航文档
func studioKeyResponseEvent(id, rawURL, mime string, headers [][]string, navigation, destination string) map[string]any {
	var encoded []any
	for _, header := range headers {
		if len(header) < 2 {
			continue
		}
		value := map[string]any{"type": "string", "value": header[1]}
		if len(header) > 2 && header[2] == "base64" {
			value = map[string]any{"type": "base64", "value": base64.StdEncoding.EncodeToString([]byte(header[1]))}
		}
		encoded = append(encoded, map[string]any{"name": header[0], "value": value})
	}
	request := map[string]any{"request": id, "url": rawURL, "method": "POST", "headers": encoded}
	if destination != "" {
		request["destination"] = destination
	}
	params := map[string]any{"context": "ctx-1", "navigation": nil, "request": request,
		"response": map[string]any{"status": 200, "url": rawURL, "mimeType": mime, "headers": []any{}}}
	if navigation != "" {
		params["navigation"] = navigation
	}
	return map[string]any{"type": "event", "method": "network.responseCompleted", "params": params}
}

// TestFetchStudioKeyCreatesProjectInNode 需要验证账号的账户常见情形：没有项目。经下拉框里的“Create project”建一个项目（勾必需的服务条款、
// 不勾邮件订阅），等它建好（页面自己的响应里出现新的 projects/<数字>），重新展开下拉框选上它，再创建 Key；项目与 Key 各只建一个，
// 记下新项目的编号与显示名，步骤里没有 Key。英文、简体中文、繁体中文、日文、韩文界面各跑一遍
func TestFetchStudioKeyCreatesProjectInNode(t *testing.T) {
	for _, lang := range []string{"en", "zh", "zh-TW", "ja", "ko"} {
		t.Run(lang, func(t *testing.T) {
			checkStudioKeyCreatesProject(t, lang)
		})
	}
}

// checkStudioKeyCreatesProject 在 lang 界面语言的合成页面上跑没有项目的场景（见 TestFetchStudioKeyCreatesProjectInNode）
func checkStudioKeyCreatesProject(t *testing.T, lang string) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "create-project", "lang": lang})
	if err != nil {
		t.Fatalf("应创建成功: %v（页面记录 %+v）", err, report)
	}
	if result.Key != fakeNewKey || result.ProjectName != "projects/123456789012" || result.ProjectNumber != "123456789012" ||
		result.ProjectDisplay != "Gemini Project" || !result.Created || !result.ProjectCreated {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	if report.ProjectCreates != 1 || report.KeyCreates != 1 || !report.Tos || report.Mail {
		t.Fatalf("应各建一个项目与 Key、只勾服务条款: %+v", report)
	}
	wantOrder := []string{"create-key", "select", "opt-create", "tos", "project-confirm", "select", "opt-project", "key-confirm"}
	if !slices.Equal(report.Log, wantOrder) {
		t.Fatalf("点击顺序\n得到 %v\n期望 %v", report.Log, wantOrder)
	}
	steps := strings.Join(result.Steps, "→")
	if strings.Contains(steps, "TESTKEY") || !strings.Contains(steps, "确认新建项目“") || !strings.Contains(steps, "新项目已出现（projects/123456789012）") {
		t.Fatalf("步骤 = %s", steps)
	}
}

// TestFetchStudioKeyExistingProjectInNode 已有项目时选它（不点下拉框里的“Create project”）；页面上需要验证账号的横幅不影响，横幅里的链接不点
func TestFetchStudioKeyExistingProjectInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "existing", "banner": true})
	if err != nil {
		t.Fatalf("应创建成功: %v（页面记录 %+v）", err, report)
	}
	if result.ProjectName != "projects/210987654321" || result.ProjectDisplay != "Default Gemini Project" || !result.Created || result.ProjectCreated {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	if report.ProjectCreates != 0 || report.KeyCreates != 1 || !slices.Equal(report.Log, []string{"create-key", "select", "opt-project", "key-confirm"}) {
		t.Fatalf("应选已有项目、只建一个 Key: %+v", report)
	}
}

// TestFetchStudioKeySlowProjectListInNode 已有项目、但下拉框展开后过一会儿才列出它（先只有导入与“Create project”）：等列表稳定，
// 选上已有项目，不新建项目
func TestFetchStudioKeySlowProjectListInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "slow-list"})
	if err != nil {
		t.Fatalf("应创建成功: %v（页面记录 %+v）", err, report)
	}
	if result.ProjectCreated || report.ProjectCreates != 0 || report.KeyCreates != 1 ||
		!slices.Equal(report.Log, []string{"create-key", "select", "opt-project", "key-confirm"}) {
		t.Fatalf("应等列表加载出已有项目再选它、不新建: %+v（结果 %+v）", report, redactResult(result))
	}
}

// TestFetchStudioKeyEmptyStateInNode 没有项目时 API Keys 页上只有“Create project”、没有“Create API key”：去一次 Projects 页点它自己的
// 新建按钮建一个项目，建好后回到 API Keys 页点“Create API key”，选上新项目创建 Key；项目与 Key 各只建一个
func TestFetchStudioKeyEmptyStateInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "no-create"})
	if err != nil {
		t.Fatalf("应创建成功: %v（页面记录 %+v）", err, report)
	}
	if !result.Created || !result.ProjectCreated || result.ProjectName != "projects/123456789012" || result.ProjectDisplay != "Gemini Project" {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	wantOrder := []string{"projects-create", "tos", "project-confirm", "create-key", "select", "opt-project", "key-confirm"}
	if report.ProjectCreates != 1 || report.KeyCreates != 1 || report.Mail || !slices.Equal(report.Log, wantOrder) ||
		!slices.Equal(report.Navigations, []string{"/apikey", "/projects", "/apikey"}) {
		t.Fatalf("页面记录 %+v，期望点击顺序 %v", report, wantOrder)
	}
	if steps := strings.Join(result.Steps, "→"); !strings.Contains(steps, "打开 Projects 页") || !strings.Contains(steps, "回到 API Keys 页") {
		t.Fatalf("步骤 = %s", steps)
	}
}

// TestFetchStudioKeyNewProjectDefaultKeyInNode 空状态下经 Projects 页新建的项目自带一个 Key，回到 API Keys 页后它晚一点才出现在响应里：
// 先等 settle 读到它就返回，不再点“Create API key”（不多建 Key）
func TestFetchStudioKeyNewProjectDefaultKeyInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "no-create", "defaultKey": true}, func() {
		studioKeyTiming.settle = time.Second
	})
	if err != nil {
		t.Fatalf("应读到新项目自带的 Key: %v（页面记录 %+v）", err, report)
	}
	if result.Key != fakeNewKey || result.ProjectNumber != "123456789012" || !result.ProjectCreated {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	if report.ProjectCreates != 1 || report.KeyCreates != 0 || slices.Contains(report.Log, "create-key") {
		t.Fatalf("不应再点“Create API key”: %+v", report)
	}
}

// TestFetchStudioKeyShownAfterReturnInNode 空状态下经 Projects 页新建项目、回到 API Keys 页后页面上显示着 Key、响应里却没有：
// 像第一次那样不创建，以“页面上已有 Key 但读不到”失败
func TestFetchStudioKeyShownAfterReturnInNode(t *testing.T) {
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "no-create", "shownKey": true})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonUnreadable {
		t.Fatalf("应以页面上已有 Key 但读不到失败: %v", err)
	}
	if report.ProjectCreates != 1 || report.KeyCreates != 0 || slices.Contains(report.Log, "create-key") {
		t.Fatalf("不应点“Create API key”: %+v", report)
	}
}

// TestFetchStudioKeyExternalProjectLinkInNode 下拉框里没有“Create project”、创建 Key 的对话框里只有指向 Google Cloud 控制台的站外链接：
// 不点链接，改去 Projects 页新建项目，回来选上它创建 Key
func TestFetchStudioKeyExternalProjectLinkInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "create-project", "externalEntry": true})
	if err != nil {
		t.Fatalf("应创建成功: %v（页面记录 %+v）", err, report)
	}
	if !result.ProjectCreated || result.ProjectNumber != "123456789012" {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	wantOrder := []string{"create-key", "select", "projects-create", "tos", "project-confirm", "create-key", "select", "opt-project", "key-confirm"}
	if slices.Contains(report.Log, "cloud-link") || report.ProjectCreates != 1 || report.KeyCreates != 1 || !slices.Equal(report.Log, wantOrder) ||
		!slices.Equal(report.Navigations, []string{"/apikey", "/projects", "/apikey"}) {
		t.Fatalf("页面记录 %+v，期望点击顺序 %v", report, wantOrder)
	}
}

// TestFetchStudioKeyEmptyProjectNameInNode 新建项目对话框的项目名为空：不勾、不确认、不输入文字，以具体原因失败（带步骤与对话框摘要）
func TestFetchStudioKeyEmptyProjectNameInNode(t *testing.T) {
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "empty-name"})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonProjectName {
		t.Fatalf("应以项目名为空失败: %v", err)
	}
	if report.ProjectCreates != 0 || report.KeyCreates != 0 || report.Tos || slices.Contains(report.Log, "project-confirm") {
		t.Fatalf("不应勾选或确认: %+v", report)
	}
	if !strings.Contains(err.Error(), "标题=Create a new project") || !strings.Contains(err.Error(), "输入框为空=Project name") {
		t.Fatalf("原因应带对话框摘要: %s", err)
	}
}

// TestFetchStudioKeyVerifyRedirectInNode API Keys 页总被跳到验证账号页：重新打开一次，仍被跳走时以具体原因失败，验证页上什么都不点
func TestFetchStudioKeyVerifyRedirectInNode(t *testing.T) {
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "verify-redirect"})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonVerifyPage {
		t.Fatalf("应以被跳到验证账号页失败: %v", err)
	}
	if len(report.Navigations) != 2 || len(report.Log) != 0 {
		t.Fatalf("应只重新打开一次、不点任何东西: %+v", report)
	}
}

// TestStudioKeyInspectExpressionReadOnly 读页面状态的表达式只读：不点击、不写值、不派发事件、不导航
func TestStudioKeyInspectExpressionReadOnly(t *testing.T) {
	target := studioKeyTargetExpression(studioKeyTarget{Scope: "dialog", Text: "x"})
	for _, forbidden := range []string{".click(", "dispatchEvent", "setter", "location.assign", "location.reload",
		"location.href =", "history.", "localStorage", "setAttribute", "removeAttribute", ".focus(", "scrollIntoView", ".checked ="} {
		if strings.Contains(studioKeyInspectExpression, forbidden) {
			t.Fatalf("读页面状态的表达式不应包含 %q", forbidden)
		}
		if strings.Contains(target, forbidden) {
			t.Fatalf("找点击目标的表达式不应包含 %q", forbidden)
		}
	}
	// 只比较、不赋值
	assignment := regexp.MustCompile(`\.(value|textContent|innerText|innerHTML)\s*=[^=]`)
	if assignment.MatchString(studioKeyInspectExpression) || assignment.MatchString(target) {
		t.Fatal("表达式不应给页面元素赋值")
	}
}

// studioKeyTableProjects 为 table 场景项目列表响应里的两个项目（合成数据）
var studioKeyTableProjects = []map[string]string{
	{"number": "210987654321", "id": "gen-lang-client-0987654321", "name": "Default Gemini Project"},
	{"number": "123456789012", "id": "gen-lang-client-0123456789", "name": "Gemini Project"},
}

// TestFetchStudioKeyProjectFromRowInNode 已有 Key、响应里 Key 的记录没有项目编号、项目列表里有两个项目（对不上哪个）：读 Key 列表里这一行
// （只读），项目列的“Default Gemini Project”整段对上其中一个项目，记下它的编号、显示名与项目 ID；不点任何东西、不创建
func TestFetchStudioKeyProjectFromRowInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "listProjects": studioKeyTableProjects,
		"rows": []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "Default Gemini Project"}}})
	if err != nil {
		t.Fatalf("应读到 Key: %v（页面记录 %+v）", err, report)
	}
	if result.Key != fakeNewKey || result.ProjectNumber != "210987654321" || result.ProjectName != "projects/210987654321" ||
		result.ProjectDisplay != "Default Gemini Project" || result.ProjectID != "gen-lang-client-0987654321" || result.Created {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	if len(report.Log) != 0 || report.KeyCreates != 0 || report.DetailsOpens != 0 {
		t.Fatalf("读这一行不应点任何东西: %+v", report)
	}
	if steps := strings.Join(result.Steps, "→"); !strings.Contains(steps, "从 Key 列表里这一行读到") || strings.Contains(steps, "TESTKEY") {
		t.Fatalf("步骤 = %s", steps)
	}
}

// TestFetchStudioKeyProjectFromDetailsInNode 响应里没有项目、Key 列表里这一行只有对不上的显示名：点这一行自己的 Key 名称打开
// “API key details”（只一次），按标签读出项目名与项目编号，用它自己的关闭按钮关掉；行里的复制、删除、更多操作、站外链接与对话框里的
// 复制、删除都不点，不点“Create API key”。英文、简体中文、繁体中文、日文、韩文界面各用一种对话框结构
func TestFetchStudioKeyProjectFromDetailsInNode(t *testing.T) {
	for _, item := range []struct{ lang, layout string }{{"en", "dl"}, {"en", "inline"}, {"zh", "form"}, {"zh-TW", "inline"}, {"ja", "table"}, {"ko", "spans"}} {
		t.Run(item.lang+"-"+item.layout, func(t *testing.T) {
			result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "lang": item.lang, "detailsLayout": item.layout,
				"rows": []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"},
					{"name": "Second key", "key": "other", "project": "Default Gemini Project"}},
				"details": map[string]string{"name": "My Other Project", "number": "345678901234"}})
			if err != nil {
				t.Fatalf("应读到 Key: %v（页面记录 %+v）", err, report)
			}
			if result.Key != fakeNewKey || result.ProjectNumber != "345678901234" || result.ProjectName != "projects/345678901234" ||
				result.ProjectDisplay != "My Other Project" || result.Created {
				t.Fatalf("结果 = %+v", redactResult(result))
			}
			if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.DetailsOpens != 1 || report.Dialogs != 0 || report.KeyCreates != 0 {
				t.Fatalf("应只点这一行的 Key 名称与详情的关闭按钮: %+v", report)
			}
			steps := strings.Join(result.Steps, "→")
			if !strings.Contains(steps, "打开 Key 详情") || !strings.Contains(steps, "项目信息：从 Key 详情读到") || !strings.Contains(steps, "关闭 Key 详情") ||
				strings.Contains(steps, "TESTKEY") {
				t.Fatalf("步骤 = %s", steps)
			}
		})
	}
}

// TestFetchStudioKeyResourceProjectNameInNode Key 列表的记录里只有 Key 的资源名（projects/<数字>/…/keys/…）与 Key 自己的名字
// “Gemini API Key”：编号取资源名，显示名不是 Key 自己的名字，而是项目列表里这个项目的（两条响应谁先到都一样；晚到时见
// TestStudioKeyCompleteDisplayWaitsForLateProjects）；
// 没有项目列表时只读地读 Key 列表这一行的项目列；这一行写着别的已知项目时不用。都不点任何东西
func TestFetchStudioKeyResourceProjectNameInNode(t *testing.T) {
	rows := func(project string) []map[string]string {
		return []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": project, "number": "210987654321"}}
	}
	for _, item := range []struct {
		name              string
		variant           map[string]any
		display, id, step string
	}{
		{"keys-first", map[string]any{"listProjects": studioKeyTableProjects, "rows": rows("Default Gemini Project")},
			"Default Gemini Project", "gen-lang-client-0987654321", ""},
		{"projects-first", map[string]any{"projectsFirst": true, "listProjects": studioKeyTableProjects, "rows": rows("Default Gemini Project")},
			"Default Gemini Project", "gen-lang-client-0987654321", ""},
		{"row-only", map[string]any{"rows": rows("Default Gemini Project")}, "Default Gemini Project", "", "项目显示名：从 Key 列表里这一行读到"},
		{"row-other-project", map[string]any{"listProjects": studioKeyTableProjects[1:], "rows": rows("Gemini Project")}, "", "",
			"Key 列表里这一行的项目与响应里的不一致，没有用"},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "table", "keyResource": true}
			for name, value := range item.variant {
				variant[name] = value
			}
			result, err, report := runStudioKeyInNode(t, variant)
			if err != nil {
				t.Fatalf("应读到 Key: %v（页面记录 %+v）", err, report)
			}
			if result.Key != fakeNewKey || result.ProjectNumber != "210987654321" || result.ProjectName != "projects/210987654321" ||
				result.ProjectDisplay != item.display || result.ProjectID != item.id || result.Created {
				t.Fatalf("结果 = %+v", redactResult(result))
			}
			if len(report.Log) != 0 || report.DetailsOpens != 0 || report.KeyCreates != 0 {
				t.Fatalf("不应点任何东西: %+v", report)
			}
			if steps := strings.Join(result.Steps, "→"); !strings.Contains(steps, item.step) || strings.Contains(steps, "TESTKEY") {
				t.Fatalf("步骤 = %s", steps)
			}
		})
	}
}

// TestStudioKeyCompleteDisplayWaitsForLateProjects 有编号、没有显示名，Key 列表里也读不到这一行时，等一会儿后到的项目列表响应
// （真实浏览器的事件随时推来，这里在另一个 goroutine 里晚 100ms 送到）补上显示名与项目 ID；一直没有时步骤里记一笔、不算失败。
// 整个过程只有读页面与读响应的命令（不点击、不导航）
func TestStudioKeyCompleteDisplayWaitsForLateProjects(t *testing.T) {
	fastStudioKey(t)
	list := ")]}'\n" + `[[["projects/210987654321","gen-lang-client-0987654321","Default Gemini Project"]]]`
	page := newFakeBiDiPageWithHooks(t, func(string) fakeEvaluate { return fakeEvaluate{value: `{"count":0,"cells":[]}`} },
		fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{result: func(method string, params map[string]any) map[string]any {
			if method == "network.getData" && params["request"] == "req-late" {
				return map[string]any{"bytes": map[string]any{"type": "string", "value": list}}
			}
			return nil
		}})
	for _, late := range []bool{true, false} {
		run := &studioKeyRun{session: &loginSession{client: page.client, contextID: "ctx-1"}, listener: &studioKeyListener{}, flow: newStudioKeyFlow()}
		if late {
			time.AfterFunc(100*time.Millisecond, func() {
				run.listener.observe(map[string]any{"request": map[string]any{"request": "req-late", "url": "https://aistudio.google.com/x"}})
			})
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		result := StudioKeyResult{Key: fakeNewKey, ProjectName: "projects/210987654321", ProjectNumber: "210987654321"}
		run.completeDisplay(ctx, &result)
		cancel()
		steps := strings.Join(run.notes, "→")
		switch {
		case late && (result.ProjectDisplay != "Default Gemini Project" || result.ProjectID != "gen-lang-client-0987654321" ||
			strings.Contains(steps, "项目显示名：没有找到")):
			t.Fatalf("应等到后到的项目列表: %+v（步骤 %s）", redactResult(result), steps)
		case !late && (result.ProjectDisplay != "" || result.ProjectNumber != "210987654321" || !strings.Contains(steps, "项目显示名：没有找到")):
			t.Fatalf("一直没有时应记一笔、编号不变: %+v（步骤 %s）", redactResult(result), steps)
		}
	}
	if methods := page.commandMethods("script.evaluate", "network.getData"); len(methods) != 0 {
		t.Fatalf("补显示名只应读页面与读响应: %v", methods)
	}
}

// TestFetchStudioKeyDetailsBeatsReplyInNode 点开详情时页面另发出一条只含一个项目的响应（与这条 Key 无关）：不按“唯一的项目”把它套到
// 这条 Key 上（有没有显示名都一样）；Key 的记录里的项目 ID 恰好是那个项目的（强依据）、详情里按标签写着的编号却是另一个时按详情，
// 那个项目的 ID 也不留下。结果的编号、显示名、项目 ID 不来自两个不同的项目
func TestFetchStudioKeyDetailsBeatsReplyInNode(t *testing.T) {
	named := map[string]string{"number": "999999999999", "id": "gen-lang-client-0999999999", "name": "Unrelated Project"}
	nameless := map[string]string{"number": "999999999999", "id": "gen-lang-client-0999999999"}
	for _, item := range []struct {
		name     string
		reply    map[string]string
		recordID string
		step     string
	}{
		{"single-named", named, "", ""},
		{"single-nameless", nameless, "", ""},
		{"id-match", nameless, "gen-lang-client-0999999999", "详情里的项目编号与响应里对上的不一致，按详情"},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "table", "detailsReply": item.reply,
				"rows":    []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}},
				"details": map[string]string{"name": "My Other Project", "number": "345678901234"}}
			if item.recordID != "" {
				variant["keyRecordID"] = item.recordID
			}
			result, err, report := runStudioKeyInNode(t, variant)
			if err != nil {
				t.Fatalf("应读到 Key: %v（页面记录 %+v）", err, report)
			}
			if result.ProjectNumber != "345678901234" || result.ProjectName != "projects/345678901234" || result.ProjectDisplay != "My Other Project" ||
				result.ProjectID != "" {
				t.Fatalf("应按详情、不拼上别的项目: %+v", redactResult(result))
			}
			if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.DetailsOpens != 1 || report.KeyCreates != 0 {
				t.Fatalf("应只点这一行的 Key 名称与详情的关闭按钮: %+v", report)
			}
			steps := strings.Join(result.Steps, "→")
			if !strings.Contains(steps, "项目信息：从 Key 详情读到") || !strings.Contains(steps, item.step) || strings.Contains(steps, "按响应") ||
				strings.Contains(steps, "TESTKEY") {
				t.Fatalf("步骤 = %s", steps)
			}
		})
	}
}

// TestFetchStudioKeyDetailsSkipsRowActionsInNode 打开详情时只点 Key 名称：Key 那一格里在名称前面另有带文字的复制、删除按钮，以及
// 文字是打码 Key（…末四位）、但 aria-label 为 Copy、提示为 Delete 或带复制图标的按钮，都不点（去掉 rowAction 或 rowIcon 过滤时这里会失败）
func TestFetchStudioKeyDetailsSkipsRowActionsInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "keyCellActions": true,
		"rows":    []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}},
		"details": map[string]string{"name": "My Other Project", "number": "345678901234"}})
	if err != nil || result.ProjectNumber != "345678901234" {
		t.Fatalf("应从详情读到项目编号: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
	if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.DetailsOpens != 1 || report.KeyCreates != 0 {
		t.Fatalf("应只点 Key 名称与详情的关闭按钮: %+v", report)
	}
}

// TestFetchStudioKeyReadOnlyPicksSavedInNode 只读（补项目信息）：页面上有两条 Key，只认已保存的那条（第二条），读它那一行的项目；
// 一行读不出编号时点的是它自己那一行的 Key 名称；不点“Create API key”
func TestFetchStudioKeyReadOnlyPicksSavedInNode(t *testing.T) {
	rows := []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "Gemini Project"},
		{"name": "Second key", "key": "other", "project": "Default Gemini Project"}}
	options := StudioKeyOptions{ReadOnly: true, SavedKey: fakeOtherKey}
	result, err, report := runStudioKeyInNodeWith(t, map[string]any{"scenario": "table", "listProjects": studioKeyTableProjects, "rows": rows}, options)
	if err != nil || result.Key != fakeOtherKey || result.ProjectNumber != "210987654321" || result.Created || len(report.Log) != 0 {
		t.Fatalf("应只读已保存的 Key 与它那一行的项目: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
	result, err, report = runStudioKeyInNodeWith(t, map[string]any{"scenario": "table", "rows": rows,
		"details": map[string]string{"name": "Default Gemini Project", "number": "210987654321"}}, options)
	if err != nil || result.Key != fakeOtherKey || result.ProjectNumber != "210987654321" ||
		!slices.Equal(report.Log, []string{"key-name-1", "details-close"}) || report.KeyCreates != 0 {
		t.Fatalf("应点已保存的 Key 那一行的名称: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
}

// TestFetchStudioKeyReadOnlyNeverCreatesInNode 只读：页面上一条 Key 都没有（账户没有项目或已有项目）时也不点“Create API key”、
// 不新建项目、不点下拉框，以“页面上没有找到已保存的 Key”失败
func TestFetchStudioKeyReadOnlyNeverCreatesInNode(t *testing.T) {
	for _, scenario := range []string{"create-project", "existing", "no-create"} {
		t.Run(scenario, func(t *testing.T) {
			_, err, report := runStudioKeyInNodeWith(t, map[string]any{"scenario": scenario}, StudioKeyOptions{ReadOnly: true, SavedKey: fakeNewKey})
			var keyErr *StudioKeyError
			if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonSavedMissing {
				t.Fatalf("应以没有找到已保存的 Key 失败: %v", err)
			}
			if len(report.Log) != 0 || report.KeyCreates != 0 || report.ProjectCreates != 0 || !slices.Equal(report.Navigations, []string{"/apikey"}) {
				t.Fatalf("只读时不应点任何东西、不应导航到别处: %+v", report)
			}
		})
	}
}

// studioKeyGluedScenes 为 TestStudioKeyGluedTextInNode 的页面（innerText 照真实浏览器，相邻的行内元素之间没有空白时文字连在一起）
const studioKeyGluedScenes = `
gluedText = true;
const beforeEvaluate = () => {};
const report = () => ({});
const copyIcon = name => h('button', {'aria-label': 'Copy', 'data-name': name || 'copy-icon'}, icon('content_copy'));
const page = (main, dialog) => {
  const overlay = h('div', {class: 'cdk-overlay-container'}, ...(dialog ? [h('div', {class: 'cdk-overlay-pane'},
    h('mat-dialog-container', {role: 'dialog'}, ...dialog))] : []));
  mount(h('body', {}, h('ms-app', {}, h('main', {}, h('h1', {}, 'API Keys'), ...main)), overlay), '/apikey');
  document.querySelector = selector => document.querySelectorAll(selector)[0] || null;
};
const createButton = () => h('button', {'data-name': 'create-key'}, icon('key'), 'Create API key');
const table = (...cells) => h('table', {class: 'mat-mdc-table'}, h('thead', {}, h('tr', {class: 'mat-mdc-header-row'},
  ...['Key', 'Project', ''].map(text => h('th', {class: 'mat-mdc-header-cell'}, text)))),
  h('tbody', {}, h('tr', {class: 'mat-mdc-row'}, ...cells, h('td', {class: 'mat-mdc-cell'}, copyIcon('copy-0'),
    h('button', {'data-name': 'delete-0'}, icon('delete'), 'Delete')))));
const keyName = () => h('button', {class: 'mdc-button name-link', 'data-name': 'key-name'}, 'Gemini API Key');
globalThis.scene = name => {
  switch (name) {
    case 'created-icon':
      page([createButton()], [h('h2', {}, 'API key created'), h('p', {}, h('span', {}, otherKey), copyIcon()), h('button', {}, 'Done')]);
      break;
    case 'details-icon':
      page([], [h('h2', {}, 'API key details'), h('div', {}, h('span', {}, 'API key'), h('span', {}, ': '), h('span', {}, otherKey), copyIcon()),
        h('button', {}, 'Close')]);
      break;
    case 'details-label-classic':
      page([], [h('h2', {}, 'API key details'), h('div', {}, h('span', {}, 'API key: '), h('span', {}, classicKey), copyIcon()), h('button', {}, 'Close')]);
      break;
    case 'details-label':
      page([], [h('h2', {}, 'API key details'), h('div', {}, h('span', {}, 'API key'), h('span', {}, otherKey)), h('button', {}, 'Close')]);
      break;
    case 'details-prefix-split':
      page([], [h('h2', {}, 'API key details'), h('div', {}, h('span', {}, 'API key'), h('span', {}, otherKey.slice(0, 24)),
        h('span', {}, '…' + otherKey.slice(-4))), h('button', {}, 'Close')]);
      break;
    case 'list-icon':
      page([createButton(), table(h('td', {class: 'mat-mdc-cell'}, keyName(), h('span', {}, '…' + otherKey.slice(-4)), copyIcon('cell-copy')),
        h('td', {class: 'mat-mdc-cell'}, 'My Project'))]);
      break;
    case 'list-own-text':
      page([createButton(), table(h('td', {class: 'mat-mdc-cell'}, keyName(), '…' + otherKey.slice(-4), copyIcon('cell-copy')),
        h('td', {class: 'mat-mdc-cell'}, 'My Project'))]);
      break;
    case 'list-full-icon':
      page([createButton(), table(h('td', {class: 'mat-mdc-cell'}, keyName(), h('span', {}, otherKey), copyIcon('cell-copy')),
        h('td', {class: 'mat-mdc-cell'}, 'My Project'))]);
      break;
  }
  return name;
};
`

// TestStudioKeyGluedTextInNode 真实浏览器里相邻的行内元素之间没有空白时（Angular 去掉了模板里只含空白的文本节点），innerText 会把 Key
// 与后面复制图标的 content_copy、前面的“API key”标签连成一串。读完整 Key（创建结果、详情）与 Key 列表都按文本节点找：读到的是 Key 本身，
// 不会把 content_copy 吞进 AQ. Key（错 Key），也不会因为前后紧挨着字母而读不到（对的 Key 被拒）；列表里打码 Key 后面紧挨着复制按钮时
// 照样认出这一行，打开详情点的是 Key 名称（不是复制按钮）。“前缀…末四位”分在两个元素里时前缀仍不算完整 Key
func TestStudioKeyGluedTextInNode(t *testing.T) {
	process := startOnboardingNode(t, "const otherKey = "+jsString(fakeOtherKey)+";\nconst classicKey = "+jsString(fakeClassicKey)+";\n"+studioKeyGluedScenes)
	evaluate := func(expression string) any {
		t.Helper()
		answer := process.evaluate(expression)
		if answer.exception != "" {
			t.Fatalf("执行失败: %s", answer.exception)
		}
		return answer.value
	}
	evaluate("scene('created-icon')")
	if text, _ := evaluate("document.querySelector('mat-dialog-container').innerText").(string); !strings.Contains(text, fakeOtherKey+"content_copy") {
		t.Fatal("合成页面的 innerText 应把 Key 与复制图标连在一起（否则测不到这种情形）")
	}
	if got := evaluate(studioKeyCreatedKeyExpression); got != `["`+fakeOtherKey+`"]` {
		t.Fatalf("创建结果对话框应只读到 Key 本身: %d 个字符", len(stringValue(got)))
	}
	for _, item := range []struct{ scene, last4, want string }{
		{"details-icon", "TST1", fakeOtherKey}, {"details-label", "TST1", fakeOtherKey},
		{"details-label-classic", StudioKeyLast4(fakeClassicKey), fakeClassicKey}, {"details-prefix-split", "TST1", ""},
	} {
		evaluate("scene('" + item.scene + "')")
		if got := evaluate(studioKeyDetailsKeyExpression(item.last4, StudioKeyPath)); got != item.want {
			t.Fatalf("%s: 详情里读到的 Key 长度 %d，期望 %d", item.scene, len(stringValue(got)), len(item.want))
		}
	}
	for scene, want := range map[string]string{"list-icon": `[{"l":"TST1"}]`, "list-own-text": `[{"l":"TST1"}]`,
		"list-full-icon": `[{"l":"TST1","f":"` + fakeOtherKey + `"}]`} {
		evaluate("scene('" + scene + "')")
		var view studioKeyListView
		encoded, _ := evaluate(studioKeyListExpression).(string)
		if json.Unmarshal([]byte(encoded), &view) != nil || view.State != studioKeyListRows {
			t.Fatalf("%s: 应认出 Key 列表: %s", scene, RedactStudioKeys(encoded))
		}
		rows, _ := json.Marshal(view.Rows)
		if string(rows) != want {
			t.Fatalf("%s: 列表 %s", scene, RedactStudioKeys(string(rows)))
		}
		target := evaluate("(() => { const element = " + studioKeyDetailsTargetExpression("TST1") + "; return element && element.getAttribute('data-name'); })()")
		if target != "key-name" {
			t.Fatalf("%s: 打开详情应点 Key 名称: %v", scene, target)
		}
	}
}

// TestStudioKeyProjectExpressionsReadOnly 读 Key 列表、这一行、详情（项目与完整 Key）与创建结果的表达式只读：不点击、不写值、不派发事件、不导航；找点击目标的
// 表达式只返回元素，不自己点
func TestStudioKeyProjectExpressionsReadOnly(t *testing.T) {
	expressions := map[string]string{
		"row": studioKeyRowExpression("TST1"), "details": studioKeyDetailsExpression(StudioKeyPath),
		"target": studioKeyDetailsTargetExpression("TST1"), "close": studioKeyDetailsCloseExpression,
		"list": studioKeyListExpression, "details-key": studioKeyDetailsKeyExpression("TST1", StudioKeyPath), "created-key": studioKeyCreatedKeyExpression,
	}
	assignment := regexp.MustCompile(`\.(value|textContent|innerText|innerHTML|checked)\s*=[^=]`)
	for name, expression := range expressions {
		for _, forbidden := range []string{".click(", "dispatchEvent", "setter", "location.assign", "location.reload", "location.href =",
			"history.", "localStorage", "setAttribute", "removeAttribute", ".focus(", "scrollIntoView", "fetch(", "XMLHttpRequest"} {
			if strings.Contains(expression, forbidden) {
				t.Fatalf("%s 表达式不应包含 %q", name, forbidden)
			}
		}
		if assignment.MatchString(expression) {
			t.Fatalf("%s 表达式不应给页面元素赋值", name)
		}
	}
}

// checkNoKeyLeak 确认错误与步骤里没有任何完整的假 Key（只允许末四位）
func checkNoKeyLeak(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, "TESTKEY") || strings.Contains(text, "TESTCLIENT") || studioKeyScanPattern.MatchString(text) {
			t.Fatalf("文字里不应有完整 Key: %s", RedactStudioKeys(text))
		}
	}
}

// TestStudioKeyListenerDeniesRequestKeys 任何域名的请求（不只读正文的那几个）：请求头（名字不区分大小写，base64 编码的先解码）、
// 地址参数 ?key=、URL 编码在地址里的请求头（webchannel 的 $httpHeaders）里像 Key 的字符串都进拒绝集合，Cookie 不看；
// 只有 Google 相关域名的响应才记下来读正文，带上 MIME、导航与请求目标
func TestStudioKeyListenerDeniesRequestKeys(t *testing.T) {
	listener := &studioKeyListener{}
	header := func(name, value string, base64Value bool) map[string]any {
		encoded := map[string]any{"type": "string", "value": value}
		if base64Value {
			encoded = map[string]any{"type": "base64", "value": base64.StdEncoding.EncodeToString([]byte(value))}
		}
		return map[string]any{"name": name, "value": encoded}
	}
	other := "AIza" + "TESTOTHER" + strings.Repeat("o", 26)
	encodedHeaders := "X-Goog-Api-Key%3A" + other + "%0D%0AX-Goog-AuthUser%3A0"
	listener.observe(map[string]any{"navigation": nil, "request": map[string]any{"request": "r1",
		"url":     "https://alkalimakersuite-pa.clients6.google.com/$rpc/x.MakerSuiteService/ListCloudApiKeys",
		"headers": []any{header("x-GOOG-api-KEY", fakeClientKey, false), header("Cookie", "k="+fakeClassicKey, false)}},
		"response": map[string]any{"mimeType": "application/json+protobuf; charset=UTF-8"}})
	listener.observe(map[string]any{"request": map[string]any{"request": "r2", "url": "https://example.com/v1/models?key=" + fakeNewKey}})
	listener.observe(map[string]any{"request": map[string]any{"request": "r3",
		"url": "https://alkalimakersuite-pa.clients6.google.com/webchannel/x?$httpHeaders=" + encodedHeaders}, "response": map[string]any{}})
	listener.observe(map[string]any{"request": map[string]any{"request": "r4", "url": "https://generativelanguage.googleapis.com/v1beta/models",
		"headers": []any{header("X-Goog-Api-Key", fakeOtherKey, true)}}, "response": map[string]any{"mimeType": ""}})
	listener.observe(map[string]any{"navigation": "nav-1", "request": map[string]any{"request": "r5", "url": "https://aistudio.google.com/apikey",
		"destination": "document"}, "response": map[string]any{"headers": []any{header("Content-Type", "text/html; charset=utf-8", false)}}})
	requests, denied := listener.take()
	sources := map[string]studioKeyDenySource{}
	for _, item := range denied {
		sources[item.key] = item.source
	}
	for key, want := range map[string]string{fakeClientKey: "请求头 x-goog-api-key（alkalimakersuite-pa.clients6.google.com）",
		fakeNewKey: "请求地址（example.com）", other: "请求地址（alkalimakersuite-pa.clients6.google.com）",
		fakeOtherKey: "请求头 x-goog-api-key（generativelanguage.googleapis.com）"} {
		if sources[key].where != want {
			t.Fatalf("…%s 的来源 = %q，期望 %q（全部 %d 条）", StudioKeyLast4(key), sources[key].where, want, len(denied))
		}
	}
	if _, ok := sources[fakeClassicKey]; ok || len(denied) != 4 {
		t.Fatalf("Cookie 不应看，拒绝集合应有 4 条: %d", len(denied))
	}
	if !sources[fakeClientKey].strong || sources[fakeNewKey].strong || sources[fakeOtherKey].strong {
		t.Fatal("只有发给 AI Studio 自己接口的请求带的 Key 才算一定是页面自己的")
	}
	var ids []string
	for _, request := range requests {
		ids = append(ids, request.id)
	}
	if !slices.Equal(ids, []string{"r1", "r3", "r4", "r5"}) {
		t.Fatalf("只有 Google 相关域名的响应读正文: %v", ids)
	}
	if requests[0].mime != "application/json+protobuf" || requests[0].name != "ListCloudApiKeys" || requests[0].document ||
		!requests[3].document || requests[3].mime != "text/html" || requests[3].destination != "document" {
		t.Fatalf("响应的类型: %+v", requests)
	}
	if more, again := listener.take(); len(more) != 0 || len(again) != 0 {
		t.Fatal("take 之后应清空")
	}
	// 同一条 Key 只进一次；之后有更强的来源（发给 AI Studio 自己接口的请求）时再交一次
	listener.observe(map[string]any{"request": map[string]any{"url": "https://example.com/?key=" + fakeNewKey}})
	if _, again := listener.take(); len(again) != 0 {
		t.Fatal("已在拒绝集合里的 Key 不应再记")
	}
	listener.observe(map[string]any{"request": map[string]any{"url": "https://aistudio.google.com/x?key=" + fakeNewKey}})
	if _, again := listener.take(); len(again) != 1 || !again[0].source.strong {
		t.Fatalf("更强的来源应再交一次: %d", len(again))
	}
}

// TestFetchStudioKeyClientKeyNeverChosenInNode 端到端：页面的 HTML、脚本、配置接口的响应里都有页面自己的公共 Key，发给接口的请求头也带着它；
// Key 列表的响应里只有打码的 Key。结果是点开这一行的详情读到的这个账户的 Key，从不是公共 Key；只点 Key 名称与详情的关闭按钮，
// 错误与步骤里只有末四位
func TestFetchStudioKeyClientKeyNeverChosenInNode(t *testing.T) {
	for _, item := range []struct {
		name    string
		variant map[string]any
		from    string
	}{
		{"details-dom", map[string]any{}, "从详情对话框读到"},
		{"details-reply", map[string]any{"detailsMasked": true, "detailsKeyReply": true}, "从点开详情后的响应读到"},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "table", "maskedList": true,
				"rows":    []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}},
				"details": map[string]string{"name": "My Other Project", "number": "345678901234"}}
			for name, value := range item.variant {
				variant[name] = value
			}
			result, err, report := runStudioKeyInNode(t, variant)
			if err != nil {
				t.Fatalf("应从详情读到 Key: %v（页面记录 %+v）", err, report)
			}
			if result.Key != fakeNewKey || result.Key == fakeClientKey || result.ProjectNumber != "345678901234" || result.Created || result.KeyCount != 1 {
				t.Fatalf("结果 = %+v", redactResult(result))
			}
			if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.DetailsOpens != 1 || report.KeyCreates != 0 || report.Dialogs != 0 {
				t.Fatalf("应只点这一行的 Key 名称与详情的关闭按钮: %+v", report)
			}
			steps := strings.Join(result.Steps, "→")
			if !strings.Contains(steps, "完整 Key："+item.from) || !strings.Contains(steps, "诊断：列表 1 行(…xxxx)") ||
				!strings.Contains(steps, "…cccc←请求头 x-goog-api-key") && !strings.Contains(steps, "…cccc←页面文档或脚本") {
				t.Fatalf("步骤 = %s", steps)
			}
			checkNoKeyLeak(t, steps)
		})
	}
}

// TestFetchStudioKeyDetailsWrongKeyInNode 详情里显示的完整 Key 末四位与这一行不同：不认，以“读不到”失败，不创建；只点 Key 名称与关闭按钮
func TestFetchStudioKeyDetailsWrongKeyInNode(t *testing.T) {
	saved := studioKeyDetailsSettle
	studioKeyDetailsSettle = 300 * time.Millisecond
	t.Cleanup(func() { studioKeyDetailsSettle = saved })
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "maskedList": true, "detailsWrongKey": true, "keyCellActions": true,
		"rows": []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}}})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonUnreadable {
		t.Fatalf("应以读不到失败: %v", err)
	}
	if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.KeyCreates != 0 {
		t.Fatalf("应只点 Key 名称与详情的关闭按钮（行里的复制、删除不点）: %+v", report)
	}
	if !strings.Contains(err.Error(), "Key 详情里没有读到末四位为 …xxxx 的完整 Key") || !strings.Contains(err.Error(), "列表 1 行(…xxxx)") {
		t.Fatalf("原因应带记录与诊断: %s", err)
	}
	checkNoKeyLeak(t, err.Error())
}

// TestFetchStudioKeyClientKeyListedInNode 页面的列表里显示的正好是公共 Key 的末四位、接口响应里也有完整的公共 Key：它在拒绝集合里，
// 不认；详情里显示的也是它，同样不认，以“读不到”失败
func TestFetchStudioKeyClientKeyListedInNode(t *testing.T) {
	saved := studioKeyDetailsSettle
	studioKeyDetailsSettle = 300 * time.Millisecond
	t.Cleanup(func() { studioKeyDetailsSettle = saved })
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table",
		"rows": []map[string]string{{"name": "Gemini API Key", "key": "client", "project": "My Other Project"}}})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonUnreadable {
		t.Fatalf("公共 Key 不应被认作账户的 Key: %v", err)
	}
	if report.KeyCreates != 0 || report.DetailsOpens != 1 {
		t.Fatalf("不应创建: %+v", report)
	}
	checkNoKeyLeak(t, err.Error())
}

// TestFetchStudioKeyListUnknownInNode 认不出 Key 列表（表格里有数据行、却没有打码的 Key），响应里却有完整 Key：不拿响应里的 Key 充数，
// 以“认不出 Key 列表”失败，不点任何东西、不创建
func TestFetchStudioKeyListUnknownInNode(t *testing.T) {
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "rowMask": "none", "listProjects": studioKeyTableProjects,
		"rows": []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "Default Gemini Project"}}})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonListUnknown {
		t.Fatalf("应以认不出 Key 列表失败: %v", err)
	}
	if len(report.Log) != 0 || report.KeyCreates != 0 || report.DetailsOpens != 0 {
		t.Fatalf("不应点任何东西: %+v", report)
	}
	if !strings.Contains(err.Error(), "列表认不出(数据行 1，创建按钮 enabled)") || !strings.Contains(err.Error(), "ListApiKeys(application/json+protobuf)") ||
		!strings.Contains(err.Error(), "…xxxx 没对上") || !strings.Contains(err.Error(), "…cccc 拒绝") {
		t.Fatalf("原因应带诊断: %s", err)
	}
	checkNoKeyLeak(t, err.Error())
}

// TestFetchStudioKeyCreatedConfirmedInNode 创建之后只认新出现、能确认的 Key：创建结果对话框只显示打码的 Key 时，按列表里新出现的一行
// 与响应对上（响应里另有公共 Key 与一条不相干的 Key，都不认）；响应里也只有打码的 Key 时关掉创建结果对话框（只一次）、打开新行的
// 详情读完整 Key；都没有新行时等到超时失败。每次都只建一个 Key
func TestFetchStudioKeyCreatedConfirmedInNode(t *testing.T) {
	for _, item := range []struct {
		name    string
		variant map[string]any
		log     []string
		reason  string
	}{
		{"row-and-reply", map[string]any{"createdRow": true, "createdMasked": true, "createExtra": true},
			[]string{"create-key", "select", "opt-project", "key-confirm"}, ""},
		{"row-and-details", map[string]any{"createdRow": true, "createdMasked": true, "createdNoReply": true},
			[]string{"create-key", "select", "opt-project", "key-confirm", "done", "key-name-0", "details-close"}, ""},
		{"nothing-new", map[string]any{"createdMasked": true, "createdNoReply": true, "createExtra": true},
			[]string{"create-key", "select", "opt-project", "key-confirm"}, StudioKeyReasonCreateFailed},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "existing"}
			for name, value := range item.variant {
				variant[name] = value
			}
			result, err, report := runStudioKeyInNode(t, variant, func() { studioKeyTiming.createWait = 1500 * time.Millisecond })
			if item.reason != "" {
				var keyErr *StudioKeyError
				if !errors.As(err, &keyErr) || keyErr.Reason != item.reason {
					t.Fatalf("应以 %q 失败: %v", item.reason, err)
				}
				checkNoKeyLeak(t, err.Error())
			} else {
				if err != nil {
					t.Fatalf("应读到新 Key: %v（页面记录 %+v）", err, report)
				}
				if result.Key != fakeNewKey || !result.Created || result.ProjectNumber != "210987654321" {
					t.Fatalf("结果 = %+v", redactResult(result))
				}
				checkNoKeyLeak(t, strings.Join(result.Steps, "→"))
			}
			if report.KeyCreates != 1 || report.ProjectCreates != 0 || !slices.Equal(report.Log, item.log) {
				t.Fatalf("页面记录 %+v，期望点击顺序 %v", report, item.log)
			}
		})
	}
}

// TestFetchStudioKeyReadOnlyMaskedInNode 只读、列表与响应里都只有打码的 Key：打开已保存的那条所在的行（第二行）的详情，读到的完整 Key
// 与已保存的相等才算；详情里的是另一条（末四位相同）时以“没有找到已保存的 Key”失败。都不点“Create API key”
func TestFetchStudioKeyReadOnlyMaskedInNode(t *testing.T) {
	rows := []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "Gemini Project"},
		{"name": "Second key", "key": "other", "project": "Default Gemini Project"}}
	variant := map[string]any{"scenario": "table", "maskedList": true, "rows": rows,
		"details": map[string]string{"name": "Default Gemini Project", "number": "210987654321"}}
	result, err, report := runStudioKeyInNodeWith(t, variant, StudioKeyOptions{ReadOnly: true, SavedKey: fakeOtherKey})
	if err != nil || result.Key != fakeOtherKey || result.ProjectNumber != "210987654321" ||
		!slices.Equal(report.Log, []string{"key-name-1", "details-close"}) || report.KeyCreates != 0 {
		t.Fatalf("应打开已保存的那条的详情: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
	twin := "AQ.TESTKEY" + strings.Repeat("q", 20) + "TST1"
	_, err, report = runStudioKeyInNodeWith(t, variant, StudioKeyOptions{ReadOnly: true, SavedKey: twin})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonSavedMissing || report.KeyCreates != 0 {
		t.Fatalf("末四位相同、完整 Key 不同时应以没有找到已保存的 Key 失败: %v %+v", err, report)
	}
}

// TestFetchStudioKeySavedIsClientInNode 已保存的 Key 就是页面自己的公共 Key（旧版本的错记录）：只读时直接失败并标出来（调用方据此作废记录），
// 不点任何东西；普通获取时不把它当已保存的 Key，照常读列表里这个账户的 Key
func TestFetchStudioKeySavedIsClientInNode(t *testing.T) {
	rows := []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "Default Gemini Project"}}
	_, err, report := runStudioKeyInNodeWith(t, map[string]any{"scenario": "table", "listProjects": studioKeyTableProjects, "rows": rows},
		StudioKeyOptions{ReadOnly: true, SavedKey: fakeClientKey})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonSavedIsClient || !keyErr.SavedIsClient || len(report.Log) != 0 {
		t.Fatalf("只读时应以已保存的是公共 Key 失败: %v %+v", err, report)
	}
	result, err, report := runStudioKeyInNodeWith(t, map[string]any{"scenario": "table", "listProjects": studioKeyTableProjects, "rows": rows},
		StudioKeyOptions{SavedKey: fakeClientKey})
	if err != nil || result.Key != fakeNewKey || len(report.Log) != 0 {
		t.Fatalf("普通获取应读到这个账户的 Key: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
}

// TestFetchStudioKeyTruncatedPrefixInNode AQ. 新格式 Key 的前缀（AQ. 后 20 位以上）也像完整 Key：列表里把 Key 显示成“长前缀…末四位”
// （同一段文字，或前缀与“…末四位”分在两个元素里；前缀的末四位可能碰巧与真正的末四位相同）、响应里只有打码的 Key 时，前缀不算完整 Key，
// 按“…末四位”认这一行、打开它的详情读完整 Key；详情里也只显示“长前缀…末四位”时以读不到失败。行上显示完整 Key、响应里却没有时
// 同样打开详情核对（读到的要与它相等：显示的是截断的前缀或末四位相同的另一条时以读不到失败）；响应里有同一条时不点。从不返回截断的前缀
func TestFetchStudioKeyTruncatedPrefixInNode(t *testing.T) {
	saved := studioKeyDetailsSettle
	studioKeyDetailsSettle = 300 * time.Millisecond
	t.Cleanup(func() { studioKeyDetailsSettle = saved })
	clicks := []string{"key-name-0", "details-close"}
	for _, item := range []struct {
		name    string
		key     string
		variant map[string]any
		log     []string
		reason  string
	}{
		{"row-prefix", "other", map[string]any{"rowMask": "prefix"}, clicks, ""},
		{"row-prefix-split", "other", map[string]any{"rowMask": "prefix-split"}, clicks, ""},
		{"row-prefix-same-tail", "saved", map[string]any{"rowMask": "prefix"}, clicks, ""},
		{"details-prefix", "other", map[string]any{"rowMask": "prefix", "detailsPrefix": true}, clicks, StudioKeyReasonUnreadable},
		{"details-prefix-same-tail", "saved", map[string]any{"rowMask": "prefix", "detailsPrefix": true}, clicks, StudioKeyReasonUnreadable},
		{"row-full-unconfirmed", "other", map[string]any{"rowMask": "full"}, clicks, ""},
		{"row-full-details-prefix", "other", map[string]any{"rowMask": "full", "detailsPrefix": true}, clicks, StudioKeyReasonUnreadable},
		{"row-full-details-twin", "other", map[string]any{"rowMask": "full", "detailsTwin": true}, clicks, StudioKeyReasonUnreadable},
		{"row-full-in-reply", "other", map[string]any{"rowMask": "full", "maskedList": false, "listProjects": studioKeyTableProjects,
			"rows": []map[string]string{{"name": "Gemini API Key", "key": "other", "project": "Default Gemini Project"}}}, nil, ""},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "table", "maskedList": true,
				"rows":    []map[string]string{{"name": "Gemini API Key", "key": item.key, "project": "My Other Project"}},
				"details": map[string]string{"name": "My Other Project", "number": "345678901234"}}
			for name, value := range item.variant {
				variant[name] = value
			}
			want := fakeOtherKey
			if item.key == "saved" {
				want = fakeNewKey
			}
			result, err, report := runStudioKeyInNode(t, variant)
			if item.reason != "" {
				var keyErr *StudioKeyError
				if !errors.As(err, &keyErr) || keyErr.Reason != item.reason {
					t.Fatalf("应以 %q 失败: %v（结果 %+v）", item.reason, err, redactResult(result))
				}
				checkNoKeyLeak(t, err.Error())
			} else {
				if err != nil {
					t.Fatalf("应读到完整 Key: %v（页面记录 %+v）", err, report)
				}
				if result.Key != want || result.Created {
					t.Fatalf("结果应是完整 Key（长度 %d），不是截断的前缀: 长度 %d", len(want), len(result.Key))
				}
				checkNoKeyLeak(t, strings.Join(result.Steps, "→"))
			}
			if !slices.Equal(report.Log, item.log) || report.KeyCreates != 0 {
				t.Fatalf("页面记录 %+v，期望点击 %v", report, item.log)
			}
		})
	}
}

// TestFetchStudioKeyCreatedDialogInNode 创建结果对话框里显示的不能直接当新 Key：显示“长前缀…末四位”时前缀不算；显示打码的新 Key 与一段带
// 完整公共 Key 的示例代码时公共 Key 在拒绝集合里，不算；显示一条不在拒绝集合里的完整 Key、末四位却与列表里新出现的行对不上时也不用。
// 有新出现的行时关掉对话框、打开新行的详情读完整 Key；没有新行时等到超时失败。从不返回前缀、公共 Key 或对不上的 Key，只建一个 Key
func TestFetchStudioKeyCreatedDialogInNode(t *testing.T) {
	created := []string{"create-key", "select", "opt-project", "key-confirm"}
	details := append(slices.Clone(created), "done", "key-name-0", "details-close")
	for _, item := range []struct {
		name    string
		variant map[string]any
		log     []string
		reason  string
		detail  string
	}{
		{"prefix-row", map[string]any{"createdShow": "prefix", "createdRow": true}, details, "", ""},
		{"prefix-no-row", map[string]any{"createdShow": "prefix"}, created, StudioKeyReasonCreateFailed, ""},
		{"client-row", map[string]any{"createdShow": "client", "createdRow": true}, details, "", ""},
		{"client-no-row", map[string]any{"createdShow": "client"}, created, StudioKeyReasonCreateFailed, ""},
		{"classic-row", map[string]any{"createdShow": "classic", "createdRow": true}, details, "", ""},
		// 对话框里的完整 Key 与后面的复制图标连在一起（innerText 照真实浏览器）：读到的是 Key 本身；还要有新出现的一行或响应印证才认，
		// 都没有时关掉对话框（只一次）等新行，仍没有就失败，不保存
		{"glued-row", map[string]any{"glue": true, "createdShow": "glued", "createdRow": true}, created, "", ""},
		{"glued-reply", map[string]any{"glue": true, "createdShow": "glued", "createdNoReply": false}, created, "", ""},
		{"glued-no-row", map[string]any{"glue": true, "createdShow": "glued"}, append(slices.Clone(created), "done"), StudioKeyReasonCreateFailed,
			"创建结果对话框里显示了新 Key（…xxxx），还没有新出现的行或响应印证"},
	} {
		t.Run(item.name, func(t *testing.T) {
			variant := map[string]any{"scenario": "existing", "createdNoReply": true}
			for name, value := range item.variant {
				variant[name] = value
			}
			result, err, report := runStudioKeyInNode(t, variant, func() { studioKeyTiming.createWait = 1500 * time.Millisecond })
			if item.reason != "" {
				var keyErr *StudioKeyError
				if !errors.As(err, &keyErr) || keyErr.Reason != item.reason || !strings.Contains(err.Error(), item.detail) {
					t.Fatalf("应以 %q 失败: %v（结果长度 %d）", item.reason, err, len(result.Key))
				}
				checkNoKeyLeak(t, err.Error())
			} else {
				if err != nil {
					t.Fatalf("应读到新 Key: %v（页面记录 %+v）", err, report)
				}
				if result.Key != fakeNewKey || !result.Created {
					t.Fatalf("结果应是新 Key: %+v", redactResult(result))
				}
				steps := strings.Join(result.Steps, "→")
				if item.name == "classic-row" && !strings.Contains(steps, "与列表里新出现的行对不上") {
					t.Fatalf("步骤应记下对话框里的 Key 对不上新行: %s", steps)
				}
				checkNoKeyLeak(t, steps)
			}
			if report.KeyCreates != 1 || report.ProjectCreates != 0 || !slices.Equal(report.Log, item.log) {
				t.Fatalf("页面记录 %+v，期望点击顺序 %v", report, item.log)
			}
		})
	}
}

// TestFetchStudioKeyClientKeyBodyOnlyInNode 公共 Key 只出现在页面文档、脚本与配置接口的正文里（请求的地址与请求头都不带它），主区域
// 另有一段带完整公共 Key 的示例代码，Key 列表只给打码的 Key：文档与脚本正文里的 Key 进拒绝集合，示例代码那一行不算，配置接口里的它也不认，
// 结果是这一行详情里的 Key；公共 Key 的指纹随结果交给调用方（账户自己的 Key 不在里面）
func TestFetchStudioKeyClientKeyBodyOnlyInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "maskedList": true, "noClientHeaders": true, "clientSample": true,
		"rows":    []map[string]string{{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}},
		"details": map[string]string{"name": "My Other Project", "number": "345678901234"}})
	if err != nil {
		t.Fatalf("应从详情读到 Key: %v（页面记录 %+v）", err, report)
	}
	if result.Key != fakeNewKey || result.Created || !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.KeyCreates != 0 {
		t.Fatalf("结果 = %+v，页面记录 %+v", redactResult(result), report)
	}
	steps := strings.Join(result.Steps, "→")
	if !strings.Contains(steps, "…cccc←页面文档或脚本") || strings.Contains(steps, "←请求头") {
		t.Fatalf("公共 Key 应只因正文进拒绝集合: %s", steps)
	}
	if !slices.Contains(result.ClientFingerprints, StudioKeyFingerprint(fakeClientKey)) || slices.Contains(result.ClientFingerprints, StudioKeyFingerprint(fakeNewKey)) {
		t.Fatalf("应交出公共 Key 的指纹、不含账户的 Key: %d 条", len(result.ClientFingerprints))
	}
	checkNoKeyLeak(t, steps)
}

// TestFetchStudioKeyEmptyStateLiveKeyInNode 页面是空状态（写着没有 Key、有创建按钮），Key 列表接口的响应里却有一条不在拒绝集合里的完整 Key：
// 页面上没有对得上的行，不拿它充数，也不创建，以“认不出 Key 列表”失败，什么都不点
func TestFetchStudioKeyEmptyStateLiveKeyInNode(t *testing.T) {
	_, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "existing", "emptyReplyKey": true})
	var keyErr *StudioKeyError
	if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonListUnknown {
		t.Fatalf("应以认不出 Key 列表失败: %v", err)
	}
	if len(report.Log) != 0 || report.KeyCreates != 0 || report.ProjectCreates != 0 {
		t.Fatalf("不应点任何东西、不应创建: %+v", report)
	}
	if !strings.Contains(err.Error(), "列表为空") || !strings.Contains(err.Error(), "…TST1 没对上") {
		t.Fatalf("原因应带诊断: %s", err)
	}
	checkNoKeyLeak(t, err.Error())
}

// TestFetchStudioKeySavedClientRowInNode 普通获取，已保存的 Key 是页面自己的公共 Key（旧版本的错记录），列表里正好有它末四位的一行：
// 它在拒绝集合里，不当作已保存的 Key，不优先选这一行，选第一行、打开它的详情读这个账户的 Key
func TestFetchStudioKeySavedClientRowInNode(t *testing.T) {
	result, err, report := runStudioKeyInNodeWith(t, map[string]any{"scenario": "table", "maskedList": true,
		"rows": []map[string]string{{"name": "Gemini API Key", "key": "other", "project": "My Other Project"},
			{"name": "Browser key", "key": "client", "project": "My Other Project"}},
		"details": map[string]string{"name": "My Other Project", "number": "345678901234"}}, StudioKeyOptions{SavedKey: fakeClientKey})
	if err != nil || result.Key != fakeOtherKey || !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.KeyCreates != 0 {
		t.Fatalf("不应按公共 Key 选行: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
}

// TestFetchStudioKeyExcludedInNode 排除的 Key（其他账户保存着的、已作废的）从不选用：列表里有别的行时选别的行；页面上的 Key 都被排除时
// 以 StudioKeyReasonExcluded 失败，不点、不创建；只有指纹（列表里看不出是哪一行）时打开详情读到它也不用，同样失败。错误里带着公共 Key 的指纹
func TestFetchStudioKeyExcludedInNode(t *testing.T) {
	saved := studioKeyDetailsSettle
	studioKeyDetailsSettle = 300 * time.Millisecond
	t.Cleanup(func() { studioKeyDetailsSettle = saved })
	variant := func(rows ...map[string]string) map[string]any {
		return map[string]any{"scenario": "table", "maskedList": true, "rows": rows,
			"details": map[string]string{"name": "My Other Project", "number": "345678901234"}}
	}
	other := map[string]string{"name": "Shared key", "key": "other", "project": "Shared Project"}
	own := map[string]string{"name": "Gemini API Key", "key": "saved", "project": "My Other Project"}
	result, err, report := runStudioKeyInNodeWith(t, variant(other, own), StudioKeyOptions{Exclude: []string{fakeOtherKey}})
	if err != nil || result.Key != fakeNewKey || !slices.Equal(report.Log, []string{"key-name-1", "details-close"}) {
		t.Fatalf("应跳过排除的那一行: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
	for _, item := range []struct {
		name    string
		options StudioKeyOptions
		log     []string
		detail  string
	}{
		{"exclude", StudioKeyOptions{Exclude: []string{fakeOtherKey}}, nil, "列表里 1 行是其他账户保存着的或已作废的 Key，排除"},
		{"fingerprint", StudioKeyOptions{ExcludeFingerprints: []string{StudioKeyFingerprint(fakeOtherKey)}}, []string{"key-name-0", "details-close"}, "列表 1 行(…TST1)"},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err, report := runStudioKeyInNodeWith(t, variant(other), item.options)
			var keyErr *StudioKeyError
			if !errors.As(err, &keyErr) || keyErr.Reason != StudioKeyReasonExcluded || !strings.Contains(err.Error(), item.detail) {
				t.Fatalf("应以页面上的 Key 都被排除失败: %v", err)
			}
			if !slices.Equal(report.Log, item.log) || report.KeyCreates != 0 || report.ProjectCreates != 0 {
				t.Fatalf("页面记录 %+v，期望点击 %v", report, item.log)
			}
			if !slices.Contains(keyErr.ClientFingerprints, StudioKeyFingerprint(fakeClientKey)) {
				t.Fatal("错误里应带公共 Key 的指纹")
			}
			checkNoKeyLeak(t, err.Error())
		})
	}
}

// TestFetchStudioKeyGluedPageInNode innerText 照真实浏览器（相邻的行内元素之间没有空白时文字连在一起）：列表里打码的 Key 后面紧挨着
// 复制按钮、详情里每项的标签、值与复制按钮紧挨着时，照样认出这一行、点它的 Key 名称打开详情读到完整 Key 与项目，再用详情的关闭按钮关掉；
// 行里与详情里的复制按钮都不点
func TestFetchStudioKeyGluedPageInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "table", "glue": true, "maskedList": true, "rowMask": "glued",
		"detailsLayout": "glued", "rows": []map[string]string{{"name": "Gemini API Key", "key": "other", "project": "My Other Project"}},
		"details": map[string]string{"name": "My Other Project", "number": "345678901234"}})
	if err != nil {
		t.Fatalf("应从详情读到 Key: %v（页面记录 %+v）", err, report)
	}
	if result.Key != fakeOtherKey || result.ProjectNumber != "345678901234" || result.ProjectDisplay != "My Other Project" || result.Created {
		t.Fatalf("结果 = %+v", redactResult(result))
	}
	if !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) || report.KeyCreates != 0 || report.Dialogs != 0 {
		t.Fatalf("应只点 Key 名称与详情的关闭按钮: %+v", report)
	}
	checkNoKeyLeak(t, strings.Join(result.Steps, "→"))
}

// TestFetchStudioKeyClientSampleInNode 主区域另有一段带完整公共 Key 的示例代码：公共 Key 不算页面上的 Key。空状态时照常创建
// （不判成认不出列表），有 Key 时照常读这一行的 Key
func TestFetchStudioKeyClientSampleInNode(t *testing.T) {
	result, err, report := runStudioKeyInNode(t, map[string]any{"scenario": "existing", "clientSample": true},
		func() { studioKeyTiming.createWait = 1500 * time.Millisecond })
	if err != nil || result.Key != fakeNewKey || !result.Created || report.KeyCreates != 1 {
		t.Fatalf("空状态应照常创建: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
	result, err, report = runStudioKeyInNode(t, map[string]any{"scenario": "table", "maskedList": true, "clientSample": true,
		"rows":    []map[string]string{{"name": "Gemini API Key", "key": "other", "project": "My Other Project"}},
		"details": map[string]string{"name": "My Other Project", "number": "345678901234"}})
	if err != nil || result.Key != fakeOtherKey || report.KeyCreates != 0 || !slices.Equal(report.Log, []string{"key-name-0", "details-close"}) {
		t.Fatalf("有 Key 时应读这一行的 Key: err=%v 结果=%+v 页面=%+v", err, redactResult(result), report)
	}
}

// stringValue 返回 node 回答里的字符串（不是字符串时为空）
func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
