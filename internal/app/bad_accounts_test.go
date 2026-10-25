package app

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// TestBadAccountMemoryStreakAndPersistence 按账号连续 2 次裸限流、期间没有成功就标坏号（写一条日志）；成功一次即清除；
// 候选整理去掉坏号、近期成功的排前，全是坏号时原样返回；记录写盘后重启（重新打开）仍在，过期的丢掉
func TestBadAccountMemoryStreakAndPersistence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	path := filepath.Join(t.TempDir(), badAccountFileName)
	now := time.Unix(2_000_000_000, 0)
	clock := func() time.Time { return now }
	memory := openBadAccountMemory(path, requests)
	memory.now = clock
	memory.observe("bad@example.com", "bad", egressBare)
	if memory.bad("bad@example.com") {
		t.Fatal("一次裸限流不应标坏号")
	}
	memory.observe("bad@example.com", "bad", egressOther)
	memory.observe("bad@example.com", "bad", egressBare)
	if !memory.bad("bad@example.com") || memory.count() != 1 {
		t.Fatal("连续 2 次裸限流（中间的其他失败不算成功）应标坏号")
	}
	if !hasLogContaining(requests, "账号连续裸限流，移出预热与替补候选 | 连续=2 | 时长=2h0m0s | 恢复=") {
		t.Fatalf("应记录标坏号，日志 = %v", serviceLogMessages(requests))
	}
	memory.observe("good@example.com", "good", egressSuccess)
	memory.observe("flip@example.com", "flip", egressBare)
	memory.observe("flip@example.com", "flip", egressSuccess)
	memory.observe("flip@example.com", "flip", egressBare)
	if memory.bad("flip@example.com") {
		t.Fatal("中间有成功时连续次数应清零")
	}
	ordered := memory.order([]string{"bad@example.com", "neutral@example.com", "good@example.com"})
	if !slices.Equal(ordered, []string{"good@example.com", "neutral@example.com"}) {
		t.Fatalf("候选整理 = %v，期望近期成功的在前、坏号去掉", ordered)
	}
	if only := memory.order([]string{"bad@example.com"}); !slices.Equal(only, []string{"bad@example.com"}) {
		t.Fatalf("全是坏号时应原样返回，实际 %v", only)
	}
	if err := memory.close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBadAccountMemory(path, requests)
	reopened.now = clock
	if !reopened.bad("bad@example.com") || reopened.bad("flip@example.com") {
		t.Fatal("重启后坏号记录应保留")
	}
	if ordered := reopened.order([]string{"neutral@example.com", "good@example.com"}); ordered[0] != "good@example.com" {
		t.Fatalf("重启后近期成功记录应保留，候选整理 = %v", ordered)
	}
	// 坏号到期后（2 小时）重新打开：不再是坏号
	later := openBadAccountMemory(path, requests)
	later.now = func() time.Time { return now.Add(badAccountHold + time.Minute) }
	if later.bad("bad@example.com") {
		t.Fatal("到期的坏号记录应丢掉")
	}
	reopened.observe("bad@example.com", "bad", egressSuccess)
	if reopened.bad("bad@example.com") {
		t.Fatal("成功一次应清除坏号")
	}
}

// TestBadAccountPardon IP 池端口按规则 A 冷却时撤回坏号记忆（pardon）：清零连续次数（之后再有一次裸限流不标坏号）；
// 窗口内标上的坏号清除并写盘，近期成功记录保留；窗口之前标上的坏号保留；没有记录的账号不报撤回
func TestBadAccountPardon(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	path := filepath.Join(t.TempDir(), badAccountFileName)
	now := time.Unix(2_000_000_000, 0)
	memory := openBadAccountMemory(path, requests)
	memory.now = func() time.Time { return now }
	const window = 10 * time.Minute
	memory.observe("old@example.com", "old", egressBare)
	memory.observe("old@example.com", "old", egressBare)
	now = now.Add(window + time.Minute)
	memory.observe("fresh@example.com", "fresh", egressSuccess)
	memory.observe("fresh@example.com", "fresh", egressBare)
	memory.observe("fresh@example.com", "fresh", egressBare)
	memory.observe("streak@example.com", "streak", egressBare)
	if !memory.bad("old@example.com") || !memory.bad("fresh@example.com") || memory.bad("streak@example.com") {
		t.Fatal("前提：old、fresh 已标坏号，streak 只有一次裸限流")
	}
	if memory.pardon("old@example.com", window) || !memory.bad("old@example.com") {
		t.Fatal("窗口之前标上的坏号不应撤回")
	}
	if !memory.pardon("fresh@example.com", window) || memory.bad("fresh@example.com") {
		t.Fatal("窗口内标上的坏号应撤回")
	}
	if memory.pardon("streak@example.com", window) || memory.pardon("none@example.com", window) || memory.pardon("", window) {
		t.Fatal("不是坏号的账号不报撤回")
	}
	memory.observe("streak@example.com", "streak", egressBare)
	memory.observe("fresh@example.com", "fresh", egressBare)
	if memory.bad("streak@example.com") || memory.bad("fresh@example.com") {
		t.Fatal("撤回后连续次数应清零：再有一次裸限流不标坏号")
	}
	memory.observe("old@example.com", "old", egressBare)
	if ordered := memory.order([]string{"neutral@example.com", "fresh@example.com"}); ordered[0] != "fresh@example.com" {
		t.Fatalf("撤回不动近期成功记录：%v", ordered)
	}
	if err := memory.close(); err != nil {
		t.Fatal(err)
	}
	reopened := openBadAccountMemory(path, requests)
	reopened.now = memory.now
	if reopened.bad("fresh@example.com") || !reopened.bad("old@example.com") {
		t.Fatal("撤回应写盘：重启后 fresh 不是坏号、old 仍是")
	}
	var nilMemory *badAccountMemory
	if nilMemory.pardon("a@example.com", window) {
		t.Fatal("nil 记忆不报撤回")
	}
}

// TestBadAccountsSavedOnStop 停止服务时立即写出坏号记忆（不等 30 秒的合并写盘）：重建运行时先停旧服务、再建新一代（新一代此时读文件），
// 最后才关闭旧一代，原先新一代读到的是旧数据
func TestBadAccountsSavedOnStop(t *testing.T) {
	manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	service := retireTestService(ctx, manager, pool, requests)
	path := filepath.Join(t.TempDir(), badAccountFileName)
	memory := openBadAccountMemory(path, requests)
	manager.badAccounts.Store(memory)
	t.Cleanup(func() { _ = memory.close() })
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalA, "", egressBare)
	if next := openBadAccountMemory(path, requests); next.bad(partitionNormalA) {
		t.Fatal("停止前还没到合并写盘的时间，文件里不应已有记录")
	}
	modelRefreshDone := make(chan struct{})
	close(modelRefreshDone)
	service.finishStop(make(chan struct{}), modelRefreshDone)
	if next := openBadAccountMemory(path, requests); !next.bad(partitionNormalA) {
		t.Fatal("停止服务后新一代应读到坏号记录")
	}
}

// TestBadAccountsSkippedByPrewarm 预热不选坏号（还有其他候选时），近期成功过的账号优先
func TestBadAccountsSkippedByPrewarm(t *testing.T) {
	manager, _, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB, partitionNormalC}, nil, 1, 3, 0, 1)
	memory := openBadAccountMemory("", requests)
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalA, "", egressBare)
	memory.observe(partitionNormalC, "", egressSuccess)
	manager.badAccounts.Store(memory)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := <-manager.StartPrewarm(ctx); err != nil {
		t.Fatal(err)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalC}) {
		t.Fatalf("预热应选近期成功过的 C、不选坏号 A，驻留 Worker = %v", warm)
	}
	if got := manager.withoutBadAccounts([]string{partitionNormalA, partitionNormalB}); !slices.Equal(got, []string{partitionNormalB}) {
		t.Fatalf("按需提升与扩容的候选应去掉坏号，实际 %v", got)
	}
}

// forbidPrimary 像单模型 403 无权限一样，在 Playground 与 Build 两个通道上给账号的主用模型写入 6 小时冷却（原因带“无权限（模型）”前缀）；
// 账号仍能服务其他模型
func forbidPrimary(t *testing.T, env *drainEnv, accountID string) {
	t.Helper()
	coolBothChannels(t, env, accountID, drainProModel, forbiddenModelRepeatCooldown, forbiddenModelReasonPrefix+"The caller does not have permission")
	if !env.pool.AccountLongCooling(accountID, drainProModel, rotationMinRemaining) || env.pool.AccountLongCooling(accountID, testRuntimeModel, rotationMinRemaining) {
		t.Fatalf("%s 应只在主用模型上长时间冷却", accountID)
	}
}

// coolBothChannels 在 Playground 与 Build 两个通道上给账号的 modelID 写入剩余 remaining 的冷却
func coolBothChannels(t *testing.T, env *drainEnv, accountID string, modelID string, remaining time.Duration, reason string) {
	t.Helper()
	now := time.Now()
	for _, scope := range []string{modelID, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, modelID)} {
		if err := env.pool.MarkCooldownIfGeneration(
			accountID, scope, env.pool.ModelAccessGeneration(accountID), now, now.Add(remaining), reason,
		); err != nil {
			t.Fatal(err)
		}
	}
}

// waitPrewarmDone 等预热这一轮结束并且驻留 Worker 达到 want 个（StartPrewarm 在第一个 Worker 就绪时就返回）
func waitPrewarmDone(t *testing.T, manager *accountWorkerManager, want int) {
	t.Helper()
	if !waitUntil(10*time.Second, func() bool { return !manager.fillActive.Load() && len(manager.WarmAccountIDs()) >= want }) {
		t.Fatalf("预热没有在期限内补到 %d 个，驻留 Worker = %v", want, sortedWarm(manager))
	}
}

// TestPrewarmPrefersPrimaryOverRecentSuccess 预热先启动能服务主用模型的账号：近期成功过、但对主用模型单模型 403 冷却（两个通道都冷却，
// 仍能服务其他模型）的 A、B 不再整组排在从没成功过、能服务主用模型的 X、Y 前面。原先先按能否服务排序、再把近期成功的整组前移，
// 线上 2500 个这样的账号让预热一直启动对主用模型冷却的账号，476 个能服务主用模型的就绪账号一直没有启动
func TestPrewarmPrefersPrimaryOverRecentSuccess(t *testing.T) {
	accounts := []string{"order-a@example.com", "order-b@example.com", "order-x@example.com", "order-y@example.com"}
	env := newDrainEnv(t, accounts, 2, 4)
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	for _, accountID := range accounts[:2] {
		memory.observe(accountID, "", egressSuccess)
		forbidPrimary(t, env, accountID)
	}
	want := []string{accounts[2], accounts[3], accounts[0], accounts[1]}
	if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, want) {
		t.Fatalf("冷启动候选排序 = %v，期望 %v：能服务主用模型的 X、Y 在前，近期成功过、对主用模型冷却的 A、B 在后", got, want)
	}
	if err := <-env.manager.StartPrewarm(env.ctx); err != nil {
		t.Fatal(err)
	}
	waitPrewarmDone(t, env.manager, 2)
	if warm := sortedWarm(env.manager); !slices.Equal(warm, accounts[2:]) {
		t.Fatalf("预热应启动能服务主用模型的 X、Y，驻留 Worker = %v", warm)
	}
}

// TestColdCandidatesRecentFirstAmongServeable 都能服务主用模型时近期成功过的账号仍排前、坏号仍移出；对主用模型冷却的账号排在能服务的之后，
// 它们之间同样是近期成功过的在前。预热按这个顺序启动
func TestColdCandidatesRecentFirstAmongServeable(t *testing.T) {
	const (
		bad      = "order-bad@example.com"
		coolOld  = "order-cool-n@example.com"
		coolNew  = "order-cool-r@example.com"
		neutral  = "order-neutral@example.com"
		recently = "order-recent@example.com"
	)
	accounts := []string{bad, coolOld, coolNew, neutral, recently}
	env := newDrainEnv(t, accounts, 1, 5)
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	memory.observe(bad, "", egressBare)
	memory.observe(bad, "", egressBare)
	memory.observe(coolNew, "", egressSuccess)
	memory.observe(recently, "", egressSuccess)
	forbidPrimary(t, env, coolOld)
	forbidPrimary(t, env, coolNew)
	want := []string{recently, neutral, coolNew, coolOld}
	if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, want) {
		t.Fatalf("冷启动候选排序 = %v，期望 %v", got, want)
	}
	if err := <-env.manager.StartPrewarm(env.ctx); err != nil {
		t.Fatal(err)
	}
	waitPrewarmDone(t, env.manager, 1)
	if warm := sortedWarm(env.manager); !slices.Equal(warm, []string{recently}) {
		t.Fatalf("预热应启动近期成功过、能服务主用模型的账号，驻留 Worker = %v", warm)
	}
}

// TestColdCandidatesPrimaryBeforeHot 主用模型先比、热门模型后比：热门模型里有主用模型以外的模型（如大量每日限额的 flash）时，
// 能服务主用模型、但在热门模型上冷却的 X 排在近期成功过、对主用模型 403 冷却、仍能服务热门模型的 R 前面（两类合在一起按个数计分时
// X、R 同分，近期成功的 R 排前，预热启动的是对主用模型冷却的 R）；主用模型个数相同时再按热门模型个数，近期成功只在两者都相同时优先
func TestColdCandidatesPrimaryBeforeHot(t *testing.T) {
	const (
		forbidden = "order-hot-r@example.com"
		hotCooled = "order-hot-x@example.com"
		capable   = "order-hot-y@example.com"
	)
	accounts := []string{forbidden, hotCooled, capable}
	env := newDrainEnv(t, accounts, 2, 3)
	env.manager.setHotModels([]string{testRuntimeModel})
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	memory.observe(forbidden, "", egressSuccess)
	forbidPrimary(t, env, forbidden)
	coolBothChannels(t, env, hotCooled, testRuntimeModel, 2*time.Hour, "每日限额")
	counts := env.pool.ServeableCounts(accounts, env.manager.deadSlotModelsFor(aistudio.PoolScopeNormal), rotationMinRemaining)
	if counts[forbidden] != 1 || counts[hotCooled] != 1 || counts[capable] != 2 {
		t.Fatalf("主用与热门合计个数 = %v，期望 R、X 同为 1、Y 为 2（这个用例要覆盖合计同分的情形）", counts)
	}
	want := []string{capable, hotCooled, forbidden}
	if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, want) {
		t.Fatalf("冷启动候选排序 = %v，期望 %v：能服务主用模型的排在只能服务热门模型的近期成功账号前面", got, want)
	}
	// X 也近期成功过：主用模型个数与 Y 相同，热门模型少一个，仍排在 Y 后面
	memory.observe(hotCooled, "", egressSuccess)
	if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, want) {
		t.Fatalf("冷启动候选排序 = %v，期望 %v：近期成功只在主用与热门模型个数都相同时优先", got, want)
	}
	if err := <-env.manager.StartPrewarm(env.ctx); err != nil {
		t.Fatal(err)
	}
	waitPrewarmDone(t, env.manager, 2)
	if warm := sortedWarm(env.manager); !slices.Equal(warm, []string{hotCooled, capable}) {
		t.Fatalf("预热应启动能服务主用模型的 X、Y，驻留 Worker = %v", warm)
	}
}

// TestColdCandidatesShortForbiddenAfterUncooled 排序按 coldRankMinRemaining（2 分钟）判断能否服务：对主用模型的 403 冷却只剩 10 分钟的
// 近期成功账号 R 排在从没成功过、没有冷却的 X 后面（按 15 分钟算时 R 能服务、与 X 同分，近期成功的 R 排前，冷却一到期多半又是 403）；
// 剩余不到 2 分钟的短冷却（分钟限额）仍算能服务，近期成功的 S 照样排前。替补同一排序，S 成了坏号后选 X，不选 R
func TestColdCandidatesShortForbiddenAfterUncooled(t *testing.T) {
	const (
		forbidden = "order-short-r@example.com"
		minute    = "order-short-s@example.com"
		fresh     = "order-short-x@example.com"
	)
	accounts := []string{forbidden, minute, fresh}
	env := newDrainEnv(t, accounts, 1, 3)
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	memory.observe(forbidden, "", egressSuccess)
	memory.observe(minute, "", egressSuccess)
	coolBothChannels(t, env, forbidden, drainProModel, 10*time.Minute, forbiddenModelReasonPrefix+"The caller does not have permission")
	coolBothChannels(t, env, minute, drainProModel, time.Minute, aistudio.MinuteQuotaKind+": per minute")
	if env.pool.AccountLongCooling(forbidden, drainProModel, rotationMinRemaining) {
		t.Fatalf("%s 的冷却只剩 10 分钟，按 15 分钟不算长时间冷却（这个用例要覆盖这个窗口）", forbidden)
	}
	want := []string{minute, fresh, forbidden}
	if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, want) {
		t.Fatalf("冷启动候选排序 = %v，期望 %v", got, want)
	}
	models := []string{testRuntimeModel}
	if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != minute {
		t.Fatalf("替补应选近期成功过、只有短冷却的 %s，实际 %q", minute, got)
	}
	memory.observe(minute, "", egressBare)
	memory.observe(minute, "", egressBare)
	if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != fresh {
		t.Fatalf("替补应选没有冷却的 %s，不选主用模型 403 冷却还剩 10 分钟的近期成功账号，实际 %q", fresh, got)
	}
}

// TestReplacementPrefersPrimaryOverRecentSuccess 替补（登记的是其他模型，候选里有对主用模型冷却的账号）与预热同一排序：
// 能服务主用模型的账号先于近期成功过、对主用模型 403 冷却的账号与目录里没有主用模型的近期成功账号（只看冷却键时它“没冷却”，
// 原先会被选中）；能服务主用模型的账号之间近期成功过的在前，它成了坏号后换成另一个能服务主用模型的
func TestReplacementPrefersPrimaryOverRecentSuccess(t *testing.T) {
	const (
		noPrimary = "order-n@example.com"
		forbidden = "order-r@example.com"
		fresh     = "order-x@example.com"
		recently  = "order-y@example.com"
	)
	env := newDrainEnv(t, []string{noPrimary, forbidden, fresh, recently}, 1, 4)
	if err := env.pool.SetCatalog(noPrimary, aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	forbidPrimary(t, env, forbidden)
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	for _, accountID := range []string{noPrimary, forbidden, recently} {
		memory.observe(accountID, "", egressSuccess)
	}
	models := []string{testRuntimeModel}
	if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != recently {
		t.Fatalf("替补应选近期成功过、能服务主用模型的 %s，实际 %q", recently, got)
	}
	memory.observe(recently, "", egressBare)
	memory.observe(recently, "", egressBare)
	if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != fresh {
		t.Fatalf("近期成功的账号成了坏号后替补应选能服务主用模型的 %s，实际 %q", fresh, got)
	}
}

// TestEgressFreshRejectsExcludeBadAccounts 出口判定的“冷号首发被拒”不计已标坏号的账号
func TestEgressFreshRejectsExcludeBadAccounts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newEgressGuard(nil, newRequestRegistry(ctx))
	bad := map[string]bool{"bad@example.com": true}
	guard.badAccount = func(accountID string) bool { return bad[accountID] }
	now := time.Now()
	guard.observeOutcome(egressObservation{Key: "", AccountID: "bad@example.com", Outcome: egressBare, At: now})
	guard.observeOutcome(egressObservation{Key: "", AccountID: "fresh@example.com", Outcome: egressBare, At: now})
	status := guard.status()
	if len(status) != 1 || status[0].FreshRejects2m != 1 || status[0].Bare1m != 2 {
		t.Fatalf("出口状态 = %+v，期望裸 429 计 2 次、冷号首发只计不是坏号的 1 次", status)
	}
}

// TestReplaceVictimConsistency 现场提升判断“能否替换”与按需替换用同一个口径（N3）：同分区有 Worker 在启动时，对请求模型不可服务的
// 健康空闲 Worker 也可以换掉，两处结论一致；能服务、刚用过的健康 Worker 两处都不换。ensureWorker 据此换掉它，不再报 Worker 容量已满
func TestReplaceVictimConsistency(t *testing.T) {
	const otherModel = "other-model"
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	if err := pool.SetCatalog(partitionNormalB, aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: otherModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	releaseLease := holdLease(t, pool, partitionNormalA)
	releaseLease()
	// 模拟同分区另一个账号正在启动（占一个槽位，分区已满）
	const opening = "opening@example.com"
	manager.rebalanceMu.Lock()
	manager.openings[opening] = make(chan struct{})
	manager.openingSet.Store(opening, struct{}{})
	manager.rebalanceMu.Unlock()
	defer func() {
		manager.rebalanceMu.Lock()
		delete(manager.openings, opening)
		manager.openingSet.Delete(opening)
		manager.rebalanceMu.Unlock()
	}()
	normal := aistudio.PoolScopeNormal
	if victim := manager.replaceVictimFor(partitionNormalB, otherModel, normal); victim != partitionNormalA {
		t.Fatalf("A 对 %s 不可服务，有 Worker 在启动时也应可替换，实际 %q", otherModel, victim)
	}
	other := manager.standbyCapacity(manager.WarmAccountIDs(), otherModel)
	if promoted := other.promotable([]string{partitionNormalB}, true); promoted != partitionNormalB {
		t.Fatalf("现场提升应与替换口径一致、选 B，实际 %q", promoted)
	}
	if victim := manager.replaceVictimFor(partitionNormalB, testRuntimeModel, normal); victim != "" {
		t.Fatalf("A 能服务 %s、刚用过，有 Worker 在启动时不应替换，实际 %q", testRuntimeModel, victim)
	}
	same := manager.standbyCapacity(manager.WarmAccountIDs(), testRuntimeModel)
	if promoted := same.promotable([]string{partitionNormalB}, true); promoted != "" {
		t.Fatalf("没有可替换的 Worker 时现场提升也不应选号，实际 %q", promoted)
	}
	if _, err := manager.ensureWorker(ctx, partitionNormalB, otherModel, false); err != nil {
		t.Fatalf("按需替换应换掉不可服务的 A，实际 %v", err)
	}
	if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalB}) {
		t.Fatalf("驻留 Worker = %v，期望 B 换下 A", warm)
	}
}

// TestReplaceVictimWarmupGrace 按需替换与扩容不换刚就绪不到 rotationWarmupGrace 的 Worker（N6），不可调度的除外
func TestReplaceVictimWarmupGrace(t *testing.T) {
	manager, pool, _ := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, partitionNormalA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	normal := aistudio.PoolScopeNormal
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("没有宽限时应可替换 A，实际 %q", victim)
	}
	manager.replaceGrace.Store(int64(rotationWarmupGrace))
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != "" {
		t.Fatalf("刚就绪的 A 在宽限内不应被替换，实际 %q", victim)
	}
	if victim := manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("宽限只作用于替换与扩容，其余淘汰照旧，实际 %q", victim)
	}
	if err := pool.MarkAuthRequired(partitionNormalA, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	if victim := manager.pickReplaceVictim("", testRuntimeModel, victimAny, normal); victim != partitionNormalA {
		t.Fatalf("不可调度的 Worker 不受宽限，实际 %q", victim)
	}
}

// TestReplacementRatePerMinute 每个分区每分钟的按需替换不超过上限（N6），预热补位不受限；没有设上限时（测试构造的管理器）不限
func TestReplacementRatePerMinute(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	ctx := context.Background()
	normal := aistudio.PoolScopeNormal
	for range 20 {
		manager.noteReplacement(ctx, normal)
	}
	if !manager.replacementAllowed(ctx, normal) {
		t.Fatal("没有设上限时不应限制")
	}
	manager.replacements.limit.Store(2)
	manager.noteReplacement(ctx, normal)
	manager.noteReplacement(ctx, normal)
	if manager.replacementAllowed(ctx, normal) {
		t.Fatal("达到上限后不应再替换")
	}
	if !manager.replacementAllowed(ctx, aistudio.PoolScopeUltra) {
		t.Fatal("上限按分区计")
	}
	if !manager.replacementAllowed(withWorkerStartSource(ctx, workerStartPrewarm), normal) {
		t.Fatal("预热补位不受上限限制")
	}
	manager.replacements.mu.Lock()
	for index := range manager.replacements.times[normal] {
		manager.replacements.times[normal][index] = time.Now().Add(-2 * replacementWindow)
	}
	manager.replacements.mu.Unlock()
	if !manager.replacementAllowed(ctx, normal) {
		t.Fatal("窗口外的替换不再计数")
	}
}

// TestPrimaryBudgetCreditsOwnExpansion 主用模型等这次选号发起的后台扩容（N4）：等待的时间延后截止时间（与现场冷启动共用 INIT_TIMEOUT 的抵扣），
// 不再排队上限一到就 429；抵扣用完后按普通容量原因处理
func TestPrimaryBudgetCreditsOwnExpansion(t *testing.T) {
	budget := &leaseWaitBudget{limit: 50 * time.Millisecond, startedAt: time.Now(), expansionCredit: time.Second, modelID: testRuntimeModel}
	ctx := context.Background()
	for range 4 {
		if err := budget.wait(ctx, nil, 60*time.Millisecond, waitReasonExpanding); err != nil {
			t.Fatalf("等后台扩容时不应按排队超限结束，实际 %v", err)
		}
	}
	if budget.coldStart < 200*time.Millisecond || budget.expired() {
		t.Fatalf("抵扣 = %s，截止时间应已延后", budget.coldStart)
	}
	limited := &leaseWaitBudget{limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: 30 * time.Millisecond, modelID: testRuntimeModel}
	var err error
	for range 10 {
		if err = limited.wait(ctx, nil, 20*time.Millisecond, waitReasonExpanding); err != nil {
			break
		}
	}
	if !isCapacityBusy(err) || limited.coldStart > 30*time.Millisecond {
		t.Fatalf("抵扣用完后应按排队超限结束: err=%v 抵扣=%s", err, limited.coldStart)
	}
	plain := &leaseWaitBudget{limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: time.Second, modelID: testRuntimeModel}
	for range 10 {
		if err = plain.wait(ctx, nil, 20*time.Millisecond, "账号全部忙碌"); err != nil {
			break
		}
	}
	if !isCapacityBusy(err) || plain.coldStart != 0 {
		t.Fatalf("其他容量原因不抵扣: err=%v 抵扣=%s", err, plain.coldStart)
	}
}

// TestPrimaryBudgetExpansionHoldsStreamStart 等后台扩容延后截止时间时，Ultra 主用模型的流式请求写出响应头前的等待同步推迟
// （与现场冷启动相同）：截止时间之前的 429 仍以 HTTP 状态返回；排队超限的文案把等后台扩容与现场冷启动分开写
func TestPrimaryBudgetExpansionHoldsStreamStart(t *testing.T) {
	hold := aistudio.NewStreamStartHold()
	budget := &leaseWaitBudget{
		limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: 80 * time.Millisecond,
		modelID: testRuntimeModel, pool: aistudio.PoolScopeUltra, quotaFailFast: true,
	}
	budget.holdStreamStart(hold)
	ctx := context.Background()
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		_ = budget.wait(ctx, nil, 60*time.Millisecond, waitReasonExpanding)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, starting := hold.State(); starting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("等后台扩容期间写出响应头前的等待应被推迟（hold 标为启动中）")
		}
		time.Sleep(time.Millisecond)
	}
	<-waited
	extension, starting := hold.State()
	if starting || extension <= 0 || extension != budget.coldStart || budget.expansion != budget.coldStart {
		t.Fatalf("等待结束后 hold 延长 %s（启动中=%v），抵扣 %s、其中扩容 %s，期望延长等于抵扣", extension, starting, budget.coldStart, budget.expansion)
	}
	var err error
	for range 20 {
		if err = budget.wait(ctx, nil, 20*time.Millisecond, waitReasonExpanding); err != nil {
			break
		}
	}
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) {
		t.Fatalf("抵扣用完后应排队超限，实际 %v", err)
	}
	if busy.ColdStart != 0 || busy.Expansion != budget.coldStart || budget.coldStart > 80*time.Millisecond {
		t.Fatalf("排队超限：现场冷启动 %s、等后台扩容 %s，抵扣 %s", busy.ColdStart, busy.Expansion, budget.coldStart)
	}
	if text := busy.Error(); strings.Contains(text, "含现场冷启动") || !strings.Contains(text, "（含等后台扩容 ") {
		t.Fatalf("文案应只写等后台扩容：%s", text)
	}
	if extension, _ := hold.State(); extension != budget.coldStart {
		t.Fatalf("hold 延长 %s，抵扣 %s，两者应同步", extension, budget.coldStart)
	}
}

// isCapacityBusy 判断错误是否为排队超限
func isCapacityBusy(err error) bool {
	_, ok := err.(*aistudio.CapacityBusyError)
	return ok
}
