package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

const (
	proPartitionA = "pro-a@example.com"
	proPartitionB = "pro-b@example.com"
)

// proPartitionManager 在 partitionTestManager 之上加 Pro 账户（proIDs，权益为 Pro）并开启 PRO_POOL，Pro 分区常驻与峰值为 proWarm/proMax
func proPartitionManager(
	t *testing.T, normalIDs []string, ultraIDs []string, proIDs []string, normalWarm, normalMax, proWarm, proMax int,
) (*accountWorkerManager, *aistudio.AccountPool, *requestRegistry) {
	t.Helper()
	manager, pool, requests := partitionTestManager(t, append(slices.Clone(normalIDs), proIDs...), ultraIDs, normalWarm, normalMax, 1, 1)
	for _, accountID := range proIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierPro, []aistudio.Model{{
			ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetPoolRules(false, true)
	manager.setProCapacity(proWarm, proMax)
	return manager, pool, requests
}

// TestProPartitionLimits Pro 分区按 PRO_WARM_WORKER_LIMIT、PRO_MAX_ACTIVE_WORKERS 单独计数：其他分区已满时 Pro 账户照样能启动；
// Pro 分区已满且 Pro Worker 正忙时 Pro 账户不能启动，也不淘汰普通或 Ultra 的空闲 Worker；空闲时只在 Pro 分区内替换。
// 不是分区的取值没有容量（不按普通分区放行）
func TestProPartitionLimits(t *testing.T) {
	manager, pool, _ := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA, proPartitionB}, 1, 1, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionUltraA, proPartitionA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatalf("%s 启动失败（各分区分开计数）: %v", accountID, err)
		}
	}
	if got := manager.warmAccountIDsIn(aistudio.PoolScopePro); !slices.Equal(got, []string{proPartitionA}) {
		t.Fatalf("Pro 分区驻留 = %v", got)
	}
	release := holdLease(t, pool, proPartitionA)
	if _, err := manager.promote(ctx, proPartitionB, testRuntimeModel); !errors.Is(err, errAccountWorkerCapacity) {
		t.Fatalf("Pro 分区已满且 Pro Worker 正忙：err = %v，期望 Worker 槽位已满", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, proPartitionA, partitionUltraA}) {
		t.Fatalf("Pro 账户启动不应淘汰其他分区的 Worker，驻留 = %v", got)
	}
	release()
	if _, err := manager.promote(ctx, proPartitionB, testRuntimeModel); err != nil {
		t.Fatalf("Pro 分区内替换失败: %v", err)
	}
	if got := sortedWarm(manager); !slices.Equal(got, []string{partitionNormalA, proPartitionB, partitionUltraA}) {
		t.Fatalf("Pro 账户启动只应淘汰 Pro Worker，驻留 = %v", got)
	}
	if occupied := manager.occupiedWorkersIn(aistudio.PoolScopePro, pool.PartitionMembers()); occupied.slots != 1 {
		t.Fatalf("Pro 分区占用 %d 个槽位，期望 1", occupied.slots)
	}
	if manager.warmTargetFor(aistudio.PoolScopePro) != 1 || manager.maxActiveFor(aistudio.PoolScopePro) != 1 {
		t.Fatal("Pro 分区的容量应取 PRO_* 设置")
	}
	for _, scope := range []aistudio.PoolScope{aistudio.PoolScopeAll, aistudio.PoolScopeNormalUltra, aistudio.PoolScopeCount} {
		if manager.warmTargetFor(scope) != 0 || manager.maxActiveFor(scope) != 0 {
			t.Fatalf("号池 %v 不是分区，容量应为 0", scope)
		}
	}
	if partitionLabel(aistudio.PoolScopePro) != "Pro 分区" || workerLogName(aistudio.PoolScopePro) != "Pro WAA Worker" ||
		workerSlotName(aistudio.PoolScopePro) != "Pro Worker" || balancedPartitionName(aistudio.PoolScopePro) != "Pro" ||
		workerLogName(aistudio.PoolScopeUltra) != "Ultra WAA Worker" || workerLogName(aistudio.PoolScopeNormal) != "WAA Worker" ||
		partitionLabel(aistudio.PoolScopeAll) != "普通分区" {
		t.Fatal("分区名称表不符")
	}
	// 预热补齐目标：没有可预热的 Pro 账户时 Pro 分区为 0；可预热的都是付费账户时普通分区为 0
	if got := manager.fillTargetFor(aistudio.PoolScopePro, aistudio.BootstrapSummary{Available: 3, UltraAvailable: 1}); got != 0 {
		t.Fatalf("没有 Pro 账户时 Pro 分区补齐目标 = %d", got)
	}
	if got := manager.fillTargetFor(aistudio.PoolScopePro, aistudio.BootstrapSummary{Available: 3, ProAvailable: 2}); got != 1 {
		t.Fatalf("有 Pro 账户时 Pro 分区补齐目标 = %d，期望 PRO_WARM_WORKER_LIMIT", got)
	}
	if got := manager.fillTargetFor(aistudio.PoolScopeNormal, aistudio.BootstrapSummary{Available: 3, UltraAvailable: 1, ProAvailable: 2}); got != 0 {
		t.Fatalf("可预热的都是付费账户时普通分区补齐目标 = %d", got)
	}
	if got := manager.fillTargetFor(aistudio.PoolScopeNormalUltra, aistudio.BootstrapSummary{Available: 3}); got != 0 {
		t.Fatalf("不是分区的补齐目标 = %d", got)
	}
}

// TestPrewarmProPartition 预热按三个分区各自补齐：Pro 分区按 PRO_WARM_WORKER_LIMIT 从 Pro 账户补齐，普通分区不计 Pro 账户；
// PRO_POOL 关闭时 Pro 账户回到普通分区，空的 Pro 分区不预热、不写预热等待
func TestPrewarmProPartition(t *testing.T) {
	for _, proPool := range []bool{true, false} {
		t.Run(fmt.Sprintf("PRO_POOL=%t", proPool), func(t *testing.T) {
			manager, pool, _ := proPartitionManager(t, []string{partitionNormalA, partitionNormalB}, nil,
				[]string{proPartitionA, proPartitionB}, 1, 3, 2, 3)
			if !proPool {
				pool.SetProPool(false)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := <-manager.StartPrewarm(ctx); err != nil {
				t.Fatalf("预热失败: %v", err)
			}
			manager.waitPrewarm()
			wantPro, wantTarget := 2, 3
			if !proPool {
				wantPro, wantTarget = 0, 1
			}
			if got := len(manager.warmAccountIDsIn(aistudio.PoolScopePro)); got != wantPro {
				t.Fatalf("Pro 分区驻留 %d 个，期望 %d", got, wantPro)
			}
			if got := len(manager.warmAccountIDsIn(aistudio.PoolScopeNormal)); got != 1 {
				t.Fatalf("普通分区驻留 %d 个，期望 1", got)
			}
			if got := manager.PrewarmTarget(); got != wantTarget || manager.prewarmNeeded() {
				t.Fatalf("预热目标 = %d，期望 %d，且已达标", got, wantTarget)
			}
			if reason := manager.prewarmState().Reason; reason != "" {
				t.Fatalf("预热不应记录等待原因: %q", reason)
			}
		})
	}
}

// TestProxyPoolCapacityIncludesPro 浏览器上限只在 PRO_POOL=true 且有 Pro 账户时计入 Pro 峰值与排空上限
func TestProxyPoolCapacityIncludesPro(t *testing.T) {
	cfg := config.Default()
	cfg.MaxActiveWorkers, cfg.UltraMaxActiveWorkers, cfg.ProMaxActiveWorkers, cfg.WarmStartupConcurrency = 10, 5, 7, 2
	base := 10 + drainLimitFor(10, 2) + 5 + drainLimitFor(5, 2)
	pro := 7 + drainLimitFor(7, 2)
	if got := proxyPoolCapacity(cfg, true); got != base+pro {
		t.Fatalf("开启且有 Pro 账户：容量 = %d，期望 %d", got, base+pro)
	}
	if got := proxyPoolCapacity(cfg, false); got != base {
		t.Fatalf("没有 Pro 账户：容量 = %d，期望 %d", got, base)
	}
	cfg.ProPool = false
	if got := proxyPoolCapacity(cfg, true); got != base {
		t.Fatalf("PRO_POOL=false：容量 = %d，期望 %d", got, base)
	}
	if hasProAccounts([]*aistudio.Account{{ID: "f", BenefitTier: aistudio.BenefitTierFree}}) ||
		!hasProAccounts([]*aistudio.Account{nil, {ID: "p", BenefitTier: aistudio.BenefitTierPro}}) {
		t.Fatal("hasProAccounts 判断不符")
	}
}

// TestPrimaryPolicyAndSupplyPro /pro 沿用 Ultra 主用模型（额度快速失败）；普通号池与 Ultra 号池（非独占的普通路径）先查普通列表；
// Worker 侧 Pro 分区用 Ultra 列表；主用模型供给在 PRO_POOL=true 时多统计 Pro 分区，不统计权益规则不让 Pro 用的模型
func TestPrimaryPolicyAndSupplyPro(t *testing.T) {
	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA}, 1, 2, 1, 2)
	service := &trackedService{pool: pool, requests: requests, workers: manager}
	service.setPrimaryModelSettings([]string{"normal-model"}, 3*time.Second, []string{testRuntimeModel, "gemini-4-carbon"}, 7*time.Second)
	for _, test := range []struct {
		pool     aistudio.PoolScope
		model    string
		listed   bool
		wait     time.Duration
		failFast bool
	}{
		{aistudio.PoolScopePro, testRuntimeModel, true, 7 * time.Second, true},
		{aistudio.PoolScopePro, "normal-model", false, 7 * time.Second, true},
		{aistudio.PoolScopeUltra, testRuntimeModel, true, 7 * time.Second, true},
		{aistudio.PoolScopeNormal, testRuntimeModel, false, 3 * time.Second, false},
		{aistudio.PoolScopeNormalUltra, "normal-model", true, 3 * time.Second, false},
		{aistudio.PoolScopeNormalUltra, testRuntimeModel, true, 7 * time.Second, true},
	} {
		policy, listed := service.primaryPolicyFor(test.pool, test.model)
		if listed != test.listed || policy.queueWait != test.wait || policy.quotaFailFast != test.failFast {
			t.Fatalf("%v %s：规则 %+v（列表=%t），期望 等待 %s 快速失败 %t（列表=%t）", test.pool, test.model, policy, listed, test.wait, test.failFast, test.listed)
		}
	}
	normal, ultra := manager.primaryModelLists()
	if !slices.Equal(manager.primaryModelListFor(aistudio.PoolScopePro), ultra) || !slices.Equal(manager.primaryModelListFor(aistudio.PoolScopeNormal), normal) {
		t.Fatal("Pro 分区应用 Ultra 主用模型")
	}
	if got := manager.deadSlotModelsForScope(aistudio.PoolScopePro); !slices.Equal(got, manager.deadSlotModelsFor(aistudio.PoolScopePro)) ||
		slices.Contains(got, "normal-model") {
		t.Fatalf("Pro 号池的死槽位模型 = %v", got)
	}
	if got := manager.deadSlotModelsForScope(aistudio.PoolScopeNormalUltra); !slices.Contains(got, "normal-model") || !slices.Contains(got, testRuntimeModel) {
		t.Fatalf("普通号池与 Ultra 号池的死槽位模型 = %v，应为两份列表的并集", got)
	}
	pool.SetModelTierRules([]string{"gemini-4-carbon"}, nil)
	if got := manager.proPrimaryModels(ultra); !slices.Equal(got, []string{testRuntimeModel}) {
		t.Fatalf("Pro 分区统计的主用模型 = %v，不应含只限 Ultra 的模型", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := manager.promote(ctx, proPartitionA, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if !manager.refreshPrimarySupply(time.Now()) {
		t.Fatal("应统计主用模型供给")
	}
	var proEntries []api.AdminPrimarySupply
	for _, entry := range manager.primarySupplyStatus() {
		if entry.Pool == "pro" {
			proEntries = append(proEntries, entry)
		}
	}
	if len(proEntries) != 1 || proEntries[0].Model != testRuntimeModel || proEntries[0].Warm != 1 {
		t.Fatalf("Pro 分区的供给 = %+v，期望一条、驻留 1", proEntries)
	}
	if supplyPoolLabel(aistudio.PoolScopePro) != "pro" || supplyPoolLabel(aistudio.PoolScopeNormalUltra) != "" {
		t.Fatal("供给条目的号池标记不符")
	}
	if field := manager.supplyCoolingField(aistudio.PoolScopePro, testRuntimeModel); field != " | 驻留冷却=0" {
		t.Fatalf("Pro 号池的驻留冷却字段 = %q", field)
	}
	pool.SetProPool(false)
	if got := manager.proPrimaryModels(ultra); len(got) != 0 {
		t.Fatalf("PRO_POOL=false 时不统计 Pro 分区：%v", got)
	}
	manager.refreshPrimarySupply(time.Now())
	for _, entry := range manager.primarySupplyStatus() {
		if entry.Pool == "pro" {
			t.Fatalf("PRO_POOL=false 后（规则版本变化）仍有 Pro 供给：%+v", entry)
		}
	}
}

// TestModelFallbackScopeWithPro MODEL_FALLBACK_SCOPE=ultra 只管 /ultra（不含 /pro），all 包括 /pro，off 都不生效
func TestModelFallbackScopeWithPro(t *testing.T) {
	for _, test := range []struct {
		scope string
		want  map[aistudio.PoolScope]bool
	}{
		{config.ModelFallbackScopeUltra, map[aistudio.PoolScope]bool{aistudio.PoolScopeUltra: true}},
		{config.ModelFallbackScopeAll, map[aistudio.PoolScope]bool{
			aistudio.PoolScopeAll: true, aistudio.PoolScopeNormal: true, aistudio.PoolScopeUltra: true, aistudio.PoolScopePro: true, aistudio.PoolScopeNormalUltra: true,
		}},
		{config.ModelFallbackScopeOff, map[aistudio.PoolScope]bool{}},
	} {
		rules, _ := config.ParseModelFallbacks(config.DefaultModelFallbacks)
		settings := &modelFallbackSettings{rules: rules, scope: test.scope}
		for scope := range aistudio.PoolScopeCount {
			if got := settings.applies(scope); got != test.want[scope] {
				t.Fatalf("MODEL_FALLBACK_SCOPE=%s 号池 %v：applies = %t", test.scope, scope, got)
			}
		}
	}
}

// TestPoolKeyedHelpersPro 按号池分开的键与标记：降级记录、403 号池过滤、请求日志的号池与协议归类
func TestPoolKeyedHelpersPro(t *testing.T) {
	if downgradeMemoryKey(aistudio.PoolScopePro, "h") != "h|pro" || downgradeMemoryKey(aistudio.PoolScopeUltra, "h") != "h|ultra" ||
		downgradeMemoryKey(aistudio.PoolScopeNormalUltra, "h") != "h" || downgradeMemoryKey(aistudio.PoolScopeNormal, "h") != "h" {
		t.Fatal("降级记录的键不符")
	}
	_, pool, _ := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA}, 1, 1, 1, 1)
	for scope, want := range map[aistudio.PoolScope][]string{
		aistudio.PoolScopePro:         {proPartitionA},
		aistudio.PoolScopeUltra:       {partitionUltraA},
		aistudio.PoolScopeNormal:      {partitionNormalA},
		aistudio.PoolScopeNormalUltra: {partitionNormalA, partitionUltraA},
	} {
		member := poolMembership(pool, scope)
		for _, accountID := range []string{partitionNormalA, partitionUltraA, proPartitionA} {
			if member(accountID) != slices.Contains(want, accountID) {
				t.Fatalf("号池 %v 的 403 过滤对 %s 判断不符", scope, accountID)
			}
		}
	}
	if poolMembership(pool, aistudio.PoolScopeAll) != nil {
		t.Fatal("不限号池不过滤")
	}
	for scope, want := range map[aistudio.PoolScope]string{
		aistudio.PoolScopePro: "pro", aistudio.PoolScopeUltra: "ultra", aistudio.PoolScopeNormal: "", aistudio.PoolScopeNormalUltra: "", aistudio.PoolScopeAll: "",
	} {
		if got := requestPoolLabel(aistudio.ContextWithPoolScope(context.Background(), scope)); got != want {
			t.Fatalf("号池 %v 的请求日志标记 = %q，期望 %q", scope, got, want)
		}
	}
	for path, want := range map[string]string{
		"/pro/v1/chat/completions":             "openai-chat",
		"/pro/v1/messages":                     "anthropic",
		"/pro/v1beta/models/m:generateContent": "gemini",
		"/trace/pro/v1/responses":              "openai-responses",
		"/pro/v1/messages/count_tokens":        "count_tokens",
		"/profile/v1/chat/completions":         "other",
	} {
		if got := requestProtocol(path); got != want {
			t.Fatalf("requestProtocol(%q) = %q，期望 %q", path, got, want)
		}
	}
}

// TestBalancedOverviewPro 平均调用概况按三个分区：Pro 的分布单独列出，不覆盖普通分区；Pro 号池为空时不列出、概况日志与原先相同
func TestBalancedOverviewPro(t *testing.T) {
	_, pool, requests := proPartitionManager(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA}, []string{proPartitionA}, 1, 1, 1, 1)
	service := &trackedService{pool: pool, requests: requests}
	snapshot := aistudio.QuotaUsageSnapshot{Day: "2026-11-21", Counts: map[string]uint32{partitionNormalA: 1, partitionNormalB: 3, proPartitionA: 9}}
	status := service.balancedOverview(snapshot, balancedRotationStats{})
	if status.Normal.Accounts != 2 || status.Normal.Max != 3 || status.Ultra.Accounts != 1 || status.Pro == nil ||
		status.Pro.Accounts != 1 || status.Pro.Max != 9 {
		t.Fatalf("概况 普通=%+v Ultra=%+v Pro=%+v", status.Normal, status.Ultra, status.Pro)
	}
	if line := balancedSummaryLine(status); !strings.Contains(line, "| Pro 可调度=1 今日用过=1") {
		t.Fatalf("概况日志缺少 Pro：%s", line)
	}
	pool.SetProPool(false)
	status = service.balancedOverview(snapshot, balancedRotationStats{})
	if status.Pro != nil || status.Normal.Accounts != 3 {
		t.Fatalf("PRO_POOL=false：Pro=%+v 普通=%+v", status.Pro, status.Normal)
	}
	if line := balancedSummaryLine(status); strings.Contains(line, "Pro") {
		t.Fatalf("Pro 号池为空时概况日志不应有 Pro：%s", line)
	}
}

// TestAdminStatusProFields 状态接口：pro_accounts 与 pro_workers 按 Pro 分区，workers（普通分区）不含 Pro Worker
func TestAdminStatusProFields(t *testing.T) {
	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA, proPartitionB}, 1, 2, 1, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionUltraA, proPartitionA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	service.proPoolConfigured.Store(true)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.ProAccounts.Total != 2 || status.ProWorkers.Warm != 1 || status.ProWorkers.Max != 3 || status.ProWorkers.WarmLimit != 1 ||
		status.Workers.Warm != 1 || status.UltraWorkers.Warm != 1 || status.UltraAccounts.Total != 1 || status.ProPoolNotice != nil {
		t.Fatalf("状态 Pro 账户=%+v Pro Worker=%+v 普通 Worker=%d Ultra Worker=%d 提示=%v",
			status.ProAccounts, status.ProWorkers, status.Workers.Warm, status.UltraWorkers.Warm, status.ProPoolNotice)
	}
	service.proPoolConfigured.Store(false)
	if status, _ := admin.Status(ctx); status.ProPoolNotice == nil || status.ProPoolNotice.ProAccounts != 2 {
		t.Fatalf("没有写 PRO_POOL 时应给出升级提示：%+v", status.ProPoolNotice)
	}
}

// TestProPoolNotice 升级提示：只在 PRO_POOL 没有写过、开关开启且有 Pro 账户时给出；建议值按 Pro 可用账户占它与来源分区（升级前 Pro 账户所在的
// 分区：ULTRA_INCLUDE_PRO 写为 true 时为 Ultra 分区，否则为普通分区）可用账户之和的比例分来源分区的常驻与峰值数（Pro 不少于默认值），
// 来源分区相应减少（不少于它的默认值、普通常驻不少于 WARM_STARTUP_CONCURRENCY），另一个分区不变；
// 运行日志变成需要提示时写一次，Worker 设置变化时再写，不再需要后清掉记录
func TestProPoolNotice(t *testing.T) {
	capacity := func(warm, maxActive, ultraWarm, ultraMax, startup int) proNoticeCapacity {
		return proNoticeCapacity{warm: warm, maxActive: maxActive, ultraWarm: ultraWarm, ultraMax: ultraMax, proWarm: 2, proMax: 5, startup: startup}
	}
	notice := newProPoolNotice(30, aistudio.BootstrapSummary{Available: 140, UltraAvailable: 10, ProAvailable: 30}, false, capacity(130, 140, 20, 40, 2))
	// 占比 30 / (100 + 30)：常驻 130 × 0.23 ≈ 30，峰值 140 × 0.23 ≈ 32；Ultra 分区不变
	if notice.Source != "normal" || notice.ProAvailable != 30 || notice.NormalAvailable != 100 || notice.UltraAvailable != 10 ||
		notice.SuggestedProWarmWorkerLimit != 30 || notice.SuggestedProMaxActiveWorkers != 32 || notice.SuggestedWarmWorkerLimit != 100 ||
		notice.SuggestedMaxActiveWorkers != 108 || notice.SuggestedUltraWarmWorkerLimit != 20 || notice.SuggestedUltraMaxActiveWorkers != 40 {
		t.Fatalf("建议值 = %+v", notice)
	}
	small := newProPoolNotice(1, aistudio.BootstrapSummary{Available: 101, ProAvailable: 1}, false, capacity(5, 10, 2, 5, 2))
	if small.SuggestedProWarmWorkerLimit != 2 || small.SuggestedProMaxActiveWorkers != 5 || small.SuggestedWarmWorkerLimit != 5 ||
		small.SuggestedMaxActiveWorkers != 10 {
		t.Fatalf("Pro 占比很小时建议值不少于默认、普通不变：%+v", small)
	}
	if none := newProPoolNotice(1, aistudio.BootstrapSummary{}, false, capacity(5, 10, 2, 5, 2)); none.SuggestedProWarmWorkerLimit != 2 || none.SuggestedWarmWorkerLimit != 5 {
		t.Fatalf("没有可用账户时按默认：%+v", none)
	}
	// Pro 占比很大：普通常驻不少于 WARM_STARTUP_CONCURRENCY（照建议保存能通过“1 到 WARM_WORKER_LIMIT”的校验），峰值不少于常驻与默认值
	dense := newProPoolNotice(2000, aistudio.BootstrapSummary{Available: 2100, ProAvailable: 2000}, false, capacity(130, 140, 2, 5, 16))
	if dense.SuggestedProWarmWorkerLimit != 124 || dense.SuggestedProMaxActiveWorkers != 133 || dense.SuggestedWarmWorkerLimit != 16 ||
		dense.SuggestedMaxActiveWorkers != 16 {
		t.Fatalf("Pro 占比很大时的建议值 = %+v", dense)
	}
	// Pro 占比很大、启动并发小：普通分区不少于它的默认值（常驻 5、峰值 10），不按份额减到 1/2
	floored := newProPoolNotice(90, aistudio.BootstrapSummary{Available: 100, ProAvailable: 90}, false, capacity(10, 20, 2, 5, 2))
	if floored.SuggestedProWarmWorkerLimit != 9 || floored.SuggestedProMaxActiveWorkers != 18 || floored.SuggestedWarmWorkerLimit != 5 ||
		floored.SuggestedMaxActiveWorkers != 10 {
		t.Fatalf("普通分区的建议值应不少于默认值：%+v", floored)
	}
	// 当前值已小于默认值时为当前值，不建议调大
	below := newProPoolNotice(90, aistudio.BootstrapSummary{Available: 100, ProAvailable: 90}, false, capacity(3, 6, 2, 5, 1))
	if below.SuggestedWarmWorkerLimit != 3 || below.SuggestedMaxActiveWorkers != 6 {
		t.Fatalf("当前值小于默认值时普通分区的建议值应为当前值：%+v", below)
	}
	cfg := config.Default()
	cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency = dense.SuggestedWarmWorkerLimit, dense.SuggestedMaxActiveWorkers, 16
	cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers = dense.SuggestedProWarmWorkerLimit, dense.SuggestedProMaxActiveWorkers
	if err := cfg.Validate(); err != nil {
		t.Fatalf("照建议值保存应能通过校验：%v", err)
	}
	// 升级前 ULTRA_INCLUDE_PRO=true：Pro 账户原先在 Ultra 分区、用 ULTRA_* 的容量，按 Pro / (Pro + Ultra) 分 ULTRA_*，普通分区不变；
	// Ultra 分区不少于默认值
	ultra := newProPoolNotice(2000, aistudio.BootstrapSummary{Available: 2030, UltraAvailable: 20, ProAvailable: 2000}, true, capacity(5, 10, 20, 40, 2))
	if ultra.Source != "ultra" || ultra.SuggestedProWarmWorkerLimit != 20 || ultra.SuggestedProMaxActiveWorkers != 40 ||
		ultra.SuggestedUltraWarmWorkerLimit != 2 || ultra.SuggestedUltraMaxActiveWorkers != 5 || ultra.SuggestedWarmWorkerLimit != 5 ||
		ultra.SuggestedMaxActiveWorkers != 10 {
		t.Fatalf("ULTRA_INCLUDE_PRO=true 的建议值 = %+v", ultra)
	}
	if line := proPoolNoticeLine(ultra); !strings.Contains(line, "ULTRA_INCLUDE_PRO=true，升级前 Pro 账户在 Ultra 分区") ||
		!strings.Contains(line, "Ultra 分区可相应减为 ULTRA_WARM_WORKER_LIMIT=2、ULTRA_MAX_ACTIVE_WORKERS=5") || strings.Contains(line, "普通分区可相应减为") {
		t.Fatalf("Ultra 来源的提示 = %s", line)
	}
	half := newProPoolNotice(10, aistudio.BootstrapSummary{Available: 30, UltraAvailable: 10, ProAvailable: 10}, true, capacity(5, 10, 20, 40, 2))
	if half.SuggestedProWarmWorkerLimit != 10 || half.SuggestedProMaxActiveWorkers != 20 || half.SuggestedUltraWarmWorkerLimit != 10 ||
		half.SuggestedUltraMaxActiveWorkers != 20 {
		t.Fatalf("Pro 与 Ultra 各半时的建议值 = %+v", half)
	}

	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, nil, []string{proPartitionA}, 1, 2, 2, 5)
	service := &trackedService{pool: pool, requests: requests, workers: manager}
	warns := func() int {
		return countLogs(requests, "PRO_POOL 默认开启：1 个 Pro 账户只服务 /pro 前缀")
	}
	service.checkProPoolNotice()
	service.checkProPoolNotice()
	if warns() != 1 || service.proPoolNotice(1) == nil {
		t.Fatalf("需要提示时写一次 WARN，实际 %d 次", warns())
	}
	if !hasLogContaining(requests, "建议 PRO_WARM_WORKER_LIMIT=2、PRO_MAX_ACTIVE_WORKERS=5", "要保持升级前的行为请设置 PRO_POOL=false") {
		t.Fatalf("提示缺少建议值：%v", serviceLogMessages(requests))
	}
	manager.setProCapacity(3, 6)
	service.checkProPoolNotice()
	if warns() != 2 {
		t.Fatalf("Pro Worker 设置变化后应再写一次，实际 %d 次", warns())
	}
	service.proPoolConfigured.Store(true)
	service.checkProPoolNotice()
	if service.proPoolNotice(1) != nil {
		t.Fatal("写了 PRO_POOL 后不应再提示")
	}
	service.proPoolConfigured.Store(false)
	service.checkProPoolNotice()
	if warns() != 3 {
		t.Fatalf("提示消失后再出现应重新写，实际 %d 次", warns())
	}
	// 保存的 ULTRA_INCLUDE_PRO 为 true（PRO_POOL=true 时不生效）：建议从 Ultra 分区分出容量，提示内容变了再写一次
	pool.SetPoolRules(true, true)
	if notice := service.proPoolNotice(1); notice == nil || notice.Source != "ultra" {
		t.Fatalf("ULTRA_INCLUDE_PRO=true 时来源分区应为 Ultra：%+v", notice)
	}
	service.checkProPoolNotice()
	if warns() != 4 || !hasLogContaining(requests, "Ultra 分区可相应减为 ULTRA_WARM_WORKER_LIMIT=") {
		t.Fatalf("来源分区变化后应再写一次，实际 %d 次", warns())
	}
	pool.SetPoolRules(false, true)
	pool.SetProPool(false)
	if service.proPoolNotice(1) != nil {
		t.Fatal("PRO_POOL=false 时不提示")
	}
	pool.SetProPool(true)
	if service.proPoolNotice(0) != nil {
		t.Fatal("没有 Pro 账户时不提示")
	}
}

// TestMissingModelHintSkipsProbe 普通路径请求只有 Pro 号池能服务的模型：错误带上改用 /pro 的提示、对外仍是模型不存在，
// 不为它在后台读目录；没有任何号池列出的模型照旧读目录
func TestMissingModelHintSkipsProbe(t *testing.T) {
	env, catalog, _ := newProbeTestEnv(t)
	pool := env.gen.pool
	if err := pool.SetCatalog("pro@example.com", aistudio.BenefitTierPro, catalogTestData(t, "alpha", "gamma").Models()); err != nil {
		t.Fatal(err)
	}
	pool.SetPoolRules(false, true)
	normal := aistudio.AccountSelection{ModelID: "gamma", Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	err := env.service.missingModelError(normal, fmt.Errorf("%w: gamma", aistudio.ErrModelNotFound))
	var hinted *aistudio.ModelInOtherPoolError
	if !errors.As(err, &hinted) || !errors.Is(err, aistudio.ErrModelNotFound) || !slices.Equal(hinted.Pools, []aistudio.PoolScope{aistudio.PoolScopePro}) ||
		hinted.PublicHint() != "This model is available through the /pro endpoints." {
		t.Fatalf("普通路径请求 gamma：err = %v，期望带 /pro 提示", err)
	}
	env.syncer.waitProbe()
	if reads := catalog.readAccounts(); len(reads) != 0 || len(probeLogLines(env.service)) != 0 {
		t.Fatalf("其他号池已列出的模型不应读目录：%v", reads)
	}
	// /pro 自己、不限号池与 Ultra 号池不加提示
	for _, scope := range []aistudio.PoolScope{aistudio.PoolScopePro, aistudio.PoolScopeAll, aistudio.PoolScopeUltra} {
		selection := normal
		selection.Pool = scope
		if err := env.service.otherPoolHint(selection, aistudio.ErrModelNotFound); err != nil {
			t.Fatalf("号池 %v 不应加提示：%v", scope, err)
		}
	}
	// 其他错误（号池暂时不可调度）不加提示
	if err := env.service.otherPoolHint(normal, aistudio.PoolNotReady("测试", nil)); err != nil {
		t.Fatalf("非模型不存在的错误不应加提示：%v", err)
	}
	err = env.service.missingModelError(aistudio.AccountSelection{ModelID: "zeta", Method: "generateContent", Pool: aistudio.PoolScopeNormal},
		fmt.Errorf("%w: zeta", aistudio.ErrModelNotFound))
	if errors.As(err, &hinted) {
		t.Fatalf("没有号池列出的模型不应加提示：%v", err)
	}
	env.syncer.waitProbe()
	if lines := probeLogLines(env.service); len(lines) != 1 || !strings.Contains(lines[0].Message, "模型=zeta") {
		t.Fatalf("没有号池列出的模型应照旧读目录：%+v", lines)
	}
}

// TestMissingModelHintVariants 改用入口提示的几种写法与号池：带思考等级后缀或 long- 前缀的模型名（默认地址解析不了、原样进入选号）按去掉后缀的
// 名称找到 /pro，不读目录；ULTRA_EXCLUSIVE=false 时普通路径为普通号池与 Ultra 号池，只有 Pro 能服务的模型同样提示 /pro、不读目录；
// 请求的方法 Pro 号池同样不支持时（用对话模型发 Live 请求）不提示，照旧交给读目录的判断
func TestMissingModelHintVariants(t *testing.T) {
	env, catalog, _ := newProbeTestEnv(t)
	pool := env.gen.pool
	if err := pool.SetCatalog("pro@example.com", aistudio.BenefitTierPro, catalogTestData(t, "alpha", "gamma").Models()); err != nil {
		t.Fatal(err)
	}
	pool.SetPoolRules(false, true)
	var hinted *aistudio.ModelInOtherPoolError
	for _, selection := range []aistudio.AccountSelection{
		{ModelID: "gamma-high", Method: "generateContent", Pool: aistudio.PoolScopeNormal},
		{ModelID: "long-gamma-low-nothinking", Method: "generateContent", Pool: aistudio.PoolScopeNormal},
		{ModelID: "gamma", Method: "generateContent", Pool: aistudio.PoolScopeNormalUltra},
	} {
		err := env.service.missingModelError(selection, fmt.Errorf("%w: %s", aistudio.ErrModelNotFound, selection.ModelID))
		if !errors.As(err, &hinted) || !errors.Is(err, aistudio.ErrModelNotFound) || !slices.Equal(hinted.Pools, []aistudio.PoolScope{aistudio.PoolScopePro}) {
			t.Fatalf("%s（号池 %v）：err = %v，期望带 /pro 提示", selection.ModelID, selection.Pool, err)
		}
	}
	env.syncer.waitProbe()
	if reads := catalog.readAccounts(); len(reads) != 0 || len(probeLogLines(env.service)) != 0 {
		t.Fatalf("带提示的请求不应读目录：%v", reads)
	}
	live := aistudio.AccountSelection{ModelID: "gamma", Method: "bidiGenerateContent", Pool: aistudio.PoolScopeNormal}
	if err := env.service.missingModelError(live, fmt.Errorf("%w: 模型 gamma 不支持 bidiGenerateContent", aistudio.ErrModelNotFound)); errors.As(err, &hinted) {
		t.Fatalf("/pro 同样不支持 Live 的模型不应提示：%v", err)
	}
}

// TestGenerateNoCandidateHintsPro 生成主路径的改用入口提示：普通路径请求付费专用模型（普通号池目录列出、PAID_ONLY_MODELS 挡住 Free，
// 选号没有候选、模型不在列）以 404 结束并带改用 /pro 的提示；带思考等级后缀的写法（默认地址解析不了、原样进入选号）同样带提示；
// 都不向上游发送。PRO_POOL=false 时 Pro 账户回到普通号池，直接服务
func TestGenerateNoCandidateHintsPro(t *testing.T) {
	const free, pro = "hint-free@example.com", "hint-pro@example.com"
	upstream := &fallbackUpstream{}
	service, pool := runningGenerateService(t, upstream, context.Background(), time.Minute, free, pro)
	catalog := fallbackCatalog()
	for accountID, tier := range map[string]aistudio.BenefitTier{free: aistudio.BenefitTierFree, pro: aistudio.BenefitTierPro} {
		if err := pool.SetCatalog(accountID, tier, catalog); err != nil {
			t.Fatal(err)
		}
	}
	service.replaceModelSnapshot(catalog)
	pool.SetModelTierRules(nil, []string{fallbackTarget})
	pool.SetPoolRules(false, true)
	contents := []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}}
	final := func(model string) error {
		t.Helper()
		ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeNormal), 10*time.Second)
		defer cancel()
		events, err := service.Generate(ctx, aistudio.GenerateRequest{ID: "req-" + model, Model: model, Contents: contents})
		if err != nil {
			return err
		}
		for event := range events {
			if event.Kind == aistudio.EventError {
				err = event.Err
			}
		}
		return err
	}
	var hinted *aistudio.ModelInOtherPoolError
	var noServing *aistudio.NoServingAccountError
	err := final(fallbackTarget)
	if !errors.As(err, &hinted) || !errors.As(err, &noServing) || noServing.Listed || noServing.HTTPStatus() != http.StatusNotFound ||
		!slices.Equal(hinted.Pools, []aistudio.PoolScope{aistudio.PoolScopePro}) || hinted.PublicHint() != "This model is available through the /pro endpoints." {
		t.Fatalf("普通路径请求付费专用模型：err = %v，期望 404 并提示 /pro", err)
	}
	err = final(fallbackTarget + "-high")
	if !errors.As(err, &hinted) || !errors.Is(err, aistudio.ErrModelNotFound) || !slices.Equal(hinted.Pools, []aistudio.PoolScope{aistudio.PoolScopePro}) {
		t.Fatalf("带后缀的付费专用模型名：err = %v，期望提示 /pro", err)
	}
	if calls := upstream.models(); len(calls) != 0 {
		t.Fatalf("不应向上游发送：%v", calls)
	}
	pool.SetProPool(false)
	if err := final(fallbackTarget); err != nil {
		t.Fatalf("PRO_POOL=false 时 Pro 账户在普通号池，应直接服务：%v", err)
	}
}

// TestMissingModelHintOffWhenProPoolOff PRO_POOL=false 时普通路径的模型不存在错误与没有这项设置之前相同：不加改用入口的提示
// （即使 Ultra 号池能服务），照旧限频地在后台读目录
func TestMissingModelHintOffWhenProPoolOff(t *testing.T) {
	env, catalog, _ := newProbeTestEnv(t)
	pool := env.gen.pool
	if err := pool.SetCatalog("ultra-1@example.com", aistudio.BenefitTierUltra, catalogTestData(t, "alpha", "gamma").Models()); err != nil {
		t.Fatal(err)
	}
	pool.SetPoolRules(false, false)
	normal := aistudio.AccountSelection{ModelID: "gamma", Method: "generateContent", Pool: aistudio.PoolScopeNormal}
	err := env.service.missingModelError(normal, fmt.Errorf("%w: gamma", aistudio.ErrModelNotFound))
	var hinted *aistudio.ModelInOtherPoolError
	if errors.As(err, &hinted) || !errors.Is(err, aistudio.ErrModelNotFound) {
		t.Fatalf("PRO_POOL=false 时不应加改用入口的提示：%v", err)
	}
	env.syncer.waitProbe()
	if lines := probeLogLines(env.service); len(lines) != 1 || !strings.Contains(lines[0].Message, "模型=gamma") {
		t.Fatalf("PRO_POOL=false 时应照旧读目录：%+v（读了 %v）", lines, catalog.readAccounts())
	}
	// 开启后 Ultra 号池能服务的模型带 /ultra 提示
	pool.SetPoolRules(false, true)
	if hint := env.service.otherPoolHint(normal, aistudio.ErrModelNotFound); !errors.As(hint, &hinted) ||
		hinted.PublicHint() != "This model is available through the /ultra endpoints." {
		t.Fatalf("PRO_POOL=true 时应提示 /ultra：%v", hint)
	}
}

// TestCheckTierRulesProCarbon PRO_POOL=true 时 Pro 号池能服务只限 Ultra 的 carbon（不在 ULTRA_ONLY_MODELS 里）写一行 WARN；
// 规则挡住时、或 PRO_POOL=false 时不写
func TestCheckTierRulesProCarbon(t *testing.T) {
	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA}, 1, 1, 1, 1)
	carbon := []aistudio.Model{{ID: "gemini-4-carbon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for _, accountID := range []string{proPartitionA, partitionUltraA} {
		tier := aistudio.BenefitTierPro
		if accountID == partitionUltraA {
			tier = aistudio.BenefitTierUltra
		}
		if err := pool.SetCatalog(accountID, tier, carbon); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{pool: pool, requests: requests, workers: manager}
	warn := "PRO_POOL=true 时 Pro 号池能服务只限 Ultra 的模型 | 模型=gemini-4-carbon"
	pool.SetModelTierRules([]string{"gemini-4-carbon"}, nil)
	service.checkTierRules()
	if hasLogContaining(requests, warn) {
		t.Fatal("ULTRA_ONLY_MODELS 挡住 carbon 时不应提示")
	}
	pool.SetModelTierRules(nil, nil)
	service.checkTierRules()
	service.checkTierRules()
	if got := countLogs(requests, warn); got != 1 {
		t.Fatalf("Pro 号池能服务 carbon 时应提示一次，实际 %d 次", got)
	}
	pool.SetProPool(false)
	service.checkTierRules()
	pool.SetProPool(true)
	service.checkTierRules()
	if got := countLogs(requests, warn); got != 2 {
		t.Fatalf("关闭期间不提示、再开启后重新提示一次，实际 %d 次", got)
	}
}

// proPoolSave 经管理接口按当前生效配置保存（mutate 改要保存的字段），按页面的方式立即热更新
func (env *includeProRuntime) proPoolSave(t *testing.T, mutate func(*api.RuntimeConfig)) {
	t.Helper()
	env.manager.mu.RLock()
	value := runtimeConfigDTO(env.manager.current.config)
	env.manager.mu.RUnlock()
	mutate(&value)
	updated, err := env.manager.UpdateRuntimeConfig(env.ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ServiceRestartRequired {
		t.Fatal("保存 Pro 号池设置后不应要求重启生成服务")
	}
}

// TestProPoolHotReloadNoRestart 热切换 PRO_POOL 不重启浏览器：Pro 账户的 Worker 原地改到 Pro 分区计数（同一次保存调大 Pro 峰值时不超峰值），
// /pro 的开关与账户池一致，管理状态、预热概况与主用模型供给按新成员；日志写明生效与提示；再关闭全部恢复。
// ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效的提示只在两项之一变化时写
func TestProPoolHotReloadNoRestart(t *testing.T) {
	env := newIncludeProRuntime(t)
	var launches atomic.Int32
	launch := env.workers.launch
	env.workers.launch = func(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		launches.Add(1)
		return launch(ctx, accountID, options)
	}
	for _, accountID := range []string{partitionNormalA, includeProAccount, partitionUltraA} {
		if _, err := env.workers.promote(env.ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	started := launches.Load()
	if env.manager.activeProPool() || env.pool.ProPool() {
		t.Fatal("固定关闭的环境里 /pro 开关应为关闭")
	}
	env.workers.refreshPrimarySupply(time.Now())

	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		on, warm, peak := true, 1, 3
		value.ProPool, value.ProWarmWorkerLimit, value.ProMaxActiveWorkers = &on, &warm, &peak
	})
	if !env.pool.ProPool() || !env.manager.activeProPool() || !env.manager.current.config.ProPool {
		t.Fatal("保存后账户池、/pro 开关与生效配置都应开启 PRO_POOL")
	}
	// 保存写出了 PRO_POOL：之后不再按“默认开启”给升级提示
	if !env.service.proPoolConfigured.Load() || env.service.proPoolNotice(1) != nil {
		t.Fatal("保存后 .env 写明了 PRO_POOL，不应再有升级提示")
	}
	if got := env.pool.PartitionMembers().Of(includeProAccount); got != aistudio.PoolScopePro {
		t.Fatalf("Pro 账户分区 = %v", got)
	}
	if got := env.workers.warmAccountIDsIn(aistudio.PoolScopePro); !slices.Equal(got, []string{includeProAccount}) {
		t.Fatalf("Pro 分区驻留 = %v", got)
	}
	if got := env.workers.warmAccountIDsIn(aistudio.PoolScopeNormal); !slices.Equal(got, []string{partitionNormalA}) {
		t.Fatalf("普通分区驻留 = %v", got)
	}
	if launches.Load() != started || len(env.workers.WarmAccountIDs()) != 3 {
		t.Fatalf("切换不应启动或关闭浏览器：启动 %d→%d，驻留 %v", started, launches.Load(), env.workers.WarmAccountIDs())
	}
	status, err := env.admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.ProWorkers.Warm != 1 || status.ProWorkers.Max != 3 || status.ProAccounts.Total != 1 || status.Workers.Warm != 1 || status.UltraWorkers.Warm != 1 {
		t.Fatalf("管理状态 Pro Worker=%+v Pro 账户=%d 普通 Worker=%d Ultra Worker=%d", status.ProWorkers, status.ProAccounts.Total, status.Workers.Warm, status.UltraWorkers.Warm)
	}
	if got := env.workers.bootstrapSummary(); got.ProAvailable != 1 || got.AvailableIn(aistudio.PoolScopeNormal) != 1 {
		t.Fatalf("切换后预热概况应按新成员重算：%+v", got)
	}
	if !env.workers.refreshPrimarySupply(time.Now()) {
		t.Fatal("规则版本变化后应重新统计主用模型供给")
	}
	if !slices.ContainsFunc(env.workers.primarySupplyStatus(), func(entry api.AdminPrimarySupply) bool {
		return entry.Pool == "pro" && entry.Model == testRuntimeModel && entry.Warm == 1
	}) {
		t.Fatalf("应有 Pro 分区的主用模型供给：%+v", env.workers.primarySupplyStatus())
	}
	for _, fragments := range [][]string{
		{"Pro 独立号池设置已更新并立即生效 | 开启=true | Pro 号池账户=1 | Pro 分区 Worker=1/3 | Ultra 分区 Worker=1/1 | 普通分区 Worker=1/2"},
		{"PRO_POOL=true 时 Pro 账户只服务 /pro 前缀，不再服务默认地址与 /ultra；Pro 账户上的文件与视频改从 /pro 引用"},
		{"配置已热更新", "Pro 并入 Ultra=false | Pro 独立号池=true | Pro 常驻 Worker=1 | Pro 峰值 Worker=3"},
	} {
		if !hasLogContaining(env.requests, fragments...) {
			t.Fatalf("缺少日志 %q，日志 = %v", fragments, serviceLogMessages(env.requests))
		}
	}
	if hasLogContaining(env.requests, "Pro 分区 Worker 超过峰值") || hasLogContaining(env.requests, "Pro 账户并入 Ultra 号池设置已更新") {
		t.Fatalf("同一次保存调大了 Pro 峰值、ULTRA_INCLUDE_PRO 没有变：不应有超峰值或并入日志：%v", serviceLogMessages(env.requests))
	}

	// ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效：成员不变，只在这一项变化时提示一次
	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		on := true
		value.UltraIncludePro = &on
	})
	if env.pool.PartitionMembers().Of(includeProAccount) != aistudio.PoolScopePro || env.pool.UltraIncludePro() {
		t.Fatal("PRO_POOL=true 时 ULTRA_INCLUDE_PRO 不应生效")
	}
	notice := "ULTRA_INCLUDE_PRO=true 在 PRO_POOL=true 时不生效：Pro 账户只服务 /pro"
	if got := countLogs(env.requests, notice); got != 1 {
		t.Fatalf("开启 ULTRA_INCLUDE_PRO 时应提示一次，实际 %d 次", got)
	}
	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		value.RequestTimeout = "4m0s"
	})
	if got := countLogs(env.requests, notice); got != 1 {
		t.Fatalf("无关的保存不应再提示，实际 %d 次", got)
	}

	env.proPoolSave(t, func(value *api.RuntimeConfig) {
		off, include := false, false
		value.ProPool, value.UltraIncludePro = &off, &include
	})
	if env.pool.ProPool() || env.manager.activeProPool() || env.pool.PartitionMembers().Of(includeProAccount) != aistudio.PoolScopeNormal {
		t.Fatal("再关闭后 Pro 账户应回到普通分区，/pro 开关关闭")
	}
	if got := env.workers.warmAccountIDsIn(aistudio.PoolScopeNormal); !slices.Equal(sortedUnique(got), []string{partitionNormalA, includeProAccount}) {
		t.Fatalf("关闭后普通分区驻留 = %v", got)
	}
	if launches.Load() != started {
		t.Fatal("关闭也不应启动浏览器")
	}
	if !hasLogContaining(env.requests, "PRO_POOL=false 时 /pro 前缀返回 404；Pro 账户回到普通号池") ||
		!hasLogContaining(env.requests, "Pro 独立号池设置已更新并立即生效 | 开启=false") {
		t.Fatalf("缺少关闭日志：%v", serviceLogMessages(env.requests))
	}
}

// TestProPoolGateFollowsGeneration /pro 的开关只从生成服务实例的账户池读取：完整实例按账户池，测试替身等没有账户池的实例为关闭；
// 需要重建生成服务的保存挂起期间开关与成员都不变
func TestProPoolGateFollowsGeneration(t *testing.T) {
	env := newIncludeProRuntime(t)
	env.pool.SetProPool(true)
	env.manager.mu.Lock()
	env.manager.syncGenerationPoolRules(env.manager.current, true)
	env.manager.mu.Unlock()
	if !env.manager.activeProPool() || !env.service.proPoolConfigured.Load() {
		t.Fatal("开关应随账户池开启，并记下 PRO_POOL 已写")
	}
	env.manager.mu.Lock()
	env.manager.syncGenerationPoolRules(restartTestGeneration("STOPPED"), false)
	env.manager.mu.Unlock()
	if env.manager.activeProPool() {
		t.Fatal("没有账户池的实例开关应为关闭")
	}
	env.manager.mu.Lock()
	env.manager.syncGenerationPoolRules(env.manager.current, false)
	env.manager.mu.Unlock()
	// 同时改账户目录（需要重建）与 PRO_POOL：成员与开关不变，等新实例按新配置建立
	env.proPoolSaveRebuild(t)
	if !env.pool.ProPool() || !env.manager.activeProPool() {
		t.Fatal("需要重建时 PRO_POOL 不应热生效")
	}
}

// proPoolSaveRebuild 同时改账户目录与 PRO_POOL=false 并保存（需要重建生成服务，不热更新）
func (env *includeProRuntime) proPoolSaveRebuild(t *testing.T) {
	t.Helper()
	env.manager.mu.RLock()
	value := runtimeConfigDTO(env.manager.current.config)
	env.manager.mu.RUnlock()
	off := false
	value.ProPool, value.AuthStates = &off, t.TempDir()
	updated, err := env.manager.UpdateRuntimeConfig(env.ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ServiceRestartRequired {
		t.Fatal("改账户目录应要求重建")
	}
}

// TestProPoolDefaultOnRuntime 按默认配置建立账户池（PRO_POOL 默认开启）：Pro 账户在 Pro 分区，普通路径与 /ultra 都不用它，
// /pro 只用它；/pro 的模型列表没有只限 Ultra 的 gemini-4-carbon（默认 ULTRA_ONLY_MODELS）
func TestProPoolDefaultOnRuntime(t *testing.T) {
	pool, _ := testAccountPool(t, partitionNormalA, proPartitionA, partitionUltraA)
	catalog := []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: "gemini-4-carbon", Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}
	for accountID, tier := range map[string]aistudio.BenefitTier{proPartitionA: aistudio.BenefitTierPro, partitionUltraA: aistudio.BenefitTierUltra} {
		if err := pool.SetCatalog(accountID, tier, catalog); err != nil {
			t.Fatal(err)
		}
	}
	configurePool(pool, config.Default())
	if !pool.ProPool() || pool.PoolOf(proPartitionA) != aistudio.PoolScopePro {
		t.Fatalf("默认配置：PRO_POOL=%t，Pro 账户号池 %v", pool.ProPool(), pool.PoolOf(proPartitionA))
	}
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	for scope, want := range map[aistudio.PoolScope]string{
		aistudio.PoolScopeNormal: partitionNormalA, aistudio.PoolScopeUltra: partitionUltraA, aistudio.PoolScopePro: proPartitionA,
	} {
		for range 3 {
			lease, err := pool.AcquireFor(aistudio.ContextWithPoolScope(context.Background(), scope), selection)
			if err != nil || lease.Account().ID != want {
				t.Fatalf("号池 %v 的请求取到 %v（%v），期望 %s", scope, lease, err, want)
			}
			_ = lease.Release()
		}
	}
	for _, model := range pool.EligibleModelsIn(aistudio.PoolScopePro, catalog) {
		if model.ID == "gemini-4-carbon" {
			t.Fatal("/pro 的模型列表不应有 gemini-4-carbon")
		}
	}
	if len(pool.EligibleModelsIn(aistudio.PoolScopeUltra, catalog)) != 2 {
		t.Fatal("/ultra 的模型列表应有 gemini-4-carbon")
	}
}

// TestProPoolOffStatusParity PRO_POOL=false：Pro 账户留在普通分区，状态接口原有字段的含义不变（workers 含 Pro Worker），
// 新字段为空（pro_accounts、pro_workers 为 0，没有 pro_pool_notice、balanced.pro），浏览器上限与日志标记与原先相同
func TestProPoolOffStatusParity(t *testing.T) {
	manager, pool, requests := proPartitionManager(t, []string{partitionNormalA}, []string{partitionUltraA}, []string{proPartitionA}, 1, 3, 1, 3)
	pool.SetProPool(false)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, accountID := range []string{partitionNormalA, partitionUltraA, proPartitionA} {
		if _, err := manager.promote(ctx, accountID, testRuntimeModel); err != nil {
			t.Fatal(err)
		}
	}
	service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: manager}
	status, err := admin.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Warm != 2 || status.UltraWorkers.Warm != 1 || status.ProWorkers.Warm != 0 || status.ProAccounts.Total != 0 ||
		status.UltraAccounts.Total != 1 || status.ProPoolNotice != nil {
		t.Fatalf("PRO_POOL=false 的状态：普通 Worker=%d Ultra Worker=%d Pro Worker=%d Pro 账户=%d 提示=%v",
			status.Workers.Warm, status.UltraWorkers.Warm, status.ProWorkers.Warm, status.ProAccounts.Total, status.ProPoolNotice)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"accounts":`, `"workers":`, `"ultra_accounts":`, `"ultra_workers":`, `"pro_accounts":`, `"pro_workers":`} {
		if !strings.Contains(string(encoded), key) {
			t.Fatalf("状态 JSON 缺少 %s", key)
		}
	}
	if strings.Contains(string(encoded), "pro_pool_notice") {
		t.Fatal("PRO_POOL=false 时不应有升级提示")
	}
	cfg := config.Default()
	cfg.ProPool = false
	if proxyPoolCapacity(cfg, true) != proxyPoolCapacity(cfg, false) {
		t.Fatal("PRO_POOL=false 时浏览器上限不计 Pro")
	}
	if manager.PrewarmTarget() != manager.prewarmTargetFor(aistudio.PoolScopeNormal)+manager.prewarmTargetFor(aistudio.PoolScopeUltra) {
		t.Fatal("PRO_POOL=false 时 Pro 分区没有预热目标")
	}
}
