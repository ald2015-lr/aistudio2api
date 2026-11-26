package aistudio

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"unicode"

	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
)

// Interaction 失败原因（最终状态 4）
//
// 最终 interaction 的 field 28（下标 27）为错误列表，每项 [<错误码>, <说明>]（按上游 Mag1cFall/AIStudio2API ddb1af5 的
// decodeInteractionErrors 移植，字段来自它对官网前端的阅读，我们的抓包里还没见过）。状态 4 时把读出的错误码与说明附在 502 的文案后面：
// "interaction failed: <错误码>: <说明>"，多项以 "; " 分隔（原先只有 "interaction failed"，看不出上游为什么失败）。
//
// 文案只进管理日志与排查记录：API 层对上游 5xx 照旧按官方通用措辞对外（500 INTERNAL "An internal error has occurred…"，不带原因，
// 见 api.upstreamPublicError），所以原因里即使有 new-api 自动禁用渠道的单词（permission、quota 等）也不会到客户端。HTTP 状态（502）、
// 协议错误码（无）、换号与冷却、403 分类都与原先相同；唯一按文案、不看状态码的判断是输入超过上限（InputTokenLimitError，会改为 400、
// 不换号），原因恰好是这类原文时不附在文案里、改写一行容错日志，免得原因改变分类。
//
// 附上前先清洗：去掉内部标记（CleanUpstreamMessage）、去掉格式字符（零宽、方向控制等 Unicode Cf 类，免得拆开邮箱躲过打码、在日志里
// 改变显示方向）、控制字符与连续空白合成一个空格、邮箱打码为前两位 + ***@***（与页面快照、tools/diag_*.sh 的打码规则一致，另认网址里
// 编码成 %40 的 @，同 tools/trace_pack.sh）、像 API Key 的片段打码（camoufoxnative.RedactStudioKeys），整段超过
// interactionFailureReasonLimit 字截断。field 28 缺位、为 null 或没有条目时用固定文案 "interaction failed"，不写日志；有值却读不出
// （不是数组、条目不是数组、错误码或说明是对象或数组，或条目的错误码与说明都没有、下标 1 之后却还有内容，即原因不在上游读的位置）时
// 跳过读不出的部分、读出的照常附上，并写一行容错日志（原文同样打码）

// interactionFailedMessage 为最终状态 4 的固定文案：读不出原因时只有这一句，读出时原因接在它后面
const interactionFailedMessage = "interaction failed"

// interactionFailureReasonLimit 为附在失败文案后面的原因的最大字数（与 403 登记的原文上限相同）
const interactionFailureReasonLimit = 300

// interactionFailurePath 为 field 28 的协议位置
const interactionFailurePath = "$event[19][0][27]"

// interactionEmailPattern 匹配原因与容错原文里的邮箱，打码为前两位 + ***@***（与页面快照、tools/diag_*.sh 的打码规则一致；另认网址里
// 编码成 %40 的 @，如 authuser=name%40example.com，同 tools/trace_pack.sh 的邮箱规则）
var interactionEmailPattern = regexp.MustCompile(`([A-Za-z0-9._%+-]{1,2})[A-Za-z0-9._%+-]*(?:@|%40)(?:[A-Za-z0-9-]+\.)+[A-Za-z]{2,}`)

// interactionFailedError 返回最终状态 4 的错误：502 RPCError，文案为固定文案，读出 field 28 的原因时接在后面（规则见文件开头）
func interactionFailedError(raw json.RawMessage, tolerance *decodeTolerance) error {
	message := interactionFailedMessage
	reason, readable := decodeInteractionErrors(raw)
	switch {
	case !readable && reason == "":
		tolerance.note(interactionMethod, interactionFailurePath, "失败原因（field 28）读不出，按固定文案报错", redactInteractionFailure(raw))
	case !readable:
		tolerance.note(interactionMethod, interactionFailurePath, "失败原因（field 28）有条目读不出，已跳过", redactInteractionFailure(raw))
	}
	if reason != "" {
		withReason := message + ": " + reason
		if InputTokenLimitError(&RPCError{Method: interactionMethod, StatusCode: http.StatusBadGateway, Message: withReason}) {
			// 原因是输入超过上限的原文：附上会让它按输入过长改判（400、不换号），只写容错日志，文案保持固定
			tolerance.note(interactionMethod, interactionFailurePath, "失败原因（field 28）像输入超过上限，不附在文案里，按固定文案报错",
				toleranceText("%s", reason))
		} else {
			message = withReason
		}
	}
	return &RPCError{Method: interactionMethod, StatusCode: http.StatusBadGateway, Message: message}
}

// decodeInteractionErrors 读出 field 28 的错误码与说明，清洗并截断后以 "<错误码>: <说明>" 写出、多项以 "; " 分隔；只有其中一项时只写那一项，
// 都没有的条目跳过。readable 为假表示有读不出的部分（不是数组、条目不是数组、错误码或说明是对象或数组，或错误码与说明都没有、下标 1 之后
// 却还有内容），读出的部分照常返回
func decodeInteractionErrors(raw json.RawMessage) (reason string, readable bool) {
	if isJSONNull(raw) {
		return "", true
	}
	items, err := rawArray(raw, interactionFailurePath, raw)
	if err != nil {
		return "", false
	}
	readable = true
	parts := make([]string, 0, len(items))
	for _, item := range items {
		if isJSONNull(item) {
			continue
		}
		fields, err := rawArray(item, interactionFailurePath+"[]", raw)
		if err != nil {
			readable = false
			continue
		}
		code, codeOK := interactionFailureText(rawAt(fields, 0))
		detail, detailOK := interactionFailureText(rawAt(fields, 1))
		readable = readable && codeOK && detailOK
		switch {
		case code != "" && detail != "":
			parts = append(parts, code+": "+detail)
		case detail != "":
			parts = append(parts, detail)
		case code != "":
			parts = append(parts, code)
		case interactionFailureHasExtra(fields):
			// 错误码与说明都没有，下标 1 之后却还有内容：原因多半不在上游读的位置（结构与上游 ddb1af5 的阅读不同），算读不出、写容错日志，
			// 免得静默落到固定文案
			readable = false
		}
	}
	reason = strings.Join(parts, "; ")
	if runes := []rune(reason); len(runes) > interactionFailureReasonLimit {
		reason = string(runes[:interactionFailureReasonLimit]) + "…"
	}
	return reason, readable
}

// interactionFailureHasExtra 判断条目在下标 1 之后有没有不是 null 的值
func interactionFailureHasExtra(fields []json.RawMessage) bool {
	for index := 2; index < len(fields); index++ {
		if !isJSONNull(fields[index]) {
			return true
		}
	}
	return false
}

// interactionFailureText 读出错误码或说明并清洗：字符串按原文，数字与布尔值按原文转成文本（错误码可能写成整数），null 与缺位为空串；
// 对象与数组读不出，ok 为假
func interactionFailureText(raw json.RawMessage) (string, bool) {
	text, _, ok := lenientText(raw)
	if !ok {
		return "", false
	}
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, dropFormatRunes(CleanUpstreamMessage(text)))
	text = strings.Join(strings.Fields(text), " ")
	return redactInteractionText(text), true
}

// dropFormatRunes 去掉格式字符（Unicode Cf 类：零宽空格与连接符、方向控制、软连字符等）：夹在邮箱里会让打码认不出，方向控制还会改变
// 日志的显示顺序
func dropFormatRunes(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
}

// redactInteractionText 给原因与容错原文打码：邮箱（含 %40 写法）打码为前两位 + ***@***，像 API Key 的片段打码
func redactInteractionText(text string) string {
	text = interactionEmailPattern.ReplaceAllString(text, "${1}***@***")
	return camoufoxnative.RedactStudioKeys(text)
}

// redactInteractionFailure 返回写进容错日志的 field 28 原文：先按 JSON 解码再编码（@ 这类转义还原成原字符，打码才认得出），
// 去掉格式字符后把邮箱与像 API Key 的片段打码；读不出 JSON 时按原文打码
func redactInteractionFailure(raw json.RawMessage) json.RawMessage {
	if raw == nil {
		return nil
	}
	text := string(raw)
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) == nil {
		var encoded bytes.Buffer
		encoder := json.NewEncoder(&encoded)
		encoder.SetEscapeHTML(false)
		if encoder.Encode(value) == nil {
			text = strings.TrimSpace(encoded.String())
		}
	}
	return json.RawMessage(redactInteractionText(dropFormatRunes(text)))
}
