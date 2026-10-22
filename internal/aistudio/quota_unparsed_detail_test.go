package aistudio

import (
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// geminiErrorDetailsName 为 geminiErrorDetailsType 去掉 type.googleapis.com/ 前缀
var geminiErrorDetailsName = strings.TrimPrefix(geminiErrorDetailsType, rpcDetailTypePrefix)

// unparsedQuotaStatus 返回带一个 GeminiApiErrorDetails 的裸 “Quota exceeded” 状态 [8, message, details]
func unparsedQuotaStatus(value string) string {
	return `[8,"Quota exceeded",[["` + geminiErrorDetailsType + `",` + value + `]]]`
}

// TestUnparsedGeminiAPIErrorDetails GeminiApiErrorDetails 的布局与已知样本不同、没有解析出违规项时，UnparsedDetail 为该详情值压缩后的原文
// （各种封装与 Build 流尾都一样），UnparsedDetailType 为去掉前缀的类型；分类照旧按裸限流逐级冷却
func TestUnparsedGeminiAPIErrorDetails(t *testing.T) {
	violation := `[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_model_per_user","GenerateRequestsPerProjectPerUserPerModelPerDay",[["model","gemini-3.1-pro"]],"50"]`
	nulls30 := geminiErrorNulls + ",null,null"
	for name, test := range map[string]struct{ value, want string }{
		// 违规项在下标 30 且多包了一层：下标 28 与第一层扫描都找不到 QuotaFailure 形状
		"下标 30 多一层": {
			value: "[" + nulls30 + ", [ [ [ " + violation + " ] ] ] ]",
			want:  "[" + nulls30 + ",[[[" + violation + "]]]]",
		},
		// 只有一个整数枚举
		"只有整数枚举": {value: "[ null ,\n null , 3 ]", want: "[null,null,3]"},
		// 下标 28 是不像违规项的嵌套数组（指标不带 “/”、限额名不带 “Per”）
		"不像违规项": {
			value: `[` + geminiErrorNulls + `,[[[null,null,null,"no-slash","NoMarker",[["model","m"]],"5"]]]]`,
			want:  `[` + geminiErrorNulls + `,[[[null,null,null,"no-slash","NoMarker",[["model","m"]],"5"]]]]`,
		},
	} {
		// “内层也稀疏”把 “null,” 都换成了 “,”，decodeJSONValue 补回 null 后原文不变
		for shape, rpcError := range decodeQuotaShapes(t, unparsedQuotaStatus(test.value)) {
			if rpcError.UnparsedDetail != test.want || rpcError.UnparsedDetailType != geminiErrorDetailsName {
				t.Fatalf("%s %s：UnparsedDetail=%q UnparsedDetailType=%q，期望 %q", name, shape, rpcError.UnparsedDetail, rpcError.UnparsedDetailType, test.want)
			}
			if len(rpcError.Metadata) != 0 {
				t.Fatalf("%s %s：不应解析出违规项：%v", name, shape, rpcError.Metadata)
			}
			cooldown, ok := QuotaCooldownForError(rpcError, time.Now())
			if !ok || cooldown.Kind != RateLimitKind || !cooldown.Escalate || cooldown.DetailTypes != geminiErrorDetailsName {
				t.Fatalf("%s %s：分类应不受影响：cooldown=%+v ok=%t", name, shape, cooldown, ok)
			}
		}
	}
}

// TestUnparsedDetailEmptyWhenParsed 解析出违规项时 UnparsedDetail 为空：A、C 两种现场样本、下标变化后扫描找到的违规项，
// 以及没有 GeminiApiErrorDetails 的 B 与 google.rpc.QuotaFailure
func TestUnparsedDetailEmptyWhenParsed(t *testing.T) {
	for name, status := range map[string]string{
		"A": freeDailyQuotaPayload, "B": userMinuteQuotaPayload, "C": paidDailyQuotaPayload,
		"下标变化":                 unparsedQuotaStatus(`[null,"x",[["a","b"]],[[[null,null,null,"a/b","PerDay",[["model","m"]],"10"]]]]`),
		"QuotaFailure 不解析也不保存": `[8,"Quota exceeded",[["type.googleapis.com/google.rpc.QuotaFailure",[[[1,2,3]]]]]]`,
		"其他私有详情不保存":            `[8,"Quota exceeded",[["type.googleapis.com/google.internal.other.v1.OtherDetails",[null,3]]]]`,
	} {
		for shape, rpcError := range decodeQuotaShapes(t, status) {
			if rpcError.UnparsedDetail != "" || rpcError.UnparsedDetailType != "" {
				t.Fatalf("%s %s：UnparsedDetail=%q UnparsedDetailType=%q，期望为空", name, shape, rpcError.UnparsedDetail, rpcError.UnparsedDetailType)
			}
		}
	}
}

// TestUnparsedDetailWithoutMetric 违规项解析出来了、却没有指标时（冷却日志同样没有“指标”、带“详情类型=”）照样保存原文：
// 只有 quota_id（症状与线上裸 “Quota exceeded” 那一行逐字段一致），或第一个违规项只有维度、带指标的违规项在后面
func TestUnparsedDetailWithoutMetric(t *testing.T) {
	for name, test := range map[string]struct {
		value    string
		metadata map[string]string
	}{
		"只有 quota_id": {
			value:    `[` + geminiErrorNulls + `,[[[null,null,null,null,"GenerateRequestsPerProjectPerUserPerModel"]]]]`,
			metadata: map[string]string{"quota_id": "GenerateRequestsPerProjectPerUserPerModel"},
		},
		"第一个违规项只有维度": {
			value: `[` + geminiErrorNulls + `,[[[null,null,null,null,null,[["location","global"]]],` +
				`[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_minute",null,[["model","m"]],"5"]]]]`,
			metadata: map[string]string{"quota_dimension_location": "global"},
		},
	} {
		for shape, rpcError := range decodeQuotaShapes(t, unparsedQuotaStatus(test.value)) {
			if rpcError.UnparsedDetail != test.value || rpcError.UnparsedDetailType != geminiErrorDetailsName {
				t.Fatalf("%s %s：UnparsedDetail=%q UnparsedDetailType=%q，期望 %q", name, shape, rpcError.UnparsedDetail, rpcError.UnparsedDetailType, test.value)
			}
			if !maps.Equal(rpcError.Metadata, test.metadata) {
				t.Fatalf("%s %s：Metadata=%v，期望 %v", name, shape, rpcError.Metadata, test.metadata)
			}
			cooldown, ok := QuotaCooldownForError(rpcError, time.Now())
			if !ok || cooldown.Kind != RateLimitKind || !cooldown.Escalate || cooldown.Metric != "" || cooldown.LimitValue != "" ||
				cooldown.Model != "" || cooldown.DetailTypes != geminiErrorDetailsName {
				t.Fatalf("%s %s：应与线上那一行一样没有指标：cooldown=%+v ok=%t", name, shape, cooldown, ok)
			}
		}
	}
}

// TestUnparsedDetailFirstOnly 同一个 429 带多个详情：任何详情（GeminiApiErrorDetails 或 ErrorInfo）解析出指标时为空；
// 都没有指标时优先保存第一个没有解析出违规项的 GeminiApiErrorDetails，都解析出违规项（没有指标）时保存第一个
func TestUnparsedDetailFirstOnly(t *testing.T) {
	gemini := func(value string) string { return `["` + geminiErrorDetailsType + `",` + value + `]` }
	quotaIDOnly := func(id string) string { return `[` + geminiErrorNulls + `,[[[null,null,null,null,"` + id + `"]]]]` }
	parsed := `[` + geminiErrorNulls + `,[[[null,null,null,"a/b","PerDay",[["model","m"]],"10"]]]]`
	other := `["type.googleapis.com/google.internal.other.v2.GeminiApiErrorDetails",[2]]`
	errorInfo := `["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","alkalimakersuite-pa.googleapis.com",` +
		`[["quota_metric","alkalimakersuite-pa.googleapis.com/generate_requests_global"],["quota_limit","GeneratePerMinutePerProjectPerUser"]]]]`
	for name, test := range map[string]struct{ details, wantType, want string }{
		"后面的 GeminiApiErrorDetails 解析出指标": {details: gemini(`[1]`) + "," + other + "," + gemini(parsed)},
		"ErrorInfo 给出指标":                  {details: gemini(`[1]`) + "," + errorInfo},
		"两个都没解析出违规项时只存第一个":                {details: gemini(`[1]`) + "," + other, wantType: geminiErrorDetailsName, want: "[1]"},
		"优先第一个没解析出违规项的": {
			details:  gemini(quotaIDOnly("PerProjectA")) + "," + other + "," + gemini(`[3]`),
			wantType: "google.internal.other.v2.GeminiApiErrorDetails", want: "[2]",
		},
		"都解析出违规项（没有指标）时存第一个": {
			details:  gemini(quotaIDOnly("PerProjectA")) + "," + gemini(quotaIDOnly("PerProjectB")),
			wantType: geminiErrorDetailsName, want: quotaIDOnly("PerProjectA"),
		},
	} {
		rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded",[`+test.details+`]]`))
		if rpcError.UnparsedDetail != test.want || rpcError.UnparsedDetailType != test.wantType {
			t.Fatalf("%s：UnparsedDetail=%q UnparsedDetailType=%q，期望 %q %q", name, rpcError.UnparsedDetail, rpcError.UnparsedDetailType, test.want, test.wantType)
		}
	}
	// 解析出违规项的详情照常读取，分类不受影响
	rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded",[`+gemini(`[1]`)+","+gemini(parsed)+`]]`))
	if cooldown, ok := QuotaCooldownForError(rpcError, time.Now()); rpcError.Metadata["quota_limit_value"] != "10" || !ok ||
		cooldown.Kind != DailyQuotaKind || cooldown.Metric != "b" || cooldown.LimitValue != "10" || cooldown.Model != "m" {
		t.Fatalf("解析出违规项的详情应照常读取：Metadata=%v cooldown=%+v", rpcError.Metadata, cooldown)
	}
}

// TestUnparsedDetailTruncated 超过 1200 字节的原文按 UTF-8 边界截断并注明总字节数；刚好 1200 字节时不截断
func TestUnparsedDetailTruncated(t *testing.T) {
	exact := `["` + strings.Repeat("a", unparsedDetailLimit-4) + `"]`
	if got := unparsedDetailText(json.RawMessage(exact)); got != exact {
		t.Fatalf("刚好 %d 字节不应截断：长度 %d", unparsedDetailLimit, len(got))
	}
	// 第 1200 个字节落在三字节汉字中间（汉字从第 2 个字节起，399 个字到第 1199 个字节）：截断点退到这个字之前
	long := `["` + strings.Repeat("额", 600) + `"]`
	rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(unparsedQuotaStatus(long)))
	suffix := "…（已截断，共 " + strconv.Itoa(len(long)) + " 字节）"
	head, found := strings.CutSuffix(rpcError.UnparsedDetail, suffix)
	if !found || !utf8.ValidString(rpcError.UnparsedDetail) || len(head) != 2+3*399 || !strings.HasPrefix(long, head) {
		t.Fatalf("截断结果不对：长度 %d，结尾 %q", len(rpcError.UnparsedDetail), rpcError.UnparsedDetail[max(0, len(rpcError.UnparsedDetail)-60):])
	}
	// 截断前先压缩：压缩后不超过上限时不截断
	spaced := "[ " + strings.Repeat("1 , ", 400) + "1 ]"
	if got := unparsedDetailText(json.RawMessage(spaced)); got != "["+strings.Repeat("1,", 400)+"1]" {
		t.Fatalf("应先压缩再判断长度：%q", got)
	}
}

// TestUnparsedDetailMalformed 畸形的详情不 panic：值不是数组时照样保存原文；压缩失败（直接调用时的非法 JSON）时保存去掉首尾空白的原样；
// 没有值的详情项只记类型
func TestUnparsedDetailMalformed(t *testing.T) {
	for _, value := range []string{`null`, `{}`, `"text"`, `[]`, `{"a": [1, 2]}`} {
		rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(unparsedQuotaStatus(value)))
		var compact strings.Builder
		for _, field := range strings.Fields(value) {
			compact.WriteString(field)
		}
		if rpcError.UnparsedDetail != compact.String() || rpcError.Code != 8 || rpcError.Message != "Quota exceeded" {
			t.Fatalf("%s：RPC error=%+v", value, rpcError)
		}
	}
	for raw, want := range map[string]string{" [1, ": "[1,", "": "", "\n{\"a\":}\n": `{"a":}`} {
		if got := unparsedDetailText(json.RawMessage(raw)); got != want {
			t.Fatalf("非法 JSON %q：%q，期望 %q", raw, got, want)
		}
	}
	bare := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded",[["`+geminiErrorDetailsType+`"]]]`))
	if bare.UnparsedDetail != "" || bare.UnparsedDetailType != "" || len(bare.DetailTypes) != 1 {
		t.Fatalf("没有值的详情项只记类型：%+v", bare)
	}
	// 经 validateRPCResponse 返回的 HTTP 429 同样带原文
	response := &RPCResponse{
		StatusCode: http.StatusTooManyRequests, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(unparsedQuotaStatus(`[3]`))),
	}
	_, err := validateRPCResponse("GenerateContent", response)
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.UnparsedDetail != "[3]" || rpcError.UnparsedDetailType != geminiErrorDetailsName {
		t.Fatalf("validateRPCResponse：err=%v", err)
	}
}
