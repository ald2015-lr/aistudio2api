package camoufoxnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gofrs/flock"
	"github.com/gorilla/websocket"
)

const aiStudioOrigin = "https://aistudio.google.com"

const generateContentPath = "/$rpc/google.internal.alkali.applications.makersuite.v1.MakerSuiteService/GenerateContent"

var publicHeaderNames = []string{
	"x-goog-api-key",
	"x-goog-authuser",
	"x-user-agent",
	"x-aistudio-visit-id",
	"x-goog-ext-519733851-bin",
	"user-agent",
}

// Worker 保存单个账户的长驻 Camoufox 与 WAA service
type Worker struct {
	mu         sync.Mutex
	process    *browserProcess
	connection *websocket.Conn
	client     *bidiClient
	contextID  string
	state      State
	closed     bool
	cacheLock  *flock.Flock
	// closing 在开始关闭时置位；stall 记录浏览器长时间无回应的原因。
	// 两者都用原子变量，Broken 读取时不需要 mu：Proof 生成期间会持有 mu 数秒，状态查询不能被它阻塞
	closing atomic.Bool
	stall   atomic.Pointer[error]
}

// Start 启动隔离 Camoufox 并完成一次官网 WAA bootstrap
func Start(ctx context.Context, options Options) (*Worker, error) {
	state, err := loadStorageState(options.StorageStatePath)
	if err != nil {
		return nil, err
	}
	if options.Model == "" {
		return nil, errors.New("WAA bootstrap 缺少实时目录聊天模型")
	}
	if options.BootstrapPrompt == "" {
		options.BootstrapPrompt = fmt.Sprintf("AIStudio2API bootstrap %d", time.Now().UnixNano())
	}
	options.reportStartup(StartupPreparingBrowser)
	ffVersion := camoufoxFirefoxMajor
	fingerprint, err := loadAccountCamoufoxConfig(options.StorageStatePath, ffVersion, options.Locale, options.Timezone)
	if err != nil {
		return nil, err
	}
	cacheDirectory, cacheLock, err := lockAccountCache(options.StorageStatePath, options.CacheExpired)
	if err != nil {
		return nil, err
	}
	if cacheDirectory != "" && options.CacheAttached != nil {
		options.CacheAttached()
	}
	options.CacheDirectory = cacheDirectory
	options.reportStartup(StartupLaunchingBrowser)
	process, endpoint, err := launchBrowser(ctx, options, fingerprint)
	if err != nil {
		var unconfirmed *browserExitUnconfirmedError
		if errors.As(err, &unconfirmed) {
			// 浏览器没能确认退出：缓存锁保留到进程退出，清理与之后的锁定都拿不到锁
			retainAccountCacheUntilExit(cacheLock, unconfirmed.exited)
			return nil, err
		}
		return nil, errors.Join(err, releaseAccountCache(cacheLock))
	}
	worker := &Worker{process: process, cacheLock: cacheLock}
	failed := true
	defer func() {
		if failed {
			_ = worker.abort()
		}
	}()
	options.reportStartup(StartupConnectingBiDi)
	dialer := websocket.Dialer{HandshakeTimeout: 30 * time.Second}
	connection, _, err := dialer.DialContext(ctx, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("连接 Camoufox BiDi: %w", err)
	}
	worker.connection = connection
	worker.client = newBiDiClient(connection)
	if err := worker.bootstrap(ctx, options, state); err != nil {
		return nil, err
	}
	failed = false
	return worker, nil
}

func (worker *Worker) abort() error {
	if worker == nil {
		return nil
	}
	worker.closing.Store(true)
	worker.mu.Lock()
	if worker.closed {
		worker.mu.Unlock()
		return nil
	}
	worker.closed = true
	connection := worker.connection
	process := worker.process
	worker.mu.Unlock()
	if connection != nil {
		_ = connection.Close()
	}
	if err := process.Close(); err != nil {
		// 没能确认浏览器退出：Start 返回后这个 Worker 不再被引用，缓存锁交给 retainAccountCacheUntilExit 保留到进程退出，
		// 启动失败后的清理与之后的锁定都拿不到锁
		retainAccountCacheUntilExit(worker.cacheLock, process.exited())
		return err
	}
	return releaseAccountCache(worker.cacheLock)
}

// ProtocolHeaders 返回官网为 GenerateContent 构造的六个公共头
func (worker *Worker) ProtocolHeaders(ctx context.Context) (http.Header, error) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if worker.closed {
		return nil, errors.New("Camoufox runtime 已关闭")
	}
	return worker.state.Headers.Clone(), nil
}

// Proof 同步官网 prompt 状态后为 SHA-256 digest 生成 fresh WAA proof
func (worker *Worker) Proof(ctx context.Context, digest string, prompt string) (string, error) {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	if worker.closed {
		return "", errors.New("Camoufox runtime 已关闭")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		value, err := worker.client.evaluateString(ctx, worker.contextID, fillPromptExpression(prompt))
		if err != nil {
			return "", worker.proofPageFailure(ctx, fmt.Errorf("同步官网 prompt: %w", err))
		}
		if normalizePromptNewlines(value) == normalizePromptNewlines(prompt) {
			break
		}
		if time.Now().After(deadline) {
			return "", worker.proofPageFailure(ctx, errors.New("官网 prompt 状态未同步"))
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return "", err
		}
	}
	proof, err := worker.client.evaluateString(ctx, worker.contextID, takeProofExpression(digest))
	if err != nil {
		return "", worker.proofPageFailure(ctx, fmt.Errorf("生成 fresh WAA proof: %w", err))
	}
	if !strings.HasPrefix(proof, "!") {
		return "", worker.proofPageFailure(ctx, errors.New("fresh WAA proof 前缀无效"))
	}
	return proof, nil
}

// proofPageFailure 为运行中 Worker 准备 proof 时的页面脚本失败追加页面快照（只在失败时多一次 evaluate）；
// 快照受请求 ctx 约束（见 withRequestPageSnapshot）：请求已取消或到期时原样返回，不再占着 mu 多等快照
func (worker *Worker) proofPageFailure(ctx context.Context, err error) error {
	return worker.client.withRequestPageSnapshot(ctx, worker.contextID, err)
}

// Broken 返回浏览器控制连接断开的原因（浏览器进程退出或连接异常）；连接正常时返回 nil
// 不获取 mu：client 只在 Start 返回前赋值一次，之后只读
func (worker *Worker) Broken() error {
	if worker.closing.Load() {
		return nil
	}
	if stalled := worker.stall.Load(); stalled != nil {
		return *stalled
	}
	if worker.client == nil {
		return nil
	}
	return worker.client.broken()
}

// markStalled 记录浏览器长时间无回应，只保留第一次的原因
func (worker *Worker) markStalled(err error) {
	worker.stall.CompareAndSwap(nil, &err)
}

// State 返回 runtime 的不可变状态副本
func (worker *Worker) State() State {
	worker.mu.Lock()
	defer worker.mu.Unlock()
	state := worker.state
	state.Headers = state.Headers.Clone()
	return state
}

// Close 结束 BiDi session 并清理 Camoufox profile
func (worker *Worker) Close() error {
	if worker == nil {
		return nil
	}
	worker.closing.Store(true)
	worker.mu.Lock()
	if worker.closed {
		worker.mu.Unlock()
		return nil
	}
	client := worker.client
	connection := worker.connection
	process := worker.process
	worker.mu.Unlock()
	if client != nil && connection != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, _ = client.command(closeCtx, "session.end", map[string]any{})
		cancel()
		_ = connection.Close()
	}
	if err := process.Close(); err != nil {
		// 没能确认浏览器退出：缓存锁保留到进程退出（调用方之后可能不再引用这个 Worker）；关闭重试成功时照常释放
		retainAccountCacheUntilExit(worker.cacheLock, process.exited())
		return err
	}
	if err := releaseAccountCache(worker.cacheLock); err != nil {
		return err
	}
	worker.mu.Lock()
	worker.closed = true
	worker.mu.Unlock()
	return nil
}

func (worker *Worker) bootstrap(ctx context.Context, options Options, storage storageState) error {
	client := worker.client
	if _, err := client.command(ctx, "session.new", map[string]any{"capabilities": map[string]any{}}); err != nil {
		return err
	}
	tree, err := client.command(ctx, "browsingContext.getTree", map[string]any{"maxDepth": 0})
	if err != nil {
		return err
	}
	contexts, _ := tree["contexts"].([]any)
	if len(contexts) == 0 {
		return errors.New("Camoufox BiDi 未返回初始 tab")
	}
	root, _ := contexts[0].(map[string]any)
	contextID, _ := root["context"].(string)
	if contextID == "" {
		return errors.New("Camoufox BiDi 初始 tab 无效")
	}
	worker.contextID = contextID
	if err := client.installLocalStorage(ctx, contextID, storage.Origins); err != nil {
		return err
	}
	if err := client.installCookies(ctx, storage.Cookies); err != nil {
		return err
	}
	target := aiStudioOrigin + "/prompts/new_chat?model=" + url.QueryEscape(options.Model)
	if options.TemporaryChat {
		target += "&temporary=true"
	}
	// 从导航到填好提示词（session.subscribe 之前）只有页面 window 上的状态，停在首次使用页时完成首次使用、重新导航后整段重做是安全的。
	// 等到页面就绪时就在首次使用页，或这一段任何一步失败时页面在首次使用页（输入框出现后 1～2 秒才被 SPA 路由过去），
	// 都完成首次使用后从重新导航开始重做（两种情形 openPromptPage 都返回 errOnboardingPage）；每次启动最多完成 onboardingCompletionLimit 次，
	// 之后仍回到首次使用页时以 OnboardingFailedPrefix 失败
	var onboarding onboardingBudget
	pageURL, snapshotKey, err := worker.openPromptPage(ctx, options, target)
	for err != nil {
		if !errors.Is(err, errOnboardingPage) {
			return err
		}
		if _, err := worker.finishOnboarding(ctx, options, &onboarding); err != nil {
			return err
		}
		pageURL, snapshotKey, err = worker.openPromptPage(ctx, options, target)
	}
	if _, err := client.command(ctx, "session.subscribe", map[string]any{
		"events":   []string{"network.beforeRequestSent"},
		"contexts": []string{contextID},
	}); err != nil {
		return err
	}
	intercept, err := client.command(ctx, "network.addIntercept", map[string]any{
		"phases":   []string{"beforeRequestSent"},
		"contexts": []string{contextID},
		"urlPatterns": []map[string]any{{
			"type":     "pattern",
			"protocol": "https",
			"hostname": "alkalimakersuite-pa.clients6.google.com",
			"pathname": generateContentPath,
		}},
	})
	if err != nil {
		return fmt.Errorf("安装 GenerateContent 拦截: %w", err)
	}
	interceptID, _ := intercept["intercept"].(string)
	if interceptID == "" {
		return errors.New("GenerateContent 拦截 ID 无效")
	}
	// 页面类失败（等待页面条件、执行页面脚本）在原错误末尾追加页面快照（见 withPageSnapshot），网络拦截与订阅命令的失败不追加。
	// 订阅与拦截之后不再重做（会重复订阅与拦截）：失败时页面停在首次使用页就就地完成、照常返回失败（见 interceptedPageFailure）
	pageFailure := func(err error) error {
		return worker.interceptedPageFailure(ctx, options, &onboarding, err)
	}
	if err := client.waitFor(ctx, contextID, runButtonEnabledExpression, 15*time.Second); err != nil {
		return pageFailure(fmt.Errorf("等待官网 Run 按钮启用: %w", err))
	}
	if _, err := client.evaluate(ctx, contextID, submitPromptExpression); err != nil {
		return pageFailure(fmt.Errorf("提交官网提示词: %w", err))
	}
	if err := client.waitFor(ctx, contextID, "Boolean(window.__aistudioWaaService)", 60*time.Second); err != nil {
		return pageFailure(fmt.Errorf("官网 WAA service 未暴露: %w", err))
	}
	requestID, err := client.waitBlockedGenerateRequest(ctx, contextID, 60*time.Second)
	if err != nil {
		return pageFailure(err)
	}
	if _, err := client.command(ctx, "network.failRequest", map[string]any{"request": requestID}); err != nil {
		return fmt.Errorf("终止 bootstrap GenerateContent: %w", err)
	}
	if _, err := client.command(ctx, "network.removeIntercept", map[string]any{"intercept": interceptID}); err != nil {
		return fmt.Errorf("移除 GenerateContent 拦截: %w", err)
	}
	actualModel, err := capturedBootstrapModel(ctx, client, contextID)
	if err != nil {
		return pageFailure(err)
	}
	if actualModel != strings.TrimPrefix(options.Model, "models/") {
		return fmt.Errorf("官网初始化页面模型不匹配 expected=%s actual=%s", options.Model, actualModel)
	}
	restored, err := client.evaluateBool(ctx, contextID, `(() => {
  if (typeof window.__aistudioRestoreBootstrapCapture !== 'function') return false;
  window.__aistudioRestoreBootstrapCapture();
  delete window.__aistudioRestoreBootstrapCapture;
  return true;
})()`)
	if err != nil {
		return pageFailure(fmt.Errorf("移除 bootstrap 请求捕获: %w", err))
	}
	if !restored {
		// 捕获装在 window 上，页面中途刷新或跳转后就不在了，快照的 cap、hook 与 nav 可以印证
		return pageFailure(errors.New("bootstrap 请求捕获未安装"))
	}
	headers := make(http.Header, len(publicHeaderNames))
	for _, name := range publicHeaderNames {
		value := client.generateHeader(name)
		if value != "" {
			headers.Set(name, value)
		}
	}
	for _, name := range []string{"user-agent", "x-goog-api-key", "x-goog-authuser", "x-user-agent"} {
		if headers.Get(name) == "" {
			return fmt.Errorf("官网 GenerateContent 缺少必要公共头 %s", name)
		}
	}
	if _, err := client.command(ctx, "session.unsubscribe", map[string]any{
		"events":   []string{"network.beforeRequestSent"},
		"contexts": []string{contextID},
	}); err != nil {
		return fmt.Errorf("停止 GenerateContent 网络事件订阅: %w", err)
	}
	userAgent, _ := client.evaluateString(ctx, contextID, "navigator.userAgent")
	platform, _ := client.evaluateString(ctx, contextID, "navigator.platform")
	timezone, _ := client.evaluateString(ctx, contextID, "Intl.DateTimeFormat().resolvedOptions().timeZone")
	worker.state = State{
		PID:         worker.process.command.Process.Pid,
		PageURL:     pageURL,
		UserAgent:   userAgent,
		Platform:    platform,
		Timezone:    timezone,
		SnapshotKey: snapshotKey,
		Headers:     headers,
	}
	return nil
}

// openPromptPage 导航到 target，等页面就绪后定位 WAA 并填好 bootstrap 提示词（启动 5/7 到 7/7 填写提示词，session.subscribe 之前），
// 返回页面地址与官网 snapshot 函数的键。等到页面就绪时停在首次使用页，或从等就绪起任何一步的页面类失败发生时页面在首次使用页，
// 都返回 errOnboardingPage（不采快照，由 bootstrap 完成首次使用后重做）；停在组织停用页（access.workspace.google.com）时
// 立即以 ServiceNotAllowedError 失败（等就绪时立刻结束等待，不再等满 120 秒），失败处读到的地址或快照在这一页时同样如此；
// 其余页面类失败与原来一样追加页面快照。失败处的识别与快照共用一个短期限（见 startupFailureDiagnosis），浏览器卡住时与原来一样
// 最多多等 pageSnapshotTimeout
func (worker *Worker) openPromptPage(ctx context.Context, options Options, target string) (string, string, error) {
	client := worker.client
	contextID := worker.contextID
	// pageFailure 为页面类失败的收尾：在首次使用页时返回 errOnboardingPage，在组织停用页时返回 ServiceNotAllowedError，否则同 withPageSnapshot
	pageFailure := func(err error) error {
		diagnosis := client.startupFailureDiagnosis(ctx, contextID, false)
		if diagnosis.onboarding {
			return errOnboardingPage
		}
		if blocked := diagnosis.serviceNotAllowed(); blocked != nil {
			return blocked
		}
		if diagnosis.suffix == "" {
			return err
		}
		return fmt.Errorf("%w%s", err, diagnosis.suffix)
	}
	options.reportStartup(StartupLoadingAIStudio)
	// 记下导航时刻：失败时快照的 since 与页面的 age 对照，看导航之后文档是否被替换过（刷新、跳转）
	client.markNavigated()
	if _, err := client.command(ctx, "browsingContext.navigate", map[string]any{
		"context": contextID,
		"url":     target,
		"wait":    "interactive",
	}); err != nil && !strings.Contains(err.Error(), "NS_ERROR_ABORT") {
		return "", "", fmt.Errorf("导航 AI Studio: %w", err)
	}
	if err := client.waitFor(ctx, contextID, workerPageReadyExpression, 120*time.Second); err != nil {
		// 初始化 ctx 到期时 location.href 也取不到（url 曾因此为空）：地址与页面快照改用同一个独立的短上下文读取
		diagnosis := client.startupFailureDiagnosis(ctx, contextID, true)
		if diagnosis.onboarding {
			return "", "", errOnboardingPage
		}
		if blocked := diagnosis.serviceNotAllowed(); blocked != nil {
			return "", "", blocked
		}
		return "", "", fmt.Errorf("AI Studio 输入框未就绪 url=%s: %w%s", diagnosis.pageURL, err, diagnosis.suffix)
	}
	pageURL, err := client.evaluateString(ctx, contextID, "location.href")
	if err != nil {
		return "", "", pageFailure(err)
	}
	// 组织停用页的地址里带回跳地址（continue=https://aistudio.google.com/…），先于登录页判断
	if isServiceNotAllowedURL(pageURL) {
		return "", "", client.serviceNotAllowed(ctx, contextID, pageURL)
	}
	if strings.Contains(pageURL, "accounts.google.com") {
		return "", "", fmt.Errorf("隔离登录态失效 url=%s", pageURL)
	}
	if client.onboardingPage(ctx, contextID) {
		return "", "", errOnboardingPage
	}
	if err := dismissKnownOverlays(ctx, client, contextID); err != nil {
		return "", "", pageFailure(err)
	}
	options.reportStartup(StartupLocatingWAA)
	snapshotKey, err := client.waitSnapshotFunction(ctx, contextID, 30*time.Second)
	if err != nil {
		return "", "", pageFailure(err)
	}
	if err := installBootstrapRequestCapture(ctx, client, contextID); err != nil {
		return "", "", pageFailure(err)
	}
	options.reportStartup(StartupBootstrappingWAA)
	filled, err := client.evaluateString(ctx, contextID, fillPromptExpression(options.BootstrapPrompt))
	if err != nil || normalizePromptNewlines(filled) != normalizePromptNewlines(options.BootstrapPrompt) {
		diagnosis := client.startupFailureDiagnosis(ctx, contextID, false)
		if diagnosis.onboarding {
			return "", "", errOnboardingPage
		}
		if blocked := diagnosis.serviceNotAllowed(); blocked != nil {
			return "", "", blocked
		}
		return "", "", fmt.Errorf("填写 bootstrap 提示词失败 value=%q err=%v%s", filled, err, diagnosis.suffix)
	}
	return pageURL, snapshotKey, nil
}

// finishOnboarding 自动完成首次使用页一次（受 budget 限制），勾选或点击过时导出完成后的 localStorage（见 OnboardingResult.LocalStorage）
// 并调用 Options.OnboardingCompleted。返回这次的结果：页面自己离开首次使用页（什么都没点）时 acted 为假
func (worker *Worker) finishOnboarding(ctx context.Context, options Options, budget *onboardingBudget) (OnboardingResult, error) {
	result, err := budget.finish(ctx, worker.client, worker.contextID)
	if err != nil {
		return result, err
	}
	if result.acted() && options.OnboardingCompleted != nil {
		result.LocalStorage = worker.client.onboardingLocalStorage(ctx, worker.contextID)
		options.OnboardingCompleted(result)
	}
	return result, nil
}

// interceptedPageFailure 处理订阅网络事件与安装拦截之后（等 Run、提交、等 WAA service、等拦截请求、读取与移除捕获）的页面类失败：
// 页面不在首次使用页时与原来一样追加页面快照（识别与快照共用一个短期限，见 startupFailureDiagnosis）。在首次使用页时先采快照
// （失败那一刻的首次使用页），再就地完成首次使用（账号以后能起；这里不重做 bootstrap，重做会重复订阅与拦截），照常返回失败：
// 原错误文字不变，末尾补一句“；已完成首次使用页”（勾选或点击过）、“；首次使用页已自行离开”（识别之后页面自己离开了、什么都没点，
// 也不写完成日志）或“；首次使用页未能完成: <原因>”（再接进度记录“ | 已做=…”），再接快照与结构摘要
func (worker *Worker) interceptedPageFailure(ctx context.Context, options Options, budget *onboardingBudget, err error) error {
	client := worker.client
	diagnosis := client.startupFailureDiagnosis(ctx, worker.contextID, false)
	if !diagnosis.onboarding {
		// 失败时页面已被跳到组织停用页：原因是账号所属组织停用了 AI Studio，按 ServiceNotAllowedError 返回（附同一份快照）
		if blocked := diagnosis.serviceNotAllowed(); blocked != nil {
			return blocked
		}
		if diagnosis.suffix == "" {
			return err
		}
		return fmt.Errorf("%w%s", err, diagnosis.suffix)
	}
	_, snapshot := client.pageDiagnosis(ctx, worker.contextID, false)
	note, progress, structure := "；已完成首次使用页", "", ""
	result, finishErr := worker.finishOnboarding(ctx, options, budget)
	switch {
	case errors.Is(finishErr, ErrServiceNotAllowed):
		// 完成首次使用途中被跳到组织停用页：原因是账号所属组织停用了 AI Studio
		return finishErr
	case finishErr != nil:
		note = "；首次使用页未能完成"
		var failure *onboardingFailure
		if errors.As(finishErr, &failure) {
			note += ": " + failure.reason
			progress = failure.progress
			structure = failure.structure
		}
	case !result.acted():
		note = "；首次使用页已自行离开"
	}
	return &onboardingInterrupted{err: err, text: err.Error() + note + progress + snapshot + structure}
}

func installBootstrapRequestCapture(ctx context.Context, client *bidiClient, contextID string) error {
	encodedPath, err := json.Marshal(generateContentPath)
	if err != nil {
		return err
	}
	expression := fmt.Sprintf(`(() => {
  const targetPath = %s;
  const matches = (input) => {
    const raw = typeof input === 'string' ? input : input?.url;
    if (!raw) return false;
    try { return new URL(raw, location.href).pathname === targetPath; } catch { return false; }
  };
  const record = (body) => {
    if (typeof body === 'string') {
      window.__aistudioBootstrapRequestBody = body;
      return;
    }
    if (body instanceof Blob) {
      body.text().then(record);
      return;
    }
    if (body instanceof ArrayBuffer) {
      record(new TextDecoder().decode(body));
      return;
    }
    if (ArrayBuffer.isView(body)) record(new TextDecoder().decode(body));
  };
  const originalFetch = window.fetch;
  const fetchWrapper = function(input, init) {
    if (matches(input)) {
      const body = init?.body;
      if (body !== undefined) record(body);
      else if (input instanceof Request) input.clone().text().then(record);
    }
    return originalFetch.apply(this, arguments);
  };
  const originalOpen = XMLHttpRequest.prototype.open;
  const originalSend = XMLHttpRequest.prototype.send;
  const requestURLs = new WeakMap();
  const openWrapper = function(method, url) {
    requestURLs.set(this, String(url));
    return originalOpen.apply(this, arguments);
  };
  const sendWrapper = function(body) {
    if (matches(requestURLs.get(this))) record(body);
    return originalSend.apply(this, arguments);
  };
  window.fetch = fetchWrapper;
  XMLHttpRequest.prototype.open = openWrapper;
  XMLHttpRequest.prototype.send = sendWrapper;
  window.__aistudioRestoreBootstrapCapture = () => {
    if (window.fetch === fetchWrapper) window.fetch = originalFetch;
    if (XMLHttpRequest.prototype.open === openWrapper) XMLHttpRequest.prototype.open = originalOpen;
    if (XMLHttpRequest.prototype.send === sendWrapper) XMLHttpRequest.prototype.send = originalSend;
  };
  return true;
})()`, encodedPath)
	installed, err := client.evaluateBool(ctx, contextID, expression)
	if err != nil {
		return fmt.Errorf("安装 bootstrap 请求捕获: %w", err)
	}
	if !installed {
		return errors.New("安装 bootstrap 请求捕获失败")
	}
	return nil
}

func capturedBootstrapModel(ctx context.Context, client *bidiClient, contextID string) (string, error) {
	if err := client.waitFor(ctx, contextID, "typeof window.__aistudioBootstrapRequestBody === 'string'", 5*time.Second); err != nil {
		return "", fmt.Errorf("官网 bootstrap 请求正文未捕获: %w", err)
	}
	body, err := client.evaluateString(ctx, contextID, `(() => {
  window.__aistudioRestoreBootstrapCapture?.();
  return window.__aistudioBootstrapRequestBody;
})()`)
	if err != nil {
		return "", err
	}
	var wire []any
	if err := json.Unmarshal([]byte(body), &wire); err != nil {
		return "", fmt.Errorf("解析官网 bootstrap 请求正文: %w", err)
	}
	if len(wire) == 0 {
		return "", errors.New("官网 bootstrap 请求缺少模型")
	}
	model, _ := wire[0].(string)
	model = strings.TrimPrefix(strings.TrimSpace(model), "models/")
	if model == "" {
		return "", errors.New("官网 bootstrap 请求模型无效")
	}
	return model, nil
}

func loadStorageState(path string) (storageState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return storageState{}, fmt.Errorf("读取 storage state: %w", err)
	}
	var state storageState
	if err := json.Unmarshal(data, &state); err != nil {
		return storageState{}, fmt.Errorf("解析 storage state: %w", err)
	}
	if len(state.Cookies) == 0 {
		return storageState{}, errors.New("storage state 没有 Cookie")
	}
	return state, nil
}
