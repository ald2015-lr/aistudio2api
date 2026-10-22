package aistudio

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// userMinuteQuotaPayload 为用户遇到的真实 429 状态（AI Studio 网页后端每账号每分钟 12 次的生成限额）：只有 ErrorInfo 与 Help，
// 没有 RetryInfo
const userMinuteQuotaPayload = `[8,"Quota exceeded for quota metric 'Generate requests' and limit 'Generate requests per minute per user' of service 'alkalimakersuite-pa.googleapis.com' for consumer 'project_number:823511539352'.",` +
	`[["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","googleapis.com",[` +
	`["quota_limit_value","12"],["quota_unit","1/min/{project}/{user}"],["window_start_time","1792587739"],` +
	`["consumer","projects/823511539352"],["quota_metric","alkalimakersuite-pa.googleapis.com/generate_requests_global"],` +
	`["service","alkalimakersuite-pa.googleapis.com"],["quota_limit","GeneratePerMinutePerProjectPerUser"],["quota_location","global"]]]],` +
	`["type.googleapis.com/google.rpc.Help",[[["Request a higher quota limit.","https://cloud.google.com/docs/quotas/help/request_increase"]]]]]]`

func quotaError(message string, metadata map[string]string, retry time.Duration) *RPCError {
	return &RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: message, Metadata: metadata, RetryDelay: retry}
}

// TestQuotaCooldownClassification 周期按 unit、limit、metric、文案顺序判定，按模型的限额不冻结整个账号
func TestQuotaCooldownClassification(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		err    *RPCError
		kind   string
		global bool
		until  time.Time
	}{
		{
			// 通用文案 + 指标名同时存在分钟与每日限额：由 quota_limit 判定为分钟
			name: "按模型分钟限额",
			err: quotaError("You exceeded your current quota, please check your plan and billing details.", map[string]string{
				"quota_metric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
				"quota_limit":  "GenerateRequestsPerMinutePerProjectPerModel-FreeTier",
			}, 0),
			kind: "分钟限额", until: now.Add(time.Minute),
		},
		{
			name: "按模型每日限额",
			err: quotaError("You exceeded your current quota", map[string]string{
				"quota_metric": "generativelanguage.googleapis.com/generate_content_free_tier_requests",
				"quota_limit":  "GenerateRequestsPerDayPerProjectPerModel-FreeTier",
			}, 0),
			kind: DailyQuotaKind, until: nextQuotaDay(now),
		},
		{
			name: "全局分钟限额",
			err: quotaError("quota", map[string]string{
				"quota_limit": "GenerateContentRequestsPerMinutePerProjectPerUser",
			}, 0),
			kind: "分钟限额", global: true, until: now.Add(time.Minute),
		},
		{
			name: "带按模型标记的 PerProjectPerUser 不算全局",
			err: quotaError("quota", map[string]string{
				"quota_limit": "GenerateRequestsPerMinutePerProjectPerUserPerModel",
			}, 0),
			kind: "分钟限额", until: now.Add(time.Minute),
		},
		{
			name: "只有通用文案时按每日限额冷却到太平洋时间次日零点",
			err:  quotaError("You exceeded your current quota, please check your plan and billing details.", nil, 0),
			kind: DailyQuotaKind, until: nextQuotaDay(now),
		},
		{
			name: "通用文案但元数据是分钟限额时按分钟",
			err: quotaError("You exceeded your current quota, please check your plan and billing details.", map[string]string{
				"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel",
			}, 0),
			kind: "分钟限额", until: now.Add(time.Minute),
		},
		{
			name: "RetryInfo 决定分钟限额恢复时间",
			err: quotaError("quota", map[string]string{
				"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel",
			}, 37*time.Second),
			kind: "分钟限额", until: now.Add(37 * time.Second),
		},
		{
			name: "RetryInfo 决定无证据 429 的恢复时间",
			err:  quotaError("Resource exhausted", nil, 5*time.Minute),
			kind: "限流", until: now.Add(5 * time.Minute),
		},
		{
			// 每日限额不会因为很短的 RetryInfo 提前恢复
			name: "每日限额忽略更短的 RetryInfo",
			err: quotaError("quota", map[string]string{
				"quota_limit": "GenerateRequestsPerDayPerProjectPerModel",
			}, 20*time.Second),
			kind: DailyQuotaKind, until: nextQuotaDay(now),
		},
		{
			name: "window_start_time 远在未来时最多冷却一分钟",
			err: quotaError("quota", map[string]string{
				"quota_limit":       "GenerateRequestsPerMinutePerProjectPerModel",
				"window_start_time": "4102444800",
			}, 0),
			kind: "分钟限额", until: now.Add(time.Minute),
		},
		{
			name: "window_start_time 合理时按窗口结束",
			err: quotaError("quota", map[string]string{
				"quota_limit":       "GenerateRequestsPerMinutePerProjectPerModel",
				"window_start_time": strconv.FormatInt(now.Add(-10*time.Second).Unix(), 10),
			}, 0),
			kind: "分钟限额", until: now.Add(50 * time.Second),
		},
		{
			// AI Studio 网页后端每账号每分钟的限额：quota_unit 给出周期，_global 指标冻结整个账号
			name: "只有 quota_unit 与全局指标时按全局分钟限额",
			err: quotaError("quota", map[string]string{
				"quota_unit":   "1/min/{project}/{user}",
				"quota_metric": "alkalimakersuite-pa.googleapis.com/generate_requests_global",
			}, 0),
			kind: MinuteQuotaKind, global: true, until: now.Add(time.Minute),
		},
		{
			name: "只有全局指标、没有周期证据时按全局限流",
			err: quotaError("quota", map[string]string{
				"quota_metric": "alkalimakersuite-pa.googleapis.com/generate_requests_global",
			}, 0),
			kind: RateLimitKind, global: true, until: now.Add(unknownRateLimitCooldown),
		},
		{
			// quota_unit 排在 quota_limit 之前：单位是每分钟时，即使限额名带 PerDay 也按分钟限额
			name: "quota_unit 为每分钟时优先于 quota_limit 的 PerDay",
			err: quotaError("quota", map[string]string{
				"quota_unit":  "1/min/{project}/{user}",
				"quota_limit": "GenerateRequestsPerDayPerProjectPerUser",
			}, 0),
			kind: MinuteQuotaKind, global: true, until: now.Add(time.Minute),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cooldown, ok := QuotaCooldownForError(test.err, now)
			if !ok {
				t.Fatal("429 没有产生冷却")
			}
			if cooldown.Kind != test.kind || cooldown.Global != test.global || !cooldown.Until.Equal(test.until) {
				t.Fatalf("cooldown=%+v，期望 kind=%s global=%v until=%s", cooldown, test.kind, test.global, test.until)
			}
		})
	}
	if _, ok := QuotaCooldownForError(&RPCError{StatusCode: http.StatusBadRequest}, now); ok {
		t.Fatal("非 429 不应冷却")
	}
}

// TestQuotaCooldownUserMinuteSample 用户样本按全局分钟限额冷却到窗口结束（window_start_time+60s），窗口已过、恰好结束或远在未来时
// 冷却一分钟；独立错误与 Build 流尾两条解码路径结果相同，冷却带上游给出的限额值与去掉服务名前缀的指标
func TestQuotaCooldownUserMinuteSample(t *testing.T) {
	start := time.Unix(1792587739, 0)
	decoded := map[string]*RPCError{
		"独立错误": DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(userMinuteQuotaPayload)),
	}
	var trailer *RPCError
	if err := buildTrailerError(json.RawMessage(userMinuteQuotaPayload)); !errors.As(err, &trailer) {
		t.Fatalf("Build 流尾错误=%v", err)
	}
	decoded["Build 流尾"] = trailer
	for name, rpcError := range decoded {
		if rpcError.StatusCode != http.StatusTooManyRequests || rpcError.RetryDelay != 0 || rpcError.Metadata["quota_limit_value"] != "12" {
			t.Fatalf("%s: RPC error=%+v", name, rpcError)
		}
		for _, test := range []struct {
			offset time.Duration
			want   func(now time.Time) time.Time
		}{
			{0, func(time.Time) time.Time { return start.Add(time.Minute) }},
			{20 * time.Second, func(time.Time) time.Time { return start.Add(time.Minute) }},
			{59 * time.Second, func(time.Time) time.Time { return start.Add(time.Minute) }},
			{60 * time.Second, func(now time.Time) time.Time { return now.Add(time.Minute) }},
			{90 * time.Second, func(now time.Time) time.Time { return now.Add(time.Minute) }},
			{-5 * time.Second, func(now time.Time) time.Time { return now.Add(time.Minute) }},
		} {
			now := start.Add(test.offset)
			cooldown, ok := QuotaCooldownForError(rpcError, now)
			if !ok {
				t.Fatalf("%s 偏移 %s：429 没有产生冷却", name, test.offset)
			}
			if want := test.want(now); !cooldown.Until.Equal(want) {
				t.Fatalf("%s 偏移 %s：Until=%s，期望 %s", name, test.offset, cooldown.Until, want)
			}
			if cooldown.Kind != MinuteQuotaKind || !cooldown.Global || cooldown.LimitValue != "12" ||
				cooldown.Metric != "generate_requests_global" || !strings.HasPrefix(cooldown.Reason, MinuteQuotaKind+": ") {
				t.Fatalf("%s 偏移 %s：cooldown=%+v", name, test.offset, cooldown)
			}
		}
	}
}

// TestQuotaMetricName 冷却日志里的指标去掉服务名前缀，没有前缀时原样返回
func TestQuotaMetricName(t *testing.T) {
	for metric, want := range map[string]string{
		"alkalimakersuite-pa.googleapis.com/generate_requests_global": "generate_requests_global",
		" generate_requests_global ":                                  "generate_requests_global",
		"":                                                            "",
	} {
		if got := quotaMetricName(metric); got != want {
			t.Fatalf("quotaMetricName(%q)=%q，期望 %q", metric, got, want)
		}
	}
	// 没有 quota_metric 与 quota_limit_value 时两个字段为空
	cooldown, ok := QuotaCooldownForError(quotaError("quota", map[string]string{"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel"}, 0), time.Now())
	if !ok || cooldown.Metric != "" || cooldown.LimitValue != "" {
		t.Fatalf("cooldown=%+v，期望没有指标与限额值", cooldown)
	}
}

// TestDecodeRPCRetryInfo 解析 google.rpc.RetryInfo 与 HTTP Retry-After
func TestDecodeRPCRetryInfo(t *testing.T) {
	err := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(
		`[null,[8,"quota",[["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","googleapis.com",[["quota_limit","GenerateRequestsPerMinutePerProjectPerModel"]]]],["type.googleapis.com/google.rpc.RetryInfo",[[12,500000000]]]]]]`,
	))
	if err.RetryDelay != 12500*time.Millisecond || err.Metadata["quota_limit"] == "" {
		t.Fatalf("RPC error=%+v", err)
	}
	stringSeconds := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(
		`[null,[8,"quota",[["type.googleapis.com/google.rpc.RetryInfo",[["30"]]]]]]`,
	))
	if stringSeconds.RetryDelay != 30*time.Second {
		t.Fatalf("字符串秒数: %+v", stringSeconds)
	}
	invalid := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(
		`[null,[8,"quota",[["type.googleapis.com/google.rpc.RetryInfo",[[-1]]]]]]`,
	))
	if invalid.RetryDelay != 0 {
		t.Fatalf("负数秒数应忽略: %+v", invalid)
	}

	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for value, want := range map[string]time.Duration{
		"":                              0,
		"45":                            45 * time.Second,
		"0":                             0,
		"-3":                            0,
		"1.5":                           0,
		"999999999":                     0,
		"Wed, 07 Oct 2026 12:02:00 GMT": 2 * time.Minute,
		"Wed, 07 Oct 2026 11:00:00 GMT": 0,
	} {
		if got := parseRetryAfter(value, now); got != want {
			t.Fatalf("Retry-After %q = %s，期望 %s", value, got, want)
		}
	}
}

// TestStreamErrorsKeepQuotaMetadata Build 流尾与 Live 带内状态保留 ErrorInfo/RetryInfo，Live 状态码 8/14/16 有对应 HTTP 状态
func TestStreamErrorsKeepQuotaMetadata(t *testing.T) {
	details := `[["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","googleapis.com",[["quota_limit","GenerateRequestsPerMinutePerProjectPerModel"]]]],["type.googleapis.com/google.rpc.RetryInfo",[[9]]]]`
	err := buildTrailerError(json.RawMessage(`[8,"quota",` + details + `]`))
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests ||
		rpcError.Metadata["quota_limit"] == "" || rpcError.RetryDelay != 9*time.Second {
		t.Fatalf("Build 流尾错误=%+v", err)
	}
	for code, want := range map[int]int{8: http.StatusTooManyRequests, 14: http.StatusServiceUnavailable, 16: http.StatusUnauthorized} {
		raw := json.RawMessage(fmt.Sprintf(`{"__sm__":{"status":[[[%d,"status",%s]]]}}`, code, details))
		event, matched, err := parseBidiStatusPayload(raw)
		if err != nil || !matched || event.Kind != BidiEventError {
			t.Fatalf("code %d: event=%+v matched=%v err=%v", code, event, matched, err)
		}
		if !errors.As(event.Err, &rpcError) || rpcError.StatusCode != want || rpcError.Code != int64(code) ||
			rpcError.Message != "status" || rpcError.Metadata["quota_limit"] == "" {
			t.Fatalf("code %d: err=%+v", code, event.Err)
		}
	}
	if _, matched, err := parseBidiStatusPayload(json.RawMessage(`{"__sm__":{"status":[[[99,"x"]]]}}`)); !matched || err == nil {
		t.Fatal("未识别的状态码应返回协议证据错误")
	}
}

// TestQuotaFailureAndBareStatus Gemini API 形状的 429 只在 QuotaFailure 里给出额度周期时仍判定为每日限额；
// 没有 message 的状态保留状态码与详情
func TestQuotaFailureAndBareStatus(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	payload := `[8,"You exceeded your current quota, please check your plan and billing details.",[["type.googleapis.com/google.rpc.QuotaFailure",[[[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_requests","GenerateRequestsPerDayPerProjectPerModel-FreeTier"]]]],["type.googleapis.com/google.rpc.RetryInfo",[[16]]]]]`
	err := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(payload))
	if err.Metadata["quota_id"] != "GenerateRequestsPerDayPerProjectPerModel-FreeTier" || err.RetryDelay != 16*time.Second {
		t.Fatalf("RPC error=%+v", err)
	}
	cooldown, ok := QuotaCooldownForError(err, now)
	if !ok || cooldown.Kind != DailyQuotaKind || cooldown.Global || !cooldown.Until.Equal(nextQuotaDay(now)) {
		t.Fatalf("cooldown=%+v", cooldown)
	}

	// 免费层 "limit: 0" 的 429 把分钟违规项排在每日违规项之前：应按每日限额冷却
	multi := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"You exceeded your current quota.",[["type.googleapis.com/google.rpc.QuotaFailure",[[`+
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_input_token_count","GenerateContentInputTokensPerModelPerMinute-FreeTier"],`+
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_requests","GenerateRequestsPerDayPerProjectPerModel-FreeTier"]`+
		`]]],["type.googleapis.com/google.rpc.RetryInfo",[[29]]]]]`))
	if cooldown, ok := QuotaCooldownForError(multi, now); !ok || cooldown.Kind != DailyQuotaKind || !cooldown.Until.Equal(nextQuotaDay(now)) {
		t.Fatalf("多个违规项含每日限额 metadata=%v cooldown=%+v", multi.Metadata, cooldown)
	}
	minutes := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[8,"You exceeded your current quota.",[["type.googleapis.com/google.rpc.QuotaFailure",[[`+
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_input_token_count","GenerateContentInputTokensPerModelPerMinute-FreeTier"],`+
		`[null,null,null,"generativelanguage.googleapis.com/generate_content_free_tier_requests","GenerateRequestsPerMinutePerProjectPerModel-FreeTier"]`+
		`]]]]]`))
	if minutes.Metadata["quota_id"] != "GenerateContentInputTokensPerModelPerMinute-FreeTier" {
		t.Fatalf("没有每日违规项时应取第一个: %v", minutes.Metadata)
	}
	if cooldown, ok := QuotaCooldownForError(minutes, now); !ok || cooldown.Kind != "分钟限额" {
		t.Fatalf("只有分钟违规项 cooldown=%+v", cooldown)
	}

	bare := DecodeRPCError("ProxyStreamedCall", http.StatusTooManyRequests, []byte(
		`[8,null,[["type.googleapis.com/google.rpc.ErrorInfo",["RATE_LIMIT_EXCEEDED","googleapis.com",[["quota_limit","GenerateRequestsPerDayPerProjectPerModel"]]]]]]`,
	))
	if bare.Code != 8 || bare.Metadata["quota_limit"] == "" || bare.Message != http.StatusText(http.StatusTooManyRequests) {
		t.Fatalf("无 message 的状态=%+v", bare)
	}
	if only := DecodeRPCError("ProxyStreamedCall", http.StatusNotImplemented, []byte(`[12]`)); only.Code != 12 {
		t.Fatalf("只有状态码=%+v", only)
	}
	if wrapped := DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(`[null,[8]]`)); wrapped.Code != 8 {
		t.Fatalf("封装只有状态码=%+v", wrapped)
	}
}

// TestValidateStopSequences 停止序列的数量与长度有上限
func TestValidateStopSequences(t *testing.T) {
	if err := ValidateStopSequences([]string{"END", "###"}); err != nil {
		t.Fatal(err)
	}
	many := make([]string, maxStopSequences+1)
	for index := range many {
		many[index] = fmt.Sprint(index)
	}
	if ValidateStopSequences(many) == nil {
		t.Fatal("超过数量上限应返回错误")
	}
	if ValidateStopSequences([]string{string(make([]byte, maxStopSequenceBytes+1))}) == nil {
		t.Fatal("超过长度上限应返回错误")
	}
}

// TestQuotaCooldownEscalate 只有没有额度周期信息、上游也没给恢复时间的非全局限流 429（裸 “Quota exceeded”）按连续次数逐级加长冷却；
// 带 RetryInfo 或 Retry-After、分钟限额、每日限额与全局冷却都不逐级加长
func TestQuotaCooldownEscalate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	bare, ok := QuotaCooldownForError(quotaError("Quota exceeded", nil, 0), now)
	if !ok || bare.Kind != RateLimitKind || bare.Global || !bare.Escalate || !bare.Until.Equal(now.Add(unknownRateLimitCooldown)) {
		t.Fatalf("裸 Quota exceeded：%+v，期望非全局限流、30 秒、逐级加长", bare)
	}
	// HTTP Retry-After（没有 RetryInfo 时由响应头补上）
	response := &RPCResponse{
		StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"45"}},
		Body: io.NopCloser(strings.NewReader(`[8,"Quota exceeded"]`)),
	}
	_, retryAfter := validateRPCResponse("GenerateContent", response)
	for name, err := range map[string]error{
		"RetryInfo":   quotaError("Quota exceeded", nil, 20*time.Second),
		"Retry-After": retryAfter,
		"分钟限额":        quotaError("Quota exceeded", map[string]string{"quota_limit": "GenerateRequestsPerMinutePerProjectPerModel"}, 0),
		"每日限额":        quotaError("Quota exceeded", map[string]string{"quota_limit": "GenerateRequestsPerDayPerProjectPerModel"}, 0),
		"通用文案按每日限额":   quotaError("You exceeded your current quota", nil, 0),
		"全局":          quotaError("Quota exceeded", map[string]string{"quota_metric": "alkalimakersuite-pa.googleapis.com/generate_requests_global"}, 0),
	} {
		cooldown, ok := QuotaCooldownForError(err, now)
		if !ok || cooldown.Escalate {
			t.Fatalf("%s：%+v ok=%t，期望不逐级加长", name, cooldown, ok)
		}
	}
	var rpcError *RPCError
	if !errors.As(retryAfter, &rpcError) || rpcError.RetryDelay != 45*time.Second {
		t.Fatalf("Retry-After 应写进 RetryDelay：%v", retryAfter)
	}
	if global, _ := QuotaCooldownForError(quotaError("Quota exceeded", map[string]string{"quota_metric": "x/generate_requests_global"}, 0), now); !global.Global || global.Kind != RateLimitKind {
		t.Fatalf("全局限流的用例应为全局限流：%+v", global)
	}
}
