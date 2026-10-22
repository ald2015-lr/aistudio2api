package app

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// unparsedDetailTypeName 为 AI Studio 网页后端私有详情的类型（去掉 type.googleapis.com/ 前缀）
const unparsedDetailTypeName = "google.internal.alkali.applications.makersuite.v1.GeminiApiErrorDetails"

// unparsedQuotaError 返回带一个布局未知（只有整数枚举）的 GeminiApiErrorDetails 的 429，外面再包一层带密钥的错误：
// 日志只应写 RPCError 的类型、文案与原文
func unparsedQuotaError(message string) error {
	payload := fmt.Sprintf(`[8,%q,[["type.googleapis.com/%s",[null,null,3]]]]`, message, unparsedDetailTypeName)
	return fmt.Errorf("调用上游 key=sk-unparsed-secret: %w", aistudio.DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(payload)))
}

// unparsedDetailLine 返回 message 对应的“额度 429 详情未识别”日志的完整文案
func unparsedDetailLine(message string) string {
	return "额度 429 详情未识别 | 类型=" + unparsedDetailTypeName + " | 文案=" + message + " | 原文=[null,null,3] | 请把这一行发给开发者"
}

// TestUnparsedQuotaDetailLogged applyQuotaCooldown 遇到带未识别 GeminiApiErrorDetails 的额度 429 时，在冷却日志之后于账号名下记一行 INFO：
// 同一种详情（类型 + 文案）不论哪个账号 10 分钟只写一条；不同文案各写一条，冷却写入或未写入、已输出内容后与附件上传阶段都写；
// 不写包装错误里的密钥；解析出指标的样本不写，只解析出 quota_id、没有指标（冷却日志带“详情类型=”）的照样写
func TestUnparsedQuotaDetailLogged(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	acquire := func(accountID string) *aistudio.AccountLease {
		t.Helper()
		lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: accountID, ModelID: testRuntimeModel})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lease.Release() })
		return lease
	}
	leaseA, leaseB := acquire(partitionNormalA), acquire(partitionNormalB)
	apply := func(lease *aistudio.AccountLease, accountID string, err error, note string, escalate bool, fromGeneration bool) aistudio.QuotaCooldown {
		t.Helper()
		cooldown, ok, stateErr := service.applyQuotaCooldown(lease, accountID, testRuntimeModel, err, note, aistudio.PoolScopeNormal, escalate, fromGeneration)
		if !ok || stateErr != nil {
			t.Fatalf("应识别为额度错误：ok=%t err=%v", ok, stateErr)
		}
		return cooldown
	}

	// 裸 “Quota exceeded”：逐级冷却第 1 档，冷却日志之后紧跟一行未识别详情
	if cooldown := apply(leaseA, partitionNormalA, unparsedQuotaError("Quota exceeded"), "", true, true); cooldown.Kind != aistudio.RateLimitKind {
		t.Fatalf("分类不应受影响：%+v", cooldown)
	}
	messages := serviceLogMessages(requests)
	cooled := slices.IndexFunc(messages, func(message string) bool { return strings.HasPrefix(message, "账号冷却 | 类型=限流") })
	unparsed := slices.Index(messages, unparsedDetailLine("Quota exceeded"))
	if cooled < 0 || unparsed != cooled+1 || countSourceLogs(requests, partitionNormalA, unparsedDetailLine("Quota exceeded")) != 1 {
		t.Fatalf("冷却日志之后应在账号名下紧跟一行未识别详情，日志 = %v", messages)
	}
	// 不同文案各写一条：B 已输出内容后的每日限额（冷却写入），A 附件上传阶段的 429（租约早于已写回的结果，冷却未写入）
	daily := "You exceeded your current quota, please check your plan and billing details."
	if cooldown := apply(leaseB, partitionNormalB, unparsedQuotaError(daily), "已输出内容后", false, true); cooldown.Kind != aistudio.DailyQuotaKind {
		t.Fatalf("应按每日限额冷却：%+v", cooldown)
	}
	apply(leaseA, partitionNormalA, unparsedQuotaError("Quota exceeded for upload"), "", true, false)
	if !hasLogContaining(requests, "账号冷却 | 类型=每日限额", "已输出内容后") ||
		!hasLogContaining(requests, "账号冷却未写入 | 类型=限流", "原因=同账号取得租约更晚的请求已写回结果") {
		t.Fatalf("应有一行写入的每日限额与一行未写入的附件上传 429，日志 = %v", serviceLogMessages(requests))
	}
	for source, message := range map[string]string{partitionNormalB: daily, partitionNormalA: "Quota exceeded for upload"} {
		if got := countSourceLogs(requests, source, unparsedDetailLine(message)); got != 1 {
			t.Fatalf("%s 的文案 %q 应写一条，实际 %d 条，日志 = %v", source, message, got, serviceLogMessages(requests))
		}
	}
	// 同一种详情换个账号再来一次（冷却未写入）：10 分钟内不再写
	apply(leaseB, partitionNormalB, unparsedQuotaError("Quota exceeded"), "", true, true)
	if got := countSourceLogs(requests, "", unparsedDetailLine("Quota exceeded")); got != 1 {
		t.Fatalf("同一种详情 10 分钟只应写一条，实际 %d 条，日志 = %v", got, serviceLogMessages(requests))
	}
	if got := countSourceLogs(requests, "", "额度 429 详情未识别"); got != 3 {
		t.Fatalf("共应写 3 条，实际 %d 条", got)
	}
	if countSourceLogs(requests, "", "sk-unparsed-secret") != 0 {
		t.Fatalf("日志不应写出包装错误里的密钥，日志 = %v", serviceLogMessages(requests))
	}

	// 解析出指标的样本（C）没有原文，不写
	apply(leaseB, partitionNormalB, paidUsageLimitError("GenerateContent"), "", true, true)
	if got := countSourceLogs(requests, "", "额度 429 详情未识别"); got != 3 {
		t.Fatalf("解析出指标时不应写，实际 %d 条", got)
	}
	// 违规项只有 quota_id：分类与线上那一行一样（限流、没有指标，冷却日志带“详情类型=”），同样写原文
	quotaIDValue := "[" + strings.Repeat("null,", 28) + `[[[null,null,null,null,"GenerateRequestsPerProjectPerUserPerModel"]]]]`
	quotaIDOnly := aistudio.DecodeRPCError("GenerateContent", http.StatusTooManyRequests, []byte(
		`[8,"Quota exceeded for quota id",[["type.googleapis.com/`+unparsedDetailTypeName+`",`+quotaIDValue+`]]]`))
	if cooldown := apply(leaseB, partitionNormalB, quotaIDOnly, "", true, true); cooldown.Kind != aistudio.RateLimitKind || cooldown.Metric != "" {
		t.Fatalf("应按没有指标的限流冷却：%+v", cooldown)
	}
	quotaIDLine := "额度 429 详情未识别 | 类型=" + unparsedDetailTypeName + " | 文案=Quota exceeded for quota id | 原文=" + quotaIDValue + " | 请把这一行发给开发者"
	if !hasLogContaining(requests, "类型=限流", "详情类型="+unparsedDetailTypeName) || countSourceLogs(requests, partitionNormalB, quotaIDLine) != 1 {
		t.Fatalf("只有 quota_id 时冷却日志应带详情类型、并写一行原文，日志 = %v", serviceLogMessages(requests))
	}
	// 不是额度错误、没有 RPCError 时不写，也不 panic
	service.logUnparsedQuotaDetail(partitionNormalA, nil)
	service.logUnparsedQuotaDetail(partitionNormalA, fmt.Errorf("其他错误"))
	if got := countSourceLogs(requests, "", "额度 429 详情未识别"); got != 4 {
		t.Fatalf("没有原文时不应写，实际 %d 条", got)
	}
}

// TestUnparsedQuotaDetailDigitsIgnored 去重的键不计文案里的连续数字：只差 project_number、retry in 时长的文案算同一种，
// 日志照原样写第一条的文案；数字以外不同的文案是另一种
func TestUnparsedQuotaDetailDigitsIgnored(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	service := &trackedService{lifecycle: ctx, requests: requests}
	first := "Quota exceeded for consumer 'project_number:823511539352'. Please retry in 21.5s."
	for _, message := range []string{first, "Quota exceeded for consumer 'project_number:1'. Please retry in 3.25s.", "Quota exceeded for user"} {
		service.logUnparsedQuotaDetail("account@example.com", unparsedQuotaError(message))
	}
	if got := countSourceLogs(requests, "", "额度 429 详情未识别"); got != 2 {
		t.Fatalf("只差数字的文案应算同一种，共应写 2 条，实际 %d 条，日志 = %v", got, serviceLogMessages(requests))
	}
	if countSourceLogs(requests, "account@example.com", unparsedDetailLine(first)) != 1 ||
		countSourceLogs(requests, "account@example.com", unparsedDetailLine("Quota exceeded for user")) != 1 {
		t.Fatalf("日志应照原样写文案，日志 = %v", serviceLogMessages(requests))
	}
}

// TestUnparsedQuotaDetailMessage 文案压成一行（连续空白合成一个空格），超过 200 字截断并加 “…”
func TestUnparsedQuotaDetailMessage(t *testing.T) {
	if got := unparsedQuotaDetailMessage("  You exceeded\nyour current quota.\n* Quota exceeded  "); got != "You exceeded your current quota. * Quota exceeded" {
		t.Fatalf("应压成一行：%q", got)
	}
	long := strings.Repeat("额", unparsedQuotaDetailMessageLimit+5)
	if got := unparsedQuotaDetailMessage(long); got != strings.Repeat("额", unparsedQuotaDetailMessageLimit)+"…" {
		t.Fatalf("应按字截到 %d 字：%q", unparsedQuotaDetailMessageLimit, got)
	}
	exact := strings.Repeat("a", unparsedQuotaDetailMessageLimit)
	if got := unparsedQuotaDetailMessage(exact); got != exact {
		t.Fatalf("刚好 %d 字不应截断：%q", unparsedQuotaDetailMessageLimit, got)
	}
}

// TestUnparsedQuotaDetailLogLimit 去重状态：同一键 10 分钟内只放行一次；键数不超过 64，满时先清过期的键、仍满时新键不放行，
// 不挤掉未过期的键，最早的键过期（最晚 10 分钟）后放行；nil 时不去重
func TestUnparsedQuotaDetailLogLimit(t *testing.T) {
	var detailLog unparsedQuotaDetailLog
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	key := func(index int) unparsedQuotaDetailKey {
		return unparsedQuotaDetailKey{detailType: unparsedDetailTypeName, message: fmt.Sprintf("文案 %d", index)}
	}
	if !detailLog.allow(key(0), now) || detailLog.allow(key(0), now.Add(unparsedQuotaDetailInterval-time.Second)) ||
		!detailLog.allow(key(0), now.Add(unparsedQuotaDetailInterval)) {
		t.Fatal("同一键应在 10 分钟后才再放行")
	}
	if !detailLog.allow(unparsedQuotaDetailKey{detailType: "other.v1.GeminiApiErrorDetails", message: "文案 0"}, now) {
		t.Fatal("类型不同应是另一个键")
	}

	// 键数上限：同一时刻之后陆续加入 64 个键都放行；10 分钟内再来新键不放行、不记下，也不挤掉已有的键
	detailLog = unparsedQuotaDetailLog{}
	full := unparsedQuotaDetailMaxKeys
	for index := range full {
		if !detailLog.allow(key(index), now.Add(time.Duration(index)*time.Millisecond)) {
			t.Fatalf("新键 %d 应放行", index)
		}
	}
	later := now.Add(time.Second)
	if detailLog.allow(key(full), later) {
		t.Fatal("满时新键在有键过期之前不应放行")
	}
	if _, recorded := detailLog.logged[key(full)]; recorded || len(detailLog.logged) != full {
		t.Fatalf("被拒的新键不应记下，键数应保持 %d，实际 %d", full, len(detailLog.logged))
	}
	if _, kept := detailLog.logged[key(0)]; !kept || detailLog.allow(key(0), later) {
		t.Fatal("满时不应清掉未过期的键，最旧的键 10 分钟内仍应去重")
	}
	// 被拒的新键在最旧的键过期（最晚 10 分钟）后放行：先清掉过期的最旧键（只清过期的）
	if !detailLog.allow(key(full), now.Add(unparsedQuotaDetailInterval)) {
		t.Fatal("最旧的键过期后新键应放行")
	}
	if _, kept := detailLog.logged[key(0)]; kept || len(detailLog.logged) != full {
		t.Fatalf("应只清掉过期的最旧键，剩 %d 个", len(detailLog.logged))
	}
	// 满时先清过期的键：再过 10 分钟来一个新键，过期的键全部清掉
	if !detailLog.allow(key(full+1), now.Add(2*unparsedQuotaDetailInterval+time.Minute)) {
		t.Fatal("新键应放行")
	}
	if got := len(detailLog.logged); got != 1 {
		t.Fatalf("过期的键应全部清掉，剩 %d 个", got)
	}

	var none *unparsedQuotaDetailLog
	if !none.allow(key(0), now) || !none.allow(key(0), now) {
		t.Fatal("nil 时不去重")
	}
}

// TestUnparsedQuotaDetailLogRotation 10 分钟内轮换超过上限种类的文案两遍：总共只放行 64 条，被挤出上限的种类不会当作新键再放行
func TestUnparsedQuotaDetailLogRotation(t *testing.T) {
	var detailLog unparsedQuotaDetailLog
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	kinds := unparsedQuotaDetailMaxKeys + 1
	allowed := 0
	for round := range 2 {
		for index := range kinds {
			key := unparsedQuotaDetailKey{detailType: unparsedDetailTypeName, message: fmt.Sprintf("文案 %d", index)}
			if detailLog.allow(key, now.Add(time.Duration(round*kinds+index)*time.Second)) {
				allowed++
			}
		}
	}
	if allowed != unparsedQuotaDetailMaxKeys {
		t.Fatalf("10 分钟内应只放行 %d 条，实际 %d 条", unparsedQuotaDetailMaxKeys, allowed)
	}
}

// TestUnparsedQuotaDetailLogFullYields 键数已满、已记下的 64 种持续出现（每秒或每 100 毫秒各来一次，过期后都比新种类先到）时，
// 被拒的新种类仍在最早的一种过期后再来时放行：拒掉新种类后，过期的种类再来时不写、不续期；之后旧的种类照常轮换，任意 10 分钟仍最多放行 64 条
func TestUnparsedQuotaDetailLogFullYields(t *testing.T) {
	start := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	key := func(index int) unparsedQuotaDetailKey {
		return unparsedQuotaDetailKey{detailType: unparsedDetailTypeName, message: fmt.Sprintf("文案 %d", index)}
	}
	fresh := key(unparsedQuotaDetailMaxKeys)
	const tick = 100 * time.Millisecond
	for _, period := range []time.Duration{time.Second, tick} {
		var detailLog unparsedQuotaDetailLog
		var allowedAt []time.Time
		var freshAllowed time.Time
		// 旧的 64 种每 period 各来一次（整秒起），新的一种每秒来一次、比整秒晚 0.5 秒，跑 10 分钟加 2 秒
		for step := time.Duration(0); step <= unparsedQuotaDetailInterval+2*time.Second; step += tick {
			now := start.Add(step)
			if step%period == 0 {
				for index := range unparsedQuotaDetailMaxKeys {
					if detailLog.allow(key(index), now) {
						allowedAt = append(allowedAt, now)
					}
				}
			}
			if step%time.Second == 500*time.Millisecond && detailLog.allow(fresh, now) {
				allowedAt = append(allowedAt, now)
				if freshAllowed.IsZero() {
					freshAllowed = now
				}
			}
		}
		// 新种类第一次被拒在 0.5 秒，旧的种类在 10 分钟时过期，新种类在随后的 0.5 秒放行
		if want := start.Add(unparsedQuotaDetailInterval + 500*time.Millisecond); !freshAllowed.Equal(want) {
			t.Fatalf("旧的种类每 %s 来一次：被拒的新种类应在 %s 放行，实际 %s", period, want.Format("15:04:05.000"), freshAllowed.Format("15:04:05.000"))
		}
		// 第一个 10 分钟放行旧的 64 种；新种类放行后清掉过期的键，旧的种类再来时放行 63 种、剩下一种被拒
		if got := len(allowedAt); got != 2*unparsedQuotaDetailMaxKeys || len(detailLog.logged) != unparsedQuotaDetailMaxKeys {
			t.Fatalf("旧的种类每 %s 来一次：应共放行 %d 条、记下 %d 个键，实际 %d 条、%d 个键",
				period, 2*unparsedQuotaDetailMaxKeys, unparsedQuotaDetailMaxKeys, got, len(detailLog.logged))
		}
		for index, at := range allowedAt {
			inWindow := 0
			for _, other := range allowedAt[index:] {
				if other.Sub(at) < unparsedQuotaDetailInterval {
					inWindow++
				}
			}
			if inWindow > unparsedQuotaDetailMaxKeys {
				t.Fatalf("旧的种类每 %s 来一次：从 %s 起 10 分钟内放行了 %d 条，应不超过 %d 条",
					period, at.Format("15:04:05.000"), inWindow, unparsedQuotaDetailMaxKeys)
			}
		}
	}

	// 被拒的新种类不再来时，过期的种类只在拒绝后 10 分钟内不续期，之后照常放行
	var detailLog unparsedQuotaDetailLog
	for index := range unparsedQuotaDetailMaxKeys {
		detailLog.allow(key(index), start)
	}
	if detailLog.allow(fresh, start.Add(time.Second)) {
		t.Fatal("满时新种类不应放行")
	}
	if detailLog.allow(key(0), start.Add(unparsedQuotaDetailInterval)) || !detailLog.allow(key(0), start.Add(unparsedQuotaDetailInterval+time.Second)) {
		t.Fatal("过期的种类应在拒绝后 10 分钟内不续期、之后照常放行")
	}
}
