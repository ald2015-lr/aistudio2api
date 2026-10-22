package app

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 额度 429 详情未识别：上游 429 带着 GeminiApiErrorDetails（AI Studio 网页后端的私有详情）、却没有解析出额度指标时
// （布局与已知样本不同，冷却日志没有“指标”、带“详情类型=”，见 aistudio.RPCError.UnparsedDetail），冷却只能按文案判断
// （裸 “Quota exceeded” 按裸限流逐级兜底）。
// applyQuotaCooldown 写完冷却日志（写入或未写入）后在账号名下记一行 INFO，带这份详情的原文，请用户发回、据此扩展解析：
// 文件复制、附件上传阶段与已输出内容后的额度 429 同样写。
// 同一种详情（类型 + 文案，文案里的连续数字不计）unparsedQuotaDetailInterval 内只写一条，不论来自哪个账号：号池繁忙时这种 429 每秒可能出现几十次；
// 同时最多记 unparsedQuotaDetailMaxKeys 种，满时新的一种先不写，等有一种过期后再写（见 allow），任意 10 分钟最多写这么多行

const (
	// unparsedQuotaDetailInterval 为同一种未识别详情两条日志的最短间隔
	unparsedQuotaDetailInterval = 10 * time.Minute
	// unparsedQuotaDetailMaxKeys 为去重状态最多记下的键数
	unparsedQuotaDetailMaxKeys = 64
	// unparsedQuotaDetailMessageLimit 为日志里上游文案的最大字数
	unparsedQuotaDetailMessageLimit = 200
)

// unparsedQuotaDetailKey 为去重的键：详情类型与写进日志的文案（连续数字换成 N，见 newUnparsedQuotaDetailKey）
type unparsedQuotaDetailKey struct {
	detailType string
	message    string
}

// unparsedQuotaDetailDigits 匹配文案里的连续数字（project_number、retry in 时长、限额值等）
var unparsedQuotaDetailDigits = regexp.MustCompile(`[0-9]+`)

// newUnparsedQuotaDetailKey 返回去重的键：文案里的连续数字换成 N，只差这些数字的文案算同一种（日志照原样写文案）
func newUnparsedQuotaDetailKey(detailType string, message string) unparsedQuotaDetailKey {
	return unparsedQuotaDetailKey{detailType: detailType, message: unparsedQuotaDetailDigits.ReplaceAllString(message, "N")}
}

// unparsedQuotaDetailLog 为“额度 429 详情未识别”的去重状态：每个键最近一次写日志的时间，最多 unparsedQuotaDetailMaxKeys 个；
// 零值可用，nil 时不去重
type unparsedQuotaDetailLog struct {
	mu     sync.Mutex
	logged map[unparsedQuotaDetailKey]time.Time
	// rejectedAt 为键数已满时最近一次拒掉新键的时间，有新键放行时清零：此后 unparsedQuotaDetailInterval 内过期的键不续期（见 allow）
	rejectedAt time.Time
}

// allow 判断这个键现在能否写一条，能写时记下时间：距上一次不足 unparsedQuotaDetailInterval 时不能写。
// 新键遇到键数已满时先清掉过期的键，仍满时不写、也不记下（不淘汰未过期的键，否则轮换超过上限种类的文案时去重失效），
// 并记下被拒的时间：此后 unparsedQuotaDetailInterval 内过期的键再来时也不写、不续期，名额留给被拒的新键，
// 所以被拒的新键隔不到 unparsedQuotaDetailInterval 又来时，最早的键过期（最晚 unparsedQuotaDetailInterval）后再来就写；
// 同时被拒的新键不止一个时按到来先后
func (detailLog *unparsedQuotaDetailLog) allow(key unparsedQuotaDetailKey, now time.Time) bool {
	if detailLog == nil {
		return true
	}
	detailLog.mu.Lock()
	defer detailLog.mu.Unlock()
	last, seen := detailLog.logged[key]
	if seen && now.Sub(last) < unparsedQuotaDetailInterval {
		return false
	}
	if seen && !detailLog.rejectedAt.IsZero() && now.Sub(detailLog.rejectedAt) < unparsedQuotaDetailInterval {
		return false
	}
	if detailLog.logged == nil {
		detailLog.logged = make(map[unparsedQuotaDetailKey]time.Time)
	}
	if !seen && len(detailLog.logged) >= unparsedQuotaDetailMaxKeys {
		for other, at := range detailLog.logged {
			if now.Sub(at) >= unparsedQuotaDetailInterval {
				delete(detailLog.logged, other)
			}
		}
		if len(detailLog.logged) >= unparsedQuotaDetailMaxKeys {
			detailLog.rejectedAt = now
			return false
		}
	}
	if !seen {
		detailLog.rejectedAt = time.Time{}
	}
	detailLog.logged[key] = now
	return true
}

// unparsedQuotaDetailMessage 把上游文案压成一行（连续空白合成一个空格），超过 unparsedQuotaDetailMessageLimit 字时截断
func unparsedQuotaDetailMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if runes := []rune(message); len(runes) > unparsedQuotaDetailMessageLimit {
		message = string(runes[:unparsedQuotaDetailMessageLimit]) + "…"
	}
	return message
}

// logUnparsedQuotaDetail 在错误链里的 RPCError 带有未识别的 GeminiApiErrorDetails 原文时，在账号名下记一行 INFO
// “额度 429 详情未识别”，按详情类型与文案（连续数字不计）去重（见本文件开头）。只写 RPCError 的类型、文案与原文，不写请求内容与包装错误的其余部分；
// 原文来自上游，原样写
func (service *trackedService) logUnparsedQuotaDetail(accountLabel string, err error) {
	var rpcError *aistudio.RPCError
	if !errors.As(err, &rpcError) || rpcError.UnparsedDetail == "" {
		return
	}
	message := unparsedQuotaDetailMessage(rpcError.Message)
	if !service.unparsedQuotaDetails.allow(newUnparsedQuotaDetailKey(rpcError.UnparsedDetailType, message), time.Now()) {
		return
	}
	service.requests.log(accountLabel, "INFO", fmt.Sprintf(
		"额度 429 详情未识别 | 类型=%s | 文案=%s | 原文=%s | 请把这一行发给开发者",
		rpcError.UnparsedDetailType, message, rpcError.UnparsedDetail,
	))
}
