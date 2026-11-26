package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 坏 IP 自动换的接入测试：启动与热更新接线、.env 保存、端到端（Worker 启动失败 → 自动换 → 旧端口上的 Worker 后台关闭一次 → 下次启动用新端口）、
// 管理操作（恢复自动换、手动换会话）

// fixedAutoManager 为带 .env、IP 池与当前生成服务的管理器（照 TestFixedHotToggle 的装配）
type fixedAutoManager struct {
	manager  *runtimeManager
	ipPool   *proxyPool
	path     string
	legacy   string
	requests *requestRegistry
}

// newFixedAutoManager 写一份固定分配开启、没有 PROXY_POOL_FIXED_AUTO_REPLACE 这一行的 .env（旧版），按 newRuntime 的顺序装配 IP 池
func newFixedAutoManager(t *testing.T) *fixedAutoManager {
	t.Helper()
	clearAppConfigEnv(t)
	clearProxyEnv(t)
	for _, key := range []string{"PROXY_POOL_MODE", "PROXY_POOL_URL", "PROXY_POOL_PORT_START", "PROXY_POOL_PORT_END"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	cfg.ProxyPool = fixedStaticPool(10001, 10004)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	legacy := strings.Replace(string(data), "PROXY_POOL_FIXED_AUTO_REPLACE=10\n", "", 1)
	if legacy == string(data) {
		t.Fatal("保存默认配置应写出 PROXY_POOL_FIXED_AUTO_REPLACE=10")
	}
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, ultraTestNormalA)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	t.Cleanup(func() { _ = workers.Close() })
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.setFixedAutoReplace(cfg.ProxyPoolFixedAutoReplace)
	ipPool.configure(cfg.ProxyPool, cfg.Proxy, proxyPoolCapacity(cfg, false))
	ipPool.attach(pool)
	workers.proxyPool = ipPool
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg}, proxyPool: ipPool,
	}
	return &fixedAutoManager{manager: manager, ipPool: ipPool, path: path, legacy: legacy, requests: requests}
}

// limit 返回 IP 池里生效的上限
func (env *fixedAutoManager) limit() int {
	env.ipPool.mu.Lock()
	defer env.ipPool.mu.Unlock()
	return env.ipPool.fixed.auto.limit
}

// TestFixedAutoConfigWiring I1/I4：读取返回 proxy_pool_fixed_auto_replace=10；.env 没有这一行时原样保存与旧版页面（不带字段）保存都逐字节不变、
// 沿用现值；改值热更新（L13、IP 池版本不变、不需要重启），文件里有这一行后照常写真实值（含 0）；越界值拒绝；sameDataConfig 比较这一项
func TestFixedAutoConfigWiring(t *testing.T) {
	env := newFixedAutoManager(t)
	ctx := context.Background()
	manager := env.manager
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.ProxyPoolFixedAutoReplace == nil || *read.ProxyPoolFixedAutoReplace != 10 || env.limit() != 10 {
		t.Fatalf("读取应返回 10：%v (%v)，IP 池 %d", read.ProxyPoolFixedAutoReplace, err, env.limit())
	}
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(env.path); string(after) != env.legacy {
		t.Fatalf("从没写过时保存默认值不应改 .env：\n%s\n---\n%s", env.legacy, after)
	}
	legacyPage := read
	legacyPage.ProxyPoolFixedAutoReplace = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacyPage); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(env.path); string(after) != env.legacy || manager.current.config.ProxyPoolFixedAutoReplace != 10 {
		t.Fatalf("旧版页面保存应沿用现值、.env 不变：\n%s", after)
	}
	if countProxyPoolLogs(env.requests, "配置已热更新") != 0 || countProxyPoolLogs(env.requests, "IP 池自动换坏 IP 设置已更新") != 0 {
		t.Fatalf("没有变化时不应热更新：%v", proxyPoolLogLines(env.requests))
	}
	env.ipPool.mu.Lock()
	version := env.ipPool.version
	env.ipPool.mu.Unlock()
	five := 5
	changed := read
	changed.ProxyPoolFixedAutoReplace = &five
	updated, err := manager.UpdateRuntimeConfig(ctx, changed)
	if err != nil || updated.ServiceRestartRequired || updated.ProxyPoolFixedAutoReplace == nil || *updated.ProxyPoolFixedAutoReplace != 5 {
		t.Fatalf("改值 = %+v (%v)", updated.ProxyPoolFixedAutoReplace, err)
	}
	if after, _ := os.ReadFile(env.path); !strings.Contains(string(after), "\nPROXY_POOL_FIXED=true\nPROXY_POOL_FIXED_AUTO_REPLACE=5\n") {
		t.Fatalf("改值应写出这一行：\n%s", after)
	}
	env.ipPool.mu.Lock()
	versionAfter := env.ipPool.version
	env.ipPool.mu.Unlock()
	if env.limit() != 5 || manager.current.config.ProxyPoolFixedAutoReplace != 5 || versionAfter != version {
		t.Fatalf("应热更新到 IP 池且不让版本 +1：%d %d (%d → %d)", env.limit(), manager.current.config.ProxyPoolFixedAutoReplace, version, versionAfter)
	}
	if countProxyPoolLogs(env.requests, "INFO service IP 池自动换坏 IP 设置已更新并立即生效 | 30 分钟内最多自动换=5 个（PROXY_POOL_FIXED_AUTO_REPLACE，0 为不自动换）") != 1 {
		t.Fatalf("应写 L13：%v", proxyPoolLogLines(env.requests))
	}
	for _, value := range []int{10, 0} {
		value := value
		changed.ProxyPoolFixedAutoReplace = &value
		if _, err := manager.UpdateRuntimeConfig(ctx, changed); err != nil {
			t.Fatal(err)
		}
		if after, _ := os.ReadFile(env.path); !strings.Contains(string(after), fmt.Sprintf("\nPROXY_POOL_FIXED_AUTO_REPLACE=%d\n", value)) || env.limit() != value {
			t.Fatalf("文件里有这一行后照常写 %d：\n%s", value, after)
		}
	}
	bad := 5001
	changed.ProxyPoolFixedAutoReplace = &bad
	var operation *adminOperationError
	if _, err := manager.UpdateRuntimeConfig(ctx, changed); !errors.As(err, &operation) || operation.status != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "PROXY_POOL_FIXED_AUTO_REPLACE 必须是 0 到 5000 的整数") || env.limit() != 0 {
		t.Fatalf("越界值应拒绝：%v", err)
	}
	value := runtimeConfigDTO(manager.current.config)
	if !sameDataConfig(value, manager.current.config, dataConfigOverrides{}) {
		t.Fatal("相同时 sameDataConfig 应为真")
	}
	value.ProxyPoolFixedAutoReplace = &five
	if sameDataConfig(value, manager.current.config, dataConfigOverrides{}) {
		t.Fatal("只差上限时 sameDataConfig 应为假")
	}
	if requiresRebuild(manager.current.config, config.Default()) {
		t.Fatal("上限是热更新的，不应要求重建")
	}
}

// TestNewRuntimeSetsFixedAutoReplace I1：newRuntime 用 config.Default() 装配后 IP 池的上限为 10（在第一次 configure 之前设置）
func TestNewRuntimeSetsFixedAutoReplace(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "auth")
	writeEnableTestAccount(t, root, "a@example.com", true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	cfg.ProxyPool = fixedStaticPool(10001, 10002)
	ipPool := newProxyPool(proxyPoolOptions{requests: requests, relayLimit: func() int { return 1 << 20 }})
	lifecycle, stop := context.WithCancel(ctx)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, ipPool)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		_ = closeRuntime()
	}()
	ipPool.mu.Lock()
	limit, set := ipPool.fixed.auto.limit, ipPool.fixed.auto.limitSet
	ipPool.mu.Unlock()
	if limit != 10 || !set {
		t.Fatalf("装配后上限 = %d（已设置 %v）", limit, set)
	}
	if status := ipPool.status(); status.Fixed == nil || status.Fixed.Auto == nil || status.Fixed.Auto.Limit != 10 {
		t.Fatalf("状态应带自动换区块：%+v", status.Fixed)
	}
}

// TestFixedAutoEndToEnd I2/M10：固定静态，端口上一个账户的 Worker 两次 ErrUpstream 启动失败 → 自动换；同端口另一个账户已启动的 Worker 由冷却回调
// 登记后台关闭（只关一次），冷却回调写 L6；两个账户下一次启动都用新端口
func TestFixedAutoEndToEnd(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com"}
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10003), 4, nil, ids...)
	env.ipPool.setFixedAutoReplace(10)
	env.ipPool.configure(fixedStaticPool(10001, 10003), proxyPoolTestGlobal, 4)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	env.ipPool.setPortCooledHook(service.proxyPortCooled)
	failing := ids[0]
	port := fixedPoolPort(env.ipPool, failing)
	env.ipPool.mu.Lock()
	users := env.ipPool.fixedUsersLocked(port)
	env.ipPool.mu.Unlock()
	serving := users[0]
	if serving == failing {
		serving = users[1]
	}
	if err := env.promote(t, serving); err != nil {
		t.Fatal(err)
	}
	env.launches.mu.Lock()
	env.launches.fail = func(_ context.Context, accountID string) error {
		if accountID == failing {
			return upstreamFailure
		}
		return nil
	}
	env.launches.mu.Unlock()
	for range 2 {
		if err := env.promote(t, failing); err == nil {
			t.Fatal("启动应失败")
		}
	}
	newFailing, newServing := fixedPoolPort(env.ipPool, failing), fixedPoolPort(env.ipPool, serving)
	if newFailing == port || newServing == port {
		t.Fatalf("应自动换：%d %d", newFailing, newServing)
	}
	deadline := time.Now().Add(10 * time.Second)
	for env.manager.hasWorker(serving) {
		if time.Now().After(deadline) {
			t.Fatal("旧端口上的 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForLog(t, env.requests, fmt.Sprintf("IP 池端口冷却：固定分配已自动换 IP，关闭用这个端口的 Worker | 端口=%d | 规则=连续出口连不通 | 挪动=2 | 撤回账号=0 | 撤回坏号=0 | 失败=0 | 关闭 Worker=1", port))
	waitForLog(t, env.requests, "IP 池固定分配：已关闭不在固定端口上的 Worker，下次启动用固定端口")
	// 补登记不可调度（绑定带过自动换的暂停）也不会再关一次：Worker 已经没有了
	service.registerUnschedulableWorkers()
	time.Sleep(50 * time.Millisecond)
	if closes := env.launches.closes(serving); closes != 1 {
		t.Fatalf("旧端口上的 Worker 应只关一次：%d", closes)
	}
	env.launches.mu.Lock()
	env.launches.fail = nil
	env.launches.mu.Unlock()
	for _, id := range []string{failing, serving} {
		if err := env.promote(t, id); err != nil {
			t.Fatal(err)
		}
		if launched := env.launches.last(id); launched != staticPortURL(t, fixedPoolPort(env.ipPool, id)) || launched == staticPortURL(t, port) {
			t.Fatalf("%s 下次启动应用新端口：%s", id, config.RedactProxy(launched))
		}
	}
	if status := env.ipPool.status(); status.Fixed == nil || status.Fixed.Auto == nil || status.Fixed.Auto.ReplacedTotal != 1 || status.Fixed.Auto.Replaced[0].Port != port {
		t.Fatalf("状态应列出自动换过的端口：%+v", status.Fixed)
	}
	assertNoPoolSecretInLogs(t, env.requests)
}

// TestFixedAutoManagerOps I3：恢复自动换（IP 池没有就绪 503、没开 409、熔断时恢复并写 L8）；手动换会话（服务停止时 restarted=0、写 L14、错误码透传）；
// 运行中 Worker 用旧会话时登记后台关闭（L15）
func TestFixedAutoManagerOps(t *testing.T) {
	dynamic := dynamicProxyPool()
	dynamic.Fixed = true
	env := newPoolWorkerEnv(t, dynamicProxyPool(), 3, nil, "x@example.com", "y@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: env.pool, service: service, requests: env.requests, workers: env.manager}
	manager := &runtimeManager{lifecycle: ctx, requests: env.requests, current: &runtimeGeneration{service: service, admin: admin}, proxyPool: env.ipPool}
	var operation *adminOperationError
	empty := &runtimeManager{lifecycle: ctx, requests: env.requests, current: &runtimeGeneration{service: service, admin: admin}}
	if _, err := empty.ResumeProxyPoolAuto(ctx); !errors.As(err, &operation) || operation.code != "proxy_pool_unavailable" || operation.status != http.StatusServiceUnavailable {
		t.Fatalf("IP 池没有就绪：%v", err)
	}
	if _, err := empty.RotateProxyPoolSession(ctx, "x@example.com"); !errors.As(err, &operation) || operation.code != "proxy_pool_unavailable" {
		t.Fatalf("IP 池没有就绪：%v", err)
	}
	if _, err := manager.ResumeProxyPoolAuto(ctx); !errors.As(err, &operation) || operation.code != "proxy_pool_not_fixed" || operation.status != http.StatusConflict {
		t.Fatalf("没开固定会话：%v", err)
	}
	if _, err := manager.RotateProxyPoolSession(ctx, "x@example.com"); !errors.As(err, &operation) || operation.code != "proxy_pool_not_fixed_dynamic" {
		t.Fatalf("没开固定会话：%v", err)
	}
	env.ipPool.setFixedAutoReplace(1)
	env.ipPool.configure(dynamic, proxyPoolTestGlobal, 3)
	// x、y 两个账户各连续两次出口连不通：第 2 个熔断
	for _, id := range []string{"x@example.com", "y@example.com"} {
		for index := range proxyPoolUpstreamFails {
			lease := env.ipPool.acquire(id, id, proxyPoolKindWorker, id, proxyPoolTestGlobal)
			lease.launchFailed(upstreamFailure, false)
			if index == 0 {
				env.clock.Advance(time.Minute)
			}
		}
	}
	if status := env.ipPool.status(); status.Fixed.Auto == nil || !status.Fixed.Auto.Halted || status.Fixed.Auto.StuckTotal != 1 {
		t.Fatalf("应熔断：%+v", status.Fixed.Auto)
	}
	resumed, err := manager.ResumeProxyPoolAuto(ctx)
	if err != nil || !resumed.Resumed || resumed.Skipped != 0 || countSourceLogs(env.requests, "service", "IP 池已恢复自动换坏会话（管理页）") != 1 {
		t.Fatalf("恢复 = %+v (%v)", resumed, err)
	}
	before := env.accounts["y@example.com"]
	binding, _ := before.CurrentProxyBinding()
	rotated, err := manager.RotateProxyPoolSession(ctx, "y@example.com")
	if err != nil || rotated.From != binding.Slot || rotated.To == rotated.From || rotated.Restarted != 0 {
		t.Fatalf("手动换会话 = %+v (%v)", rotated, err)
	}
	if countSourceLogs(env.requests, "y@example.com", fmt.Sprintf("IP 池固定会话手动换会话 | 会话=%s → %s | 关闭 Worker=0", rotated.From, rotated.To)) != 1 {
		t.Fatalf("应写 L14：%v", proxyPoolLogLines(env.requests))
	}
	if after, _ := before.CurrentProxyBinding(); after.Slot != rotated.To {
		t.Fatalf("没有 Worker 的账户应立即用新会话：%s", fixedBindingText(after))
	}
	if status := env.ipPool.status(); status.Fixed.Auto.StuckTotal != 0 {
		t.Fatalf("手动换会话后连不通的记录应清除：%+v", status.Fixed.Auto)
	}
	if _, err := manager.RotateProxyPoolSession(ctx, "ghost@example.com"); !errors.As(err, &operation) || operation.code != "account_not_found" || operation.status != http.StatusNotFound {
		t.Fatalf("不存在的账户：%v", err)
	}
	// 运行中的 Worker 用旧会话：换会话后登记后台关闭，下次启动用新会话
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	result, err := env.ipPool.fixedRotateSession("x@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !env.ipPool.servesSession("x@example.com", result.From) {
		t.Fatal("旧 Worker 仍在旧会话上服务")
	}
	if restarted := env.manager.resetSessionWorker("x@example.com", result.From); restarted != 1 {
		t.Fatalf("应登记 1 个后台关闭：%d", restarted)
	}
	waitForLog(t, env.requests, "IP 池固定会话已换会话：已关闭用旧会话的 Worker，下次启动用新会话")
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	if launched := env.launches.last("x@example.com"); !strings.Contains(launched, result.To) {
		t.Fatalf("下次启动应用新会话：%s", config.RedactProxy(launched))
	}
	if env.manager.resetSessionWorker("x@example.com", result.From) != 0 {
		t.Fatal("已在新会话上的 Worker 不应登记")
	}
	assertNoPoolSecretInLogs(t, env.requests)
}
