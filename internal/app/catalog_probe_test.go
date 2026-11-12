package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// probeTestCatalog 为缺少模型读目录测试的假目录服务：按账户给出权益与目录（newModel 列在 hasNew 的账户上），记下读过的账户
type probeTestCatalog struct {
	pool   *aistudio.AccountPool
	pooled *aistudio.PooledService
	tiers  map[string]aistudio.BenefitTier
	base   aistudio.ModelCatalogData
	full   aistudio.ModelCatalogData
	hasNew map[string]bool
	busy   map[string]bool
	mu     sync.Mutex
	reads  []string
}

func (catalog *probeTestCatalog) RefreshAccountModels(ctx context.Context, accountID string) ([]aistudio.Model, error) {
	models, _, err := catalog.TryRefreshAccountModels(ctx, accountID)
	return models, err
}

func (catalog *probeTestCatalog) TryRefreshAccountModels(_ context.Context, accountID string) ([]aistudio.Model, bool, error) {
	catalog.mu.Lock()
	if catalog.busy[accountID] {
		catalog.mu.Unlock()
		return nil, true, nil
	}
	catalog.reads = append(catalog.reads, accountID)
	data := catalog.base
	if catalog.hasNew[accountID] {
		data = catalog.full
	}
	catalog.mu.Unlock()
	models := data.Models()
	if err := catalog.pool.SetCatalog(accountID, catalog.tiers[accountID], models); err != nil {
		return nil, false, err
	}
	return models, false, nil
}

func (catalog *probeTestCatalog) CachedModels() []aistudio.Model {
	return catalog.pooled.CachedModels()
}

func (catalog *probeTestCatalog) readAccounts() []string {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	return append([]string(nil), catalog.reads...)
}

func (catalog *probeTestCatalog) resetReads() {
	catalog.mu.Lock()
	catalog.reads = nil
	catalog.mu.Unlock()
}

// newProbeTestEnv 建 5 个账户：free、plus 各一个，pro 一个，ultra 两个；新模型 beta 只在 ultra-2 上
func newProbeTestEnv(t *testing.T) (*syncTestEnv, *probeTestCatalog, *syncTestClock) {
	t.Helper()
	tiers := map[string]aistudio.BenefitTier{
		"free@example.com": aistudio.BenefitTierFree, "plus@example.com": aistudio.BenefitTierPlus,
		"pro@example.com": aistudio.BenefitTierPro, "ultra-1@example.com": aistudio.BenefitTierUltra,
		"ultra-2@example.com": aistudio.BenefitTierUltra,
	}
	ids := make([]string, 0, len(tiers))
	for id := range tiers {
		ids = append(ids, id)
	}
	env := newSyncTestEnv(t, ids...)
	catalog := &probeTestCatalog{
		pool: env.gen.pool, pooled: env.gen.pooled, tiers: tiers,
		base: catalogTestData(t, "alpha"), full: catalogTestData(t, "alpha", "beta"),
		hasNew: map[string]bool{"ultra-2@example.com": true}, busy: map[string]bool{},
	}
	for id, tier := range tiers {
		if err := env.gen.pool.SetCatalog(id, tier, catalog.base.Models()); err != nil {
			t.Fatal(err)
		}
	}
	env.service.catalog = catalog
	env.service.dataContext = env.ctx
	clock := newSyncTestClock()
	env.syncer.now = clock.Now
	return env, catalog, clock
}

func probeLogLines(service *trackedService) []api.AdminLog {
	service.requests.mu.Lock()
	defer service.requests.mu.Unlock()
	var lines []api.AdminLog
	for _, entry := range service.requests.logs {
		if strings.HasPrefix(entry.Message, "目录缺少请求模型") {
			lines = append(lines, entry)
		}
	}
	return lines
}

func missingSelection(model string) aistudio.AccountSelection {
	return aistudio.AccountSelection{ModelID: model, Method: "generateContent"}
}

// TestMissingModelProbeReadsHighestTierFirst 请求的模型不在任何目录里时按权益从高到低（Ultra 优先）读目录，读到即停并合并进目录；
// 方法不支持、模型已在目录里时不触发
func TestMissingModelProbeReadsHighestTierFirst(t *testing.T) {
	env, catalog, clock := newProbeTestEnv(t)
	pool := env.gen.pool
	if env.syncer.probeMissingModel("beta", aistudio.PoolScopeAll) != true {
		t.Fatal("应开始读目录")
	}
	env.syncer.waitProbe()
	reads := catalog.readAccounts()
	if len(reads) == 0 || len(reads) > 2 || reads[len(reads)-1] != "ultra-2@example.com" {
		t.Fatalf("应先读 Ultra 账户、读到 ultra-2 即停，实际读了 %v", reads)
	}
	for _, id := range reads {
		if !strings.HasPrefix(id, "ultra-") {
			t.Fatalf("Ultra 账户之前不应读低权益账户，实际 %v", reads)
		}
	}
	if !pool.ModelListedIn(aistudio.PoolScopeAll, "beta") {
		t.Fatal("读到的新模型应合并进目录")
	}
	lines := probeLogLines(env.service)
	if len(lines) != 1 || lines[0].Level != "INFO" || !strings.Contains(lines[0].Message, "模型=beta") ||
		!strings.Contains(lines[0].Message, "结果=已读到（ultra-2@example.com）") {
		t.Fatalf("日志 = %+v", lines)
	}

	// 选号失败的入口：模型已在目录里、只是方法或能力不支持时不触发。先把时钟推过同名与全局的限频间隔，挡住它的只能是目录判断
	clock.Advance(missingProbeModelInterval + time.Second)
	catalog.resetReads()
	env.service.noteMissingModel(missingSelection("beta"), fmt.Errorf("%w: 模型 beta 不支持 bidiGenerateContent", aistudio.ErrModelNotFound))
	env.syncer.waitProbe()
	if reads := catalog.readAccounts(); len(reads) != 0 {
		t.Fatalf("模型已在目录里时不应读目录，实际 %v", reads)
	}
	if lines := probeLogLines(env.service); len(lines) != 1 {
		t.Fatalf("模型已在目录里时不应写读目录日志：%+v", lines)
	}
	// 对照：同一时刻限频并不挡这个模型名
	if !env.syncer.probeMissingModel("beta", aistudio.PoolScopeAll) {
		t.Fatal("过了限频间隔应能触发（上面没有读目录是因为模型已在目录里）")
	}
	env.syncer.waitProbe()
}

// TestMissingModelProbeFromLiveAndTranscription Live 会话与转写的选号以目录中没有请求模型失败时，与生成选号一样在后台读目录
// （同一套限频），当前请求照旧返回该错误；读到后新模型在目录里
func TestMissingModelProbeFromLiveAndTranscription(t *testing.T) {
	for name, candidates := range map[string]func(*trackedService, context.Context, string) ([]string, error){
		"Live": func(service *trackedService, ctx context.Context, model string) ([]string, error) {
			return service.bidiCandidates(ctx, model, model)
		},
		"转写": func(service *trackedService, ctx context.Context, model string) ([]string, error) {
			return service.transcriptionCandidates(ctx, model)
		},
	} {
		t.Run(name, func(t *testing.T) {
			env, catalog, _ := newProbeTestEnv(t)
			manager := newAccountWorkerManager(env.gen.pool, nil, env.service.requests, "", "", time.Minute, 1, 1, 1, false)
			t.Cleanup(func() { _ = manager.Close() })
			env.service.workers = manager
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := candidates(env.service, ctx, "beta"); !errors.Is(err, aistudio.ErrModelNotFound) {
				t.Fatalf("本次请求应照旧按目录中没有这个模型返回，实际 %v", err)
			}
			env.syncer.waitProbe()
			if reads := catalog.readAccounts(); len(reads) == 0 {
				t.Fatal("应在后台读目录")
			}
			if !env.gen.pool.ModelListedIn(aistudio.PoolScopeAll, "beta") {
				t.Fatal("读到之后新模型应在目录里，客户端重试可用")
			}
			if lines := probeLogLines(env.service); len(lines) != 1 || !strings.Contains(lines[0].Message, "模型=beta") {
				t.Fatalf("应写一行读目录日志：%+v", lines)
			}
		})
	}
}

// TestMissingModelProbeRateLimits 同一模型名 10 分钟一次（不分大小写）、全局 1 分钟一次；上游 429 暂停期间不读；
// 一轮最多读 3 个账户，忙碌的跳过
func TestMissingModelProbeRateLimits(t *testing.T) {
	env, catalog, clock := newProbeTestEnv(t)
	catalog.busy["ultra-1@example.com"] = true
	if !env.syncer.probeMissingModel("gamma", aistudio.PoolScopeAll) {
		t.Fatal("第一次应读目录")
	}
	env.syncer.waitProbe()
	reads := catalog.readAccounts()
	if len(reads) != 3 || reads[0] != "ultra-2@example.com" || reads[1] != "pro@example.com" {
		t.Fatalf("忙碌的 ultra-1 跳过，最多读 3 个账户（ultra-2、pro、再下一档），实际 %v", reads)
	}
	lines := probeLogLines(env.service)
	if len(lines) != 1 || !strings.Contains(lines[0].Message, "结果=仍没有这个模型") || !strings.Contains(lines[0].Message, "跳过=1") {
		t.Fatalf("日志 = %+v", lines)
	}

	catalog.resetReads()
	if env.syncer.probeMissingModel("delta", aistudio.PoolScopeAll) {
		t.Fatal("1 分钟内其他模型名也不应触发（全局限频）")
	}
	clock.Advance(time.Minute)
	if env.syncer.probeMissingModel("GAMMA", aistudio.PoolScopeAll) {
		t.Fatal("同一模型名（不分大小写）10 分钟内不应再触发")
	}
	env.syncer.mu.Lock()
	env.syncer.pausedUntil = clock.Now().Add(time.Minute)
	env.syncer.mu.Unlock()
	if env.syncer.probeMissingModel("delta", aistudio.PoolScopeAll) {
		t.Fatal("上游 429 暂停期间不应读目录")
	}
	clock.Advance(2 * time.Minute)
	if !env.syncer.probeMissingModel("delta", aistudio.PoolScopeAll) {
		t.Fatal("暂停结束、过了全局间隔后其他模型名应能触发（暂停期间被挡的不算触发过）")
	}
	env.syncer.waitProbe()
	clock.Advance(10 * time.Minute)
	if !env.syncer.probeMissingModel("gamma", aistudio.PoolScopeAll) {
		t.Fatal("过了 10 分钟同一模型名应能再触发")
	}
	env.syncer.waitProbe()
	env.syncer.probe.mu.Lock()
	recent := len(env.syncer.probe.recent)
	env.syncer.probe.mu.Unlock()
	if recent != 1 {
		t.Fatalf("过期的模型名记录应清掉，实际 %d 条", recent)
	}
	// 读目录照常经调度器：结束后没有残留的执行中计数
	env.syncer.mu.Lock()
	running := env.syncer.running
	env.syncer.mu.Unlock()
	if running != 0 {
		t.Fatalf("调度器执行中计数 = %d，期望 0", running)
	}
}

// TestMissingModelProbeFromLeaseSelection 选号以目录中没有请求模型失败时触发读目录，当前请求照旧返回该错误
func TestMissingModelProbeFromLeaseSelection(t *testing.T) {
	env, catalog, _ := newProbeTestEnv(t)
	manager := newAccountWorkerManager(env.gen.pool, nil, env.service.requests, "", "", time.Minute, 1, 1, 1, false)
	t.Cleanup(func() { _ = manager.Close() })
	env.service.workers = manager
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := env.service.acquireWarmLease(ctx, missingSelection("beta"))
	if err == nil || !strings.Contains(err.Error(), aistudio.ErrModelNotFound.Error()) {
		t.Fatalf("本次请求应照旧按目录中没有这个模型返回，实际 %v", err)
	}
	env.syncer.waitProbe()
	if reads := catalog.readAccounts(); len(reads) == 0 {
		t.Fatal("应在后台读目录")
	}
	if !env.gen.pool.ModelListedIn(aistudio.PoolScopeAll, "beta") {
		t.Fatal("读到之后新模型应在目录里，客户端重试可用")
	}
}
