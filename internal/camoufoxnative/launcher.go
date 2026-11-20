package camoufoxnative

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

var bidiEndpointPattern = regexp.MustCompile(`ws://[^\s]+`)

const browserProcessCloseTimeout = 5 * time.Second

type browserProcessTerminator func(context.Context, *exec.Cmd) error

type browserProcess struct {
	command *exec.Cmd
	done    chan struct{}
	waitErr error
	profile string
	lock    *flock.Flock
	// output 为最近的输出（已去掉敏感内容，见 browser_exit.go），drained 在 stdout 与 stderr 都读完后关闭
	output  *browserOutput
	drained chan struct{}
	mu      sync.Mutex
	// stopping 表示服务已开始关闭这个进程（之后的退出不是自行退出，不报退出码）
	stopping bool
	closed   bool
}

// launchBrowser 启动 Camoufox 并返回原生 WebDriver BiDi 端点
func launchBrowser(ctx context.Context, options Options, config map[string]any) (*browserProcess, string, error) {
	if _, err := os.Stat(options.ExecutablePath); err != nil {
		return nil, "", fmt.Errorf("Camoufox 不可用: %w", err)
	}
	environment, err := camoufoxEnvironment(config)
	if err != nil {
		return nil, "", err
	}
	profile, profileLock, err := createProfile()
	if err != nil {
		return nil, "", err
	}
	prefs, err := firefoxPreferences(options.Proxy, options.ProxyBypass)
	if err != nil {
		_ = removeProfile(profile, profileLock)
		return nil, "", err
	}
	if options.Headless {
		prefs["layout.frame_rate"] = headlessFrameRate
	}
	if options.CacheDirectory != "" {
		for key, value := range cachePreferences(options.CacheDirectory) {
			prefs[key] = value
		}
	}
	if err := writeUserJS(profile, prefs); err != nil {
		_ = removeProfile(profile, profileLock)
		return nil, "", err
	}

	arguments := []string{"--remote-debugging-port=0"}
	if options.Headless {
		arguments = append(arguments, "-no-remote", "-headless")
	} else {
		arguments = append(arguments, "-wait-for-browser")
	}
	arguments = append(arguments, "-profile", profile)
	command := exec.Command(options.ExecutablePath, arguments...)
	command.Env = environment
	configureBrowserProcess(command, options.Headless)
	// stdout 与 stderr 用 os.Pipe（不用 StdoutPipe）：Wait 不关读端，进程退出前最后的输出读得完（写进退出错误，见 browser_exit.go）
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = removeProfile(profile, profileLock)
		return nil, "", err
	}
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		_ = removeProfile(profile, profileLock)
		return nil, "", err
	}
	command.Stdout, command.Stderr = stdoutWriter, stderrWriter
	err = command.Start()
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	if err != nil {
		_ = stdout.Close()
		_ = stderr.Close()
		_ = removeProfile(profile, profileLock)
		return nil, "", fmt.Errorf("启动 Camoufox: %w", err)
	}
	process := &browserProcess{
		command: command,
		done:    make(chan struct{}),
		profile: profile,
		lock:    profileLock,
		output:  &browserOutput{},
		drained: make(chan struct{}),
	}
	endpoints := make(chan string, 2)
	var scanned sync.WaitGroup
	for _, reader := range []*os.File{stdout, stderr} {
		scanned.Add(1)
		go func() {
			defer scanned.Done()
			scanBrowserOutput(reader, options.Log, endpoints, process.output)
			_ = reader.Close()
		}()
	}
	go func() {
		scanned.Wait()
		close(process.drained)
	}()
	if err := attachBrowserProcess(command); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		_ = removeProfile(profile, profileLock)
		return nil, "", fmt.Errorf("绑定 Camoufox 进程: %w", err)
	}
	go func() {
		waitErr := command.Wait()
		process.mu.Lock()
		process.waitErr = waitErr
		process.mu.Unlock()
		close(process.done)
	}()
	timeout := options.ReadyTimeout
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case endpoint := <-endpoints:
		return process, normalizeBiDiEndpoint(endpoint), nil
	case <-process.done:
		process.mu.Lock()
		process.closed = true
		process.mu.Unlock()
		_ = removeProfile(profile, profileLock)
		// 写明退出码与最近的输出（等输出读完有上限，见 exitedError）
		return nil, "", process.exitedError(browserExitBeforeEndpoint, browserExitWait)
	case <-timer.C:
		return nil, "", process.abortLaunch(fmt.Errorf("等待 Camoufox BiDi 端点超时: %s", timeout))
	case <-ctx.Done():
		return nil, "", process.abortLaunch(ctx.Err())
	}
}

// browserExitUnconfirmedError 为启动失败、关闭浏览器时没能确认进程退出的错误，文案与原错误相同：
// 浏览器可能还在用账户缓存目录，调用方据 exited 保留缓存锁直到进程退出（见 retainAccountCacheUntilExit）
type browserExitUnconfirmedError struct {
	err    error
	exited <-chan struct{}
}

func (unconfirmed *browserExitUnconfirmedError) Error() string { return unconfirmed.err.Error() }

func (unconfirmed *browserExitUnconfirmedError) Unwrap() error { return unconfirmed.err }

// abortLaunch 关闭报告 BiDi 端点之前就失败的浏览器并返回 cause；没能确认进程退出时包装成 browserExitUnconfirmedError
func (process *browserProcess) abortLaunch(cause error) error {
	if process.Close() == nil {
		return cause
	}
	return &browserExitUnconfirmedError{err: cause, exited: process.exited()}
}

// exited 返回浏览器进程退出时关闭的通道
func (process *browserProcess) exited() <-chan struct{} {
	if process == nil {
		return nil
	}
	return process.done
}

// Close 关闭 Camoufox 并删除隔离 profile
func (process *browserProcess) Close() error {
	return process.close(browserProcessCloseTimeout, terminateBrowserProcess)
}

func (process *browserProcess) close(timeout time.Duration, terminate browserProcessTerminator) error {
	if process == nil {
		return nil
	}
	process.mu.Lock()
	if process.closed {
		process.mu.Unlock()
		return nil
	}
	process.stopping = true
	process.mu.Unlock()
	if timeout <= 0 {
		timeout = browserProcessCloseTimeout
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var closeErr error
	select {
	case <-process.done:
	default:
		closeErr = terminate(closeCtx, process.command)
		select {
		case <-process.done:
		case <-closeCtx.Done():
			closeErr = errors.Join(closeErr, fmt.Errorf("等待 Camoufox 进程退出: %w", closeCtx.Err()))
		}
	}
	closeErr = errors.Join(closeErr, removeProfile(process.profile, process.lock))
	if closeErr != nil {
		return closeErr
	}
	process.mu.Lock()
	process.closed = true
	process.mu.Unlock()
	return nil
}

// scanBrowserOutput 逐行读取浏览器输出：照常转写到 mirror、找 BiDi 端点，另把最近几行记进 output（退出时写进错误，见 browser_exit.go）
func scanBrowserOutput(reader io.Reader, mirror io.Writer, endpoints chan<- string, output *browserOutput) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if mirror != nil {
			_, _ = fmt.Fprintln(mirror, line)
		}
		output.add(line)
		if endpoint := bidiEndpointPattern.FindString(line); endpoint != "" {
			select {
			case endpoints <- endpoint:
			default:
			}
		}
	}
}

func normalizeBiDiEndpoint(endpoint string) string {
	parsed, err := url.Parse(endpoint)
	if err == nil && (parsed.Path == "" || parsed.Path == "/") {
		parsed.Path = "/session"
		return parsed.String()
	}
	return endpoint
}

// headlessFrameRate 为无头 Worker 的页面刷新帧率
const headlessFrameRate = 1

func firefoxPreferences(proxyValue, bypass string) (map[string]any, error) {
	prefs := map[string]any{
		"remote.active-protocols":           1,
		"devtools.chrome.enabled":           true,
		"devtools.debugger.remote-enabled":  true,
		"browser.shell.checkDefaultBrowser": false,
	}
	proxyValue = strings.TrimSpace(proxyValue)
	if proxyValue == "" {
		return prefs, nil
	}
	parsed, err := url.Parse(proxyValue)
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("Camoufox 代理 URL 无效")
	}
	if parsed.User != nil {
		return nil, fmt.Errorf("Camoufox 原生代理暂不接受账号密码")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port <= 0 {
		return nil, fmt.Errorf("Camoufox 代理缺少有效端口")
	}
	prefs["network.proxy.type"] = 1
	prefs["network.proxy.no_proxies_on"] = bypass
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		prefs["network.proxy.http"] = parsed.Hostname()
		prefs["network.proxy.http_port"] = port
		prefs["network.proxy.ssl"] = parsed.Hostname()
		prefs["network.proxy.ssl_port"] = port
		prefs["network.proxy.share_proxy_settings"] = true
	case "socks4", "socks5":
		prefs["network.proxy.socks"] = parsed.Hostname()
		prefs["network.proxy.socks_port"] = port
		if strings.EqualFold(parsed.Scheme, "socks5") {
			prefs["network.proxy.socks_version"] = 5
			prefs["network.proxy.socks_remote_dns"] = true
		} else {
			prefs["network.proxy.socks_version"] = 4
		}
	default:
		return nil, fmt.Errorf("Camoufox 代理协议必须是 http、https、socks4 或 socks5")
	}
	return prefs, nil
}

func writeUserJS(profile string, prefs map[string]any) error {
	keys := make([]string, 0, len(prefs))
	for key := range prefs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	for _, key := range keys {
		value, err := firefoxPrefLiteral(prefs[key])
		if err != nil {
			return fmt.Errorf("Firefox pref %s: %w", key, err)
		}
		builder.WriteString("user_pref(")
		builder.WriteString(strconv.Quote(key))
		builder.WriteString(", ")
		builder.WriteString(value)
		builder.WriteString(");\n")
	}
	if err := os.WriteFile(filepath.Join(profile, "user.js"), []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("写入 Camoufox profile: %w", err)
	}
	return nil
}

func firefoxPrefLiteral(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return strconv.Quote(typed), nil
	case bool:
		return strconv.FormatBool(typed), nil
	case int:
		return strconv.Itoa(typed), nil
	default:
		return "", fmt.Errorf("不支持 %T", value)
	}
}
