package camoufoxnative

import (
	"sync/atomic"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type bidiClient struct {
	connection *websocket.Conn
	// writeMu 保证同一时刻只有一个写入者（gorilla/websocket 不支持并发写）
	writeMu sync.Mutex
	// mu 保护下面的等待表与后台读取协程写入的观测字段
	mu                       sync.Mutex
	nextID                   int
	pending                  map[int]chan bidiReply
	readErr                  error
	generateHeaders          map[string]string
	blockedGenerateRequestID string
	accessTokenStatus        int
}

type bidiReply struct {
	message map[string]any
	err     error
}

type bidiCommandError struct {
	method  string
	code    string
	message string
	payload string
}

func (err *bidiCommandError) Error() string {
	return fmt.Sprintf("BiDi %s 失败: %s", err.method, err.payload)
}

// bidiWriteTimeout 为单条命令写入本机 WebSocket 的上限；写超时会让连接失效，因此不跟随命令自身的短期限
const bidiWriteTimeout = 30 * time.Second

// newBiDiClient 创建 WebDriver BiDi 客户端并启动后台读取协程。
//
// 后台协程持续读取连接上的所有消息：命令回复按 id 交给对应的等待者，网络事件更新观测字段。
// 这样多条命令可以同时进行，单条命令超时只是不再等待它的回复（迟到的回复会被丢弃），
// 不需要关闭连接。原实现一次只能执行一条命令并在命令超时时关闭整个连接，
// 浏览器稍慢一次，这个 Worker 上所有进行中的请求就一起失败
func newBiDiClient(connection *websocket.Conn) *bidiClient {
	client := &bidiClient{
		connection:      connection,
		pending:         make(map[int]chan bidiReply),
		generateHeaders: make(map[string]string),
	}
	go client.readLoop()
	return client
}

// readLoop 读取连接上的全部消息直到连接关闭，然后让所有等待中的命令以连接错误结束
func (client *bidiClient) readLoop() {
	for {
		var message map[string]any
		if err := client.connection.ReadJSON(&message); err != nil {
			readErr := fmt.Errorf("BiDi 连接已断开: %w", err)
			client.mu.Lock()
			client.readErr = readErr
			pending := client.pending
			client.pending = make(map[int]chan bidiReply)
			client.mu.Unlock()
			for _, reply := range pending {
				reply <- bidiReply{err: readErr}
			}
			return
		}
		client.observe(message)
		messageID, ok := number(message["id"])
		if !ok {
			continue
		}
		client.mu.Lock()
		reply := client.pending[int(messageID)]
		delete(client.pending, int(messageID))
		client.mu.Unlock()
		if reply != nil {
			reply <- bidiReply{message: message}
		}
	}
}

// broken 返回连接断开的原因；连接正常时返回 nil
func (client *bidiClient) broken() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.readErr
}

// command 发送一条 BiDi 命令并等待它的回复
func (client *bidiClient) command(ctx context.Context, method string, params map[string]any) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bidiStats.commands.Add(1)
	bidiStats.inflight.Add(1)
	commandStarted := time.Now()
	defer func() {
		bidiStats.inflight.Add(-1)
		elapsed := int64(time.Since(commandStarted))
		bidiStats.nanos.Add(elapsed)
		for {
			current := bidiStats.maxNanos.Load()
			if elapsed <= current || bidiStats.maxNanos.CompareAndSwap(current, elapsed) {
				break
			}
		}
	}()
	client.mu.Lock()
	if client.readErr != nil {
		err := client.readErr
		client.mu.Unlock()
		return nil, err
	}
	client.nextID++
	id := client.nextID
	reply := make(chan bidiReply, 1)
	client.pending[id] = reply
	client.mu.Unlock()
	abandon := func() {
		client.mu.Lock()
		delete(client.pending, id)
		client.mu.Unlock()
	}
	client.writeMu.Lock()
	err := client.connection.SetWriteDeadline(time.Now().Add(bidiWriteTimeout))
	if err == nil {
		err = client.connection.WriteJSON(map[string]any{"id": id, "method": method, "params": params})
	}
	client.writeMu.Unlock()
	if err != nil {
		abandon()
		// gorilla/websocket 写失败后连接不可再写：关闭连接，让读取协程退出并把连接标记为断开，
		// Worker 随之报告失败并被重建，而不是看起来正常却每条命令都失败
		_ = client.connection.Close()
		return nil, err
	}
	deadline := time.Now().Add(150 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	var message map[string]any
	select {
	case result := <-reply:
		if result.err != nil {
			return nil, result.err
		}
		message = result.message
	case <-ctx.Done():
		abandon()
		return nil, ctx.Err()
	case <-timer.C:
		abandon()
		return nil, fmt.Errorf("BiDi %s 等待回复超时: %w", method, context.DeadlineExceeded)
	}
	if message["type"] != "success" {
		encoded, _ := json.Marshal(message)
		code, _ := message["error"].(string)
		detail, _ := message["message"].(string)
		return nil, &bidiCommandError{method: method, code: code, message: detail, payload: string(encoded)}
	}
	result, _ := message["result"].(map[string]any)
	return result, nil
}

// generateHeader 返回 bootstrap 期间观测到的官网 GenerateContent 请求头
func (client *bidiClient) generateHeader(name string) string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.generateHeaders[name]
}

// blockedRequestID 返回被拦截的官网 GenerateContent 请求 id
func (client *bidiClient) blockedRequestID() string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.blockedGenerateRequestID
}

// tokenStatus 返回最近一次 GenerateAccessToken 的 HTTP 状态，0 表示尚未完成
func (client *bidiClient) tokenStatus() int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.accessTokenStatus
}

// resetTokenStatus 清除 GenerateAccessToken 状态，等待下一次请求
func (client *bidiClient) resetTokenStatus() {
	client.mu.Lock()
	client.accessTokenStatus = 0
	client.mu.Unlock()
}

// observe 在后台读取协程中记录穿插的网络事件
func (client *bidiClient) observe(message map[string]any) {
	method, _ := message["method"].(string)
	if !strings.HasPrefix(method, "network.") {
		return
	}
	params, _ := message["params"].(map[string]any)
	request, _ := params["request"].(map[string]any)
	rawURL, _ := request["url"].(string)
	requestMethod, _ := request["method"].(string)
	parsed, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(parsed.Hostname(), "alkalimakersuite-pa.clients6.google.com") || requestMethod != http.MethodPost {
		return
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if parsed.Path == accessTokenPath {
		response, _ := params["response"].(map[string]any)
		if status, ok := number(response["status"]); ok && method == "network.responseCompleted" {
			client.accessTokenStatus = int(status)
		}
		return
	}
	if parsed.Path != generateContentPath {
		return
	}
	if headers, ok := request["headers"].([]any); ok {
		for _, item := range headers {
			header, _ := item.(map[string]any)
			name, _ := header["name"].(string)
			value := remoteBytesValue(header["value"])
			if name != "" {
				client.generateHeaders[strings.ToLower(name)] = value
			}
		}
	}
	blocked, _ := params["isBlocked"].(bool)
	requestID, _ := request["request"].(string)
	if method == "network.beforeRequestSent" && blocked && requestID != "" {
		client.blockedGenerateRequestID = requestID
	}
}

// installCookies 按 storage state 的分区恢复 Cookie
func (client *bidiClient) installCookies(ctx context.Context, cookies []storageCookie) error {
	for _, item := range cookies {
		cookie := map[string]any{
			"name":     item.Name,
			"value":    map[string]any{"type": "string", "value": item.Value},
			"domain":   item.Domain,
			"path":     item.Path,
			"httpOnly": item.HTTPOnly,
			"secure":   item.Secure,
		}
		if item.Expires > 0 {
			cookie["expiry"] = int64(math.Floor(item.Expires))
		}
		sameSite := strings.ToLower(item.SameSite)
		if sameSite == "strict" || sameSite == "lax" || sameSite == "none" && item.Secure {
			cookie["sameSite"] = sameSite
		}
		params := map[string]any{"cookie": cookie}
		if item.PartitionKey != "" {
			params["partition"] = map[string]any{"type": "storageKey", "sourceOrigin": item.PartitionKey}
		}
		if _, err := client.command(ctx, "storage.setCookie", params); err != nil {
			return fmt.Errorf("写入 Cookie %s: %w", item.Name, err)
		}
	}
	return nil
}

// installLocalStorage 在站点脚本前恢复各 origin 的 localStorage
func (client *bidiClient) installLocalStorage(ctx context.Context, contextID string, origins []storageOrigin) error {
	values := make(map[string]map[string]string, len(origins))
	for _, origin := range origins {
		items := make(map[string]string, len(origin.LocalStorage))
		for _, item := range origin.LocalStorage {
			items[item.Name] = item.Value
		}
		values[origin.Origin] = items
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		return err
	}
	function := fmt.Sprintf(`() => {
  const all = %s;
  const current = all[location.origin];
  if (!current) return;
  for (const [name, value] of Object.entries(current)) localStorage.setItem(name, value);
}`, encoded)
	_, err = client.command(ctx, "script.addPreloadScript", map[string]any{
		"functionDeclaration": function,
		"contexts":            []string{contextID},
	})
	if err != nil {
		return fmt.Errorf("安装 localStorage preload: %w", err)
	}
	return nil
}

// evaluate 在页面默认主世界执行表达式
func (client *bidiClient) evaluate(ctx context.Context, contextID, expression string) (map[string]any, error) {
	result, err := client.command(ctx, "script.evaluate", map[string]any{
		"expression":   expression,
		"target":       map[string]any{"context": contextID},
		"awaitPromise": true,
	})
	if err != nil {
		return nil, err
	}
	if result["type"] == "exception" {
		encoded, _ := json.Marshal(result)
		return nil, fmt.Errorf("页面表达式异常: %s", encoded)
	}
	remote, _ := result["result"].(map[string]any)
	return remote, nil
}

func (client *bidiClient) evaluateString(ctx context.Context, contextID, expression string) (string, error) {
	result, err := client.evaluate(ctx, contextID, expression)
	if err != nil {
		return "", err
	}
	value, _ := result["value"].(string)
	return value, nil
}

func (client *bidiClient) evaluateBool(ctx context.Context, contextID, expression string) (bool, error) {
	result, err := client.evaluate(ctx, contextID, expression)
	if err != nil {
		return false, err
	}
	value, _ := result["value"].(bool)
	return value, nil
}

// waitFor 将页面条件的等待期限传递给 BiDi 命令
func (client *bidiClient) waitFor(ctx context.Context, contextID, expression string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		ready, err := client.evaluateBool(ctx, contextID, expression)
		if err != nil && !retryablePageEvaluation(err) {
			return err
		}
		if err == nil && ready {
			return nil
		}
		if err := waitContext(ctx, 200*time.Millisecond); err != nil {
			return err
		}
	}
	return fmt.Errorf("等待页面条件超时: %s", expression)
}

// waitSnapshotFunction 在阶段期限内定位页面函数
func (client *bidiClient) waitSnapshotFunction(ctx context.Context, contextID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for time.Now().Before(deadline) {
		key, err := client.evaluateString(ctx, contextID, snapshotHookExpression())
		if err != nil && !retryablePageEvaluation(err) {
			return "", err
		}
		if err == nil && key != "" && key != "no_default_MakerSuite" && key != "no_snapshot_fn" {
			return key, nil
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if err := waitContext(ctx, 500*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", errors.New("官网高层 snapshot 函数定位超时")
}

// waitBlockedGenerateRequest 在阶段期限内接收网络拦截事件
func (client *bidiClient) waitBlockedGenerateRequest(ctx context.Context, contextID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	for time.Now().Before(deadline) {
		if requestID := client.blockedRequestID(); requestID != "" {
			return requestID, nil
		}
		if _, err := client.evaluateBool(ctx, contextID, "true"); err != nil && !retryablePageEvaluation(err) {
			return "", err
		}
		if err := waitContext(ctx, 100*time.Millisecond); err != nil {
			return "", err
		}
	}
	return "", errors.New("官网 GenerateContent 拦截事件超时")
}

func retryablePageEvaluation(err error) bool {
	var commandError *bidiCommandError
	if !errors.As(err, &commandError) || commandError.method != "script.evaluate" {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(commandError.code))
	if code == "no such frame" || code == "no such browsing context" {
		return true
	}
	message := strings.ToLower(commandError.message)
	contextLost := strings.Contains(message, "browsing context") || strings.Contains(message, "realm")
	destroyed := strings.Contains(message, "discarded") || strings.Contains(message, "destroyed") || strings.Contains(message, "navigation")
	return (code == "no such handle" || code == "unknown error") && contextLost && destroyed
}

func waitContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func snapshotHookExpression() string {
	return `(() => {
  const makerSuite = window.default_MakerSuite;
  if (!makerSuite) return 'no_default_MakerSuite';
  const currentKey = window.__aistudioWaaSnapshotKey;
  if (currentKey && makerSuite[currentKey]?.__aistudioWrapped) return currentKey;
  let snapshotKey = null;
  for (const key of Object.keys(makerSuite)) {
    try {
      if (typeof makerSuite[key] !== 'function') continue;
      const source = makerSuite[key].toString();
      if (source.includes('.snapshot({') && source.includes('content') && source.includes('yield')) {
        snapshotKey = key;
        break;
      }
    } catch (_) {}
  }
  if (!snapshotKey) return 'no_snapshot_fn';
  const original = makerSuite[snapshotKey];
  const wrapped = function(...args) {
    const service = args[0];
    if (service && (typeof service === 'object' || typeof service === 'function')) {
      window.__aistudioWaaService = service;
    }
    return original.apply(this, args);
  };
  wrapped.__aistudioWrapped = true;
  makerSuite[snapshotKey] = wrapped;
  window.__aistudioWaaSnapshotKey = snapshotKey;
  return snapshotKey;
})()`
}

func takeProofExpression(digest string) string {
	encoded, _ := json.Marshal(digest)
	return fmt.Sprintf(`(async () => {
  const makerSuite = window.default_MakerSuite;
  const service = window.__aistudioWaaService;
  const snapshotKey = window.__aistudioWaaSnapshotKey;
  if (!makerSuite || !service || !snapshotKey || typeof makerSuite[snapshotKey] !== 'function') {
    throw new Error('官方 WAA service 尚未就绪');
  }
  return await makerSuite[snapshotKey](service, %s);
})()`, encoded)
}

func remoteBytesValue(value any) string {
	item, _ := value.(map[string]any)
	raw, _ := item["value"].(string)
	return raw
}

func number(value any) (float64, bool) {
	switch item := value.(type) {
	case float64:
		return item, true
	case int:
		return float64(item), true
	default:
		return 0, false
	}
}

// bidiStats 统计发往浏览器的 BiDi 命令。每个进行中的请求都要靠命令从浏览器取响应，
// 命令量直接决定浏览器与本程序的 CPU 占用，也决定新请求生成 proof 时要排多久
var bidiStats struct {
	commands atomic.Int64
	inflight atomic.Int64
	nanos    atomic.Int64
	maxNanos atomic.Int64
}

// RuntimeStats 返回浏览器命令与受保护请求轮询的累计统计（管理接口 /api/debug/perf）
func RuntimeStats() map[string]any {
	commands := bidiStats.commands.Load()
	average := 0.0
	if commands > 0 {
		average = float64(bidiStats.nanos.Load()) / float64(commands) / float64(time.Millisecond)
	}
	return map[string]any{
		"bidi_commands":         commands,
		"bidi_inflight":         bidiStats.inflight.Load(),
		"bidi_avg_ms":           average,
		"bidi_max_ms":           float64(bidiStats.maxNanos.Load()) / float64(time.Millisecond),
		"header_polls":          protectedStats.headerPolls.Load(),
		"chunk_polls":           protectedStats.chunkPolls.Load(),
		"chunk_polls_with_data": protectedStats.chunkPollsWithData.Load(),
		"long_poll_ms":          protectedLongPollWait.Milliseconds(),
	}
}
