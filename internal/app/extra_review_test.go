package app

import (
	"context"
	"errors"
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

// 补充池第 1 轮复查的回归测试：主分区核对可接替账户时不把补充池 Worker 的账户当备用号、停服之后补充池预热不再接续、忙碌的本路由补充池
// Worker 留在主分组、本路由借来的账户重放时计入补充池、count_tokens 借不到能跑的账户时按 503、默认地址号池没有账户时照样借用、
// 补充池内的按需替换不误报超过峰值

// spareMember 让账户成为补充池成员，但能服务 extraCoolModel：目录为 testRuntimeModel、extraCoolModel 与 otherModel，只在 otherModel 上冷却
func spareMember(t *testing.T, env *extraEnv, accountID string) {
	t.Helper()
	const otherModel = "extra-review-other-model"
	if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{
		borrowChat(testRuntimeModel), borrowChat(extraCoolModel), borrowChat(otherModel),
	}); err != nil {
		t.Fatal(err)
	}
	coolModel(t, env.pool, accountID, otherModel, 2*time.Hour, "每日限额")
}

// retryAtOf 返回账户的冷却淘汰登记的下次尝试时间（没有登记时 ok 为假）
func retryAtOf(manager *accountWorkerManager, accountID string) (time.Time, bool) {
	manager.retireMu.Lock()
	defer manager.retireMu.Unlock()
	entry := manager.retirements[accountID]
	if entry == nil {
		return time.Time{}, false
	}
	return entry.retryAt, true
}

// TestExtraOwnedNotSpare 主分区核对“有没有可接替账户”时，不把持有补充池 Worker 的成员算作家分区的空闲备用号（替补挑选本来就挑不到它）：
// 可接替的只有它时，冷却淘汰按没有可接替账户推迟（不关闭主分区 Worker、不写替补跳过，keepWarm 几轮下来同一个冷却号不被反复关闭再冷启动），
// 冷却轮换与冷却清扫不换下主分区 Worker
func TestExtraOwnedNotSpare(t *testing.T) {
	t.Run("冷却淘汰", func(t *testing.T) {
		cooled, spare := "spare-retire-a@example.com", "spare-retire-b@example.com"
		env := newExtraEnv(t, []string{cooled, spare}, nil, 1, 1, 0, 1, 1, 1)
		env.manager.setPrimaryModels([]string{extraCoolModel}, nil)
		env.member(t, aistudio.BenefitTierFree, cooled)
		spareMember(t, env, spare)
		env.promote(t, cooled)
		env.pass(t)
		if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{spare}) {
			t.Fatalf("补充池 Worker = %v，期望 %s", got, spare)
		}
		worker := env.workerOf(cooled)
		launches := len(env.launches())
		for range 4 {
			env.service.keepWarmPass(env.ctx, time.Now())
			// 登记由淘汰循环异步处理：等它按没有可接替账户推迟（或把 Worker 关掉）
			waitUntil(3*time.Second, func() bool {
				retryAt, ok := retryAtOf(env.manager, cooled)
				return ok && retryAt.After(time.Now()) || env.workerOf(cooled) != worker
			})
			env.manager.waitPrewarm()
			env.waitLaunches(t)
		}
		if got := env.launches()[launches:]; len(got) != 0 {
			t.Fatalf("可接替的只有补充池 Worker 时不应淘汰再冷启动：%v", got)
		}
		if env.workerOf(cooled) != worker {
			t.Fatal("主分区的冷却 Worker 不应被关闭")
		}
		if retryAt, ok := retryAtOf(env.manager, cooled); !ok || !retryAt.After(time.Now()) {
			t.Fatal("登记应按没有可接替账户推迟")
		}
		if n := countLogs(env.requests, "替补跳过"); n != 0 {
			t.Fatalf("不应写替补跳过：%d 行", n)
		}
		if n := countLogs(env.requests, "WAA Worker 冷却淘汰 |"); n != 0 {
			t.Fatalf("不应写冷却淘汰：%d 行", n)
		}
	})
	t.Run("冷却轮换", func(t *testing.T) {
		cooled := extraIDsOf("spare-rotate-a", 3)
		spare := "spare-rotate-b@example.com"
		env := newExtraEnv(t, append(slices.Clone(cooled), spare), nil, 3, 3, 0, 1, 1, 1)
		env.member(t, aistudio.BenefitTierFree, cooled...)
		spareMember(t, env, spare)
		env.promote(t, cooled...)
		env.pass(t)
		if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{spare}) {
			t.Fatalf("补充池 Worker = %v，期望 %s", got, spare)
		}
		for _, accountID := range cooled {
			env.manager.extraAccount(accountID).readyAt.Store(time.Now().Add(-time.Hour).UnixNano())
		}
		env.service.rotateCooledWorkers(env.ctx)
		env.manager.waitPrewarm()
		if n := countLogs(env.requests, "WAA Worker 冷却轮换 |"); n != 0 {
			t.Fatalf("可接替的只有补充池 Worker 时不应冷却轮换：%d 行", n)
		}
		if got := env.in(aistudio.PoolScopeNormal); !slices.Equal(got, cooled) {
			t.Fatalf("普通分区 = %v，期望不变 %v", got, cooled)
		}
	})
	t.Run("冷却清扫", func(t *testing.T) {
		cooled, spare := "spare-sweep-a@example.com", "spare-sweep-b@example.com"
		env := newExtraEnv(t, []string{cooled, spare}, nil, 1, 1, 0, 1, 1, 1)
		spareMember(t, env, spare)
		env.promote(t, cooled)
		coolGlobally(t, env.pool, cooled)
		env.pass(t)
		if got := env.in(aistudio.PoolScopeExtra); !slices.Equal(got, []string{spare}) {
			t.Fatalf("补充池 Worker = %v，期望 %s", got, spare)
		}
		worker := env.workerOf(cooled)
		if closed := env.service.sweepDeadWorkers(env.ctx); closed != 0 {
			t.Fatalf("可接替的只有补充池 Worker 时冷却清扫不应关闭主分区 Worker：关闭 %d 个", closed)
		}
		if env.workerOf(cooled) != worker {
			t.Fatal("主分区的全局冷却 Worker 不应被关闭")
		}
	})
}

// TestExtraLaunchStopsWithService 停服之后补充池预热不再接续：第一个补充池预热启动成功、它的接续在停服（服务不在运行、ResetAll 取消并重建
// 后台 context、关闭全部 Worker）之后才执行时，不再挑下一个冷成员启动；停服之后直接调用接续同样什么也不做
func TestExtraLaunchStopsWithService(t *testing.T) {
	stop := func(t *testing.T, env *extraEnv) {
		t.Helper()
		running := false
		active := func() bool { return running }
		env.manager.serviceActive.Store(&active)
		if err := env.manager.ResetAll(); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("启动成功后接续落在停服之后", func(t *testing.T) {
		ids := extraIDsOf("stop-park", 4)
		env := newExtraEnv(t, ids, nil, 0, 4, 0, 1, 3, 4)
		env.member(t, aistudio.BenefitTierFree, ids...)
		gate := make(chan struct{})
		var once sync.Once
		env.manager.launch = func(_ context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
			env.mu.Lock()
			env.launched = append(env.launched, accountID)
			env.mu.Unlock()
			first := false
			once.Do(func() { first = true })
			if first {
				<-gate
			}
			worker, closed := aistudio.NewStubWorker(accountID, 0)
			env.mu.Lock()
			env.closed = append(env.closed, closed)
			env.mu.Unlock()
			return worker, nil
		}
		env.service.extraPass(env.ctx)
		if !waitUntil(5*time.Second, func() bool { return len(env.launches()) == 1 }) {
			t.Fatal("补充池预热没有开始")
		}
		// 第一个启动成功后，它的 goroutine 在接续之前（extraLaunchDone 的 clearWarmFailure）等着；期间停服
		env.manager.warmFailMu.Lock()
		close(gate)
		if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
			env.manager.warmFailMu.Unlock()
			t.Fatal("第一个补充池 Worker 没有就绪")
		}
		stop(t, env)
		stoppedAt := len(env.launches())
		env.manager.warmFailMu.Unlock()
		env.waitLaunches(t)
		time.Sleep(100 * time.Millisecond)
		env.waitLaunches(t)
		if got := env.launches()[stoppedAt:]; len(got) != 0 {
			t.Fatalf("停服之后补充池预热的接续又启动了 %v", got)
		}
		if live := env.liveBrowsers(); live != 0 {
			t.Fatalf("停服之后还有 %d 个浏览器在运行", live)
		}
	})
	t.Run("停服之后直接接续", func(t *testing.T) {
		ids := extraIDsOf("stop-chain", 4)
		env := newExtraEnv(t, ids, nil, 0, 4, 0, 1, 0, 4)
		env.member(t, aistudio.BenefitTierFree, ids...)
		env.pass(t) // 常驻 0：只建成员缓存
		env.setExtra(true, 3, 4)
		// 停服（ResetAll）会清空成员缓存：先取下来，模拟停服之前取到缓存、停服之后才发起的接续
		view := env.manager.extraView.Load()
		stop(t, env)
		env.manager.extraLaunch(view)
		env.waitLaunches(t)
		if got := env.launches(); len(got) != 0 {
			t.Fatalf("服务已停止，补充池预热不应启动浏览器：%v", got)
		}
	})
}

// TestExtraOwnBusyStaysInMain 本路由自己的补充池 Worker 在忙（1 秒后空出）、另有一个没冷却的冷备用号：额度 429 之后只用已启动 Worker 的选号
// 与补充池关闭时一样按账号全部忙碌等它，不按“额度 429 后没有已启动的 Worker”立即返回；排队快照的热忙碌数不把它与补充池忙碌重复计
func TestExtraOwnBusyStaysInMain(t *testing.T) {
	own, standby := "ownbusy-normal-a@example.com", "ownbusy-normal-b@example.com"
	env := newBorrowEnv(t, []string{own, standby}, nil, nil, 1, 2)
	env.cool(t, own)
	env.lend(t, own)
	release := occupy(t, env.pool, own)
	go func() {
		time.Sleep(time.Second)
		release()
	}()
	ctx := withoutColdStart(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal))
	startedAt := time.Now()
	account, err := env.acquire(t, ctx, testRuntimeModel, 10*time.Second)
	if err != nil || account != own {
		t.Fatalf("选号得到 %q、错误 %v，期望等本路由的补充池 Worker 空出后取到 %s", account, err, own)
	}
	if elapsed := time.Since(startedAt); elapsed < 800*time.Millisecond {
		t.Fatalf("用了 %s，应等到它空出", elapsed)
	}
	if got := env.launches(); slices.Contains(got, standby) {
		t.Fatalf("只用已启动 Worker 的选号不应冷启动：%v", got)
	}
	// 排队快照：忙碌的 L 留在主分组里，热忙碌数按不含 L 的主分组写，只计在补充池忙碌里
	run := &extraRun{active: true, hotDone: true, mainBusy: 0, hot: aistudio.AccountCandidateGroups{WarmBusy: []string{own}}}
	snapshot := queueSnapshot{warmBusy: 1}
	run.annotate(&snapshot)
	if text := snapshot.text(time.Now()); snapshot.warmBusy != 0 || snapshot.extraBusy != 1 || !strings.Contains(text, "补充池忙碌=1") {
		t.Fatalf("排队快照 = %q（热忙碌 %d、补充池忙碌 %d），期望热忙碌 0、补充池忙碌 1", text, snapshot.warmBusy, snapshot.extraBusy)
	}
}

// TestExtraOwnReplayCountsInExtra 本路由借来的账户（默认地址的 Free 成员）发送时 Worker 失败、重建后在原账号重放：按留存口径仍是成员，
// 立即在补充池重开，不等家分区（普通分区已满、另一个 Worker 在忙）的槽位，也不替换家分区的 Worker；重放那次尝试照样记为由补充池服务
func TestExtraOwnReplayCountsInExtra(t *testing.T) {
	busy, member := "ownreplay-normal-a@example.com", "ownreplay-normal-b@example.com"
	env := newBorrowEnv(t, []string{busy, member}, nil, nil, 1, 1)
	env.manager.maxActive.Store(1)
	env.promote(t, busy)
	env.cool(t, member)
	env.lend(t, member)
	release := occupy(t, env.pool, busy)
	defer release()
	env.upstream.failAccount(member, waaRuntimeFailure("GenerateContent"))
	// 第二次调用（重放）时活动请求应标 extra
	var mu sync.Mutex
	calls, markedOnReplay := 0, false
	env.upstream.beforeAccount(member, func(context.Context, aistudio.GenerateRequest) error {
		mu.Lock()
		defer mu.Unlock()
		if calls++; calls == 2 {
			for _, request := range env.requests.list() {
				markedOnReplay = markedOnReplay || request.Extra
			}
		}
		return nil
	})
	busyWorker := env.workerOf(busy)
	launches := len(env.launches())
	startedAt := time.Now()
	account, err := env.generate(t, aistudio.PoolScopeNormal, testRuntimeModel, 20*time.Second)
	if err != nil || account != member || !slices.Equal(env.upstream.accounts(0), []string{member, member}) {
		t.Fatalf("服务账户 %q、错误 %v、上游调用 %v，期望 Worker 重建后在 %s 上重放成功", account, err, env.upstream.accounts(0), member)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("重放用了 %s，不应等家分区的槽位", elapsed)
	}
	if got := env.launches()[launches:]; !slices.Equal(got, []string{member}) {
		t.Fatalf("重放应重新启动原账号一次：%v", got)
	}
	if !env.manager.extraOwnedHas(member) || !slices.Equal(env.in(aistudio.PoolScopeExtra), []string{member}) {
		t.Fatalf("重放时的重新启动应计入补充池：补充池 %v、普通分区 %v", env.in(aistudio.PoolScopeExtra), env.in(aistudio.PoolScopeNormal))
	}
	if env.workerOf(busy) != busyWorker || countLogs(env.requests, "按需替换") != 0 {
		t.Fatal("不应替换家分区里健康的 Worker")
	}
	mu.Lock()
	defer mu.Unlock()
	if !markedOnReplay {
		t.Fatal("在补充池重开后的重放尝试应记为由补充池服务")
	}
}

// TestExtraOverflowBusyLendableNotMain 只在不够时补充（overflow）：留在主分组 WarmBusy 里的忙碌 L 不算主号池的热 Worker。/pro 自己的补充池
// Worker 一个在忙、一个空闲，另有一个能冷启动的 Pro 备用号：主号池先冷启动备用号（3 秒内不借），不按“主号池热 Worker 都在忙”借补充池
func TestExtraOverflowBusyLendableNotMain(t *testing.T) {
	own, idle, standby := "ovbusy-pro-a@example.com", "ovbusy-pro-b@example.com", "ovbusy-pro-c@example.com"
	env := newBorrowEnv(t, nil, nil, []string{own, idle, standby}, 2, 2)
	env.configure(config.ExtraPoolModeOverflow, true, false, 2, 2)
	env.cool(t, own, idle)
	env.lend(t, own, idle)
	release := occupy(t, env.pool, own)
	defer release()
	account, err := env.acquire(t, aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopePro), testRuntimeModel, 10*time.Second)
	if err != nil || account != standby {
		t.Fatalf("选号得到 %q、错误 %v，期望主号池先冷启动 %s（忙碌的补充池 Worker 不算主号池的热 Worker）", account, err, standby)
	}
	if !slices.Contains(env.launches(), standby) {
		t.Fatal("主号池应冷启动备用号")
	}
}

// TestExtraCountTokensLendableCannotServe 默认地址 count_tokens 请求只能由补充池服务的付费模型，L 非空但里面只有跑不了付费模型的 Free 成员：
// 与 L 为空时一样按 503（模型在列表里）返回，不按没有候选的 404
func TestExtraCountTokensLendableCannotServe(t *testing.T) {
	normal, freeMember, pro := "countfree-normal-a@example.com", "countfree-normal-b@example.com", "countfree-pro@example.com"
	env := newBorrowEnv(t, []string{normal, freeMember}, nil, []string{pro}, 1, 2)
	env.upstream.count = func(ctx context.Context, request aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
		lease, err := env.pool.AcquireFor(ctx, aistudio.AccountSelection{
			ModelID: request.Model, Method: "countTokens", Pool: aistudio.PoolScopeFromContext(ctx),
		})
		if err != nil {
			return aistudio.TokenCount{}, err
		}
		_ = lease.Release()
		return aistudio.TokenCount{InputTokens: 1}, nil
	}
	env.promote(t, normal)
	env.cool(t, freeMember)
	env.lend(t, freeMember)
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), 5*time.Second)
	defer cancel()
	_, err := env.service.CountTokens(ctx, aistudio.TokenCountRequest{Model: borrowPaidModel})
	var noServing *aistudio.NoServingAccountError
	if !errors.As(err, &noServing) || !noServing.Listed || noServing.HTTPStatus() != http.StatusServiceUnavailable {
		t.Fatalf("err = %v（%T），期望模型在列表里的 503", err, err)
	}
}

// TestExtraEmptyRouteBorrows 默认地址的号池一个账户都没有（只有 Pro 账户，PRO_POOL=true）、补充池里有 Pro 成员的 Worker：/v1/models 合并了
// 补充池的模型，按列表里的模型发请求由补充池服务，不按号池没有账户返回
func TestExtraEmptyRouteBorrows(t *testing.T) {
	pro := []string{"emptyroute-pro-a@example.com", "emptyroute-pro-b@example.com"}
	env := newBorrowEnv(t, nil, nil, pro, 1, 2)
	env.cool(t, pro[0])
	env.lend(t, pro[0])
	models, err := env.service.Models(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal))
	if err != nil || !slices.ContainsFunc(models, func(model aistudio.Model) bool { return model.ID == borrowPaidModel }) {
		t.Fatalf("默认地址的模型列表 = %v（%v），期望合并补充池的模型", models, err)
	}
	for _, model := range []string{testRuntimeModel, borrowPaidModel} {
		if account, err := env.generate(t, aistudio.PoolScopeNormal, model, 5*time.Second); err != nil || account != pro[0] {
			t.Fatalf("模型 %s：服务账户 %q、错误 %v，期望借补充池的 %s", model, account, err, pro[0])
		}
	}
}

// TestExtraOverPeakIgnoresReplacement 补充池满到峰值后的按需替换是先启后停：新 Worker 启动期间（旧 Worker 已被预留为淘汰对象）不写超过峰值；
// 峰值真的调小到驻留数以下时照常写一次
func TestExtraOverPeakIgnoresReplacement(t *testing.T) {
	ids := extraIDsOf("overpeak", 3)
	env := newExtraEnv(t, append(slices.Clone(ids), partitionNormalA), nil, 1, 1, 0, 1, 2, 2)
	env.member(t, aistudio.BenefitTierFree, ids...)
	env.promote(t, partitionNormalA)
	env.pass(t)
	extra := env.in(aistudio.PoolScopeExtra)
	if len(extra) != 2 {
		t.Fatalf("补充池 Worker = %v", extra)
	}
	cold := ""
	for _, accountID := range ids {
		if !slices.Contains(extra, accountID) {
			cold = accountID
		}
	}
	// 其中一个补充池 Worker 对请求的模型是死槽位：扩容按替换它启动
	coolModel(t, env.pool, extra[0], testRuntimeModel, 2*time.Hour, "每日限额")
	gate := make(chan struct{})
	env.mu.Lock()
	env.gate = gate
	env.mu.Unlock()
	done := env.manager.extraExpandInBackground(cold, testRuntimeModel)
	if !waitUntil(5*time.Second, func() bool { _, opening := env.manager.openingSet.Load(cold); return opening }) {
		close(gate)
		t.Fatal("扩容没有开始")
	}
	env.manager.noteExtraOverPeak()
	close(gate)
	<-done
	if n := countLogs(env.requests, "补充池 Worker 超过峰值"); n != 0 {
		t.Fatalf("补充池内的按需替换期间不应写超过峰值：%d 行", n)
	}
	env.mu.Lock()
	env.gate = nil
	env.mu.Unlock()
	env.setExtra(true, 1, 1)
	env.manager.noteExtraOverPeak()
	if n := countLogs(env.requests, "补充池 Worker 超过峰值"); n != 1 {
		t.Fatalf("峰值调小到驻留数以下时应写一次超过峰值：%d 行", n)
	}
}

// TestExtraReleaseReasonWhenDisabled 关闭补充池、家分区已满：补充池 Worker 空闲时释放，原因写明是关闭补充池（不只写“家分区已满”）
func TestExtraReleaseReasonWhenDisabled(t *testing.T) {
	env := newExtraEnv(t, nil, []string{proPartitionA, proPartitionB}, 0, 1, 1, 1, 1, 2)
	env.member(t, aistudio.BenefitTierPro, proPartitionA)
	env.pass(t)
	env.promote(t, proPartitionB)
	if !env.manager.extraOwnedHas(proPartitionA) {
		t.Fatal("应先有补充池 Worker")
	}
	env.setExtra(false, 1, 2)
	env.pass(t)
	if env.manager.extraOwnedHas(proPartitionA) || env.workerOf(proPartitionA) != nil {
		t.Fatal("关闭补充池、家分区已满时应在空闲后释放")
	}
	if countFrom(env.requests, proPartitionA, "补充池 Worker 释放 | 原因=补充池已关闭，家分区已满") != 1 {
		t.Fatalf("释放日志不符：%q", logsFrom(env.requests, proPartitionA))
	}
}
