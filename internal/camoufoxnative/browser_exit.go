package camoufoxnative

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Camoufox 退出时的诊断（按上游 Mag1cFall/AIStudio2API c3c9cbc 移植，用户批准的例外）：Camoufox 在报告 BiDi 端点前退出（启动）、
// 运行中退出（BiDi 连接随之断开）或登录期间退出时，错误写明退出码（被信号结束时为信号）与浏览器最近的 browserOutputLines 行输出。
// 输出照常转写到 Options.Log（不变；服务的 Worker 与登录都不设置它，浏览器输出原先不进任何日志，现在只经这里的退出错误带出最近几行）；
// 另在内存里留最近几行（环形，有上限），写进错误前去掉敏感内容：URL 只留 scheme、host 与 path（去掉账号密码、查询参数与片段），
// 不带 scheme 的 user:password@host 只留 host，邮箱打码，Authorization、Cookie 等凭据头之后整段换成 ***，Bearer 令牌与
// 密码、token、API Key 等的值换成 ***（含 JSON 写法）；网页脚本的控制台消息不留。
// 只改错误文案与输出的读取方式（stdout、stderr 改用 os.Pipe，Wait 不再关掉读端，退出前最后几行不丢），启动参数、环境、prefs、
// 页面与网络行为都不变

// browserOutputLines 为退出错误里保留的最近输出行数
const browserOutputLines = 20

// browserOutputLineLimit 为每行写进错误的最大字符数
const browserOutputLineLimit = 200

// browserExitWait 为报告端点前退出时等输出读完的上限，以及 BiDi 连接断开后等 Camoufox 进程退出并读完输出的合计上限
// （与上游 c3c9cbc 相同，两段共用一个期限）
const browserExitWait = 5 * time.Second

// browserOutputSeparator 为错误里各行输出之间的分隔：错误与日志都是单行，诊断脚本按行读取
const browserOutputSeparator = " ⏎ "

// browserOutput 保存 Camoufox 最近输出的行（已去掉敏感内容），进程退出时写进错误
type browserOutput struct {
	mu    sync.Mutex
	lines []string
}

// pageScriptMessage 匹配网页脚本的控制台消息行（带页面地址，与浏览器自身退出无关），不留；浏览器自身脚本（chrome://、resource://）的照留
var pageScriptMessage = regexp.MustCompile(`^JavaScript (warning|error): https?://`)

// add 记下一行输出（去掉敏感内容、截短），只留最近 browserOutputLines 行；空行与网页脚本的控制台消息不留
func (output *browserOutput) add(line string) {
	if output == nil {
		return
	}
	line = strings.TrimSpace(line)
	if line == "" || pageScriptMessage.MatchString(line) {
		return
	}
	line = sanitizeBrowserOutput(line)
	output.mu.Lock()
	defer output.mu.Unlock()
	output.lines = append(output.lines, line)
	if extra := len(output.lines) - browserOutputLines; extra > 0 {
		output.lines = append(output.lines[:0], output.lines[extra:]...)
	}
}

// text 返回最近输出的各行，以 browserOutputSeparator 连接；没有输出时为空
func (output *browserOutput) text() string {
	if output == nil {
		return ""
	}
	output.mu.Lock()
	defer output.mu.Unlock()
	return strings.Join(output.lines, browserOutputSeparator)
}

var (
	// outputURLPattern 匹配输出里的 URL（任意 scheme）
	outputURLPattern = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://[^\s"'<>]+`)
	// outputEmailPattern 匹配邮箱，打码为前两位 + ***@***（与页面快照的打码规则一致）
	outputEmailPattern = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*@(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)
	// outputHeaderPattern 匹配凭据类的请求头与响应头（Authorization、Proxy-Authorization、Cookie、Set-Cookie，含 JSON 里带引号的键）：
	// 值里可能有空格（Bearer <token>）与多个 name=value，冒号或等号之后到行尾整段换成 ***
	outputHeaderPattern = regexp.MustCompile(`(?i)\b((?:proxy-)?authorization|(?:set-)?cookie)("?\s*[=:]\s*).*$`)
	// outputSecretPattern 匹配 key=value、key: value 与 JSON "key":"value" 形式的密码、token、API Key、secret 等（键名可带前缀，
	// 如 proxy_password、refresh_token、x-goog-api-key），只留键名
	outputSecretPattern = regexp.MustCompile(`(?i)\b([\w-]*(?:password|passwd|pwd|token|api[_-]?key|secret|credentials?))("?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"']+)`)
	// outputBearerPattern 匹配不在 Authorization 头里的 Bearer 令牌
	outputBearerPattern = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._~+/=-]+`)
	// outputUserinfoPattern 匹配不带 scheme 的 user:password@host（代理地址常这样写），账号密码换成 ***
	outputUserinfoPattern = regexp.MustCompile(`[^\s:/@"'<>=]+:[^\s:/@"'<>]+@([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])`)
)

// sanitizeBrowserOutput 去掉一行输出里的敏感内容并截到 browserOutputLineLimit 字：URL 只留 scheme://host/path（账号密码、
// 查询参数与片段去掉，有查询参数时写 ?…），不带 scheme 的 user:password@host 只留 host，邮箱打码，Authorization、Cookie 等
// 凭据头之后整段换成 ***，Bearer 令牌、密码、token、API Key 等的值换成 ***；控制字符换成空格
func sanitizeBrowserOutput(line string) string {
	line = outputURLPattern.ReplaceAllStringFunc(line, sanitizeOutputURL)
	line = outputUserinfoPattern.ReplaceAllString(line, "***@${1}")
	line = outputEmailPattern.ReplaceAllString(line, "${1}***@***")
	line = outputHeaderPattern.ReplaceAllString(line, "${1}${2}***")
	line = outputBearerPattern.ReplaceAllString(line, "${1} ***")
	line = outputSecretPattern.ReplaceAllString(line, "${1}${2}***")
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, line)
	if runes := []rune(line); len(runes) > browserOutputLineLimit {
		line = string(runes[:browserOutputLineLimit]) + "…"
	}
	return line
}

// sanitizeOutputURL 把 URL 换成 scheme://host/path：账号密码（代理地址可能带着）、查询参数与片段都不留
func sanitizeOutputURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		// 解析不了：去掉 @ 之前的账号密码与 ? 之后的部分
		if cut := strings.IndexAny(raw, "?#"); cut >= 0 {
			raw = raw[:cut] + "?…"
		}
		if scheme := strings.Index(raw, "://"); scheme >= 0 {
			if at := strings.LastIndex(raw, "@"); at > scheme {
				raw = raw[:scheme+3] + "***@" + raw[at+1:]
			}
		}
		return raw
	}
	result := parsed.Scheme + "://"
	if parsed.User != nil {
		result += "***@"
	}
	result += parsed.Host + parsed.EscapedPath()
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.ForceQuery {
		result += "?…"
	}
	return result
}

// BrowserExitedError 为 Camoufox 进程自行退出的错误（不是服务关闭它）：Stage 为退出的时机（报告 BiDi 端点前、运行中），
// Code 为退出码（Windows 为十六进制）或结束它的信号，Clean 表示退出码为 0，Output 为最近的输出（已去掉敏感内容，以 ⏎ 分隔）
type BrowserExitedError struct {
	Stage  string
	Code   string
	Clean  bool
	Output string
	err    error
}

func (exited *BrowserExitedError) Error() string {
	text := fmt.Sprintf("Camoufox %s退出，%s", exited.Stage, exited.Code)
	if exited.Output == "" {
		return text + "，没有输出"
	}
	return text + " | 最近输出=" + exited.Output
}

// Unwrap 返回 Wait 的原错误（*exec.ExitError，退出码为 0 时为 nil）
func (exited *BrowserExitedError) Unwrap() error {
	return exited.err
}

// 退出的时机（BrowserExitedError.Stage）
const (
	browserExitBeforeEndpoint = "在报告 BiDi 端点前"
	browserExitRunning        = "运行中"
)

// exitCodeText 返回进程的退出码说明：“退出码 N”（Windows 为十六进制，便于对照 NTSTATUS），被信号结束时为“信号 <名称>”
func exitCodeText(state *os.ProcessState) string {
	if state == nil {
		return "退出码未知"
	}
	code := state.ExitCode()
	switch {
	case code < 0:
		return "信号 " + strings.TrimPrefix(state.String(), "signal: ")
	case runtime.GOOS == "windows":
		return fmt.Sprintf("退出码 0x%08X", uint32(code))
	}
	return "退出码 " + strconv.Itoa(code)
}

// exitedError 返回进程已退出的错误（调用方确认 done 已关闭）；先在 wait 内等输出读完，最后几行不丢
func (process *browserProcess) exitedError(stage string, wait time.Duration) *BrowserExitedError {
	timer := time.NewTimer(max(wait, 0))
	defer timer.Stop()
	select {
	case <-process.drained:
	case <-timer.C:
	}
	process.mu.Lock()
	waitErr := process.waitErr
	process.mu.Unlock()
	state := process.command.ProcessState
	return &BrowserExitedError{
		Stage:  stage,
		Code:   exitCodeText(state),
		Clean:  state != nil && state.ExitCode() == 0,
		Output: process.output.text(),
		err:    waitErr,
	}
}

// exitFailure 在 BiDi 连接断开后调用：Camoufox 在 wait 内自行退出时返回写明退出码与最近输出的错误（等进程退出与等输出读完
// 合计不超过 wait：浏览器的子进程还占着输出管道时，已读到的行照样写进错误）；进程仍在运行、由服务关闭（Close 已开始）或
// 没有进程（测试的假连接）时返回 nil
func (process *browserProcess) exitFailure(wait time.Duration) *BrowserExitedError {
	if process == nil || process.done == nil || process.stoppingNow() {
		return nil
	}
	deadline := time.Now().Add(wait)
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-process.done:
	case <-timer.C:
		return nil
	}
	if process.stoppingNow() {
		return nil
	}
	return process.exitedError(browserExitRunning, time.Until(deadline))
}

// stoppingNow 返回服务是否已开始关闭这个进程
func (process *browserProcess) stoppingNow() bool {
	process.mu.Lock()
	defer process.mu.Unlock()
	return process.stopping
}

// browserExitFailure 返回错误链上的 Camoufox 退出错误，没有时为 nil
func browserExitFailure(err error) *BrowserExitedError {
	var exited *BrowserExitedError
	if errors.As(err, &exited) {
		return exited
	}
	return nil
}
