package app

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// firstWarmCatalog 为首轮预热测试的目录服务：目录缓存里总有 testRuntimeModel（启动不等首个账户读取目录）
type firstWarmCatalog struct{}

func (firstWarmCatalog) RefreshAccountModels(context.Context, string) ([]aistudio.Model, error) {
	return firstWarmCatalog{}.CachedModels(), nil
}

func (firstWarmCatalog) CachedModels() []aistudio.Model {
	return []aistudio.Model{{
		ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true},
	}}
}

// firstWarmEnv 为首轮预热测试的环境：纯 Go 后端（启动返回 stub Worker），经真实的 trackedService.Start 启动；
// launched 记录冷启动的账户（按开始的先后），gates 中的账户启动停在启动中直到对应通道关闭，fail 中的账户启动失败
type firstWarmEnv struct {
	service  *trackedService
	manager  *accountWorkerManager
	pool     *aistudio.AccountPool
	requests *requestRegistry
	upstream *rateUpstream
	ctx      context.Context
	mu       sync.Mutex
	launched []string
	gates    map[string]chan struct{}
	fail     map[string]error
}

func newFirstWarmEnv(t *testing.T, normalIDs []string, ultraIDs []string, normalWarm, normalMax, ultraWarm, ultraMax int) *firstWarmEnv {
	t.Helper()
	manager, pool, requests := partitionTestManager(t, normalIDs, ultraIDs, normalWarm, normalMax, ultraWarm, ultraMax)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	env := &firstWarmEnv{
		manager: manager, pool: pool, requests: requests, ctx: ctx, upstream: &rateUpstream{},
		gates: make(map[string]chan struct{}), fail: make(map[string]error),
	}
	manager.launch = func(launchCtx context.Context, accountID string, _ camoufoxnative.Options) (*aistudio.NativeWorker, error) {
		env.mu.Lock()
		env.launched = append(env.launched, accountID)
		gate, failure := env.gates[accountID], env.fail[accountID]
		env.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-launchCtx.Done():
				return nil, launchCtx.Err()
			}
		}
		if failure != nil {
			return nil, failure
		}
		worker, _ := aistudio.NewStubWorker(accountID, 0)
		return worker, nil
	}
	service := retireTestService(ctx, manager, pool, requests)
	service.catalog = firstWarmCatalog{}
	service.service = env.upstream
	service.timeout.Store(int64(time.Minute))
	note := authRequiredHook(service.noteAuthRequired)
	manager.authRequired.Store(&note)
	env.service = service
	t.Cleanup(func() { _, _ = service.Stop() })
	return env
}

// gate 让账户的 Worker 启动停在启动中，返回放行函数（需在启动之前调用）
func (env *firstWarmEnv) gate(accountIDs ...string) func() {
	gate := make(chan struct{})
	env.mu.Lock()
	for _, accountID := range accountIDs {
		env.gates[accountID] = gate
	}
	env.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

func (env *firstWarmEnv) launches() []string {
	env.mu.Lock()
	defer env.mu.Unlock()
	return slices.Clone(env.launched)
}

// start 经 trackedService.Start 启动生成服务，断言立即进入运行
func (env *firstWarmEnv) start(t *testing.T) {
	t.Helper()
	models, started, err := env.service.Start(env.ctx, nil)
	if err != nil || !started || len(models) == 0 {
		t.Fatalf("Start = (%d 个模型, %t, %v)，期望启动成功", len(models), started, err)
	}
	if state := env.service.State(); state != "RUNNING" {
		t.Fatalf("Start 返回后状态为 %s，期望 RUNNING（首轮预热在后台进行）", state)
	}
}

// waitLaunched 等到 launched 至少有 count 个账户
func (env *firstWarmEnv) waitLaunched(t *testing.T, count int) []string {
	t.Helper()
	if !waitUntil(5*time.Second, func() bool { return len(env.launches()) >= count }) {
		t.Fatalf("冷启动只有 %v，期望至少 %d 个", env.launches(), count)
	}
	return env.launches()
}

// waitQueued 等到有请求进入调度等待队列
func waitQueued(t *testing.T, manager *accountWorkerManager) {
	t.Helper()
	if !waitUntil(5*time.Second, func() bool {
		manager.dispatch.mu.Lock()
		defer manager.dispatch.mu.Unlock()
		return len(manager.dispatch.queues) > 0
	}) {
		t.Fatal("请求没有进入调度等待队列")
	}
}

func normalSelection() aistudio.AccountSelection {
	return aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
}

// firstWarmReason 返回不等待的选号因首轮预热没有立即可用的账号时的原因，取得租约时释放并返回空
func firstWarmReason(t *testing.T, service *trackedService, ctx context.Context) string {
	t.Helper()
	lease, err := service.acquireWarmLeaseNow(ctx, normalSelection(), nil)
	if err == nil {
		_ = lease.Release()
		return ""
	}
	var noImmediate *noImmediateLeaseError
	if !errors.As(err, &noImmediate) {
		t.Fatalf("不等待的选号返回 %v，期望 *noImmediateLeaseError", err)
	}
	return noImmediate.reason
}

// TestStartRunningWhileFirstPrewarmFails 首批账户的 Worker 都启动失败（Camoufox 崩溃、落到登录页等）：Start 照常进入运行并立即返回，
// 不按启动失败处理（原先等首个 Worker、首轮失败后整个启动失败，ResetAll 后被监督器按退避反复重启）；首轮预热失败只写一行 WARN，
// 服务保持运行，首轮状态已清除，请求按普通路径选号
func TestStartRunningWhileFirstPrewarmFails(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	env.fail[partitionNormalA] = errors.New("Camoufox 进程退出（测试）")
	env.fail[partitionNormalB] = errors.New("Camoufox 进程退出（测试）")
	startedAt := time.Now()
	env.start(t)
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("Start 用了 %s，期望不等预热结果", elapsed)
	}
	if !waitUntil(5*time.Second, func() bool {
		return hasLogContaining(env.requests, "WAA Worker 首轮预热失败", "稍后自动重试")
	}) {
		t.Fatalf("首轮预热失败应写 WARN，日志 = %v", serviceLogMessages(env.requests))
	}
	if state := env.service.State(); state != "RUNNING" {
		t.Fatalf("首轮预热失败后状态为 %s，期望保持 RUNNING", state)
	}
	if pending := env.manager.firstWarmPendingNames(); len(pending) != 0 {
		t.Fatalf("首轮结束后不应还有待定分区: %v", pending)
	}
	if name, _ := env.service.Step(); name != "" {
		t.Fatalf("进入运行后不应还有启动步骤: %q", name)
	}
	for _, message := range serviceLogMessages(env.requests) {
		if strings.Contains(message, "生成服务启动失败") {
			t.Fatalf("预热失败不应按启动失败处理: %q", message)
		}
		if strings.Contains(message, "WAA Worker 首轮预热失败") &&
			(strings.Contains(message, "\n") || !strings.Contains(message, partitionNormalA) || !strings.Contains(message, partitionNormalB)) {
			t.Fatalf("首轮预热失败应写成一行并带上失败的账户: %q", message)
		}
	}
}

// TestStartRunningWhenAllAccountsNeedLogin 启用账户全部需要登录、目录缓存还在时不算启动失败（与上游 c77dd89 相同，文档按此描述）：
// Start 照常进入运行，首轮预热只写一行“没有可预热的账户”的 WARN，请求按没有符合条件的账户返回（503）；账户恢复后 keepWarm 自动补齐 Worker，
// 不需要重启服务。原先这种情况按没有可用账户启动失败，停在 STOPPED
func TestStartRunningWhenAllAccountsNeedLogin(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	for _, accountID := range []string{partitionNormalA, partitionNormalB} {
		if err := env.pool.MarkAuthRequired(accountID, "HTTP 401"); err != nil {
			t.Fatal(err)
		}
	}
	env.start(t)
	if !waitUntil(5*time.Second, func() bool {
		return hasLogContaining(env.requests, "WAA Worker 首轮预热失败", "没有可预热的账户", "稍后自动重试")
	}) {
		t.Fatalf("没有可预热的账户时首轮预热应写 WARN，日志 = %v", serviceLogMessages(env.requests))
	}
	if state := env.service.State(); state != "RUNNING" {
		t.Fatalf("状态为 %s，期望保持 RUNNING", state)
	}
	_, err := env.service.acquireWarmLeaseNow(env.ctx, normalSelection(), nil)
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) {
		t.Fatalf("选号返回 %v，期望 *aistudio.AccountsNotReadyError（503）", err)
	}
	if err := env.pool.MarkReady(partitionNormalA); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(3*keepWarmInterval, func() bool { return slices.Equal(env.manager.WarmAccountIDs(), []string{partitionNormalA}) }) {
		t.Fatalf("账户恢复后 keepWarm 应补齐 Worker，热 Worker = %v，冷启动 = %v", env.manager.WarmAccountIDs(), env.launches())
	}
	if state := env.service.State(); state != "RUNNING" {
		t.Fatalf("状态为 %s，期望保持 RUNNING", state)
	}
}

// TestRequestWaitsForFirstWarmWorker 首轮预热出结果之前到达的请求（经 Generate）等预热的第一个 Worker，就绪后在它上面生成，
// 不另起冷启动；不等待的选号此时按“等待首个 Worker 预热”没有立即可用的账号
func TestRequestWaitsForFirstWarmWorker(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	release := env.gate(partitionNormalA, partitionNormalB)
	defer release()
	env.start(t)
	warming := env.waitLaunched(t, 1)[0]
	if reason := firstWarmReason(t, env.service, env.ctx); reason != waitReasonFirstWarm {
		t.Fatalf("首轮预热出结果前不等待的选号原因 = %q，期望 %q", reason, waitReasonFirstWarm)
	}
	ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal), 10*time.Second)
	defer cancel()
	events, err := env.service.Generate(ctx, aistudio.GenerateRequest{
		ID: "req-first-warm", Model: testRuntimeModel,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	})
	if err != nil {
		t.Fatalf("运行中的服务应接受请求: %v", err)
	}
	waitQueued(t, env.manager)
	time.Sleep(3 * schedulingRecheck / 2)
	if launched := env.launches(); len(launched) != 1 {
		t.Fatalf("请求等待期间冷启动了 %v，期望只有首轮预热的 %s", launched, warming)
	}
	release()
	var text string
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			err = event.Err
		}
	}
	if err != nil || text != "hello" {
		t.Fatalf("text=%q err=%v，期望在首轮预热的 Worker 上生成成功", text, err)
	}
	env.upstream.mu.Lock()
	calls := slices.Clone(env.upstream.calls)
	env.upstream.mu.Unlock()
	if !slices.Equal(calls, []string{warming}) {
		t.Fatalf("上游收到的账户 = %v，期望只有首轮预热的 %s", calls, warming)
	}
	if launched := env.launches(); len(launched) != 1 {
		t.Fatalf("冷启动 = %v，期望只有首轮预热的一个", launched)
	}
}

// TestFirstWarmPartitionsIndependent 各分区分别等自己的首轮预热：Ultra 先就绪时 /ultra 请求立即用它，普通请求仍等普通分区；
// 普通先就绪时普通请求立即用它，/ultra 请求仍等 Ultra 分区；等待期间都不另起冷启动
func TestFirstWarmPartitionsIndependent(t *testing.T) {
	for _, first := range []aistudio.PoolScope{aistudio.PoolScopeUltra, aistudio.PoolScopeNormal} {
		t.Run(first.String()+" 先就绪", func(t *testing.T) {
			env := newFirstWarmEnv(t,
				[]string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraC}, 1, 2, 1, 2)
			releaseNormal := env.gate(partitionNormalA, partitionNormalB)
			releaseUltra := env.gate(partitionUltraA, partitionUltraC)
			defer releaseNormal()
			defer releaseUltra()
			env.start(t)
			env.waitLaunched(t, 2)
			if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal", "ultra"}) {
				t.Fatalf("两个分区都在首轮预热时待定分区 = %v", pending)
			}
			normalCtx := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormal)
			ultraCtx := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeUltra)
			ready, waiting := ultraCtx, normalCtx
			readyPartition, waitingName := aistudio.PoolScopeUltra, "normal"
			if first == aistudio.PoolScopeUltra {
				releaseUltra()
			} else {
				releaseNormal()
				ready, waiting = normalCtx, ultraCtx
				readyPartition, waitingName = aistudio.PoolScopeNormal, "ultra"
			}
			if !waitUntil(5*time.Second, func() bool { return len(env.manager.warmAccountIDsIn(readyPartition)) == 1 }) {
				t.Fatalf("%s 分区的首轮预热没有就绪", readyPartition)
			}
			if !waitUntil(5*time.Second, func() bool {
				return slices.Equal(env.manager.firstWarmPendingNames(), []string{waitingName})
			}) {
				t.Fatalf("待定分区 = %v，期望只剩 %s", env.manager.firstWarmPendingNames(), waitingName)
			}
			lease, err := env.service.acquireWarmLeaseNow(ready, normalSelection(), nil)
			if err != nil {
				t.Fatalf("%s 请求应立即用本分区已就绪的 Worker，实际 %v", readyPartition, err)
			}
			if got := partitionOf(lease.Account().ID, env.pool.PartitionMembers()); got != readyPartition {
				t.Fatalf("取得的账户 %s 属于 %s", lease.Account().ID, got)
			}
			_ = lease.Release()
			if reason := firstWarmReason(t, env.service, waiting); reason != waitReasonFirstWarm {
				t.Fatalf("%s 请求原因 = %q，期望仍等本分区的首轮预热", waitingName, reason)
			}
			if launched := env.launches(); len(launched) != 2 {
				t.Fatalf("冷启动 = %v，期望只有两个分区首轮预热的各一个", launched)
			}
		})
	}
}

// TestFirstWarmCrossPartitionScopeWaitsByStandbyPartition 请求号池跨普通与 Ultra 两个分区（ULTRA_EXCLUSIVE=false、PRO_POOL=true 时
// 普通路径为 PoolScopeNormalUltra），普通分区首轮还在启动、Ultra 分区已出结果（常驻数 0）：
//   - 普通账户也能服务的模型，备用账号有落在普通分区的，仍等普通分区的首个 Worker，不另起冷启动；
//   - 只有 Ultra 账户能服务的模型（ULTRA_ONLY_MODELS 这类权益规则下的模型），备用账号都在已出结果的 Ultra 分区，不被普通分区的首轮挡住，
//     立即现场冷启动那个 Ultra 账户（原先按号池判断，一直等到普通分区的首个 Worker 或请求期限）
func TestFirstWarmCrossPartitionScopeWaitsByStandbyPartition(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, []string{partitionUltraA, partitionUltraB}, 1, 2, 0, 2)
	release := env.gate(partitionNormalA, partitionNormalB)
	defer release()
	env.start(t)
	warming := env.waitLaunched(t, 1)[0]
	if !waitUntil(5*time.Second, func() bool {
		return slices.Equal(env.manager.firstWarmPendingNames(), []string{"normal"})
	}) {
		t.Fatalf("待定分区 = %v，期望只有普通分区（Ultra 常驻数为 0，第一轮就清除）", env.manager.firstWarmPendingNames())
	}
	scoped := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeNormalUltra)
	if reason := firstWarmReason(t, env.service, scoped); reason != waitReasonFirstWarm {
		t.Fatalf("普通账户也能服务的模型原因 = %q，期望仍等普通分区的首轮预热", reason)
	}
	ctx, cancel := context.WithTimeout(scoped, 3*time.Second)
	defer cancel()
	startedAt := time.Now()
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: partitionUltraModel, Method: "generateContent"})
	if err != nil {
		t.Fatalf("只有 %s 能服务的模型被普通分区的首轮挡住: %v（用时 %s，冷启动 %v，待定分区 %v）",
			partitionUltraB, err, time.Since(startedAt).Round(time.Millisecond), env.launches(), env.manager.firstWarmPendingNames())
	}
	if lease.Account().ID != partitionUltraB {
		t.Fatalf("取得的账户 = %s，期望 %s", lease.Account().ID, partitionUltraB)
	}
	_ = lease.Release()
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("选号用了 %s，期望立即现场冷启动", elapsed)
	}
	if launched := env.launches(); !slices.Equal(launched, []string{warming, partitionUltraB}) {
		t.Fatalf("冷启动 = %v，期望只有普通分区首轮的 %s 与现场冷启动的 %s", launched, warming, partitionUltraB)
	}
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal"}) {
		t.Fatalf("普通分区的首轮状态不应受影响: %v", pending)
	}
}

// TestFirstWarmReleasesPartitionWithoutCandidates 分区的首轮预热没有可启动的账户（候选都在预热失败冷却中，或该分区常驻数为 0）时
// 立即清除，不让请求等：/ultra 请求按普通路径现场冷启动 Ultra 账户，普通分区仍在首轮预热中也不影响它
func TestFirstWarmReleasesPartitionWithoutCandidates(t *testing.T) {
	for _, test := range []struct {
		name      string
		ultraWarm int
		prepare   func(env *firstWarmEnv)
	}{
		{name: "候选都在预热失败冷却中", ultraWarm: 1, prepare: func(env *firstWarmEnv) { env.manager.noteWarmFailure(partitionUltraA) }},
		{name: "Ultra 常驻数为 0", ultraWarm: 0, prepare: func(*firstWarmEnv) {}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newFirstWarmEnv(t, []string{partitionNormalA}, []string{partitionUltraA}, 1, 2, test.ultraWarm, 2)
			releaseNormal := env.gate(partitionNormalA)
			defer releaseNormal()
			test.prepare(env)
			env.start(t)
			env.waitLaunched(t, 1)
			if !waitUntil(5*time.Second, func() bool {
				return slices.Equal(env.manager.firstWarmPendingNames(), []string{"normal"})
			}) {
				t.Fatalf("待定分区 = %v，期望只有普通分区", env.manager.firstWarmPendingNames())
			}
			ctx, cancel := context.WithTimeout(aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeUltra), 3*time.Second)
			defer cancel()
			startedAt := time.Now()
			lease, err := env.service.acquireWarmLease(ctx, normalSelection())
			if err != nil {
				t.Fatalf("/ultra 请求不应等没有候选的 Ultra 分区: %v", err)
			}
			defer lease.Release()
			if lease.Account().ID != partitionUltraA || time.Since(startedAt) > 2*time.Second {
				t.Fatalf("取得 %s，用时 %s，期望立即现场冷启动 Ultra 账户", lease.Account().ID, time.Since(startedAt))
			}
		})
	}
}

// TestFirstWarmWaitHonorsDeadline 首轮预热一直没有结果时，等待受请求期限约束：期限到时按期限结束，期间不另起冷启动
func TestFirstWarmWaitHonorsDeadline(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	release := env.gate(partitionNormalA, partitionNormalB)
	defer release()
	env.start(t)
	env.waitLaunched(t, 1)
	ctx, cancel := context.WithTimeout(env.ctx, 400*time.Millisecond)
	defer cancel()
	startedAt := time.Now()
	lease, err := env.service.acquireWarmLease(ctx, normalSelection())
	if err == nil {
		_ = lease.Release()
		t.Fatal("首轮预热没有结果时请求不应取得租约")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望按请求期限结束", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("期限 400ms 的请求 %s 才结束", elapsed)
	}
	if launched := env.launches(); len(launched) != 1 {
		t.Fatalf("冷启动 = %v，期望只有首轮预热的一个", launched)
	}
}

// TestPrimaryBudgetCreditsFirstWarm 主用模型等首轮预热的第一个 Worker 与等后台扩容一样按现场冷启动抵扣（合计不超过 INIT_TIMEOUT），
// 排队上限一到不会就按 429 结束；抵扣用完后按普通容量原因处理
func TestPrimaryBudgetCreditsFirstWarm(t *testing.T) {
	budget := &leaseWaitBudget{limit: 50 * time.Millisecond, startedAt: time.Now(), expansionCredit: time.Second, modelID: testRuntimeModel}
	ctx := context.Background()
	for range 4 {
		if err := budget.wait(ctx, nil, 60*time.Millisecond, waitReasonFirstWarm); err != nil {
			t.Fatalf("等首轮预热时不应按排队超限结束，实际 %v", err)
		}
	}
	if budget.coldStart < 200*time.Millisecond || budget.expired() {
		t.Fatalf("抵扣 = %s，截止时间应已延后", budget.coldStart)
	}
	limited := &leaseWaitBudget{limit: 20 * time.Millisecond, startedAt: time.Now(), expansionCredit: 30 * time.Millisecond, modelID: testRuntimeModel}
	var err error
	for range 10 {
		if err = limited.wait(ctx, nil, 20*time.Millisecond, waitReasonFirstWarm); err != nil {
			break
		}
	}
	if !isCapacityBusy(err) || limited.coldStart > 30*time.Millisecond {
		t.Fatalf("抵扣用完后应按排队超限结束: err=%v 抵扣=%s", err, limited.coldStart)
	}
}

// TestServiceStepsAndStatus 启动与停止的步骤：经状态推送依次看到 start_prewarm，进入运行后清除；停止依次经过 cancel_requests、
// close_workers、flush_writes 后清除；状态接口带服务端时刻，有步骤时带步骤与进入时刻
func TestServiceStepsAndStatus(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	admin := &runtimeAdmin{lifecycle: env.ctx, pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
	var mu sync.Mutex
	var steps []string
	var stepStatus api.AdminStatus
	env.service.setStatusPublisher(func() {
		status, err := admin.Status(env.ctx)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if len(steps) == 0 || steps[len(steps)-1] != status.Transition {
			steps = append(steps, status.Transition)
		}
		if status.Transition == stepCloseWorkers {
			stepStatus = status
		}
	})
	launching := ""
	models, started, err := env.service.Start(env.ctx, func() { launching, _ = env.service.Step() })
	if err != nil || !started || len(models) == 0 {
		t.Fatalf("Start = (%d, %t, %v)", len(models), started, err)
	}
	if launching != stepPrepareCatalog {
		t.Fatalf("启动回调时的步骤 = %q，期望 %s", launching, stepPrepareCatalog)
	}
	mu.Lock()
	got := slices.Clone(steps)
	mu.Unlock()
	if !slices.Equal(got, []string{stepStartPrewarm, ""}) {
		t.Fatalf("启动步骤推送 = %v，期望 [start_prewarm 清除]", got)
	}
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Transition != "" || status.TransitionSince != nil || status.Time.IsZero() || time.Since(status.Time) > time.Minute {
		t.Fatalf("运行中的状态 transition=%q since=%v time=%v", status.Transition, status.TransitionSince, status.Time)
	}
	if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
		t.Fatal("首轮预热没有就绪")
	}
	mu.Lock()
	steps = nil
	mu.Unlock()
	if stopped, err := env.service.Stop(); err != nil || !stopped {
		t.Fatalf("Stop = (%t, %v)", stopped, err)
	}
	mu.Lock()
	got, closing := slices.Clone(steps), stepStatus
	mu.Unlock()
	if !slices.Equal(got, []string{stepCancelRequests, stepCloseWorkers, stepFlushWrites, ""}) {
		t.Fatalf("停止步骤推送 = %v", got)
	}
	if closing.State != "STOPPED" || closing.TransitionSince == nil || closing.TransitionSince.After(closing.Time) ||
		closing.Time.Sub(*closing.TransitionSince) > time.Minute {
		t.Fatalf("关闭 Worker 步骤的状态 state=%s since=%v time=%v", closing.State, closing.TransitionSince, closing.Time)
	}
	if encoded, err := json.Marshal(closing); err != nil || !strings.Contains(string(encoded), `"transition":"close_workers","transition_since":"`) {
		t.Fatalf("状态 JSON = %s（%v），期望带 transition 与 transition_since", encoded, err)
	}
	if name, since := env.service.Step(); name != "" || !since.IsZero() {
		t.Fatalf("停止后步骤 = %q %v，期望清除", name, since)
	}
}

// TestStartFailureKeepsLaunchSemantics 真正的启动失败（没有目录缓存、首轮目录同步后仍没有模型）照旧按启动失败返回、回到停止，
// 清理步骤经过 wait_catalog、close_workers、flush_writes 后清除，不进入运行
func TestStartFailureKeepsLaunchSemantics(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	env.service.catalog = emptyCatalog{}
	var mu sync.Mutex
	var steps []string
	env.service.setStatusPublisher(func() {
		name, _ := env.service.Step()
		mu.Lock()
		steps = append(steps, name)
		mu.Unlock()
	})
	_, started, err := env.service.Start(env.ctx, nil)
	if !errors.Is(err, aistudio.ErrNoEligibleAccount) || started {
		t.Fatalf("Start = (%t, %v)，期望按没有可用账户失败", started, err)
	}
	if state := env.service.State(); state != "STOPPED" {
		t.Fatalf("启动失败后状态为 %s", state)
	}
	mu.Lock()
	got := slices.Clone(steps)
	mu.Unlock()
	if !slices.Equal(got, []string{stepWaitCatalog, stepCloseWorkers, stepFlushWrites, ""}) {
		t.Fatalf("启动失败的步骤推送 = %v", got)
	}
	if launched := env.launches(); len(launched) != 0 {
		t.Fatalf("启动失败不应预热: %v", launched)
	}
}

// TestStartupLoginPageWithBackgroundPrewarm 首轮预热在后台进行时启动落到登录页的保护照旧：A 落到 Google 登录页标需要登录
// （来源=启动），B 补上驻留，服务一直在运行
func TestStartupLoginPageWithBackgroundPrewarm(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	env.fail[partitionNormalA] = camoufoxLoginPageError()
	// 两个同时启动：B 停在启动中，直到 A 已被标为需要登录
	releaseB := env.gate(partitionNormalB)
	defer releaseB()
	env.start(t)
	if !waitUntil(5*time.Second, func() bool {
		return testStatus(t, env.pool, partitionNormalA).State == aistudio.AccountAuthRequired
	}) {
		t.Fatalf("A 应标为需要登录，日志 = %v", serviceLogMessages(env.requests))
	}
	releaseB()
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(env.manager.WarmAccountIDs(), partitionNormalB) }) {
		t.Fatalf("B 应驻留，驻留 = %v", env.manager.WarmAccountIDs())
	}
	if !hasLogContaining(env.requests, "账号需要登录 | 来源=启动") {
		t.Fatalf("应写“账号需要登录 | 来源=启动”，日志 = %v", serviceLogMessages(env.requests))
	}
	if state := env.service.State(); state != "RUNNING" {
		t.Fatalf("状态为 %s，期望一直运行", state)
	}
}

// TestStatusDedupeIgnoresTime 管理页连接的状态推送去重不比较生成时刻：内容相同、只是 time 不同的状态快照不再发出，内容变化时照常发出，
// 其他事件不受影响
func TestStatusDedupeIgnoresTime(t *testing.T) {
	var dedupe statusDedupe
	status := api.AdminStatus{State: "RUNNING", Running: true, Time: time.Now()}
	if !dedupe.fresh(api.AdminEvent{Type: "status", Data: status}) {
		t.Fatal("第一份状态快照应发出")
	}
	status.Time = status.Time.Add(time.Second)
	if dedupe.fresh(api.AdminEvent{Type: "status", Data: status}) {
		t.Fatal("只是生成时刻不同的状态快照不应再发出")
	}
	status.Transition = stepCloseWorkers
	if !dedupe.fresh(api.AdminEvent{Type: "status", Data: status}) {
		t.Fatal("步骤变化后的状态快照应发出")
	}
	if !dedupe.fresh(api.AdminEvent{Type: "log", Data: api.AdminLog{Message: "x"}}) ||
		!dedupe.fresh(api.AdminEvent{Type: "log", Data: api.AdminLog{Message: "x"}}) {
		t.Fatal("其他事件不去重")
	}
}

// TestFirstWarmWarmOnlyReturnsWithoutExpanding 只用已启动 Worker 的选号（额度 429 之后那次）在首轮预热出结果前不等、也不另发起后台扩容：
// 按额度 429 后没有已启动的 Worker 立即返回，首轮预热就是那次扩容
func TestFirstWarmWarmOnlyReturnsWithoutExpanding(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	release := env.gate(partitionNormalA, partitionNormalB)
	defer release()
	env.start(t)
	env.waitLaunched(t, 1)
	ctx, cancel := context.WithTimeout(withoutColdStart(env.ctx), 3*time.Second)
	defer cancel()
	startedAt := time.Now()
	lease, err := env.service.acquireWarmLease(ctx, normalSelection())
	if err == nil {
		_ = lease.Release()
		t.Fatal("没有已启动的 Worker 时不应取得租约")
	}
	if !noWarmWorker(err) || time.Since(startedAt) > time.Second {
		t.Fatalf("err = %v（%s），期望立即按额度 429 后没有已启动的 Worker 返回", err, time.Since(startedAt))
	}
	time.Sleep(100 * time.Millisecond)
	if launched := env.launches(); len(launched) != 1 {
		t.Fatalf("冷启动 = %v，期望不另发起后台扩容", launched)
	}
}

// TestFirstWarmOnlyForInitialPrewarm 只有生成服务启动时的首轮置位：keepWarm、热更新与淘汰后的补齐（StartPrewarm）不让请求等；
// 首轮没能开始（已有一轮在进行）时立即全部清除
func TestFirstWarmOnlyForInitialPrewarm(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	release := env.gate(partitionNormalA)
	defer release()
	env.manager.StartPrewarm(env.ctx)
	env.waitLaunched(t, 1)
	if pending := env.manager.firstWarmPendingNames(); len(pending) != 0 {
		t.Fatalf("补齐预热不应置位首轮状态: %v", pending)
	}
	if err := <-env.manager.startFirstPrewarm(env.ctx); err == nil || !strings.Contains(err.Error(), "WAA 预热已在进行") {
		t.Fatalf("已有一轮在进行时首轮应立即返回，实际 %v", err)
	}
	if pending := env.manager.firstWarmPendingNames(); len(pending) != 0 {
		t.Fatalf("首轮没能开始时应全部清除，待定分区 = %v", pending)
	}
}

// TestFirstWarmClearedOnStop 首轮预热进行中停止服务：这一轮随服务停止结束，各分区的首轮状态全部清除（状态接口不再显示等待首个 Worker）
func TestFirstWarmClearedOnStop(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	release := env.gate(partitionNormalA)
	defer release()
	env.start(t)
	env.waitLaunched(t, 1)
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal"}) {
		t.Fatalf("首轮预热进行中待定分区 = %v", pending)
	}
	admin := &runtimeAdmin{lifecycle: env.ctx, pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Workers.Prewarm.FirstWarmPending, []string{"normal"}) || !strings.Contains(string(encoded), `"first_warm_pending":["normal"]`) ||
		!strings.Contains(string(encoded), `"time":"`) || strings.Contains(string(encoded), `"transition"`) {
		t.Fatalf("首轮预热进行中的状态 = %s", encoded)
	}
	if stopped, err := env.service.Stop(); err != nil || !stopped {
		t.Fatalf("Stop = (%t, %v)", stopped, err)
	}
	if pending := env.manager.firstWarmPendingNames(); len(pending) != 0 {
		t.Fatalf("停止后待定分区 = %v，期望全部清除", pending)
	}
	if status, _ := admin.Status(env.ctx); len(status.Workers.Prewarm.FirstWarmPending) != 0 {
		t.Fatalf("停止后状态里的待定分区 = %v", status.Workers.Prewarm.FirstWarmPending)
	}
}

// waitFillRounds 等首轮预热的循环在调用之后再开始 rounds 轮（每轮开始时写 fillLoopAt）：rounds 为 2 时调用之前已经发生的事件
// 至少被完整的一轮看到
func waitFillRounds(t *testing.T, manager *accountWorkerManager, rounds int) {
	t.Helper()
	last := manager.fillLoopAt.Load()
	for range rounds {
		if !waitUntil(5*time.Second, func() bool { return manager.fillLoopAt.Load() != last }) {
			t.Fatal("首轮预热的循环没有继续")
		}
		last = manager.fillLoopAt.Load()
	}
}

// TestFirstWarmPendingWhileLaunchInflight 分区还有首轮预热的启动在进行（B 停在启动中）、另一个首轮账户 A 已启动失败、本轮没有新的候选时，
// 首轮状态不清除：请求照旧等 B，不现场冷启动已经失败过的 A；B 就绪后清除，请求在 B 上取得租约
func TestFirstWarmPendingWhileLaunchInflight(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 3, 0, 1)
	env.fail[partitionNormalA] = errors.New("Camoufox 进程退出（测试）")
	releaseB := env.gate(partitionNormalB)
	defer releaseB()
	env.start(t)
	env.waitLaunched(t, 2)
	if !waitUntil(5*time.Second, func() bool {
		_, failed := env.manager.recentWarmFailures(time.Now())[partitionNormalA]
		return failed
	}) {
		t.Fatalf("A 的启动失败没有被首轮记下，日志 = %v", serviceLogMessages(env.requests))
	}
	waitFillRounds(t, env.manager, 2)
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal"}) {
		t.Fatalf("B 还在启动中时待定分区 = %v，期望仍为 [normal]", pending)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 500*time.Millisecond)
	defer cancel()
	if lease, err := env.service.acquireWarmLease(ctx, normalSelection()); err == nil {
		_ = lease.Release()
		t.Fatalf("B 还在启动中时请求取得了 %s，期望等首轮预热", lease.Account().ID)
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望等首轮预热直到请求期限", err)
	}
	if launched := env.launches(); len(launched) != 2 {
		t.Fatalf("首轮还有启动在进行时请求另起了冷启动: %v", launched)
	}
	releaseB()
	waitCtx, waitCancel := context.WithTimeout(env.ctx, 5*time.Second)
	defer waitCancel()
	lease, err := env.service.acquireWarmLease(waitCtx, normalSelection())
	if err != nil {
		t.Fatalf("B 就绪后请求应取得租约: %v", err)
	}
	defer lease.Release()
	if lease.Account().ID != partitionNormalB || len(env.launches()) != 2 {
		t.Fatalf("取得 %s、冷启动 %v，期望用首轮预热的 B", lease.Account().ID, env.launches())
	}
	if !waitUntil(5*time.Second, func() bool { return len(env.manager.firstWarmPendingNames()) == 0 }) {
		t.Fatalf("B 就绪后待定分区 = %v", env.manager.firstWarmPendingNames())
	}
}

// TestFixedAccountSkipsFirstWarm 固定账户的选号（显式账户、文件所属账户）在首轮预热出结果前不等首个 Worker，直接现场启动该账户
func TestFixedAccountSkipsFirstWarm(t *testing.T) {
	for _, test := range []struct {
		name      string
		selection func(env *firstWarmEnv, accountID string) aistudio.AccountSelection
	}{
		{name: "显式账户", selection: func(_ *firstWarmEnv, accountID string) aistudio.AccountSelection {
			selection := normalSelection()
			selection.AccountID = accountID
			return selection
		}},
		{name: "文件所属账户", selection: func(env *firstWarmEnv, accountID string) aistudio.AccountSelection {
			if err := env.pool.BindResource("files/first-warm-test", accountID); err != nil {
				t.Fatal(err)
			}
			selection := normalSelection()
			selection.ResourceID = "files/first-warm-test"
			return selection
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
			releaseA := env.gate(partitionNormalA)
			releaseB := env.gate(partitionNormalB)
			defer releaseA()
			defer releaseB()
			env.start(t)
			warming := env.waitLaunched(t, 1)[0]
			fixed, releaseFixed := partitionNormalB, releaseB
			if warming == partitionNormalB {
				fixed, releaseFixed = partitionNormalA, releaseA
			}
			releaseFixed()
			ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
			defer cancel()
			startedAt := time.Now()
			lease, err := env.service.acquireWarmLease(ctx, test.selection(env, fixed))
			if err != nil {
				t.Fatalf("固定到 %s 的选号不应等首轮预热的 %s: %v", fixed, warming, err)
			}
			defer lease.Release()
			if lease.Account().ID != fixed || time.Since(startedAt) > 2*time.Second {
				t.Fatalf("取得 %s，用时 %s，期望立即现场启动 %s", lease.Account().ID, time.Since(startedAt), fixed)
			}
			if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal"}) {
				t.Fatalf("首轮预热的 %s 还在启动，待定分区 = %v", warming, pending)
			}
		})
	}
}

// TestFirstWarmSettlesPartitionOnClassifyFailure 分区的候选分类失败、该分区又没有进行中的启动时首轮状态立即清除，不等别的分区：
// 账户超过 bootstrapCacheMinAccounts 时预热概况缓存 bootstrapSummaryTTL，期间 Ultra 账户被删光，概况仍按有 Ultra 账户给 Ultra 分区常驻数，
// 分类则因 Ultra 号池为空失败。普通分区的首轮启动停在启动中，Ultra 分区仍应在第一轮清除
func TestFirstWarmSettlesPartitionOnClassifyFailure(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 2, 1, 2)
	release := env.gate(partitionNormalA)
	defer release()
	stale := env.manager.pool.BootstrapSummary()
	stale.Available, stale.UltraAvailable = bootstrapCacheMinAccounts+2, 1
	env.manager.bootstrapMu.Lock()
	env.manager.bootstrapCache, env.manager.bootstrapAt, env.manager.bootstrapVersion = stale, time.Now(), env.pool.PoolRuleVersion()
	env.manager.bootstrapMu.Unlock()
	if target := env.manager.fillTargetFor(aistudio.PoolScopeUltra, env.manager.bootstrapSummary()); target != 1 {
		t.Fatalf("过期概况下 Ultra 分区常驻数 = %d，期望 1", target)
	}
	if _, err := env.manager.classifyBootstrapCandidates(env.ctx, nil, aistudio.PoolScopeUltra); err == nil {
		t.Fatal("Ultra 号池为空时分类应失败")
	}
	env.start(t)
	env.waitLaunched(t, 1)
	waitFillRounds(t, env.manager, 2)
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"normal"}) {
		t.Fatalf("Ultra 分区分类失败后待定分区 = %v，期望只剩普通分区", pending)
	}
}

// TestFirstWarmClassifyFailureKeepsPendingWhileLaunchInflight 分区的候选分类失败、但该分区还有首轮预热的启动在进行时不清除首轮状态：
// Ultra 分区首轮的 A 停在启动中（B 在预热失败冷却中，不在首轮候选里），随后两个 Ultra 账户的目录都不再含预热概况缓存里列的模型，
// 下一轮 Ultra 分区分类失败。首轮状态仍待定，/ultra 请求照旧等 A，不现场冷启动 B（与 A 同时占用冷启动名额）
func TestFirstWarmClassifyFailureKeepsPendingWhileLaunchInflight(t *testing.T) {
	env := newFirstWarmEnv(t, nil, []string{partitionUltraA, partitionUltraB}, 1, 2, 2, 3)
	release := env.gate(partitionUltraA)
	defer release()
	env.manager.noteWarmFailure(partitionUltraB)
	env.start(t)
	if launched := env.waitLaunched(t, 1); launched[0] != partitionUltraA {
		t.Fatalf("首轮预热启动了 %v，期望 %s", launched, partitionUltraA)
	}
	waitFillRounds(t, env.manager, 1)
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"ultra"}) {
		t.Fatalf("A 还在启动中时待定分区 = %v，期望 [ultra]", pending)
	}
	ultraOnly := []aistudio.Model{{ID: partitionUltraModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for _, accountID := range []string{partitionUltraA, partitionUltraB} {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierUltra, ultraOnly); err != nil {
			t.Fatal(err)
		}
	}
	stale := aistudio.BootstrapSummary{Available: bootstrapCacheMinAccounts + 2, UltraAvailable: 2, ModelIDs: []string{testRuntimeModel}}
	env.manager.bootstrapMu.Lock()
	env.manager.bootstrapCache, env.manager.bootstrapAt, env.manager.bootstrapVersion = stale, time.Now(), env.pool.PoolRuleVersion()
	env.manager.bootstrapMu.Unlock()
	if _, err := env.manager.classifyBootstrapCandidates(env.ctx, nil, aistudio.PoolScopeUltra); err == nil {
		t.Fatal("Ultra 账户的目录都不含概况里的模型时分类应失败")
	}
	if slots := env.manager.fillTargetFor(aistudio.PoolScopeUltra, env.manager.bootstrapSummary()) - 1; slots <= 0 {
		t.Fatalf("Ultra 分区还应有空位（常驻数减进行中的启动 = %d），否则这一轮不分类", slots)
	}
	waitFillRounds(t, env.manager, 2)
	if pending := env.manager.firstWarmPendingNames(); !slices.Equal(pending, []string{"ultra"}) {
		t.Fatalf("分类失败但 A 还在启动中时待定分区 = %v，期望仍为 [ultra]", pending)
	}
	ultraCtx := aistudio.ContextWithPoolScope(env.ctx, aistudio.PoolScopeUltra)
	ctx, cancel := context.WithTimeout(ultraCtx, 500*time.Millisecond)
	defer cancel()
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: partitionUltraModel, Method: "generateContent"})
	if err == nil {
		_ = lease.Release()
		t.Fatalf("A 还在启动中时请求取得了 %s，期望等首轮预热", lease.Account().ID)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v，期望等首轮预热直到请求期限", err)
	}
	if launched := env.launches(); !slices.Equal(launched, []string{partitionUltraA}) {
		t.Fatalf("首轮还有启动在进行时请求另起了冷启动: %v", launched)
	}
}

// staticAdminSource 为管理事件流测试的固定快照来源
type staticAdminSource struct {
	status api.AdminStatus
}

func (source staticAdminSource) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (source staticAdminSource) Status(context.Context) (api.AdminStatus, error) {
	return source.status, nil
}

func (source staticAdminSource) Accounts(context.Context) ([]api.AdminAccount, error) {
	return nil, nil
}

func (source staticAdminSource) Cooldowns(context.Context) ([]api.AdminCooldown, error) {
	return nil, nil
}

// TestAdminEventsDedupeStatus 管理页事件流（openAdminEvents）对状态快照去重：只差生成时刻的状态不再发出，内容变化的照常发出、
// 同一内容只发一次，其他事件不受影响
func TestAdminEventsDedupeStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	requests := newRequestRegistry(ctx)
	status := api.AdminStatus{State: "RUNNING", Running: true, Time: time.Now()}
	events, err := openAdminEvents(ctx, ctx, requests, staticAdminSource{status: status})
	if err != nil {
		t.Fatal(err)
	}
	next := func() api.AdminEvent {
		t.Helper()
		select {
		case event, ok := <-events:
			if !ok {
				t.Fatal("事件流提前关闭")
			}
			return event
		case <-ctx.Done():
			t.Fatal("等事件超时")
		}
		return api.AdminEvent{}
	}
	for _, want := range []string{"status", "models", "accounts", "cooldowns"} {
		if event := next(); event.Type != want {
			t.Fatalf("初始事件 %s，期望 %s", event.Type, want)
		}
	}
	// 每发布一份状态就跟一条日志并读到它为止：订阅者的待发队列会合并还没发出的同类快照，逐份读才能看到事件流本身的去重
	statusesUntil := func(published api.AdminStatus, sentinel string) []string {
		t.Helper()
		requests.publish(api.AdminEvent{Type: "status", Data: published})
		requests.publish(api.AdminEvent{Type: "log", Data: api.AdminLog{Message: sentinel}})
		var got []string
		for {
			event := next()
			if entry, ok := event.Data.(api.AdminLog); ok && event.Type == "log" && entry.Message == sentinel {
				return got
			}
			if sent, ok := event.Data.(api.AdminStatus); ok && event.Type == "status" {
				got = append(got, sent.Transition)
			}
		}
	}
	later := status
	later.Time = status.Time.Add(2 * time.Second)
	if got := statusesUntil(later, "same-content"); len(got) != 0 {
		t.Fatalf("只差生成时刻的状态快照又发出了 %d 份", len(got))
	}
	stepping := later
	stepping.Transition = stepCloseWorkers
	if got := statusesUntil(stepping, "step-changed"); !slices.Equal(got, []string{stepCloseWorkers}) {
		t.Fatalf("步骤变化后发出的状态 = %v，期望一份 %s", got, stepCloseWorkers)
	}
	stepping.Time = stepping.Time.Add(time.Second)
	if got := statusesUntil(stepping, "step-same"); len(got) != 0 {
		t.Fatalf("步骤不变、只差生成时刻的状态快照又发出了 %d 份", len(got))
	}
}

// TestFirstWarmSettlesOnFirstReadyWorker 分区的第一个 Worker 就绪就清除首轮状态，不等同分区首轮的其他启动结束（管理页不再显示等首个 Worker）
func TestFirstWarmSettlesOnFirstReadyWorker(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 2, 2, 0, 1)
	releaseA := env.gate(partitionNormalA)
	releaseB := env.gate(partitionNormalB)
	defer releaseA()
	defer releaseB()
	env.start(t)
	env.waitLaunched(t, 2)
	releaseA()
	if !waitUntil(5*time.Second, func() bool { return slices.Contains(env.manager.ReadyWarmAccountIDs(), partitionNormalA) }) {
		t.Fatal("A 没有就绪")
	}
	if !waitUntil(3*time.Second, func() bool { return len(env.manager.firstWarmPendingNames()) == 0 }) {
		t.Fatalf("A 已就绪、B 还在启动中时待定分区 = %v，期望已清除", env.manager.firstWarmPendingNames())
	}
	if slices.Contains(env.manager.ReadyWarmAccountIDs(), partitionNormalB) {
		t.Fatal("B 应仍在启动中")
	}
}

// TestFirstWarmNotStartedWhenClosed 管理器已关闭（生成服务重建时旧一代已关闭）时首轮没能开始：立即返回错误并全部清除，不留下待定分区
func TestFirstWarmNotStartedWhenClosed(t *testing.T) {
	manager, _, _ := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-manager.startFirstPrewarm(context.Background()); err == nil || !strings.Contains(err.Error(), "已关闭") {
		t.Fatalf("管理器已关闭时首轮应立即返回，实际 %v", err)
	}
	if pending := manager.firstWarmPendingNames(); len(pending) != 0 {
		t.Fatalf("首轮没能开始时应全部清除，待定分区 = %v", pending)
	}
}

// firstWarmOtherModel 为只有一个账户能服务的模型（TestFirstWarmNoWaitWithoutStandby）
const firstWarmOtherModel = "test-model-other"

// TestFirstWarmNoWaitWithoutStandby 首轮预热出结果前，请求的模型没有要冷启动的备用账号（只有冷却中的账户能服务它，首轮启动中的账户服务不了）时
// 不等首个 Worker，照常按全部冷却处理并立即返回，不另起冷启动
func TestFirstWarmNoWaitWithoutStandby(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 2, 0, 1)
	releaseA := env.gate(partitionNormalA)
	releaseB := env.gate(partitionNormalB)
	defer releaseA()
	defer releaseB()
	env.start(t)
	warming := env.waitLaunched(t, 1)[0]
	cooled := partitionNormalB
	if warming == partitionNormalB {
		cooled = partitionNormalA
	}
	if err := env.pool.SetCatalog(cooled, aistudio.BenefitTierFree, []aistudio.Model{
		{ID: testRuntimeModel, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}},
		{ID: firstWarmOtherModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := env.pool.MarkCooldownIfGeneration(
		cooled, firstWarmOtherModel, env.pool.ModelAccessGeneration(cooled), now, now.Add(time.Hour), "每日限额",
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(env.ctx, 3*time.Second)
	defer cancel()
	startedAt := time.Now()
	lease, err := env.service.acquireWarmLease(ctx, aistudio.AccountSelection{ModelID: firstWarmOtherModel, Method: "generateContent"})
	if err == nil {
		_ = lease.Release()
		t.Fatalf("只有冷却中的账户能服务的模型不应取得租约，取得 %s", lease.Account().ID)
	}
	if errors.Is(err, context.DeadlineExceeded) || time.Since(startedAt) > 2*time.Second {
		t.Fatalf("err = %v（%s），期望不等首轮预热、按全部冷却立即返回", err, time.Since(startedAt))
	}
	var cooling *aistudio.AllCoolingError
	if !errors.As(err, &cooling) {
		t.Fatalf("err = %v，期望按全部冷却返回", err)
	}
	if launched := env.launches(); len(launched) != 1 {
		t.Fatalf("冷启动 = %v，期望只有首轮预热的 %s", launched, warming)
	}
}

// recordSteps 记下每次状态推送时生成服务的步骤（相邻重复的只记一次）
func recordSteps(service *trackedService) func() []string {
	var mu sync.Mutex
	var steps []string
	service.setStatusPublisher(func() {
		name, _ := service.Step()
		mu.Lock()
		defer mu.Unlock()
		if len(steps) == 0 || steps[len(steps)-1] != name {
			steps = append(steps, name)
		}
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(steps)
	}
}

// TestStopWhileLaunching 启动中（准备模型目录时）停止：状态立即是 STOPPED、步骤为 cancel_requests 并推送；启动随后结束时不再进入
// start_prewarm，清理经过 close_workers、flush_writes 后清除，不进入运行
func TestStopWhileLaunching(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	steps := recordSteps(env.service)
	stopped := make(chan error, 1)
	_, started, err := env.service.Start(env.ctx, func() {
		go func() {
			ok, err := env.service.Stop()
			if err == nil && !ok {
				err = errors.New("Stop 返回没有停止")
			}
			stopped <- err
		}()
		if !waitUntil(5*time.Second, func() bool { name, _ := env.service.Step(); return name == stepCancelRequests }) {
			t.Error("启动中停止没有进入 cancel_requests")
		}
		if state := env.service.State(); state != "STOPPED" {
			t.Errorf("启动中停止后状态为 %s", state)
		}
	})
	if started || err == nil {
		t.Fatalf("启动中停止后 Start = (%t, %v)，期望没有启动", started, err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := steps(); !slices.Equal(got, []string{stepCancelRequests, stepCloseWorkers, stepFlushWrites, ""}) {
		t.Fatalf("启动中停止的步骤推送 = %v", got)
	}
	if state := env.service.State(); state != "STOPPED" {
		t.Fatalf("状态为 %s", state)
	}
}

// TestStopWaitsCatalogStep 停止时模型目录同步还没停：步骤经过 wait_catalog_stop，同步停下后再关闭 Worker；已经停了的不经过这一步
// （TestServiceStepsAndStatus）
func TestStopWaitsCatalogStep(t *testing.T) {
	env := newFirstWarmEnv(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
	env.start(t)
	if !waitUntil(5*time.Second, func() bool { return len(env.manager.WarmAccountIDs()) == 1 }) {
		t.Fatal("首轮预热没有就绪")
	}
	// 模拟后台目录同步停得慢：停止开始后 200ms 才结束
	catalogDone := make(chan struct{})
	env.service.lifecycleMu.Lock()
	env.service.modelRefreshDone = catalogDone
	env.service.lifecycleMu.Unlock()
	var once sync.Once
	steps := recordSteps(env.service)
	record := env.service.statusPublisher.Load()
	env.service.setStatusPublisher(func() {
		if name, _ := env.service.Step(); name == stepCancelRequests {
			once.Do(func() { time.AfterFunc(200*time.Millisecond, func() { close(catalogDone) }) })
		}
		(*record)()
	})
	if ok, err := env.service.Stop(); err != nil || !ok {
		t.Fatalf("Stop = (%t, %v)", ok, err)
	}
	if got := steps(); !slices.Equal(got, []string{stepCancelRequests, stepWaitCatalogStop, stepCloseWorkers, stepFlushWrites, ""}) {
		t.Fatalf("目录同步还没停时的停止步骤推送 = %v", got)
	}
}

// TestNewRuntimeWiresStatusPublisher 装配运行时（newRuntime）时把步骤变化的状态推送接到管理页事件：生成服务推送一次，订阅者收到一份
// 带当前步骤的 status 快照
func TestNewRuntimeWiresStatusPublisher(t *testing.T) {
	isolateDisableNotes(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := newRequestRegistry(ctx)
	cfg := config.Default()
	cfg.AuthStates = t.TempDir()
	cfg.WAABackend = config.WAABackendGo
	lifecycle, stop := context.WithCancel(ctx)
	useTempCatalogCacheDir(t)
	_, admin, closeRuntime, err := newRuntime(ctx, lifecycle, cfg, requests, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.pool.WaitBackground)
	defer func() {
		stop()
		if err := closeRuntime(); err != nil {
			t.Error(err)
		}
	}()
	subscriber := requests.subscribe(ctx)
	events := requests.activateSubscriber(subscriber, nil, nil)
	admin.service.enterStopStep(stepCloseWorkers)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case event := <-events:
			if status, ok := event.Data.(api.AdminStatus); ok && event.Type == "status" {
				if status.Transition != stepCloseWorkers || status.TransitionSince == nil || status.Time.IsZero() {
					t.Fatalf("推送的状态 transition=%q since=%v time=%v", status.Transition, status.TransitionSince, status.Time)
				}
				admin.service.storeStep("")
				return
			}
		case <-deadline:
			t.Fatal("步骤变化后管理页没有收到状态快照")
		}
	}
}
