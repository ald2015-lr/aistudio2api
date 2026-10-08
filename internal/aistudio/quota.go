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
// 上游给出 RetryInfo 或 Retry-After 时：分钟限额和无周期证据的 429 以它为准；
// 每日限额取它与太平洋时间次日零点中较晚的一个，避免额度耗尽的账号每隔几十秒被重新选中
func QuotaCooldownForError(err error, now time.Time) (QuotaCooldown, bool) {
	var rpcError *RPCError
	if !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
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
	until, kind := now.Add(unknownRateLimitCooldown), "限流"
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
			until, kind = minuteQuotaReset(rpcError.Metadata["window_start_time"], now), "分钟限额"
			break
		}
	}
	if kind == "限流" && strings.Contains(strings.ToLower(rpcError.Message), "you exceeded your current quota") {
		until, kind = nextQuotaDay(now), DailyQuotaKind
	}
	if delay := rpcError.RetryDelay; delay > 0 {
		retryAt := now.Add(delay)
		if kind != DailyQuotaKind || retryAt.After(until) {
			until = retryAt
		}
	}
	return QuotaCooldown{Until: until, Global: global, Kind: kind, Reason: kind + ": " + err.Error()}, true
}

// DailyQuotaKind 为每日限额的冷却类型，app 层的通道共用额度学习按这个值识别
const DailyQuotaKind = "每日限额"

// unknownRateLimitCooldown 为没有额度信息的 429 的冷却时长
const unknownRateLimitCooldown = 30 * time.Second

func minuteQuotaEvidence(value string) bool {
	return strings.Contains(value, "/min/") || strings.Contains(value, "perminute") ||
		strings.Contains(value, "per_min") || strings.Contains(value, "per minute")
}

func dailyQuotaEvidence(value string) bool {
	return strings.Contains(value, "/day/") || strings.Contains(value, "perday") ||
		strings.Contains(value, "per_day") || strings.Contains(value, "per day") ||
		strings.Contains(value, "daily limit") || strings.Contains(value, "try again tomorrow")
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

func nextQuotaDay(now time.Time) time.Time {
	location, err := time.LoadLocation(quotaResetTimezone)
	if err != nil {
		panic(err)
	}
	local := now.In(location)
	return time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, location)
}
