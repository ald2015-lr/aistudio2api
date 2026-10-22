package app

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// dataResetTransport 是转写与 Live 恢复测试的传输层：ListModels 返回 testRuntimeModel（generateContent、bidiGenerateContent），
// GenerateAccessToken 返回固定 token，Drive 上传与删除直接成功；生成（DoProtected）与 Live 建立（OpenBidiProtected）时 failing 中账户的
// 前几次调用按协议 Code 5（404 Ambiguous request）失败，其余的生成返回一个只有完成帧的流，Live 建立先用 setupDelay（取到租约后
// 冷启动 Worker、握手等的耗时），再返回一个空会话并记下它的上下文与释放函数（模拟会话一直在用这个租约）。
// calls 记录各次生成与 Live 建立用的账户
type dataResetTransport struct {
	mu         sync.Mutex
	failing    map[string]int
	setupDelay time.Duration
	calls      []string
	sessions   []context.Context
	releases   []func() error
}

func (transport *dataResetTransport) Do(_ context.Context, request aistudio.RPCRequest) (*aistudio.RPCResponse, error) {
	switch request.Method {
	case "ListModels":
		return videoResetResponse(http.StatusOK,
			`[[["models/`+testRuntimeModel+`",null,null,"Reset Test",null,1048576,65536,["generateContent","bidiGenerateContent"]]]]`), nil
	case "GenerateAccessToken":
		return videoResetResponse(http.StatusOK, `["reset-test-token"]`), nil
	}
	return nil, errors.New("测试不支持 " + request.Method)
}

func (transport *dataResetTransport) UploadDrive(
	_ context.Context, _ string, _ string, request aistudio.UploadRequest,
) (aistudio.FileRef, error) {
	return aistudio.FileRef{ID: "drive-reset-test", Name: request.Name, MIME: request.MIME}, nil
}

func (transport *dataResetTransport) DownloadDrive(context.Context, string, string, string) (aistudio.MediaStream, error) {
	return aistudio.MediaStream{}, errors.New("测试不下载 Drive 文件")
}

func (transport *dataResetTransport) DeleteDrive(context.Context, string, string, string) error {
	return nil
}

// fail 记录一次调用，返回这次是否按 Code 5 失败
func (transport *dataResetTransport) fail(accountID string) bool {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	transport.calls = append(transport.calls, accountID)
	fail := transport.failing[accountID] > 0
	if fail {
		transport.failing[accountID]--
	}
	return fail
}

func (transport *dataResetTransport) DoProtected(
	_ context.Context, request aistudio.GenerateRequest, _ aistudio.RPCRequest,
) (*aistudio.RPCResponse, error) {
	if transport.fail(request.AccountID) {
		return videoResetResponse(http.StatusNotFound, `[5,"Ambiguous request for service ''"]`), nil
	}
	return videoResetResponse(http.StatusOK, `[[[[[null,1]]]]]`), nil
}

func (transport *dataResetTransport) OpenBidiProtected(
	ctx context.Context, _ aistudio.BidiRequest, _ aistudio.RequestContext, lease *aistudio.AccountLease, release func() error,
) (*aistudio.BidiSession, error) {
	if transport.fail(lease.Account().ID) {
		return nil, waaRuntimeFailure("BidiGenerateContent")
	}
	if transport.setupDelay > 0 {
		timer := time.NewTimer(transport.setupDelay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	transport.mu.Lock()
	transport.sessions = append(transport.sessions, ctx)
	transport.releases = append(transport.releases, release)
	transport.mu.Unlock()
	return &aistudio.BidiSession{}, nil
}

func (transport *dataResetTransport) called() []string {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return slices.Clone(transport.calls)
}

// openedSessions 返回已建立会话的上下文
func (transport *dataResetTransport) openedSessions() []context.Context {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return slices.Clone(transport.sessions)
}

// closeSessions 释放已建立会话占着的租约
func (transport *dataResetTransport) closeSessions() {
	transport.mu.Lock()
	releases := transport.releases
	transport.releases = nil
	transport.mu.Unlock()
	for _, release := range releases {
		_ = release()
	}
}

// dataResetTestService 返回接真实 PooledService（传输层为 dataResetTransport）的生成服务，账户见 workerResetTestService
func dataResetTestService(
	t *testing.T, failing map[string]int, accountIDs ...string,
) (*trackedService, *dataResetTransport, map[string]func() int) {
	t.Helper()
	service, closed := workerResetTestService(t, newWorkerFailureService(nil), accountIDs...)
	transport := &dataResetTransport{failing: failing}
	t.Cleanup(transport.closeSessions)
	client, err := aistudio.NewClient(aistudio.ClientOptions{Transport: transport, Protected: transport})
	if err != nil {
		t.Fatal(err)
	}
	pooled, err := aistudio.NewPooledService(service.pool, client)
	if err != nil {
		t.Fatal(err)
	}
	service.service = pooled
	service.state.Store(serviceRunning)
	service.dataContext = service.lifecycle
	return service, transport, closed
}

// startBackgroundReset 让账号进入“后台关闭进行中”：一个一直持有的租约（同账号的长流）加一次登记了独占等待的后台关闭。
// 返回结束长流的函数；长流结束后关闭照常完成
func startBackgroundReset(t *testing.T, service *trackedService, accountID string) func() {
	t.Helper()
	release := holdLease(t, service.pool, accountID)
	failed, err := service.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: accountID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.workers.scheduleWorkerReset(
		accountID, service.workers.WorkerGeneration(accountID), failed, workerResetRequest,
	); err != nil {
		t.Fatal(err)
	}
	if service.workers.pendingWorkerReset(accountID) == nil {
		t.Fatalf("%s 没有登记后台关闭", accountID)
	}
	return release
}

// TestTranscriptionResetSkipsResettingCandidate 转写经真实的 PooledService.Transcribe 在 alice 上遇到协议 Code 5、alice 的关闭要等同账号
// 另一个请求（300ms 后结束）时，唯一的另一个候选 bob 有进行中的后台关闭（登记着独占等待，要等 bob 的长流结束）：bob 不算可换的候选，
// 照旧等 alice 的关闭完成后在 alice 重放成功。换到 bob 的话会在 aistudio 层的取租约里排到 bob 的关闭结束，alice 先关闭完成也用不上，
// 等满 REQUEST_TIMEOUT 后 504
func TestTranscriptionResetSkipsResettingCandidate(t *testing.T) {
	service, transport, closed := dataResetTestService(t, map[string]int{resetTestAlice: 1}, resetTestAlice, resetTestBob)
	service.timeout.Store(int64(2 * time.Second))
	endBobStream := startBackgroundReset(t, service, resetTestBob)
	defer endBobStream()
	const holdFor = 300 * time.Millisecond
	releaseAlice := holdLease(t, service.pool, resetTestAlice)
	defer releaseAlice()
	time.AfterFunc(holdFor, releaseAlice)
	audio := []byte("RIFF-reset-test")
	startedAt := time.Now()
	_, err := service.Transcribe(context.Background(), aistudio.TranscriptionRequest{
		ID: "transcribe-reset-test", Model: testRuntimeModel, Name: "reset.wav", MIME: "audio/wav",
		Size: int64(len(audio)), Reader: bytes.NewReader(audio),
	})
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("Transcribe 失败（%s 后，调用=%v）: %v，期望等 alice 的关闭完成后在 alice 重放成功", elapsed, transport.called(), err)
	}
	if calls := transport.called(); !slices.Equal(calls, []string{resetTestAlice, resetTestAlice}) {
		t.Fatalf("生成调用 = %v，期望 alice 失败后关闭完成、在 alice 重放", calls)
	}
	if elapsed < holdFor-50*time.Millisecond || elapsed > holdFor+time.Second {
		t.Fatalf("用时 %s，期望约等到 alice 另一个请求结束（%s）", elapsed, holdFor)
	}
	endBobStream()
	waitResetsSettled(t, service.workers)
	if got := closed[resetTestAlice](); got != 1 {
		t.Fatalf("alice 的 Worker 关闭 %d 次，期望 1 次", got)
	}
}

// TestBidiResetSkipsResettingCandidate 不固定账号的 Live 在 alice 上遇到协议 Code 5、alice 有长流时，唯一的另一个候选 bob 有进行中的
// 后台关闭：bob 不算可换的候选，恢复按不能换号处理，在 liveResetWait（REQUEST_TIMEOUT）内以 503 结束，不进入 bob 的取租约
// （那里要排到 bob 的长流结束）
func TestBidiResetSkipsResettingCandidate(t *testing.T) {
	upstream, service, _, release := newBidiSwitchTest(t, resetTestAlice, resetTestBob)
	defer release()
	upstream.pool = service.pool
	const limit = 300 * time.Millisecond
	service.timeout.Store(int64(limit))
	endBobStream := startBackgroundReset(t, service, resetTestBob)
	defer endBobStream()
	ctx, cancel := context.WithTimeout(service.lifecycle, 3*time.Second)
	defer cancel()
	startedAt := time.Now()
	_, err := service.OpenBidi(ctx, aistudio.BidiRequest{Model: testRuntimeModel})
	elapsed := time.Since(startedAt)
	result := <-upstream.result
	if slices.Contains(result.acquiring, resetTestBob) {
		t.Fatalf("取租约的账号 = %v，bob 有进行中的后台关闭，不应换过去（用时 %s，err=%v）", result.acquiring, elapsed, err)
	}
	var notReady *aistudio.AccountsNotReadyError
	if !errors.As(err, &notReady) || !strings.Contains(err.Error(), "内没有关闭重建") || ctx.Err() != nil {
		t.Fatalf("err = %v，期望 alice 等满上限后以 503（没有关闭重建）结束", err)
	}
	if elapsed < limit-50*time.Millisecond || elapsed > limit+time.Second {
		t.Fatalf("用时 %s，期望约 REQUEST_TIMEOUT %s", elapsed, limit)
	}
	release()
	endBobStream()
	waitResetsSettled(t, service.workers)
}

// TestBidiSetupDeadline Live 会话建立阶段的期限（liveResetWait，这里 300ms）：经真实的 PooledService.OpenBidi，取租约排队到期时以 503 结束，
// 不挂到长流结束或客户端断开。覆盖不固定账号、所有候选都有进行中的后台关闭（awaitLiveReset 只处理只剩一个账号的情况）；
// 固定账号先在取租约里排队（并发已满）、之后同账号才登记后台关闭（awaitLiveReset 检查时还没有）；以及 alice 遇到 Code 5 后恢复回调
// 返回未恢复、换到并发已满的 bob 排队（恢复回调返回后重新计时）三种情况
func TestBidiSetupDeadline(t *testing.T) {
	for _, mode := range []string{"所有候选都在关闭", "先排队后登记关闭", "换号后排队"} {
		t.Run(mode, func(t *testing.T) {
			var failing map[string]int
			var wantCalls []string
			if mode == "换号后排队" {
				failing = map[string]int{resetTestAlice: 1}
				wantCalls = []string{resetTestAlice}
			}
			service, transport, _ := dataResetTestService(t, failing, resetTestAlice, resetTestBob)
			const limit = 300 * time.Millisecond
			service.timeout.Store(int64(limit))
			request := aistudio.BidiRequest{Model: testRuntimeModel}
			switch mode {
			case "所有候选都在关闭":
				for _, accountID := range []string{resetTestAlice, resetTestBob} {
					endStream := startBackgroundReset(t, service, accountID)
					defer endStream()
				}
			case "先排队后登记关闭":
				// alice 的并发（2）被一个长流和一个短请求占满；Live 固定 alice 排队后，短请求遇到 Code 5，经后台关闭转成独占等待
				request.AccountID = resetTestAlice
				endStream := holdLease(t, service.pool, resetTestAlice)
				defer endStream()
				failed, err := service.pool.AcquireFor(context.Background(), aistudio.AccountSelection{AccountID: resetTestAlice})
				if err != nil {
					t.Fatal(err)
				}
				generation := service.workers.WorkerGeneration(resetTestAlice)
				time.AfterFunc(150*time.Millisecond, func() {
					_, _ = service.workers.scheduleWorkerReset(resetTestAlice, generation, failed, workerResetRequest)
				})
			case "换号后排队":
				// alice 有长流，Code 5 后的关闭等不到（不固定账号只等 50ms 就换号）；bob 的并发（2）被两个长流占满
				for _, accountID := range []string{resetTestAlice, resetTestBob, resetTestBob} {
					endStream := holdLease(t, service.pool, accountID)
					defer endStream()
				}
			}
			ctx, cancel := context.WithTimeout(service.lifecycle, 3*time.Second)
			defer cancel()
			startedAt := time.Now()
			_, err := service.OpenBidi(ctx, request)
			elapsed := time.Since(startedAt)
			var notReady *aistudio.AccountsNotReadyError
			if !errors.As(err, &notReady) || ctx.Err() != nil {
				t.Fatalf("%s 后 err=%v，期望在建立期限内以号池暂时不可用（503）结束", elapsed, err)
			}
			if !strings.Contains(err.Error(), "Live 会话建立在 300ms 内没有取到账号") || errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v，期望写建立期限内没有取到账号，且不按取消处理", err)
			}
			if elapsed < limit-50*time.Millisecond || elapsed > limit+time.Second {
				t.Fatalf("用时 %s，期望约建立期限 %s", elapsed, limit)
			}
			if calls := transport.called(); !slices.Equal(calls, wantCalls) {
				t.Fatalf("Live 建立调用 = %v，期望 %v（没有取到租约时不进入上游）", calls, wantCalls)
			}
			if mode == "换号后排队" && !workerResetLogged(service, "账号切换") {
				t.Fatal("alice 未恢复后应换号")
			}
			if mode == "先排队后登记关闭" && service.workers.pendingWorkerReset(resetTestAlice) == nil {
				t.Fatal("alice 应已登记后台关闭")
			}
		})
	}
}

// TestBidiSetupDeadlineSparesSession 建立期限只管取租约：取到租约后的建立（这里耗时为期限的 2 倍，相当于冷启动 Worker、握手）
// 不受它限制，会话建立成功后，会话本身的上下文（由建立用的上下文派生）过了期限也不会被取消。覆盖直接建立，以及先在 alice 遇到 Code 5、
// 关闭完成后重放（恢复回调返回后重新计时、取到租约后解除）两种情况
func TestBidiSetupDeadlineSparesSession(t *testing.T) {
	for _, replay := range []bool{false, true} {
		name := map[bool]string{false: "直接建立", true: "关闭后重放"}[replay]
		t.Run(name, func(t *testing.T) {
			failing := map[string]int{}
			if replay {
				failing[resetTestAlice] = 1
			}
			service, transport, closed := dataResetTestService(t, failing, resetTestAlice)
			const limit = 300 * time.Millisecond
			service.timeout.Store(int64(limit))
			transport.setupDelay = 2 * limit
			session, err := service.OpenBidi(service.lifecycle, aistudio.BidiRequest{Model: testRuntimeModel})
			if err != nil || session == nil {
				t.Fatalf("OpenBidi err=%v，期望建立成功（调用=%v）", err, transport.called())
			}
			want := []string{resetTestAlice}
			if replay {
				want = append(want, resetTestAlice)
			}
			if calls := transport.called(); !slices.Equal(calls, want) {
				t.Fatalf("Live 建立调用 = %v，期望 %v", calls, want)
			}
			sessions := transport.openedSessions()
			if len(sessions) != 1 {
				t.Fatalf("建立了 %d 个会话，期望 1 个", len(sessions))
			}
			time.Sleep(limit + 100*time.Millisecond)
			if err := sessions[0].Err(); err != nil {
				t.Fatalf("会话上下文在建立期限之后被取消: %v（cause=%v）", err, context.Cause(sessions[0]))
			}
			if replay {
				if got := closed[resetTestAlice](); got != 1 {
					t.Fatalf("alice 的 Worker 关闭 %d 次，期望 1 次", got)
				}
			}
		})
	}
}

// TestLiveResetEndsBetweenTimeoutAndCheck 等待到期与检查之间关闭恰好成功结束（可以重放）时：liveResetExpired 不写“后台关闭已放弃”，
// awaitLiveReset 照常建立会话（liveResetOutcome 返回 nil）；放弃与还没结束时仍按 503 结束
func TestLiveResetEndsBetweenTimeoutAndCheck(t *testing.T) {
	ctx := context.Background()
	succeeded := finishedWorkerReset(true, false, nil)
	var notReady *aistudio.AccountsNotReadyError
	text := liveResetExpired(ctx, succeeded, time.Minute).Error()
	if strings.Contains(text, "已放弃") || strings.Contains(text, "内没有关闭重建") || !strings.Contains(text, "刚完成关闭重建") {
		t.Fatalf("关闭恰好成功结束时的错误 = %q，期望写刚完成关闭重建", text)
	}
	if err := liveResetOutcome(ctx, succeeded, time.Minute, false); err != nil {
		t.Fatalf("等待到期后关闭恰好成功结束时 err=%v，期望照常建立会话", err)
	}
	if err := liveResetOutcome(ctx, finishedWorkerReset(false, false, nil), time.Minute, false); err != nil {
		t.Fatalf("版本已经不在时 err=%v，期望照常建立会话", err)
	}
	abandoned := liveResetOutcome(ctx, finishedWorkerReset(false, true, nil), time.Minute, false)
	if !errors.As(abandoned, &notReady) || !strings.Contains(abandoned.Error(), "后台关闭已放弃") {
		t.Fatalf("后台关闭已放弃时 err=%v，期望 503 并写已放弃", abandoned)
	}
	running := liveResetOutcome(ctx, &workerReset{done: make(chan struct{})}, time.Minute, false)
	if !errors.As(running, &notReady) || !strings.Contains(running.Error(), "在 1m0s 内没有关闭重建") {
		t.Fatalf("关闭还没结束时 err=%v，期望 503 并写等满上限", running)
	}
	if err := liveResetOutcome(ctx, &workerReset{done: make(chan struct{})}, time.Minute, true); err != nil {
		t.Fatalf("已经等到结束时 err=%v，期望照常建立会话", err)
	}
}
