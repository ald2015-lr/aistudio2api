package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/chromeauth"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

// IP 池接入测试（I1–I14）：Worker 启停、公共头、观察点、热更新、重建、状态、测试代理、登录与跟随默认代理

// assertNoPoolSecret 断言文本里没有测试 IP 池的密码与用户名
func assertNoPoolSecret(t *testing.T, where string, text string) {
	t.Helper()
	for _, secret := range []string{proxyPoolTestPassword, "S3cr3t-P%40ss", "user-static", "customer-a1"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s 含 IP 池账号密码（%s）: %s", where, secret, text)
		}
	}
}

// assertNoPoolSecretInLogs 断言管理页日志里没有测试 IP 池的账号密码
func assertNoPoolSecretInLogs(t *testing.T, registry *requestRegistry) {
	t.Helper()
	registry.mu.Lock()
	logs := append([]api.AdminLog(nil), registry.logs...)
	registry.mu.Unlock()
	for _, entry := range logs {
		assertNoPoolSecret(t, "日志", entry.Source+" "+entry.Message)
	}
}

// poolLaunchRecorder 为 manager.launch 的替身：记下每次启动拿到的代理，按 fail 决定是否失败，成功时返回 stub Worker
type poolLaunchRecorder struct {
	mu            sync.Mutex
	proxies       map[string][]string
	closeFailures map[string]int
	fail          func(ctx context.Context, accountID string) error
}

func newPoolLaunchRecorder() *poolLaunchRecorder {
	return &poolLaunchRecorder{proxies: make(map[string][]string), closeFailures: make(map[string]int)}
}

func (recorder *poolLaunchRecorder) launch(ctx context.Context, accountID string, options camoufoxnative.Options) (*aistudio.NativeWorker, error) {
	recorder.mu.Lock()
	recorder.proxies[accountID] = append(recorder.proxies[accountID], options.Proxy)
	fail := recorder.fail
	failures := recorder.closeFailures[accountID]
	recorder.mu.Unlock()
	if fail != nil {
		if err := fail(ctx, accountID); err != nil {
			return nil, err
		}
	}
	worker, _ := aistudio.NewStubWorker(accountID, failures)
	return worker, nil
}

func (recorder *poolLaunchRecorder) last(accountID string) string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	proxies := recorder.proxies[accountID]
	if len(proxies) == 0 {
		return ""
	}
	return proxies[len(proxies)-1]
}

// poolWorkerEnv 为带 IP 池的 Worker 管理器：账户池、管理器、IP 池（假时钟、无跨进程锁）与启动记录
type poolWorkerEnv struct {
	pool     *aistudio.AccountPool
	accounts map[string]*aistudio.Account
	manager  *accountWorkerManager
	ipPool   *proxyPool
	requests *requestRegistry
	launches *poolLaunchRecorder
	clock    *proxyPoolTestClock
}

func newPoolWorkerEnv(t *testing.T, cfg config.ProxyPool, maxActive int, ownProxy map[string]string, ids ...string) *poolWorkerEnv {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	pool, accounts := testAccountPool(t, ids...)
	byID := make(map[string]*aistudio.Account, len(accounts))
	for _, account := range accounts {
		byID[account.ID] = account
		if proxy, ok := ownProxy[account.ID]; ok {
			setAccountProxy(t, pool, account.ID, proxy)
		}
	}
	clock := newProxyPoolTestClock()
	ipPool, requests := newTestProxyPool(t, clock, "", "")
	manager := newAccountWorkerManager(pool, accounts, requests, "", proxyPoolTestGlobal, time.Minute, maxActive, maxActive, 2, false)
	launches := newPoolLaunchRecorder()
	manager.launch = launches.launch
	manager.proxyPool = ipPool
	t.Cleanup(func() { _ = manager.Close() })
	ipPool.configure(cfg, proxyPoolTestGlobal, maxActive)
	ipPool.attach(pool)
	return &poolWorkerEnv{pool: pool, accounts: byID, manager: manager, ipPool: ipPool, requests: requests, launches: launches, clock: clock}
}

// setAccountProxy 经账户租约写账户自己的代理（与管理页保存相同的路径，Worker 配置在之后创建的管理器里读取）
func setAccountProxy(t *testing.T, pool *aistudio.AccountPool, accountID, proxy string) {
	t.Helper()
	lease, err := pool.AcquireAccount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	accountConfig := lease.Account().Config
	accountConfig.Proxy = proxy
	if err := lease.SaveConfig(accountConfig); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

func (env *poolWorkerEnv) promote(t *testing.T, accountID string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := env.manager.promote(ctx, accountID, testRuntimeModel)
	return err
}

// assignment 返回账户在状态接口里的 Worker 租约槽位（没有时为空）
func (env *poolWorkerEnv) assignment(accountID string) string {
	status := env.ipPool.status()
	if status == nil {
		return ""
	}
	for _, assignment := range status.Assignments {
		if assignment.AccountID == accountID && assignment.Kind == proxyPoolKindWorker {
			return assignment.Slot
		}
	}
	return ""
}

// I1 + I2：没有账户代理时 Worker 用池分配的端口，Go 侧出口与浏览器一致，浏览器确认关闭后归还；有账户代理时不申请；
// 关闭失败时端口保持占用、重试关闭成功后归还；IP 池关闭时 Worker 走 PROXY 并登记直通租约，开启后计入 unpooled
func TestProxyPoolWorkerLaunch(t *testing.T) {
	const own = "http://203.0.113.50:3128"
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10003), 3, map[string]string{"own@example.com": own},
		"a@example.com", "b@example.com", "own@example.com")
	a, b := env.accounts["a@example.com"], env.accounts["b@example.com"]

	if err := env.promote(t, a.ID); err != nil {
		t.Fatal(err)
	}
	slotA := env.assignment(a.ID)
	port, err := strconv.Atoi(slotA)
	if err != nil || port < 10001 || port > 10003 {
		t.Fatalf("a 的端口 = %q", slotA)
	}
	if launched := env.launches.last(a.ID); launched != staticPortURL(t, port) || a.EffectiveProxy(proxyPoolTestGlobal) != launched {
		t.Fatalf("Worker 拿到的代理与 Go 侧出口不一致：Worker 端口 %d，Go 侧 %q", port, config.RedactProxy(a.EffectiveProxy(proxyPoolTestGlobal)))
	}
	if binding, ok := a.CurrentProxyBinding(); !ok || !binding.Active || binding.Slot != slotA {
		t.Fatalf("a 的绑定 = %+v", binding)
	}
	if workerConfig := env.manager.accounts[a.ID].config; workerConfig.Proxy != proxyPoolTestGlobal {
		t.Fatalf("Worker 配置不应被改写：%q", workerConfig.Proxy)
	}

	ownAccount := env.accounts["own@example.com"]
	if err := env.promote(t, ownAccount.ID); err != nil {
		t.Fatal(err)
	}
	if launched := env.launches.last(ownAccount.ID); launched != own || env.assignment(ownAccount.ID) != "" {
		t.Fatalf("有账户代理时 Worker 代理 = %q、租约 = %q", launched, env.assignment(ownAccount.ID))
	}

	// b 的浏览器第一次关闭失败：端口保持占用，重试关闭成功后归还
	env.launches.closeFailures[b.ID] = 1
	if err := env.promote(t, b.ID); err != nil {
		t.Fatal(err)
	}
	slotB := env.assignment(b.ID)
	if slotB == "" || slotB == slotA {
		t.Fatalf("b 的端口 = %q（a 为 %q）", slotB, slotA)
	}
	if err := env.manager.Reset(b.ID); err == nil {
		t.Fatal("第一次关闭应失败")
	}
	if env.assignment(b.ID) != slotB {
		t.Fatal("关闭失败时端口应保持占用")
	}
	if err := env.manager.Reset(b.ID); err != nil {
		t.Fatal(err)
	}
	if env.assignment(b.ID) != "" {
		t.Fatal("关闭成功后应归还端口")
	}
	if binding, ok := b.CurrentProxyBinding(); !ok || binding.Active {
		t.Fatalf("归还后 b 的绑定应为默认出口：%+v", binding)
	}

	// a 关闭后再启动回到原端口（亲和）
	if err := env.manager.Reset(a.ID); err != nil {
		t.Fatal(err)
	}
	if err := env.promote(t, a.ID); err != nil {
		t.Fatal(err)
	}
	if env.assignment(a.ID) != slotA {
		t.Fatalf("a 再启动的端口 = %q，期望回到 %q", env.assignment(a.ID), slotA)
	}
	if err := env.manager.Reset(a.ID); err != nil {
		t.Fatal(err)
	}

	// IP 池关闭：Worker 走 PROXY，登记直通租约（Go 侧同样走 PROXY）；再开启后计入 unpooled，关闭 Worker 后才切到 IP 池
	env.ipPool.configure(config.DefaultProxyPool(), proxyPoolTestGlobal, 3)
	if err := env.promote(t, a.ID); err != nil {
		t.Fatal(err)
	}
	if launched := env.launches.last(a.ID); launched != proxyPoolTestGlobal || a.EffectiveProxy(proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatalf("IP 池关闭时 Worker 代理 = %q", launched)
	}
	if env.ipPool.status() != nil {
		t.Fatal("IP 池关闭且没有池租约时状态应省略")
	}
	env.ipPool.configure(staticProxyPool(10001, 10003), proxyPoolTestGlobal, 3)
	if status := env.ipPool.status(); status == nil || status.Unpooled != 1 {
		t.Fatalf("开启后仍走默认代理的 Worker = %+v", status)
	}
	if a.EffectiveProxy(proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatal("运行中的 Worker 的账户 Go 侧应仍走 PROXY")
	}
	if err := env.manager.Reset(a.ID); err != nil {
		t.Fatal(err)
	}
	if proxy := a.EffectiveProxy(proxyPoolTestGlobal); proxy == proxyPoolTestGlobal || !strings.Contains(proxy, "isp.example.com:1000") {
		t.Fatalf("Worker 关闭后应切到 IP 池默认出口：%q", config.RedactProxy(proxy))
	}
	assertNoPoolSecretInLogs(t, env.requests)
}

// I1：启动被取消时 launchFailed 不计入端口健康；代理上游失败（ErrUpstream）连续 2 次冷却端口；与代理无关的失败不计
func TestProxyPoolWorkerLaunchFailures(t *testing.T) {
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10001), 2, nil, "a@example.com")
	a := env.accounts["a@example.com"]
	upstream := fmt.Errorf("加载 AI Studio 页面；本机代理中转：连不上上游代理%w", proxyrelay.ErrUpstream)

	// 取消的启动：即使带着 ErrUpstream 也不计
	env.launches.fail = func(ctx context.Context, _ string) error {
		<-ctx.Done()
		return upstream
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := env.manager.promote(ctx, a.ID, testRuntimeModel)
		done <- err
	}()
	for env.assignment(a.ID) == "" {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("取消的启动应失败")
	}
	upstreamFails := func() int {
		env.ipPool.mu.Lock()
		defer env.ipPool.mu.Unlock()
		if state := env.ipPool.ports[10001]; state != nil {
			return state.upstreamFails
		}
		return 0
	}
	if env.assignment(a.ID) != "" || upstreamFails() != 0 {
		t.Fatalf("取消后应归还且不计入：租约=%q 连续失败=%d", env.assignment(a.ID), upstreamFails())
	}

	// 与代理无关的失败不计
	env.launches.fail = func(context.Context, string) error { return errors.New("WAA 初始化失败") }
	if err := env.promote(t, a.ID); err == nil {
		t.Fatal("启动应失败")
	}
	if upstreamFails() != 0 {
		t.Fatal("与代理无关的失败不应计入")
	}
	// 连续 2 次代理上游失败：端口冷却
	env.launches.fail = func(context.Context, string) error { return upstream }
	for range 2 {
		if err := env.promote(t, a.ID); err == nil {
			t.Fatal("启动应失败")
		}
	}
	if status := env.ipPool.status(); status == nil || status.Cooling != 1 {
		t.Fatalf("连续 2 次出口连不通后端口应冷却：%+v", status)
	}
	if countProxyPoolLogs(env.requests, "IP 池端口冷却 | 端口=10001") != 1 {
		t.Fatal("缺少端口冷却日志")
	}
}

// I3：峰值内的 Worker 各占不同端口；端口少于浏览器数时共用并限流告警
func TestProxyPoolWorkersDistinctPorts(t *testing.T) {
	ids := proxyPoolTestIDs(12)
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10020), len(ids), nil, ids...)
	for _, id := range ids {
		if err := env.promote(t, id); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	for _, id := range ids {
		slot := env.assignment(id)
		if slot == "" {
			t.Fatalf("%s 没有端口", id)
		}
		if other, ok := seen[slot]; ok {
			t.Fatalf("%s 与 %s 同用端口 %s", id, other, slot)
		}
		seen[slot] = id
	}
	if status := env.ipPool.status(); status.InUse != len(ids) || status.Shared != 0 {
		t.Fatalf("状态 = %+v", status)
	}

	short := newPoolWorkerEnv(t, staticProxyPool(10001, 10004), len(ids), nil, ids...)
	for _, id := range ids {
		if err := short.promote(t, id); err != nil {
			t.Fatal(err)
		}
	}
	if status := short.ipPool.status(); status.InUse != 4 || status.Shared != 4 {
		t.Fatalf("端口不足时状态 = %+v", status)
	}
	if count := countProxyPoolLogs(short.requests, "IP 池端口不足，与其他账号共用"); count != 1 {
		t.Fatalf("端口不足的 WARN 每分钟只记一行：%d", count)
	}
}

// fakeTCPProxy 记下连接数的本机 TCP 监听器（读到请求后直接关闭，模拟连不通的代理）
type fakeTCPProxy struct {
	listener net.Listener
	hits     atomic.Int32
}

func newFakeTCPProxy(t *testing.T) *fakeTCPProxy {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &fakeTCPProxy{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.hits.Add(1)
			go func() {
				buffer := make([]byte, 1024)
				_ = connection.SetReadDeadline(time.Now().Add(time.Second))
				_, _ = connection.Read(buffer)
				_ = connection.Close()
			}()
		}
	}()
	return proxy
}

func (proxy *fakeTCPProxy) url() string {
	return "http://" + proxy.listener.Addr().String()
}

// I4：公共头发现时按账户当前的出口取 client：绑定变化后下一次发现走新出口；账户代理的校验不变
func TestProxyPoolHeaderProviderFollowsBinding(t *testing.T) {
	first, second := newFakeTCPProxy(t), newFakeTCPProxy(t)
	account := &aistudio.Account{ID: "a@example.com", Config: aistudio.DefaultAccountConfig("a@example.com")}
	account.SetProxyBinding(&aistudio.ProxyBinding{URL: first.url(), Slot: "1"})
	provider, err := newAccountHeaderProvider([]*aistudio.Account{account}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := provider.ProtocolHeaders(ctx, account.ID); err == nil {
		t.Fatal("假代理不会返回公共头")
	}
	if first.hits.Load() == 0 || second.hits.Load() != 0 {
		t.Fatalf("第一次发现应走绑定的出口：first=%d second=%d", first.hits.Load(), second.hits.Load())
	}
	account.SetProxyBinding(&aistudio.ProxyBinding{URL: second.url(), Slot: "2"})
	before := first.hits.Load()
	if _, err := provider.ProtocolHeaders(ctx, account.ID); err == nil {
		t.Fatal("假代理不会返回公共头")
	}
	if second.hits.Load() == 0 || first.hits.Load() != before {
		t.Fatalf("绑定变化后应换到新出口：first=%d second=%d", first.hits.Load(), second.hits.Load())
	}
	// 账户代理的校验不变：无效代理在准备更新时就报错
	invalid := aistudio.DefaultAccountConfig(account.ID)
	invalid.Proxy = "ftp://h:1"
	if _, err := provider.prepareUpdate(account, invalid); err == nil {
		t.Fatal("无效的账户代理应在准备更新时报错")
	}
}

// I6：生成本身的裸限流 429 记到账户最新的池租约端口；生成成功清空规则 A 的记录；两个不同账号在同一端口裸限流且其间没有成功时冷却
func TestProxyPoolObservationHooks(t *testing.T) {
	skipNearQuotaDay(t, time.Hour)
	upstream := &bareRateUpstream{limited: map[string]bool{}, midStream: map[string]bool{}}
	service, _ := rateLadderService(t, upstream, "a@example.com", "b@example.com", "c@example.com")
	clock := newProxyPoolTestClock()
	ipPool, logs := newTestProxyPool(t, clock, "", "")
	ipPool.configure(staticProxyPool(10001, 10001), "", 3)
	ipPool.attach(service.pool)
	service.workers.proxyPool = ipPool
	hits := func() int {
		status := ipPool.status()
		if status == nil || len(status.HotPorts) == 0 {
			return 0
		}
		return status.HotPorts[0].RateLimited
	}

	// a 在 10001 上裸限流，归还后 b 拿到同一端口、生成成功：规则 A 的记录清空，之后 b 的裸限流不凑成两个账号
	leaseA := ipPool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	upstream.set("a@example.com", true)
	failRateRequest(t, service, "r-a", "a@example.com")
	if hits() == 0 {
		t.Fatal("裸限流 429 应记到 a 的端口")
	}
	leaseA.release()
	leaseB := ipPool.acquire("b@example.com", "b@example.com", proxyPoolKindWorker, "b", "")
	if leaseB.Slot() != "10001" {
		t.Fatalf("b 的端口 = %q", leaseB.Slot())
	}
	if text, err, _ := runModelRequest(t, service, "r-b-ok", "b@example.com", rateLadderModel); err != nil || text == "" {
		t.Fatalf("b 应生成成功：%q %v", text, err)
	}
	upstream.set("b@example.com", true)
	failRateRequest(t, service, "r-b", "b@example.com")
	if status := ipPool.status(); status.Cooling != 0 {
		t.Fatal("其间有生成成功时不应冷却")
	}
	// c 也在这个端口上裸限流（端口不足共用）：窗口内两个不同账号、其间没有成功，c 刚在别的出口上成功过（IP 的证据），冷却
	leaseC := ipPool.acquire("c@example.com", "c@example.com", proxyPoolKindWorker, "c", "")
	upstream.set("c@example.com", true)
	markProxyPoolProven(ipPool, "c@example.com")
	failRateRequest(t, service, "r-c", "c@example.com")
	if status := ipPool.status(); status.Cooling != 1 {
		t.Fatalf("两个不同账号裸限流后应冷却：%+v", status)
	}
	leaseB.release()
	leaseC.release()
	if countProxyPoolLogs(logs, "IP 池端口冷却 | 端口=10001") != 1 {
		t.Fatal("缺少端口冷却日志")
	}
}

// C9：config.Config 的每个字段都在“热更新 / 需要重建 / 管理进程”之一：改动它要么让 liveFieldsEqual 为假（热更新），
// 要么让 requiresRebuild 为真（重建），要么在管理进程字段清单里；热更新与重建字段同时要进 sameDataConfig（否则“需要重启”提示不准）
func TestConfigFieldsCovered(t *testing.T) {
	management := map[string]bool{"ListenAddr": true, "ProxyAPIKey": true, "AutoStart": true, "RequestBodyLog": true, "AdminPassword": true}
	base := config.Default()
	value := reflectValueOf(&base)
	for index := 0; index < value.NumField(); index++ {
		field := value.Type().Field(index)
		if !field.IsExported() || management[field.Name] {
			continue
		}
		changed := base
		mutateConfigField(t, &changed, field.Name)
		live := !liveFieldsEqual(changed, base)
		rebuild := requiresRebuild(changed, base)
		if !live && !rebuild {
			t.Errorf("字段 %s 既不热更新也不重建，也不在管理进程清单里", field.Name)
			continue
		}
		if sameDataConfig(runtimeConfigDTO(changed), base, dataConfigOverrides{}) {
			t.Errorf("字段 %s 改动后 sameDataConfig 仍为真：保存后“需要重启”的判断看不到它", field.Name)
		}
		if !sameDataConfig(runtimeConfigDTO(changed), changed, dataConfigOverrides{}) {
			t.Errorf("字段 %s：保存值与生效值相同时 sameDataConfig 应为真（否则一直提示需要重启）", field.Name)
		}
	}
	if !sameDataConfig(runtimeConfigDTO(base), base, dataConfigOverrides{}) {
		t.Fatal("默认配置的 sameDataConfig 应为真")
	}
	pool := base
	pool.ProxyPool = staticProxyPool(10001, 10200)
	if liveFieldsEqual(pool, base) || requiresRebuild(pool, base) {
		t.Fatal("IP 池应按热更新处理")
	}
}

// I7：保存 IP 池设置写入 .env 规范值，读取原样返回、不提示需要重启、立即生效；原样再保存 .env 不变；旧版页面不带该字段时沿用现值；
// 端口为 0 时报错；只改峰值时 IP 池的容量随之更新
func TestProxyPoolHotConfig(t *testing.T) {
	clearAppConfigEnv(t)
	clearProxyEnv(t)
	for _, key := range []string{"PROXY_POOL_MODE", "PROXY_POOL_URL", "PROXY_POOL_PORT_START", "PROXY_POOL_PORT_END"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, ultraTestNormalA)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	t.Cleanup(func() { _ = workers.Close() })
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(cfg.ProxyPool, cfg.Proxy, proxyPoolCapacity(cfg))
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

	value := runtimeConfigDTO(cfg)
	if value.ProxyPool == nil || value.ProxyPool.Mode != "off" || value.ProxyPool.PortStart != 10001 || value.ProxyPool.PortEnd != 10200 {
		t.Fatalf("默认的 proxy_pool = %+v", value.ProxyPool)
	}
	value.ProxyPool = &api.ProxyPoolConfig{Mode: "Static", URL: " " + proxyPoolTestStaticURL + " ", PortStart: 10001, PortEnd: 10200}
	updated, err := manager.UpdateRuntimeConfig(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := config.NormalizeProxyPool(staticProxyPool(10001, 10200))
	if updated.ServiceRestartRequired || updated.ProxyPool == nil || updated.ProxyPool.Mode != "static" || updated.ProxyPool.URL != want.URL {
		t.Fatalf("保存后 = restart:%t proxy_pool:%+v", updated.ServiceRestartRequired, updated.ProxyPool)
	}
	saved, err := config.Load(path)
	if err != nil || !saved.ProxyPool.Equal(want) || saved.ProxyPool.URL != want.URL || saved.ProxyPool.Mode != "static" {
		t.Fatalf(".env 的 IP 池 = %+v（%v）", saved.ProxyPool, err)
	}
	if !manager.current.config.ProxyPool.Equal(want) || !liveFieldsEqual(saved, manager.current.config) || requiresRebuild(saved, manager.current.config) {
		t.Fatal("IP 池应已热更新到生效配置")
	}
	if status := ipPool.status(); status == nil || status.Mode != "static" || status.Total != 200 || status.Capacity != proxyPoolCapacity(saved) {
		t.Fatalf("热更新后 IP 池状态 = %+v", status)
	}
	if binding, ok := accounts[0].CurrentProxyBinding(); !ok || binding.Slot == "" {
		t.Fatal("没有 Worker 的账户应立即切到默认出口")
	}

	// 读取原样返回；原样再保存 .env 逐字节不变
	read, err := manager.RuntimeConfig(ctx)
	if err != nil || read.ProxyPool == nil || *read.ProxyPool != *updated.ProxyPool || read.ServiceRestartRequired {
		t.Fatalf("读取 = %+v（%v）", read.ProxyPool, err)
	}
	before, _ := os.ReadFile(path)
	if _, err := manager.UpdateRuntimeConfig(ctx, read); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("原样再保存 .env 变了：\n%s\n---\n%s", before, after)
	}
	assertNoPoolSecret(t, "状态", mustJSON(t, ipPool.status()))

	// 旧版页面：不带 proxy_pool 时沿用现值
	legacy := read
	legacy.ProxyPool = nil
	if _, err := manager.UpdateRuntimeConfig(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if saved, _ := config.Load(path); !saved.ProxyPool.Equal(want) {
		t.Fatalf("旧版页面保存后 IP 池 = %+v", saved.ProxyPool)
	}
	// 端口为 0（输入框被清空）报错，文案不含地址
	zero := read
	zero.ProxyPool = &api.ProxyPoolConfig{Mode: "static", URL: proxyPoolTestStaticURL, PortStart: 0, PortEnd: 10200}
	_, err = manager.UpdateRuntimeConfig(ctx, zero)
	message := assertAdminError(t, err, 400, "invalid_config")
	if !strings.Contains(message, "PROXY_POOL_PORT_START") {
		t.Fatalf("端口为 0 的错误 = %s", message)
	}
	assertNoPoolSecret(t, "错误", message)
	invalid := read
	invalid.ProxyPool = &api.ProxyPoolConfig{Mode: "dynamic", URL: proxyPoolTestStaticURL, PortStart: 10001, PortEnd: 10200}
	_, err = manager.UpdateRuntimeConfig(ctx, invalid)
	assertNoPoolSecret(t, "错误", assertAdminError(t, err, 400, "invalid_config"))

	// 只改峰值：IP 池的容量随热更新变化
	peak := read
	peak.MaxActiveWorkers = cfg.MaxActiveWorkers + 40
	if _, err := manager.UpdateRuntimeConfig(ctx, peak); err != nil {
		t.Fatal(err)
	}
	saved, _ = config.Load(path)
	if status := ipPool.status(); status.Capacity != proxyPoolCapacity(saved) || status.Capacity <= proxyPoolCapacity(cfg) {
		t.Fatalf("改峰值后容量 = %d，期望 %d", status.Capacity, proxyPoolCapacity(saved))
	}
	assertNoPoolSecretInLogs(t, requests)
}

// I8 + I9：newRuntime 在 NewAccountPool 之前写默认出口（构造后每个账户的出口已是 IP 池），装配完成后推送目标为新账户池；
// 状态接口带 proxy_pool，账户状态带端口兜底
func TestNewRuntimePrimesProxyPool(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), "auth")
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	for _, id := range ids {
		writeEnableTestAccount(t, root, id, true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = root
	cfg.WAABackend = config.WAABackendGo
	cfg.ProxyPool = staticProxyPool(10001, 10010)
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
	for _, id := range ids {
		account, err := admin.pool.Account(id)
		if err != nil {
			t.Fatal(err)
		}
		proxy := account.EffectiveProxy(cfg.Proxy)
		port, ok := cfg.ProxyPool.HostMatches(proxy)
		if !ok || port < 10001 || port > 10010 || proxy != staticPortURL(t, port) {
			t.Fatalf("%s 的出口 = %q，期望 IP 池端口", id, config.RedactProxy(proxy))
		}
	}
	ipPool.mu.Lock()
	attached := ipPool.owner == proxyPoolOwner(admin.pool)
	ipPool.mu.Unlock()
	if !attached {
		t.Fatal("装配完成后推送目标应为新账户池")
	}
	status, err := admin.Status(ctx)
	if err != nil || status.ProxyPool == nil || status.ProxyPool.Mode != "static" || status.ProxyPool.Total != 10 {
		t.Fatalf("状态 = %+v（%v）", status.ProxyPool, err)
	}
	assertNoPoolSecret(t, "状态", mustJSON(t, status))
	accounts, err := admin.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.ProxyPoolSlot == "" || account.ProxyPoolActive {
			t.Fatalf("账户 DTO 应带默认出口端口（不活动）：%+v", account)
		}
	}
	assertNoPoolSecret(t, "账户", mustJSON(t, accounts))
	assertNoPoolSecretInLogs(t, requests)
}

// reflectValueOf 返回配置结构体的反射值
func reflectValueOf(cfg *config.Config) reflect.Value {
	return reflect.ValueOf(cfg).Elem()
}

// mutateConfigField 只改动配置的一个字段（改成与默认值不同的合法形状），供 C9 判断它属于哪个比较集合
func mutateConfigField(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	switch name {
	case "ProxyPool":
		cfg.ProxyPool = staticProxyPool(10001, 10200)
		return
	case "DowngradeGuard":
		cfg.DowngradeGuard.Enabled = !cfg.DowngradeGuard.Enabled
		return
	}
	field := reflectValueOf(cfg).FieldByName(name)
	switch field.Kind() {
	case reflect.String:
		field.SetString(field.String() + "-changed")
	case reflect.Bool:
		field.SetBool(!field.Bool())
	case reflect.Int:
		field.SetInt(field.Int() + 1)
	case reflect.Int64:
		field.SetInt(field.Int() + int64(time.Second))
	case reflect.Slice:
		field.Set(reflect.Append(field, reflect.ValueOf("changed-model")))
	default:
		t.Fatalf("字段 %s 的类型 %s 没有改动方法，请在 mutateConfigField 里补上", name, field.Kind())
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// poolProbe 为按出口地址给 IP 的假探测：静态按端口给不同 IP（fail 中的端口失败、same 中的端口给同一个 IP），动态按会话给 IP
type poolProbe struct {
	mu      sync.Mutex
	proxies []string
	fail    map[int]bool
	same    map[int]bool
}

var poolProbePort = regexp.MustCompile(`:(\d+)$`)

func (probe *poolProbe) probe(_ context.Context, proxy string) api.ProxyTestProbe {
	probe.mu.Lock()
	probe.proxies = append(probe.proxies, proxy)
	probe.mu.Unlock()
	if strings.HasPrefix(proxy, "http://127.0.0.1:") {
		return api.ProxyTestProbe{IP: "192.0.2.200", ElapsedMS: 5}
	}
	match := poolProbePort.FindStringSubmatch(proxy)
	if match == nil {
		return api.ProxyTestProbe{Error: "地址无效"}
	}
	port, _ := strconv.Atoi(match[1])
	if probe.fail[port] {
		return api.ProxyTestProbe{Error: "连不上上游代理（" + proxy + "）"}
	}
	if probe.same[port] {
		return api.ProxyTestProbe{IP: "198.51.100.77", ElapsedMS: 7}
	}
	return api.ProxyTestProbe{IP: fmt.Sprintf("198.51.100.%d", port%250), ElapsedMS: 7}
}

// I10：抽测静态 IP 取首中尾端口、标出重复 IP 与失败；动态用 15 个字符的临时会话号；测账户实际出口返回端口；
// “检查全部端口”单飞、只走后台路径、不持有 proxyTestMu；结果、日志与错误都不含账号密码
func TestProxyPoolTestProxy(t *testing.T) {
	probe := &poolProbe{fail: map[int]bool{10010: true}, same: map[int]bool{10001: true, 10005: true}}
	manager, _ := newProxyTestManager(t, "", &recordingProxyProbe{answer: probe.probe})
	ctx := context.Background()
	staticNoAuth := api.ProxyPoolConfig{Mode: "static", URL: "http://isp.example.com:10000", PortStart: 10001, PortEnd: 10010}
	result, err := manager.TestProxy(ctx, api.ProxyTestInput{Pool: &staticNoAuth, Samples: 3})
	if err != nil {
		t.Fatal(err)
	}
	slots := make([]string, 0, len(result.Samples))
	for _, sample := range result.Samples {
		slots = append(slots, sample.Slot)
	}
	if !slices.Equal(slots, []string{"10001", "10005", "10010"}) || result.Proxy != "http://isp.example.com" {
		t.Fatalf("抽测的端口 = %v，出口 = %q", slots, result.Proxy)
	}
	if result.DistinctIPs == nil || *result.DistinctIPs != 1 ||
		!slices.Contains(result.Warnings, proxyWarningDuplicateIP) || !slices.Contains(result.Warnings, proxyWarningPoolPartial) {
		t.Fatalf("抽测结果 = %+v", result)
	}

	// 带账号密码的静态：浏览器路径经本机中转；错误按上游脱敏
	probe.fail = map[int]bool{10001: true}
	withAuth := api.ProxyPoolConfig{Mode: "static", URL: proxyPoolTestStaticURL, PortStart: 10001, PortEnd: 10002}
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Pool: &withAuth, Samples: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Samples) != 2 || !result.Relayed || !result.Authenticated {
		t.Fatalf("端口少于抽测数时全部抽测：%+v", result)
	}
	assertNoPoolSecret(t, "抽测结果", mustJSON(t, result))

	// 动态：临时会话号 "t" + 14 位十六进制
	dynamic := api.ProxyPoolConfig{Mode: "dynamic", URL: proxyPoolTestDynamicURL, PortStart: 10001, PortEnd: 10200}
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Pool: &dynamic})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Samples) != proxyPoolSampleDefault {
		t.Fatalf("动态抽测数 = %d", len(result.Samples))
	}
	for _, sample := range result.Samples {
		if !regexp.MustCompile(`^t[0-9a-f]{14}$`).MatchString(sample.Slot) {
			t.Fatalf("临时会话号 = %q", sample.Slot)
		}
	}
	assertNoPoolSecret(t, "动态抽测结果", mustJSON(t, result))
	if _, err := manager.TestProxy(ctx, api.ProxyTestInput{Pool: &api.ProxyPoolConfig{Mode: "off", PortStart: 10001, PortEnd: 10200}}); err == nil {
		t.Fatal("IP 池关闭时抽测应报错")
	}

	// 检查全部端口：只走后台请求路径；已在进行时 409；不持有 proxyTestMu
	probe.fail, probe.same = nil, map[int]bool{10001: true, 10005: true}
	clock := newProxyPoolTestClock()
	ipPool, _ := newTestProxyPool(t, clock, "", "")
	manager.proxyPool = ipPool
	manager.lifecycle = ctx
	release := make(chan struct{})
	var checked atomic.Int32
	manager.probeProxy = func(ctx context.Context, proxy string) api.ProxyTestProbe {
		checked.Add(1)
		<-release
		return probe.probe(ctx, proxy)
	}
	if err := manager.CheckProxyPool(ctx, api.ProxyPoolCheckInput{Pool: &staticNoAuth}); err != nil {
		t.Fatal(err)
	}
	assertAdminError(t, manager.CheckProxyPool(ctx, api.ProxyPoolCheckInput{Pool: &staticNoAuth}), 409, "proxy_pool_check_busy")
	if !manager.proxyTestMu.TryLock() {
		t.Fatal("检查全部端口不应持有 proxyTestMu")
	}
	manager.proxyTestMu.Unlock()
	assertAdminError(t, manager.CheckProxyPool(ctx, api.ProxyPoolCheckInput{Pool: &dynamic}), 400, "invalid_config")
	close(release)
	deadline := time.Now().Add(10 * time.Second)
	for {
		status := ipPool.status()
		if status == nil {
			// IP 池本身关闭时状态省略，检查结果直接读
			ipPool.mu.Lock()
			check := ipPool.check
			running := ipPool.checkRunning
			ipPool.mu.Unlock()
			if check != nil && !running {
				if check.Total != 10 || check.Done != 10 || len(check.Failed) != 0 || len(check.Duplicates) != 1 {
					t.Fatalf("检查结果 = %+v", check)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("检查没有结束")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if checked.Load() != 10 {
		t.Fatalf("检查只走后台路径，每个端口一次：%d", checked.Load())
	}
	assertNoPoolSecretInLogs(t, manager.requests)
}

// I10：测试账户实际出口（account_id）：返回它当前的端口；复查 2-2：账户有自己的代理（编辑对话框里清空了）时测清空后 IP 池给它的出口，
// IP 池关闭时测默认代理
func TestProxyPoolTestAccountProxy(t *testing.T) {
	const own = "http://203.0.113.50:3128"
	env := newPoolWorkerEnv(t, staticProxyPool(10001, 10003), 2, map[string]string{"own@example.com": own}, "a@example.com", "own@example.com")
	probe := &poolProbe{}
	manager, _ := newProxyTestManager(t, "", &recordingProxyProbe{answer: probe.probe})
	manager.proxyPool = env.ipPool
	admin := &runtimeAdmin{pool: env.pool, workers: env.manager, requests: manager.requests}
	manager.current = &runtimeGeneration{admin: admin, config: config.Config{Proxy: proxyPoolTestGlobal}}
	cleared, err := manager.TestProxy(context.Background(), api.ProxyTestInput{AccountID: "own@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	preview, ok := env.ipPool.accountPreview("own@example.com")
	if !ok || cleared.Slot != preview.slot || cleared.Proxy != "http://isp.example.com:"+preview.slot ||
		slices.Contains(probe.proxies, own) || !slices.Contains(probe.proxies, preview.url) {
		t.Fatalf("清空账户代理后的出口测试 = %+v，期望端口 %s", cleared, preview.slot)
	}
	if countSourceLogs(manager.requests, "service", "代理测试 | IP 池=静态 | 端口="+preview.slot) != 1 {
		t.Fatal("缺少清空账户代理后测试的日志")
	}
	assertNoPoolSecret(t, "清空账户代理后的出口测试", mustJSON(t, cleared))
	if err := env.promote(t, "a@example.com"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.TestProxy(context.Background(), api.ProxyTestInput{AccountID: "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Slot != env.assignment("a@example.com") || result.Slot == "" || !result.Relayed {
		t.Fatalf("账户出口测试 = %+v（端口 %q）", result, env.assignment("a@example.com"))
	}
	assertNoPoolSecret(t, "账户出口测试", mustJSON(t, result))
	assertNoPoolSecretInLogs(t, manager.requests)
	if _, err := manager.TestProxy(context.Background(), api.ProxyTestInput{AccountID: "missing@example.com"}); err == nil {
		t.Fatal("不存在的账户应报错")
	}
	env.ipPool.configure(config.ProxyPool{Mode: config.ProxyPoolOff}, proxyPoolTestGlobal, 2)
	cleared, err = manager.TestProxy(context.Background(), api.ProxyTestInput{AccountID: "own@example.com"})
	if err != nil || cleared.Slot != "" || cleared.Proxy != config.RedactProxy(proxyPoolTestGlobal) {
		t.Fatalf("IP 池关闭时清空账户代理后测默认代理：%+v（%v）", cleared, err)
	}
	manager.current = &runtimeGeneration{admin: admin, config: config.Config{}}
	_, err = manager.TestProxy(context.Background(), api.ProxyTestInput{AccountID: "own@example.com"})
	assertAdminError(t, err, 400, "proxy_required")
}

// poolLogin 为记录代理的隔离登录：登录时检查 IP 池里有这次登录的租约
type poolLogin struct {
	proxyRecordingLogin
	during func()
}

func (login *poolLogin) Login(ctx context.Context, request aistudio.IsolatedLoginRequest) (aistudio.IsolatedLoginResult, error) {
	if login.during != nil {
		login.during()
	}
	return login.proxyRecordingLogin.Login(ctx, request)
}

func (login *poolLogin) Verify(ctx context.Context, request aistudio.IsolatedLoginRequest, state aistudio.StorageState) (aistudio.LoginVerification, error) {
	login.mu.Lock()
	login.proxies = append(login.proxies, request.Proxy)
	login.mu.Unlock()
	if login.during != nil {
		login.during()
	}
	return aistudio.LoginVerification{Authenticated: true}, nil
}

// I11：新账户登录、已有账户重新登录与验证都持有 IP 池出口并在返回时归还；新账户登录的端口记到账户名下（之后的 Worker 回到这个 IP）；
// Chrome 导入在 IP 池开启时逐个账号、各用各的出口
func TestProxyPoolAdminLoginLeases(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	admin, _, _ := enableTestAdmin(t, "a@example.com")
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10004), "", 2)
	ipPool.attach(admin.pool)
	admin.workers.proxyPool = ipPool
	launches := newPoolLaunchRecorder()
	admin.workers.launch = launches.launch
	leases := func() []api.AdminProxyPoolAssignment {
		if status := ipPool.status(); status != nil {
			return status.Assignments
		}
		return nil
	}
	login := &poolLogin{proxyRecordingLogin: proxyRecordingLogin{email: "new@example.com"}}
	var during []api.AdminProxyPoolAssignment
	login.during = func() { during = leases() }
	admin.login = login

	created, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{})
	if err != nil {
		t.Fatal(err)
	}
	proxies := login.recorded()
	port, ok := staticProxyPool(10001, 10004).HostMatches(proxies[0])
	if len(proxies) != 1 || !ok || proxies[0] != staticPortURL(t, port) {
		t.Fatalf("新账户登录的代理 = %v", proxies)
	}
	if len(during) != 1 || during[0].Kind != proxyPoolKindLogin || during[0].Slot != strconv.Itoa(port) {
		t.Fatalf("登录期间的租约 = %+v", during)
	}
	if len(leases()) != 0 {
		t.Fatalf("登录返回后应归还：%+v", leases())
	}
	if created.ProxyPoolSlot != strconv.Itoa(port) || created.Proxy != "" {
		t.Fatalf("新账户 = %+v，期望默认出口回到登录用的端口 %d", created, port)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, created.ID, testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if launched := launches.last(created.ID); launched != staticPortURL(t, port) {
		t.Fatalf("新账户的 Worker 应回到登录时的端口 %d：%q", port, config.RedactProxy(launched))
	}

	// 验证：账户已有 Worker 时共用它的端口；返回后只剩 Worker 的租约
	during = nil
	if _, err := admin.verifyLogin(context.Background(), created.ID); err != nil {
		t.Fatal(err)
	}
	if verified := login.recorded(); verified[len(verified)-1] != staticPortURL(t, port) {
		t.Fatalf("验证的代理应与 Worker 同端口：%q", config.RedactProxy(verified[len(verified)-1]))
	}
	if len(during) != 2 || len(leases()) != 1 || leases()[0].Kind != proxyPoolKindWorker {
		t.Fatalf("验证期间 %+v，之后 %+v", during, leases())
	}

	// 重新登录：先关闭 Worker（归还端口），登录用自己的端口，返回后归还
	login.email = "a@example.com"
	during = nil
	if _, err := admin.LoginAccount(context.Background(), "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(during) != 2 || len(leases()) != 1 {
		t.Fatalf("重新登录期间 %+v，之后 %+v", during, leases())
	}

	// Chrome 导入：IP 池开启时逐个账号、各自出口
	var imports []chromeauth.ImportOptions
	var importMu sync.Mutex
	previous := chromeImport
	chromeImport = func(_ context.Context, options chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		importMu.Lock()
		imports = append(imports, options)
		importMu.Unlock()
		var results []chromeauth.ImportResult
		for _, id := range options.AccountIDs {
			results = append(results, chromeauth.ImportResult{Email: strings.TrimPrefix(id, "Default/")})
		}
		return results, nil
	}
	t.Cleanup(func() { chromeImport = previous })
	// 复查 2-2：导入只在 Go 侧做 OAuth，每个账号导入完就归还端口、只记下端口号，账户新建后按记下的值登记到名下
	results, claims, err := admin.importChrome(context.Background(), "root", "", []string{"Default/x@example.com", "Default/y@example.com"})
	if err != nil || len(results) != 2 || len(imports) != 2 || len(claims) != 2 || claims[0] == nil || claims[1] == nil {
		t.Fatalf("逐个导入：%d 个结果、%d 次调用、出口记录 %v（%v）", len(results), len(imports), claims, err)
	}
	if claims[0].port == 0 || claims[0].port == claims[1].port || len(leases()) != 1 {
		t.Fatalf("逐个导入应各自独占、导入完就归还：%d / %d，之后的占用 %+v", claims[0].port, claims[1].port, leases())
	}
	for index, options := range imports {
		if len(options.AccountIDs) != 1 {
			t.Fatalf("IP 池开启时每次只导入一个账号：%v", options.AccountIDs)
		}
		if options.Proxy != staticPortURL(t, claims[index].port) {
			t.Fatalf("导入应使用 IP 池出口 %d：%q", claims[index].port, config.RedactProxy(options.Proxy))
		}
	}
	claims[0].adopt("x@example.com")
	pool := admin.ipPool()
	pool.mu.Lock()
	affinity, owner := pool.affinity["x@example.com"], pool.portLocked(claims[0].port).lastAccount
	pool.mu.Unlock()
	if affinity.port != claims[0].port || owner != "x@example.com" {
		t.Fatalf("账户新建后应按记下的端口登记：亲和 %d、归属 %q，期望 %d", affinity.port, owner, claims[0].port)
	}
	imports = nil
	if _, batchClaims, err := admin.importChrome(context.Background(), "root", "http://203.0.113.50:3128", []string{"Default/x@example.com", "Default/y@example.com"}); err != nil || len(batchClaims) != 2 || batchClaims[0] != nil {
		t.Fatalf("整批导入不应留出口记录：%v %v", batchClaims, err)
	}
	if len(imports) != 1 || imports[0].Proxy != "http://203.0.113.50:3128" {
		t.Fatalf("填了账户代理时整批一次：%+v", imports)
	}
	if len(leases()) != 1 {
		t.Fatalf("导入结束后应归还全部导入租约：%+v", leases())
	}
	assertNoPoolSecretInLogs(t, admin.requests)
}

// I12：“改为跟随默认代理”只改规范值等于 PROXY 的账户；运行中的 Worker 不重启、补直通租约（Go 侧仍走 PROXY），
// 关闭后改用 IP 池；没有 Worker 的账户立即切到默认出口
func TestFollowDefaultProxy(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	admin, _, accounts := enableTestAdmin(t, "a@example.com", "b@example.com", "c@example.com")
	admin.config = config.Config{Proxy: proxyPoolTestGlobal}
	for _, id := range []string{"a@example.com", "b@example.com"} {
		setAccountProxy(t, admin.pool, id, proxyPoolTestGlobal)
	}
	setAccountProxy(t, admin.pool, "c@example.com", "http://203.0.113.50:3128")
	// Worker 配置按账户代理重建（与管理页保存相同）
	for _, account := range accounts {
		admin.workers.accounts[account.ID].config = admin.workers.workerConfig(account)
		admin.workers.accounts[account.ID].ownProxy = true
	}
	admin.workers.globalProxy = proxyPoolTestGlobal
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10004), proxyPoolTestGlobal, 2)
	ipPool.attach(admin.pool)
	admin.workers.proxyPool = ipPool
	launches := newPoolLaunchRecorder()
	admin.workers.launch = launches.launch
	if pinned := ipPool.pinnedDefault(); !slices.Equal(pinned, []string{"a@example.com", "b@example.com"}) {
		t.Fatalf("固定了默认代理的账户 = %v", pinned)
	}
	// 复查 2-3：账户列表的固定标记由服务端按当前生效的 PROXY 给出
	pinnedFlags := func() []string {
		t.Helper()
		listed, err := admin.Accounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, account := range listed {
			if account.ProxyPoolPinned {
				ids = append(ids, account.ID)
			}
		}
		slices.Sort(ids)
		return ids
	}
	if flags := pinnedFlags(); !slices.Equal(flags, []string{"a@example.com", "b@example.com"}) {
		t.Fatalf("账户列表的固定标记 = %v", flags)
	}
	if single, err := admin.account("a@example.com"); err != nil || !single.ProxyPoolPinned {
		t.Fatalf("单个账户的固定标记 = %+v（%v）", single, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, "a@example.com", testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if launches.last("a@example.com") != proxyPoolTestGlobal {
		t.Fatal("固定了默认代理的账户启动时不用 IP 池")
	}

	result, err := admin.FollowDefaultProxy(context.Background())
	if err != nil || result.Changed != 2 || result.Running != 1 || result.Skipped != 0 {
		t.Fatalf("结果 = %+v（%v）", result, err)
	}
	a, _ := admin.pool.Account("a@example.com")
	b, _ := admin.pool.Account("b@example.com")
	c, _ := admin.pool.Account("c@example.com")
	if diskAccountProxy(t, a.Directory) != "" || diskAccountProxy(t, b.Directory) != "" || c.Config.Proxy == "" {
		t.Fatal("只应清空等于 PROXY 的账户代理")
	}
	if a.EffectiveProxy(proxyPoolTestGlobal) != proxyPoolTestGlobal {
		t.Fatal("运行中的 Worker 不重启：Go 侧在浏览器关闭前仍走 PROXY")
	}
	if _, ok := staticProxyPool(10001, 10004).HostMatches(b.EffectiveProxy(proxyPoolTestGlobal)); !ok {
		t.Fatalf("没有 Worker 的账户应立即切到 IP 池：%q", config.RedactProxy(b.EffectiveProxy(proxyPoolTestGlobal)))
	}
	if len(ipPool.pinnedDefault()) != 0 || len(pinnedFlags()) != 0 {
		t.Fatal("改完后不应再有固定了默认代理的账户")
	}
	// 复查 2-3：Worker 仍走默认代理（补登记的直通租约）时验证用同一个上游，不另分端口
	login := &poolLogin{proxyRecordingLogin: proxyRecordingLogin{email: "a@example.com"}}
	var inUse int
	login.during = func() { inUse = ipPool.status().InUse }
	admin.login = login
	if _, err := admin.verifyLogin(context.Background(), "a@example.com"); err != nil {
		t.Fatal(err)
	}
	if verified := login.recorded(); len(verified) != 1 || verified[0] != proxyPoolTestGlobal || inUse != 0 {
		t.Fatalf("验证应与仍走默认代理的 Worker 同一上游：%v，验证期间占用端口 %d", verified, inUse)
	}
	if err := admin.workers.Reset("a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := staticProxyPool(10001, 10004).HostMatches(a.EffectiveProxy(proxyPoolTestGlobal)); !ok {
		t.Fatal("Worker 关闭后应改用 IP 池")
	}
	if _, err := admin.workers.promote(ctx, "a@example.com", testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if _, ok := staticProxyPool(10001, 10004).HostMatches(launches.last("a@example.com")); !ok {
		t.Fatal("再启动的 Worker 应使用 IP 池")
	}
	if countSourceLogs(admin.requests, "service", "账户改为跟随默认代理 | 账户=2 | 运行中的 Worker=1") != 1 {
		t.Fatal("缺少跟随默认代理的日志")
	}
}

// 复查 2-1：IP 池开启时新账户对话框留空“测试代理”测此刻会分给登录的池出口（不占用），带端口号或临时会话号；
// IP 池关闭时仍测默认代理（为空时报 proxy_required）
func TestProxyPoolTestBlankProxy(t *testing.T) {
	probe := &poolProbe{}
	manager, _ := newProxyTestManager(t, "", &recordingProxyProbe{answer: probe.probe})
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	manager.proxyPool = ipPool
	ipPool.configure(staticProxyPool(10001, 10003), "", 2)
	held := ipPool.acquire("a@example.com", "a@example.com", proxyPoolKindWorker, "a", "")
	ctx := context.Background()
	result, err := manager.TestProxy(ctx, api.ProxyTestInput{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Slot != "10002" || result.Proxy != "http://isp.example.com:10002" || !result.Relayed || result.Browser.IP == "" {
		t.Fatalf("留空应测此刻会分给登录的端口 10002（10001 已被占用）：%+v", result)
	}
	if !slices.Contains(probe.proxies, staticPortURL(t, 10002)) {
		t.Fatal("后台请求路径应直连分给登录的端口")
	}
	if status := ipPool.status(); status.Leases != 1 || status.InUse != 1 {
		t.Fatalf("测试不应占用端口：%+v", status)
	}
	if countSourceLogs(manager.requests, "service", "代理测试 | IP 池=静态 | 端口=10002") != 1 {
		t.Fatal("缺少留空测试的日志")
	}
	assertNoPoolSecret(t, "留空测试结果", mustJSON(t, result))
	held.release()

	ipPool.configure(dynamicProxyPool(), "", 2)
	result, err = manager.TestProxy(ctx, api.ProxyTestInput{Proxy: "  "})
	if err != nil {
		t.Fatal(err)
	}
	direct := slices.ContainsFunc(probe.proxies, func(proxy string) bool { return strings.Contains(proxy, "sessid-"+result.Slot+":") })
	if !regexp.MustCompile(`^n[0-9a-f]{11}$`).MatchString(result.Slot) || !direct {
		t.Fatalf("动态留空应测一个新的临时会话号：%+v", result)
	}
	assertNoPoolSecret(t, "动态留空测试结果", mustJSON(t, result))

	ipPool.configure(config.ProxyPool{Mode: config.ProxyPoolOff, URL: proxyPoolTestStaticURL}, "", 2)
	_, err = manager.TestProxy(ctx, api.ProxyTestInput{})
	assertAdminError(t, err, 400, "proxy_required")
	assertNoPoolSecretInLogs(t, manager.requests)
}

// poolTestStorageState 返回能通过签名检查的认证状态（与 proxyRecordingLogin 相同）
func poolTestStorageState() aistudio.StorageState {
	cookies := make([]aistudio.StateCookie, 0, 3)
	for _, name := range []string{"SAPISID", "__Secure-1PAPISID", "__Secure-3PAPISID"} {
		cookies = append(cookies, aistudio.StateCookie{
			Name: name, Value: "test", Domain: ".google.com", Path: "/", Expires: -1, Secure: true, SameSite: "None",
		})
	}
	return aistudio.StorageState{Cookies: cookies, Origins: []aistudio.StorageOrigin{}}
}

// 复查 1-1：用已存在的邮箱再添加账户（浏览器登录、Chrome 导入）时失败，已有账户的端口、会话号与绑定都不变；
// 新账户才采用登录或导入用的出口
func TestProxyPoolAdoptOnlyNewAccounts(t *testing.T) {
	isolateDisableNotes(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	admin, _, _ := enableTestAdmin(t, "a@example.com")
	ipPool, _ := newTestProxyPool(t, newProxyPoolTestClock(), "", "")
	ipPool.configure(staticProxyPool(10001, 10004), "", 2)
	ipPool.attach(admin.pool)
	admin.workers.proxyPool = ipPool
	launches := newPoolLaunchRecorder()
	admin.workers.launch = launches.launch
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := admin.workers.promote(ctx, "a@example.com", testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	affinity := func(id string) int {
		ipPool.mu.Lock()
		defer ipPool.mu.Unlock()
		return ipPool.affinity[id].port
	}
	before := affinity("a@example.com")
	account, _ := admin.pool.Account("a@example.com")
	login := &poolLogin{proxyRecordingLogin: proxyRecordingLogin{email: "a@example.com"}}
	admin.login = login
	if _, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{}); err == nil {
		t.Fatal("用已存在的邮箱添加应失败")
	}
	if loginPort, _ := staticProxyPool(10001, 10004).HostMatches(login.recorded()[0]); loginPort == before {
		t.Fatal("测试前提：登录应分到与 Worker 不同的端口")
	}
	if after := affinity("a@example.com"); after != before {
		t.Fatalf("重复添加不应改掉已有账户的端口：%d → %d", before, after)
	}
	if binding, ok := account.CurrentProxyBinding(); !ok || binding.Slot != strconv.Itoa(before) || !binding.Active {
		t.Fatalf("已有账户的绑定应仍是 Worker 的出口：%+v", binding)
	}
	if status := ipPool.status(); len(status.Assignments) != 1 || status.Assignments[0].Kind != proxyPoolKindWorker {
		t.Fatalf("登录租约应已归还：%+v", status.Assignments)
	}
	if err := admin.workers.Reset("a@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.workers.promote(ctx, "a@example.com", testRuntimeModel); err != nil {
		t.Fatal(err)
	}
	if launched := launches.last("a@example.com"); launched != staticPortURL(t, before) {
		t.Fatalf("重启后的 Worker 应回到原端口 %d：%q", before, config.RedactProxy(launched))
	}
	if err := admin.workers.Reset("a@example.com"); err != nil {
		t.Fatal(err)
	}

	// 动态：重复添加（浏览器登录与 Chrome 导入）不改已有账户的会话号；新导入的账户采用导入用的临时会话号
	ipPool.configure(dynamicProxyPool(), "", 2)
	session := func(id string) (string, bool) {
		ipPool.mu.Lock()
		defer ipPool.mu.Unlock()
		_, adopted := ipPool.sessions[id]
		return ipPool.sessionLocked(id), adopted
	}
	sessionBefore, _ := session("a@example.com")
	if _, err := admin.CreateAccount(context.Background(), api.AccountCreateInput{}); err == nil {
		t.Fatal("用已存在的邮箱添加应失败")
	}
	var imports []chromeauth.ImportOptions
	var importMu sync.Mutex
	previousImport, previousRoot := chromeImport, chromeImportRoot
	chromeImport = func(_ context.Context, options chromeauth.ImportOptions) ([]chromeauth.ImportResult, error) {
		importMu.Lock()
		imports = append(imports, options)
		importMu.Unlock()
		var results []chromeauth.ImportResult
		for _, id := range options.AccountIDs {
			results = append(results, chromeauth.ImportResult{Email: strings.TrimPrefix(id, "Default/"), State: poolTestStorageState()})
		}
		return results, nil
	}
	chromeImportRoot = func() (string, error) { return "root", nil }
	t.Cleanup(func() { chromeImport, chromeImportRoot = previousImport, previousRoot })
	if _, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{AccountIDs: []string{"Default/a@example.com"}}); err == nil {
		t.Fatal("导入已存在的账号应失败")
	}
	if after, adopted := session("a@example.com"); after != sessionBefore || adopted {
		t.Fatalf("重复添加不应改掉已有账户的会话号：%s → %s（adopted=%v）", sessionBefore, after, adopted)
	}
	if status := ipPool.status(); len(status.Assignments) != 0 {
		t.Fatalf("导入失败后租约应全部归还：%+v", status.Assignments)
	}
	created, err := admin.ImportChromeAccounts(context.Background(), api.ChromeImportInput{AccountIDs: []string{"Default/z@example.com"}})
	if err != nil || len(created) != 1 {
		t.Fatalf("导入新账号：%+v（%v）", created, err)
	}
	zSession, adopted := session("z@example.com")
	if !adopted || !strings.HasPrefix(zSession, "n") || !strings.Contains(imports[len(imports)-1].Proxy, "sessid-"+zSession+":") {
		t.Fatalf("新导入的账户应采用导入用的会话号：%s（adopted=%v）", zSession, adopted)
	}
	if status := ipPool.status(); len(status.Assignments) != 0 {
		t.Fatalf("导入完成后租约应全部归还：%+v", status.Assignments)
	}
	assertNoPoolSecretInLogs(t, admin.requests)
}
