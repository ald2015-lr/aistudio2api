package aistudio

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const quotaResetTimezone = "America/Los_Angeles"

// CooldownState 表示账户或模型暂时不可调度的状态
type CooldownState struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

// Active 判断冷却状态当前是否生效
func (c CooldownState) Active(now time.Time) bool {
	return !c.Until.IsZero() && now.Before(c.Until)
}

// QuotaCooldown 表示上游额度限制对应的调度冷却
type QuotaCooldown struct {
	Until  time.Time
	Global bool
	Kind   string
	Reason string
	// LimitValue 为上游给出的限额值（ErrorInfo 的 quota_limit_value，或 QuotaFailure、GeminiApiErrorDetails 违规项的 quota_value，
	// 如分钟限额 12、free 每日 10、pro/ultra 每日 50），Metric 为限额指标（quota_metric 去掉服务名前缀，如 generate_requests_global、
	// generate_requests_per_model_per_user），Model 为违规项维度里的模型（quota_dimension_model，如 gemini-3.1-pro：额度按它计，
	// 可能与请求的模型名不同）：只写进冷却日志，用来核实各档位的真实上限；上游没给时为空
	LimitValue string
	Metric     string
	Model      string
	// DetailTypes 为状态 details 的全部类型（RPCError.DetailTypes，逗号分隔）：冷却日志在没有指标时写出，用来核实裸 429 带了哪些详情
	DetailTypes string
	// Escalate 为没有额度周期信息、上游也没给恢复时间（RetryInfo、文案里的 “retry in”、Retry-After）的非全局限流 429：app 层按连续次数逐级加长冷却
	// （见 AccountPool.MarkRateLimitCooldown）。分钟限额、每日限额、全局冷却与带恢复时间的 429 都为 false，按原样冷却
	Escalate bool
}

// QuotaCooldownForError 解析上游分钟或每日额度限制。
//
// 周期按 quota_unit、quota_limit、quota_metric、错误文案的顺序取第一个有周期证据的字段：
// 同一个 quota_metric（如 generate_content_free_tier_requests）同时有分钟和每日两种限额，
// 不能按指标名猜周期。没有任何周期证据、只有通用文案 “You exceeded your current quota” 时按每日限额处理：
// AI Studio 对额度耗尽常只返回这句文案，按短期限流冷却会让额度已经用完的账号每隔几十秒被重新选中、整池反复 429。
// 只有元数据明确为全局（_global、PerProjectPerUser）且没有按模型标记时才冻结整个账号，
// 否则只冷却失败的模型或通道。
//
// 上游给出恢复时间（RetryInfo、文案里的 “retry in <时长>”、Retry-After，见 RPCError.RetryDelay）时：分钟限额和无周期证据的 429 以它为准；
// 每日限额取它与太平洋时间次日零点中较晚的一个，避免额度耗尽的账号每隔几十秒被重新选中。
//
// 输入 token 超过上限（提示词过长，见 InputTokenLimitError）即使按 429 返回也不是额度问题：不冷却账号
func QuotaCooldownForError(err error, now time.Time) (QuotaCooldown, bool) {
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
		return QuotaCooldown{}, false
	}
	if InputTokenLimitError(rpcError) {
		return QuotaCooldown{}, false
	}
	metadata := strings.ToLower(strings.Join([]string{
		rpcError.Metadata["quota_metric"],
		rpcError.Metadata["quota_limit"],
		rpcError.Metadata["quota_unit"],
		rpcError.Metadata["quota_id"],
	}, " "))
	global := (strings.Contains(metadata, "_global") || strings.Contains(metadata, "perprojectperuser")) &&
		!strings.Contains(metadata, "per_model") && !strings.Contains(metadata, "permodel") &&
		!strings.Contains(metadata, "{model}")
	// 没有可识别额度信息的 429（上游限流、资源暂时耗尽等）：该账户的这个模型短暂冷却。
	// 不冷却的话调度会反复选中它，每次都白白失败一轮再切号，高并发时拖慢所有请求
	until, kind := now.Add(unknownRateLimitCooldown), RateLimitKind
	for _, evidence := range []string{
		rpcError.Metadata["quota_unit"], rpcError.Metadata["quota_limit"], rpcError.Metadata["quota_id"],
		rpcError.Metadata["quota_metric"], rpcError.Message,
	} {
		evidence = strings.ToLower(evidence)
		if dailyQuotaEvidence(evidence) {
			until, kind = nextQuotaDay(now), DailyQuotaKind
			break
		}
		if minuteQuotaEvidence(evidence) {
			until, kind = minuteQuotaReset(rpcError.Metadata["window_start_time"], now), MinuteQuotaKind
			break
		}
	}
	if kind == RateLimitKind && strings.Contains(strings.ToLower(rpcError.Message), "you exceeded your current quota") {
		until, kind = nextQuotaDay(now), DailyQuotaKind
	}
	if delay := rpcError.RetryDelay; delay > 0 {
		retryAt := now.Add(delay)
		if kind != DailyQuotaKind || retryAt.After(until) {
			until = retryAt
		}
	}
	return QuotaCooldown{
		Until: until, Global: global, Kind: kind, Reason: kind + ": " + err.Error(),
		LimitValue:  strings.TrimSpace(rpcError.Metadata["quota_limit_value"]),
		Metric:      quotaMetricName(rpcError.Metadata["quota_metric"]),
		Model:       strings.TrimSpace(rpcError.Metadata[quotaDimensionPrefix+"model"]),
		DetailTypes: strings.Join(rpcError.DetailTypes, ","),
		Escalate:    kind == RateLimitKind && !global && rpcError.RetryDelay <= 0,
	}, true
}

// quotaMetricName 去掉 quota_metric 的服务名前缀（alkalimakersuite-pa.googleapis.com/generate_requests_global → generate_requests_global）
func quotaMetricName(metric string) string {
	metric = strings.TrimSpace(metric)
	if index := strings.LastIndexByte(metric, '/'); index >= 0 {
		return metric[index+1:]
	}
	return metric
}

// DailyQuotaKind 为每日限额的冷却类型，app 层的通道共用额度学习按这个值识别
const DailyQuotaKind = "每日限额"

// MinuteQuotaKind 为分钟限额的冷却类型（quota_unit、quota_limit 等带每分钟周期，如 AI Studio 每账号每分钟 12 次的 GeneratePerMinutePerProjectPerUser）
const MinuteQuotaKind = "分钟限额"

// RateLimitKind 为没有额度周期信息的 429 的冷却类型（冷却 unknownRateLimitCooldown；上游给出 RetryInfo 时以它为准）。
// 非全局、上游没给恢复时间的这类 429 由 app 层按连续次数逐级加长冷却（见 QuotaCooldown.Escalate）
const RateLimitKind = "限流"

// unknownRateLimitCooldown 为没有额度信息的 429 的冷却时长
const unknownRateLimitCooldown = 30 * time.Second

// ShortCooldownMax 为“短冷却”的上限：剩余不到它的冷却（分钟限额最长一分钟、没有额度信息的限流第一档 30 秒，或长冷却的最后一段）
// 很快自行恢复。淘汰选号不把这样的空闲 Worker 当作冷却中（WarmWorkerStates：全局冷却剩余不少于它，或该模型所有支持的启用通道
// 都在冷却且最早恢复不少于它才算，不看冷却原因）；账号全局 * 上剩余不到它的额度类冷却（分钟限额、限流，见 rateCooldownReason）
// 只等到期，同账号其他请求或非生成操作的成功不提前清除（markModelAccessVerified、ClearCooldownIfGeneration）
const ShortCooldownMax = 2 * time.Minute

// rateCooldownReason 判断冷却原因来自分钟限额或限流的 429（QuotaCooldownForError 写的原因为“类型: 原文”，落盘后前缀照样保留）。
// 转录上传等暂时故障写的 30 秒全局冷却、每日限额与 403 暂停都不是
func rateCooldownReason(reason string) bool {
	return strings.HasPrefix(reason, MinuteQuotaKind+": ") || strings.HasPrefix(reason, RateLimitKind+": ")
}

func minuteQuotaEvidence(value string) bool {
	return strings.Contains(value, "/min/") || strings.Contains(value, "perminute") ||
		strings.Contains(value, "per_min") || strings.Contains(value, "per minute")
}

// dailyQuotaEvidence 判断小写后的字段或文案带每日周期。“reached your usage limit” 为 pro/ultra 每日限额的文案
// （“You've reached your usage limit. Retry with an alternative Gemini model or upgrade your Google AI plan…”）；
// 不收更宽的 “usage limit”，免得误伤其他周期的文案
func dailyQuotaEvidence(value string) bool {
	return strings.Contains(value, "/day/") || strings.Contains(value, "perday") ||
		strings.Contains(value, "per_day") || strings.Contains(value, "per day") ||
		strings.Contains(value, "daily limit") || strings.Contains(value, "try again tomorrow") ||
		strings.Contains(value, "reached your usage limit")
}

// minuteQuotaReset 按 window_start_time 推算分钟窗口结束时间；时间异常（已过期或远在未来）时冷却一分钟
func minuteQuotaReset(windowStart string, now time.Time) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(windowStart), 10, 64)
	if err == nil {
		until := time.Unix(seconds, 0).Add(time.Minute)
		if until.After(now) && !until.After(now.Add(time.Minute)) {
			return until
		}
	}
	return now.Add(time.Minute)
}

// NextQuotaDay 返回 now 之后下一个额度日的开始时间（太平洋时间零点，每日限额在这时恢复）：
// app 层单模型 403 的长时间冷却按它计算
func NextQuotaDay(now time.Time) time.Time {
	return nextQuotaDay(now)
}

func nextQuotaDay(now time.Time) time.Time {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		panic(err)
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
}
