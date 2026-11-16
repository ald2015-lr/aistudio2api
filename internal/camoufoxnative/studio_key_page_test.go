package camoufoxnative

import (
	"context"
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
//     建好项目后 API Keys 页才出现“Create API key”。defaultKey 为真时新项目自带一个 Key（回到 API Keys 页 300ms 后才在 Key 列表的
//     响应里出现）；shownKey 为真时回到 API Keys 页后页面上显示着 Key 的末几位、响应里却没有；
//   - externalEntry 为真时（配合 create-project）下拉框里没有“Create project”，创建 Key 的对话框里只有指向 Google Cloud 控制台的
//     站外新建项目链接（不该点）。
//
// 页面自己的“请求”只是排进 responses，由 Go 侧假浏览器在下一条命令时以 network.responseCompleted 事件发出、network.getData 回答正文。
// 真实指针点击模拟为：点击表达式里的 scrollIntoView 记下目标，input.performActions 时点它（__click）
const fakeStudioKeySite = `
const texts = {
  en: {create: 'Create API key', keyTitle: 'Create API key', name: 'Name your key', choose: 'Choose an imported project', cancel: 'Cancel',
    confirm: 'Create key', imp: 'Import project', none: 'No Cloud Projects Available', newProject: 'Create project',
    projectTitle: 'Create a new project', projectName: 'Project name', tos: 'I agree to the Google Cloud Terms of Service *',
    mail: 'Email me with product updates and offers', projectConfirm: 'Create project', created: 'API key created'},
  zh: {create: '创建 API 密钥', keyTitle: '创建 API 密钥', name: '为密钥命名', choose: '选择已导入的项目', cancel: '取消', confirm: '创建密钥',
    imp: '导入项目', none: '没有可用的 Cloud 项目', newProject: '创建项目', projectTitle: '创建新项目', projectName: '项目名称',
    tos: '我同意 Google Cloud 服务条款 *', mail: '通过电子邮件接收产品动态和优惠', projectConfirm: '创建', created: '已创建 API 密钥'},
  'zh-TW': {create: '建立 API 金鑰', keyTitle: '建立 API 金鑰', name: '為金鑰命名', choose: '選擇已匯入的專案', cancel: '取消', confirm: '建立金鑰',
    imp: '匯入專案', none: '沒有可用的 Cloud 專案', newProject: '建立專案', projectTitle: '建立新專案', projectName: '專案名稱',
    tos: '我同意 Google Cloud 服務條款 *', mail: '透過電子郵件接收產品更新', projectConfirm: '建立', created: '已建立 API 金鑰'},
  ja: {create: 'API キーを作成', keyTitle: 'API キーを作成', name: 'キーの名前', choose: 'インポートしたプロジェクトを選択', cancel: 'キャンセル',
    confirm: 'キーを作成', imp: 'プロジェクトをインポート', none: 'プロジェクトがありません', newProject: 'プロジェクトを作成',
    projectTitle: '新しいプロジェクトを作成', projectName: 'プロジェクト名', tos: 'Google Cloud 利用規約に同意します *', mail: 'メールで最新情報を受け取る',
    projectConfirm: '作成', created: 'API キーを作成しました'},
  ko: {create: 'API 키 만들기', keyTitle: 'API 키 만들기', name: '키 이름', choose: '가져온 프로젝트 선택', cancel: '취소', confirm: '키 만들기',
    imp: '프로젝트 가져오기', none: '프로젝트가 없습니다', newProject: '프로젝트 만들기', projectTitle: '새 프로젝트 만들기', projectName: '프로젝트 이름',
    tos: 'Google Cloud 서비스 약관에 동의합니다 *', mail: '이메일로 소식 받기', projectConfirm: '만들기', created: 'API 키가 생성됨'},
};
const T = texts[variant.lang || 'en'];
const log = [];
let responses = [];
let requestSeq = 0;
const state = {navigations: [], projectCreates: 0, keyCreates: 0, project: null, selected: null, pendingProject: 0, tos: null, mail: null,
  projectPane: null, listReadyAt: 0, listLoaded: variant.scenario !== 'slow-list', keysAt: 0};
if (variant.scenario === 'existing' || variant.scenario === 'slow-list') state.project = {number: '210987654321', name: 'Default Gemini Project', id: 'gen-lang-client-0987654321'};
Element.prototype.scrollIntoView = function () { globalThis.__target = this; };
const nameOf = element => element.getAttribute('data-name') || element.localName;
const respond = (url, body) => responses.push({id: 'req-' + (++requestSeq), url, body});
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
const rpc = name => 'https://alkalimakersuite-pa.clients6.google.com/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/' + name;
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
const createKey = () => {
  state.keyCreates++;
  const project = state.selected;
  respond(rpc('CreateApiKey'), ")]}'\n" + JSON.stringify([[['projects/' + project.number + '/locations/global/keys/k1', null, fakeKey,
    ['projects/' + project.number, project.name]]]]));
  replace(keyDialog, h('h2', {}, T.created), h('p', {}, fakeKey), h('button', {'data-name': 'copy'}, 'Copy'), h('button', {'data-name': 'done'}, 'Done'));
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
const apikeyPage = () => {
  let create = h('button', {'data-name': 'create-key'}, icon('key'), T.create);
  create.addEventListener('click', openKeyDialog);
  if (variant.scenario === 'no-create' && !state.project) {
    // 没有项目的空状态：只有新建项目的按钮（本流程不点它，改去 Projects 页）
    create = h('button', {'data-name': 'apikey-project'}, icon('add'), T.newProject);
    create.addEventListener('click', openProjectDialog);
  }
  const created = state.project && state.projectCreates > 0;
  const children = [h('h1', {}, 'API Keys'), h('p', {}, created && variant.shownKey ? 'Gemini API Key …WXYZ' : 'No API keys yet'), create];
  if (variant.banner) {
    children.unshift(h('div', {role: 'alert', 'data-name': 'banner'}, 'Your account must be verified before it can continue using the Gemini API.',
      h('a', {href: '/verify-account', 'data-name': 'verify-link'}, 'Verify')));
  }
  overlay = h('div', {class: 'cdk-overlay-container'});
  show(h('body', {}, h('ms-app', {}, h('main', {}, ...children)), overlay), '/apikey');
  if (created && variant.defaultKey) state.keysAt = Date.now() + 300;
  else respond(rpc('ListApiKeys'), ")]}'\n[]");
  if (state.project && variant.scenario !== 'slow-list') {
    respond(rpc('ListCloudProjects'), ")]}'\n" + JSON.stringify([[['projects/' + state.project.number, state.project.id, state.project.name]]]));
  }
};
globalThis.__navigate = url => {
  const path = new URL(url).pathname;
  state.navigations.push(path);
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
    // 新项目自带的 Key 晚一点才出现在 Key 列表的响应里
    state.keysAt = 0;
    respond(rpc('ListApiKeys'), ")]}'\n" + JSON.stringify([[['projects/' + state.project.number + '/locations/global/keys/k0', null, fakeKey,
      ['projects/' + state.project.number, state.project.name]]]]));
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
  tos: Boolean(state.tos && state.tos.checked), mail: Boolean(state.mail && state.mail.checked)});
`

// studioKeyNodeReport 为合成页面的记录
type studioKeyNodeReport struct {
	Log            []string
	Navigations    []string
	ProjectCreates int
	KeyCreates     int
	Tos            bool
	Mail           bool
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
	t.Cleanup(func() { studioKeyTiming = saved })
}

// runStudioKeyInNode 在合成页面上跑完整的获取流程（Go 侧轮询、读响应、真实指针点击、导航都经假浏览器转到 node）；
// tweaks 在调短时限之后再改个别时限
func runStudioKeyInNode(t *testing.T, variant map[string]any, tweaks ...func()) (StudioKeyResult, error, studioKeyNodeReport) {
	t.Helper()
	fastStudioKey(t)
	for _, tweak := range tweaks {
		tweak()
	}
	encodedVariant, _ := json.Marshal(variant)
	process := startOnboardingNode(t, "const variant = "+string(encodedVariant)+";\nconst fakeKey = "+jsString(fakeNewKey)+";\n"+fakeStudioKeySite)
	var mu sync.Mutex
	bodies := map[string]string{}
	drain := func() []map[string]any {
		answer := process.evaluate("__drain()")
		text, _ := answer.value.(string)
		var items []struct {
			ID   string `json:"id"`
			URL  string `json:"url"`
			Body string `json:"body"`
		}
		_ = json.Unmarshal([]byte(text), &items)
		var events []map[string]any
		mu.Lock()
		defer mu.Unlock()
		for _, item := range items {
			bodies[item.ID] = item.Body
			events = append(events, map[string]any{"type": "event", "method": "network.responseCompleted", "params": map[string]any{
				"context": "ctx-1", "request": map[string]any{"request": item.ID, "url": item.URL}, "response": map[string]any{"status": 200},
			}})
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
	result, err := session.fetchStudioKey(ctx)
	var report studioKeyNodeReport
	reported := process.evaluate("__report")
	if text, ok := reported.value.(string); !ok || json.Unmarshal([]byte(text), &report) != nil {
		t.Fatalf("读取页面记录失败: %+v", reported)
	}
	return result, err, report
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
