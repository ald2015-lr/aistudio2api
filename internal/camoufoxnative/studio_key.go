package camoufoxnative

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 获取 AI Studio API Key：用账户已保存的登录态启动无头隔离浏览器（与账户验证相同的方式，见 Verify），打开 API Keys 页面
// （aistudio.google.com/apikey），只读监听页面自己发出的请求的响应（WebDriver BiDi network.responseCompleted + network.addDataCollector
// + network.getData），从响应正文里解析出 Key 与项目（见 ExtractStudioKeys）。本流程不手工构造任何发往 Google 的请求：
//   - 页面里已有 Key：只读取，不创建；
//   - 页面加载完成后仍没有 Key：用真实指针点击页面自己的“Create API key”按钮与对话框里的确认按钮（只创建一个），
//     请求由页面自己发出，再从之后的响应里读取新 Key。
//
// Key 是机密：不写日志，错误文案里任何像 Key 的片段都打码（RedactStudioKeys）。

// StudioKeyPath 为 API Keys 页面的路径
const StudioKeyPath = "/apikey"

const (
	// studioKeySettle 为导航后等页面加载 Key 列表的时间：超过这段时间仍没有从响应里读到 Key、页面上有创建按钮时才创建
	studioKeySettle = 15 * time.Second
	// studioKeyCreateWait 为点击创建之后等新 Key 出现在响应里的时间
	studioKeyCreateWait = 60 * time.Second
	// studioKeyProjectWait 为读到 Key 但还没找到项目名时多等的时间
	studioKeyProjectWait = 5 * time.Second
	// studioKeyMaxBody 为单个响应正文收集的上限（字节）
	studioKeyMaxBody = 8 << 20
)

// 失败原因（StudioKeyError.Reason），写进管理页与运行日志，不含 Key
const (
	StudioKeyReasonLoginExpired = "AI Studio 登录已失效，请重新登录"
	StudioKeyReasonOrgDisabled  = ServiceNotAllowedPrefix
	StudioKeyReasonNoCreate     = "页面上没有 Key，也没有找到“Create API key”按钮"
	StudioKeyReasonCreateFailed = "已点击创建，但没有在页面响应里读到新 Key"
	StudioKeyReasonDialog       = "创建 Key 的对话框里没有可点击的确认按钮（可能需要先选择或创建 Google Cloud 项目，请在浏览器里手动完成一次）"
	StudioKeyReasonCollector    = "浏览器不支持读取页面响应正文"
	StudioKeyReasonUnreadable   = "页面上已有 Key，但没能从页面响应里读到完整 Key（页面接口可能已改版），没有创建新 Key"
)

// StudioKeyResult 为一次获取的结果。Key 是机密，调用方只能把它写进 0600 的文件，不能写日志
type StudioKeyResult struct {
	Key            string
	ProjectName    string
	ProjectNumber  string
	ProjectDisplay string
	// Created 表示这次通过页面的创建流程新建了 Key
	Created bool
	// KeyCount 为在页面响应里看到的 Key 个数
	KeyCount int
}

// StudioKeyError 为获取失败；Reason 不含 Key（已打码）
type StudioKeyError struct {
	Reason string
	Cause  error
}

func (err *StudioKeyError) Error() string {
	if err.Cause == nil {
		return err.Reason
	}
	return err.Reason + ": " + RedactStudioKeys(err.Cause.Error())
}

func (err *StudioKeyError) Unwrap() error {
	return err.Cause
}

// FetchStudioKey 用已有登录态打开 API Keys 页面读取（必要时创建）API Key
func FetchStudioKey(ctx context.Context, options LoginOptions, storageStateJSON []byte) (result StudioKeyResult, err error) {
	options, err = validateLoginOptions(options)
	if err != nil {
		return StudioKeyResult{}, err
	}
	var state storageState
	if err := json.Unmarshal(storageStateJSON, &state); err != nil {
		return StudioKeyResult{}, fmt.Errorf("解析 storage state: %w", err)
	}
	if len(state.Cookies) == 0 {
		return StudioKeyResult{}, errors.New("storage state 没有 Cookie")
	}
	fetchCtx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	session, err := startLoginSession(fetchCtx, options, true, state)
	if err != nil {
		return StudioKeyResult{}, &StudioKeyError{Reason: "启动浏览器失败", Cause: err}
	}
	defer func() {
		session.client.responseHook.Store(nil)
		// 结果已经拿到时浏览器关闭失败不影响结果；关闭错误可能带页面信息，不并入返回值
		_ = session.Close()
	}()
	session.onboardingCompleted = options.OnboardingCompleted
	return session.fetchStudioKey(fetchCtx)
}

// studioKeyListener 收集页面响应的请求 ID（只收 Google 相关域名），由后台读取协程写入
type studioKeyListener struct {
	mu       sync.Mutex
	requests []string
}

func (listener *studioKeyListener) observe(params map[string]any) {
	request, _ := params["request"].(map[string]any)
	requestID, _ := request["request"].(string)
	rawURL, _ := request["url"].(string)
	if requestID == "" || !studioKeyResponseHost(rawURL) {
		return
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	if len(listener.requests) < 4096 {
		listener.requests = append(listener.requests, requestID)
	}
}

func (listener *studioKeyListener) take() []string {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	taken := listener.requests
	listener.requests = nil
	return taken
}

// studioKeyResponseHost 判断响应是否来自可能带 Key 的域名：aistudio、makersuite 与 googleapis
func studioKeyResponseHost(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "aistudio.google.com" || strings.Contains(host, "makersuite") ||
		host == "googleapis.com" || strings.HasSuffix(host, ".googleapis.com") ||
		strings.HasSuffix(host, ".clients6.google.com")
}

func (session *loginSession) fetchStudioKey(ctx context.Context) (StudioKeyResult, error) {
	client := session.client
	listener := &studioKeyListener{}
	hook := listener.observe
	client.responseHook.Store(&hook)
	if _, err := client.command(ctx, "session.subscribe", map[string]any{
		"events":   []string{"network.responseCompleted"},
		"contexts": []string{session.contextID},
	}); err != nil {
		return StudioKeyResult{}, &StudioKeyError{Reason: "订阅页面响应失败", Cause: err}
	}
	collected, err := client.command(ctx, "network.addDataCollector", map[string]any{
		"dataTypes":          []string{"response"},
		"maxEncodedDataSize": studioKeyMaxBody,
		"contexts":           []string{session.contextID},
	})
	if err != nil {
		return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonCollector, Cause: err}
	}
	collector, _ := collected["collector"].(string)
	navigate := func() error {
		if _, err := client.command(ctx, "browsingContext.navigate", map[string]any{
			"context": session.contextID,
			"url":     aiStudioOrigin + StudioKeyPath,
			"wait":    "interactive",
		}); err != nil && !strings.Contains(err.Error(), "NS_ERROR_ABORT") {
			return &StudioKeyError{Reason: "打开 API Keys 页面失败", Cause: err}
		}
		return nil
	}
	if err := navigate(); err != nil {
		return StudioKeyResult{}, err
	}
	var (
		onboarding  onboardingBudget
		keys        []StudioKeyCandidate
		navigatedAt = time.Now()
		created     bool
		createdAt   time.Time
		keysAt      time.Time
		dialogSince time.Time
		selectTried bool
		existing    = map[string]bool{}
	)
	for {
		if err := ctx.Err(); err != nil {
			reason := "获取 API Key 超时"
			if created {
				reason = StudioKeyReasonCreateFailed
			}
			return StudioKeyResult{}, &StudioKeyError{Reason: reason, Cause: err}
		}
		for _, requestID := range listener.take() {
			body, ok := client.studioKeyResponseBody(ctx, collector, requestID)
			if ok {
				keys = mergeStudioKeys(keys, ExtractStudioKeys(body))
			}
		}
		if len(keys) > 0 && keysAt.IsZero() {
			keysAt = time.Now()
		}
		if chosen, ok := chooseStudioKey(keys, existing, created); ok {
			if chosen.ProjectName != "" || time.Since(keysAt) >= studioKeyProjectWait {
				return StudioKeyResult{
					Key: chosen.Key, ProjectName: chosen.ProjectName, ProjectNumber: chosen.ProjectNumber,
					ProjectDisplay: chosen.ProjectDisplay, Created: created, KeyCount: len(keys),
				}, nil
			}
		}
		pageURL, err := client.evaluateString(ctx, session.contextID, "location.href")
		if err != nil {
			if !retryablePageEvaluation(err) {
				return StudioKeyResult{}, &StudioKeyError{Reason: "读取页面失败", Cause: err}
			}
			if err := waitContext(ctx, 300*time.Millisecond); err != nil {
				continue
			}
			continue
		}
		switch {
		case isServiceNotAllowedURL(pageURL):
			return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonOrgDisabled}
		case isGoogleLoginURL(pageURL):
			return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonLoginExpired}
		case session.atOnboarding(ctx, pageURL):
			if err := session.finishOnboarding(ctx, &onboarding); err != nil {
				return StudioKeyResult{}, &StudioKeyError{Reason: "AI Studio 首次使用页未能完成", Cause: errors.New(StripPageSnapshots(err.Error()))}
			}
			if err := navigate(); err != nil {
				return StudioKeyResult{}, err
			}
			navigatedAt = time.Now()
			continue
		}
		if !created && time.Since(navigatedAt) >= studioKeySettle {
			// 页面加载完成后仍没有 Key：记下已看到的 Key（应为空），点击页面自己的创建按钮
			for _, candidate := range keys {
				existing[candidate.Key] = true
			}
			if len(keys) > 0 {
				// 读到了 Key 只是缺项目名，前面会在 studioKeyProjectWait 后返回，这里不创建
				if err := waitContext(ctx, 300*time.Millisecond); err != nil {
					continue
				}
				continue
			}
			// 页面上看得到 Key（通常只显示开头或末几位），响应里却没读到：不创建，免得多建一个
			if shown, _ := client.evaluateBool(ctx, session.contextID, studioKeyShownExpression); shown {
				return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonUnreadable}
			}
			if err := session.click(ctx, session.contextID, studioKeyCreateButtonExpression); err != nil {
				if errors.Is(err, errElementMissing) {
					if time.Since(navigatedAt) >= 2*studioKeySettle {
						return StudioKeyResult{}, errStudioKeyNone
					}
				} else if !retryablePageEvaluation(err) {
					return StudioKeyResult{}, &StudioKeyError{Reason: "点击创建按钮失败", Cause: err}
				}
			} else {
				created = true
				createdAt = time.Now()
				dialogSince = time.Now()
			}
		}
		if created && !dialogSince.IsZero() {
			// 创建对话框：点击对话框里的确认按钮（按钮还不可用时等一会儿；有下拉框没选时选一次第一项）
			state, _ := client.evaluateString(ctx, session.contextID, studioKeyDialogStateExpression)
			switch state {
			case "ready":
				if err := session.click(ctx, session.contextID, studioKeyDialogConfirmExpression); err == nil {
					dialogSince = time.Time{}
				}
			case "select":
				if !selectTried {
					selectTried = true
					_ = session.click(ctx, session.contextID, studioKeyDialogSelectExpression)
					_ = waitContext(ctx, 800*time.Millisecond)
					_ = session.click(ctx, session.contextID, studioKeyFirstOptionExpression)
				} else if time.Since(dialogSince) > 20*time.Second {
					return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonDialog}
				}
			case "none":
				// 没有对话框：可能页面直接创建了 Key，继续等响应
				if time.Since(dialogSince) > 10*time.Second {
					dialogSince = time.Time{}
				}
			default:
				if time.Since(dialogSince) > 20*time.Second {
					return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonDialog}
				}
			}
		}
		if created && time.Since(createdAt) > studioKeyCreateWait {
			return StudioKeyResult{}, &StudioKeyError{Reason: StudioKeyReasonCreateFailed}
		}
		if err := waitContext(ctx, 500*time.Millisecond); err != nil {
			continue
		}
	}
}

// errStudioKeyNone 表示页面上没有 Key、也找不到创建按钮
var errStudioKeyNone = &StudioKeyError{Reason: StudioKeyReasonNoCreate}

// ErrStudioKeyNone 供调用方判断“没有 Key”
func ErrStudioKeyNone(err error) bool {
	var keyErr *StudioKeyError
	return errors.As(err, &keyErr) && keyErr.Reason == StudioKeyReasonNoCreate
}

// chooseStudioKey 选出要记录的 Key：创建之后只认创建前没有的新 Key（有多条时取第一条）；没有创建时取第一条
func chooseStudioKey(keys []StudioKeyCandidate, existing map[string]bool, created bool) (StudioKeyCandidate, bool) {
	for _, candidate := range keys {
		if created && existing[candidate.Key] {
			continue
		}
		return candidate, true
	}
	return StudioKeyCandidate{}, false
}

// studioKeyResponseBody 用 network.getData 读取已收集的响应正文；读不到（不是被收集的请求、正文过大、已被释放）时返回 false
func (client *bidiClient) studioKeyResponseBody(ctx context.Context, collector string, requestID string) ([]byte, bool) {
	params := map[string]any{"dataType": "response", "request": requestID}
	if collector != "" {
		params["collector"] = collector
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	result, err := client.command(readCtx, "network.getData", params)
	if err != nil {
		return nil, false
	}
	data, _ := result["bytes"].(map[string]any)
	kind, _ := data["type"].(string)
	value, _ := data["value"].(string)
	if kind == "base64" {
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return nil, false
		}
		return decoded, true
	}
	return []byte(value), value != ""
}

// studioKeyButtonText 为匹配“创建 API Key”按钮文字的正则（多语言），在页面里用
const studioKeyButtonText = `/(create\s+api\s+key|create\s+key|创建\s*api\s*(密钥|key)|建立\s*api\s*(金鑰|key)|api\s*キーを作成|キーを作成|api\s*키\s*만들기)/i`

// studioKeyCreateButtonExpression 返回页面主区域（不在对话框里）可见、可用的创建按钮，没有时为 null
const studioKeyCreateButtonExpression = `(() => {` + pageDOMHelpers + `
  const pattern = ` + studioKeyButtonText + `;
  const label = element => ((element.innerText || '') + ' ' + (element.getAttribute('aria-label') || '')).replace(/\s+/g, ' ').trim();
  const inDialog = element => element.closest('[role=dialog], mat-dialog-container, .cdk-overlay-container');
  return [...document.querySelectorAll('button, a[role=button], [role=button]')]
    .filter(visible).filter(buttonEnabled).filter(element => !inDialog(element))
    .find(element => pattern.test(label(element))) || null;
})()`

// studioKeyShownExpression 判断页面主区域（不含侧栏导航与对话框）是否显示着 Key：完整或打码的 AIza…、AQ.…，或“…末四位”
const studioKeyShownExpression = `(() => {
  const main = document.querySelector('main, [role=main]') || document.body;
  if (!main) return false;
  const text = main.innerText || '';
  return /\bAIza[0-9A-Za-z_-]{0,35}|\bAQ\.[0-9A-Za-z_-]{0,}|(\.\.\.|…)[0-9A-Za-z_-]{4}\b/.test(text);
})()`

// studioKeyDialogScope 为创建对话框的查找范围
const studioKeyDialogScope = `
  const dialogs = [...document.querySelectorAll('[role=dialog], mat-dialog-container')].filter(visible);
  const dialog = dialogs[dialogs.length - 1];
  const label = element => ((element.innerText || '') + ' ' + (element.getAttribute('aria-label') || '')).replace(/\s+/g, ' ').trim();
  const confirmPattern = /(create|创建|建立|作成|만들기|generate|生成)/i;
  const cancelPattern = /(cancel|取消|キャンセル|취소|close|关闭)/i;
  const confirm = dialog ? [...dialog.querySelectorAll('button')].filter(visible)
    .filter(element => confirmPattern.test(label(element)) && !cancelPattern.test(label(element))).pop() : null;
`

// studioKeyDialogStateExpression 返回对话框状态：none（没有对话框）、ready（确认按钮可点）、select（确认按钮不可用且有下拉框）、wait
const studioKeyDialogStateExpression = `(() => {` + pageDOMHelpers + studioKeyDialogScope + `
  if (!dialog) return 'none';
  if (confirm && buttonEnabled(confirm)) return 'ready';
  if (dialog.querySelector('mat-select, [role=combobox], select')) return 'select';
  return 'wait';
})()`

// studioKeyDialogConfirmExpression 返回对话框里可用的确认按钮
const studioKeyDialogConfirmExpression = `(() => {` + pageDOMHelpers + studioKeyDialogScope + `
  return confirm && buttonEnabled(confirm) ? confirm : null;
})()`

// studioKeyDialogSelectExpression 返回对话框里的下拉框
const studioKeyDialogSelectExpression = `(() => {` + pageDOMHelpers + studioKeyDialogScope + `
  if (!dialog) return null;
  return [...dialog.querySelectorAll('mat-select, [role=combobox], select')].filter(visible)[0] || null;
})()`

// studioKeyFirstOptionExpression 返回展开的下拉列表里第一个可选项
const studioKeyFirstOptionExpression = `(() => {` + pageDOMHelpers + `
  return [...document.querySelectorAll('mat-option, [role=option]')].filter(visible)
    .find(element => element.getAttribute('aria-disabled') !== 'true') || null;
})()`
