package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 连续裸限流逐级冷却的测试：档位时长在服务实例上改小（{150ms, 300ms, 30m, 2h}），“冷却到期后再失败”真实等待毫秒级的冷却到期

// rateLadderModel 为 Playground 与 Build 目录都支持的测试模型（testRuntimeModel 带转录能力，Build 通道不支持它）
const rateLadderModel = "rate-ladder-model"

// testRateLadder 为测试用的档位：前两档只有几百毫秒（等到期不拖慢测试），第三档起与默认相同。不再改小：冷却写入后请求还要判断
// 同一账号能否换通道再试，冷却在这之前就到期时（-race 下调度停顿几十毫秒）同一请求会在冷却到期后再试一次并升档，档位与调用次数都对不上
var testRateLadder = []time.Duration{150 * time.Millisecond, 300 * time.Millisecond, rateLimitLevel3, rateLimitLevel4}

// bareRateUpstream 让 limited 中的账号对每次生成返回裸限流 429（只有 “Quota exceeded”，没有额度信息与恢复时间），midStream 中的账号
// 先输出一段正文、再在流内返回同样的 429；其余账号正常返回。记录“账号/通道”
type bareRateUpstream struct {
	mu        sync.Mutex
	limited   map[string]bool
	midStream map[string]bool
	calls     []string
}

func bareRateLimitError() *aistudio.RPCError {
	return &aistudio.RPCError{Method: "GenerateContent", StatusCode: http.StatusTooManyRequests, Code: 8, Message: "Quota exceeded"}
}

func (upstream *bareRateUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *bareRateUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *bareRateUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

func (upstream *bareRateUpstream) set(accountID string, limited bool) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	upstream.limited[accountID] = limited
}

func (upstream *bareRateUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	limited, midStream := upstream.limited[request.AccountID], upstream.midStream[request.AccountID]
	upstream.mu.Unlock()
	if limited {
		return nil, bareRateLimitError()
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	if midStream {
		events <- aistudio.Event{Kind: aistudio.EventError, Err: bareRateLimitError()}
	} else {
		events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	}
	close(events)
	return events, nil
}

// rateLadderService 返回 Playground 与 Build 都启用、Worker 都已就绪的生成服务：两个目录都支持 rateLadderModel，
// 档位为 testRateLadder，带号池级限流判定；两个通道没有判定为独立额度（按共用处理）
func rateLadderService(t *testing.T, upstream aistudio.Service, accountIDs ...string) (*trackedService, *aistudio.AccountPool) {
	t.Helper()
	service, pool := firstEventTestService(t, upstream, 0, accountIDs...)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	target := aistudio.Model{ID: rateLadderModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range accountIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
	}
	service.rateLimitLadder = testRateLadder
	service.rateLimits = newRateLimitGuard()
	return service, pool
}

// rateCooldowns 返回账号在两个通道上 rateLadderModel 的冷却到期时间（没有冷却时为零值）
func rateCooldowns(t *testing.T, pool *aistudio.AccountPool, accountID string) (time.Time, time.Time) {
	t.Helper()
	status, ok := pool.StatusOf(accountID)
	if !ok {
		t.Fatalf("账号 %s 不存在", accountID)
	}
	return status.Cooldowns[rateLadderModel].Until, status.Cooldowns[aistudio.ChannelCooldownScope(aistudio.ChannelBuild, rateLadderModel)].Until
}

// waitRateCooldownExpired 等到账号两个通道上的冷却都到期（模拟“冷却到期后再取得租约”）
func waitRateCooldownExpired(t *testing.T, pool *aistudio.AccountPool, accountID string) {
	t.Helper()
	playground, build := rateCooldowns(t, pool, accountID)
	latest := playground
	if build.After(latest) {
		latest = build
	}
	if remaining := time.Until(latest); remaining > time.Second {
		t.Fatalf("账号 %s 的冷却还剩 %s，不应在测试里等待", accountID, remaining)
	}
	time.Sleep(time.Until(latest) + 5*time.Millisecond)
}

// skipNearQuotaDay 距太平洋时间零点不足 need 时跳过：每档都被截到次日零点（见 aistudio.MarkRateLimitCooldown），
// 这时断言的档位时长（冷却中、死槽位的剩余时间）不成立，零点后档位作废
func skipNearQuotaDay(t *testing.T, need time.Duration) {
	t.Helper()
	if remaining := time.Until(aistudio.NextQuotaDay(time.Now())); remaining < need {
		t.Skipf("距太平洋时间零点只剩 %s，档位会被截短", remaining.Round(time.Second))
	}
}

// countSourceLogs 返回来源为 source、包含全部片段的运行日志条数（source 为空时不限来源）
func countSourceLogs(requests *requestRegistry, source string, fragments ...string) int {
	requests.mu.Lock()
	defer requests.mu.Unlock()
	count := 0
	for _, entry := range requests.logs {
		if source != "" && entry.Source != source {
			continue
		}
		matched := true
		for _, fragment := range fragments {
			matched = matched && strings.Contains(entry.Message, fragment)
		}
		if matched {
			count++
		}
	}
	return count
}

// failRateRequest 在账号上执行一次 rateLadderModel 的请求，必须以裸限流 429 结束
func failRateRequest(t *testing.T, service *trackedService, requestID string, accountID string) {
	t.Helper()
	text, err, _ := runModelRequest(t, service, requestID, accountID, rateLadderModel)
	var rpcError *aistudio.RPCError
	if text != "" || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("请求 %s：text=%q err=%v，期望裸限流 429", requestID, text, err)
	}
}

// TestRateLimitLadderThresholds 各档与门槛的关系：第 2 档剩余 5m→2m 内算冷却中，第 3 档剩余 30m→15m 内是死槽位且留出排空超时的余量；
// 第 1 档与 aistudio 对裸限流的 30 秒冷却相同；测试可在实例上改小档位
func TestRateLimitLadderThresholds(t *testing.T) {
	if rateLimitLevel2 < aistudio.ShortCooldownMax+time.Minute {
		t.Fatalf("第 2 档 %s 应不少于 ShortCooldownMax+1m", rateLimitLevel2)
	}
	if rateLimitLevel3 < rotationMinRemaining+10*time.Minute {
		t.Fatalf("第 3 档 %s 应不少于 rotationMinRemaining+10m", rateLimitLevel3)
	}
	now := time.Now()
	cooldown, ok := aistudio.QuotaCooldownForError(bareRateLimitError(), now)
	if !ok || !cooldown.Escalate || cooldown.Until.Sub(now) != rateLimitLevel1 {
		t.Fatalf("裸限流 %+v，期望逐级加长、第 1 档 %s", cooldown, rateLimitLevel1)
	}
	service := &trackedService{}
	if steps := service.rateLimitSteps(); !slices.Equal(steps, []time.Duration{rateLimitLevel1, rateLimitLevel2, rateLimitLevel3, rateLimitLevel4}) {
		t.Fatalf("默认档位 = %v", steps)
	}
	service.rateLimitLadder = testRateLadder
	if steps := service.rateLimitSteps(); !slices.Equal(steps, testRateLadder) {
		t.Fatalf("改小后的档位 = %v", steps)
	}
}

// TestRateLimitGuardSystemic 号池级限流：窗口内同一号池同一模型至少 5 个不同账号裸限流、这段时间又没有成功才成立（同一账号重复不算，
// 窗口外的不算，号池与模型分开计）；每个窗口只告警一次；成功一次重置
func TestRateLimitGuardSystemic(t *testing.T) {
	var missing *rateLimitGuard
	if systemic, warn := missing.observe(aistudio.PoolScopeNormal, "a", "m", time.Now()); systemic || warn {
		t.Fatal("nil 判定不应成立")
	}
	missing.succeeded(aistudio.PoolScopeNormal, "m", time.Now())

	guard := newRateLimitGuard()
	start := time.Now()
	observe := func(accountID string, at time.Duration) (bool, bool) {
		return guard.observe(aistudio.PoolScopeNormal, accountID, "m", start.Add(at))
	}
	for index := range 4 {
		if systemic, warn := observe(fmt.Sprintf("a%d", index), time.Duration(index)*time.Second); systemic || warn {
			t.Fatalf("第 %d 个账号时不应成立", index+1)
		}
	}
	if systemic, _ := observe("a3", 5*time.Second); systemic {
		t.Fatal("同一账号重复限流不算新的账号")
	}
	if systemic, warn := observe("a4", 6*time.Second); !systemic || !warn {
		t.Fatalf("第 5 个账号时应成立并告警：systemic=%t warn=%t", systemic, warn)
	}
	if systemic, warn := observe("a5", 7*time.Second); !systemic || warn {
		t.Fatalf("同一窗口内再次成立：systemic=%t warn=%t，期望不再告警", systemic, warn)
	}
	// 号池与模型分开计
	if systemic, _ := guard.observe(aistudio.PoolScopeUltra, "a0", "m", start.Add(8*time.Second)); systemic {
		t.Fatal("另一个号池不应受影响")
	}
	if systemic, _ := guard.observe(aistudio.PoolScopeNormal, "a0", "other", start.Add(8*time.Second)); systemic {
		t.Fatal("另一个模型不应受影响")
	}
	// 窗口过后重新攒满 5 个账号：再次告警
	later := rateLimitSystemicWindow + 10*time.Second
	for index := range 4 {
		if systemic, _ := observe(fmt.Sprintf("b%d", index), later+time.Duration(index)*time.Second); systemic {
			t.Fatalf("窗口外的失败不应计入（第 %d 个）", index+1)
		}
	}
	if systemic, warn := observe("b4", later+5*time.Second); !systemic || !warn {
		t.Fatalf("下一个窗口内成立应再告警一次：systemic=%t warn=%t", systemic, warn)
	}
	// 成功一次重置：之后窗口内再多的失败也不成立，直到距成功超过窗口
	success := later + 6*time.Second
	guard.succeeded(aistudio.PoolScopeNormal, "m", start.Add(success))
	for index := range 5 {
		if systemic, _ := observe(fmt.Sprintf("c%d", index), success+time.Duration(index+1)*time.Second); systemic {
			t.Fatalf("成功之后窗口内的失败不应成立（第 %d 个）", index+1)
		}
	}
	// 成功之后的那批失败也已在窗口外
	afterWindow := success + rateLimitSystemicWindow + 10*time.Second
	for index := range 4 {
		observe(fmt.Sprintf("d%d", index), afterWindow+time.Duration(index)*time.Millisecond)
	}
	if systemic, warn := observe("d4", afterWindow+5*time.Millisecond); !systemic || !warn {
		t.Fatalf("距成功超过窗口后应再次成立：systemic=%t warn=%t", systemic, warn)
	}
}

// TestQuotaSharingSharedFor 连续限流按通道共用判定：只有已判定为独立时不共用，共用、观察中、没有记录都按共用；按权益分组分开；只读
func TestQuotaSharingSharedFor(t *testing.T) {
	var missing *quotaSharing
	if !missing.sharedFor("m", aistudio.PoolScopeNormal) {
		t.Fatal("nil 判定应按共用")
	}
	sharing := newQuotaSharing("", nil)
	if !sharing.sharedFor("m", aistudio.PoolScopeNormal) {
		t.Fatal("没有记录时应按共用")
	}
	for _, test := range []struct {
		stats  quotaSharingStats
		shared bool
	}{
		{stats: quotaSharingStats{Shared: 2}, shared: true},
		{stats: quotaSharingStats{Shared: quotaSharedMinEvidence}, shared: true},
		{stats: quotaSharingStats{Shared: 40, Independent: 1}, shared: true},
		{stats: quotaSharingStats{Independent: 1}, shared: false},
		{stats: quotaSharingStats{Shared: 19, Independent: 1}, shared: false},
	} {
		stats := test.stats
		sharing.models["m"] = &stats
		if got := sharing.sharedFor("m", aistudio.PoolScopeNormal); got != test.shared {
			t.Fatalf("证据 %+v（%s）：sharedFor=%t，期望 %t", test.stats, sharing.verdictLocked("m"), got, test.shared)
		}
	}
	// Ultra 权益单独一组
	sharing.models["m"] = &quotaSharingStats{Shared: quotaSharedMinEvidence}
	sharing.models[quotaStatsKey("m", aistudio.PoolScopeUltra)] = &quotaSharingStats{Independent: 3}
	if !sharing.sharedFor("m", aistudio.PoolScopeNormal) || sharing.sharedFor("m", aistudio.PoolScopeUltra) {
		t.Fatal("权益分组应分开判定")
	}
	if len(sharing.pending) != 0 || len(sharing.sinceRevisit) != 0 {
		t.Fatalf("sharedFor 不应改观察与复核计数：pending=%v sinceRevisit=%v", sharing.pending, sharing.sinceRevisit)
	}
}

// TestRateLimitEscalatesUntilRetired 账号对模型一直裸限流：每次冷却到期后再失败升一档（两个通道共用档位并同步冷却），
// 第 3 档起两个通道剩余都不少于 rotationMinRemaining，驻留 Worker 登记淘汰；第 1、2 档不登记
func TestRateLimitEscalatesUntilRetired(t *testing.T) {
	// 第 3 档要剩余不少于 rotationMinRemaining 才是死槽位
	skipNearQuotaDay(t, rotationMinRemaining+time.Minute)
	const limited, healthy = "a-rate@example.com", "b-rate@example.com"
	upstream := &bareRateUpstream{limited: map[string]bool{limited: true}}
	service, pool := rateLadderService(t, upstream, limited, healthy)
	for level := 1; level <= 3; level++ {
		if level > 1 {
			waitRateCooldownExpired(t, pool, limited)
		}
		failRateRequest(t, service, fmt.Sprintf("req-ladder-%d", level), limited)
		if got := countSourceLogs(service.requests, limited, "账号冷却 | 类型=限流", fmt.Sprintf("连续限流=第 %d 档 | 同步=两个通道", level)); got != 1 {
			t.Fatalf("第 %d 次失败后日志 = %v，期望一条第 %d 档的冷却", level, serviceLogMessages(service.requests), level)
		}
		if level < 3 && service.workers.retirementCount() != 0 {
			t.Fatalf("第 %d 档不应登记淘汰", level)
		}
	}
	// 同一账号两个通道都已冷却：每次都只白试一次，不再换到另一个通道
	if calls := upstream.called(); len(calls) != 3 {
		t.Fatalf("上游调用 = %v，期望每次请求只试一次", calls)
	}
	playground, build := rateCooldowns(t, pool, limited)
	for name, until := range map[string]time.Time{"Playground": playground, "Build": build} {
		if remaining := time.Until(until); remaining < rotationMinRemaining {
			t.Fatalf("第 3 档后 %s 剩余 %s，期望不少于 %s", name, remaining, rotationMinRemaining)
		}
	}
	if !pool.AccountLongCooling(limited, rateLadderModel, rotationMinRemaining) {
		t.Fatal("第 3 档后该模型应成为死槽位")
	}
	if got := service.workers.retirementCount(); got != 1 {
		t.Fatalf("第 3 档后应登记淘汰，登记数 = %d", got)
	}
	// 冷却期间不固定账号的请求直接选另一个账号
	if text, err, _ := runModelRequest(t, service, "req-ladder-other", "", rateLadderModel); err != nil || text != "hello" {
		t.Fatalf("另一个账号应完成请求：text=%q err=%v", text, err)
	}
}

// TestRateLimitStrikeResetBySuccess 账号到第 2 档、冷却到期后成功一次：档位清零并写一行“限流档位清零”，之后再失败从第 1 档起
func TestRateLimitStrikeResetBySuccess(t *testing.T) {
	const account = "a-reset@example.com"
	upstream := &bareRateUpstream{limited: map[string]bool{account: true}}
	service, pool := rateLadderService(t, upstream, account)
	failRateRequest(t, service, "req-reset-1", account)
	waitRateCooldownExpired(t, pool, account)
	failRateRequest(t, service, "req-reset-2", account)
	if countSourceLogs(service.requests, account, "连续限流=第 2 档") != 1 {
		t.Fatalf("应到第 2 档，日志 = %v", serviceLogMessages(service.requests))
	}
	waitRateCooldownExpired(t, pool, account)
	upstream.set(account, false)
	if text, err, _ := runModelRequest(t, service, "req-reset-ok", account, rateLadderModel); err != nil || text != "hello" {
		t.Fatalf("冷却到期后应成功：text=%q err=%v", text, err)
	}
	// 成功记录在后台写入，回调写日志
	pool.WaitBackground()
	if countSourceLogs(service.requests, account, "限流档位清零 | 范围=", "| 此前连续=第 2 档") != 1 {
		t.Fatalf("成功后应写档位清零，日志 = %v", serviceLogMessages(service.requests))
	}
	upstream.set(account, true)
	failRateRequest(t, service, "req-reset-3", account)
	if got := countSourceLogs(service.requests, account, "连续限流=第 1 档"); got != 2 {
		t.Fatalf("清零后再失败应从第 1 档起：第 1 档日志 %d 条，日志 = %v", got, serviceLogMessages(service.requests))
	}
	if countSourceLogs(service.requests, account, "连续限流=第 3 档") != 0 {
		t.Fatal("清零后不应升到第 3 档")
	}
}

// TestRateLimitSystemicDoesNotEscalate 号池内 5 个账号都裸限流、又没有任何成功：判定为号池级限流，只写一条 WARN，冷却到期后再失败
// 也不升档；号池里有一次成功后判定解除，再失败照常升档
func TestRateLimitSystemicDoesNotEscalate(t *testing.T) {
	accounts := make([]string, 0, 5)
	limited := make(map[string]bool)
	for index := range 5 {
		accountID := fmt.Sprintf("a-systemic-%d@example.com", index)
		accounts = append(accounts, accountID)
		limited[accountID] = true
	}
	upstream := &bareRateUpstream{limited: limited}
	service, pool := rateLadderService(t, upstream, accounts...)
	for round := 1; round <= 2; round++ {
		for index, accountID := range accounts {
			if round > 1 {
				waitRateCooldownExpired(t, pool, accountID)
			}
			failRateRequest(t, service, fmt.Sprintf("req-systemic-%d-%d", round, index), accountID)
		}
	}
	if got := countSourceLogs(service.requests, "service", "号池级限流 | 模型="+rateLadderModel+" | 号池内 10 分钟内至少 5 个账号返回限流且没有成功，不升级冷却"); got != 1 {
		t.Fatalf("号池级限流告警 %d 条，期望 1 条；日志 = %v", got, serviceLogMessages(service.requests))
	}
	for _, accountID := range accounts {
		if countSourceLogs(service.requests, accountID, "连续限流=第 2 档") != 0 {
			t.Fatalf("号池级限流时 %s 不应升档", accountID)
		}
	}
	// 第二轮每个账号都按号池级限流不升档
	if got := countSourceLogs(service.requests, "", "连续限流=第 1 档", "号池级限流不升档"); got < len(accounts) {
		t.Fatalf("号池级限流不升档的冷却日志 %d 条，期望至少 %d 条", got, len(accounts))
	}
	// 一个账号恢复并成功：判定解除，另一个账号冷却到期后再失败升到第 2 档
	waitRateCooldownExpired(t, pool, accounts[0])
	upstream.set(accounts[0], false)
	if text, err, _ := runModelRequest(t, service, "req-systemic-ok", accounts[0], rateLadderModel); err != nil || text != "hello" {
		t.Fatalf("恢复的账号应成功：text=%q err=%v", text, err)
	}
	waitRateCooldownExpired(t, pool, accounts[1])
	failRateRequest(t, service, "req-systemic-after", accounts[1])
	if countSourceLogs(service.requests, accounts[1], "连续限流=第 2 档") != 1 {
		t.Fatalf("成功之后应照常升档，日志 = %v", serviceLogMessages(service.requests))
	}
}

// TestRateLimitAfterOutputDoesNotEscalate 已向客户端输出内容后的流内裸限流照常冷却，但不作为“冷却到期后仍失败”的证据：
// 冷却到期后再次在流内失败仍为第 1 档；首事件前的失败照常升档
func TestRateLimitAfterOutputDoesNotEscalate(t *testing.T) {
	const account = "a-midstream@example.com"
	upstream := &bareRateUpstream{limited: map[string]bool{}, midStream: map[string]bool{account: true}}
	service, pool := rateLadderService(t, upstream, account)
	for index := range 2 {
		if index > 0 {
			waitRateCooldownExpired(t, pool, account)
		}
		text, err, _ := runModelRequest(t, service, fmt.Sprintf("req-midstream-%d", index), account, rateLadderModel)
		var rpcError *aistudio.RPCError
		if text != "hello" || !errors.As(err, &rpcError) || rpcError.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("第 %d 次：text=%q err=%v，期望输出正文后流内 429", index+1, text, err)
		}
	}
	if got := countSourceLogs(service.requests, account, "连续限流=第 1 档", "| 已输出内容后"); got != 2 {
		t.Fatalf("已输出内容后的限流应两次都为第 1 档：%d 条，日志 = %v", got, serviceLogMessages(service.requests))
	}
	waitRateCooldownExpired(t, pool, account)
	upstream.mu.Lock()
	upstream.midStream[account] = false
	upstream.limited[account] = true
	upstream.mu.Unlock()
	failRateRequest(t, service, "req-midstream-first", account)
	if countSourceLogs(service.requests, account, "连续限流=第 2 档") != 1 {
		t.Fatalf("首事件前的失败应照常升档，日志 = %v", serviceLogMessages(service.requests))
	}
}

// uploadRateUpstream 让 Playground 租约的内联附件上传返回 Drive 的裸 429（Build 通道直接携带 inlineData，不经 Drive）；
// 生成照 bareRateUpstream 处理
type uploadRateUpstream struct {
	*bareRateUpstream
}

func driveRateLimitError() error {
	return fmt.Errorf("上传内联附件: %w", &aistudio.RPCError{
		Method: "DriveUpload", StatusCode: http.StatusTooManyRequests,
		Message: `{"error":{"code":429,"message":"Rate Limit Exceeded","errors":[{"reason":"rateLimitExceeded"}]}}`,
	})
}

func (upstream *uploadRateUpstream) UploadInlineMediaToLease(
	_ context.Context, target *aistudio.AccountLease, contents []aistudio.Content, temporary *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	if target.Channel() == aistudio.ChannelBuild {
		return contents, temporary, nil
	}
	return nil, nil, driveRateLimitError()
}

// TestRateLimitUploadNotEscalated Playground 租约上传内联附件时 Drive 返回裸 429：错误不是生成本身返回的，与模型额度无关，
// 只冷却 Playground 30 秒（不建档位、不同步冷却 Build、不计入号池级限流），同一账号随后在 Build 通道完成请求
func TestRateLimitUploadNotEscalated(t *testing.T) {
	const account = "a-upload@example.com"
	// 前提：这个错误本身按裸限流逐级冷却（Escalate 为真），只因为不是生成本身的错误才不逐级
	if cooldown, ok := aistudio.QuotaCooldownForError(driveRateLimitError(), time.Now()); !ok || !cooldown.Escalate || cooldown.Global {
		t.Fatalf("Drive 的裸 429 = %+v（ok=%t），期望非全局、可逐级的限流", cooldown, ok)
	}
	upstream := &uploadRateUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}}
	service, pool := rateLadderService(t, upstream, account)
	request := aistudio.GenerateRequest{
		ID: "req-upload", Model: rateLadderModel, AccountID: account,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
			{Text: "hi"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}},
		}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	text := ""
	var requestErr error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			requestErr = event.Err
		}
	}
	if requestErr != nil || text != "hello" {
		t.Fatalf("同一账号应在 Build 通道完成请求：text=%q err=%v，日志 = %v", text, requestErr, serviceLogMessages(service.requests))
	}
	if calls := upstream.called(); !slices.Equal(calls, []string{account + "/" + string(aistudio.ChannelBuild)}) {
		t.Fatalf("上游生成调用 = %v，期望上传失败后只在 Build 通道生成一次", calls)
	}
	playground, build := rateCooldowns(t, pool, account)
	if remaining := time.Until(playground); remaining <= 0 || remaining > rateLimitLevel1 {
		t.Fatalf("Playground 冷却剩余 %s，期望第 1 档（%s）以内", remaining, rateLimitLevel1)
	}
	if !build.IsZero() {
		t.Fatalf("上传失败不应同步冷却 Build：恢复=%s", build)
	}
	if got := countSourceLogs(service.requests, account, "账号冷却 | 类型=限流 | 范围="+rateLadderModel); got != 1 {
		t.Fatalf("Playground 限流冷却日志 %d 条，期望 1 条；日志 = %v", got, serviceLogMessages(service.requests))
	}
	if got := countSourceLogs(service.requests, "", "连续限流="); got != 0 {
		t.Fatalf("上传失败不应逐级冷却：日志 = %v", serviceLogMessages(service.requests))
	}
	service.rateLimits.mu.Lock()
	observed := len(service.rateLimits.scopes[rateLimitScopeKey{pool: aistudio.PoolScopeNormal, modelID: rateLadderModel}].failed)
	service.rateLimits.mu.Unlock()
	if observed != 0 {
		t.Fatalf("上传失败不应计入号池级限流：记录 %d 个账号", observed)
	}
}

// cleanupRateUpstream 让内联附件上传成功，但删除临时 Drive 副本时返回 cleanupErr；生成本身返回非 RPC 的连接错误
type cleanupRateUpstream struct {
	*bareRateUpstream
	cleanupErr error
}

// driveDeleteError 为删除 Drive 文件返回非 2xx 时的错误（同 aistudio driveUploadRPCError）
func driveDeleteError(status int) error {
	return &aistudio.RPCError{
		Method: "DriveDelete", StatusCode: status,
		Message: fmt.Sprintf(`{"error":{"code":%d,"message":"%s"}}`, status, http.StatusText(status)),
	}
}

func (upstream *cleanupRateUpstream) UploadInlineMediaToLease(
	_ context.Context, _ *aistudio.AccountLease, contents []aistudio.Content, _ *aistudio.TemporaryFileCopies,
) ([]aistudio.Content, *aistudio.TemporaryFileCopies, error) {
	return contents, aistudio.NewTemporaryFileCopiesForTest(upstream.cleanupErr), nil
}

func (upstream *cleanupRateUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, request.AccountID+"/"+channel)
	upstream.mu.Unlock()
	return nil, errors.New("read tcp 10.0.0.1:52144->142.250.0.1:443: read: connection reset by peer")
}

// TestRateLimitCleanupErrorNotEscalated 附件上传成功、生成以非 RPC 的连接错误失败、随后删除临时 Drive 副本时 Drive 返回 429 或 403：
// 额度冷却与单模型 403 只看这次尝试本身的错误，清理错误与模型额度、模型权限无关，不能当成生成本身的裸限流逐级冷却（两个通道都冷却、
// 计入号池级限流）或单模型 403 冷却；清理错误照旧并入返回的错误
func TestRateLimitCleanupErrorNotEscalated(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusForbidden} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			account := fmt.Sprintf("a-cleanup-%d@example.com", status)
			cleanupErr := driveDeleteError(status)
			// 前提：生成错误与清理错误合并后，额度判断与 403 判断会沿错误链找到 Drive 的错误
			joined := errors.Join(errors.New("read tcp: connection reset by peer"), cleanupErr)
			if cooldown, ok := aistudio.QuotaCooldownForError(joined, time.Now()); status == http.StatusTooManyRequests && (!ok || !cooldown.Escalate) {
				t.Fatalf("合并后的错误 = %+v（ok=%t），期望可逐级的限流", cooldown, ok)
			}
			if status == http.StatusForbidden && !isPermissionDenied(joined) {
				t.Fatal("合并后的错误应被识别为 403 无权限")
			}
			upstream := &cleanupRateUpstream{bareRateUpstream: &bareRateUpstream{limited: map[string]bool{}}, cleanupErr: cleanupErr}
			service, pool := rateLadderService(t, upstream, account)
			request := aistudio.GenerateRequest{
				ID: "req-cleanup", Model: rateLadderModel, AccountID: account,
				Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{
					{Text: "hi"}, {InlineData: &aistudio.Blob{MIME: "image/png", Data: []byte("png")}},
				}}},
			}
			requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
			defer requestCancel()
			events := make(chan aistudio.Event, 8)
			go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
			var requestErr error
			for event := range events {
				if event.Kind == aistudio.EventError {
					requestErr = event.Err
				}
			}
			var rpcError *aistudio.RPCError
			if requestErr == nil || !strings.Contains(requestErr.Error(), "connection reset by peer") || !errors.As(requestErr, &rpcError) || rpcError.Method != "DriveDelete" {
				t.Fatalf("请求错误 = %v，期望连接错误并带上清理时的 Drive 错误", requestErr)
			}
			if calls := upstream.called(); len(calls) != 1 {
				t.Fatalf("上游生成调用 = %v，期望一次", calls)
			}
			playground, build := rateCooldowns(t, pool, account)
			if !playground.IsZero() || !build.IsZero() {
				t.Fatalf("清理的 Drive 错误不应冷却账号：Playground=%s Build=%s，日志 = %v", playground, build, serviceLogMessages(service.requests))
			}
			if got := countSourceLogs(service.requests, account, "账号冷却 |"); got != 0 {
				t.Fatalf("清理的 Drive 错误不应写冷却：日志 = %v", serviceLogMessages(service.requests))
			}
			if got := countSourceLogs(service.requests, "", "连续限流="); got != 0 {
				t.Fatalf("清理的 Drive 错误不应逐级冷却：日志 = %v", serviceLogMessages(service.requests))
			}
			service.rateLimits.mu.Lock()
			observed := len(service.rateLimits.scopes[rateLimitScopeKey{pool: aistudio.PoolScopeNormal, modelID: rateLadderModel}].failed)
			service.rateLimits.mu.Unlock()
			if observed != 0 {
				t.Fatalf("清理的 Drive 错误不应计入号池级限流：记录 %d 个账号", observed)
			}
		})
	}
}

// TestQuotaCooldownKeepsLaterCooldown 同账号并发：租约较早的请求先把模型逐级冷却到 30 分钟（两个通道同步冷却，驻留 Worker 登记淘汰），
// 租约稍晚的请求随后遇到附件上传的 Drive 429（不逐级，只写本通道 30 秒）：不能把 Playground 缩短成 30 秒，只推进检查时间，
// 如实记一行“账号冷却未写入”并写实际生效的恢复时间；账号仍是死槽位
func TestQuotaCooldownKeepsLaterCooldown(t *testing.T) {
	// 逐级冷却要剩余不少于 rotationMinRemaining 才是死槽位
	skipNearQuotaDay(t, rotationMinRemaining+time.Minute)
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	// 第 1 档直接用第 3 档的时长，不必等前两档到期
	service.rateLimitLadder = []time.Duration{rateLimitLevel3, rateLimitLevel4}
	service.rateLimits = newRateLimitGuard()
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: rateLadderModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	// 两个租约都取 Playground（不固定时第二个租约会分到空闲的 Build 通道）
	selection := aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: rateLadderModel, Method: "generateContent", PlaygroundOnly: true}
	earlier, err := pool.AcquireFor(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = earlier.Release() }()
	time.Sleep(2 * time.Millisecond)
	later, err := pool.AcquireFor(ctx, selection)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = later.Release() }()
	if earlier.Channel() != aistudio.ChannelPlayground || later.Channel() != aistudio.ChannelPlayground || !later.CheckedAt().After(earlier.CheckedAt()) {
		t.Fatalf("两个租约应都在 Playground 且后者更晚：%s@%s %s@%s", earlier.Channel(), earlier.CheckedAt(), later.Channel(), later.CheckedAt())
	}
	if _, ok, stateErr := service.applyQuotaCooldown(earlier, partitionNormalA, rateLadderModel, bareRateLimitError(), "", aistudio.PoolScopeNormal, true, true); !ok || stateErr != nil {
		t.Fatalf("裸限流：ok=%t err=%v", ok, stateErr)
	}
	playground, build := rateCooldowns(t, pool, partitionNormalA)
	if time.Until(playground) < rotationMinRemaining || time.Until(build) < rotationMinRemaining {
		t.Fatalf("逐级冷却后两个通道都应剩余不少于 %s：Playground=%s Build=%s", rotationMinRemaining, playground, build)
	}
	if got := service.workers.retirementCount(); got != 1 {
		t.Fatalf("逐级冷却到 30 分钟后应登记淘汰，登记数 = %d", got)
	}
	cooldown, ok, stateErr := service.applyQuotaCooldown(later, partitionNormalA, rateLadderModel, driveRateLimitError(), "", aistudio.PoolScopeNormal, true, false)
	if !ok || stateErr != nil || cooldown.Kind != aistudio.RateLimitKind {
		t.Fatalf("附件上传的 Drive 429：%+v ok=%t err=%v", cooldown, ok, stateErr)
	}
	kept, keptBuild := rateCooldowns(t, pool, partitionNormalA)
	if !kept.Equal(playground) || !keptBuild.Equal(build) {
		t.Fatalf("较晚租约的短冷却不应缩短已有冷却：Playground %s → %s，Build %s → %s", playground, kept, build, keptBuild)
	}
	if remaining := time.Until(kept); remaining < rotationMinRemaining {
		t.Fatalf("Playground 剩余 %s，期望不少于 %s", remaining, rotationMinRemaining)
	}
	if !pool.AccountLongCooling(partitionNormalA, rateLadderModel, rotationMinRemaining) {
		t.Fatal("账号在该模型上应仍是死槽位")
	}
	if got := countSourceLogs(requests, partitionNormalA, "账号冷却未写入 | 类型=限流 | 范围="+rateLadderModel+" | 恢复="+playground.Format(time.RFC3339),
		"原因=已有更晚的冷却，保留原冷却"); got != 1 {
		t.Fatalf("应如实记一行未写入并写实际生效的恢复时间，日志 = %v", serviceLogMessages(requests))
	}
	if got := countSourceLogs(requests, partitionNormalA, "账号冷却 | 类型=限流"); got != 1 {
		t.Fatalf("只应有逐级冷却那一行写入，日志 = %v", serviceLogMessages(requests))
	}
	// 检查时间已推进到较晚的租约：较早租约的结果随后不再写回
	if _, ok, _ := service.applyQuotaCooldown(earlier, partitionNormalA, rateLadderModel, driveRateLimitError(), "", aistudio.PoolScopeNormal, true, false); !ok {
		t.Fatal("附件上传的 Drive 429 应被识别为额度错误")
	}
	if got := countSourceLogs(requests, partitionNormalA, "账号冷却未写入 | 类型=限流", "原因=同账号取得租约更晚的请求已写回结果"); got != 1 {
		t.Fatalf("较早租约应按过旧不写，日志 = %v", serviceLogMessages(requests))
	}
}

// TestRateLimitLevelTwoVictimExpandable 分区已满、唯一的驻留 Worker 在主用模型上到了第 2 档（两个通道都冷却 5 分钟）：
// 淘汰选号把它算作冷却中，扩容可以为备用账号替换它；第 1 档（30 秒）不替换
func TestRateLimitLevelTwoVictimExpandable(t *testing.T) {
	// 第 2 档要剩余不少于 ShortCooldownMax 才算冷却中
	skipNearQuotaDay(t, aistudio.ShortCooldownMax+time.Minute)
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: rateLadderModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.AcquireFor(ctx, aistudio.AccountSelection{AccountID: partitionNormalA, ModelID: rateLadderModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	cooldown, ok, stateErr := service.applyQuotaCooldown(lease, partitionNormalA, rateLadderModel, bareRateLimitError(), "", aistudio.PoolScopeNormal, true, true)
	_ = lease.Release()
	if !ok || stateErr != nil || cooldown.Kind != aistudio.RateLimitKind {
		t.Fatalf("裸限流：%+v ok=%t err=%v", cooldown, ok, stateErr)
	}
	if got := manager.standbyCapacity(manager.WarmAccountIDs(), rateLadderModel).expandable([]string{partitionNormalB}); got != "" {
		t.Fatalf("第 1 档（30 秒）不应为备用账号替换 A，实际 %q", got)
	}
	// 第 1 档冷却到期后取得的租约再次失败（不等真实的 30 秒：检查时间取第 1 档到期之后）
	result, err := pool.MarkRateLimitCooldown(aistudio.RateLimitMark{
		AccountID: partitionNormalA, ModelID: rateLadderModel, Channel: aistudio.ChannelPlayground,
		Generation: pool.ModelAccessGeneration(partitionNormalA), CheckedAt: cooldown.Until.Add(time.Millisecond),
		Cooldown: cooldown, Ladder: service.rateLimitSteps(), Shared: true, Escalate: true,
	})
	if err != nil || result.Level != 2 || !result.Synced {
		t.Fatalf("第二次裸限流：%+v err=%v，期望第 2 档并同步两个通道", result, err)
	}
	if got := manager.standbyCapacity(manager.WarmAccountIDs(), rateLadderModel).expandable([]string{partitionNormalB}); got != partitionNormalB {
		t.Fatalf("第 2 档冷却中的 A 应可被替换，扩容应返回 B，实际 %q", got)
	}
	if service.workers.retirementCount() != 0 {
		t.Fatal("第 2 档不应登记淘汰")
	}
}

// TestCapacityBusyDetailSnapshot 主用模型排队超限时错误带候选快照与主用模型供给里的驻留冷却数，写进“主用模型排队超过…”一句；
// 错误文案（进内部日志与用量账本）不含快照
func TestCapacityBusyDetailSnapshot(t *testing.T) {
	service, pool := primaryBusyService(t, nil, primaryTestA, primaryTestB)
	ctx, cancel := context.WithTimeout(service.lifecycle, 10*time.Second)
	defer cancel()
	release := holdLease(t, pool, primaryTestA)
	defer release()
	// B 驻留但对主用模型冷却：不进任何分组，供给里计为驻留冷却
	if err := pool.MarkCooldownIfGeneration(primaryTestB, testRuntimeModel, pool.ModelAccessGeneration(primaryTestB),
		time.Now(), time.Now().Add(2*time.Hour), aistudio.DailyQuotaKind+": 测试"); err != nil {
		t.Fatal(err)
	}
	service.setPrimaryModels([]string{testRuntimeModel}, 200*time.Millisecond)
	service.workers.refreshPrimarySupply(time.Now())
	if entry, ok := supplyOf(service.workers.primarySupplyStatus(), testRuntimeModel); !ok || entry.Warm != 1 || entry.Cooling != 1 {
		t.Fatalf("主用模型供给 = %+v，期望 热 1 · 冷却 1", entry)
	}
	lease, err, _ := acquirePrimary(ctx, service, aistudio.PoolScopeNormal)
	var busy *aistudio.CapacityBusyError
	if lease != nil || !errors.As(err, &busy) {
		t.Fatalf("lease=%v err=%v，期望号池繁忙", lease, err)
	}
	for _, fragment := range []string{" | 模型=" + testRuntimeModel, " | 同条件队列=1/1", " | 热空闲=0 | 热忙碌=1 | 备用=0 | 备用忙碌=0", " | 最早恢复=", " | 驻留冷却=1"} {
		if !strings.Contains(busy.Detail, fragment) {
			t.Fatalf("快照 %q 缺少 %q", busy.Detail, fragment)
		}
	}
	if !strings.HasSuffix(busy.Detail, " | 驻留冷却=1") {
		t.Fatalf("驻留冷却应在快照末尾：%q", busy.Detail)
	}
	if text := err.Error(); strings.Contains(text, "热忙碌") || strings.Contains(text, "驻留冷却") {
		t.Fatalf("错误文案不应含快照：%q", text)
	}
	if !strings.HasSuffix(primaryQueueExceededMessage(busy), busy.Detail) {
		t.Fatalf("排队超限的进度应以快照结尾：%q", primaryQueueExceededMessage(busy))
	}
	// 生成请求的进度里同样带快照
	if _, err, _ := runScopedPrimaryRequest(t, service, aistudio.PoolScopeNormal, "req-busy-detail", ""); !errors.As(err, &busy) {
		t.Fatalf("生成请求 err=%v，期望号池繁忙", err)
	}
	if !progressLoggedAll(service, "主用模型排队超过 200ms，立即返回 429 | 原因=账号全部忙碌", " | 热忙碌=1", " | 驻留冷却=1") {
		t.Fatal("请求进度应写明排队超限时的快照与驻留冷却数")
	}
}

// TestSupplyCoolingField 驻留冷却字段读主用模型供给缓存：Ultra 号池找 Ultra 列表的条目，其余找普通列表的条目；没有缓存或条目时为空
func TestSupplyCoolingField(t *testing.T) {
	var missing *accountWorkerManager
	if got := missing.supplyCoolingField(aistudio.PoolScopeNormal, "m"); got != "" {
		t.Fatalf("nil 管理器 = %q", got)
	}
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	if got := manager.supplyCoolingField(aistudio.PoolScopeNormal, "m"); got != "" {
		t.Fatalf("没有缓存时 = %q", got)
	}
	manager.setPrimaryModels([]string{"m"}, []string{"m"})
	manager.primarySupply.Store(&primarySupplySnapshot{entries: []api.AdminPrimarySupply{
		{Model: "m", Warm: 2, Cooling: 3}, {Model: "m", Pool: aistudio.PoolScopeUltra.String(), Cooling: 5},
	}})
	for pool, want := range map[aistudio.PoolScope]string{
		aistudio.PoolScopeNormal: " | 驻留冷却=3", aistudio.PoolScopeAll: " | 驻留冷却=3", aistudio.PoolScopeUltra: " | 驻留冷却=5",
	} {
		if got := manager.supplyCoolingField(pool, "models/M"); got != want {
			t.Fatalf("号池 %q = %q，期望 %q", pool.String(), got, want)
		}
	}
	if got := manager.supplyCoolingField(aistudio.PoolScopeNormal, "other"); got != "" {
		t.Fatalf("没有条目的模型 = %q", got)
	}
}
