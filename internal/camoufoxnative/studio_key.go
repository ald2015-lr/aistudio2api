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
// + network.getData），从响应正文里解析出 Key 与项目（见 ExtractStudioKeys、ExtractStudioProjects）。本流程不手工构造任何发往 Google 的请求：
//   - 页面里已有 Key：只读取，不创建；页面上看得到 Key、响应里却读不到时失败，不创建；
//   - 页面加载完成后仍没有 Key：用真实指针点击页面自己的“Create API key”按钮，再按对话框的状态一步步点页面自己的控件
//     （见 studioKeyFlow：有项目时选一个，没有项目时经页面自己的新建项目入口建一个、等它出现），最后点创建 Key 的确认。
//     页面上没有可用的“Create API key”、却有新建项目的按钮时（没有项目的空状态），先去 Projects 页新建项目，回来等 Key 列表
//     加载后再点（新项目自带 Key 时直接读取）。每次点“Create API key”之前都核对页面上是否已显示 Key（显示着、响应里却读不到时
//     不创建），点了之后要先见到对话框才会再点。请求都由页面自己发出，再从之后的响应里读取新 Key。每次运行最多新建一个项目、
//     一个 Key，从不删除任何东西，不点站外链接。
//   - 页面上有验证账号的提示（需要验证账号的账户）不影响：只要 API Keys 页面可用就照常读取或创建；页面被跳到验证账号页时
//     重新打开一次，仍被跳走才失败。
//
// Key 是机密：不写日志，错误文案里任何像 Key 的片段都打码（RedactStudioKeys），页面文字读出来之前就把 Key 换成 <key>。

// StudioKeyPath 为 API Keys 页面的路径
const StudioKeyPath = "/apikey"

// StudioKeyMinTimeout 为整次获取（启动浏览器到读到 Key）的最短时限：调用方给的时限（驱动的 REQUEST_TIMEOUT）更短时按它，
// 免得没有项目的账户在建好项目之后、建 Key 之前超时（新建项目再创建 Key 最长约 4 分钟）
const StudioKeyMinTimeout = 5 * time.Minute

const (
	// studioKeyProjectNameWait 为读到 Key 但还没找到项目名时多等的时间
	studioKeyProjectNameWait = 5 * time.Second
	// studioKeyMaxBody 为单个响应正文收集的上限（字节）
	studioKeyMaxBody = 8 << 20
)

// 失败原因（StudioKeyError.Reason），写进管理页与运行日志，不含 Key；创建流程各步卡住时的原因见 studio_key_flow.go
const (
	StudioKeyReasonLoginExpired = "AI Studio 登录已失效，请重新登录"
	StudioKeyReasonOrgDisabled  = ServiceNotAllowedPrefix
	StudioKeyReasonNoCreate     = "页面上没有 Key，也没有找到“Create API key”按钮"
	StudioKeyReasonCreateFailed = "已点击创建，但没有在页面响应里读到新 Key"
	StudioKeyReasonDialog       = "创建 Key 的对话框里没有找到可点的下一步"
	StudioKeyReasonCollector    = "浏览器不支持读取页面响应正文"
	StudioKeyReasonUnreadable   = "页面上已有 Key，但没能从页面响应里读到完整 Key（页面接口可能已改版），没有创建新 Key"
	StudioKeyReasonTimeout      = "获取 API Key 超时"
	StudioKeyReasonFlowTimeout  = "获取 API Key 超时（创建流程没有走完）"
)

// StudioKeyResult 为一次获取的结果。Key 是机密，调用方只能把它写进 0600 的文件，不能写日志
type StudioKeyResult struct {
	Key            string
	ProjectName    string
	ProjectNumber  string
	ProjectDisplay string
	// Created 表示这次通过页面的创建流程新建了 Key
	Created bool
	// ProjectCreated 表示这次先经页面新建了一个 Google Cloud 项目
	ProjectCreated bool
	// KeyCount 为在页面响应里看到的 Key 个数
	KeyCount int
	// Steps 为创建流程做过的步骤（页面文字已打码、截短，不含 Key），没有创建时为空
	Steps []string
}

// StudioKeyError 为获取失败；Reason 不含 Key（已打码），Detail 为接在后面的步骤记录与对话框摘要（打码、截短，可为空）
type StudioKeyError struct {
	Reason string
	Detail string
	Cause  error
}

func (err *StudioKeyError) Error() string {
	text := err.Reason + RedactStudioKeys(err.Detail)
	if err.Cause == nil {
		return text
	}
	return text + ": " + RedactStudioKeys(err.Cause.Error())
}

func (err *StudioKeyError) Unwrap() error {
	return err.Cause
}

// FetchStudioKey 用已有登录态打开 API Keys 页面读取（必要时创建）API Key；整次的时限为 options.Timeout，不短于 StudioKeyMinTimeout
func FetchStudioKey(ctx context.Context, options LoginOptions, storageStateJSON []byte) (result StudioKeyResult, err error) {
	options, err = validateLoginOptions(options)
	if err != nil {
		return StudioKeyResult{}, err
	}
	options.Timeout = studioKeyTimeout(options.Timeout)
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

// studioKeyTimeout 返回整次获取的时限：timeout 与 StudioKeyMinTimeout 中较大者
func studioKeyTimeout(timeout time.Duration) time.Duration {
	return max(timeout, StudioKeyMinTimeout)
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
	// navigate 在当前标签页打开 AI Studio 站内的页面（只用于 API Keys 页、Projects 页与对话框里的站内链接）
	navigate := func(path string) error {
		if _, err := client.command(ctx, "browsingContext.navigate", map[string]any{
			"context": session.contextID,
			"url":     aiStudioOrigin + path,
			"wait":    "interactive",
		}); err != nil && !strings.Contains(err.Error(), "NS_ERROR_ABORT") {
			return &StudioKeyError{Reason: "打开 AI Studio 页面失败（" + path + "）", Cause: err}
		}
		return nil
	}
	if err := navigate(StudioKeyPath); err != nil {
		return StudioKeyResult{}, err
	}
	var (
		onboarding    onboardingBudget
		keys          []StudioKeyCandidate
		navigatedAt   = time.Now()
		keysAt        time.Time
		existing      = map[string]bool{}
		flow          = newStudioKeyFlow()
		verifyRetried bool
		offSiteSince  time.Time
		dismissedAt   time.Time
	)
	fail := func(reason string, cause error) (StudioKeyResult, error) {
		return StudioKeyResult{}, &StudioKeyError{Reason: reason, Detail: flow.detail(), Cause: cause}
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(flow.timeoutReason(time.Now()), err)
		}
		for _, requestID := range listener.take() {
			body, ok := client.studioKeyResponseBody(ctx, collector, requestID)
			if ok {
				keys = mergeStudioKeys(keys, ExtractStudioKeys(body))
				flow.noteProjects(ExtractStudioProjects(body))
			}
		}
		if len(keys) > 0 && keysAt.IsZero() {
			keysAt = time.Now()
		}
		if chosen, ok := chooseStudioKey(keys, existing, flow.running()); ok {
			if chosen.ProjectName != "" || time.Since(keysAt) >= studioKeyProjectNameWait {
				return flow.result(chosen, len(keys)), nil
			}
		}
		pageURL, err := client.evaluateString(ctx, session.contextID, "location.href")
		if err != nil {
			if !retryablePageEvaluation(err) {
				return fail("读取页面失败", err)
			}
			_ = waitContext(ctx, 300*time.Millisecond)
			continue
		}
		switch {
		case isServiceNotAllowedURL(pageURL):
			return fail(StudioKeyReasonOrgDisabled, nil)
		case isGoogleLoginURL(pageURL):
			return fail(StudioKeyReasonLoginExpired, nil)
		case isStudioVerifyURL(pageURL):
			// 需要验证账号的账户：API Keys 页面通常照常可用，只有页面被跳到验证账号页时才处理。还没开始创建时重新打开一次，
			// 仍被跳走（或创建过程中被跳走）才失败；不点验证页上的任何东西
			if !flow.running() && !verifyRetried {
				verifyRetried = true
				flow.note("页面跳到验证账号页，重新打开 API Keys 页")
				if err := navigate(StudioKeyPath); err != nil {
					return StudioKeyResult{}, err
				}
				navigatedAt = time.Now()
				continue
			}
			return fail(StudioKeyReasonVerifyPage, nil)
		case session.atOnboarding(ctx, pageURL):
			if err := session.finishOnboarding(ctx, &onboarding); err != nil {
				return fail("AI Studio 首次使用页未能完成", errors.New(StripPageSnapshots(err.Error())))
			}
			if err := navigate(StudioKeyPath); err != nil {
				return StudioKeyResult{}, err
			}
			navigatedAt = time.Now()
			continue
		}
		now := time.Now()
		if host := studioKeyPageHost(pageURL); host != "" && host != "aistudio.google.com" {
			// 页面跳出了 AI Studio（不是登录页、组织停用页）：等一会儿，仍不回来就失败
			if offSiteSince.IsZero() {
				offSiteSince = now
			}
			if now.Sub(offSiteSince) >= studioKeyTiming.noDialog {
				return fail(StudioKeyReasonLeftPage+"（"+serviceNotAllowedPath(pageURL)+"）", nil)
			}
			_ = waitContext(ctx, studioKeyTiming.poll)
			continue
		}
		offSiteSince = time.Time{}
		if !flow.running() {
			if time.Since(navigatedAt) < studioKeyTiming.settle {
				_ = waitContext(ctx, studioKeyTiming.poll)
				continue
			}
			// 页面加载完成后仍没有 Key：记下已看到的 Key（应为空），点击页面自己的创建按钮
			for _, candidate := range keys {
				existing[candidate.Key] = true
			}
			if len(keys) > 0 {
				// 读到了 Key 只是缺项目名，前面会在 studioKeyProjectNameWait 后返回，这里不创建
				_ = waitContext(ctx, 300*time.Millisecond)
				continue
			}
			// 页面上看得到 Key（通常只显示开头或末几位），响应里却没读到：不创建，免得多建一个
			if shown, _ := client.evaluateBool(ctx, session.contextID, studioKeyShownExpression); shown {
				return fail(StudioKeyReasonUnreadable, nil)
			}
			page, inspected := session.studioKeyInspect(ctx)
			if inspected {
				flow.lastPage = &page
				switch classifyStudioKeyDialog(page.Dialog) {
				case studioKeyDialogKey:
					// 页面自己打开了创建对话框（新用户引导）：直接按对话框处理
					flow.opened(&page, now)
					flow.note("页面自己打开了创建对话框")
					continue
				case studioKeyDialogVerify:
					step := flow.decideVerifyDialog(page.Dialog, now)
					if step.Kind == studioKeyStepFail {
						return fail(step.Reason, nil)
					}
					if step.Kind == studioKeyStepClick {
						if _, err := session.pointerClick(ctx, session.contextID, studioKeyTargetExpression(step.Target)); err == nil ||
							errors.Is(err, errElementMissing) || retryablePageEvaluation(err) {
							flow.apply(step, now)
						} else {
							return fail("关闭验证账号提示失败", err)
						}
					}
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				case studioKeyDialogNone:
				default:
					// 别的对话框挡着（介绍、公告等）：点一次它自己的关闭类按钮，等一会儿；仍在时照旧点创建按钮
					if dismissedAt.IsZero() {
						dismissedAt = now
						for _, button := range page.Dialog.Buttons {
							if button.Enabled && !button.link() && studioKeyLabels.cancel.MatchString(button.Text) {
								step := studioKeyClickStep("dialog", button, "", "关闭对话框“"+button.Text+"”")
								if _, err := session.pointerClick(ctx, session.contextID, studioKeyTargetExpression(step.Target)); err == nil {
									flow.apply(step, now)
								}
								break
							}
						}
					}
					if now.Sub(dismissedAt) < studioKeyTiming.retry*2 {
						_ = waitContext(ctx, studioKeyTiming.poll)
						continue
					}
				}
			}
			attempted, err := session.pointerClick(ctx, session.contextID, studioKeyCreateButtonExpression)
			if attempted {
				var base *studioKeyPage
				if inspected {
					base = &page
				}
				flow.opened(base, now)
			}
			if err != nil {
				switch {
				case errors.Is(err, errElementMissing):
					if time.Since(navigatedAt) >= 2*studioKeyTiming.settle {
						if inspected {
							// 没有可用的“Create API key”、却有新建项目的按钮（账户还没有项目时的空状态）：先去 Projects 页新建项目
							if step, ok := flow.emptyStateStep(page); ok {
								flow.apply(step, now)
								if err := navigate(step.Path); err != nil {
									return fail("打开 AI Studio 页面失败（"+step.Path+"）", err)
								}
								navigatedAt = time.Now()
								continue
							}
						}
						if inspected && page.Create == "disabled" {
							return fail(StudioKeyReasonCreateDisabled, nil)
						}
						return fail(StudioKeyReasonNoCreate, nil)
					}
				case attempted || !retryablePageEvaluation(err):
					return fail("点击创建按钮失败", err)
				}
			}
		} else {
			page, inspected := session.studioKeyInspect(ctx)
			if inspected {
				flow.observe(page, now)
				step := flow.decide(page, now)
				switch step.Kind {
				case studioKeyStepFail:
					return fail(step.Reason, nil)
				case studioKeyStepNavigate:
					flow.apply(step, now)
					if err := navigate(step.Path); err != nil {
						return fail("打开 AI Studio 页面失败（"+step.Path+"）", err)
					}
					navigatedAt = time.Now()
				case studioKeyStepClick:
					if step.Target.Scope == "page-create" {
						// 再点“Create API key”之前像第一次那样核对：页面上已显示 Key（如新项目自带的 Key）、响应里却读不到时不创建
						if shown, _ := client.evaluateBool(ctx, session.contextID, studioKeyShownExpression); shown {
							return fail(StudioKeyReasonUnreadable, nil)
						}
					}
					attempted, err := session.pointerClick(ctx, session.contextID, studioKeyTargetExpression(step.Target))
					if attempted {
						flow.apply(step, now)
					}
					if err != nil && (attempted || !errors.Is(err, errElementMissing) && !retryablePageEvaluation(err)) {
						return fail("点击页面控件失败", err)
					}
				}
			}
			// 点了确认或“Create API key”之后等新 Key 超时（读不到页面状态时也要判，见 studioKeyFlow.expired）
			if reason := flow.expired(now); reason != "" {
				return fail(reason, nil)
			}
		}
		_ = waitContext(ctx, studioKeyTiming.poll)
	}
}

// studioKeyInspect 读一次页面状态；页面正在跳转、表达式出错时返回 false（下一轮再读）
func (session *loginSession) studioKeyInspect(ctx context.Context) (studioKeyPage, bool) {
	encoded, err := session.client.evaluateString(ctx, session.contextID, studioKeyInspectExpression)
	if err != nil || encoded == "" {
		return studioKeyPage{}, false
	}
	var page studioKeyPage
	if err := json.Unmarshal([]byte(encoded), &page); err != nil {
		return studioKeyPage{}, false
	}
	return page, true
}

// isStudioVerifyURL 判断页面是不是 AI Studio 的验证账号页（aistudio.google.com/verify-account）
func isStudioVerifyURL(pageURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(pageURL))
	return err == nil && strings.EqualFold(parsed.Hostname(), "aistudio.google.com") && strings.HasPrefix(parsed.Path, "/verify-account")
}

// studioKeyPageHost 返回 http(s) 页面的主机名（小写），其他地址（about:blank 等）为空
func studioKeyPageHost(pageURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(pageURL))
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// result 整理返回值：没有从 Key 的记录里读到项目、这次新建了一个项目时，用新项目补上；显示名缺时用响应里同一项目的显示名补上
func (flow *studioKeyFlow) result(chosen StudioKeyCandidate, count int) StudioKeyResult {
	result := StudioKeyResult{
		Key: chosen.Key, ProjectName: chosen.ProjectName, ProjectNumber: chosen.ProjectNumber, ProjectDisplay: chosen.ProjectDisplay,
		Created: flow.running(), ProjectCreated: flow.projectCreated(), KeyCount: count,
	}
	if result.Created {
		result.Steps = append([]string(nil), flow.steps...)
	}
	if result.ProjectName == "" && result.ProjectCreated && len(flow.newProjects) == 1 {
		project := flow.newProjects[0]
		result.ProjectName, result.ProjectNumber = project.Name, project.Number
		if result.ProjectDisplay == "" {
			result.ProjectDisplay = project.Display
		}
	}
	if result.ProjectDisplay == "" && result.ProjectNumber != "" {
		if project, ok := flow.project(result.ProjectNumber); ok {
			result.ProjectDisplay = project.Display
		}
	}
	return result
}

// timeoutReason 返回总时限到了时的原因（按卡在哪一步）
func (flow *studioKeyFlow) timeoutReason(now time.Time) string {
	switch {
	case !flow.keyConfirmAt.IsZero():
		return StudioKeyReasonCreateFailed
	case flow.projectStarted() && flow.projectReadyAt.IsZero():
		if flow.projectConfirmAt.IsZero() {
			return StudioKeyReasonProjectNoDialog
		}
		return StudioKeyReasonProjectTimeout
	case flow.running():
		return StudioKeyReasonFlowTimeout
	}
	return StudioKeyReasonTimeout
}

// ErrStudioKeyNone 供调用方判断“没有 Key”（页面上没有 Key，也找不到创建按钮）
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

// studioKeyCreateButtonExpression 返回页面主区域（不在对话框里）可见、可用的创建按钮（站外链接除外），没有时为 null
var studioKeyCreateButtonExpression = `(() => {` + pageDOMHelpers + studioKeyDOMHelpers + `
  return pageCreateButtons().find(buttonEnabled) || null;
})()`

// studioKeyShownExpression 判断页面主区域（不含侧栏导航与对话框）是否显示着 Key：完整或打码的 AIza…、AQ.…，或“…末四位”
const studioKeyShownExpression = `(() => {
  const main = document.querySelector('main, [role=main]') || document.body;
  if (!main) return false;
  const text = main.innerText || '';
  return /\bAIza[0-9A-Za-z_-]{0,35}|\bAQ\.[0-9A-Za-z_-]{0,}|(\.\.\.|…)[0-9A-Za-z_-]{4}\b/.test(text);
})()`
