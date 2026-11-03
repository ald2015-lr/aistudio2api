package aistudio

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"unicode"
)

// ProtocolEvidenceError 保存无法解释的协议位置和原始值
type ProtocolEvidenceError struct {
	Method string
	Path   string
	Detail string
	Raw    json.RawMessage
	// Err 为导致解析失败的原始错误（如读取上游响应超时），保留错误链供重试判断使用
	Err error
}

// PromptFeedbackError 表示上游拒绝当前输入且没有返回候选
type PromptFeedbackError struct {
	Reason string
	// Message 为上游对拦截原因的说明（promptFeedback.blockReasonMessage），没有时为空。
	// 只进日志与内部错误；公开协议的错误文案由 api 包按 Reason 统一生成，不含这段原文
	Message string
	Raw     json.RawMessage
}

// Unwrap 返回原始错误，使 errors.Is 能识别读取超时、连接断开等可重试的底层错误
func (e *ProtocolEvidenceError) Unwrap() error {
	return e.Err
}

// Error 返回协议证据错误
func (e *ProtocolEvidenceError) Error() string {
	if e.Method == "" {
		return fmt.Sprintf("协议位置 %s: %s", e.Path, e.Detail)
	}
	return fmt.Sprintf("AI Studio %s 协议位置 %s: %s", e.Method, e.Path, e.Detail)
}

// Error 返回上游输入拒绝原因，上游给出说明时附在后面
func (e *PromptFeedbackError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("AI Studio 拒绝当前输入: %s: %s", e.Reason, e.Message)
	}
	return fmt.Sprintf("AI Studio 拒绝当前输入: %s", e.Reason)
}

// Unwrap 将输入拒绝映射为无效请求
func (e *PromptFeedbackError) Unwrap() error {
	return ErrInvalidArgument
}

type sparseJSONReader struct {
	source        *bufio.Reader
	pending       []byte
	inString      bool
	escaped       bool
	previousToken byte
}

func newSparseJSONReader(source io.Reader) io.Reader {
	return &sparseJSONReader{source: bufio.NewReader(source)}
}

func (r *sparseJSONReader) Read(destination []byte) (int, error) {
	written := 0
	for written < len(destination) {
		if len(r.pending) > 0 {
			count := copy(destination[written:], r.pending)
			r.pending = r.pending[count:]
			written += count
			continue
		}
		if written > 0 && r.source.Buffered() == 0 {
			return written, nil
		}
		value, err := r.source.ReadByte()
		if err != nil {
			if written > 0 {
				return written, nil
			}
			return 0, err
		}
		r.pending = r.normalize(value)
	}
	return written, nil
}

func (r *sparseJSONReader) normalize(value byte) []byte {
	if r.inString {
		if r.escaped {
			r.escaped = false
		} else if value == '\\' {
			r.escaped = true
		} else if value == '"' {
			r.inString = false
		}
		return []byte{value}
	}
	if value == '"' {
		r.inString = true
		r.previousToken = value
		return []byte{value}
	}
	if value == ',' && (r.previousToken == '[' || r.previousToken == ',') {
		r.previousToken = value
		return []byte("null,")
	}
	if value == ']' && r.previousToken == ',' {
		r.previousToken = value
		return []byte("null]")
	}
	if !unicode.IsSpace(rune(value)) {
		r.previousToken = value
	}
	return []byte{value}
}

func decodeJSONValue(raw []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(newSparseJSONReader(bytes.NewReader(raw)))
	decoder.UseNumber()
	var value json.RawMessage
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("JSON+protobuf 包含多个根值")
		}
		return nil, err
	}
	return value, nil
}

func decodeGenerateItems(source io.Reader, consume func(json.RawMessage) error) error {
	decoder := json.NewDecoder(newSparseJSONReader(source))
	decoder.UseNumber()
	rootStart, err := decoder.Token()
	if err != nil {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error(), Err: err}
	}
	if delimiter, ok := rootStart.(json.Delim); !ok || delimiter != '[' {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根值不是数组"}
	}
	if !decoder.More() {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根数组缺少 field 1"}
	}
	fieldStart, err := decoder.Token()
	if err != nil {
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: err.Error(), Err: err}
	}
	// field 1 为 null（[null,[code,message]] 这种只有流尾状态的形状）：没有帧，照常读取流尾，流尾是错误时交出上游错误，
	// 否则由完成帧校验报“流结束前没有完成帧”（原先报“field 1 不是 repeated 数组”，丢掉了流尾的状态码）
	if fieldStart != nil {
		if delimiter, ok := fieldStart.(json.Delim); !ok || delimiter != '[' {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: "field 1 不是 repeated 数组"}
		}
		index := 0
		for decoder.More() {
			var raw json.RawMessage
			if err := decoder.Decode(&raw); err != nil {
				return &ProtocolEvidenceError{Method: "GenerateContent", Path: fmt.Sprintf("$[0][%d]", index), Detail: err.Error(), Err: err}
			}
			if err := consume(append(json.RawMessage(nil), raw...)); err != nil {
				return err
			}
			index++
		}
		if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
			if err != nil {
				return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: err.Error(), Err: err}
			}
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$[0]", Detail: "field 1 没有正常结束"}
		}
	}
	fieldIndex := 1
	for decoder.More() {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: fmt.Sprintf("$[%d]", fieldIndex), Detail: err.Error(), Err: err}
		}
		// 流尾 google.rpc 状态 [[...帧], [code, message, details]]：code 不为 0 时按状态码返回上游错误（429 冷却换号、5xx 重试、
		// 400 按请求错误）。原先跳过，最后都成了“流结束前没有完成帧”。完成帧之后的流尾错误同样返回，先于完成帧校验
		if fieldIndex == 1 {
			if err := playgroundTrailerError(raw); err != nil {
				return err
			}
		}
		fieldIndex++
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim(']') {
		if err != nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error(), Err: err}
		}
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "根数组没有正常结束"}
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: "响应后存在第二个根值", Raw: extra}
		}
		return &ProtocolEvidenceError{Method: "GenerateContent", Path: "$", Detail: err.Error(), Err: err}
	}
	return nil
}

// trailerError 解码流式响应列表之后的 google.rpc 状态 [code, message, details]：code 为 0 时返回 nil，否则按状态码换算的 HTTP 状态
// 返回带 ErrorInfo 与 RetryInfo 的 RPCError。流尾为 null、[] 或状态码为 null（JSON+protobuf 省略了默认值 0）时按 0（成功）处理，
// 写一行容错日志（tolerance 为 nil 时不写）；状态不是数组或状态码不是整数时返回协议错误。Playground GenerateContent、Build 代理与
// Interaction 共用
func trailerError(method string, raw json.RawMessage, tolerance *decodeTolerance) error {
	if isJSONNull(raw) {
		tolerance.note(method, "$[1]", "流尾状态为 null，按 0（成功）", raw)
		return nil
	}
	status, err := rawArray(raw, "$[1]", raw)
	if err != nil {
		return &ProtocolEvidenceError{Method: method, Path: "$[1]", Detail: "流尾状态无效", Raw: cloneRaw(raw)}
	}
	if len(status) == 0 || isJSONNull(status[0]) {
		tolerance.note(method, "$[1][0]", "流尾状态码省略或为 null，按 0（成功）", raw)
		return nil
	}
	code, err := rawInt64(status[0], "$[1][0]", raw)
	if err != nil {
		return withMethod(err, method)
	}
	if code == 0 {
		return nil
	}
	statusCode := http.StatusBadGateway
	if mapped, ok := interactionStatusHTTP[code]; ok {
		statusCode = mapped
	}
	// 按完整状态解码，保留 ErrorInfo 与 RetryInfo
	return DecodeRPCError(method, statusCode, raw)
}

// playgroundTrailerError 解码 Playground GenerateContent 的流尾：是 google.rpc 状态（数组，第一项为整数状态码）时按 trailerError 处理；
// null、空数组与其他形状照旧忽略，不因为没见过的流尾让原本正常的回复失败
func playgroundTrailerError(raw json.RawMessage) error {
	status, err := rawArray(raw, "$[1]", raw)
	if err != nil || len(status) == 0 {
		return nil
	}
	if _, err := rawInt64(status[0], "$[1][0]", raw); err != nil {
		return nil
	}
	return trailerError("GenerateContent", raw, nil)
}

// errMissingFinishFrame 表示生成流正常结束却没有完成帧（FrameDecoder.End、BuildStreamDecoder.End 的错误链上带着它）
var errMissingFinishFrame = errors.New("流结束前没有完成帧")

// StreamInterrupted 判断生成流在完成帧之前中断：正常结束却没有完成帧，或响应在 JSON 结构中途就结束了（解码报 io.ErrUnexpectedEOF、
// 读到 io.EOF，即响应被截断）。连接重置、HTTP/2 流被重置这类读取错误由调用方从读取器上判断（见 streamReadFailure）。
//
// 只认这几种：上游字段类型变了（json.UnmarshalTypeError）、函数调用缺少名称、JSON 语法错误等确定性的格式不符不算中断，
// 同一账号重发只会得到同样的结果；读取超时与取消也不算（由本服务的期限处理）
func StreamInterrupted(err error) bool {
	if errors.Is(err, errMissingFinishFrame) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var evidence *ProtocolEvidenceError
	return errors.As(err, &evidence) && evidence.Err == io.EOF
}

func rawArray(raw json.RawMessage, path string, evidence json.RawMessage) ([]json.RawMessage, error) {
	var values []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil {
		return nil, &ProtocolEvidenceError{Path: path, Detail: "期望数组", Raw: cloneEvidence(raw, evidence)}
	}
	return values, nil
}

func rawString(raw json.RawMessage, path string, evidence json.RawMessage) (string, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", &ProtocolEvidenceError{Path: path, Detail: "期望字符串", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawInt64(raw json.RawMessage, path string, evidence json.RawMessage) (int64, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望整数", Raw: cloneEvidence(raw, evidence)}
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望整数", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawFloat64(raw json.RawMessage, path string, evidence json.RawMessage) (float64, error) {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望数字", Raw: cloneEvidence(raw, evidence)}
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return 0, &ProtocolEvidenceError{Path: path, Detail: "期望数字", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawBool(raw json.RawMessage, path string, evidence json.RawMessage) (bool, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("1")) {
		return true, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("0")) {
		return false, nil
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, &ProtocolEvidenceError{Path: path, Detail: "期望布尔值", Raw: cloneEvidence(raw, evidence)}
	}
	return value, nil
}

func rawAt(values []json.RawMessage, index int) json.RawMessage {
	if index < 0 || index >= len(values) {
		return nil
	}
	return values[index]
}

func isJSONNull(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func cloneEvidence(primary json.RawMessage, fallback json.RawMessage) json.RawMessage {
	if len(primary) > 0 {
		return append(json.RawMessage(nil), primary...)
	}
	return append(json.RawMessage(nil), fallback...)
}
