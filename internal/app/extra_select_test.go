package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池的请求侧（见 extra_select.go 批 3）：借用通道、一起分担与只在不够时补充、只能由补充池服务的请求、模型列表与别名、count_tokens 覆盖、
// 跨号池借用的守卫键与重放、请求日志与排队快照的标记

const (
	// borrowPaidModel 为付费模型：只有 Pro 与 Ultra 账户的目录里有
	borrowPaidModel = "gemini-4-argon"
	// borrowUltraModel 为 Ultra 独占模型：只有 Ultra 账户的目录里有（测试里按 ULTRA_ONLY_MODELS 设置）
	borrowUltraModel = "gemini-4-carbon"
	// borrowFreeModel 只在普通账户的目录里有（/pro 的模型列表不应因补充池多出它）
	borrowFreeModel = "extra-free-only-model"
)

// borrowChat 返回测试目录里的文本对话模型
func borrowChat(id string) aistudio.Model {
	return aistudio.Model{ID: id, Name: id, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}}
}

// borrowCall 为借用测试上游收到的一次生成调用：账户、模型与调用时请求 context 上的号池
type borrowCall struct {
	account string
	model   string
	scope   aistudio.PoolScope
}

// borrowUpstream 为借用测试的上游：按顺序记下每次生成调用；fail 中的账户按队列返回错误，failFirst 让最先的几次调用（不论账户）返回错误；
// before 在返回之前执行（模拟发送时 Worker 不在了要重开等），hold 中的账户在通道关闭前不返回（让账户保持忙碌）
type borrowUpstream struct {
	mu        sync.Mutex
	calls     []borrowCall
	fail      map[string][]error
	failFirst []error
	before    map[string]func(ctx context.Context, request aistudio.GenerateRequest) error
	count     func(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error)
}

func (upstream *borrowUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *borrowUpstream) CountTokens(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	upstream.mu.Lock()
	count := upstream.count
	upstream.mu.Unlock()
	if count == nil {
		return aistudio.TokenCount{}, nil
	}
	return count(ctx, request)
}

func (upstream *borrowUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		lease.RecordGenerationSent()
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, borrowCall{
		account: request.AccountID, model: strings.TrimPrefix(request.Model, "models/"), scope: aistudio.PoolScopeFromContext(ctx),
	})
	var failure error
	if len(upstream.failFirst) > 0 {
		failure, upstream.failFirst = upstream.failFirst[0], upstream.failFirst[1:]
	} else if queue := upstream.fail[request.AccountID]; len(queue) > 0 {
		failure, upstream.fail[request.AccountID] = queue[0], queue[1:]
	}
	before := upstream.before[request.AccountID]
	upstream.mu.Unlock()
	if before != nil {
		if err := before(ctx, request); err != nil {
			return nil, err
		}
	}
	if failure != nil {
		return nil, failure
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (upstream *borrowUpstream) called() []borrowCall {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// accounts 返回 from 之后各次调用的账户
func (upstream *borrowUpstream) accounts(from int) []string {
	calls := upstream.called()
	result := make([]string, 0, len(calls))
	for _, call := range calls[min(from, len(calls)):] {
		result = append(result, call.account)
	}
	return result
}

func (upstream *borrowUpstream) failAccount(accountID string, errs ...error) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.fail == nil {
		upstream.fail = make(map[string][]error)
	}
	upstream.fail[accountID] = append(upstream.fail[accountID], errs...)
}

func (upstream *borrowUpstream) beforeAccount(accountID string, hook func(ctx context.Context, request aistudio.GenerateRequest) error) {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.before == nil {
		upstream.before = make(map[string]func(ctx context.Context, request aistudio.GenerateRequest) error)
	}
	upstream.before[accountID] = hook
}

// borrowEnv 为借用测试的环境：普通（Free）、Ultra 与 Pro 账户（PRO_POOL=true），纯 Go 后端（启动返回 stub Worker），补充池开启
// （常驻、峰值按参数）。普通账户的目录为 testRuntimeModel、extraCoolModel 与 borrowFreeModel；Pro 账户另有 borrowPaidModel；
// Ultra 账户再有 borrowUltraModel（ULTRA_ONLY_MODELS）。各分区的峰值都比账户数多 1，常驻为 0（测试自己决定启动谁）
type borrowEnv struct {
	*extraEnv
	upstream *borrowUpstream
}

func newBorrowEnv(t *testing.T, normalIDs []string, ultraIDs []string, proIDs []string, extraWarm, extraMax int) *borrowEnv {
	t.Helper()
	manager, pool, requests := proPartitionManager(t, normalIDs, ultraIDs, proIDs, 0, len(normalIDs)+1, 0, len(proIDs)+1)
	manager.setUltraCapacity(0, len(ultraIDs)+1)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	env := &borrowEnv{
		extraEnv: &extraEnv{ctx: ctx, manager: manager, pool: pool, requests: requests, fail: make(map[string]error)},
		upstream: &borrowUpstream{},
	}
	manager.launch = func(launchCtx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		env.mu.Lock()
		env.launched = append(env.launched, accountID)
		gate := env.gate
		env.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-launchCtx.Done():
				return nil, launchCtx.Err()
			}
		}
		worker, closed := aistudio.NewStubWorker(accountID, 0)
		env.mu.Lock()
		env.closed = append(env.closed, closed)
		env.mu.Unlock()
		return worker, nil
	}
	free := []aistudio.Model{borrowChat(testRuntimeModel), borrowChat(extraCoolModel), borrowChat(borrowFreeModel)}
	paid := []aistudio.Model{borrowChat(testRuntimeModel), borrowChat(extraCoolModel), borrowChat(borrowPaidModel)}
	for _, accountID := range normalIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, free); err != nil {
			t.Fatal(err)
		}
	}
	for _, accountID := range proIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierPro, paid); err != nil {
			t.Fatal(err)
		}
	}
	for _, accountID := range ultraIDs {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierUltra, append(slices.Clone(paid), borrowChat(borrowUltraModel))); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetModelTierRules([]string{borrowUltraModel}, nil)
	env.service = retireTestService(ctx, manager, pool, requests)
	env.service.service = env.upstream
	env.service.timeout.Store(int64(time.Minute))
	env.service.rateLimits = newRateLimitGuard()
	// 经公开 API 处理链与 count_tokens 的请求要求服务在运行
	env.service.state.Store(serviceRunning)
	env.service.dataContext = ctx
	env.service.replaceModelSnapshot([]aistudio.Model{
		borrowChat(testRuntimeModel), borrowChat(extraCoolModel), borrowChat(borrowFreeModel), borrowChat(borrowPaidModel), borrowChat(borrowUltraModel),
	})
	env.configure(config.ExtraPoolModeShare, true, false, extraWarm, extraMax)
	return env
}

// configure 写入补充池设置与账户池的补充池规则（与热更新相同）
func (env *borrowEnv) configure(mode string, defaultPaid bool, includeUltra bool, warm, maxActive int) {
	env.manager.setExtraSettings(extraPoolSettings{
		enabled: true, mode: mode, defaultPaid: defaultPaid, includeUltra: includeUltra, warm: warm, max: maxActive,
	})
	env.pool.SetExtraPoolRules(aistudio.ExtraPoolRules{Enabled: true, DefaultPaid: defaultPaid, IncludeUltra: includeUltra})
}

// disable 关闭补充池
func (env *borrowEnv) disable() {
	env.manager.setExtraSettings(extraPoolSettings{mode: config.ExtraPoolModeShare})
	env.pool.SetExtraPoolRules(aistudio.ExtraPoolRules{})
}

// cool 让账户成为补充池成员：extraCoolModel 冷却 2 小时（目录里其他模型照常能用）
func (env *borrowEnv) cool(t *testing.T, accountIDs ...string) {
	t.Helper()
	for _, accountID := range accountIDs {
		coolModel(t, env.pool, accountID, extraCoolModel, 2*time.Hour, "每日限额")
	}
}

// lend 让成员按补充池预热启动，并核对补充池 Worker 正好是 want
func (env *borrowEnv) lend(t *testing.T, want ...string) {
	t.Helper()
	env.pass(t)
	slices.Sort(want)
	if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, want) {
		t.Fatalf("补充池 Worker = %v，期望 %v", got, want)
	}
}

// occupy 占满账户的并发（每账户 2 个），让它按忙碌分组；返回释放函数
func occupy(t *testing.T, pool *aistudio.AccountPool, accountID string) func() {
	t.Helper()
	first, second := holdLease(t, pool, accountID), holdLease(t, pool, accountID)
	return func() {
		first()
		second()
	}
}

// generate 在入口 scope 上请求一次模型（期限 within），返回服务它的账户（失败时为空）与错误
func (env *borrowEnv) generate(t *testing.T, scope aistudio.PoolScope, model string, within time.Duration) (string, error) {
	t.Helper()
	return env.generateCtx(t, aistudio.ContextWithPoolScope(env.ctx, scope), model, within, "")
}

var borrowRequestSeq struct {
	mu   sync.Mutex
	next int
}

// generateCtx 与 generate 相同，parent 为请求的 context（已写入号池），id 为空时自动编号
func (env *borrowEnv) generateCtx(t *testing.T, parent context.Context, model string, within time.Duration, id string) (string, error) {
	t.Helper()
	if id == "" {
		borrowRequestSeq.mu.Lock()
		borrowRequestSeq.next++
		id = fmt.Sprintf("req-borrow-%d", borrowRequestSeq.next)
		borrowRequestSeq.mu.Unlock()
	}
	request := aistudio.GenerateRequest{
		ID: id, Model: model, Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(parent, within)
	defer requestCancel()
	before := len(env.upstream.called())
	events := make(chan aistudio.Event, 8)
	env.service.requests.start(parent, request, requestCancel)
	go env.service.generateWithRetry(parent, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	text := ""
	var failure error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			failure = event.Err
		}
	}
	if failure != nil || text == "" {
		if failure == nil {
			failure = errors.New("没有正文")
		}
		return "", failure
	}
	accounts := env.upstream.accounts(before)
	if len(accounts) == 0 {
		return "", errors.New("上游没有收到请求")
	}
	return accounts[len(accounts)-1], nil
}

// acquire 在入口 scope 上按借用计划做一次选号（与生成请求相同的选号上下文），返回租约的账户；租约随即释放
func (env *borrowEnv) acquire(t *testing.T, ctx context.Context, model string, within time.Duration) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	scope := aistudio.PoolScopeFromContext(ctx)
	ctx = withExtraPlan(ctx, env.service.newExtraPlan(ctx, scope, model, true))
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: model, Method: "generateContent", Pool: scope})
	if err != nil {
		return "", err
	}
	accountID := lease.Account().ID
	_ = lease.Release()
	return accountID, nil
}

// TestExtraLendsOnlyMembers 只借出可借出的补充池 Worker（L）：默认地址请求、普通号池全忙时 Pro 成员的补充池 Worker 可借；健康的 Pro Worker
// （在 Pro 分区，不是成员）与冷却已结束、等待归还的补充池 Worker 都不借
func TestExtraLendsOnlyMembers(t *testing.T) {
	normal, lent, healthy, recovered := "lend-normal@example.com", "lend-pro-a@example.com", "lend-pro-b@example.com", "lend-pro-c@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent, healthy, recovered}, 2, 3)
	env.manager.setProCapacity(0, 1)
	env.cool(t, lent, recovered)
	env.promote(t, normal, healthy)
	env.lend(t, lent, recovered)
	// 冷却结束、家分区已满：等待归还或释放（对账时它正忙，没有被释放）
	env.recover(t, recovered)
	release := holdLease(t, env.pool, recovered)
	env.service.extraPass(env.ctx)
	release()
	if view := env.manager.extraView.Load(); view == nil || !hasKey(view.returning, recovered) {
		t.Fatal("冷却已结束、家分区已满的补充池 Worker 应等待归还")
	}
	if got := env.manager.extraLendableIDs(); !slices.Equal(got, []string{lent}) {
		t.Fatalf("可借出的补充池 Worker = %v，期望只有仍是成员的 %s", got, lent)
	}
	releaseNormal := occupy(t, env.pool, normal)
	defer releaseNormal()
	if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second); err != nil || account != lent {
		t.Fatalf("普通号池全忙：服务账户 %q、错误 %v，期望借补充池的 %s", account, err, lent)
	}
	releaseLent := occupy(t, env.pool, lent)
	defer releaseLent()
	before := len(env.upstream.called())
	if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 1500*time.Millisecond); err == nil {
		t.Fatalf("可借出的补充池 Worker 也忙时应等待，却由 %s 服务", account)
	}
	for _, accountID := range env.upstream.accounts(before) {
		if accountID == healthy || accountID == recovered {
			t.Fatalf("不应借出健康的 Pro Worker 或等待归还的补充池 Worker：%s", accountID)
		}
	}
}

// TestExtraShareRotation 一起分担：主号池 2 个空闲、补充池 3 个空闲时，连续 5 个请求两边分别取到 2 次与 3 次（计数器从 0 开始）；
// 优先填满时固定主号池在前；平均调用开启时两边都先取没有进行中请求的 Worker
func TestExtraShareRotation(t *testing.T) {
	normalIDs := []string{"share-normal-a@example.com", "share-normal-b@example.com"}
	proIDs := []string{"share-pro-a@example.com", "share-pro-b@example.com", "share-pro-c@example.com"}
	setup := func(t *testing.T) *borrowEnv {
		env := newBorrowEnv(t, normalIDs, nil, proIDs, 3, 3)
		env.cool(t, proIDs...)
		env.promote(t, normalIDs...)
		env.lend(t, proIDs...)
		return env
	}
	served := func(t *testing.T, env *borrowEnv, count int) (main int, extra int) {
		t.Helper()
		for range count {
			account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(proIDs, account) {
				extra++
			} else {
				main++
			}
		}
		return main, extra
	}
	t.Run("按空闲数轮取", func(t *testing.T) {
		env := setup(t)
		if main, extra := served(t, env, 5); main != 2 || extra != 3 {
			t.Fatalf("主号池 %d 次、补充池 %d 次，期望 2 与 3", main, extra)
		}
	})
	t.Run("优先填满", func(t *testing.T) {
		env := setup(t)
		env.pool.SetRoutingStrategy("fill-first")
		if main, extra := served(t, env, 5); main != 5 || extra != 0 {
			t.Fatalf("优先填满：主号池 %d 次、补充池 %d 次，期望主号池在前", main, extra)
		}
	})
	t.Run("平均调用先取空闲", func(t *testing.T) {
		env := setup(t)
		env.manager.balanced.Store(true)
		// 主号池两个 Worker 都有进行中的请求、补充池有完全空闲的：先借补充池
		releaseA, releaseB := holdLease(t, env.pool, normalIDs[0]), holdLease(t, env.pool, normalIDs[1])
		if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second); err != nil || !slices.Contains(proIDs, account) {
			t.Fatalf("主号池都有进行中的请求：服务账户 %q、错误 %v，期望补充池里空闲的", account, err)
		}
		releaseA()
		releaseB()
		// 补充池都有进行中的请求、主号池有完全空闲的：先用主号池
		var releases []func()
		for _, accountID := range proIDs {
			releases = append(releases, holdLease(t, env.pool, accountID))
		}
		defer func() {
			for _, release := range releases {
				release()
			}
		}()
		for range 3 {
			if account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 10*time.Second); err != nil || !slices.Contains(normalIDs, account) {
				t.Fatalf("补充池都有进行中的请求：服务账户 %q、错误 %v，期望主号池里空闲的", account, err)
			}
		}
	})
}

// TestExtraOverflowTriggers 只在主号池不够时补充（overflow）的触发矩阵（§4.5）
func TestExtraOverflowTriggers(t *testing.T) {
	normal, lent, cold := "overflow-normal@example.com", "overflow-pro-a@example.com", "overflow-pro-b@example.com"
	standby := "overflow-normal-b@example.com"
	home := "overflow-pro-home@example.com"
	type check func(t *testing.T, env *borrowEnv)
	normalCtx := func(env *borrowEnv) context.Context {
		return aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal)
	}
	expectLent := func(t *testing.T, env *borrowEnv, ctx context.Context, within time.Duration) {
		t.Helper()
		startedAt := time.Now()
		account, err := env.acquire(t, ctx, testRuntimeModel, 10*time.Second)
		if err != nil || account != lent {
			t.Fatalf("选号得到 %q、错误 %v，期望借补充池的 %s", account, err, lent)
		}
		if elapsed := time.Since(startedAt); elapsed > within {
			t.Fatalf("借用用了 %s，期望在 %s 内（立即借）", elapsed, within)
		}
	}
	for _, test := range []struct {
		name string
		// promoteNormal 为普通账户先启动 Worker（否则它是备用账户）；standby 为另有一个没有 Worker 的普通账户（普通分区有空槽）
		promoteNormal bool
		standby       bool
		run           check
	}{
		{name: "主号池有空闲时从不借", promoteNormal: true, run: func(t *testing.T, env *borrowEnv) {
			for range 4 {
				if account, err := env.acquire(t, normalCtx(env), testRuntimeModel, 10*time.Second); err != nil || account != normal {
					t.Fatalf("主号池有空闲：选号得到 %q、错误 %v", account, err)
				}
			}
		}},
		{name: "都在忙", promoteNormal: true, standby: true, run: func(t *testing.T, env *borrowEnv) {
			// 主号池的热 Worker 都忙、另有能扩容的备用账户：立即借，不先为主号池后台扩容
			release := occupy(t, env.pool, normal)
			defer release()
			launches := len(env.launches())
			expectLent(t, env, normalCtx(env), 900*time.Millisecond)
			if got := env.launches()[launches:]; slices.Contains(got, standby) {
				t.Fatalf("主号池都在忙时应立即借补充池，不应先扩容主号池：%v", got)
			}
		}},
		{name: "全部冷却", promoteNormal: true, run: func(t *testing.T, env *borrowEnv) {
			coolModelFor(t, env.pool, normal, testRuntimeModel, 2*time.Hour)
			expectLent(t, env, normalCtx(env), 900*time.Millisecond)
		}},
		{name: "额度 429 后只能用已启动的 Worker：立即借", run: func(t *testing.T, env *borrowEnv) {
			launches := len(env.launches())
			expectLent(t, env, withoutColdStart(normalCtx(env)), 900*time.Millisecond)
			if got := env.launches()[launches:]; len(got) != 0 {
				t.Fatalf("只用已启动的 Worker 不应冷启动：%v", got)
			}
		}},
		{name: "额度 429 后只能用已启动的 Worker：不扩容", promoteNormal: true, run: func(t *testing.T, env *borrowEnv) {
			releaseNormal, releaseLent := occupy(t, env.pool, normal), occupy(t, env.pool, lent)
			defer releaseNormal()
			defer releaseLent()
			env.cool(t, cold)
			env.service.extraPass(env.ctx)
			env.waitLaunches(t)
			launches := len(env.launches())
			if _, err := env.acquire(t, withoutColdStart(normalCtx(env)), testRuntimeModel, 1500*time.Millisecond); err == nil {
				t.Fatal("主号池与补充池都忙时应等待")
			}
			if got := env.launches()[launches:]; len(got) != 0 {
				t.Fatalf("只用已启动的 Worker 不应为补充池扩容：%v", got)
			}
		}},
		{name: "主号池能现场冷启动时先冷启动", run: func(t *testing.T, env *borrowEnv) {
			ctx := withExtraOverflowWait(normalCtx(env), 50*time.Millisecond)
			if account, err := env.acquire(t, ctx, testRuntimeModel, 10*time.Second); err != nil || account != normal {
				t.Fatalf("主号池能现场冷启动：选号得到 %q、错误 %v，期望主号池先冷启动 %s", account, err, normal)
			}
			if !slices.Contains(env.launches(), normal) {
				t.Fatal("主号池应现场冷启动")
			}
		}},
		{name: "排队满门槛后借到", run: func(t *testing.T, env *borrowEnv) {
			// 主号池的备用账户正在启动（启动被拦住）：排队门槛之前不借，过了门槛借补充池
			env.mu.Lock()
			env.gate = make(chan struct{})
			gate := env.gate
			env.mu.Unlock()
			go func() { _, _ = env.manager.promote(env.ctx, normal, testRuntimeModel) }()
			defer close(gate)
			if !waitUntil(5*time.Second, func() bool { return slices.Contains(env.manager.OpeningAccountIDs(), normal) }) {
				t.Fatal("主号池的账户没有进入启动中")
			}
			if account, err := env.acquire(t, normalCtx(env), testRuntimeModel, time.Second); err == nil {
				t.Fatalf("排队不到 3 秒不应借补充池，却得到 %s", account)
			}
			startedAt := time.Now()
			account, err := env.acquire(t, withExtraOverflowWait(normalCtx(env), 50*time.Millisecond), testRuntimeModel, 10*time.Second)
			if err != nil || account != lent || time.Since(startedAt) < 50*time.Millisecond {
				t.Fatalf("排队满门槛后：选号得到 %q、错误 %v、用时 %s", account, err, time.Since(startedAt))
			}
		}},
		{name: "本路由自己的补充池 Worker 也只在不够时用", run: func(t *testing.T, env *borrowEnv) {
			env.promote(t, home)
			proCtx := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopePro)
			for range 4 {
				if account, err := env.acquire(t, proCtx, testRuntimeModel, 10*time.Second); err != nil || account != home {
					t.Fatalf("/pro 主号池有空闲：选号得到 %q、错误 %v，期望 %s（剔除后补充池 Worker 不在主分组里）", account, err, home)
				}
			}
			release := occupy(t, env.pool, home)
			defer release()
			if account, err := env.acquire(t, proCtx, testRuntimeModel, 10*time.Second); err != nil || account != lent {
				t.Fatalf("/pro 主号池都忙：选号得到 %q、错误 %v，期望借 %s", account, err, lent)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			normalIDs := []string{normal}
			if test.standby {
				normalIDs = append(normalIDs, standby)
			}
			env := newBorrowEnv(t, normalIDs, nil, []string{lent, cold, home}, 1, 2)
			env.configure(config.ExtraPoolModeOverflow, true, false, 1, 2)
			env.cool(t, lent)
			if test.promoteNormal {
				env.promote(t, normal)
			}
			env.lend(t, lent)
			test.run(t, env)
		})
	}
}

// TestExtraFailFastPreserved 主用模型快速失败（ULTRA_PRIMARY_MODELS，候选全部冷却时立即返回）：
//   - 跨号池：/ultra 请求 argon，Ultra 号池全部冷却，补充池里跨号池借来的 Pro 成员空闲时借到；它在忙时立即按全部冷却返回 429，不为补充池等待；
//   - 本路由：/pro 请求 argon，能跑它的只有 /pro 自己的 Pro 成员（补充池 Worker），它在忙、1 秒后空出：忙碌的 L 留在主分组里，与补充池关闭时
//     一样按账号全部忙碌等它（不立即返回 429）
func TestExtraFailFastPreserved(t *testing.T) {
	t.Run("跨号池不为补充池等待", func(t *testing.T) {
		cooled, lent := "failfast-ultra@example.com", "failfast-pro-b@example.com"
		env := newBorrowEnv(t, nil, []string{cooled}, []string{lent}, 1, 2)
		env.service.setPrimaryModelSettings(nil, 0, []string{borrowPaidModel}, 10*time.Second)
		env.promote(t, cooled)
		coolModelFor(t, env.pool, cooled, borrowPaidModel, 2*time.Hour)
		env.cool(t, lent)
		env.lend(t, lent)
		if account, err := env.generate(t, aistudio.PoolScopeUltra, borrowPaidModel, 10*time.Second); err != nil || account != lent {
			t.Fatalf("主号池全部冷却：服务账户 %q、错误 %v，期望借补充池的 %s", account, err, lent)
		}
		release := occupy(t, env.pool, lent)
		defer release()
		startedAt := time.Now()
		_, err := env.generate(t, aistudio.PoolScopeUltra, borrowPaidModel, 20*time.Second)
		var cooling *aistudio.AllCoolingError
		if !errors.As(err, &cooling) {
			t.Fatalf("补充池只有忙碌的 Worker：err = %v，期望全部冷却的 429", err)
		}
		if elapsed := time.Since(startedAt); elapsed > 3*time.Second {
			t.Fatalf("快速失败用了 %s，不应为补充池等待", elapsed)
		}
	})
	t.Run("本路由的忙碌 Worker 照常等待", func(t *testing.T) {
		cooled, own := "failfast-pro-a@example.com", "failfast-pro-own@example.com"
		env := newBorrowEnv(t, nil, nil, []string{cooled, own}, 1, 2)
		env.service.setPrimaryModelSettings(nil, 0, []string{borrowPaidModel}, 10*time.Second)
		env.promote(t, cooled)
		coolModelFor(t, env.pool, cooled, borrowPaidModel, 2*time.Hour)
		env.cool(t, own)
		env.lend(t, own)
		release := occupy(t, env.pool, own)
		go func() {
			time.Sleep(time.Second)
			release()
		}()
		startedAt := time.Now()
		account, err := env.generate(t, aistudio.PoolScopePro, borrowPaidModel, 20*time.Second)
		if err != nil || account != own {
			t.Fatalf("本路由的补充池 Worker 在忙：服务账户 %q、错误 %v，期望等它空出后由 %s 服务（与补充池关闭时相同）", account, err, own)
		}
		if elapsed := time.Since(startedAt); elapsed < 800*time.Millisecond {
			t.Fatalf("用了 %s，应等到它空出", elapsed)
		}
	})
}

// TestExtraBudgetShared 主用模型等补充池的时间计入同一份排队预算：主号池全部冷却、补充池能借的都在忙时，按补充池忙碌排队，
// 到排队上限按号池繁忙（CapacityBusyError）返回
func TestExtraBudgetShared(t *testing.T) {
	normal, lent := "budget-normal@example.com", "budget-pro@example.com"
	env := newBorrowEnv(t, []string{normal}, nil, []string{lent}, 1, 1)
	env.service.setPrimaryModelSettings([]string{testRuntimeModel}, 600*time.Millisecond, nil, 0)
	env.promote(t, normal)
	coolModelFor(t, env.pool, normal, testRuntimeModel, 2*time.Hour)
	env.cool(t, lent)
	env.lend(t, lent)
	release := occupy(t, env.pool, lent)
	defer release()
	startedAt := time.Now()
	_, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
	var busy *aistudio.CapacityBusyError
	if !errors.As(err, &busy) || busy.Reason != waitReasonExtraBusy {
		t.Fatalf("err = %v，期望按补充池忙碌排队到上限的号池繁忙", err)
	}
	if elapsed := time.Since(startedAt); elapsed < 500*time.Millisecond || elapsed > 8*time.Second {
		t.Fatalf("排队 %s，期望约为排队上限 600ms", elapsed)
	}
}

// TestExtraExpansionBackground 补充池没有空闲 Worker、有冷成员时，请求发起一次补充池后台扩容，按“补充池 Worker 正在扩容”排队，
// 扩容出来的 Worker 就绪后借到；同一次选号不重复扩容
func TestExtraExpansionBackground(t *testing.T) {
	normal := "expand-normal@example.com"
	members := []string{"expand-pro-a@example.com", "expand-pro-b@example.com"}
	env := newBorrowEnv(t, []string{normal}, nil, members, 0, 2)
	env.promote(t, normal)
	coolModelFor(t, env.pool, normal, testRuntimeModel, 2*time.Hour)
	env.cool(t, members...)
	env.lend(t)
	env.mu.Lock()
	env.gate = make(chan struct{})
	gate := env.gate
	env.mu.Unlock()
	launches := len(env.launches())
	progress := newLeaseWaitProgress("req-extra-expand")
	progress.firstDelay = 10 * time.Millisecond
	ctx := withLeaseWaitProgress(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), progress)
	type result struct {
		account string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		account, err := env.acquire(t, ctx, testRuntimeModel, 20*time.Second)
		done <- result{account, err}
	}()
	if !waitUntil(5*time.Second, func() bool { return len(env.launches()) > launches }) {
		t.Fatal("请求没有发起补充池扩容")
	}
	// 扩容还没结束：等过两次复查，不再扩容别的成员
	time.Sleep(2200 * time.Millisecond)
	if got := env.launches()[launches:]; len(got) != 1 || !slices.Contains(members, got[0]) {
		t.Fatalf("同一次选号应只扩容一个成员：%v", got)
	}
	if countLogs(env.requests, "排队等待 | 原因="+waitReasonExtraExpanding) == 0 {
		t.Fatal("应按“补充池 Worker 正在扩容”排队")
	}
	expanded := env.launches()[launches]
	close(gate)
	got := <-done
	if got.err != nil || got.account != expanded {
		t.Fatalf("扩容就绪后：选号得到 %q、错误 %v，期望 %s", got.account, got.err, expanded)
	}
	if !env.manager.extraOwnedHas(expanded) {
		t.Fatal("请求扩容出来的 Worker 应计入补充池")
	}
}

// TestExtraFreeNeverServesPaid 默认地址请求付费模型（PAID_ONLY_MODELS）：Free 成员的补充池 Worker 空闲、Pro 成员的忙碌时等 Pro，永远不取 Free
func TestExtraFreeNeverServesPaid(t *testing.T) {
	free, paid := "paid-free@example.com", "paid-pro@example.com"
	env := newBorrowEnv(t, []string{free}, nil, []string{paid}, 2, 2)
	// Free 账户的目录里也有付费模型，由权益规则挡住
	if err := env.pool.SetCatalog(free, aistudio.BenefitTierFree, []aistudio.Model{
		borrowChat(testRuntimeModel), borrowChat(extraCoolModel), borrowChat(borrowPaidModel),
	}); err != nil {
		t.Fatal(err)
	}
	env.pool.SetModelTierRules([]string{borrowUltraModel}, []string{borrowPaidModel})
	env.cool(t, free, paid)
	env.lend(t, free, paid)
	release := occupy(t, env.pool, paid)
	type result struct {
		account string
		err     error
	}
	done := make(chan result, 1)
	go func() {
		account, err := env.generate(t, aistudio.PoolScopeNormal, borrowPaidModel, 20*time.Second)
		done <- result{account, err}
	}()
	time.Sleep(800 * time.Millisecond)
	if calls := env.upstream.called(); len(calls) != 0 {
		t.Fatalf("Pro 成员忙碌时应等待，不应发出：%v", calls)
	}
	release()
	got := <-done
	if got.err != nil || got.account != paid {
		t.Fatalf("服务账户 %q、错误 %v，期望等到 Pro 成员 %s", got.account, got.err, paid)
	}
	for _, call := range env.upstream.called() {
		if call.account == free {
			t.Fatal("Free 账户不应服务付费模型")
		}
	}
}

// TestExtraOnlyRequestErrors 默认地址请求付费模型（只能由补充池服务）：补充池的候选全部冷却时按 429 返回、带 Retry-After；补充池里没有能借的时
// 按 503（模型在列，官方措辞）；EXTRA_POOL_DEFAULT_PAID 关闭时与没有补充池时相同，按 404 并提示改用 /pro
func TestExtraOnlyRequestErrors(t *testing.T) {
	member, healthy, normal := "only-pro-a@example.com", "only-pro-b@example.com", "only-normal@example.com"
	t.Run("补充池全部冷却", func(t *testing.T) {
		env := newBorrowEnv(t, []string{normal}, nil, []string{member}, 1, 1)
		env.cool(t, member)
		coolModelFor(t, env.pool, member, borrowPaidModel, 2*time.Hour)
		env.lend(t, member)
		_, err := env.generate(t, aistudio.PoolScopeNormal, borrowPaidModel, 20*time.Second)
		var cooling *aistudio.AllCoolingError
		if !errors.As(err, &cooling) || cooling.HTTPStatus() != http.StatusTooManyRequests || time.Until(cooling.RetryAt()) < time.Hour {
			t.Fatalf("err = %v，期望全部冷却的 429、Retry-After 为最早恢复（约 2 小时）", err)
		}
		if cooling.Pool != aistudio.PoolScopeExtra {
			t.Fatalf("号池 = %v，期望补充池（按通用官方措辞，不写 Ultra 或 Pro 号池）", cooling.Pool)
		}
	})
	t.Run("补充池没有能借的", func(t *testing.T) {
		env := newBorrowEnv(t, []string{normal}, nil, []string{healthy}, 1, 1)
		env.lend(t)
		_, err := env.generate(t, aistudio.PoolScopeNormal, borrowPaidModel, 20*time.Second)
		var noServing *aistudio.NoServingAccountError
		if !errors.As(err, &noServing) || noServing.HTTPStatus() != http.StatusServiceUnavailable || noServing.RetryAt().IsZero() {
			t.Fatalf("err = %v，期望 503（模型在列）并带 Retry-After", err)
		}
		if message := noServing.PublicMessage(); !strings.Contains(message, "is listed") || strings.Contains(message, "exceeded") {
			t.Fatalf("对外说明 = %q，期望官方措辞", message)
		}
	})
	t.Run("默认地址不借付费模型", func(t *testing.T) {
		env := newBorrowEnv(t, []string{normal}, nil, []string{member}, 1, 1)
		env.configure(config.ExtraPoolModeShare, false, false, 1, 1)
		env.cool(t, member)
		env.lend(t, member)
		_, err := env.generate(t, aistudio.PoolScopeNormal, borrowPaidModel, 20*time.Second)
		var hinted *aistudio.ModelInOtherPoolError
		if !errors.Is(err, aistudio.ErrModelNotFound) || !errors.As(err, &hinted) || !strings.Contains(hinted.PublicHint(), "/pro") {
			t.Fatalf("err = %v，期望 404 并提示改用 /pro", err)
		}
		if calls := env.upstream.called(); len(calls) != 0 {
			t.Fatalf("不应借补充池：%v", calls)
		}
	})
}

// TestExtraProRouteNoUltraOnly 含 Ultra 时 /pro 请求 Ultra 独占模型不借 Ultra 成员：按 404 返回，与补充池关闭时相同
func TestExtraProRouteNoUltraOnly(t *testing.T) {
	pro, ultra := "proroute-pro@example.com", "proroute-ultra@example.com"
	env := newBorrowEnv(t, nil, []string{ultra}, []string{pro}, 1, 1)
	env.configure(config.ExtraPoolModeShare, true, true, 1, 1)
	env.cool(t, ultra)
	env.lend(t, ultra)
	_, on := env.generate(t, aistudio.PoolScopePro, borrowUltraModel, 20*time.Second)
	env.disable()
	_, off := env.generate(t, aistudio.PoolScopePro, borrowUltraModel, 20*time.Second)
	if on == nil || off == nil || on.Error() != off.Error() || !errors.Is(on, aistudio.ErrModelNotFound) {
		t.Fatalf("开启 %v，关闭 %v，期望都按模型不存在返回且相同", on, off)
	}
	if calls := env.upstream.called(); len(calls) != 0 {
		t.Fatalf("/pro 不应借 Ultra 成员跑 Ultra 独占模型：%v", calls)
	}
}

// modelEntries 返回模型列表的“ID 通道”
func modelEntries(models []aistudio.Model) []string {
	entries := make([]string, 0, len(models))
	for _, model := range models {
		entries = append(entries, model.ID+" "+strings.Join(model.Channels, ","))
	}
	return entries
}

// TestModelsDefaultRouteExtraPaid 默认地址的模型列表合并补充池能服务的付费模型（按目录顺序、同 ID 合并通道）；/pro、/ultra 不变；关闭、
// EXTRA_POOL_DEFAULT_PAID 关闭时与没有补充池时相同
func TestModelsDefaultRouteExtraPaid(t *testing.T) {
	env := newBorrowEnv(t, []string{"models-normal@example.com"}, []string{"models-ultra@example.com"}, []string{"models-pro@example.com"}, 1, 1)
	list := func(scope aistudio.PoolScope) []string {
		models, err := env.service.Models(aistudio.ContextWithPoolScope(env.ctx, scope))
		if err != nil {
			t.Fatal(err)
		}
		return modelEntries(models)
	}
	env.disable()
	offDefault, offPro, offUltra := list(aistudio.PoolScopeNormal), list(aistudio.PoolScopePro), list(aistudio.PoolScopeUltra)
	env.configure(config.ExtraPoolModeShare, true, false, 1, 1)
	onDefault := list(aistudio.PoolScopeNormal)
	want := []string{testRuntimeModel + " playground", extraCoolModel + " playground", borrowFreeModel + " playground", borrowPaidModel + " playground"}
	if !slices.Equal(onDefault, want) {
		t.Fatalf("开启后默认地址 = %v，期望 %v（按目录顺序合并付费模型）", onDefault, want)
	}
	if slices.Contains(offDefault, borrowPaidModel+" playground") {
		t.Fatalf("关闭时默认地址不应列出付费模型：%v", offDefault)
	}
	if got := list(aistudio.PoolScopePro); !slices.Equal(got, offPro) {
		t.Fatalf("/pro 的列表不应随补充池变化：开启 %v，关闭 %v", got, offPro)
	}
	if got := list(aistudio.PoolScopeUltra); !slices.Equal(got, offUltra) {
		t.Fatalf("/ultra 的列表不应随补充池变化：开启 %v，关闭 %v", got, offUltra)
	}
	env.configure(config.ExtraPoolModeShare, false, false, 1, 1)
	if got := list(aistudio.PoolScopeNormal); !slices.Equal(got, offDefault) {
		t.Fatalf("EXTRA_POOL_DEFAULT_PAID 关闭时默认地址 = %v，期望与关闭时相同 %v", got, offDefault)
	}
	merged := mergeEligibleModels(
		[]aistudio.Model{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		[]aistudio.Model{{ID: "c", Channels: []string{"playground"}}, {ID: "a", Channels: []string{"playground"}}},
		[]aistudio.Model{{ID: "b", Channels: []string{"build"}}, {ID: "c", Channels: []string{"build", "playground"}}},
	)
	if got := modelEntries(merged); !slices.Equal(got, []string{"a playground", "b build", "c playground,build"}) {
		t.Fatalf("合并结果 = %v", got)
	}
}
