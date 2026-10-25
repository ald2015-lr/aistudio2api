package camoufoxnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// snapshotJSON 取出“ | 页面=”之后的快照 JSON 并解码，确认它是合法 JSON
func snapshotJSON(t *testing.T, message string) map[string]any {
	t.Helper()
	at := strings.LastIndex(message, " | 页面=")
	if at < 0 {
		t.Fatalf("错误末尾没有页面快照: %q", message)
	}
	text := message[at+len(" | 页面="):]
	if utf8.RuneCountInString(text) > pageSnapshotLimit {
		t.Fatalf("快照 %d 个字符，超过上限 %d: %s", utf8.RuneCountInString(text), pageSnapshotLimit, text)
	}
	if strings.Contains(text, "|") || strings.Contains(text, "\n") {
		t.Fatalf("快照里不应有“|”或换行（日志字段以“ | ”分隔）: %s", text)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("快照不是合法 JSON: %v: %s", err, text)
	}
	return decoded
}

// TestFormatPageSnapshotExample 覆盖完整快照：地址只留 host、path 与 model，邮箱打码，“|”换成“¦”，字段顺序固定；
// since 由 Go 侧填写、排在 age 之后（页面返回的同名字段不采用），没有导航记录时省略
func TestFormatPageSnapshotExample(t *testing.T) {
	raw := `{"href":"https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest&authuser=2&continue=ab123%40gmail.com#x",` +
		`"title":"Google AI Studio | Chat","ready":"complete","vis":"visible","age":93,"since":1,"nav":"reload","box":[1,1],"input":[1,0],"textareas":0,` +
		`"ta":["div@ms-chunk-editor: Ask lihai.bo@gmail.com"],"run":[1,1,0],"waa":false,"hook":false,"cap":true,` +
		`"overlays":["dialog: Signed in as lihai.bo@gmail.com Got it"],"alerts":["Failed to load | contact ab9@gmail.com"],` +
		`"ms":["ms-app","ms-prompt-box"],"body":"Welcome x@y.io"}`
	want := ` | 页面={"url":"aistudio.google.com/prompts/new_chat?model=gemini-flash-latest","title":"Google AI Studio ¦ Chat",` +
		`"ready":"complete","vis":"visible","age":93,"since":118,"nav":"reload","box":[1,1],"input":[1,0],"textareas":0,` +
		`"ta":["div@ms-chunk-editor: Ask li***@***"],"run":[1,1,0],"waa":false,"hook":false,"cap":true,` +
		`"overlays":["dialog: Signed in as li***@*** Got it"],"alerts":["Failed to load ¦ contact ab***@***"],` +
		`"ms":["ms-app","ms-prompt-box"],"body":"Welcome x***@***"}`
	since := int64(118)
	got := formatPageSnapshot(raw, nil, &since)
	if got != want {
		t.Fatalf("快照格式\n得到 %s\n期望 %s", got, want)
	}
	snapshotJSON(t, "前缀"+got)
	if decoded := snapshotJSON(t, formatPageSnapshot(raw, nil, nil)); decoded["since"] != nil {
		t.Fatalf("没有导航记录时不应写 since（也不采用页面返回的值）: %v", decoded)
	}
}

// TestFormatPageSnapshotTruncates 超长快照依次缩短后不超过上限、仍是合法 JSON，计数、地址与 waa 保留
func TestFormatPageSnapshotTruncates(t *testing.T) {
	long := strings.Repeat("正文很长 ", 100)
	overlays := make([]string, 6)
	for index := range overlays {
		overlays[index] = fmt.Sprintf("dialog: 弹层 %d %s", index, strings.Repeat("字", 170))
	}
	tags := make([]string, 20)
	for index := range tags {
		tags[index] = fmt.Sprintf("ms-component-with-a-long-name-%02d", index)
	}
	encoded, _ := json.Marshal(map[string]any{
		"href": "https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest", "title": strings.Repeat("标题", 80),
		"ready": "interactive", "vis": "hidden", "box": []int{2, 2}, "input": []int{2, 1}, "textareas": 3, "run": []int{1, 0, 0},
		"waa": true, "overlays": overlays, "ms": tags, "body": long, "err": []string{"body: x", "ms: y"},
	})
	got := formatPageSnapshot(string(encoded), nil, nil)
	decoded := snapshotJSON(t, got)
	if decoded["url"] != "aistudio.google.com/prompts/new_chat?model=gemini-flash-latest" || decoded["waa"] != true {
		t.Fatalf("缩短后丢了地址或 waa: %v", decoded)
	}
	for _, key := range []string{"box", "input", "textareas", "run", "ready", "vis"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("缩短后丢了 %s: %v", key, decoded)
		}
	}
	if list, _ := decoded["overlays"].([]any); len(list) == 0 {
		t.Fatalf("缩短后至少保留第一条弹层文字: %v", decoded)
	}

	// 缩短步骤用完仍超长时按字符硬截断（只会出现在页面返回意外的超长值时）
	huge := pageSnapshot{URL: strings.Repeat("a", 2000)}
	if text := huge.encode(); utf8.RuneCountInString(text) != pageSnapshotLimit || !strings.HasSuffix(text, "…") {
		t.Fatalf("硬截断后应正好 %d 个字符并以…结尾，得到 %d", pageSnapshotLimit, utf8.RuneCountInString(text))
	}
}

// TestFormatPageSnapshotReductionOrder 超长时先缩短、清空正文，再缩短组件标签，标签只保留前面的优先段（输入框、Run 按钮与弹层所在的组件）
func TestFormatPageSnapshotReductionOrder(t *testing.T) {
	tags := make([]string, 20)
	for index := range tags {
		tags[index] = fmt.Sprintf("ms-component-name-%02d", index)
	}
	tags[0], tags[1] = "ms-prompt-input-wrapper", "ms-run-button"
	overlay := "dialog: " + strings.Repeat("弹", 150)
	alerts := []string{"Failed to load models " + strings.Repeat("a", 90), "Draft saved " + strings.Repeat("b", 90)}
	encoded, _ := json.Marshal(map[string]any{
		"href": "https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest", "title": "Google AI Studio",
		"ready": "complete", "vis": "visible", "age": 15, "nav": "navigate", "box": []int{1, 1}, "input": []int{1, 0}, "textareas": 0,
		"run": []int{1, 1, 0}, "waa": false, "hook": true, "cap": true, "overlays": []string{overlay}, "alerts": alerts, "ms": tags,
		"body": strings.Repeat("z", 200),
	})
	since := int64(112)
	decoded := snapshotJSON(t, formatPageSnapshot(string(encoded), nil, &since))
	if _, ok := decoded["body"]; ok {
		t.Fatalf("缩短组件标签之前应先清空正文: %v", decoded)
	}
	// 提示条在正文之后、组件标签之前缩短为 1 条、60 字
	if list, _ := decoded["alerts"].([]any); len(list) != 1 || list[0] != truncateRunes(alerts[0], 60) {
		t.Fatalf("清空正文之后应先缩短提示条: %v", decoded["alerts"])
	}
	if decoded["age"] != float64(15) || decoded["since"] != float64(112) || decoded["nav"] != "navigate" {
		t.Fatalf("缩短后应保留 age、since、nav: %v", decoded)
	}
	list, _ := decoded["ms"].([]any)
	if len(list) != 10 || list[0] != "ms-prompt-input-wrapper" || list[1] != "ms-run-button" {
		t.Fatalf("组件标签应保留前 10 个（优先段在前）: %v", list)
	}
	if overlays, _ := decoded["overlays"].([]any); len(overlays) != 1 || overlays[0] != overlay {
		t.Fatalf("清空正文、缩短标签已够时弹层文字应保持完整: %v", decoded["overlays"])
	}
	if decoded["hook"] != true || decoded["cap"] != true || decoded["waa"] != false {
		t.Fatalf("缩短后应保留 waa、hook、cap: %v", decoded)
	}
}

// TestFormatPageSnapshotFailure 快照本身失败时写“快照失败: <简短原因>”：单行、无“|”、邮箱打码、截短
func TestFormatPageSnapshotFailure(t *testing.T) {
	cause := fmt.Errorf("BiDi 连接已断开: websocket: close 1006\n(abnormal | closure) ab9@gmail.com %s", strings.Repeat("x", 300))
	got := formatPageSnapshot("", cause, nil)
	if !strings.HasPrefix(got, " | 页面=快照失败: BiDi 连接已断开: websocket: close 1006 (abnormal ¦ closure) ab***@***") {
		t.Fatalf("快照失败文本 = %q", got)
	}
	if strings.Contains(got, "\n") || strings.Count(got, "|") != 1 {
		t.Fatalf("快照失败文本只能有开头的一个“|”且不换行: %q", got)
	}
	reason := strings.TrimPrefix(got, " | 页面=快照失败: ")
	if utf8.RuneCountInString(reason) != pageSnapshotReasonLimit || !strings.HasSuffix(reason, "…") {
		t.Fatalf("原因应截到 %d 个字符，得到 %d", pageSnapshotReasonLimit, utf8.RuneCountInString(reason))
	}
	if got := formatPageSnapshot("undefined", nil, nil); got != " | 页面=快照失败: 返回值不是 JSON" {
		t.Fatalf("非 JSON 返回值 = %q", got)
	}
	// 个别字段类型不符时照常输出其余字段，并记一条 err
	partial := formatPageSnapshot(`{"title":"AI Studio","box":"bad","waa":true}`, nil, nil)
	decoded := snapshotJSON(t, partial)
	if decoded["title"] != "AI Studio" || decoded["waa"] != true || !strings.Contains(partial, `"err":["decode: box"]`) {
		t.Fatalf("字段类型不符时的快照 = %s", partial)
	}
}

// TestSnapshotEmailMasking 邮箱打码保留前两位 + ***@***，与 tools/diag_*.sh 的打码一致
func TestSnapshotEmailMasking(t *testing.T) {
	cases := map[string]string{
		"lisa.example2020@gmail.com":            "li***@***",
		"a@b.co":                                "a***@***",
		"账号ab.cd+x@corp.example.com的":           "账号ab***@***的",
		"两个 one@x.org 与 two@y.net":              "两个 on***@*** 与 tw***@***",
		"没有邮箱 user@localhost":                   "没有邮箱 user@localhost",
		"已打码 li***@***":                         "已打码 li***@***",
		"https://x.com/u/ab123%40gmail.com?a=1": "https://x.com/u/ab123%40gmail.com?a=1",
		// 页面里截短后以“…”结尾、截断点落在域名里的残缺邮箱同样打码；已打码后再截短的不再变化
		"Signed in as abcdef@gma…": "Signed in as ab***@***…",
		"x someone.name@gmail.c…":  "x so***@***…",
		"末尾 lisa@…":                "末尾 li***@***…",
		"已打码 li***@*…":             "已打码 li***@*…",
		"没截断 user@localhost 后面还有":  "没截断 user@localhost 后面还有",
	}
	for input, want := range cases {
		if got := snapshotText(input, 200); got != want {
			t.Fatalf("snapshotText(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestPageSnapshotURL 地址只保留 host、path 与 model 参数
func TestPageSnapshotURL(t *testing.T) {
	cases := map[string]string{
		"https://aistudio.google.com/prompts/new_chat?model=gemini-flash-latest&temporary=true": "aistudio.google.com/prompts/new_chat?model=gemini-flash-latest",
		"https://aistudio.google.com/prompts/new_chat?model=models%2Fgemini-2.5-pro":            "aistudio.google.com/prompts/new_chat?model=models/gemini-2.5-pro",
		"https://accounts.google.com/v3/signin/identifier?continue=https%3A%2F%2Fx&Email=a%40b": "accounts.google.com/v3/signin/identifier",
		"https://aistudio.google.com/app/prompts/abc#frag":                                      "aistudio.google.com/app/prompts/abc",
		"about:blank":  "about:blank",
		"":             "",
		"%zz?secret=1": "%zz",
	}
	for input, want := range cases {
		if got := pageSnapshotURL(input); got != want {
			t.Fatalf("pageSnapshotURL(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestStripPageSnapshots 按文案判断错误时去掉每行的页面快照（连同同一行接在后面的说明），其他行不变
func TestStripPageSnapshots(t *testing.T) {
	cases := map[string]string{
		`官网 prompt 状态未同步 | 页面={"body":"The input token count exceeds"}`:                "官网 prompt 状态未同步",
		"官网 WAA service 未暴露: x | 页面=快照失败: EOF；本机代理中转：上游 407":                           "官网 WAA service 未暴露: x",
		"第一行 | 页面={\"a\":1}\nAI Studio GenerateContent 返回 HTTP 429: input token count": "第一行\nAI Studio GenerateContent 返回 HTTP 429: input token count",
		"没有快照 | 端口=10001": "没有快照 | 端口=10001",
	}
	for input, want := range cases {
		if got := StripPageSnapshots(input); got != want {
			t.Fatalf("StripPageSnapshots(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestWithPageSnapshotKeepsPrefix 追加快照不改变原错误的文字前缀与错误链；调用方已取消时不追加
func TestWithPageSnapshotKeepsPrefix(t *testing.T) {
	page := newFakeBiDiPage(t, nil)
	cause := fmt.Errorf("官网 WAA service 未暴露: %w", context.DeadlineExceeded)
	expired, cancel := context.WithTimeout(context.Background(), 0)
	defer cancel()
	<-expired.Done()
	wrapped := page.client.withPageSnapshot(expired, "ctx-1", cause)
	if !strings.HasPrefix(wrapped.Error(), cause.Error()+" | 页面={") {
		t.Fatalf("原错误前缀应逐字不变: %q", wrapped.Error())
	}
	if !errors.Is(wrapped, context.DeadlineExceeded) || !errors.Is(wrapped, cause) {
		t.Fatal("追加快照后错误链应保留")
	}
	if decoded := snapshotJSON(t, wrapped.Error()); decoded["since"] != nil {
		t.Fatalf("没有导航 AI Studio 时快照不应写 since: %v", decoded)
	}
	if page.snapshots.Load() != 1 {
		t.Fatalf("应采集 1 次快照，得到 %d", page.snapshots.Load())
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if got := page.client.withPageSnapshot(canceled, "ctx-1", cause); got != cause {
		t.Fatalf("调用方已取消时应原样返回，得到 %v", got)
	}
	if page.snapshots.Load() != 1 {
		t.Fatalf("调用方已取消时不应采集快照，共 %d 次", page.snapshots.Load())
	}
	if got := page.client.withPageSnapshot(context.Background(), "ctx-1", nil); got != nil {
		t.Fatalf("没有错误时应返回 nil，得到 %v", got)
	}
}

// TestPageSnapshotExpressionReadOnly 快照表达式只读：不点击、不写值、不派发事件、不导航
func TestPageSnapshotExpressionReadOnly(t *testing.T) {
	for _, forbidden := range []string{".click(", "dispatchEvent", ".value =", "setter", "location.assign", "location.reload",
		"location.href =", "history.", "localStorage", "removeChild", ".remove(", "innerHTML =", ".focus(",
		"__aistudioRestoreBootstrapCapture(", "__aistudioRestoreBootstrapCapture?.(", "setAttribute"} {
		if strings.Contains(pageSnapshotExpression, forbidden) {
			t.Fatalf("快照表达式不应包含 %q", forbidden)
		}
	}
	for _, selector := range []string{"'ms-prompt-box'", "'ms-prompt-box textarea'", "'textarea'", "'ms-run-button button'",
		"'[role=dialog]'", "'[role=alertdialog]'", "'mat-dialog-container'", "'.cdk-overlay-container'", "window.__aistudioWaaService",
		"window.__aistudioWaaSnapshotKey", "typeof window.__aistudioRestoreBootstrapCapture", "getEntriesByType('navigation')", "[role=alert]"} {
		if !strings.Contains(pageSnapshotExpression, selector) {
			t.Fatalf("快照表达式缺少 %s", selector)
		}
	}
}

// fakeSnapshotDOM 为 node 里执行快照表达式的最小 DOM 替身：ms-app 里有一个不可见 textarea 的输入框、一个可见的 contenteditable
// 编辑区与一个禁用的 Run 按钮，弹层里有一个 ms-g1-welcome-dialog，另有一个只含 iframe 的空弹层；标题与正文的邮箱跨过页面里的截断点。
// 主区域有几条提示条（带邮箱、重复、不可见、没有文字），弹层里也有一条（已记在弹层里，不再记进 alerts）。
// 第二次执行时去掉 location、让 textarea 的可见判断抛错、去掉 ms-run-button 与对话框，检查单项失败不影响其他项、
// 没有 ms-run-button 时按文字为 Run 的按钮找所在组件，以及空的 .cdk-overlay-container 只在有遮罩时记一条
const fakeSnapshotDOM = `
const fs = require('fs');
const expression = fs.readFileSync(process.argv[2], 'utf8');
const all = [];
const node = (localName, options = {}, children = []) => {
  const item = {
    localName,
    innerText: options.text || '',
    parentElement: null,
    children,
    checkVisibility: () => { if (options.broken) throw new Error('broken'); return options.visible !== false; },
    matches: selector => selector === ':disabled' ? Boolean(options.disabled) : false,
    closest: () => null,
    getAttribute: name => (options.attributes || {})[name] ?? null,
    querySelectorAll: selector => {
      const found = [];
      const walk = parent => {
        for (const child of parent.children) {
          if (selector === '*' || selector === child.localName || (selector.startsWith('.') && (child.options.classes || []).includes(selector.slice(1)))) found.push(child);
          walk(child);
        }
      };
      walk(item);
      return found;
    },
    querySelector: selector => item.querySelectorAll(selector)[0] || null,
    options,
  };
  for (const child of children) child.parentElement = item;
  return item;
};
const alerts = [
  node('div', {text: ' Failed to load\n models. Contact ab12@gmail.com '}),
  node('div', {text: 'Failed to load models. Contact ab12@gmail.com'}),
  node('div', {visible: false, text: 'hidden alert'}),
  node('div'),
  node('mat-error', {text: 'Draft saved'}),
  node('div', {text: 'Third one'}),
];
const textarea = node('textarea', {visible: false});
const editor = node('div', {attributes: {contenteditable: 'true', 'aria-label': 'Start typing a prompt'}});
const box = node('ms-prompt-box', {}, [node('ms-autosize-textarea', {}, [textarea])]);
const chunk = node('ms-chunk-editor', {}, [editor]);
const runButton = node('button', {disabled: true, text: 'Run'});
const runHost = node('ms-run-button', {}, [runButton]);
const navItem = node('ms-nav-item');
const app = node('ms-app', {}, [node('ms-navbar', {}, [navItem]), node('ms-prompt-input-wrapper', {}, [box, chunk]), runHost, node('ms-hidden', {visible: false}), ...alerts]);
const dialogAlert = node('div', {text: 'Alert in dialog'});
const welcome = node('ms-g1-welcome-dialog', {}, [node('button', {text: 'Close'}), dialogAlert]);
const dialog = node('mat-dialog-container', {text: '  Welcome\n to   AI Studio '}, [welcome]);
const frameDialog = node('div', {}, [node('iframe')]);
const overlay = node('div', {text: 'Welcome to AI Studio'}, [dialog]);
const body = node('body', {text: 'x'.repeat(170) + ' Signed in as someone.name@gmail.com'}, [app, overlay, frameDialog]);
const html = node('html', {}, [body]);
const collect = item => { all.push(item); item.children.forEach(collect); };
collect(html);
const sendHost = node('ms-send-button', {}, [node('button', {attributes: {'aria-label': 'Run prompt'}})]);
const selectors = {
  'ms-prompt-box': [box], 'ms-prompt-box textarea': [textarea], 'textarea': [textarea],
  'textarea, [contenteditable]:not([contenteditable="false"]), [role=textbox]': [textarea, editor],
  'ms-run-button button': [runButton], 'button': [runButton, welcome.children[0]],
  '[role=dialog]': [dialog, frameDialog], '[role=alertdialog]': [], 'mat-dialog-container': [dialog], '.cdk-overlay-container': [overlay], '*': all,
  '[role=alert], [role=status], [aria-live=assertive], [aria-live=polite], mat-error': [dialogAlert, ...alerts],
};
globalThis.document = {
  title: 'y'.repeat(110) + ' abcdef@gmail.com', readyState: 'complete', visibilityState: 'visible', body,
  querySelectorAll: selector => { if (!(selector in selectors)) throw new Error('unexpected selector ' + selector); return selectors[selector]; },
};
globalThis.location = {href: 'https://aistudio.google.com/prompts/new_chat?model=m'};
Object.defineProperty(globalThis, 'performance', {configurable: true, value: {now: () => 93400, getEntriesByType: type => type === 'navigation' ? [{type: 'reload'}] : []}});
globalThis.window = globalThis;
window.__aistudioWaaSnapshotKey = 'snapshotKey';
window.__aistudioRestoreBootstrapCapture = () => { throw new Error('快照不应调用请求捕获的还原函数'); };
const first = eval(expression);
selectors['textarea'] = [node('textarea', {broken: true})];
selectors['ms-run-button button'] = [];
selectors['button'] = [welcome.children[0], sendHost.children[0]];
selectors['[role=dialog]'] = [];
selectors['mat-dialog-container'] = [];
selectors['.cdk-overlay-container'] = [node('div'), node('div', {}, [node('div', {classes: ['cdk-overlay-backdrop-showing']})])];
delete globalThis.location;
delete window.__aistudioWaaSnapshotKey;
delete window.__aistudioRestoreBootstrapCapture;
console.log(JSON.stringify([JSON.parse(first), JSON.parse(eval(expression))]));
`

// TestPageSnapshotExpressionInNode 在 node 里用最小 DOM 替身执行快照表达式（机器上没有 node 时跳过）：语法正确、各项计数正确、
// 页面文字先打码再截短、组件标签按优先级收集、空弹层也记一条、单项失败只记入 err
func TestPageSnapshotExpressionInNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("没有 node，跳过快照表达式的执行检查")
	}
	directory := t.TempDir()
	expressionPath := filepath.Join(directory, "expression.js")
	scriptPath := filepath.Join(directory, "dom.js")
	if err := os.WriteFile(expressionPath, []byte(pageSnapshotExpression), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scriptPath, []byte(fakeSnapshotDOM), 0o600); err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command(node, scriptPath, expressionPath).CombinedOutput()
	if err != nil {
		t.Fatalf("node 执行快照表达式失败: %v\n%s", err, output)
	}
	var results []map[string]any
	if err := json.Unmarshal(output, &results); err != nil || len(results) != 2 {
		t.Fatalf("node 输出无法解析: %v\n%s", err, output)
	}
	first := results[0]
	// 页面里先打码再截短：邮箱跨过截断点（标题 120、正文 200）时本地部分也不会留下
	title, _ := first["title"].(string)
	bodyText, _ := first["body"].(string)
	if title != strings.Repeat("y", 110)+" ab***@***" || bodyText != strings.Repeat("x", 170)+" Signed in as so***@***" {
		t.Fatalf("页面里应先打码再截短: title=%q body=%q", title, bodyText)
	}
	formatted := formatPageSnapshot(mustJSON(t, first), nil, nil)
	for _, leaked := range []string{"abcdef", "someone", "gmail"} {
		if strings.Contains(formatted, leaked) {
			t.Fatalf("快照里不应出现邮箱的任何部分 %q: %s", leaked, formatted)
		}
	}
	snapshotJSON(t, formatted)
	want := map[string]string{
		"age": "93", "nav": `"reload"`, "box": "[1,1]", "input": "[1,0]", "textareas": "0", "run": "[1,1,0]",
		"waa": "false", "hook": "true", "cap": "true",
		"ta":       `["div@ms-chunk-editor: Start typing a prompt"]`,
		"overlays": `["dialog: Welcome to AI Studio","dialog: (无文字, iframe)"]`,
		// 弹层里的提示条已记在弹层里；主区域的去掉多余空白、打码、去重，不可见与没有文字的跳过，最多 2 条
		"alerts": `["Failed to load models. Contact ab***@***","Draft saved"]`,
		// 编辑区与 Run 按钮所在的组件、弹层里的组件在前，再按文档顺序补足（不可见的 ms-hidden 不算）
		"ms": `["ms-chunk-editor","ms-prompt-input-wrapper","ms-app","ms-run-button","ms-g1-welcome-dialog","ms-navbar","ms-nav-item","ms-prompt-box","ms-autosize-textarea"]`,
	}
	for key, value := range want {
		if got := mustJSON(t, first[key]); got != value {
			t.Fatalf("node 快照 %s = %s，期望 %s（全部：%s）", key, got, value, output)
		}
	}
	second := results[1]
	if _, ok := second["href"]; ok {
		t.Fatalf("location 不可用时不应有 href: %v", second)
	}
	if _, ok := second["textareas"]; ok {
		t.Fatalf("textarea 可见判断抛错时不应有 textareas: %v", second)
	}
	errorsText := fmt.Sprint(second["err"])
	if !strings.Contains(errorsText, "href: ") || !strings.Contains(errorsText, "textareas: broken") || second["title"] != title {
		t.Fatalf("单项失败应只记入 err、不影响其他项: %v", second)
	}
	if second["hook"] != false || second["cap"] != false || mustJSON(t, second["run"]) != "[0,0,0]" {
		t.Fatalf("整页重载后 hook、cap 应为 false，没有 ms-run-button 时 run 为 [0,0,0]: %v", second)
	}
	if tags := mustJSON(t, second["ms"]); !strings.HasPrefix(tags, `["ms-chunk-editor","ms-prompt-input-wrapper","ms-app","ms-send-button",`) {
		t.Fatalf("没有 ms-run-button 时应按文字为 Run 的按钮找所在组件: %s", tags)
	}
	if overlays := mustJSON(t, second["overlays"]); overlays != `["cdk-overlay: (遮罩)"]` {
		t.Fatalf("空的 .cdk-overlay-container 只在有遮罩时记一条: %s", overlays)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
