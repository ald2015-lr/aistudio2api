package aistudio

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// 三种上游 429 的现场原文（grpc-web JSON 数组的状态 [8, message, details]）：
//   - A：free 号每日限额（每天 10 次），详情只有 GeminiApiErrorDetails，文案带 “Please retry in <时长>”；
//   - B：pro/ultra 号每分钟限额（全模型共用，每分钟 12 次），即 userMinuteQuotaPayload（ErrorInfo 与 Help）；
//   - C：pro/ultra 号每日限额（每天 50 次），详情同 A 的形状（维度顺序不同），文案是 “You've reached your usage limit”，没有恢复时间。
// GeminiApiErrorDetails 的值是数组，下标 28（前 28 个元素为 null）是 QuotaFailure 形状 [[违规项...]]

// geminiErrorDetailsType 为 AI Studio 网页后端的私有详情类型
const geminiErrorDetailsType = "type.googleapis.com/google.internal.alkali.applications.makersuite.v1.GeminiApiErrorDetails"

// geminiErrorNulls 为 GeminiApiErrorDetails 下标 28 之前的 28 个 null
const geminiErrorNulls = "null,null,null,null,null,null,null,null,null,null,null,null,null,null," +
	"null,null,null,null,null,null,null,null,null,null,null,null,null,null"

// freeDailyQuotaPayload 为 A：free 号 gemini-3.1-pro 每日 10 次
const freeDailyQuotaPayload = `[8,"You exceeded your current quota, please check your plan and billing details. For more information on this error, head to: https://ai.google.dev/gemini-api/docs/rate-limits. To monitor your current usage, head to: https://ai.dev/rate-limit. \n* Quota exceeded for metric: generativelanguage.googleapis.com/generate_requests_per_model_per_user, limit: 10, model: gemini-3.1-pro\nPlease retry in 16h6m4.716518794s.",` +
	`[["` + geminiErrorDetailsType + `",[` + geminiErrorNulls + `,[[[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_model_per_user",` +
	`"GenerateRequestsPerProjectPerUserPerModelPerDay",[["location","global"],["model","gemini-3.1-pro"]],"10"]]]]]]]`

// paidDailyQuotaPayload 为 C：pro/ultra 号 gemini-3.1-pro 每日 50 次
const paidDailyQuotaPayload = `[8,"You've reached your usage limit. Retry with an alternative Gemini model or upgrade your Google AI plan for higher limits. To view plans head to https://one.google.com/ai.",` +
	`[["` + geminiErrorDetailsType + `",[` + geminiErrorNulls + `,[[[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_model_per_user",` +
	`"GenerateRequestsPerProjectPerUserPerModelPerDay",[["model","gemini-3.1-pro"],["location","global"]],"50"]]]]]]]`

// freeDailyRetryIn 为 A 文案里的恢复时间
const freeDailyRetryIn = 16*time.Hour + 6*time.Minute + 4716518794*time.Nanosecond

// quotaPayloadShapes 返回同一状态的几种封装：独立状态、流式封装 [null,[...]]、稀疏的流式封装 [,[...]]，
// 以及内层的 null 也省略的稀疏形状（decodeJSONValue 把稀疏数组补成 null）
func quotaPayloadShapes(status string) map[string]string {
	return map[string]string{
		"独立状态":    status,
		"null 封装": "[null," + status + "]",
		"稀疏封装":    "[," + status + "]",
		"内层也稀疏":   "[," + strings.ReplaceAll(status, "null,", ",") + "]",
	}
}

// decodeQuotaShapes 用 DecodeRPCError 解码各种封装，独立状态另经 Build 流尾解码一次
func decodeQuotaShapes(t *testing.T, status string) map[string]*RPCError {
	t.Helper()
	decoded := make(map[string]*RPCError)
	for name, payload := range quotaPayloadShapes(status) {
		decoded[name] = DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(payload))
	}
	var trailer *RPCError
	if err := buildTrailerError(json.RawMessage(status)); !errors.As(err, &trailer) {
		t.Fatalf("Build 流尾错误=%v", err)
	}
	decoded["Build 流尾"] = trailer
	return decoded
}

// TestQuotaRecognitionFreeDaily A：GeminiApiErrorDetails 解析出指标、限额名、维度与限额值，文案的 “retry in” 作为恢复时间；
// 按模型的每日限额（非全局、不逐级），恢复时间为太平洋时间次日零点与 now+retry in 中较晚的一个
func TestQuotaRecognitionFreeDaily(t *testing.T) {
	for name, rpcError := range decodeQuotaShapes(t, freeDailyQuotaPayload) {
		if rpcError.Code != 8 || rpcError.RetryDelay != freeDailyRetryIn || !strings.Contains(rpcError.Message, "\n* Quota exceeded for metric") {
			t.Fatalf("%s：RPC error=%+v", name, rpcError)
		}
		for key, want := range map[string]string{
			"quota_metric":             "generativelanguage.googleapis.com/generate_requests_per_model_per_user",
			"quota_id":                 "GenerateRequestsPerProjectPerUserPerModelPerDay",
			"quota_limit_value":        "10",
			"quota_dimension_model":    "gemini-3.1-pro",
			"quota_dimension_location": "global",
		} {
			if got := rpcError.Metadata[key]; got != want {
				t.Fatalf("%s：Metadata[%s]=%q，期望 %q（全部 %v）", name, key, got, want, rpcError.Metadata)
			}
		}
		if want := []string{strings.TrimPrefix(geminiErrorDetailsType, "type.googleapis.com/")}; !slices.Equal(rpcError.DetailTypes, want) {
			t.Fatalf("%s：DetailTypes=%v，期望 %v", name, rpcError.DetailTypes, want)
		}
		for _, now := range []time.Time{
			// 08:59 PDT：now+16h6m 晚于太平洋时间次日零点（零点在前），按 retry in
			time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC),
			// 07:53 PDT：now+16h6m 为当天 23:59 PDT，早于次日零点（零点在后），按次日零点
			time.Date(2026, 10, 22, 14, 53, 0, 0, time.UTC),
		} {
			cooldown, ok := QuotaCooldownForError(rpcError, now)
			if !ok {
				t.Fatalf("%s now=%s：429 没有产生冷却", name, now)
			}
			want := nextQuotaDay(now)
			if retryAt := now.Add(freeDailyRetryIn); retryAt.After(want) {
				want = retryAt
			}
			if cooldown.Kind != DailyQuotaKind || cooldown.Global || cooldown.Escalate || !cooldown.Until.Equal(want) ||
				cooldown.LimitValue != "10" || cooldown.Metric != "generate_requests_per_model_per_user" || cooldown.Model != "gemini-3.1-pro" ||
				cooldown.DetailTypes != strings.TrimPrefix(geminiErrorDetailsType, "type.googleapis.com/") {
				t.Fatalf("%s now=%s：cooldown=%+v，期望每日限额、非全局、Until=%s", name, now, cooldown, want)
			}
		}
	}
	// 两个 now 分别落在零点前后，两种结果都要覆盖到
	before, after := time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC), time.Date(2026, 10, 22, 14, 53, 0, 0, time.UTC)
	if !before.Add(freeDailyRetryIn).After(nextQuotaDay(before)) || after.Add(freeDailyRetryIn).After(nextQuotaDay(after)) {
		t.Fatal("测试时间没有覆盖零点在前与零点在后两种情况")
	}
}

// TestQuotaRecognitionPaidDaily C：同样解析 GeminiApiErrorDetails（维度顺序不同），“reached your usage limit” 归为每日，
// “Retry with an alternative” 不当作恢复时间：按模型的每日限额冷却到太平洋时间次日零点，不逐级
func TestQuotaRecognitionPaidDaily(t *testing.T) {
	now := time.Date(2026, 10, 22, 15, 59, 0, 0, time.UTC)
	for name, rpcError := range decodeQuotaShapes(t, paidDailyQuotaPayload) {
		if rpcError.RetryDelay != 0 || rpcError.Metadata["quota_limit_value"] != "50" ||
			rpcError.Metadata["quota_dimension_model"] != "gemini-3.1-pro" || rpcError.Metadata["quota_dimension_location"] != "global" {
			t.Fatalf("%s：RPC error=%+v", name, rpcError)
		}
		cooldown, ok := QuotaCooldownForError(rpcError, now)
		if !ok || cooldown.Kind != DailyQuotaKind || cooldown.Global || cooldown.Escalate || !cooldown.Until.Equal(nextQuotaDay(now)) ||
			cooldown.LimitValue != "50" || cooldown.Metric != "generate_requests_per_model_per_user" || cooldown.Model != "gemini-3.1-pro" {
			t.Fatalf("%s：cooldown=%+v ok=%t，期望每日限额、非全局、冷却到次日零点", name, cooldown, ok)
		}
	}
	// 只有文案、没有详情时也按每日限额（文案证据），仍不逐级
	message := quotaError("You've reached your usage limit. Retry with an alternative Gemini model or upgrade your Google AI plan for higher limits.", nil, 0)
	cooldown, ok := QuotaCooldownForError(message, now)
	if !ok || cooldown.Kind != DailyQuotaKind || cooldown.Global || cooldown.Escalate || !cooldown.Until.Equal(nextQuotaDay(now)) {
		t.Fatalf("只有 C 的文案：cooldown=%+v ok=%t", cooldown, ok)
	}
	// 更宽的 “usage limit” 不算每日证据
	if dailyQuotaEvidence("usage limit exceeded for this minute") {
		t.Fatal("“usage limit” 不应算每日证据")
	}
}

// TestQuotaRecognitionMinuteUnchanged B 在三种封装下仍是全局分钟限额、上限 12，没有模型维度；有指标时不需要详情类型
func TestQuotaRecognitionMinuteUnchanged(t *testing.T) {
	now := time.Unix(1792587739, 0).Add(20 * time.Second)
	for name, rpcError := range decodeQuotaShapes(t, userMinuteQuotaPayload) {
		cooldown, ok := QuotaCooldownForError(rpcError, now)
		if !ok || cooldown.Kind != MinuteQuotaKind || !cooldown.Global || cooldown.Escalate || cooldown.LimitValue != "12" ||
			cooldown.Metric != "generate_requests_global" || cooldown.Model != "" || !cooldown.Until.Equal(time.Unix(1792587739, 0).Add(time.Minute)) {
			t.Fatalf("%s：cooldown=%+v ok=%t", name, cooldown, ok)
		}
		if !slices.Equal(rpcError.DetailTypes, []string{"google.rpc.ErrorInfo", "google.rpc.Help"}) || rpcError.RetryDelay != 0 {
			t.Fatalf("%s：DetailTypes=%v RetryDelay=%s", name, rpcError.DetailTypes, rpcError.RetryDelay)
		}
	}
}

// TestRetryDelayFromMessage 文案里的 “retry in <Go 时长>”：结尾句点不吞进时长，大小写不敏感，超过 24 小时截到 24 小时，
// “Retry with …”、没有数字或时长为 0 时不采纳
func TestRetryDelayFromMessage(t *testing.T) {
	for message, want := range map[string]time.Duration{
		"Please retry in 16h6m4.716518794s.":     freeDailyRetryIn,
		"Please retry in 30s":                    30 * time.Second,
		"RETRY IN 1.5s, thanks":                  1500 * time.Millisecond,
		"retry in 250ms.":                        250 * time.Millisecond,
		"retry in 5µs":                           5 * time.Microsecond,
		"retry in 48h":                           maxRetryDelay,
		"retry in 0s":                            0,
		"Please retry in a few minutes":          0,
		"Retry with an alternative Gemini model": 0,
		"noretry in 30s":                         0,
		"":                                       0,
	} {
		if got := retryDelayFromMessage(message); got != want {
			t.Fatalf("retryDelayFromMessage(%q)=%s，期望 %s", message, got, want)
		}
	}
}

// TestRetryDelayPriority 恢复时间的优先级：RetryInfo > 文案里的 “retry in” > HTTP Retry-After
func TestRetryDelayPriority(t *testing.T) {
	withRetryInfo := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(
		`[8,"Please retry in 5m.",[["type.googleapis.com/google.rpc.RetryInfo",[[12]]]]]`,
	))
	if withRetryInfo.RetryDelay != 12*time.Second {
		t.Fatalf("RetryInfo 应优先于文案：%s", withRetryInfo.RetryDelay)
	}
	for name, test := range map[string]struct {
		body string
		want time.Duration
	}{
		"文案优先于 Retry-After":  {`[8,"Please retry in 5m."]`, 5 * time.Minute},
		"没有文案时用 Retry-After": {`[8,"Quota exceeded"]`, 45 * time.Second},
	} {
		response := &RPCResponse{
			StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"45"}},
			Body: io.NopCloser(strings.NewReader(test.body)),
		}
		_, err := validateRPCResponse("GenerateContent", response)
		var rpcError *RPCError
		if !errors.As(err, &rpcError) || rpcError.RetryDelay != test.want {
			t.Fatalf("%s：err=%v，期望 RetryDelay=%s", name, err, test.want)
		}
	}
	// A 的文案恢复时间优先于 Retry-After
	response := &RPCResponse{
		StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"45"}},
		Body: io.NopCloser(strings.NewReader(freeDailyQuotaPayload)),
	}
	_, err := validateRPCResponse("GenerateContent", response)
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.RetryDelay != freeDailyRetryIn {
		t.Fatalf("A 经 validateRPCResponse：err=%v", err)
	}
}

// TestQuotaFailureDimensionsAndValue google.rpc.QuotaFailure 违规项的维度与限额值（数字或字符串）同样读出；
// ErrorInfo 已给出的字段不被违规项覆盖（不论两者先后）；有每日违规项时取它
func TestQuotaFailureDimensionsAndValue(t *testing.T) {
	quotaFailure := `["type.googleapis.com/google.rpc.QuotaFailure",[[` +
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_requests","GenerateRequestsPerMinutePerProjectPerModel-FreeTier",[["model","gemini-x-flash"]],15],` +
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_requests","GenerateRequestsPerDayPerProjectPerModel-FreeTier",[["model","gemini-x-pro"],["location","global"]],250]` +
		`]]]`
	decoded := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"You exceeded your current quota.",[`+quotaFailure+`]]`))
	for key, want := range map[string]string{
		"quota_id": "GenerateRequestsPerDayPerProjectPerModel-FreeTier", "quota_limit_value": "250",
		"quota_dimension_model": "gemini-x-pro", "quota_dimension_location": "global",
	} {
		if got := decoded.Metadata[key]; got != want {
			t.Fatalf("Metadata[%s]=%q，期望 %q（全部 %v）", key, got, want, decoded.Metadata)
		}
	}
	if cooldown, ok := QuotaCooldownForError(decoded, time.Now()); !ok || cooldown.Kind != DailyQuotaKind || cooldown.LimitValue != "250" || cooldown.Model != "gemini-x-pro" {
		t.Fatalf("cooldown=%+v ok=%t", cooldown, ok)
	}

	errorInfo := `["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","googleapis.com",[["quota_limit_value","12"],["quota_metric","x.googleapis.com/from_error_info"]]]]`
	for name, details := range map[string]string{
		"ErrorInfo 在前": errorInfo + "," + quotaFailure,
		"ErrorInfo 在后": quotaFailure + "," + errorInfo,
	} {
		rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"quota",[`+details+`]]`))
		if rpcError.Metadata["quota_limit_value"] != "12" || rpcError.Metadata["quota_metric"] != "x.googleapis.com/from_error_info" ||
			rpcError.Metadata["quota_id"] != "GenerateRequestsPerDayPerProjectPerModel-FreeTier" || rpcError.Metadata["quota_dimension_model"] != "gemini-x-pro" {
			t.Fatalf("%s：ErrorInfo 的字段应保留、其余由违规项补上：%v", name, rpcError.Metadata)
		}
		if !slices.Equal(rpcError.DetailTypes, []string{"google.rpc.ErrorInfo", "google.rpc.QuotaFailure"}) &&
			!slices.Equal(rpcError.DetailTypes, []string{"google.rpc.QuotaFailure", "google.rpc.ErrorInfo"}) {
			t.Fatalf("%s：DetailTypes=%v", name, rpcError.DetailTypes)
		}
	}
}

// TestGeminiAPIErrorDetailsScan 下标 28 不是 QuotaFailure 形状时扫描第一层其余元素；不像违规项的嵌套数组（指标不带 “/”、
// 限额名不带 “Per”）不误认；类型按后缀匹配
func TestGeminiAPIErrorDetailsScan(t *testing.T) {
	violation := `[[[null,null,null,"generativelanguage.googleapis.com/generate_requests_per_model_per_user","GenerateRequestsPerProjectPerUserPerModelPerDay",[["model","gemini-3.1-pro"]],"10"]]]`
	decode := func(typeURL string, value string) *RPCError {
		return DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"quota",[["`+typeURL+`",`+value+`]]]`))
	}
	moved := decode(geminiErrorDetailsType, `[null,"x",[["a","b"]],`+violation+`]`)
	if moved.Metadata["quota_limit_value"] != "10" || moved.Metadata["quota_dimension_model"] != "gemini-3.1-pro" {
		t.Fatalf("下标变化时应扫描找到违规项：%v", moved.Metadata)
	}
	renamed := decode("type.googleapis.com/google.internal.other.v2.GeminiApiErrorDetails", `[`+geminiErrorNulls+`,`+violation+`]`)
	if renamed.Metadata["quota_id"] != "GenerateRequestsPerProjectPerUserPerModelPerDay" {
		t.Fatalf("包名变化时应按后缀识别：%v", renamed.Metadata)
	}
	lookalike := decode(geminiErrorDetailsType, `[[[["a","b","c","no-slash","NoMarker",[["model","m"]],"5"]]],[[[1,2,3]]]]`)
	if len(lookalike.Metadata) != 0 {
		t.Fatalf("不像违规项的嵌套数组不应被读取：%v", lookalike.Metadata)
	}
	other := decode("type.googleapis.com/google.internal.other.v1.OtherDetails", `[`+geminiErrorNulls+`,`+violation+`]`)
	if len(other.Metadata) != 0 || !slices.Equal(other.DetailTypes, []string{"google.internal.other.v1.OtherDetails"}) {
		t.Fatalf("未识别的类型只记进 DetailTypes：Metadata=%v DetailTypes=%v", other.Metadata, other.DetailTypes)
	}
}

// TestQuotaDetailsMalformed 畸形的详情不 panic，解析不了的字段只跳过；未识别的类型去重后记进 DetailTypes，
// 裸 429（没有指标）照旧按限流逐级冷却并带上详情类型
func TestQuotaDetailsMalformed(t *testing.T) {
	for _, value := range []string{
		`null`, `{}`, `"text"`, `[]`, `[1,2,3]`,
		`[` + geminiErrorNulls + `,"not-array"]`,
		`[` + geminiErrorNulls + `,[]]`,
		`[` + geminiErrorNulls + `,[[]]]`,
		`[` + geminiErrorNulls + `,[["x"]]]`,
		`[` + geminiErrorNulls + `,[[[null,null,null,7,8,9,10]]]]`,
		`[` + geminiErrorNulls + `,[[[null,null,null,"a/b","PerDay",[["model"],[1,2],"x",[null,"v"]],true]]]]`,
		`[` + geminiErrorNulls + `,[[[null,null,null,"a/b","PerDay",{"model":"m"},"ten"]]]]`,
		`[` + geminiErrorNulls + `,[[[null,null,null,"a/b","PerDay",[["model",5]],{"v":1}]]]]`,
	} {
		for _, typeURL := range []string{geminiErrorDetailsType, "type.googleapis.com/google.rpc.QuotaFailure"} {
			rpcError := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded",[["`+typeURL+`",`+value+`]]]`))
			if rpcError.Code != 8 || rpcError.Message != "Quota exceeded" || rpcError.Metadata["quota_limit_value"] != "" || rpcError.Metadata["quota_dimension_model"] != "" {
				t.Fatalf("%s %s：RPC error=%+v", typeURL, value, rpcError)
			}
		}
	}
	// 形状正确但字段畸形：指标和限额名照常读出，畸形的维度与限额值跳过
	partial := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"quota",[["`+geminiErrorDetailsType+`",[`+geminiErrorNulls+
		`,[[[null,null,null,"a/b","PerDay",[["model"],[1,2],["location","global"]],"ten"]]]]]]]`))
	if partial.Metadata["quota_metric"] != "a/b" || partial.Metadata["quota_dimension_location"] != "global" ||
		partial.Metadata["quota_dimension_model"] != "" || partial.Metadata["quota_limit_value"] != "" {
		t.Fatalf("畸形字段应只跳过：%v", partial.Metadata)
	}
	// 详情类型：去掉前缀、按出现顺序去重；解析不了的详情项也照记类型
	bare := DecodeRPCError("ProxyStreamedCall", http.StatusTooManyRequests, []byte(
		`[8,"Quota exceeded",[["type.googleapis.com/google.internal.foo.v1.Unknown",[1]],["type.googleapis.com/google.rpc.Help",[]],`+
			`["type.googleapis.com/google.internal.foo.v1.Unknown"],[7,"x"],"junk"]]`,
	))
	if !slices.Equal(bare.DetailTypes, []string{"google.internal.foo.v1.Unknown", "google.rpc.Help"}) {
		t.Fatalf("DetailTypes=%v", bare.DetailTypes)
	}
	cooldown, ok := QuotaCooldownForError(bare, time.Now())
	if !ok || cooldown.Kind != RateLimitKind || !cooldown.Escalate || cooldown.Metric != "" ||
		cooldown.DetailTypes != "google.internal.foo.v1.Unknown,google.rpc.Help" {
		t.Fatalf("带未识别详情的裸 429：cooldown=%+v ok=%t", cooldown, ok)
	}
	if plain, _ := QuotaCooldownForError(DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"Quota exceeded"]`)), time.Now()); plain.DetailTypes != "" {
		t.Fatalf("没有详情时详情类型应为空：%+v", plain)
	}
}
