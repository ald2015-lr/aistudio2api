package aistudio

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

// 合成号池的规模，按用户服务器实测环境设置：14,856 Free + 77 Ultra，其中约 12,000 个账户处于全局（"*"）每日限额冷却，
// 140 个热 Worker 账户，每个账户目录都含 gemini-3.1-pro-preview
const (
	scaleFreeAccounts    = 14856
	scaleUltraAccounts   = 77
	scaleCoolingAccounts = 12000
	scaleWarmAccounts    = 140
	scaleModel           = "gemini-3.1-pro-preview"
	scaleAliasModel      = "gemini-flash-latest"
)

// scaleCatalog 返回合成账户的模型目录：主用模型、带别名的 flash 模型以及若干陪衬模型
func scaleCatalog() []Model {
	models := []Model{
		{
			ID: scaleModel, Methods: []string{"generateContent", "countTokens", "bidiGenerateContent"},
			Capabilities: map[string]bool{"chat_model": true, "thinking": true},
		},
		{
			ID: "gemini-2.5-flash", Methods: []string{"generateContent", "countTokens"},
			Capabilities:      map[string]bool{"chat_model": true},
			CapabilityOptions: map[string][]string{"aliases": {scaleAliasModel}},
		},
		{
			ID: "gemini-3.1-flash-lite-preview", Methods: []string{"generateContent", "countTokens"},
			Capabilities: map[string]bool{"chat_model": true},
		},
		{
			ID: "imagen-4.0", Methods: []string{"predict"},
			Capabilities: map[string]bool{"image_generation": true},
		},
	}
	for index := 0; index < 20; index++ {
		models = append(models, Model{
			ID: fmt.Sprintf("filler-model-%02d", index), Methods: []string{"generateContent", "countTokens"},
			Capabilities: map[string]bool{"chat_model": true},
		})
	}
	return models
}

// scaleAccount 构造一个只在内存中的账户（没有目录，不读写磁盘）；目录内容键按 SetCatalog 的方式用目录指纹预先写好
func scaleAccount(id string, tier BenefitTier, catalog []Model, coolingUntil time.Time) *Account {
	fingerprint, _ := accountCatalogFingerprint(tier, catalog)
	account := &Account{
		ID:          id,
		Config:      AccountConfig{Label: id, Enabled: true, Locale: "en-US", Timezone: "UTC"},
		State:       AccountReady,
		BenefitTier: tier,
		Models:      cloneAccountModels(catalog),
		runtime: accountRuntimeState{
			Cooldowns: make(map[string]CooldownState), Resources: make(map[string]ResourceBinding),
			ModelAccess: make(map[string]ModelAccess), BenefitTier: tier, CatalogFingerprint: "scale",
		},
		initializedAt: time.Now(),
	}
	setCatalogKeyLocked(account, fingerprint)
	if !coolingUntil.IsZero() {
		account.runtime.Cooldowns[globalCooldownKey] = CooldownState{Until: coolingUntil.UTC(), Reason: "每日限额"}
	}
	return account
}

// scalePool 构造合成号池，返回号池与 140 个热账户 ID（都取未冷却的 Free 账户）
func scalePool(t testing.TB) (*AccountPool, []string) {
	t.Helper()
	catalog := scaleCatalog()
	coolingUntil := time.Now().Add(20 * time.Hour)
	accounts := make([]*Account, 0, scaleFreeAccounts+scaleUltraAccounts)
	warm := make([]string, 0, scaleWarmAccounts)
	cooling := 0
	for index := 0; index < scaleFreeAccounts; index++ {
		id := fmt.Sprintf("free-%05d@example.com", index)
		until := time.Time{}
		// 前 140 个 Free 账户为热账户且未冷却；其余账户中冷却账户分散在整个序列里，避免可用账户集中在一端
		if index >= scaleWarmAccounts && index%6 != 0 && cooling < scaleCoolingAccounts {
			until = coolingUntil
			cooling++
		}
		if until.IsZero() && len(warm) < scaleWarmAccounts {
			warm = append(warm, id)
		}
		accounts = append(accounts, scaleAccount(id, BenefitTierFree, catalog, until))
	}
	for index := 0; index < scaleUltraAccounts; index++ {
		accounts = append(accounts, scaleAccount(fmt.Sprintf("ultra-%03d@example.com", index), BenefitTierUltra, catalog, time.Time{}))
	}
	pool := NewAccountPool(accounts, 4)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground})
	return pool, warm
}

// scaleHarnessDuration 返回压测时长：默认 2 秒保证单元测试快速结束，POOL_SCALE_SECONDS 可改为 10 秒复现报告数据
func scaleHarnessDuration() time.Duration {
	return time.Duration(scaleHarnessEnv("POOL_SCALE_SECONDS", 2)) * time.Second
}

// scaleHarnessGoroutines 返回压测并发数，默认 64，POOL_SCALE_GOROUTINES 可改
func scaleHarnessGoroutines() int {
	return scaleHarnessEnv("POOL_SCALE_GOROUTINES", 64)
}

func scaleHarnessEnv(name string, fallback int) int {
	if value := os.Getenv(name); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}

// latencySummary 为一组耗时的分位统计
type latencySummary struct {
	count          int
	p50, p99, max  time.Duration
	total          time.Duration
	throughputPerS float64
}

func summarizeLatencies(samples []time.Duration, elapsed time.Duration) latencySummary {
	sort.Slice(samples, func(left, right int) bool { return samples[left] < samples[right] })
	summary := latencySummary{count: len(samples)}
	if len(samples) == 0 {
		return summary
	}
	for _, sample := range samples {
		summary.total += sample
	}
	summary.p50 = samples[len(samples)/2]
	summary.p99 = samples[min(len(samples)-1, len(samples)*99/100)]
	summary.max = samples[len(samples)-1]
	summary.throughputPerS = float64(len(samples)) / elapsed.Seconds()
	return summary
}

// poolLockSnapshot 记录账户池锁统计的快照，用于计算区间内的持锁时间
type poolLockSnapshot struct {
	holdNanos, waitNanos, acquisitions, slowHolds int64
}

func snapshotPoolLock() poolLockSnapshot {
	return poolLockSnapshot{
		holdNanos: poolLockCounters.holdNanos.Load(), waitNanos: poolLockCounters.waitNanos.Load(),
		acquisitions: poolLockCounters.acquisitions.Load(), slowHolds: poolLockCounters.slowHolds.Load(),
	}
}

// measureClassify 用 workers 个 goroutine 循环调用 ClassifyCandidates，返回调用耗时分位与区间内的锁统计
func measureClassify(
	t testing.TB, pool *AccountPool, selection AccountSelection, warm []string, workers int, duration time.Duration,
) (latencySummary, time.Duration, time.Duration) {
	t.Helper()
	ctx := context.Background()
	before := snapshotPoolLock()
	maxHoldBefore := poolLockCounters.maxHoldNanos.Load()
	poolLockCounters.maxHoldNanos.Store(0)
	deadline := time.Now().Add(duration)
	perWorker := make([][]time.Duration, workers)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	started := time.Now()
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			samples := make([]time.Duration, 0, 4096)
			for time.Now().Before(deadline) {
				begin := time.Now()
				groups, err := pool.ClassifyCandidates(ctx, selection, warm)
				samples = append(samples, time.Since(begin))
				if err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					errMu.Unlock()
					return
				}
				if len(groups.WarmReady)+len(groups.WarmAvailable)+len(groups.StandbyReady) == 0 {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("分类结果没有可用账户")
					}
					errMu.Unlock()
					return
				}
			}
			perWorker[worker] = samples
		}(worker)
	}
	wg.Wait()
	elapsed := time.Since(started)
	if firstErr != nil {
		t.Fatalf("ClassifyCandidates 压测失败: %v", firstErr)
	}
	var samples []time.Duration
	for _, part := range perWorker {
		samples = append(samples, part...)
	}
	after := snapshotPoolLock()
	maxHold := time.Duration(poolLockCounters.maxHoldNanos.Load())
	storeMaxInt64(&poolLockCounters.maxHoldNanos, maxHoldBefore)
	summary := summarizeLatencies(samples, elapsed)
	t.Logf("ClassifyCandidates 压测 | goroutine=%d 时长=%s 调用=%d 吞吐=%.0f/s | p50=%s p99=%s max=%s | 锁：持锁总计=%s 等锁总计=%s 加锁=%d 慢持锁=%d 最长持锁=%s",
		workers, elapsed.Round(time.Millisecond), summary.count, summary.throughputPerS,
		summary.p50, summary.p99, summary.max,
		time.Duration(after.holdNanos-before.holdNanos).Round(time.Millisecond),
		time.Duration(after.waitNanos-before.waitNanos).Round(time.Millisecond),
		after.acquisitions-before.acquisitions, after.slowHolds-before.slowHolds, maxHold,
	)
	return summary, time.Duration(after.holdNanos - before.holdNanos), maxHold
}

// TestClassifyCandidatesScale 在合成的 15k 账户号池上压测候选分类：64 个 goroutine 循环调用，记录分位耗时与账户池锁的持锁时间。
// 默认只跑 2 秒并只记录数据；POOL_SCALE_ASSERT=1 时按目标断言（p99 < 5ms，最长持锁 < 20ms）
func TestClassifyCandidatesScale(t *testing.T) {
	if testing.Short() {
		t.Skip("合成号池压测在 -short 下跳过")
	}
	pool, warm := scalePool(t)
	selection := AccountSelection{ModelID: scaleModel, Method: "generateContent", Pool: PoolScopeNormal}
	groups, err := pool.ClassifyCandidates(context.Background(), selection, warm)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("合成号池分类 | WarmReady=%d WarmAvailable=%d WarmBusy=%d StandbyReady=%d StandbyBusy=%d Eligible=%v 最早冷却=%v",
		len(groups.WarmReady), len(groups.WarmAvailable), len(groups.WarmBusy), len(groups.StandbyReady), len(groups.StandbyBusy),
		groups.Eligible, groups.EarliestCooldown)
	if len(groups.WarmReady) != scaleWarmAccounts {
		t.Fatalf("WarmReady = %d，期望 %d", len(groups.WarmReady), scaleWarmAccounts)
	}
	summary, _, maxHold := measureClassify(t, pool, selection, warm, scaleHarnessGoroutines(), scaleHarnessDuration())
	if os.Getenv("POOL_SCALE_ASSERT") == "1" {
		if summary.p99 >= 5*time.Millisecond {
			t.Fatalf("ClassifyCandidates p99 = %s，期望 < 5ms", summary.p99)
		}
		if maxHold >= 20*time.Millisecond {
			t.Fatalf("账户池锁最长持锁 = %s，期望 < 20ms", maxHold)
		}
	}
}

// TestPoolAdminCallsScale 记录管理页与预热路径上重活接口在 15k 账户下的单次耗时（即持锁时间上限）
func TestPoolAdminCallsScale(t *testing.T) {
	if testing.Short() {
		t.Skip("合成号池压测在 -short 下跳过")
	}
	pool, warm := scalePool(t)
	catalog := scaleCatalog()
	timeCall := func(name string, call func()) time.Duration {
		begin := time.Now()
		call()
		elapsed := time.Since(begin)
		t.Logf("%s 耗时 %s", name, elapsed)
		return elapsed
	}
	// 第一次调用包含各账户派生缓存与索引的首次构建，第二次为稳态耗时
	for _, round := range []string{"首次", "稳态"} {
		timeCall("Status("+round+")", func() { pool.Status() })
		timeCall("CatalogModels("+round+")", func() { pool.CatalogModels(catalog) })
		timeCall("EligibleModelsIn(normal, "+round+")", func() { pool.EligibleModelsIn(PoolScopeNormal, catalog) })
		timeCall("EligibleModelsIn(ultra, "+round+")", func() { pool.EligibleModelsIn(PoolScopeUltra, catalog) })
		timeCall("CanonicalModelIDIn(normal, 别名, "+round+")", func() { pool.CanonicalModelIDIn(PoolScopeNormal, scaleAliasModel) })
		timeCall("CanonicalModelIDIn(ultra, 未知模型, "+round+")", func() { pool.CanonicalModelIDIn(PoolScopeUltra, "no-such-model") })
		timeCall("BootstrapSummary("+round+")", func() { pool.BootstrapSummary() })
		timeCall("SpareAccounts(全量, "+round+")", func() {
			pool.SpareAccounts(PoolScopeNormal, map[string]struct{}{}, []string{scaleModel}, 5*time.Minute, 0)
		})
		timeCall("SpareAccounts(limit=8, "+round+")", func() {
			pool.SpareAccounts(PoolScopeNormal, map[string]struct{}{}, []string{scaleModel}, 5*time.Minute, 8)
		})
	}
	// 状态缓存过期后的重建耗时（缓存 1 秒）
	time.Sleep(statusCacheTTL + 100*time.Millisecond)
	timeCall("Status(缓存过期后重建)", func() { pool.Status() })
	standby := make([]string, 0, 3000)
	groups, err := pool.ClassifyCandidatesCached(AccountSelection{ModelID: scaleModel, Method: "generateContent", Pool: PoolScopeNormal}, warm)
	if err != nil {
		t.Fatal(err)
	}
	standby = append(standby, groups.StandbyReady...)
	timeCall(fmt.Sprintf("PreferWarmPool(%d, 首次)", len(standby)), func() { pool.PreferWarmPool(standby) })
	timeCall(fmt.Sprintf("PreferWarmPool(%d, 稳态)", len(standby)), func() { pool.PreferWarmPool(standby) })
	timeCall("PoolStateCounts", func() { pool.PoolStateCounts() })
	timeCall("UltraAccountIDs", func() { pool.UltraAccountIDs() })
	timeCall("EnabledAccountsIn(normal)", func() { pool.EnabledAccountsIn(PoolScopeNormal) })
	timeCall("attemptCandidatesIn(normal)", func() { pool.attemptCandidatesIn(PoolScopeNormal) })
	timeCall("TryAcquireFor(AllowedAccountIDs=热账户)", func() {
		lease, _, err := pool.TryAcquireFor(context.Background(), AccountSelection{
			ModelID: scaleModel, Method: "generateContent", Pool: PoolScopeNormal, AllowedAccountIDs: warm,
		})
		if err != nil {
			t.Fatal(err)
		}
		if lease != nil {
			_ = lease.Release()
		}
	})
}
