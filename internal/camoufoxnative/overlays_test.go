package camoufoxnative

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

// fakeOverlaysDOM 在 fakeOnboardingDOMLib 上逐个场景执行 dismissOverlaysExpression：每个场景搭一页，记下返回值、是否抛错与被点击的元素
const fakeOverlaysDOM = `
const fs = require('fs');
const expression = fs.readFileSync(process.argv[2], 'utf8');
` + fakeOnboardingDOMLib + `
const results = {};
const scenario = (name, body) => {
  mount(body, '/prompts/new_chat');
  let value = null, error = '';
  try { value = (0, eval)(expression); } catch (failure) { error = String(failure); }
  results[name] = {value, error, clicks: clicks.slice()};
};
const closeIcon = () => h('mat-icon', {class: 'material-symbols-outlined'}, 'close');
const prompt = () => h('ms-prompt-box', {}, h('textarea', {}));

// 中文界面：欢迎对话框里有关闭按钮与“开始使用”，引导浮层的退出按钮只有中文 aria-label，Cookie 告知条只有确认按钮
scenario('zh', h('body', {},
  prompt(),
  h('ms-g1-welcome-dialog', {},
    h('button', {'aria-label': '关闭对话框', 'data-name': 'welcome-close'}, closeIcon()),
    h('h2', {}, '欢迎使用新版 AI Studio'),
    h('button', {class: 'ms-button-primary', 'data-name': 'welcome-start'}, '开始使用')),
  h('div', {class: 'xap-tour'}, h('button', {class: 'xap-tour-exit-button', 'aria-label': '退出导览', 'data-name': 'tour-exit'}, closeIcon())),
  h('div', {id: 'glue-cookie-notification-bar-1', class: 'glue-cookie-notification-bar'},
    h('div', {id: 'glue-cookie-notification-bar-1-text'}, '我们使用 Cookie',
      h('button', {class: 'glue-cookie-notification-bar__accept', 'data-name': 'cookie-accept'}, '知道了'))),
  h('button', {class: 'glue-cookie-notification-bar__accept', 'data-name': 'stray-accept'}, '确定')));

// 英文界面（原来的写法）：关闭按钮带 aria-label="Close dialog"，引导浮层 aria-label="Close guided tour"，告知条有拒绝按钮时只点拒绝
scenario('en', h('body', {},
  prompt(),
  h('ms-g1-welcome-dialog', {},
    h('button', {class: 'ms-button-primary', 'data-name': 'welcome-start'}, 'Get started'),
    h('button', {'aria-label': 'Close dialog', 'data-name': 'welcome-close'}, closeIcon())),
  h('button', {'aria-label': 'Close guided tour', 'data-name': 'tour-close'}),
  h('div', {id: 'glue-cookie-notification-bar-1', class: 'glue-cookie-notification-bar'},
    h('button', {class: 'glue-cookie-notification-bar__accept', 'data-name': 'cookie-accept'}, 'OK, got it'),
    h('button', {class: 'glue-cookie-notification-bar__reject', 'data-name': 'cookie-reject'}, 'No thanks'))));

// 认不出关闭按钮、对话框里有多个可点按钮：不猜、不抛错（上游的 uniqueVisible 在这里抛错，Worker 启动随之失败）
scenario('ambiguous', h('body', {},
  h('ms-g1-welcome-dialog', {},
    h('button', {'data-name': 'learn'}, '了解详情'),
    h('button', {class: 'ms-button-primary', 'data-name': 'start'}, '开始使用'))));

// 认不出关闭按钮、只有一个可见可点的按钮：点它；隐藏的按钮不算
scenario('single', h('body', {},
  h('ms-g1-welcome-dialog', {},
    h('button', {hidden: '', 'data-name': 'hidden'}, '稍后'),
    h('button', {class: 'ms-button-primary', 'data-name': 'start'}, '开始使用'))));

// 关闭按钮禁用、告知条的拒绝按钮被隐藏：都不点（有拒绝按钮的条不改点确认）
scenario('disabled', h('body', {},
  h('ms-g1-welcome-dialog', {},
    h('button', {disabled: '', 'aria-label': 'Close dialog', 'data-name': 'disabled-close'}, closeIcon()),
    h('button', {'data-name': 'one'}, '了解详情'),
    h('button', {'data-name': 'two'}, '开始使用')),
  h('div', {class: 'glue-cookie-notification-bar'},
    h('button', {class: 'glue-cookie-notification-bar__reject', style: 'display: none', 'data-name': 'hidden-reject'}, '拒绝'),
    h('button', {class: 'glue-cookie-notification-bar__accept', 'data-name': 'cookie-accept'}, '接受'))));

// 判断可见性时页面抛错：跳过这个按钮，表达式本身不抛错
const broken = h('button', {'aria-label': '关闭', 'data-name': 'broken'}, closeIcon());
broken.checkVisibility = () => { throw new Error('页面异常'); };
scenario('throwing', h('body', {}, h('ms-g1-welcome-dialog', {}, broken)));

// 没有任何弹层
scenario('none', h('body', {}, prompt()));
console.log(JSON.stringify(results));
`

// TestDismissOverlaysExpressionInNode 在 node 里用最小 DOM 替身执行 dismissOverlaysExpression（机器上没有 node 时跳过）：
// 中文界面也能关掉欢迎对话框、引导浮层与 Cookie 告知条；欢迎对话框只点第一个可见可点的关闭按钮，认不出关闭按钮时只在唯一一个
// 可点按钮时点它；告知条有拒绝按钮时点拒绝、没有时点确认，条外的确认按钮不点；任何情况下都不抛错
func TestDismissOverlaysExpressionInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node，跳过启动弹层表达式的执行检查")
	}
	directory := t.TempDir()
	expressionPath := filepath.Join(directory, "expression.js")
	scriptPath := filepath.Join(directory, "dom.js")
	if err := os.WriteFile(expressionPath, []byte(dismissOverlaysExpression), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(fakeOverlaysDOM), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, scriptPath, expressionPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行启动弹层表达式失败: %v\n%s", err, output)
	}
	var results map[string]struct {
		Value  int
		Error  string
		Clicks []string
	}
	if err := json.Unmarshal(output, &results); err != nil {
		t.Fatalf("node 输出无法解析: %v\n%s", err, output)
	}
	cases := []struct {
		name   string
		clicks []string
	}{
		{name: "zh", clicks: []string{"welcome-close", "tour-exit", "cookie-accept"}},
		{name: "en", clicks: []string{"welcome-close", "tour-close", "cookie-reject"}},
		{name: "ambiguous", clicks: []string{}},
		{name: "single", clicks: []string{"start"}},
		{name: "disabled", clicks: []string{}},
		{name: "throwing", clicks: []string{}},
		{name: "none", clicks: []string{}},
	}
	for _, item := range cases {
		result, ok := results[item.name]
		if !ok {
			t.Fatalf("缺少场景 %s：%s", item.name, output)
		}
		if result.Error != "" {
			t.Fatalf("场景 %s：表达式不应抛错（会让 Worker 启动失败），得到 %s", item.name, result.Error)
		}
		if !slices.Equal(result.Clicks, item.clicks) || result.Value != len(item.clicks) {
			t.Fatalf("场景 %s：点击 %v（返回 %d），期望 %v", item.name, result.Clicks, result.Value, item.clicks)
		}
	}
}
