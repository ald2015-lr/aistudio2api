package camoufoxnative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// pageSnapshotTimeout 为失败处一次页面诊断（地址与快照）的上限。启动时上下文从 context.Background 派生：
	// 失败处的 ctx 可能已经过期（INIT_TIMEOUT 到期），快照不能跟着失败；浏览器卡住时最多多等这么久
	pageSnapshotTimeout = 4 * time.Second
	// pageSnapshotRequestMargin 为运行中请求的快照在请求期限之前留出的余量（见 withRequestPageSnapshot）
	pageSnapshotRequestMargin = 200 * time.Millisecond
	// pageSnapshotMinimum 为运行中请求采集快照至少要剩下的时间，不足时不采集
	pageSnapshotMinimum = 500 * time.Millisecond
	// pageSnapshotLimit 为快照 JSON 的长度上限（按字符计），超出时按 pageSnapshotReductions 依次缩短
	pageSnapshotLimit = 800
	// pageSnapshotReasonLimit 为“快照失败: <原因>”里原因的长度上限（按字符计）
	pageSnapshotReasonLimit = 120
)

// PageSnapshotMarker 为页面类失败的错误里页面快照的开头：原错误之后接“ | 页面={…}”或“ | 页面=快照失败: <原因>”，快照本身只占一行、不含“ | ”
const PageSnapshotMarker = " | 页面="

// StripPageSnapshots 去掉文本每一行里从 PageSnapshotMarker 或首次使用页的进度记录（OnboardingProgressMarker，接在快照之前）
// 起到行尾的部分（页面快照、进度记录，以及外层接在它们后面的说明）。快照与进度记录里是页面上的文字（标题、弹层、正文、按钮与勾选框标签），
// 按文案判断错误类型时先去掉，免得页面文字被当成上游错误
func StripPageSnapshots(text string) string {
	if !strings.Contains(text, PageSnapshotMarker) && !strings.Contains(text, OnboardingProgressMarker) {
		return text
	}
	lines := strings.Split(text, "\n")
	for index, line := range lines {
		for _, marker := range []string{OnboardingProgressMarker, PageSnapshotMarker} {
			if before, _, found := strings.Cut(line, marker); found {
				line = before
			}
		}
		lines[index] = line
	}
	return strings.Join(lines, "\n")
}

// pageSnapshotExpression 只读采集页面当前状态，供页面类失败在原错误末尾追加“ | 页面=…”：
// 只读 DOM 与 window 属性，不点击、不写值、不派发事件、不导航；可见与启用的判断与就绪检查共用 pageDOMHelpers。
// 每项单独 try/catch，单项失败记入 err，不影响其他项。页面文字先打码再截短（截断点落在邮箱中间时残缺的地址认不出来），
// 截短处以“…”结尾，返回 JSON 字符串；地址缩短、再次打码与总长度控制在 Go 侧完成（见 formatPageSnapshot）
const pageSnapshotExpression = `(() => {` + pageDOMHelpers + `
  const out = {};
  const errors = [];
  const clean = value => String(value ?? '').replace(/\s+/g, ' ').trim();
  const mask = text => text.replace(/([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}/g, '$1***@***');
  const short = (value, limit) => {
    const text = mask(clean(value));
    return text.length > limit ? text.slice(0, limit - 1) + '…' : text;
  };
  const attempt = (name, collect) => {
    try {
      collect();
    } catch (error) {
      errors.push(name + ': ' + short(error && error.message ? error.message : error, 60));
    }
  };
  const count = selector => {
    const items = [...document.querySelectorAll(selector)];
    return [items.length, items.filter(visible).length];
  };
  // msAncestors 返回元素往上最近的几个 ms- 祖先标签名（由近到远）
  const msAncestors = (element, limit) => {
    const names = [];
    for (let node = element.parentElement, depth = 0; node && depth < 30 && names.length < limit; node = node.parentElement, depth++) {
      if (node.localName.startsWith('ms-')) names.push(node.localName);
    }
    return names;
  };
  // ms 按优先级收集：输入框与 Run 按钮所在的组件、弹层里的组件最能说明改版或被什么挡住，排在前面，最后按文档顺序补足 20 个
  const tags = new Set();
  const addTags = names => {
    for (const name of names) if (tags.size < 20) tags.add(name);
  };
  const overlayElements = [];
  attempt('href', () => { out.href = String(location.href); });
  attempt('title', () => { out.title = short(document.title, 120); });
  attempt('ready', () => { out.ready = String(document.readyState); });
  attempt('vis', () => { out.vis = String(document.visibilityState); });
  attempt('age', () => { out.age = Math.round(performance.now() / 1000); });
  attempt('nav', () => {
    const entry = performance.getEntriesByType('navigation')[0];
    if (entry) out.nav = String(entry.type);
  });
  attempt('box', () => { out.box = count('ms-prompt-box'); });
  attempt('input', () => { out.input = count('ms-prompt-box textarea'); });
  attempt('textareas', () => { out.textareas = [...document.querySelectorAll('textarea')].filter(visible).length; });
  attempt('ta', () => {
    const editors = [...document.querySelectorAll('textarea, [contenteditable]:not([contenteditable="false"]), [role=textbox]')].filter(visible);
    out.ta = editors.slice(0, 3).map(element => {
      const owners = msAncestors(element, 3);
      addTags(owners);
      const hint = short(element.getAttribute('placeholder') || element.getAttribute('aria-label') || element.getAttribute('data-placeholder'), 40);
      return element.localName + '@' + (owners[0] || '-') + (hint ? ': ' + hint : '');
    });
  });
  attempt('run', () => {
    const buttons = [...document.querySelectorAll('ms-run-button button')];
    const shown = buttons.filter(visible);
    out.run = [buttons.length, shown.length, shown.filter(buttonEnabled).length];
    // 没有 ms-run-button 时找文字或 aria-label 以 Run 开头的可见按钮，记下它所在的组件
    const labelledRun = button => /^run\b/i.test(clean(button.getAttribute('aria-label'))) || /^run\b/i.test(clean(button.innerText));
    const runs = shown.length ? shown : [...document.querySelectorAll('button')].filter(visible).filter(labelledRun);
    for (const button of runs.slice(0, 2)) addTags(msAncestors(button, 3));
  });
  attempt('waa', () => { out.waa = Boolean(window.__aistudioWaaService); });
  attempt('hook', () => { out.hook = Boolean(window.__aistudioWaaSnapshotKey); });
  attempt('cap', () => { out.cap = typeof window.__aistudioRestoreBootstrapCapture === 'function'; });
  attempt('overlays', () => {
    const elements = new Set();
    const texts = new Set();
    const overlays = [];
    const kinds = [
      ['dialog', '[role=dialog]'],
      ['alertdialog', '[role=alertdialog]'],
      ['mat-dialog', 'mat-dialog-container'],
      ['cdk-overlay', '.cdk-overlay-container']
    ];
    for (const [label, selector] of kinds) {
      for (const element of document.querySelectorAll(selector)) {
        if (overlays.length >= 6 || elements.has(element) || !visible(element)) continue;
        elements.add(element);
        let text = short(element.innerText, 160);
        if (!text) {
          // 没有文字的弹层（只有 iframe、遮罩或加载圈）也记一条，免得读成没有弹层；.cdk-overlay-container 常驻且平时为空，只在有遮罩时记
          if (label !== 'cdk-overlay') text = '(无文字' + (element.querySelector('iframe') ? ', iframe' : '') + ')';
          else if (element.querySelector('.cdk-overlay-backdrop-showing')) text = '(遮罩)';
          else continue;
        }
        if (texts.has(text)) continue;
        texts.add(text);
        overlayElements.push(element);
        overlays.push(label + ': ' + text);
      }
    }
    out.overlays = overlays;
  });
  // alerts 为弹层之外页面主区域里的错误与提示条（role=alert、role=status、aria-live 区域、mat-error），弹层里的已记在 overlays
  attempt('alerts', () => {
    const inOverlay = element => {
      for (let node = element, depth = 0; node && depth < 30; node = node.parentElement, depth++) {
        if (overlayElements.includes(node)) return true;
      }
      return false;
    };
    const texts = [];
    for (const element of document.querySelectorAll('[role=alert], [role=status], [aria-live=assertive], [aria-live=polite], mat-error')) {
      if (texts.length >= 2) break;
      if (!visible(element) || inOverlay(element)) continue;
      const text = short(element.innerText, 100);
      if (text && !texts.includes(text)) texts.push(text);
    }
    out.alerts = texts;
  });
  attempt('body', () => { out.body = short(document.body ? document.body.innerText : '', 200); });
  attempt('ms', () => {
    let fromOverlays = 0;
    for (const overlay of overlayElements) {
      for (const element of overlay.querySelectorAll('*')) {
        if (fromOverlays >= 6 || tags.size >= 20) break;
        const name = element.localName;
        if (name.startsWith('ms-') && !tags.has(name) && visible(element)) {
          tags.add(name);
          fromOverlays++;
        }
      }
    }
    for (const element of document.querySelectorAll('*')) {
      if (tags.size >= 20) break;
      const name = element.localName;
      if (name.startsWith('ms-') && !tags.has(name) && visible(element)) tags.add(name);
    }
    out.ms = [...tags];
  });
  if (errors.length) out.err = errors.slice(0, 4);
  return JSON.stringify(out);
})()`

// pageSnapshot 为写进日志的页面快照字段（含义见 docs/logging.md）；Go 侧只保留这些字段并重新编码，字段顺序即输出顺序。
// Href 为页面返回的完整地址，只用来生成 URL，不写进日志；Since 由 Go 侧填写（见 sinceNavigated），不取页面返回的值
type pageSnapshot struct {
	Href      string   `json:"href,omitempty"`
	URL       string   `json:"url,omitempty"`
	Title     string   `json:"title,omitempty"`
	Ready     string   `json:"ready,omitempty"`
	Vis       string   `json:"vis,omitempty"`
	Age       *int64   `json:"age,omitempty"`
	Since     *int64   `json:"since,omitempty"`
	Nav       string   `json:"nav,omitempty"`
	Box       []int    `json:"box,omitempty"`
	Input     []int    `json:"input,omitempty"`
	Textareas *int     `json:"textareas,omitempty"`
	Editors   []string `json:"ta,omitempty"`
	Run       []int    `json:"run,omitempty"`
	WAA       *bool    `json:"waa,omitempty"`
	Hook      *bool    `json:"hook,omitempty"`
	Capture   *bool    `json:"cap,omitempty"`
	Overlays  []string `json:"overlays,omitempty"`
	Alerts    []string `json:"alerts,omitempty"`
	Tags      []string `json:"ms,omitempty"`
	Body      string   `json:"body,omitempty"`
	Errors    []string `json:"err,omitempty"`
}

// pageSnapshotReductions 为快照超过 pageSnapshotLimit 时依次累加的缩短步骤：正文多是侧栏导航文字，最先缩短、清空；
// 再缩短提示条、组件标签（只留前面的优先段：输入框、Run 按钮与弹层所在的组件）与弹层文字。计数、地址与 age、since、nav、waa、hook、cap 一直保留；
// 全部用完仍超长时按字符硬截断（见 encode）
var pageSnapshotReductions = []func(*pageSnapshot){
	func(snapshot *pageSnapshot) { snapshot.Body = truncateRunes(snapshot.Body, 80) },
	func(snapshot *pageSnapshot) { snapshot.Body = "" },
	func(snapshot *pageSnapshot) { snapshot.Alerts = shortenTexts(firstItems(snapshot.Alerts, 1), 60) },
	func(snapshot *pageSnapshot) { snapshot.Tags = firstItems(snapshot.Tags, 10) },
	func(snapshot *pageSnapshot) { snapshot.Overlays = shortenTexts(firstItems(snapshot.Overlays, 3), 100) },
	func(snapshot *pageSnapshot) { snapshot.Editors = shortenTexts(snapshot.Editors, 50) },
	func(snapshot *pageSnapshot) { snapshot.Tags = firstItems(snapshot.Tags, 5) },
	func(snapshot *pageSnapshot) { snapshot.Overlays = shortenTexts(firstItems(snapshot.Overlays, 1), 60) },
	func(snapshot *pageSnapshot) {
		snapshot.Errors = nil
		snapshot.Title = truncateRunes(snapshot.Title, 40)
		snapshot.Editors = firstItems(snapshot.Editors, 1)
	},
}

// snapshotEmailPattern 匹配快照文本里的邮箱，打码为前两位 + ***@***（与快照表达式里的 mask、tools/diag_*.sh 的打码规则一致）
var snapshotEmailPattern = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)

// snapshotTruncatedEmailPattern 匹配截断在域名里、以“…”结尾的残缺邮箱（域名不完整，snapshotEmailPattern 认不出来）。
// 快照表达式已先打码再截短，这里兜底，同样打码为前两位 + ***@***
var snapshotTruncatedEmailPattern = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@[A-Za-z0-9.-]*…$`)

// markNavigated 记下 Worker 发出导航 AI Studio 命令的时刻，供页面快照写 since（只记时间，不影响导航本身）
func (client *bidiClient) markNavigated() {
	client.navigatedAt.Store(time.Now().UnixNano())
}

// sinceNavigated 返回导航 AI Studio 至今的秒数（四舍五入）；还没导航（登录等其他页面）时返回 nil，快照不写 since
func (client *bidiClient) sinceNavigated() *int64 {
	at := client.navigatedAt.Load()
	if at == 0 {
		return nil
	}
	seconds := int64(math.Round(time.Since(time.Unix(0, at)).Seconds()))
	return &seconds
}

// pageDiagnosis 在启动时的页面类失败处读取页面快照（运行中请求见 withRequestPageSnapshot），返回追加到原错误末尾的“ | 页面=…”；
// withURL 时先读 location.href，地址与快照共用同一个从 context.Background 派生的短上下文（失败处的 ctx 可能已经过期）。
// 调用方已取消（停止服务、客户端断开）时不读取、返回空串：这不是页面问题，调用方也不会记录这次失败
func (client *bidiClient) pageDiagnosis(ctx context.Context, contextID string, withURL bool) (pageURL string, suffix string) {
	pageURL, suffix, _ = client.pageDiagnosisBlocked(ctx, contextID, withURL)
	return pageURL, suffix
}

// pageDiagnosisBlocked 同 pageDiagnosis，另返回页面停在组织停用页时的地址（见 pageDiagnosisWithin 的 blocked）
func (client *bidiClient) pageDiagnosisBlocked(ctx context.Context, contextID string, withURL bool) (pageURL string, suffix string, blocked string) {
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", "", ""
	}
	diagnosisCtx, cancel := context.WithTimeout(context.Background(), pageSnapshotTimeout)
	defer cancel()
	return client.pageDiagnosisWithin(diagnosisCtx, contextID, withURL)
}

// pageDiagnosisWithin 同 pageDiagnosis，地址与快照用调用方给的诊断上下文（与同一处的其他诊断共用期限，见 startupFailureDiagnosis）。
// blocked 为页面停在组织停用页（access.workspace.google.com，见 ServiceNotAllowedError）时的地址，取自读到的地址或快照里的 href，
// 不多做 evaluate；不在时为空
func (client *bidiClient) pageDiagnosisWithin(diagnosisCtx context.Context, contextID string, withURL bool) (pageURL string, suffix string, blocked string) {
	if withURL {
		pageURL, _ = client.evaluateString(diagnosisCtx, contextID, "location.href")
	}
	raw, err := client.evaluateString(diagnosisCtx, contextID, pageSnapshotExpression)
	switch {
	case isServiceNotAllowedURL(pageURL):
		blocked = pageURL
	case err == nil:
		blocked = snapshotServiceNotAllowed(raw)
	}
	return pageURL, formatPageSnapshot(raw, err, client.sinceNavigated()), blocked
}

// withPageSnapshot 在页面类失败的原错误末尾追加“ | 页面=…”；原错误的文字与错误链（errors.Is/As）不变，
// 应用层与 tools/diag_*.sh 仍按原前缀匹配
func (client *bidiClient) withPageSnapshot(ctx context.Context, contextID string, err error) error {
	if err == nil {
		return nil
	}
	_, suffix := client.pageDiagnosis(ctx, contextID, false)
	if suffix == "" {
		return err
	}
	return fmt.Errorf("%w%s", err, suffix)
}

// withRequestPageSnapshot 为运行中请求（准备 proof）的页面类失败追加页面快照。与启动时不同，快照上下文从请求 ctx 派生、
// 期限截在请求期限之前 pageSnapshotRequestMargin：请求取消时快照随之结束，快照结束时请求也还没到期。
// aistudio.NativeWorker.Prepare 按 ctx.Err() 区分请求自己结束与 Worker 失败，快照不能把请求拖过期限，否则坏页面的 Worker 被当成请求到期、不重建。
// 请求已结束、剩余时间不足 pageSnapshotMinimum，或快照期间请求被取消时原样返回原错误
func (client *bidiClient) withRequestPageSnapshot(ctx context.Context, contextID string, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	deadline := time.Now().Add(pageSnapshotTimeout)
	if requestDeadline, ok := ctx.Deadline(); ok {
		if limit := requestDeadline.Add(-pageSnapshotRequestMargin); limit.Before(deadline) {
			deadline = limit
		}
	}
	if time.Until(deadline) < pageSnapshotMinimum {
		return err
	}
	snapshotCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	raw, snapshotErr := client.evaluateString(snapshotCtx, contextID, pageSnapshotExpression)
	if ctx.Err() != nil {
		return err
	}
	return fmt.Errorf("%w%s", err, formatPageSnapshot(raw, snapshotErr, client.sinceNavigated()))
}

// formatPageSnapshot 把页面返回的快照整理成“ | 页面={…}”：地址只留 host、path 与 model 参数，邮箱打码，
// “|”换成“¦”（日志字段以“ | ”分隔），总长度不超过 pageSnapshotLimit；since 为 Go 侧记下的导航至今秒数（nil 时不写）。
// 快照本身失败（浏览器已断开、超时等）时返回“ | 页面=快照失败: <简短原因>”，不掩盖原错误
func formatPageSnapshot(raw string, err error, since *int64) string {
	if err != nil {
		return PageSnapshotMarker + "快照失败: " + snapshotText(strings.Join(strings.Fields(err.Error()), " "), pageSnapshotReasonLimit)
	}
	var snapshot pageSnapshot
	if decodeErr := json.Unmarshal([]byte(raw), &snapshot); decodeErr != nil {
		// 个别字段类型不符时 Unmarshal 跳过该字段、照常解出其余字段，记一条 err 后继续
		var typeErr *json.UnmarshalTypeError
		if !errors.As(decodeErr, &typeErr) {
			return PageSnapshotMarker + "快照失败: 返回值不是 JSON"
		}
		snapshot.Errors = append(snapshot.Errors, "decode: "+typeErr.Field)
	}
	snapshot.URL = pageSnapshotURL(snapshot.Href)
	snapshot.Href = ""
	snapshot.Since = since
	snapshot.sanitize()
	return PageSnapshotMarker + snapshot.encode()
}

// sanitize 对快照里的全部文本打码并去掉“|”；页面里已经截短，这里只防御页面返回的超长值
func (snapshot *pageSnapshot) sanitize() {
	snapshot.URL = snapshotText(snapshot.URL, 160)
	snapshot.Title = snapshotText(snapshot.Title, 120)
	snapshot.Ready = snapshotText(snapshot.Ready, 20)
	snapshot.Vis = snapshotText(snapshot.Vis, 20)
	snapshot.Nav = snapshotText(snapshot.Nav, 20)
	snapshot.Editors = shortenTexts(firstItems(snapshot.Editors, 3), 100)
	snapshot.Body = snapshotText(snapshot.Body, 200)
	snapshot.Overlays = shortenTexts(firstItems(snapshot.Overlays, 6), 180)
	snapshot.Alerts = shortenTexts(firstItems(snapshot.Alerts, 2), 120)
	snapshot.Tags = shortenTexts(firstItems(snapshot.Tags, 20), 40)
	snapshot.Errors = shortenTexts(firstItems(snapshot.Errors, 4), 80)
}

// encode 编码快照，超过 pageSnapshotLimit 时依次应用缩短步骤
func (snapshot pageSnapshot) encode() string {
	text := snapshot.marshal()
	for _, reduce := range pageSnapshotReductions {
		if utf8.RuneCountInString(text) <= pageSnapshotLimit {
			return text
		}
		reduce(&snapshot)
		text = snapshot.marshal()
	}
	return truncateRunes(text, pageSnapshotLimit)
}

// marshal 编码为单行 JSON；不转义 <、>、&，日志里保持可读
func (snapshot *pageSnapshot) marshal() string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(snapshot); err != nil {
		return "{}"
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}

// pageSnapshotURL 把页面地址缩短为 host + path，查询参数只保留 model；没有 host 的地址（about:blank 等）只留 scheme 与 opaque
func pageSnapshotURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
			raw = raw[:cut]
		}
		return truncateRunes(raw, 80)
	}
	if parsed.Host == "" {
		if parsed.Opaque != "" {
			return truncateRunes(parsed.Scheme+":"+parsed.Opaque, 40)
		}
		return truncateRunes(parsed.Scheme+":"+parsed.Path, 80)
	}
	value := parsed.Host + parsed.Path
	if model := parsed.Query().Get("model"); model != "" {
		value += "?model=" + model
	}
	return value
}

// snapshotText 对一段快照文本打码（含截断在域名里的残缺邮箱）、把“|”换成“¦”并截到 limit 个字符
func snapshotText(text string, limit int) string {
	text = snapshotEmailPattern.ReplaceAllString(text, "${1}***@***")
	text = snapshotTruncatedEmailPattern.ReplaceAllString(text, "${1}***@***…")
	return truncateRunes(strings.ReplaceAll(text, "|", "¦"), limit)
}

// shortenTexts 对每一项调用 snapshotText；空列表返回 nil，编码时省略
func shortenTexts(items []string, limit int) []string {
	if len(items) == 0 {
		return nil
	}
	shortened := make([]string, len(items))
	for index, item := range items {
		shortened[index] = snapshotText(item, limit)
	}
	return shortened
}

// firstItems 返回前 limit 项（不修改原切片）
func firstItems(items []string, limit int) []string {
	if len(items) <= limit {
		return items
	}
	return items[:limit:limit]
}

// truncateRunes 把文本截到 limit 个字符，截断时以“…”结尾
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	if limit <= 1 {
		return "…"
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}
