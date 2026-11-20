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
// + network.getData），从响应正文里解析出候选 Key 与项目（见 ExtractStudioKeys、ExtractStudioProjects）。本流程不手工构造任何发往 Google
// 的请求。
//
// 页面自己的公共 Key 不是账户的 Key：AI Studio 的每个页面都把一条公共的浏览器 Key 写在 HTML 配置里，并以 X-Goog-Api-Key（或 ?key=）
// 发给自己的接口。以前按文本在 HTML 里扫到它，每个账户都“读到”同一条 Key。现在（见 studio_key_confirm.go）：
//   - 任何请求的地址与请求头里像 Key 的字符串、页面文档与脚本正文里像 Key 的字符串都进这次运行的拒绝集合，从不选用；
//   - 只有页面接口的 JSON 类正文才找候选，而且只认整段是 Key 的字符串；
//   - 候选要与页面主区域 Key 列表里的某一行对上（末四位相同；页面显示完整 Key 时完整相等）才算这个账户的 Key。列表里有行、响应里
//     却没有对得上的完整 Key 时，用真实指针点这一行自己的 Key 名称打开详情（每次运行最多一次），从点开后新到的响应或详情对话框里
//     读出末四位相同的完整 Key，再用对话框自己的关闭按钮关掉；行里的复制、删除等控件一律不点；
//   - 认不出 Key 列表（既没有行、也不是页面自己的空状态）时失败，不拿响应里的 Key 充数；
//   - 调用方给的排除的 Key（其他账户保存着的、已作废的，见 StudioKeyOptions.Exclude）同样从不选用；认出的页面公共 Key 以 SHA-256 指纹
//     随结果与错误交回（ClientFingerprints），调用方据此作废旧版本误记的记录。
//
// 页面里读完整 Key 与 Key 列表都按文本节点找（见 textRuns、keyTexts）：真实浏览器里相邻的行内元素之间没有空白时 innerText 会把 Key 与
// 旁边复制图标的文字、前面的标签连成一串，对整段 innerText 用正则会读错（把 content_copy 吞进 AQ. Key）或读不到。创建结果对话框里读到的
// Key 还要有新出现的一行或响应印证才认。
//
// 流程：
//   - 页面里已有 Key：只读取，不创建；读不到完整 Key 时失败，不创建；
//   - 页面是空状态（没有 Key）：用真实指针点击页面自己的“Create API key”按钮，再按对话框的状态一步步点页面自己的控件
//     （见 studioKeyFlow：有项目时选一个，没有项目时经页面自己的新建项目入口建一个、等它出现），最后点创建 Key 的确认。
//     页面上没有可用的“Create API key”、却有新建项目的按钮时（没有项目的空状态），先去 Projects 页新建项目，回来等 Key 列表
//     加载后再点（新项目自带 Key 时直接读取）。每次点“Create API key”之前都核对页面上是否已有 Key（有就不创建），点了之后要先见到
//     对话框才会再点。创建之后只认新出现的 Key：创建结果对话框里显示的，或新出现的一行与响应里末四位对上的（都没有时打开新行的详情）。
//     每次运行最多新建一个项目、一个 Key，从不删除任何东西，不点站外链接。
//   - 页面上有验证账号的提示（需要验证账号的账户）不影响：只要 API Keys 页面可用就照常读取或创建；页面被跳到验证账号页时
//     重新打开一次，仍被跳走才失败。
//   - 只读（StudioKeyOptions.ReadOnly，补项目信息）：从不点“Create API key”、不新建项目、不点创建流程里的控件，只在 Key 列表里找
//     账户已保存的那条（末四位所在的行，完整 Key 要相等）；找不到时失败（StudioKeyReasonSavedMissing），不创建。
//   - 选定的 Key 在响应里没有项目编号时，依次与这次所有响应里的项目对照、读 Key 列表里这一行（只读）、打开这条 Key 自己的
//     “API key details”对话框读出项目名与项目编号后用它自己的关闭按钮关掉（每次运行最多点开一次，见 studio_key_project.go）；
//     都没有时照样返回 Key，步骤里记一笔，不算失败。
//
// Key 是机密：不写日志，错误文案里任何像 Key 的片段都打码（RedactStudioKeys），页面文字读出来之前就把 Key 换成 <key>；诊断只写末四位。

// StudioKeyPath 为 API Keys 页面的路径
const StudioKeyPath = "/apikey"

// StudioKeyMinTimeout 为整次获取（启动浏览器到读到 Key）的最短时限：调用方给的时限（驱动的 REQUEST_TIMEOUT）更短时按它，
// 免得没有项目的账户在建好项目之后、建 Key 之前超时（新建项目再创建 Key 最长约 4 分钟）
const StudioKeyMinTimeout = 5 * time.Minute

// studioKeyMaxBody 为单个响应正文收集的上限（字节）
const studioKeyMaxBody = 8 << 20

// 失败原因（StudioKeyError.Reason），写进管理页与运行日志，不含 Key；创建流程各步卡住时的原因见 studio_key_flow.go
const (
	StudioKeyReasonLoginExpired = "AI Studio 登录已失效，请重新登录"
	StudioKeyReasonOrgDisabled  = ServiceNotAllowedPrefix
	StudioKeyReasonNoCreate     = "页面上没有 Key，也没有找到“Create API key”按钮"
	StudioKeyReasonCreateFailed = "已点击创建，但没有在页面响应里读到新 Key"
	StudioKeyReasonDialog       = "创建 Key 的对话框里没有找到可点的下一步"
	StudioKeyReasonCollector    = "浏览器不支持读取页面响应正文"
	StudioKeyReasonUnreadable   = "页面上已有 Key，但响应里没有对得上的完整 Key，打开这条 Key 的详情也没有读到，没有创建新 Key"
	StudioKeyReasonTimeout      = "获取 API Key 超时"
	StudioKeyReasonFlowTimeout  = "获取 API Key 超时（创建流程没有走完）"
	StudioKeyReasonSavedMissing = "页面上没有找到已保存的 Key（可能已在官网删除），没有创建新 Key"
	// StudioKeyReasonListUnknown 为认不出 Key 列表：不拿响应里的 Key 充数
	StudioKeyReasonListUnknown = "页面上认不出 Key 列表，无法确认响应里的 Key 属于这个账户，没有保存，也没有创建"
	// StudioKeyReasonSavedIsClient 为只读时已保存的 Key 是页面自己的公共 Key
	StudioKeyReasonSavedIsClient = "已保存的 Key 是页面自己的公共 Key（不是这个账户的），没有可补的项目信息；请重新获取这个账户的 Key"
	// StudioKeyReasonDetailsBlocked 为要打开 Key 详情时页面上一直有对话框挡着
	StudioKeyReasonDetailsBlocked = "页面上已有 Key，要打开它的详情读完整 Key 时一直有对话框挡着，没有创建新 Key"
	// StudioKeyReasonCreatedUnreadable 为创建后新出现的一行读不到完整 Key
	StudioKeyReasonCreatedUnreadable = "已创建 Key，Key 列表里也出现了新的一行，但响应、创建结果与这条 Key 的详情里都没有读到它的完整 Key"
	// StudioKeyReasonExcluded 为页面上的 Key 都是排除的（StudioKeyOptions.Exclude：其他账户保存着的，或已作废的）
	StudioKeyReasonExcluded = "页面上的 Key 都已保存在其他账户里或已作废（不是这个账户自己的），没有可用的 Key，没有创建新 Key"
)

// StudioKeyOptions 为一次获取的方式
type StudioKeyOptions struct {
	// ReadOnly 为只读（补项目信息）：从不点“Create API key”、不新建项目、不点创建流程里的任何控件，只在页面的 Key 里找 SavedKey
	// （完整相等）并读它的项目；找不到时以 StudioKeyReasonSavedMissing 失败，不创建
	ReadOnly bool
	// SavedKey 为账户已保存的 Key（机密，不写日志）：这次没有创建、页面上有多条 Key 时优先选它所在的行；ReadOnly 时必须是完整的 Key。
	// 它在这次的拒绝集合里（页面自己的公共 Key）或排除的 Key 里时不用
	SavedKey string
	// Exclude 为不是这个账户的 Key（机密，不写日志）：其他账户保存着的 Key，以及这个账户已作废的 Key。ExcludeFingerprints 为只剩
	// SHA-256 指纹的作废 Key（见 StudioKeyFingerprint）。排除的 Key 从不选用：选行时跳过末四位与 Exclude 里某条相同的行（行上显示着
	// 完整 Key 时按完整 Key 判断），响应、详情与创建结果里读到排除的 Key 也不用；页面上的 Key 都被排除时以 StudioKeyReasonExcluded
	// 失败（不创建）。只读时不按末四位跳过行（只找 SavedKey 那一行）
	Exclude             []string
	ExcludeFingerprints []string
}

// StudioKeyResult 为一次获取的结果。Key 是机密，调用方只能把它写进 0600 的文件，不能写日志
type StudioKeyResult struct {
	Key            string
	ProjectName    string
	ProjectNumber  string
	ProjectDisplay string
	// ProjectID 为项目 ID（如 gen-lang-client-0123456789，尽力而为，可为空）
	ProjectID string
	// Created 表示这次通过页面的创建流程新建了 Key
	Created bool
	// ProjectCreated 表示这次先经页面新建了一个 Google Cloud 项目
	ProjectCreated bool
	// KeyCount 为页面 Key 列表里的行数
	KeyCount int
	// Steps 为创建流程做过的步骤与补项目信息的记录（页面文字已打码、截短，不含 Key），都没有时为空
	Steps []string
	// ClientFingerprints 为这次认出的页面自己的公共 Key 的 SHA-256 指纹（见 StudioKeyFingerprint；拒绝集合里来源一定是页面自己的，
	// 最多 studioKeyClientLimit 条）：调用方据此作废保存着它们的记录
	ClientFingerprints []string
}

// StudioKeyError 为获取失败；Reason 不含 Key（已打码），Detail 为接在后面的步骤记录、对话框摘要与诊断（打码、截短，只有末四位，可为空）
type StudioKeyError struct {
	Reason string
	Detail string
	Cause  error
	// SavedIsClient 表示 StudioKeyOptions.SavedKey 是页面自己的公共 Key（写在页面文档或脚本里，或页面发给 AI Studio 自己接口的请求
	// 带着它）：调用方不应再把它当作账户的 Key 保留
	SavedIsClient bool
	// ClientFingerprints 同 StudioKeyResult.ClientFingerprints（失败之前认出的）
	ClientFingerprints []string
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

// FetchStudioKey 用已有登录态打开 API Keys 页面读取（必要时创建；keyOptions.ReadOnly 时只读）API Key；整次的时限为 options.Timeout，
// 不短于 StudioKeyMinTimeout
func FetchStudioKey(ctx context.Context, options LoginOptions, storageStateJSON []byte, keyOptions StudioKeyOptions) (result StudioKeyResult, err error) {
	options, err = validateLoginOptions(options)
	if err != nil {
		return StudioKeyResult{}, err
	}
	if keyOptions.ReadOnly && !IsStudioKey(keyOptions.SavedKey) {
		return StudioKeyResult{}, errors.New("只读获取需要账户已保存的完整 Key")
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
	return session.fetchStudioKey(fetchCtx, keyOptions)
}

// studioKeyTimeout 返回整次获取的时限：timeout 与 StudioKeyMinTimeout 中较大者
func studioKeyTimeout(timeout time.Duration) time.Duration {
	return max(timeout, StudioKeyMinTimeout)
}

// studioKeyResponse 为一条要读正文的响应（只记类型与来源，不含正文、不含地址参数）
type studioKeyResponse struct {
	id   string
	host string
	// name 为地址的最后一段（RPC 方法名，如 ListCloudApiKeys；batchexecute 为 rpcids），只留像名字的字符，诊断用
	name        string
	mime        string
	document    bool
	destination string
}

// studioKeyDenySource 为拒绝集合里一条 Key 的来源（诊断用，不含 Key）。strong 表示一定是页面自己的 Key：写在页面文档或脚本里，
// 或页面发给 AI Studio 自己接口（aistudio.google.com、MakerSuite）的请求带着它
type studioKeyDenySource struct {
	where  string
	strong bool
}

// studioKeyDenied 为新进拒绝集合的一条
type studioKeyDenied struct {
	key    string
	source studioKeyDenySource
}

// studioKeyDenyLimit 为一次运行里拒绝集合的上限
const studioKeyDenyLimit = 256

// studioKeyListener 由后台读取协程在每个 network.responseCompleted 上调用（只读已收到的事件，不改订阅、不拦截）：
//   - 任何域名的请求：地址（含 ?key= 与 webchannel 的 $httpHeaders）与请求头（如 X-Goog-Api-Key；Cookie 除外）里像 Key 的字符串
//     都是页面自己发请求用的 Key（页面的公共 Key），记进拒绝集合；
//   - Google 相关域名（见 studioKeyResponseHost）的响应记下请求 ID 与类型，由 drain 读正文。
type studioKeyListener struct {
	mu       sync.Mutex
	requests []studioKeyResponse
	denied   []studioKeyDenied
	// deniedSet 记下已交出的 Key 与来源是否“一定是页面自己的”（同一条 Key 后来有更强的来源时再交一次）
	deniedSet map[string]bool
}

// studioKeyHeaderLimit 为请求头值与地址扫描的上限（字节）：钩子跑在 BiDi 读取协程里，必须便宜
const studioKeyHeaderLimit = 8 << 10

func (listener *studioKeyListener) observe(params map[string]any) {
	request, _ := params["request"].(map[string]any)
	requestID, _ := request["request"].(string)
	rawURL, _ := request["url"].(string)
	if len(rawURL) > 2*studioKeyHeaderLimit {
		// data: 之类的长地址只看开头（钩子跑在读取协程里，不解析整段）
		rawURL = rawURL[:2*studioKeyHeaderLimit]
	}
	host := studioKeyURLHost(rawURL)
	found := studioKeyRequestKeys(rawURL, host, request["headers"])
	var meta *studioKeyResponse
	if requestID != "" && studioKeyResponseHost(rawURL) {
		response, _ := params["response"].(map[string]any)
		navigation, _ := params["navigation"].(string)
		destination, _ := request["destination"].(string)
		meta = &studioKeyResponse{id: requestID, host: host, name: studioKeyRequestName(rawURL), mime: studioKeyResponseMime(response),
			document: navigation != "", destination: strings.ToLower(destination)}
	}
	if len(found) == 0 && meta == nil {
		return
	}
	listener.mu.Lock()
	defer listener.mu.Unlock()
	for _, item := range found {
		listener.deny(item)
	}
	if meta != nil && len(listener.requests) < 4096 {
		listener.requests = append(listener.requests, *meta)
	}
}

func (listener *studioKeyListener) deny(item studioKeyDenied) {
	if listener.deniedSet == nil {
		listener.deniedSet = make(map[string]bool)
	}
	strong, seen := listener.deniedSet[item.key]
	if seen && (strong || !item.source.strong) || !seen && len(listener.deniedSet) >= studioKeyDenyLimit {
		return
	}
	listener.deniedSet[item.key] = item.source.strong
	listener.denied = append(listener.denied, item)
}

// take 取出新到的响应与新进拒绝集合的 Key
func (listener *studioKeyListener) take() ([]studioKeyResponse, []studioKeyDenied) {
	listener.mu.Lock()
	defer listener.mu.Unlock()
	requests, denied := listener.requests, listener.denied
	listener.requests, listener.denied = nil, nil
	return requests, denied
}

// studioKeyRequestKeys 返回请求地址与请求头里像 Key 的字符串与来源（地址另按 URL 解码后再扫一次：webchannel 把请求头编码在地址参数里）
func studioKeyRequestKeys(rawURL string, host string, headers any) []studioKeyDenied {
	var found []studioKeyDenied
	seen := map[string]bool{}
	strong := studioKeyStrongHost(host)
	add := func(text string, where string) {
		if len(text) > studioKeyHeaderLimit {
			text = text[:studioKeyHeaderLimit]
		}
		if !strings.Contains(text, "AIza") && !strings.Contains(text, "AQ.") {
			return
		}
		for _, key := range scanStudioKeyStrings(text) {
			if !seen[key] {
				seen[key] = true
				found = append(found, studioKeyDenied{key: key, source: studioKeyDenySource{where: where + "（" + host + "）", strong: strong}})
			}
		}
	}
	if rawURL != "" {
		add(rawURL, "请求地址")
		if decoded, err := url.QueryUnescape(rawURL); err == nil && decoded != rawURL {
			add(decoded, "请求地址")
		}
	}
	items, _ := headers.([]any)
	for _, raw := range items {
		header, _ := raw.(map[string]any)
		name, _ := header["name"].(string)
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || name == "cookie" {
			continue
		}
		add(studioKeyHeaderValue(header["value"]), "请求头 "+studioKeySafeName(name))
	}
	return found
}

// studioKeyHeaderValue 返回 BiDi 请求头的值（{type: string|base64, value}），base64 的先解码；解不开时为空
func studioKeyHeaderValue(value any) string {
	item, _ := value.(map[string]any)
	kind, _ := item["type"].(string)
	raw, _ := item["value"].(string)
	if kind != "base64" {
		return raw
	}
	if len(raw) > 2*studioKeyHeaderLimit {
		return ""
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return ""
	}
	return string(decoded)
}

// studioKeyStrongHost 判断请求是发给 AI Studio 自己的（aistudio.google.com、MakerSuite 接口）：这类请求带的 Key 一定是页面自己的
func studioKeyStrongHost(host string) bool {
	return host == "aistudio.google.com" || strings.Contains(host, "makersuite")
}

// studioKeyURLHost 返回地址的主机名（小写），解析不了时为空
func studioKeyURLHost(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Hostname())
}

// studioKeyRequestName 返回地址的最后一段（batchexecute 取 rpcids），只留像名字的字符，诊断用；像 Key 的换成 <key>
func studioKeyRequestName(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	name := parsed.Path
	if index := strings.LastIndex(strings.TrimRight(name, "/"), "/"); index >= 0 {
		name = strings.TrimRight(name, "/")[index+1:]
	}
	if strings.HasSuffix(parsed.Path, "/batchexecute") {
		if ids := parsed.Query().Get("rpcids"); ids != "" {
			name = "batchexecute:" + ids
		}
	}
	if name == "" {
		name = "/"
	}
	return studioKeySafeName(name)
}

// studioKeySafeName 只留字母、数字与 ._:$,- 并截到 48 个字符；像 Key 的换成 <key>
func studioKeySafeName(name string) string {
	if studioKeyScanPattern.MatchString(name) {
		return "<key>"
	}
	var builder strings.Builder
	for _, r := range name {
		if r < 128 && (isKeyByte(byte(r)) || strings.ContainsRune("._:$,", r)) {
			builder.WriteRune(r)
			if builder.Len() >= 48 {
				break
			}
		}
	}
	return builder.String()
}

// studioKeyResponseMime 返回响应的 MIME（小写，去掉 ; 之后的参数）；mimeType 为空时看响应头的 Content-Type
func studioKeyResponseMime(response map[string]any) string {
	mime, _ := response["mimeType"].(string)
	if strings.TrimSpace(mime) == "" {
		headers, _ := response["headers"].([]any)
		for _, raw := range headers {
			header, _ := raw.(map[string]any)
			if name, _ := header["name"].(string); strings.EqualFold(name, "content-type") {
				mime = studioKeyHeaderValue(header["value"])
				break
			}
		}
	}
	mime, _, _ = strings.Cut(mime, ";")
	return strings.ToLower(strings.TrimSpace(mime))
}

// studioKeyResponseHost 判断响应是否来自要读正文的域名：aistudio、makersuite 与 googleapis（只有其中 JSON 类的正文找候选，
// 文档与脚本里的 Key 进拒绝集合，见 studioKeyBodyKind）
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

func (session *loginSession) fetchStudioKey(ctx context.Context, options StudioKeyOptions) (StudioKeyResult, error) {
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
		navigatedAt   = time.Now()
		confirmedAt   time.Time
		newRowAt      time.Time
		shownAt       time.Time
		flow          = newStudioKeyFlow()
		verifyRetried bool
		offSiteSince  time.Time
		dismissedAt   time.Time
	)
	run := &studioKeyRun{session: session, collector: collector, listener: listener, flow: flow, options: options}
	run.loadExclusions()
	fail := func(reason string, cause error) (StudioKeyResult, error) {
		return StudioKeyResult{}, run.failure(reason, cause)
	}
	for {
		if err := ctx.Err(); err != nil {
			return fail(flow.timeoutReason(time.Now()), err)
		}
		run.drain(ctx)
		pageURL, err := client.evaluateString(ctx, session.contextID, "location.href")
		if err != nil {
			if !retryablePageEvaluation(err) {
				return fail("读取页面失败", err)
			}
			_ = waitContext(ctx, 300*time.Millisecond)
			continue
		}
		switch {
		case studioKeyDeniedReason(pageURL) != "":
			// 拒绝页（组织停用页、servicerestricted 等，先于登录页判断；地区说明页）：原因按种类写
			return fail(studioKeyDeniedReason(pageURL), nil)
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
		list, listed := run.readList(ctx)
		if !flow.running() {
			if run.savedDenied() && options.ReadOnly {
				// 只读时已保存的 Key 就是页面自己的公共 Key：不是这个账户的，没有可补的
				return fail(StudioKeyReasonSavedIsClient, nil)
			}
			if listed && len(list.Rows) > 0 {
				// 页面上有 Key 列表：只认对得上列表里某一行的 Key（见 chooseStudioKeyRow），从不创建；排除的 Key 的行不选
				settled := time.Since(navigatedAt) >= studioKeyTiming.settle
				rows := run.usableRows(list.Rows)
				if len(rows) == 0 {
					// 页面上的 Key 都是其他账户保存着的或已作废的（多等 projectName 让列表渲染完）
					if time.Since(navigatedAt) < studioKeyTiming.settle+studioKeyTiming.projectName {
						_ = waitContext(ctx, studioKeyTiming.poll)
						continue
					}
					return fail(StudioKeyReasonExcluded, nil)
				}
				choice := chooseStudioKeyRow(rows, run.keys, run.unusable, run.savedKey(), options.ReadOnly)
				if choice.confirmed && (settled || !run.savedPending(list)) {
					if confirmedAt.IsZero() {
						confirmedAt = now
					}
					if choice.candidate.ProjectName != "" || now.Sub(confirmedAt) >= studioKeyTiming.projectName {
						return run.finish(ctx, choice.candidate), nil
					}
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				}
				confirmedAt = time.Time{}
				if !settled {
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				}
				if !choice.hasRow {
					// 只读：列表里没有已保存的那条（多等 projectName 让列表渲染完）
					if time.Since(navigatedAt) < studioKeyTiming.settle+studioKeyTiming.projectName {
						_ = waitContext(ctx, studioKeyTiming.poll)
						continue
					}
					return fail(StudioKeyReasonSavedMissing, nil)
				}
				// 响应里没有对得上这一行的完整 Key（列表接口只给打码的 Key）：打开这一行自己的详情读完整 Key（每次运行最多一次）
				if blocked, reason := session.studioKeyClearDialog(ctx, flow, now, &dismissedAt); blocked {
					if reason != "" {
						return fail(reason, nil)
					}
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				}
				candidate, ok := run.detailsKey(ctx, choice.row, func(key string) bool {
					return !run.isExcluded(key) && (!options.ReadOnly || key == options.SavedKey)
				})
				if !ok {
					read := ""
					if run.details != nil {
						read = run.details.key
					}
					switch {
					case options.ReadOnly && read != "":
						return fail(StudioKeyReasonSavedMissing, nil)
					case read != "" && run.isExcluded(read):
						// 详情里读到的是其他账户保存着的或已作废的 Key（只知道指纹、列表里看不出来的）：打开详情最多一次，不再找别的行
						return fail(StudioKeyReasonExcluded, nil)
					}
					return fail(StudioKeyReasonUnreadable, nil)
				}
				return run.finish(ctx, candidate), nil
			}
			if time.Since(navigatedAt) < studioKeyTiming.settle {
				_ = waitContext(ctx, studioKeyTiming.poll)
				continue
			}
			if !listed || list.State != studioKeyListEmpty || run.liveCandidates() {
				// 认不出 Key 列表（既没有对得上的行，也不是页面自己的空状态），或响应里有不在拒绝集合里的 Key、页面上却没有对得上的行：
				// 不拿响应里的 Key 充数，也不创建
				if time.Since(navigatedAt) < studioKeyTiming.settle+studioKeyTiming.projectName {
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				}
				return fail(StudioKeyReasonListUnknown, nil)
			}
			if options.ReadOnly {
				// 只读：页面上一条 Key 都没有（空状态）；不点任何东西
				return fail(StudioKeyReasonSavedMissing, nil)
			}
			// 页面是空状态：记下这时的列表与响应里的 Key（之后只认新出现的），点击页面自己的创建按钮
			run.markBefore(list)
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
						session.studioKeyDismiss(ctx, flow, page, now)
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
			var dialogKeys []string
			if inspected && classifyStudioKeyDialog(page.Dialog) == studioKeyDialogCreated {
				dialogKeys = run.readCreatedKeys(ctx)
			}
			// 创建之后只认新出现、得到印证的 Key：创建结果对话框里显示的（要与新出现的一行或响应印证），或新出现的一行与响应里末四位
			// 对上的（见 newStudioKey）
			candidate, row, awaiting, ok := run.newStudioKey(list, dialogKeys)
			if ok {
				return run.finish(ctx, candidate), nil
			}
			if awaiting && row == nil {
				if shownAt.IsZero() {
					shownAt = now
				}
				if now.Sub(shownAt) >= studioKeyTiming.projectName && len(dialogKeys) > 0 && !flow.keyConfirmAt.IsZero() && !run.createdClosed {
					// 创建结果对话框里显示着新 Key，列表里却一直没有新出现的行、响应里也没有它：用对话框自己的关闭类按钮关掉它（只一次），
					// 等列表刷新出新的一行再核对
					run.createdClosed = true
					if closed, err := session.pointerClick(ctx, session.contextID, studioKeyDetailsCloseExpression); closed && err == nil {
						flow.note("关闭创建结果对话框（等列表里出现新的一行核对新 Key）")
					}
					_ = waitContext(ctx, studioKeyTiming.poll)
					continue
				}
			}
			if row != nil {
				if newRowAt.IsZero() {
					newRowAt = now
				}
				if now.Sub(newRowAt) >= studioKeyTiming.projectName && inspected && studioKeyListPath(page.Path) {
					switch {
					case page.Dialog != nil && !flow.keyConfirmAt.IsZero() && !run.createdClosed:
						// 点过创建 Key 的确认、列表里已出现新的一行，创建结果对话框里却只有打码的 Key、响应里也没有：用对话框自己的关闭类
						// 按钮关掉它（只一次），再打开新行的详情
						run.createdClosed = true
						if closed, err := session.pointerClick(ctx, session.contextID, studioKeyDetailsCloseExpression); closed && err == nil {
							flow.note("关闭创建结果对话框")
						}
						_ = waitContext(ctx, studioKeyTiming.poll)
						continue
					case page.Dialog == nil && len(page.Options) == 0:
						candidate, ok := run.detailsKey(ctx, *row, run.freshKey)
						if !ok {
							if flow.keyConfirmAt.IsZero() {
								// 没点过创建 Key 的确认（如新项目自带的 Key）：按页面上已有 Key 但读不到算
								return fail(StudioKeyReasonUnreadable, nil)
							}
							return fail(StudioKeyReasonCreatedUnreadable, nil)
						}
						return run.finish(ctx, candidate), nil
					}
				}
			} else {
				newRowAt = time.Time{}
			}
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
					if step.Target.Scope == "page-create" && (!listed || len(list.Rows) > 0 || run.liveCandidates()) {
						// 再点“Create API key”之前像第一次那样核对：页面上已有 Key（如新项目自带的 Key，上面会读它或打开它的详情）、
						// 响应里有新 Key，或读不到 Key 列表时这一轮不点，免得多建
						break
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

// studioKeyListPath 判断页面是 API Keys 页（/apikey，或官网文档现在链接的 /api-keys）：只在这里打开新行的详情
func studioKeyListPath(path string) bool {
	return strings.HasPrefix(path, StudioKeyPath) || strings.HasPrefix(path, "/api-keys")
}

// studioKeyDismiss 点一次对话框自己的关闭类按钮（取消、关闭等，链接除外）
func (session *loginSession) studioKeyDismiss(ctx context.Context, flow *studioKeyFlow, page studioKeyPage, now time.Time) {
	if page.Dialog == nil {
		return
	}
	for _, button := range page.Dialog.Buttons {
		if button.Enabled && !button.link() && studioKeyLabels.cancel.MatchString(button.Text) {
			step := studioKeyClickStep("dialog", button, "", "关闭对话框“"+button.Text+"”")
			if _, err := session.pointerClick(ctx, session.contextID, studioKeyTargetExpression(step.Target)); err == nil {
				flow.apply(step, now)
			}
			return
		}
	}
}

// studioKeyClearDialog 在打开 Key 详情之前处理挡着的对话框与下拉列表：验证账号提示只点它的关闭类按钮；别的对话框点一次它自己的
// 关闭类按钮，等一会儿仍在就放弃（返回失败原因）。blocked 为假时页面上没有挡着的东西
func (session *loginSession) studioKeyClearDialog(ctx context.Context, flow *studioKeyFlow, now time.Time, dismissedAt *time.Time) (bool, string) {
	page, inspected := session.studioKeyInspect(ctx)
	if !inspected {
		return true, ""
	}
	if page.Dialog == nil && len(page.Options) == 0 {
		return false, ""
	}
	flow.lastPage = &page
	if classifyStudioKeyDialog(page.Dialog) == studioKeyDialogVerify {
		step := flow.decideVerifyDialog(page.Dialog, now)
		switch step.Kind {
		case studioKeyStepFail:
			return true, step.Reason
		case studioKeyStepClick:
			if _, err := session.pointerClick(ctx, session.contextID, studioKeyTargetExpression(step.Target)); err == nil {
				flow.apply(step, now)
			}
		}
		return true, ""
	}
	if dismissedAt.IsZero() {
		*dismissedAt = now
		session.studioKeyDismiss(ctx, flow, page, now)
		return true, ""
	}
	if now.Sub(*dismissedAt) < studioKeyTiming.retry*2 {
		return true, ""
	}
	return true, StudioKeyReasonDetailsBlocked
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

// result 整理返回值：没有从 Key 的记录里读到项目、这次新建了一个项目时，用新项目补上；显示名、项目 ID 缺时用响应里同一项目的补上
func (flow *studioKeyFlow) result(chosen StudioKeyCandidate, count int) StudioKeyResult {
	result := StudioKeyResult{
		Key: chosen.Key, ProjectName: chosen.ProjectName, ProjectNumber: chosen.ProjectNumber, ProjectDisplay: chosen.ProjectDisplay,
		ProjectID: chosen.ProjectID, Created: flow.running(), ProjectCreated: flow.projectCreated(), KeyCount: count,
	}
	if result.Created {
		result.Steps = append([]string(nil), flow.steps...)
	}
	if result.ProjectName == "" && result.ProjectCreated && len(flow.newProjects) == 1 {
		applyStudioProject(&result, flow.newProjects[0])
	}
	if result.ProjectNumber != "" {
		if project, ok := flow.project(result.ProjectNumber); ok {
			applyStudioProject(&result, project)
		}
	}
	return result
}

// applyStudioProject 用项目补上结果里缺的项目字段：编号只在结果还没有编号时补（已有编号的不换），显示名与项目 ID 缺时补
func applyStudioProject(result *StudioKeyResult, project StudioProject) {
	if result.ProjectNumber == "" && project.Number != "" {
		result.ProjectName, result.ProjectNumber = "projects/"+project.Number, project.Number
	}
	if result.ProjectNumber != "" && project.Number != "" && project.Number != result.ProjectNumber {
		return
	}
	if result.ProjectDisplay == "" {
		result.ProjectDisplay = project.Display
	}
	if result.ProjectID == "" {
		result.ProjectID = project.ID
	}
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

// studioKeyDeniedReason 返回 API Keys 页面被跳到拒绝页时的失败原因：拒绝账号的页面为种类的固定文案（组织停用为
// StudioKeyReasonOrgDisabled），地区说明页为 RegionNotSupportedPrefix；不是拒绝页时为空
func studioKeyDeniedReason(pageURL string) string {
	if kind := pageRestriction(pageURL); kind != "" {
		return kind.Reason()
	}
	if isRegionNotSupportedURL(pageURL) {
		return RegionNotSupportedPrefix
	}
	return ""
}
