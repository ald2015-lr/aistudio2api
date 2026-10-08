package app

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/camoufoxnative"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

const (
	// streamTestModel 只有 playground 列表中的账号能经 Playground 调用、build 列表中的账号能经 Build 调用
	streamTestModel   = "stream-tier-model"
	streamPlaygroundA = "playground-a@example.com"
	streamPlaygroundB = "playground-b@example.com"
	streamBuildA      = "build-a@example.com"
	streamUltraPlay   = "ultra-play@example.com"
	streamUltraBuild  = "ultra-build@example.com"
)

// channelRecorder 是测试用的上游：记录每次尝试使用的账号与通道，立即返回一段正文
type channelRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (recorder *channelRecorder) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (recorder *channelRecorder) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (recorder *channelRecorder) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		channel = string(lease.Channel())
	}
	recorder.mu.Lock()
	recorder.calls = append(recorder.calls, request.AccountID+"/"+channel)
	recorder.mu.Unlock()
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (recorder *channelRecorder) called() []string {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return slices.Clone(recorder.calls)
}

// streamChannelService 返回 Playground 与 Build 都启用、Worker 都已就绪的生成服务（单账号并发为 1）：playground 中的账号只能经
// Playground 调用 streamTestModel，build 中的账号只能经 Build 调用；ultra 中的账号权益为 Ultra
func streamChannelService(t *testing.T, playground, build, ultra []string) (*trackedService, *aistudio.AccountPool, *channelRecorder) {
	t.Helper()
	recorder := &channelRecorder{}
	service, pool := firstEventTestService(t, recorder, 0, append(slices.Clone(playground), build...)...)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	pool.SetPerAccountConcurrency(1)
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range append(slices.Clone(playground), build...) {
		tier := aistudio.BenefitTierFree
		if slices.Contains(ultra, accountID) {
			tier = aistudio.BenefitTierUltra
		}
		models := []aistudio.Model{base}
		if slices.Contains(playground, accountID) {
			models = append(models, target)
		}
		if err := pool.SetCatalog(accountID, tier, models); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(build, accountID) {
			if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
				t.Fatal(err)
			}
		}
	}
	service.setStreamPlaygroundModels(nil)
	preverifyModelAccess(t, pool, append(slices.Clone(playground), build...)...)
	return service, pool, recorder
}

// preverifyModelAccess 预先写入时间在未来的模型资格（两个通道）：请求结束后的后台写入（markModelAccessVerifiedAsync）
// 因此不再改动账户状态文件，不会与测试结束时删除临时目录竞争
func preverifyModelAccess(t *testing.T, pool *aistudio.AccountPool, accountIDs ...string) {
	t.Helper()
	future := time.Now().Add(time.Hour)
	for _, accountID := range accountIDs {
		lease, err := pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
		if err != nil {
			t.Fatal(err)
		}
		generation := lease.ModelAccessGeneration()
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
		for _, channel := range []aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild} {
			scope := aistudio.ChannelCooldownScope(channel, streamTestModel)
			if _, err := pool.MarkModelAccessVerifiedIfGeneration(accountID, scope, generation, future); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// runStreamChannelRequest 执行一次生成，返回这次请求最终使用的账号与通道（"账号/通道"）、用时与错误
func runStreamChannelRequest(
	t *testing.T, service *trackedService, recorder *channelRecorder, parent context.Context, stream bool, diag *generationDiagnostics,
) (string, time.Duration, error) {
	t.Helper()
	request := aistudio.GenerateRequest{
		ID: "req-stream-channel", Model: streamTestModel, Stream: stream,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	if diag == nil {
		diag = newGenerationDiagnostics(request, "未发送")
	}
	before := len(recorder.called())
	requestCtx, requestCancel := context.WithTimeout(parent, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	startedAt := time.Now()
	go service.generateWithRetry(parent, requestCtx, requestCancel, startedAt, request, "", events, diag)
	var err error
	for event := range events {
		if event.Kind == aistudio.EventError {
			err = event.Err
		}
	}
	elapsed := time.Since(startedAt)
	calls := recorder.called()
	if len(calls) == before {
		return "", elapsed, err
	}
	return calls[len(calls)-1], elapsed, err
}

// dispatchWaiters 返回调度等待队列中的请求数
func dispatchWaiters(service *trackedService) int {
	queue := service.workers.dispatch
	queue.mu.Lock()
	defer queue.mu.Unlock()
	waiters := 0
	for _, entries := range queue.queues {
		waiters += len(entries)
	}
	return waiters
}

// TestStreamPrefersPlayground 只有列表中的模型（按降级判定的规则整理：去空白、去 models/ 前缀、不分大小写）的流式请求，
// 在没有限定通道、没有固定账号或文件、两个通道都启用时才先试 Playground；列表默认为空
func TestStreamPrefersPlayground(t *testing.T) {
	service, pool, _ := streamChannelService(t, []string{streamPlaygroundA}, []string{streamBuildA}, nil)
	request := aistudio.GenerateRequest{Model: streamTestModel, Stream: true}
	selection := aistudio.AccountSelection{ModelID: streamTestModel, Method: "generateContent"}
	if service.streamPrefersPlayground(request, selection) {
		t.Fatal("列表为空（默认）时不应优先 Playground")
	}
	service.setStreamPlaygroundModels([]string{" models/Stream-Tier-Model ", "other-model"})
	if !service.streamPrefersPlayground(request, selection) {
		t.Fatal("列表中的模型（大小写、models/ 前缀不同）流式请求应先试 Playground")
	}
	for _, test := range []struct {
		name   string
		change func(*aistudio.GenerateRequest, *aistudio.AccountSelection)
	}{
		{name: "非流式", change: func(request *aistudio.GenerateRequest, _ *aistudio.AccountSelection) { request.Stream = false }},
		{name: "不在列表中的模型", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) {
			selection.ModelID = testRuntimeModel
		}},
		{name: "只走 Build", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) { selection.BuildOnly = true }},
		{name: "只走 Playground", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) {
			selection.PlaygroundOnly = true
		}},
		{name: "降级判定的 Playground 优先", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) {
			selection.PlaygroundFirst = true
		}},
		{name: "指定账号", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) {
			selection.AccountID = streamPlaygroundA
		}},
		{name: "绑定文件", change: func(_ *aistudio.GenerateRequest, selection *aistudio.AccountSelection) {
			selection.ResourceID = "files/abc"
		}},
	} {
		changedRequest, changedSelection := request, selection
		test.change(&changedRequest, &changedSelection)
		if service.streamPrefersPlayground(changedRequest, changedSelection) {
			t.Fatalf("%s：不应先试 Playground", test.name)
		}
	}
	for _, channel := range []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground} {
		pool.SetUpstreamChannels([]aistudio.Channel{channel})
		if service.streamPrefersPlayground(request, selection) {
			t.Fatalf("只启用 %s 通道时不应先试 Playground", channel)
		}
	}
}

// TestStreamPlaygroundSelection 列表中的模型的流式请求：有空闲的 Playground 账号时总是走 Playground；Playground 账号全部忙碌时
// 立即改走 Build，不排队等 Playground，也不留下等待者与租约。不在列表中的模型与非流式请求照常在两个通道间轮询
func TestStreamPlaygroundSelection(t *testing.T) {
	service, pool, recorder := streamChannelService(t, []string{streamPlaygroundA}, []string{streamBuildA}, nil)
	ctx := service.lifecycle
	run := func(stream bool) (string, time.Duration) {
		t.Helper()
		used, elapsed, err := runStreamChannelRequest(t, service, recorder, ctx, stream, nil)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		return used, elapsed
	}
	rotation := func(stream bool) []string {
		t.Helper()
		var used []string
		for range 4 {
			channel, _ := run(stream)
			used = append(used, channel)
		}
		return sortedUnique(used)
	}
	both := []string{streamBuildA + "/build", streamPlaygroundA + "/playground"}

	if got := rotation(true); !slices.Equal(got, both) {
		t.Fatalf("列表为空时流式请求应在两个通道间轮询，实际 %v", got)
	}

	service.setStreamPlaygroundModels([]string{streamTestModel})
	if got := rotation(true); !slices.Equal(got, []string{streamPlaygroundA + "/playground"}) {
		t.Fatalf("列表中的模型流式请求在 Playground 空闲时应总是走 Playground，实际 %v", got)
	}
	if got := rotation(false); !slices.Equal(got, both) {
		t.Fatalf("列表中的模型非流式请求应照常轮询，实际 %v", got)
	}

	// Playground 账号忙碌（达到单账号并发）：立即改走 Build
	release := holdLease(t, pool, streamPlaygroundA)
	used, elapsed := run(true)
	if used != streamBuildA+"/build" {
		t.Fatalf("Playground 账号全部忙碌时应改走 Build，实际 %q", used)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Playground 繁忙时应立即改走 Build，实际用时 %s", elapsed)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("改走 Build 后调度等待队列还有 %d 个等待者", waiters)
	}
	if !slices.ContainsFunc(serviceLogMessages(service.requests), func(message string) bool {
		return strings.Contains(message, "Playground 繁忙，改用其他通道")
	}) {
		t.Fatal("改走 Build 时应写入请求进度")
	}
	release()
	assertLeasesReleased(t, pool)

	// Playground 账号空闲后恢复走 Playground
	if used, _ := run(true); used != streamPlaygroundA+"/playground" {
		t.Fatalf("Playground 账号空闲后应恢复走 Playground，实际 %q", used)
	}
	assertLeasesReleased(t, pool)
}

// TestStreamPlaygroundFallbackPrefersPlayground 账号同时可经 Playground 与 Build 调用时，两个通道共用账号并发：Playground 账号全部忙碌后
// 改为不限通道排队的请求，在账号空闲时仍走 Playground，不会因为通道轮询轮到同一账号的 Build（排队等的是同一个账号，改走 Build 不省时间）；
// 通道顺序把 Build 排在前面时也一样。改为不限通道后不限定 Playground：Playground 冷却时立即走同一账号的 Build
func TestStreamPlaygroundFallbackPrefersPlayground(t *testing.T) {
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	bothChannels := func(t *testing.T, accounts []string) (*trackedService, *aistudio.AccountPool, *channelRecorder) {
		t.Helper()
		service, pool, recorder := streamChannelService(t, accounts, nil, nil)
		for _, accountID := range accounts {
			if err := pool.SetBuildCatalog(accountID, []aistudio.Model{target}); err != nil {
				t.Fatal(err)
			}
		}
		service.setStreamPlaygroundModels([]string{streamTestModel})
		return service, pool, recorder
	}
	for _, test := range []struct {
		name     string
		accounts []string
		channels []aistudio.Channel
	}{
		{name: "单个账号", accounts: []string{streamPlaygroundA}},
		{name: "两个账号", accounts: []string{streamPlaygroundA, streamPlaygroundB}},
		{name: "Build 在前的通道顺序", accounts: []string{streamPlaygroundA}, channels: []aistudio.Channel{aistudio.ChannelBuild, aistudio.ChannelPlayground}},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, pool, recorder := bothChannels(t, test.accounts)
			if test.channels != nil {
				pool.SetUpstreamChannels(test.channels)
			}
			used, _, err := runStreamChannelRequest(t, service, recorder, service.lifecycle, true, nil)
			if err != nil || used != streamPlaygroundA+"/playground" {
				t.Fatalf("第一个请求应走 %s 的 Playground：used=%q err=%v", streamPlaygroundA, used, err)
			}
			var releases []func()
			for _, accountID := range test.accounts {
				releases = append(releases, holdLease(t, pool, accountID))
			}
			type result struct {
				used string
				err  error
			}
			done := make(chan result, 1)
			go func() {
				used, _, err := runStreamChannelRequest(t, service, recorder, service.lifecycle, true, nil)
				done <- result{used: used, err: err}
			}()
			time.Sleep(300 * time.Millisecond)
			// 上一个请求选中的账号先空闲
			releases[0]()
			select {
			case got := <-done:
				if got.err != nil || got.used != streamPlaygroundA+"/playground" {
					t.Fatalf("账号全部忙碌后 %s 先空闲，列表中的流式请求应走它的 Playground：used=%q err=%v",
						streamPlaygroundA, got.used, got.err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("账号空闲后请求没有完成")
			}
			for _, release := range releases[1:] {
				release()
			}
			if waiters := dispatchWaiters(service); waiters != 0 {
				t.Fatalf("请求完成后调度等待队列还有 %d 个等待者", waiters)
			}
			assertLeasesReleased(t, pool)
		})
	}
	t.Run("Playground 冷却时走同一账号的 Build", func(t *testing.T) {
		service, pool, recorder := bothChannels(t, []string{streamPlaygroundA})
		// 模型资格的检查时间预先写在一小时后，冷却的检查时间要更晚才会写入
		until := time.Now().Add(30 * time.Second)
		if err := pool.MarkCooldownIfGeneration(streamPlaygroundA, streamTestModel, pool.ModelAccessGeneration(streamPlaygroundA),
			time.Now().Add(2*time.Hour), until, "每日限额"); err != nil {
			t.Fatal(err)
		}
		used, elapsed, err := runStreamChannelRequest(t, service, recorder, service.lifecycle, true, nil)
		if err != nil || used != streamPlaygroundA+"/build" {
			t.Fatalf("Playground 冷却时应走同一账号的 Build：used=%q err=%v", used, err)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("Playground 冷却时应立即改走 Build，实际用时 %s", elapsed)
		}
		if waiters := dispatchWaiters(service); waiters != 0 {
			t.Fatalf("请求完成后调度等待队列还有 %d 个等待者", waiters)
		}
	})
}

// TestStreamPlaygroundFailedColdStartNotRepeated 不等待的 Playground 选号现场冷启动失败后，改为不限通道选号时不再启动同一账号：
// 启动次数与列表为空时相同，并按启动失败原因结束
func TestStreamPlaygroundFailedColdStartNotRepeated(t *testing.T) {
	for _, listed := range []bool{false, true} {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
		var launches atomic.Int32
		manager.launch = func(context.Context, string, camoufoxnative.Options) (*aistudio.NativeWorker, error) {
			launches.Add(1)
			return nil, errors.New("模拟浏览器启动失败")
		}
		pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
		target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
		if err := pool.SetCatalog(partitionNormalA, aistudio.BenefitTierFree, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetBuildCatalog(partitionNormalA, []aistudio.Model{target}); err != nil {
			t.Fatal(err)
		}
		preverifyModelAccess(t, pool, partitionNormalA)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		recorder := &channelRecorder{}
		service := &trackedService{
			lifecycle: ctx, service: recorder, pool: pool, requests: requests, workers: manager,
			forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
		}
		service.timeout.Store(int64(time.Minute))
		var models []string
		if listed {
			models = []string{streamTestModel}
		}
		service.setStreamPlaygroundModels(models)
		_, _, err := runStreamChannelRequest(t, service, recorder, ctx, true, nil)
		if err == nil || !strings.Contains(err.Error(), "模拟浏览器启动失败") {
			t.Fatalf("列表=%v：err = %v，期望按启动失败原因结束", models, err)
		}
		if got := launches.Load(); got != 1 {
			t.Fatalf("列表=%v：启动次数 = %d，期望 1（启动失败的账号不应在同一次尝试里再启动）", models, got)
		}
		if waiters := dispatchWaiters(service); waiters != 0 {
			t.Fatalf("列表=%v：调度等待队列还有 %d 个等待者", models, waiters)
		}
		assertLeasesReleased(t, pool)
	}
}

// TestStreamPlaygroundRetriesKeepPreference 同一请求换号重试时每次尝试都先试 Playground：第一个 Playground 账号失败后
// 换到另一个空闲的 Playground 账号，不会因为重试改走 Build
func TestStreamPlaygroundRetriesKeepPreference(t *testing.T) {
	upstream := newStallingService(streamPlaygroundA)
	service, pool := firstEventTestService(t, upstream, 150*time.Millisecond, streamPlaygroundA, streamPlaygroundB, streamBuildA)
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	target := aistudio.Model{ID: streamTestModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	base := aistudio.Model{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	for _, accountID := range []string{streamPlaygroundA, streamPlaygroundB} {
		if err := pool.SetCatalog(accountID, aistudio.BenefitTierFree, []aistudio.Model{base, target}); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.SetCatalog(streamBuildA, aistudio.BenefitTierFree, []aistudio.Model{base}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(streamBuildA, []aistudio.Model{target}); err != nil {
		t.Fatal(err)
	}
	service.setStreamPlaygroundModels([]string{streamTestModel})
	preverifyModelAccess(t, pool, streamPlaygroundA, streamPlaygroundB, streamBuildA)
	request := aistudio.GenerateRequest{
		ID: "req-stream-retry", Model: streamTestModel, Stream: true,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	requestCtx, requestCancel := context.WithTimeout(service.lifecycle, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go service.generateWithRetry(service.lifecycle, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	for event := range events {
		if event.Kind == aistudio.EventError {
			t.Fatalf("请求失败: %v", event.Err)
		}
	}
	upstream.mu.Lock()
	calls := slices.Clone(upstream.calls)
	upstream.mu.Unlock()
	if !slices.Equal(calls, []string{streamPlaygroundA, streamPlaygroundB}) {
		t.Fatalf("尝试顺序 = %v，期望先 %s 超时再换到 %s（仍走 Playground）", calls, streamPlaygroundA, streamPlaygroundB)
	}
	assertLeasesReleased(t, pool)
}

// TestStreamPlaygroundGuardWins 模型同时在降级判定与流式优先列表中时按降级判定处理：Playground 账号忙碌时等 Playground，
// 不改走 Build；账号空闲后在 Playground 上完成
func TestStreamPlaygroundGuardWins(t *testing.T) {
	saved := conversationMemory
	conversationMemory = &downgradeMemory{entries: make(map[string][]downgradeMemoryEntry)}
	t.Cleanup(func() { conversationMemory = saved })
	service, pool, recorder := streamChannelService(t, []string{streamPlaygroundA}, []string{streamBuildA}, nil)
	service.setStreamPlaygroundModels([]string{streamTestModel})
	guard := config.DefaultDowngradeGuard()
	guard.Models = []string{streamTestModel}
	service.setDowngradeGuard(guard)
	request := aistudio.GenerateRequest{
		ID: "req-stream-channel", Model: streamTestModel, Stream: true,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	gate, rejected := service.prepareDowngradeGate(service.lifecycle, request, request.Contents)
	if gate == nil || rejected != nil {
		t.Fatalf("准备降级判定失败: gate=%v rejected=%v", gate, rejected)
	}
	diag := newGenerationDiagnostics(request, "未发送")
	diag.downgrade = gate

	release := holdLease(t, pool, streamPlaygroundA)
	type result struct {
		used string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		used, _, err := runStreamChannelRequest(t, service, recorder, service.lifecycle, true, diag)
		done <- result{used: used, err: err}
	}()
	select {
	case got := <-done:
		t.Fatalf("降级判定的模型在 Playground 忙碌时应等待 Playground，实际已走 %q（err=%v）", got.used, got.err)
	case <-time.After(500 * time.Millisecond):
	}
	if calls := recorder.called(); len(calls) != 0 {
		t.Fatalf("等待 Playground 期间不应调用上游，实际 %v", calls)
	}
	release()
	select {
	case got := <-done:
		if got.err != nil || got.used != streamPlaygroundA+"/playground" {
			t.Fatalf("Playground 账号空闲后应在 Playground 上完成：used=%q err=%v", got.used, got.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Playground 账号空闲后请求没有完成")
	}
	assertLeasesReleased(t, pool)
}

// TestStreamPlaygroundStaysInPool 不等待的 Playground 选号只看请求的号池：/ultra 请求的 Ultra Playground 账号忙碌时改走
// Ultra 号池的 Build，不会借用普通号池空闲的 Playground 账号
func TestStreamPlaygroundStaysInPool(t *testing.T) {
	service, pool, recorder := streamChannelService(t,
		[]string{streamPlaygroundA, streamUltraPlay}, []string{streamUltraBuild}, []string{streamUltraPlay, streamUltraBuild})
	service.setStreamPlaygroundModels([]string{streamTestModel})
	ultra := aistudio.ContextWithPoolScope(service.lifecycle, aistudio.PoolScopeUltra)
	used, _, err := runStreamChannelRequest(t, service, recorder, ultra, true, nil)
	if err != nil || used != streamUltraPlay+"/playground" {
		t.Fatalf("/ultra 请求应走 Ultra 号池的 Playground 账号：used=%q err=%v", used, err)
	}
	release := holdLease(t, pool, streamUltraPlay)
	used, elapsed, err := runStreamChannelRequest(t, service, recorder, ultra, true, nil)
	release()
	if err != nil || used != streamUltraBuild+"/build" {
		t.Fatalf("Ultra Playground 账号忙碌时应改走 Ultra 号池的 Build：used=%q err=%v", used, err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("改走 Build 应立即进行，实际用时 %s", elapsed)
	}
	for _, call := range recorder.called() {
		if strings.HasPrefix(call, streamPlaygroundA) {
			t.Fatalf("/ultra 请求不应使用普通号池的账号，调用记录 %v", recorder.called())
		}
	}
	assertLeasesReleased(t, pool)
}

// TestAcquireWarmLeaseNowDoesNotWait 不等待的选号：账号全部忙碌时立即返回没有立即可用的账号，不加入调度等待队列、不留下租约；
// 有空闲账号时与 acquireWarmLease 一样取得租约
func TestAcquireWarmLeaseNowDoesNotWait(t *testing.T) {
	service, pool, _ := streamChannelService(t, []string{streamPlaygroundA}, []string{streamBuildA}, nil)
	// 限时只为回归时尽快失败：不等待的选号本身不应用到这个时限
	ctx, cancel := context.WithTimeout(service.lifecycle, 5*time.Second)
	defer cancel()
	selection := aistudio.AccountSelection{ModelID: streamTestModel, Method: "generateContent", PlaygroundFirst: true}
	lease, err := service.acquireWarmLeaseNow(ctx, selection, nil)
	if err != nil || lease.Channel() != aistudio.ChannelPlayground || lease.Account().ID != streamPlaygroundA {
		t.Fatalf("Playground 账号空闲时应立即取得：lease=%v err=%v", lease, err)
	}
	startedAt := time.Now()
	_, busyErr := service.acquireWarmLeaseNow(ctx, selection, nil)
	var noImmediate *noImmediateLeaseError
	if !errors.As(busyErr, &noImmediate) {
		t.Fatalf("账号全部忙碌时 err = %v，期望没有立即可用的账号", busyErr)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("不等待的选号用时 %s", elapsed)
	}
	if waiters := dispatchWaiters(service); waiters != 0 {
		t.Fatalf("不等待的选号不应加入调度等待队列，实际 %d 个等待者", waiters)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	assertLeasesReleased(t, pool)
}

// TestAcquireWarmLeaseNowColdStart 不等待的选号可以现场冷启动备用账号，但只在分区有空槽、冷启动名额空闲时：分区已满时不替换
// 其他账号的空闲 Worker，冷启动名额已满时不排队；两种情况都立即返回，不留下租约、启动名额或启动中的 Worker
func TestAcquireWarmLeaseNowColdStart(t *testing.T) {
	t.Run("分区有空槽时现场启动", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA}, nil, 1, 1, 0, 1)
		service := &trackedService{lifecycle: context.Background(), pool: pool, requests: requests, workers: manager}
		lease, err := service.acquireWarmLeaseNow(context.Background(), aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
		if err != nil {
			t.Fatalf("分区有空槽时应现场启动备用账号: %v", err)
		}
		_ = lease.Release()
		if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
			t.Fatalf("现场启动后 Worker = %v", warm)
		}
	})
	t.Run("分区已满时不替换空闲 Worker", func(t *testing.T) {
		manager, pool, requests := partitionTestManager(t, []string{partitionNormalA, partitionNormalB}, nil, 1, 1, 0, 1)
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		service := &trackedService{lifecycle: ctx, pool: pool, requests: requests, workers: manager}
		if _, err := manager.ensureWorker(ctx, partitionNormalA, "", false); err != nil {
			t.Fatal(err)
		}
		// 只允许账号 B：A 的 Worker 空闲但不是候选，排队选号会淘汰它为 B 启动 Worker
		selection := aistudio.AccountSelection{
			ModelID: testRuntimeModel, Method: "generateContent", AllowedAccountIDs: []string{partitionNormalB},
		}
		_, err := service.acquireWarmLeaseNow(ctx, selection, nil)
		var noImmediate *noImmediateLeaseError
		if !errors.As(err, &noImmediate) {
			t.Fatalf("分区已满时 err = %v，期望没有立即可用的账号", err)
		}
		if _, err := manager.ensureWorker(withImmediateStartup(ctx), partitionNormalB, "", false); !errors.Is(err, errAccountWorkerCapacity) {
			t.Fatalf("不等待的现场启动在分区已满时 err = %v，期望容量已满", err)
		}
		if warm := sortedWarm(manager); !slices.Equal(warm, []string{partitionNormalA}) {
			t.Fatalf("不等待的选号不应替换空闲 Worker，Worker = %v", warm)
		}
		if waiters := dispatchWaiters(service); waiters != 0 {
			t.Fatalf("调度等待队列还有 %d 个等待者", waiters)
		}
		assertLeasesReleased(t, pool)
	})
	t.Run("冷启动名额已满时不排队", func(t *testing.T) {
		manager, gate := slotTestManager(t, "camoufox-test", 1, "a@example.com")
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		service := &trackedService{lifecycle: ctx, pool: manager.pool, requests: manager.requests, workers: manager}
		// 按需启动最多占用容量 + 1 个名额
		var held []func()
		for range 2 {
			release, ok := manager.startupSlots.tryAcquire(manager.warmConcurrencyValue)
			if !ok {
				t.Fatal("名额未满时应取得")
			}
			held = append(held, release)
		}
		if _, ok := manager.startupSlots.tryAcquire(manager.warmConcurrencyValue); ok {
			t.Fatal("名额已满时不应再取得")
		}
		_, err := service.acquireWarmLeaseNow(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
		var noImmediate *noImmediateLeaseError
		if !errors.As(err, &noImmediate) {
			t.Fatalf("冷启动名额已满时 err = %v，期望没有立即可用的账号", err)
		}
		if gate.isStarted("a@example.com") {
			t.Fatal("冷启动名额已满时不应开始启动浏览器")
		}
		waitStartupUsage(t, &manager.startupSlots, 2, 0, 0)
		if notOpening, _ := manager.withoutOpening([]string{"a@example.com"}); len(notOpening) != 1 {
			t.Fatal("放弃现场启动后不应留下启动中的 Worker")
		}
		assertLeasesReleased(t, manager.pool)
		for _, release := range held {
			release()
		}
		gate.open()
		lease, err := service.acquireWarmLeaseNow(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}, nil)
		if err != nil {
			t.Fatalf("名额释放后应现场启动: %v", err)
		}
		_ = lease.Release()
		waitStartupUsage(t, &manager.startupSlots, 0, 0, 0)
	})
}
