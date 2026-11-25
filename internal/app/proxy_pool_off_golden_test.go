package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// IP 池固定分配（PROXY_POOL_FIXED）默认关闭时与原行为逐字节一致的金样：一个固定的 IP 池场景（静态端口段与动态会话、亲和、端口不足共用、
// 新账户登录与整批导入、规则 A/B 冷却、整体故障保护、其他进程锁住与探测释放、外部占用、热更新端口段/PROXY/容量、预览、状态、换会话）
// 每一步记下规范化日志、各账户绑定（脱敏）、租约端口、状态接口 JSON、runtime 记录文件文本（去掉 saved_at）与固定分配文件是否存在，
// 与 testdata/proxy_pool_off_golden.json 逐字节比较。金样在加入固定分配之前的代码上用 -update 生成，之后每一次改动都必须让它原样通过

const proxyPoolOffGoldenFile = "proxy_pool_off_golden.json"

// proxyPoolGoldenStep 为场景的一步：Logs 为这一步新增的日志行
type proxyPoolGoldenStep struct {
	Name      string          `json:"name"`
	Logs      []string        `json:"logs"`
	Bindings  []string        `json:"bindings"`
	Leases    []string        `json:"leases"`
	Extra     []string        `json:"extra,omitempty"`
	Status    json.RawMessage `json:"status"`
	Record    string          `json:"record"`
	FixedFile bool            `json:"fixed_file"`
}

// proxyPoolGoldenReader 为确定的随机源（临时会话号、登录租约键）
type proxyPoolGoldenReader struct {
	mu   sync.Mutex
	next byte
}

func (reader *proxyPoolGoldenReader) Read(buffer []byte) (int, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	for index := range buffer {
		reader.next = reader.next*31 + 7
		buffer[index] = reader.next
	}
	return len(buffer), nil
}

// proxyPoolGoldenLocks 为跨进程端口锁替身：other 里的端口被“其他进程”锁住
type proxyPoolGoldenLocks struct {
	mu    sync.Mutex
	held  map[int]bool
	other map[int]bool
	calls int
}

func (locks *proxyPoolGoldenLocks) tryLock(_, _ string, port int) (proxyPoolPortLock, bool, error) {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	locks.calls++
	if locks.other[port] {
		return nil, false, nil
	}
	locks.held[port] = true
	return &fakeProxyPortLock{mu: &locks.mu, held: locks.held, port: port}, true, nil
}

func (locks *proxyPoolGoldenLocks) setOther(port int, locked bool) {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	if locked {
		locks.other[port] = true
	} else {
		delete(locks.other, port)
	}
}

func (locks *proxyPoolGoldenLocks) heldPorts() []int {
	locks.mu.Lock()
	defer locks.mu.Unlock()
	ports := make([]int, 0, len(locks.held))
	for port := range locks.held {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	return ports
}

// proxyPoolGoldenEnv 为金样场景的环境
type proxyPoolGoldenEnv struct {
	t        *testing.T
	clock    *proxyPoolTestClock
	pool     *proxyPool
	registry *requestRegistry
	owner    *fakeProxyOwner
	locks    *proxyPoolGoldenLocks
	dir      string
	leases   map[string]*proxyPoolLease
	logged   int
	steps    []proxyPoolGoldenStep
}

var proxyPoolGoldenSavedAt = regexp.MustCompile(`(?m)^\s*"saved_at": "[^"]*",?\n`)

// newProxyPoolGoldenEnv 建一个带记录文件、假端口锁、假时钟与确定随机源的 IP 池
func newProxyPoolGoldenEnv(t *testing.T, dir string) *proxyPoolGoldenEnv {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registry := newRequestRegistry(ctx)
	clock := newProxyPoolTestClock()
	locks := &proxyPoolGoldenLocks{held: make(map[int]bool), other: make(map[int]bool)}
	pool := newProxyPool(proxyPoolOptions{
		path: filepath.Join(dir, proxyPoolFileName), lockDir: "locks", requests: registry, now: clock.Now,
		random: &proxyPoolGoldenReader{}, tryLock: locks.tryLock, relayLimit: func() int { return 1 << 20 },
	})
	t.Cleanup(func() {
		pool.mu.Lock()
		if pool.recheckTimer != nil {
			pool.recheckTimer.Stop()
		}
		pool.mu.Unlock()
		pool.flush()
	})
	return &proxyPoolGoldenEnv{
		t: t, clock: clock, pool: pool, registry: registry, locks: locks, dir: dir,
		leases: make(map[string]*proxyPoolLease),
	}
}

func (env *proxyPoolGoldenEnv) acquire(name, key, accountID, kind string) *proxyPoolLease {
	lease := env.pool.acquire(key, accountID, kind, accountID, proxyPoolTestGlobal)
	env.leases[name] = lease
	return lease
}

func (env *proxyPoolGoldenEnv) release(name string) {
	env.leases[name].release()
	delete(env.leases, name)
}

func (env *proxyPoolGoldenEnv) launchFailed(name string, cause error) {
	env.leases[name].launchFailed(cause, false)
	delete(env.leases, name)
}

// record 记下这一步的可观察输出
func (env *proxyPoolGoldenEnv) record(name string, extra ...string) {
	t := env.t
	t.Helper()
	statusJSON := []byte("null")
	if status := env.pool.status(); status != nil {
		data, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		statusJSON = data
	}
	env.pool.flush()
	step := proxyPoolGoldenStep{Name: name, Status: statusJSON, Extra: extra}
	lines := proxyPoolLogLines(env.registry)
	step.Logs = append([]string{}, lines[env.logged:]...)
	env.logged = len(lines)
	if env.owner != nil {
		ids := env.owner.AccountIDs()
		sort.Strings(ids)
		for _, id := range ids {
			binding := env.owner.binding(id)
			if binding == nil {
				step.Bindings = append(step.Bindings, id+" nil")
				continue
			}
			step.Bindings = append(step.Bindings, fmt.Sprintf("%s url=%s slot=%s active=%v tag=%s effective=%s",
				id, config.RedactProxy(binding.URL), binding.Slot, binding.Active, binding.EgressTag,
				config.RedactProxy(env.owner.effective(id, proxyPoolTestGlobal))))
		}
	}
	names := make([]string, 0, len(env.leases))
	for leaseName := range env.leases {
		names = append(names, leaseName)
	}
	sort.Strings(names)
	for _, leaseName := range names {
		lease := env.leases[leaseName]
		env.pool.mu.Lock()
		line := fmt.Sprintf("%s login=%v account=%s kind=%s port=%d slot=%s pooled=%v shared=%v attached=%v released=%v url=%s",
			leaseName, strings.HasPrefix(lease.key, "login:"), lease.accountID, lease.kind, lease.port, lease.slot, lease.pooled,
			lease.shared, lease.attached, lease.released, config.RedactProxy(lease.url))
		env.pool.mu.Unlock()
		step.Leases = append(step.Leases, line)
	}
	step.Extra = append(step.Extra, fmt.Sprintf("held=%v", env.locks.heldPorts()))
	if data, err := os.ReadFile(filepath.Join(env.dir, proxyPoolFileName)); err == nil {
		step.Record = proxyPoolGoldenSavedAt.ReplaceAllString(string(data), "")
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "proxy-pool-fixed.json")); err == nil {
		step.FixedFile = true
	}
	env.steps = append(env.steps, step)
}

// runProxyPoolOffScenario 跑一遍金样场景，返回编码后的 JSON
func runProxyPoolOffScenario(t *testing.T) []byte {
	t.Helper()
	return runProxyPoolOffScenarioIn(t, nil)
}

// runProxyPoolOffScenarioIn 与 runProxyPoolOffScenario 相同，seed 不为 nil 时在建 IP 池之前往 runtime 目录里放文件（休眠的固定表）
func runProxyPoolOffScenarioIn(t *testing.T, seed func(dir string)) []byte {
	t.Helper()
	dir := t.TempDir()
	if seed != nil {
		seed(dir)
	}
	env := newProxyPoolGoldenEnv(t, dir)
	pool, clock, locks := env.pool, env.clock, env.locks
	const own = "http://203.0.113.50:3128"
	ids := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com"}
	pinnedID, ownID := "pinned@example.com", "own@example.com"

	pool.configure(staticProxyPool(10001, 10006), proxyPoolTestGlobal, 6)
	env.record("configure static")

	// 还没进账户池的账户先 prime（与 newRuntime 相同），之后 attach
	primed := &aistudio.Account{ID: ids[0], Config: aistudio.AccountConfig{Enabled: true}}
	pool.prime(primed)
	primedBinding, _ := primed.CurrentProxyBinding()
	owner := newFakeProxyOwner(ids...)
	owner.add(ownID, own)
	owner.add(pinnedID, proxyPoolTestGlobal)
	env.owner = owner
	pool.attach(owner)
	env.record("attach", "primed="+config.RedactProxy(primedBinding.URL)+" slot="+primedBinding.Slot,
		fmt.Sprintf("poolsAccount a=%v own=%v pinned=%v", pool.poolsAccount(ids[0]), pool.poolsAccount(ownID), pool.poolsAccount(pinnedID)),
		fmt.Sprintf("pooledAccounts=%v", proxyPoolGoldenSet(pool.pooledAccounts(append(slices.Clone(ids), ownID, pinnedID)))))

	// Worker 申请、同键共用、启动成功、登录跟随、归还
	closers := make(map[string]*fakeProxyCloser)
	for _, id := range ids[:3] {
		lease := env.acquire("worker:"+id, id, id, proxyPoolKindWorker)
		closers[id] = &fakeProxyCloser{}
		lease.attach(closers[id])
	}
	env.acquire("verify:a", ids[0], ids[0], proxyPoolKindVerify)
	env.acquire("login:b", ids[1], ids[1], proxyPoolKindLogin)
	env.record("workers and same key",
		"slot a="+pool.slotLabel(ids[0])+" b="+pool.slotLabel(ids[1])+" d="+pool.slotLabel(ids[3]),
		fmt.Sprintf("servesOnPort a=%v", pool.servesOnPort(ids[0], leasePortLocked(env.leases["worker:"+ids[0]]))),
		"logField a="+env.leases["worker:"+ids[0]].logField())
	env.release("verify:a")
	env.release("login:b")
	closers[ids[0]].close()
	delete(env.leases, "worker:"+ids[0])
	clock.Advance(time.Minute)
	env.record("release a")

	// 亲和：a 回到原端口
	env.acquire("worker:"+ids[0], ids[0], ids[0], proxyPoolKindWorker).attach(&fakeProxyCloser{})
	env.record("affinity a")

	// 端口不足共用：再开 d、e 与两个没有账户的登录、一个导入
	env.acquire("worker:"+ids[3], ids[3], ids[3], proxyPoolKindWorker)
	env.acquire("worker:"+ids[4], ids[4], ids[4], proxyPoolKindWorker)
	env.acquire("login:new1", "login:new1", "", proxyPoolKindLogin)
	env.acquire("login:new2", "login:new2", "", proxyPoolKindLogin)
	env.record("shortage")

	// 新账户登录 adopt、整批导入 claim adopt
	owner.add("new1@example.com", "")
	env.leases["login:new1"].adopt("new1@example.com")
	pool.refresh(owner, "new1@example.com")
	env.release("login:new1")
	env.release("login:new2")
	env.release("worker:" + ids[4])
	env.release("worker:" + ids[3])
	var claims []*proxyPoolClaim
	for index := range 3 {
		lease := env.acquire(fmt.Sprintf("import:%d", index), fmt.Sprintf("login:imp%d", index), "", proxyPoolKindImport)
		claims = append(claims, lease.releaseClaim())
		delete(env.leases, fmt.Sprintf("import:%d", index))
	}
	for index, claim := range claims {
		id := fmt.Sprintf("imp%d@example.com", index)
		owner.add(id, "")
		claim.adopt(id)
		pool.refresh(owner, id)
	}
	env.record("login adopt and import claims", fmt.Sprintf("claims=%d", len(claims)))

	// 规则 A：b、c 在同一端口上返回裸限流、其中一个近期在别处成功过
	portB := leasePortLocked(env.leases["worker:"+ids[1]])
	env.acquire("worker:"+ids[2]+":2", ids[2], ids[2], proxyPoolKindWorker)
	markProxyPoolProven(pool, ids[1])
	pool.rateLimited(ids[1], false)
	pool.mu.Lock()
	pool.ports[portB].failures = append(pool.ports[portB].failures, proxyPoolFailure{account: ids[2], at: pool.now(), proven: false})
	pool.mu.Unlock()
	pool.rateLimited(ids[1], false)
	pool.succeeded(ids[0])
	env.record("rule A", fmt.Sprintf("portB=%d", portB))

	// 规则 B：同一端口连续两次上游失败
	failing := env.acquire("worker:fail1", ids[3], ids[3], proxyPoolKindWorker)
	failPort := leasePortLocked(failing)
	env.launchFailed("worker:fail1", upstreamFailure)
	second := env.acquire("worker:fail2", ids[3], ids[3], proxyPoolKindWorker)
	secondPort := leasePortLocked(second)
	env.launchFailed("worker:fail2", upstreamFailure)
	clock.Advance(time.Minute)
	env.record("rule B", fmt.Sprintf("failPort=%d secondPort=%d", failPort, secondPort))

	// 整体故障保护：多数端口出错时不再冷却
	pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		pool.portLocked(port).upstreamFails = 1
	}
	pool.mu.Unlock()
	systemic := env.acquire("worker:sys", ids[3], ids[3], proxyPoolKindWorker)
	env.launchFailed("worker:sys", upstreamFailure)
	_ = systemic
	pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		pool.portLocked(port).upstreamFails = 0
	}
	pool.mu.Unlock()
	env.record("systemic protection")

	// 其他进程锁住全部空闲端口：申请锁不到、记下重试时间并共用；到重试时间后探测到释放
	var lockedOut []int
	pool.mu.Lock()
	for port := 10001; port <= 10006; port++ {
		if state := pool.ports[port]; state == nil || len(state.holders) == 0 {
			lockedOut = append(lockedOut, port)
		}
	}
	pool.mu.Unlock()
	for _, port := range lockedOut {
		locks.setOther(port, true)
	}
	env.acquire("worker:"+ids[3], ids[3], ids[3], proxyPoolKindWorker)
	env.acquire("worker:"+ids[4], ids[4], ids[4], proxyPoolKindWorker)
	env.record("locked elsewhere", fmt.Sprintf("lockedOut=%v", lockedOut))
	for _, port := range lockedOut[:len(lockedOut)/2] {
		locks.setOther(port, false)
	}
	clock.Advance(proxyPoolLockedRetry + time.Second)
	pool.recheckFired()
	env.record("probe released")
	for _, port := range lockedOut {
		locks.setOther(port, false)
	}
	clock.Advance(proxyPoolLockedRetry + time.Second)
	pool.recheckFired()
	env.release("worker:" + ids[4])
	env.record("probe released 2")

	// 外部占用：PROXY 指向段内端口；之后换回
	pool.configure(staticProxyPool(10001, 10006), "http://isp.example.com:10004", 6)
	env.record("external PROXY")
	pool.configure(staticProxyPool(10001, 10006), proxyPoolTestGlobal, 6)
	env.record("PROXY restored")

	// 热更新：端口段、容量
	pool.configure(staticProxyPool(10003, 10008), proxyPoolTestGlobal, 6)
	env.record("range moved")
	pool.setCapacity(12)
	env.record("capacity up")
	pool.configure(staticProxyPool(10003, 10008), proxyPoolTestGlobal, 4)
	env.record("capacity down")
	clock.Advance(3 * time.Hour)

	// 预览、跟随默认代理补登记、pinned 标记
	loginPreview, loginOK := pool.loginPreview()
	accountPreview, accountOK := pool.accountPreview(ids[3])
	ownPreview, ownOK := pool.accountPreview(ownID)
	passthrough := pool.passthrough(pinnedID, pinnedID, proxyPoolKindWorker, pinnedID, proxyPoolTestGlobal)
	env.leases["passthrough:pinned"] = passthrough
	accounts := []api.AdminAccount{{ID: ids[0]}, {ID: ownID}, {ID: pinnedID}, {ID: ids[3]}}
	pool.markAccounts(accounts)
	accountsJSON, _ := json.Marshal(accounts)
	env.record("previews",
		fmt.Sprintf("login=%v slot=%s url=%s", loginOK, loginPreview.slot, config.RedactProxy(loginPreview.url)),
		fmt.Sprintf("account=%v slot=%s url=%s", accountOK, accountPreview.slot, config.RedactProxy(accountPreview.url)),
		fmt.Sprintf("own=%v slot=%s", ownOK, ownPreview.slot),
		fmt.Sprintf("pinnedDefault=%v", pool.pinnedDefault()),
		"accounts="+string(accountsJSON))
	env.release("passthrough:pinned")

	// 账户改了自己的代理、账户离开账户池
	owner.add(ids[4], own)
	pool.refresh(owner, ids[4])
	owner.remove("imp2@example.com")
	pool.attach(owner)
	env.record("own proxy and removed")

	// 全部归还
	names := make([]string, 0, len(env.leases))
	for name := range env.leases {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env.release(name)
	}
	for _, closer := range closers {
		closer.close()
	}
	clock.Advance(proxyPoolWakeDelay + time.Second)
	pool.recheckFired()
	env.record("all released")

	// 动态：会话、登录临时会话 adopt、连续上游失败换会话、出口判定换会话
	pool.configure(dynamicProxyPool(), proxyPoolTestGlobal, 6)
	env.record("dynamic")
	env.acquire("worker:"+ids[0], ids[0], ids[0], proxyPoolKindWorker).attach(&fakeProxyCloser{})
	login := env.acquire("login:dyn", "login:dyn", "", proxyPoolKindLogin)
	owner.add("dyn@example.com", "")
	login.adopt("dyn@example.com")
	pool.refresh(owner, "dyn@example.com")
	env.release("login:dyn")
	env.record("dynamic leases")
	env.acquire("worker:"+ids[1], ids[1], ids[1], proxyPoolKindWorker)
	env.launchFailed("worker:"+ids[1], upstreamFailure)
	clock.Advance(time.Minute)
	env.acquire("worker:"+ids[1], ids[1], ids[1], proxyPoolKindWorker)
	env.launchFailed("worker:"+ids[1], upstreamFailure)
	env.record("dynamic upstream rotation")
	clock.Advance(proxyPoolRotateInterval + time.Minute)
	rotated := rotateSessionForTest(pool, ids[2], "出口受限")
	env.record("dynamic egress rotation", fmt.Sprintf("rotated=%v", rotated))
	env.release("worker:" + ids[0])
	pool.configure(config.DefaultProxyPool(), proxyPoolTestGlobal, 6)
	env.record("off")

	data, err := json.MarshalIndent(env.steps, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	assertNoPoolSecret(t, "金样", text)
	return append(data, '\n')
}

func proxyPoolGoldenSet(set map[string]struct{}) []string {
	ids := make([]string, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// TestProxyPoolOffGolden 固定分配关闭（默认）时，IP 池场景的全部可观察输出与加入固定分配之前的代码逐字节相同
func TestProxyPoolOffGolden(t *testing.T) {
	path := filepath.Join("testdata", proxyPoolOffGoldenFile)
	got := runProxyPoolOffScenario(t)
	if again := runProxyPoolOffScenario(t); string(again) != string(got) {
		t.Fatalf("同一场景跑两遍结果不同：%s", goldenDiff(got, again))
	}
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取金样失败（先用 -update 生成）: %v", err)
	}
	if string(want) != string(got) {
		t.Fatalf("与金样不同：%s", goldenDiff(want, got))
	}
}
