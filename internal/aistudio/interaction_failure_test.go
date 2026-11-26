package aistudio

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"unicode"
)

// interactionFinalErrors 为带 field 28（下标 27）的最终 interaction 事件：status 为最终状态，failure 为 field 28 的原文（nil 时写 null），
// 用量写在 field 13（总数 10）
func interactionFinalErrors(status any, failure json.RawMessage) []any {
	interaction := make([]any, 28)
	interaction[1] = status
	interaction[12] = []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}
	if failure != nil {
		interaction[27] = failure
	}
	event := make([]any, 20)
	event[19] = []any{interaction}
	return event
}

// TestInteractionFinalStatusTable 最终 interaction 各状态在“已交出内容”（先有一段正文）与“未交出内容”时的结果：2、3 按 stop 正常结束，
// 6 按 max_tokens 正常结束（各出口的换算见 api 包的 TestInteractionMaxTokensExits），都照常交出用量；4 返回 502，能读出 field 28 时
// 错误码与说明接在 "interaction failed" 后面，读不出时为固定文案并写一行容错日志，缺位或 null 时为固定文案、不写日志；5 返回 502
// “interaction cancelled”；表外的状态（7）已交出内容时写容错日志并按 other 结束、未交出时按协议证据错误返回（这两条容错保持原样）
func TestInteractionFinalStatusTable(t *testing.T) {
	text := interactionDeltaEvent([]any{[]any{"Hello"}})
	type outcome struct {
		finish   string // 正常结束时的结束原因
		message  string // 返回 502 RPCError 时的文案
		evidence bool   // 返回协议证据错误
		logKind  string // 应恰好写一行的容错日志种类（空为不写任何容错日志）
	}
	for _, test := range []struct {
		name      string
		final     []any
		delivered outcome
		empty     outcome
	}{
		{name: "状态 2", final: interactionFinal(2, []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}),
			delivered: outcome{finish: "stop"}, empty: outcome{finish: "stop"}},
		{name: "状态 3", final: interactionFinal(3, []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}),
			delivered: outcome{finish: "stop"}, empty: outcome{finish: "stop"}},
		{name: "状态 6", final: interactionFinal(6, []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}),
			delivered: outcome{finish: "max_tokens"}, empty: outcome{finish: "max_tokens"}},
		{name: "状态 4 能读出原因", final: interactionFinalErrors(4, json.RawMessage(`[["INTERNAL","Backend error."]]`)),
			delivered: outcome{message: "interaction failed: INTERNAL: Backend error."},
			empty:     outcome{message: "interaction failed: INTERNAL: Backend error."}},
		{name: "状态 4 读不出原因", final: interactionFinalErrors(4, json.RawMessage(`"broken"`)),
			delivered: outcome{message: "interaction failed", logKind: "失败原因（field 28）读不出"},
			empty:     outcome{message: "interaction failed", logKind: "失败原因（field 28）读不出"}},
		{name: "状态 4 没有 field 28", final: interactionFinal(4, nil),
			delivered: outcome{message: "interaction failed"}, empty: outcome{message: "interaction failed"}},
		{name: "状态 4 field 28 为 null", final: interactionFinalErrors(4, nil),
			delivered: outcome{message: "interaction failed"}, empty: outcome{message: "interaction failed"}},
		{name: "状态 5", final: interactionFinalErrors(5, json.RawMessage(`[["CANCELLED","ignored"]]`)),
			delivered: outcome{message: "interaction cancelled"}, empty: outcome{message: "interaction cancelled"}},
		{name: "表外状态 7", final: interactionFinal(7, []any{4, nil, nil, nil, 6, nil, nil, nil, nil, 10}),
			delivered: outcome{finish: "other", logKind: "最终状态不在表里"}, empty: outcome{evidence: true}},
	} {
		for _, delivered := range []bool{true, false} {
			want, label := test.empty, test.name+"（未交出内容）"
			events := []any{test.final}
			if delivered {
				want, label = test.delivered, test.name+"（已交出内容）"
				events = []any{text, test.final}
			}
			log := captureToleranceLog(t)
			got, err := decodeInteractionTest(interactionTestStream(events...))
			kinds := []EventKind{}
			if delivered {
				kinds = append(kinds, EventText)
			}
			var rpcError *RPCError
			var evidence *ProtocolEvidenceError
			switch {
			case want.finish != "":
				kinds = append(kinds, EventUsage, EventFinish)
				if err != nil || !reflect.DeepEqual(eventKinds(got), kinds) || got[len(got)-1].FinishReason != want.finish ||
					got[len(got)-2].Usage.TotalTokens != 10 {
					t.Fatalf("%s：事件 = %+v，错误 = %v，期望以 %s 结束", label, got, err, want.finish)
				}
			case want.message != "":
				if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadGateway || rpcError.Code != 0 ||
					rpcError.Message != want.message || !reflect.DeepEqual(eventKinds(got), kinds) {
					t.Fatalf("%s：事件 = %+v，错误 = %#v，期望 502 %q", label, got, err, want.message)
				}
			case want.evidence:
				if !errors.As(err, &evidence) || !strings.Contains(evidence.Detail, "未识别的最终状态") {
					t.Fatalf("%s：错误 = %v，期望协议证据错误“未识别的最终状态”", label, err)
				}
			}
			if all := log.lines(""); want.logKind == "" && len(all) != 0 {
				t.Fatalf("%s：不应写容错日志：\n%s", label, log.buffer.String())
			} else if want.logKind != "" {
				if len(all) != 1 {
					t.Fatalf("%s：容错日志应只有一行：\n%s", label, log.buffer.String())
				}
				log.expectOne(t, want.logKind)
			}
		}
	}
}

// TestInteractionFailureReason field 28 的读取与清洗：多项按 "; " 连接、只有错误码或只有说明的写那一项、整数错误码按原文；去掉内部标记，
// 控制字符与连续空白合成一个空格，去掉格式字符（零宽、方向控制），邮箱打码（含 JSON 转义的 @、网址里的 %40、被零宽字符拆开的），像 API Key
// 的片段打码，超过上限截断；读不出的条目（含错误码与说明都没有、下标 1 之后却有内容的）跳过、读出的照常附上并写一行容错日志（原文同样打码）；
// 缺位、null、[] 与没有内容的条目为固定文案、不写日志，错误码或说明读出时下标 1 之后的内容不管；原因像输入超过上限时不附上（免得改判成 400）
func TestInteractionFailureReason(t *testing.T) {
	fakeKey := "AQ." + strings.Repeat("T", 24)
	// leaks 判断文本里有没有露出完整邮箱或 Key：先去掉格式字符再找，被零宽字符拆开的邮箱同样算露出
	leaks := func(text string) bool {
		visible := strings.Map(func(r rune) rune {
			if unicode.Is(unicode.Cf, r) {
				return -1
			}
			return r
		}, text)
		return strings.Contains(visible, "example.com") || strings.Contains(visible, fakeKey)
	}
	long := strings.Repeat("x", interactionFailureReasonLimit+50)
	for _, test := range []struct {
		name    string
		raw     string
		message string
		logKind string
	}{
		{name: "多项", raw: `[["INTERNAL","Backend error."],[null,"Second."],["UNAVAILABLE"],[]]`,
			message: "interaction failed: INTERNAL: Backend error.; Second.; UNAVAILABLE"},
		{name: "整数错误码", raw: `[[13,"Boom."]]`, message: "interaction failed: 13: Boom."},
		{name: "内部标记与空白", raw: `[["INTERNAL","generic::internal: Line one\n\tline\u0000two [original: x] (qos=y)"]]`,
			message: "interaction failed: INTERNAL: Line one line two"},
		{name: "邮箱打码", raw: `[["PERMISSION_DENIED","Account someone.name@example.com and other@example.com lack access"]]`,
			message: "interaction failed: PERMISSION_DENIED: Account so***@*** and ot***@*** lack access"},
		{name: "网址里 %40 的邮箱打码", raw: `[["PERMISSION_DENIED","Open https://support.test/?authuser=someone%40example.com to fix"]]`,
			message: "interaction failed: PERMISSION_DENIED: Open https://support.test/?authuser=so***@*** to fix"},
		{name: "零宽字符与方向控制", raw: `[["PERMISSION_DENIED","Account some\u200bone@exa\u200bmple.com\u202e lacks\u00ad access"]]`,
			message: "interaction failed: PERMISSION_DENIED: Account so***@*** lacks access"},
		{name: "Key 打码", raw: `[["INVALID_ARGUMENT","key ` + fakeKey + ` rejected"]]`,
			message: "interaction failed: INVALID_ARGUMENT: key <key:…TTTT> rejected"},
		{name: "截断", raw: `[["INTERNAL","` + long + `"]]`,
			message: "interaction failed: " + string([]rune("INTERNAL: " + long)[:interactionFailureReasonLimit]) + "…"},
		{name: "有条目读不出", raw: `[["INTERNAL","Kept."],{"code":1},[{"x":1},"Detail only."],"bad"]`,
			message: "interaction failed: INTERNAL: Kept.; Detail only.", logKind: "有条目读不出，已跳过"},
		{name: "整段读不出", raw: `{"code":"INTERNAL","message":"someone@example.com"}`,
			message: "interaction failed", logKind: "读不出，按固定文案报错"},
		{name: "整段读不出（原文里 %40 与零宽字符拆开的邮箱）",
			raw:     `{"url":"?authuser=someone%40example.com","who":"other@exa\u200bmple.com"}`,
			message: "interaction failed", logKind: "读不出，按固定文案报错"},
		{name: "原因不在下标 0、1", raw: `[[null,null,"Reason somewhere@example.com"]]`,
			message: "interaction failed", logKind: "读不出，按固定文案报错"},
		{name: "部分条目的原因不在下标 0、1", raw: `[["INTERNAL","Kept."],[null,null,{"message":"x"}]]`,
			message: "interaction failed: INTERNAL: Kept.", logKind: "有条目读不出，已跳过"},
		{name: "读出时下标 1 之后的内容不管", raw: `[["INTERNAL","Kept.",{"extra":1}],[null,"Second.","more"]]`,
			message: "interaction failed: INTERNAL: Kept.; Second."},
		{name: "null", raw: `null`, message: "interaction failed"},
		{name: "空列表", raw: `[]`, message: "interaction failed"},
		{name: "没有内容的条目", raw: `[[],[null,null],[null,null,null],null]`, message: "interaction failed"},
		{name: "像输入超过上限", raw: `[["INVALID_ARGUMENT","The input token count exceeds the maximum number of tokens allowed 131072."]]`,
			message: "interaction failed", logKind: "像输入超过上限"},
	} {
		log := captureToleranceLog(t)
		tolerance := newDecodeTolerance(nil, "")
		err := interactionFailedError(json.RawMessage(test.raw), tolerance)
		var rpcError *RPCError
		if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusBadGateway || rpcError.Code != 0 ||
			rpcError.Method != interactionMethod || rpcError.Message != test.message {
			t.Fatalf("%s：错误 = %#v\n期望文案 %q", test.name, err, test.message)
		}
		if InputTokenLimitError(err) {
			t.Fatalf("%s：原因不应让错误按输入超过上限改判：%v", test.name, err)
		}
		if leaks(err.Error()) {
			t.Fatalf("%s：错误里不应有完整邮箱或 Key：%q", test.name, err)
		}
		lines := log.lines("")
		if test.logKind == "" {
			if len(lines) != 0 {
				t.Fatalf("%s：不应写容错日志：\n%s", test.name, log.buffer.String())
			}
			continue
		}
		line := log.expectOne(t, test.logKind)
		if len(lines) != 1 || line["path"] != interactionFailurePath {
			t.Fatalf("%s：容错日志 = %v", test.name, lines)
		}
		if raw, _ := line["raw"].(string); leaks(raw) || strings.Contains(raw, "\u200b") {
			t.Fatalf("%s：容错日志原文应已打码：%q", test.name, raw)
		}
	}
}
