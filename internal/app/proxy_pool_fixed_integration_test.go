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

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配的接入测试：Worker 启停、预检与名额、管理页登录/验证、删除账户、冷却回调与换 IP 的后台关闭、热更新与 .env

// fixedPoolPort 返回账户的固定端口
func fixedPoolPort(pool *proxyPool, accountID string) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.fixed.entries[accountID].port
}

// coolFixedPort 用规则 B 冷却账户的固定端口（同端口连续两次上游失败的启动，不经 Worker 管理器）
func coolFixedPort(t *testing.T, pool *proxyPool, accountID string) {
	t.Helper()
	for range proxyPoolUpstreamFails {
		lease := pool.acquire(accountID, accountID, proxyPoolKindWorker, accountID, proxyPoolTestGlobal)
		if lease.Refused() != nil {
			t.Fatalf("冷却前申请被拒：%v", lease.Refused())
		}
		lease.launchFailed(upstreamFailure, false)
	}
}

// TestFixedWorkerUsesFixedPort Worker 用固定端口，与 Go 侧出口一致；同端口两个账户可以同时启动
func TestFixedWorkerUsesFixedPort(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10001), 3, nil, "a@example.com", "b@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10001), proxyPoolTestGlobal, 3)
	for _, id := range []string{"a@example.com", "b@example.com"} {
		if err := env.promote(t, id); err != nil {
			t.Fatal(err)
		}
		if launched := env.launches.last(id); launched != staticPortURL(t, 10001) || env.accounts[id].EffectiveProxy(proxyPoolTestGlobal) != launched {
			t.Fatalf("%s 的 Worker 应在固定端口上、与 Go 侧一致：%s", id, config.RedactProxy(launched))
		}
	}
	if status := env.ipPool.status(); status.InUse != 1 || len(status.Assignments) != 2 || status.Fixed == nil || status.Fixed.MaxPerPort != 2 {
		t.Fatalf("两个账户应共用 10001：%+v", status)
	}
	assertNoPoolSecretInLogs(t, env.requests)
}

// TestFixedWorkerRefusedWhenPaused 固定端口冷却时：启动在取名额之前被拒（accountWorkerInitError、errors.Is 暂停）、不启动浏览器、不写 F13；
// 名额到手之后才暂停（预检之后的竞态）时归还名额，另一个账户马上能启动；预热与补充池不记失败
func TestFixedWorkerRefusedWhenPaused(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10002), 3, nil, "a@example.com", "b@example.com", "c@example.com", "d@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 3)
	// 有可执行文件路径时启动才占冷启动名额（启动预热并发 2）：被拒的启动没有归还名额时，两次之后健康账户的启动会一直等名额
	env.manager.mu.RLock()
	for _, worker := range env.manager.accounts {
		worker.mu.Lock()
		worker.config.ExecutablePath = "camoufox-test"
		worker.mu.Unlock()
	}
	env.manager.mu.RUnlock()
	pausedID := "a@example.com"
	port := fixedPoolPort(env.ipPool, pausedID)
	var healthy string
	for _, id := range []string{"b@example.com", "c@example.com", "d@example.com"} {
		if fixedPoolPort(env.ipPool, id) != port {
			healthy = id
			break
		}
	}
	coolFixedPort(t, env.ipPool, pausedID)
	err := env.promote(t, pausedID)
	var initErr *accountWorkerInitError
	if !errors.As(err, &initErr) || !errors.Is(err, errProxyPoolPortPaused) {
		t.Fatalf("固定端口冷却时应拒绝启动：%v", err)
	}
	if env.launches.last(pausedID) != "" || countSourceLogs(env.requests, "", "WAA Worker 不启动：IP 池固定端口暂停使用") != 0 {
		t.Fatal("预检应在取名额之前拒绝：不启动浏览器、不写 F13")
	}
	// 绕过预检（名额到手后才暂停）：名额归还，反复几次之后健康账户仍能马上启动
	for range 4 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := env.manager.startReservedWorker(ctx, pausedID, testRuntimeModel)
		cancel()
		if !errors.Is(err, errProxyPoolPortPaused) || errors.Is(err, errAccountWorkerCapacity) {
			t.Fatalf("名额到手后被拒应返回暂停错误（不是容量已满）：%v", err)
		}
	}
	if countSourceLogs(env.requests, "", fmt.Sprintf("WAA Worker 不启动：IP 池固定端口暂停使用 | 端口=%d | 原因=冷却中（连续出口连不通）", port)) != 4 {
		t.Fatalf("名额到手后被拒应写 F13：%v", proxyPoolLogLines(env.requests))
	}
	if err := env.promote(t, healthy); err != nil {
		t.Fatalf("被拒的启动应归还名额，健康账户应能启动：%v", err)
	}
	if env.launches.last(pausedID) != "" {
		t.Fatal("暂停的账户不应启动浏览器")
	}
	// 预热、补充池按暂停跳过，不记预热失败
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if env.manager.extraLaunchDone(ctx, pausedID, &accountWorkerInitError{err: &proxyPoolPausedError{Port: port, Reason: fixedPauseCoolingUpstream}}) {
		t.Fatal("补充池遇到暂停不应接着补")
	}
	env.manager.warmFailMu.Lock()
	_, failed := env.manager.warmFailures[pausedID]
	env.manager.warmFailMu.Unlock()
	if failed {
		t.Fatal("暂停不应记预热失败")
	}
	if binding, _ := env.accounts[pausedID].CurrentProxyBinding(); binding.Hold == "" || binding.URL != staticPortURL(t, port) {
		t.Fatalf("暂停的账户绑定应仍是固定端口并带原因：%s", fixedBindingText(binding))
	}
	if status, _ := env.pool.StatusOf(pausedID); status.State != aistudio.AccountProxyPaused {
		t.Fatalf("暂停的账户状态 = %s", status.State)
	}
}

// fixedAdminEnv 为带固定分配 IP 池的管理端（管理页登录、验证、删除）
func fixedAdminEnv(t *testing.T, cfg config.ProxyPool, ids ...string) (*runtimeAdmin, *proxyPool, *poolLogin, *poolLaunchRecorder) {
	t.Helper()
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	admin, _, _ := enableTestAdmin(t, ids...)
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(cfg.PortStart, cfg.PortEnd), "", 2)
	ipPool.attach(admin.pool)
	ipPool.configure(cfg, "", 2)
	admin.workers.proxyPool = ipPool
	launches := newPoolLaunchRecorder()
	admin.workers.launch = launches.launch
	login := &poolLogin{proxyRecordingLogin: proxyRecordingLogin{email: "new@example.com"}}
	admin.login = login
	return admin, ipPool, login, launches
}

// TestFixedAdminLoginAdopt 新账户浏览器登录：登录用的端口就是之后的固定端口（来源 login，预留归零），之后的 Worker 在同一端口；
// 邮箱已存在时 409、不改表、预留归零
func TestFixedAdminLoginAdopt(t *testing.T) {
	admin, ipPool, login, launches := fixedAdminEnv(t, fixedStaticPool(10001, 10004), "a@example.com")
	created, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	proxies := login.recorded()
	port := fixedPoolPort(ipPool, created.ID)
	if len(proxies) != 1 || port == 0 || proxies[0] != staticPortURL(t, port) {
		t.Fatalf("登录用的端口应写成固定端口：%v / %d", proxies, port)
	}
	ipPool.mu.Lock()
	source, reserved := ipPool.fixed.entries[created.ID].source, len(ipPool.fixed.reserved)
	ipPool.mu.Unlock()
	if source != fixedSourceLogin || reserved != 0 {
		t.Fatalf("来源 %s、预留 %d", source, reserved)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, created.ID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if launched := launches.last(created.ID); launched != staticPortURL(t, port) {
		t.Fatalf("Worker 应在固定端口 %d 上：%s", port, config.RedactProxy(launched))
	}
	before := fixedPoolPort(ipPool, "a@example.com")
	login.mu.Lock()
	login.email = "a@example.com"
	login.mu.Unlock()
	if _, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{}); err == nil {
		t.Fatal("邮箱已存在应返回 409")
	}
	ipPool.mu.Lock()
	reserved = len(ipPool.fixed.reserved)
	ipPool.mu.Unlock()
	if fixedPoolPort(ipPool, "a@example.com") != before || reserved != 0 {
		t.Fatalf("409 时不应改表、预留应归零：%d → %d，预留 %d", before, fixedPoolPort(ipPool, "a@example.com"), reserved)
	}
}

// TestFixedAdminOpsPaused 固定端口暂停时登录、验证返回 409 proxy_pool_port_paused（错误不含地址），不启动浏览器、不用默认代理；
// 全部端口暂停时新账户登录 503 proxy_pool_no_port
func TestFixedAdminOpsPaused(t *testing.T) {
	admin, ipPool, login, _ := fixedAdminEnv(t, fixedStaticPool(10001, 10001), "a@example.com")
	coolFixedPort(t, ipPool, "a@example.com")
	check := func(name string, err error, status int, code string) {
		t.Helper()
		var operation *adminOperationError
		if !errors.As(err, &operation) || operation.status != status || operation.code != code {
			t.Fatalf("%s：%v", name, err)
		}
		assertNoPoolSecret(t, name, err.Error())
		if strings.Contains(err.Error(), "isp.example.com") {
			t.Fatalf("%s 的错误不应含地址：%v", name, err)
		}
	}
	_, err := admin.verifyLogin(context.Background(), "a@example.com")
	check("验证", err, http.StatusConflict, "proxy_pool_port_paused")
	if !strings.Contains(err.Error(), "这个账户的固定端口 10001 暂停使用（冷却中（连续出口连不通），恢复=") {
		t.Fatalf("错误文案：%v", err)
	}
	_, err = admin.LoginAccount(context.Background(), "a@example.com")
	check("登录", err, http.StatusConflict, "proxy_pool_port_paused")
	_, err = admin.CreateAccount(context.Background(), api.AccountCreateInput{})
	check("新账户登录", err, http.StatusServiceUnavailable, "proxy_pool_no_port")
	if proxies := login.recorded(); len(proxies) != 0 {
		t.Fatalf("暂停时不应启动浏览器：%v", proxies)
	}
	// 暂停解除后可以验证（账户租约已归还）
	ipPool.mu.Lock()
	ipPool.ports[10001].coolUntil = time.Time{}
	ipPool.mu.Unlock()
	ipPool.recheckFired()
	if _, err := admin.verifyLogin(context.Background(), "a@example.com"); err != nil {
		t.Fatalf("暂停解除后应能验证：%v", err)
	}
}

// TestFixedDeleteAccountReleases 单个删除与批量删除释放固定位置，其他账户不变
func TestFixedDeleteAccountReleases(t *testing.T) {
	admin, ipPool, _, _ := fixedAdminEnv(t, fixedStaticPool(10001, 10002), "a@example.com", "b@example.com", "c@example.com")
	before := map[string]int{}
	for _, id := range []string{"a@example.com", "b@example.com", "c@example.com"} {
		before[id] = fixedPoolPort(ipPool, id)
	}
	if err := admin.DeleteAccount(context.Background(), "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if fixedPoolPort(ipPool, "a@example.com") != 0 || fixedPoolPort(ipPool, "b@example.com") != before["b@example.com"] {
		t.Fatal("单个删除应只释放这个账户")
	}
	if _, err := admin.DeleteAccounts(context.Background(), api.BulkDeleteInput{AccountIDs: []string{"b@example.com"}}); err != nil {
		t.Fatal(err)
	}
	if fixedPoolPort(ipPool, "b@example.com") != 0 || fixedPoolPort(ipPool, "c@example.com") != before["c@example.com"] {
		t.Fatal("批量删除应只释放这些账户")
	}
	if countSourceLogs(ipPool.requests, "a@example.com", "IP 池固定分配已释放 | 端口=", "原因=账户已删除") != 1 {
		t.Fatalf("应写 F7：%v", proxyPoolLogLines(ipPool.requests))
	}
}

// TestFixedCooledRelax 固定分配下的冷却回调：只给过渡 Worker 登记关闭（workerResetProxyFixed），写 F19 与 F20
func TestFixedCooledRelax(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10002), 3, nil, "x@example.com", "y@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 3)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	port := fixedPoolPort(env.ipPool, "x@example.com")
	service.relaxProxyPort(proxyPoolCooling{Port: port, Rule: proxyPoolRuleUpstream, Fixed: true, Workers: []string{"x@example.com"}})
	deadline := time.Now().Add(10 * time.Second)
	for env.manager.hasWorker("x@example.com") {
		if time.Now().After(deadline) {
			t.Fatal("过渡 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForLog(t, env.requests, fmt.Sprintf("IP 池端口冷却：固定分配不换端口，这个端口上的账户暂停使用到恢复 | 端口=%d | 规则=连续出口连不通 | 撤回账号=0 | 撤回坏号=0 | 失败=0 | 关闭过渡 Worker=1", port))
	waitForLog(t, env.requests, "IP 池固定分配：已关闭不在固定端口上的 Worker，下次启动用固定端口")
	if countSourceLogs(env.requests, "", "撤回逐级冷却并换端口重启 Worker") != 0 {
		t.Fatal("固定分配下不应写换端口重启的那一行")
	}
}

// TestFixedReassignRestartsWorkers 换 IP 后给在旧端口上服务的 Worker 登记后台关闭（F20），下次启动用新端口；重新平均不登记
func TestFixedReassignRestartsWorkers(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10003), 3, nil, "x@example.com", "y@example.com", "z@example.com")
	env.ipPool.configure(fixedStaticPool(10001, 10003), proxyPoolTestGlobal, 3)
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	old := fixedPoolPort(env.ipPool, "x@example.com")
	if _, err := env.ipPool.fixedRebalance(nil, false); err != nil || !env.manager.hasWorker("x@example.com") {
		t.Fatalf("重新平均不应关闭 Worker：%v", err)
	}
	result, serving, err := env.ipPool.fixedReassign(old, false)
	if err != nil || len(serving) != 1 || serving[0] != "x@example.com" {
		t.Fatalf("换 IP = %+v %v (%v)", result, serving, err)
	}
	if restarted := env.manager.resetFixedWorkers(serving, old); restarted != 1 {
		t.Fatalf("应登记 1 个后台关闭：%d", restarted)
	}
	deadline := time.Now().Add(10 * time.Second)
	for env.manager.hasWorker("x@example.com") {
		if time.Now().After(deadline) {
			t.Fatal("旧端口上的 Worker 应在后台关闭")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitForLog(t, env.requests, "IP 池固定分配：已关闭不在固定端口上的 Worker，下次启动用固定端口")
	if err := env.promote(t, "x@example.com"); err != nil {
		t.Fatal(err)
	}
	if launched := env.launches.last("x@example.com"); launched != staticPortURL(t, fixedPoolPort(env.ipPool, "x@example.com")) || launched == staticPortURL(t, old) {
		t.Fatalf("下次启动应用新端口：%s", config.RedactProxy(launched))
	}
	result.Restarted = 1
	if text := fixedReassignLog(result); !strings.HasPrefix(text, fmt.Sprintf("IP 池固定端口换 IP | 端口=%d | 挪动=1 | 挪到=", old)) ||
		!strings.HasSuffix(text, "| 重启 Worker=1 | 这个端口不再分配（可在 IP 池设置里恢复）") {
		t.Fatalf("F16 = %s", text)
	}
}

// TestFixedHotToggle 管理页开关：读取返回 proxy_pool_fixed；开 / 关热更新、不需要重启；旧版页面（不带该字段）保存其他设置不改开关；
// .env 原本没有这一行时保存 false 逐字节不变，保存 true 写出，之后照常写真实值
func TestFixedHotToggle(t *testing.T) {
	clearAppConfigEnv(t)
	clearProxyEnv(t)
	for _, key := range []string{"PROXY_POOL_MODE", "PROXY_POOL_URL", "PROXY_POOL_PORT_START", "PROXY_POOL_PORT_END", "PROXY_POOL_FIXED"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	cfg.ProxyPool = staticProxyPool(10001, 10004)
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	// 旧版 .env：没有 PROXY_POOL_FIXED 这一行
	data, _ := os.ReadFile(path)
	legacyEnv := strings.Replace(string(data), "PROXY_POOL_FIXED=false\n", "", 1)
	if err := os.WriteFile(path, []byte(legacyEnv), 0o600); err != nil {
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
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.ProxyPoolFixed == nil || *read.ProxyPoolFixed {
		t.Fatalf("读取应返回 proxy_pool_fixed=false：%v (%v)", read.ProxyPoolFixed, err)
	}
	// 原样保存：.env 逐字节不变（不写 PROXY_POOL_FIXED=false）
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); string(after) != legacyEnv {
		t.Fatalf("从没开过时保存 false 不应改 .env：\n%s\n---\n%s", legacyEnv, after)
	}
	// 只改开关：热更新、不需要重启
	on := read
	enabled := true
	on.ProxyPoolFixed = &enabled
	updated, err := manager.UpdateRuntimeConfig(ctx, on)
	if err != nil || updated.ServiceRestartRequired || updated.ProxyPoolFixed == nil || !*updated.ProxyPoolFixed {
		t.Fatalf("开启 = %+v (%v)", updated.ProxyPoolFixed, err)
	}
	saved, _ := config.Load(path)
	if !saved.ProxyPool.Fixed || !manager.current.config.ProxyPool.Fixed || !liveFieldsEqual(saved, manager.current.config) || requiresRebuild(saved, cfg) {
		t.Fatal("开关应热更新到生效配置")
	}
	if after, _ := os.ReadFile(path); !strings.Contains(string(after), "\nPROXY_POOL_PORT_END=10004\nPROXY_POOL_FIXED=true\n") {
		t.Fatalf("开启应写出 PROXY_POOL_FIXED=true：\n%s", after)
	}
	if status := ipPool.status(); status.Fixed == nil || status.Fixed.Accounts != 1 {
		t.Fatalf("开启后固定分配应生效：%+v", status.Fixed)
	}
	if binding, _ := accounts[0].CurrentProxyBinding(); binding.Slot != fmt.Sprint(fixedPoolPort(ipPool, ultraTestNormalA)) {
		t.Fatalf("没有 Worker 的账户应立即切到固定端口：%s", fixedBindingText(binding))
	}
	// 旧版页面：带 proxy_pool、不带 proxy_pool_fixed，保存其他设置不改开关
	legacy, _ := manager.RuntimeConfig(ctx)
	legacy.ProxyPoolFixed = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if saved, _ := config.Load(path); !saved.ProxyPool.Fixed || !manager.current.config.ProxyPool.Fixed {
		t.Fatal("旧版页面保存不应关掉固定分配")
	}
	// 关闭：文件里已有这一行，照常写 false
	off := legacy
	disabled := false
	off.ProxyPoolFixed = &disabled
	if _, err := manager.UpdateRuntimeConfig(ctx, off); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(path); !strings.Contains(string(after), "\nPROXY_POOL_FIXED=false\n") || manager.current.config.ProxyPool.Fixed {
		t.Fatalf("关闭应写出 false 并热更新：\n%s", after)
	}
	if status := ipPool.status(); status.Fixed != nil {
		t.Fatal("关闭后状态不应有 fixed")
	}
	// sameDataConfig：只差固定分配时为假，相同时为真
	value := runtimeConfigDTO(manager.current.config)
	if !sameDataConfig(value, manager.current.config, dataConfigOverrides{}) {
		t.Fatal("相同的固定分配 sameDataConfig 应为真")
	}
	value.ProxyPoolFixed = &enabled
	if sameDataConfig(value, manager.current.config, dataConfigOverrides{}) {
		t.Fatal("只差固定分配时 sameDataConfig 应为假")
	}
}

// TestFixedRuntimeManagerOps 管理接口：服务停止时重新平均不清理、换 IP 不登记重启；没开固定分配 409；IP 池没有就绪 503
func TestFixedRuntimeManagerOps(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10002), 3, nil, "x@example.com", "y@example.com")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	service := &trackedService{lifecycle: ctx, pool: env.pool, requests: env.requests, workers: env.manager}
	admin := &runtimeAdmin{lifecycle: ctx, pool: env.pool, service: service, requests: env.requests, workers: env.manager}
	manager := &runtimeManager{lifecycle: ctx, requests: env.requests, current: &runtimeGeneration{service: service, admin: admin}, proxyPool: env.ipPool}
	var operation *adminOperationError
	if _, err := manager.RebalanceProxyPool(ctx, api.ProxyPoolFixedInput{}); !errors.As(err, &operation) || operation.code != "proxy_pool_not_fixed" || operation.status != http.StatusConflict {
		t.Fatalf("没开固定分配时：%v", err)
	}
	env.ipPool.configure(fixedStaticPool(10001, 10002), proxyPoolTestGlobal, 3)
	port := fixedPoolPort(env.ipPool, "x@example.com")
	result, err := manager.ReassignProxyPoolPort(ctx, port, api.ProxyPoolFixedInput{})
	if err != nil || result.Moved != 1 || result.Restarted != 0 || !result.Excluded {
		t.Fatalf("换 IP = %+v (%v)", result, err)
	}
	if countSourceLogs(env.requests, "service", fmt.Sprintf("IP 池固定端口换 IP | 端口=%d | 挪动=1 |", port)) != 1 {
		t.Fatalf("应写 F16：%v", proxyPoolLogLines(env.requests))
	}
	if restored, err := manager.RestoreProxyPoolPort(ctx, port); err != nil || !restored.Restored {
		t.Fatalf("恢复 = %+v (%v)", restored, err)
	}
	if _, err := manager.ReassignProxyPoolPort(ctx, 30001, api.ProxyPoolFixedInput{}); !errors.As(err, &operation) || operation.code != "proxy_pool_port_out_of_range" {
		t.Fatalf("段外端口：%v", err)
	}
	if rebalanced, err := manager.RebalanceProxyPool(ctx, api.ProxyPoolFixedInput{DryRun: true}); err != nil || !rebalanced.DryRun || rebalanced.Pruned != 0 {
		t.Fatalf("服务停止时 dry-run = %+v (%v)", rebalanced, err)
	}
	empty := &runtimeManager{lifecycle: ctx, requests: env.requests, current: &runtimeGeneration{service: service, admin: admin}}
	if _, err := empty.RebalanceProxyPool(ctx, api.ProxyPoolFixedInput{}); !errors.As(err, &operation) || operation.code != "proxy_pool_unavailable" {
		t.Fatalf("IP 池没有就绪：%v", err)
	}
}
