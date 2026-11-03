package aistudio

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// 解码容错
//
// 官网的 JSON+protobuf 响应会省略末尾未设置的字段、把未设置的枚举写成 null，也会新增枚举值与来源类型。解码器原先“读不懂的位置一律中止”
// （ProtocolEvidenceError）：正文已经生成、额度已扣，客户端却拿到 500 “An internal error has occurred”（Omni 视频 MIME 枚举省略成 null
// 即为一例）。现在按下面的规则继续解码，只有正文本身读不出来时才报错：
//   - 结构性问题（空帧、field 1 里的 null 或稀疏元素、候选为空数组）：跳过这一帧或元素；多个候选取第一个（本服务只请求一个候选：
//     OpenAI 的 n 与 Gemini 的 candidateCount 不是 1 时按 400 拒绝）
//   - 枚举为 null 或不在表里：取缺省值，不在表里的保留为 *_<n> 这类可读名；不丢正文
//   - 辅助元数据（用量、引用、grounding 等）解码失败：丢掉这一项或这一条目，继续解码其余内容；用量走本地估算。promptFeedback 决定这一帧
//     是不是输入拦截，读不出时按 unspecified 拦截，不按未拦截跳过（跳过整帧后流里没有完成帧，会按“没有完成帧”在同账号重新生成）
//
// 每次容错都记下来：同一请求里同一种容错（方法、种类与去掉下标的位置）只写一行 WARN 到运行日志（slog）与该请求的排查记录，带截断的原文
// （toleranceSnippetLimit 字节以内，长 Base64 只留长度），协议变化能被发现又不刷屏；请求的管理日志不逐帧写

// toleranceSnippetLimit 为容错日志里原文的最大字节数
const toleranceSnippetLimit = 240

// toleranceMediaRun 以上长度的 Base64 字母连续段（整个字符串，或 data URL、夹在文本里的一段）按媒体数据处理，日志里只写长度
const toleranceMediaRun = 96

// decodeTolerance 记录一个请求解码时已经写过的容错，并把新的一种写进运行日志与排查记录。nil 接收者什么也不做
type decodeTolerance struct {
	trace     *Trace
	requestID string
	mu        sync.Mutex
	seen      map[string]struct{}
}

type decodeToleranceContextKey struct{}

// newDecodeTolerance 返回一个请求的解码容错记录；trace 为该请求的排查记录（主路由请求为 nil）
func newDecodeTolerance(trace *Trace, requestID string) *decodeTolerance {
	return &decodeTolerance{trace: trace, requestID: requestID, seen: make(map[string]struct{})}
}

// ContextWithDecodeTolerance 为一个请求挂上解码容错记录：同一请求的各次尝试（换号、重新生成、自动续写的各轮）共用，同一种容错只写一行。
// 已经挂过时原样返回；没有挂时 Generate 按每次调用各挂一个
func ContextWithDecodeTolerance(ctx context.Context, requestID string) context.Context {
	if _, ok := ctx.Value(decodeToleranceContextKey{}).(*decodeTolerance); ok {
		return ctx
	}
	return context.WithValue(ctx, decodeToleranceContextKey{}, newDecodeTolerance(TraceFromContext(ctx), requestID))
}

// decodeToleranceFrom 返回上下文里的解码容错记录；没有时新建一个（只在这次解码内去重）
func decodeToleranceFrom(ctx context.Context) *decodeTolerance {
	if tolerance, ok := ctx.Value(decodeToleranceContextKey{}).(*decodeTolerance); ok {
		return tolerance
	}
	return newDecodeTolerance(TraceFromContext(ctx), "")
}

// toleranceIndex 匹配位置里的数组下标，去重时不区分是第几帧、第几个 Part 或条目
var toleranceIndex = regexp.MustCompile(`\[\d+\]`)

// note 记下一次容错：method 为上游方法，path 为协议位置，kind 为容错的种类与处理（中文），raw 为该位置的原文（nil 表示缺位）。
// 同一请求里方法、种类与去掉下标的位置都相同的只写第一次
func (t *decodeTolerance) note(method string, path string, kind string, raw json.RawMessage) {
	if t == nil {
		return
	}
	key := method + "\x00" + kind + "\x00" + toleranceIndex.ReplaceAllString(path, "[]")
	t.mu.Lock()
	if _, exists := t.seen[key]; exists {
		t.mu.Unlock()
		return
	}
	t.seen[key] = struct{}{}
	t.mu.Unlock()
	snippet := toleranceSnippet(raw)
	slog.Warn("上游响应解码容错，已继续", "request_id", t.requestID, "method", method, "path", path, "kind", kind, "raw", snippet)
	t.trace.Note(fmt.Sprintf("解码容错 | %s | 位置=%s | %s | 原文=%s", method, path, kind, snippet))
}

// toleranceText 把一段说明写成容错日志的原文（不是协议原文时用，如候选个数）
func toleranceText(format string, args ...any) json.RawMessage {
	encoded, _ := json.Marshal(fmt.Sprintf(format, args...))
	return encoded
}

// toleranceJSONString 匹配原文里的一个 JSON 字符串字面量（含转义）
var toleranceJSONString = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

// toleranceMediaSegment 匹配字符串里的一段 Base64 字母（标准与 URL 安全，含填充；分行的 Base64 按换行连成一段）。空格不算在内：
// 普通文本按空格分词，不会连成一段
var toleranceMediaSegment = regexp.MustCompile(`[A-Za-z0-9+/_\-]+(?:[\r\n]+[A-Za-z0-9+/_\-]+)*=*`)

// toleranceSnippet 返回写进容错日志的原文：长 Base64 只留长度（整个字符串是 Base64 时写成 "<N 字符>"，data URL 与夹在文本里的一段
// 只替换那一段），超过 toleranceSnippetLimit 字节截断；缺位时为“缺位”
func toleranceSnippet(raw json.RawMessage) string {
	if raw == nil {
		return "缺位"
	}
	var compact bytes.Buffer
	text := string(raw)
	if json.Compact(&compact, raw) == nil {
		text = compact.String()
	}
	// 逐个字符串字面量按解码后的值判断（JSON 转义 \n、\/、\u002b 与夹带的换行都已还原），先替换再截断：截断在前时被切开的 Base64 认不出来
	text = toleranceJSONString.ReplaceAllStringFunc(text, func(literal string) string {
		if len(literal) < toleranceMediaRun+2 {
			return literal
		}
		var value string
		if json.Unmarshal([]byte(literal), &value) != nil {
			return literal
		}
		if toleranceMediaLike(value) {
			return fmt.Sprintf(`"<%d 字符>"`, len(value))
		}
		replaced := false
		value = toleranceMediaSegment.ReplaceAllStringFunc(value, func(segment string) string {
			count := 0
			for index := 0; index < len(segment); index++ {
				if segment[index] != '\r' && segment[index] != '\n' {
					count++
				}
			}
			if count < toleranceMediaRun {
				return segment
			}
			replaced = true
			return fmt.Sprintf("<%d 字符>", len(segment))
		})
		if !replaced {
			return literal
		}
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(value) != nil {
			return literal
		}
		return strings.TrimSuffix(encoded.String(), "\n")
	})
	if len(text) <= toleranceSnippetLimit {
		return text
	}
	cut := toleranceSnippetLimit
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + fmt.Sprintf("…（共 %d 字节）", len(raw))
}

// toleranceMediaLike 判断一个字符串是否像 Base64 媒体数据或签名：去掉空白后至少 toleranceMediaRun 个字符，且全是标准或 URL 安全的
// Base64 字母（含填充）
func toleranceMediaLike(value string) bool {
	count := 0
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case character == ' ' || character == '\n' || character == '\r' || character == '\t':
		case character >= 'A' && character <= 'Z', character >= 'a' && character <= 'z', character >= '0' && character <= '9',
			character == '+', character == '/', character == '=', character == '_', character == '-':
			count++
		default:
			return false
		}
	}
	return count >= toleranceMediaRun
}

// jsonKind 返回 JSON 值的类型：null（含缺位）、string、number、bool、array、object；无法识别时为空串
func jsonKind(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "null"
	}
	switch trimmed[0] {
	case '"':
		return "string"
	case '[':
		return "array"
	case '{':
		return "object"
	case 't', 'f':
		return "bool"
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return "number"
	}
	return ""
}

// lenientInt64 按最合理的整数读一个数：整数、带小数的数（四舍五入）、数字字符串都接受；null、缺位与其他类型返回假
func lenientInt64(raw json.RawMessage) (int64, bool) {
	var number json.Number
	switch jsonKind(raw) {
	case "number":
		number = json.Number(bytes.TrimSpace(raw))
	case "string":
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, false
		}
		number = json.Number(strings.TrimSpace(text))
	default:
		return 0, false
	}
	if value, err := strconv.ParseInt(number.String(), 10, 64); err == nil {
		return value, true
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || math.Abs(value) > math.MaxInt64/2 {
		return 0, false
	}
	return int64(math.Round(value)), true
}

// lenientBool 按“非 0、非 false 即为真”读一个标记：布尔值、数字与 "true"/"false"/"1"/"0" 字符串；null 与缺位为假。
// 其他写法（对象、数组、不认识的字符串）返回 false 与假
func lenientBool(raw json.RawMessage) (bool, bool) {
	switch jsonKind(raw) {
	case "null":
		return false, true
	case "bool":
		var value bool
		return value, json.Unmarshal(raw, &value) == nil
	case "number":
		value, err := strconv.ParseFloat(string(bytes.TrimSpace(raw)), 64)
		return err == nil && value != 0, err == nil
	case "string":
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return false, false
		}
		switch strings.ToLower(strings.TrimSpace(text)) {
		case "true", "1":
			return true, true
		case "false", "0", "":
			return false, true
		}
	}
	return false, false
}

// lenientText 读一段文本：字符串原样返回，null 与缺位为空串，数字与布尔值按 JSON 原文转成文本（converted 为真）；
// 对象与数组读不出文本，ok 为假
func lenientText(raw json.RawMessage) (text string, converted bool, ok bool) {
	switch jsonKind(raw) {
	case "null":
		return "", false, true
	case "string":
		if json.Unmarshal(raw, &text) != nil {
			return "", false, false
		}
		return text, false, true
	case "number", "bool":
		return string(bytes.TrimSpace(raw)), true, true
	}
	return "", false, false
}

// decodeLenientBase64 解码 Base64：标准与 URL 安全两种字母表，带或不带填充都接受，夹带的换行与空白先去掉；都不是时返回假。
// 标准带填充的写法最先尝试，原本能解码的数据结果不变
func decodeLenientBase64(encoded string) ([]byte, bool) {
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if data, err := encoding.DecodeString(encoded); err == nil {
			return data, true
		}
	}
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\n' || r == '\r' || r == '\t' {
			return -1
		}
		return r
	}, encoded)
	if stripped != encoded {
		return decodeLenientBase64(stripped)
	}
	return nil, false
}

// sniffMediaMIME 在上游没有给出 MIME 时按文件头判断媒体类型（http.DetectContentType，认不出时为 application/octet-stream）
func sniffMediaMIME(data []byte) string {
	mime := http.DetectContentType(data)
	if index := strings.IndexByte(mime, ';'); index >= 0 {
		mime = strings.TrimSpace(mime[:index])
	}
	return mime
}
