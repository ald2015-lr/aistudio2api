package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// OpenBidi 将实时会话绑定到当前生成服务生命周期
func (service *trackedService) OpenBidi(ctx context.Context, request aistudio.BidiRequest) (*aistudio.BidiSession, error) {
	api.SetAccessLogTarget(ctx, request.Model, "")
	request.Model = service.pool.CanonicalModelIDIn(aistudio.PoolScopeFromContext(ctx), request.Model)
	requestCtx, cancel, err := service.bidiRequestContext(ctx)
	if err != nil {
		api.SetAccessLogError(ctx, err)
		return nil, err
	}
	if request.AccountID == "" && len(request.AllowedAccountIDs) == 0 {
		request.AllowedAccountIDs, err = service.bidiCandidates(requestCtx, request.Model, request.ModelAccessScope)
		if err != nil {
			api.SetAccessLogError(ctx, err)
			cancel()
			return nil, err
		}
	}
	workerGenerations := make(map[string]uint64)
	recoveredWorkers := make(map[string]struct{})
	// pinned 为指定账户或会话绑定（只能用这个账号）；resetSelection 为判断恢复时还能不能换号的选号条件（见 dataResetSwitchWait）
	pinned := strings.TrimSpace(request.AccountID) != "" || strings.TrimSpace(request.SessionToken) != ""
	resetSelection := aistudio.AccountSelection{
		ModelID: request.Model, ModelAccessScope: request.ModelAccessScope, Method: "bidiGenerateContent",
		AllowedAccountIDs: request.AllowedAccountIDs,
	}
	request.ObserveWAARuntime = func(accountID string, generation uint64) {
		workerGenerations[accountID] = generation
	}
	request.ObserveModelAccessChange = service.publishModelAccess
	request.ObserveAccountFailure = func(accountID string, cause error) {
		label := accountID
		for _, status := range service.pool.Status() {
			if status.ID == accountID {
				label = status.Label
				break
			}
		}
		service.requests.log(label, "WARN", fmt.Sprintf(
			"账号切换 | 模型=%s\n原因: %s",
			strings.TrimPrefix(request.Model, "models/"), strings.TrimSpace(cause.Error()),
		))
	}
	// 会话建立阶段的期限（见 liveSetupDeadline）：只在取租约（选号排队）时计时，取到租约后解除，会话建立后不再生效
	setupWait := service.liveResetWait()
	setupCtx, setupDeadline := newLiveSetupDeadline(requestCtx, setupWait)
	recoverWorker := func(recoveryCtx context.Context, accountID string, cause error) (bool, error) {
		workerFailed := service.workers.WorkerFailed(accountID)
		waaRuntimeFailed := aistudio.DefinitiveWAARuntimeFailure(cause)
		workerReplaced := errors.Is(cause, errAccountWorkerReplaced)
		recoverCurrentGeneration := needsWAARuntimeRecovery(cause, false, workerFailed, workerReplaced)
		if recoveryCtx.Err() != nil || !recoverCurrentGeneration {
			return false, nil
		}
		expectedGeneration := workerGenerations[accountID]
		// 还能换号时只等一小会儿，等不到就换号；不能换号时最多等 REQUEST_TIMEOUT（Live 没有期限，不能一直挂到同账号的长流结束），
		// 到期按号池暂时不可用（503）结束
		wait, switchable := service.dataResetSwitchWait(recoveryCtx, pinned, resetSelection, workerGenerations, accountID)
		if !switchable {
			wait = service.liveResetWait()
		}
		recovered, _, deferred, recoveryErr := service.recoverWorkerOnce(
			recoveryCtx, nil, accountID, expectedGeneration, recoveredWorkers,
			recoverCurrentGeneration, workerFailed || waaRuntimeFailed, wait,
		)
		if recoveryErr == nil && deferred != nil && !switchable {
			recoveryErr = liveResetExpired(recoveryCtx, deferred, wait)
		}
		return recovered, recoveryErr
	}
	request.RecoverWAARuntime = func(recoveryCtx context.Context, accountID string, cause error) (bool, error) {
		// 恢复回调在取到租约、尝试失败之后调用：等关闭另有上限（dataResetSwitchWait、liveResetWait），不计入建立期限
		setupDeadline.disarm()
		recovered, err := recoverWorker(recoveryCtx, accountID, cause)
		if err == nil {
			// 接下来 aistudio 层换号或在原账号重放，都要重新取租约：重新计时
			setupDeadline.arm()
		}
		return recovered, err
	}
	setupCtx = aistudio.ContextWithAccountSelectionObserver(setupCtx, func(account *aistudio.Account) {
		setupDeadline.disarm()
		workerGenerations[account.ID] = service.workers.WorkerGeneration(account.ID)
		api.SetAccessLogTarget(ctx, request.Model, account.Config.Label)
	})
	bidi, ok := service.service.(aistudio.BidiService)
	if !ok {
		setupDeadline.stop()
		cancel()
		return nil, fmt.Errorf("bidi service 不可用")
	}
	if err := service.awaitLiveReset(setupCtx, request); err != nil {
		setupDeadline.stop()
		api.SetAccessLogError(ctx, err)
		cancel()
		return nil, err
	}
	setupDeadline.arm()
	session, err := bidi.OpenBidi(setupCtx, request)
	if setupDeadline.stop() && errors.Is(context.Cause(setupCtx), errLiveSetupDeadline) {
		// 期限在取租约时到期（客户端先断开时 cause 是请求的取消，照常返回原错误）：建立用的上下文已经取消，这次建立按号池暂时不可用
		// （503）结束。取消前恰好取到了租约、建立却成功时会话的上下文已取消，同样关闭它
		if err == nil && session != nil {
			_ = session.Close()
		}
		err = liveSetupExpired(ctx, setupWait)
	}
	if err != nil {
		api.SetAccessLogError(ctx, err)
		cancel()
		return nil, err
	}
	return session, nil
}

// errLiveSetupDeadline 为 Live 会话建立阶段的期限到期（取消建立用上下文的 cause，见 liveSetupDeadline）
var errLiveSetupDeadline = errors.New("Live 会话建立期限到期")

// liveSetupExpired 返回 Live 会话建立在 wait 内没有取到账号租约时的错误：与 liveResetExpired 同为号池暂时不可用（503），客户端稍后重试
func liveSetupExpired(ctx context.Context, wait time.Duration) error {
	return aistudio.PoolNotReadyIn(aistudio.PoolScopeFromContext(ctx), fmt.Sprintf(
		"Live 会话建立在 %s 内没有取到账号（候选账号的失败 Worker 都在等同账号的请求结束后关闭重建，或并发都已占满，或候选都在冷却、每分钟限速中），稍后重试", wait,
	), nil)
}

// liveSetupDeadline 为 Live 会话建立阶段的期限。Live 的请求上下文没有期限，aistudio 层的取租约也不设上限：候选账号都登记着后台关闭的
// 独占等待（要等同账号的长流结束，最长到总时长上限），或排队后同账号才登记关闭、并发一直占满时，会话建立会挂到长流结束或客户端断开。
// 建立用的上下文（newLiveSetupDeadline 返回）只在取租约时计时：开始建立和恢复回调返回、准备重新取租约时计时（arm），
// 选号观察回调（取到租约后触发）时解除（disarm），到期带 errLiveSetupDeadline 取消建立用的上下文，取租约随之结束。
// 会话本身的上下文由建立用的上下文派生（aistudio 层不另分），所以取消只在计时中发生、与解除在同一把锁内判断，
// 建立结束后调用 stop，之后不再计时，也不会再取消：会话建立成功后不受这个期限影响
type liveSetupDeadline struct {
	mu      sync.Mutex
	cancel  context.CancelCauseFunc
	limit   time.Duration
	timer   *time.Timer
	epoch   uint64
	expired bool
	stopped bool
}

// newLiveSetupDeadline 返回建立用的上下文与它的期限（limit 为每次取租约的上限）；还没有开始计时
func newLiveSetupDeadline(ctx context.Context, limit time.Duration) (context.Context, *liveSetupDeadline) {
	setupCtx, cancel := context.WithCancelCause(ctx)
	return setupCtx, &liveSetupDeadline{cancel: cancel, limit: limit}
}

// arm 开始（或重新开始）计时；已经到期或建立已结束时不再计时
func (deadline *liveSetupDeadline) arm() {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	if deadline.stopped || deadline.expired {
		return
	}
	if deadline.timer != nil {
		deadline.timer.Stop()
	}
	deadline.epoch++
	epoch := deadline.epoch
	deadline.timer = time.AfterFunc(deadline.limit, func() { deadline.fire(epoch) })
}

// fire 为计时到期：只在这一次计时仍有效（没有解除、重新计时或结束）时取消建立用的上下文
func (deadline *liveSetupDeadline) fire(epoch uint64) {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	if deadline.stopped || deadline.expired || deadline.epoch != epoch {
		return
	}
	deadline.expired = true
	deadline.timer = nil
	deadline.cancel(errLiveSetupDeadline)
}

// disarm 解除计时（取到租约后）；已经到期的不撤销
func (deadline *liveSetupDeadline) disarm() {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	deadline.epoch++
	if deadline.timer != nil {
		deadline.timer.Stop()
		deadline.timer = nil
	}
}

// stop 结束建立阶段：之后不再计时，也不会再取消建立用的上下文；返回期限是否已经到期
func (deadline *liveSetupDeadline) stop() bool {
	deadline.mu.Lock()
	defer deadline.mu.Unlock()
	deadline.stopped = true
	deadline.epoch++
	if deadline.timer != nil {
		deadline.timer.Stop()
		deadline.timer = nil
	}
	return deadline.expired
}

// awaitLiveReset 在 Live 会话只能用一个账号（指定账户、会话绑定的账户，或候选只剩一个）、这个账号有进行中的后台关闭时先等它，
// 最多 liveResetWait，等不到返回与恢复回调相同的 503（见 liveResetExpired）。后台关闭登记着独占等待，期间账户不发出新租约，
// 而 Live 的请求上下文没有期限、aistudio 层取租约不设上限：不先等的话，第一次 503 之后按同一账号重试只能在取租约里排到建立期限
// （liveSetupDeadline）到期，503 的原因也看不出是在等后台关闭。只检查进入时已登记的关闭，之后才登记的由建立期限兜底。
// 关闭结束（不论能否重放）或请求能换号时返回 nil，照常建立会话
func (service *trackedService) awaitLiveReset(ctx context.Context, request aistudio.BidiRequest) error {
	if service.workers == nil || !service.workers.hasPendingWorkerResets() {
		return nil
	}
	accountID := service.liveOnlyAccount(ctx, request)
	if accountID == "" {
		return nil
	}
	pending := service.workers.pendingWorkerReset(accountID)
	if pending == nil {
		return nil
	}
	wait := service.liveResetWait()
	finished, err := pending.wait(ctx, wait)
	if err != nil {
		return err
	}
	return liveResetOutcome(ctx, pending, wait, finished)
}

// liveResetOutcome 为 awaitLiveReset 等完（最多 wait）之后的结果：关闭已经结束（finished），或等待到期与这里之间关闭恰好成功结束、
// 可以重放时返回 nil，照常建立会话；否则按 liveResetExpired 返回 503
func liveResetOutcome(ctx context.Context, pending *workerReset, wait time.Duration, finished bool) error {
	if finished {
		return nil
	}
	if done, _ := pending.wait(ctx, 0); done && pending.replayable() {
		return nil
	}
	return liveResetExpired(ctx, pending, wait)
}

// liveOnlyAccount 返回 Live 会话建立只能用的账号：指定账户、会话绑定的账户，或候选只剩一个账户；还能换号或解析不出时返回空
func (service *trackedService) liveOnlyAccount(ctx context.Context, request aistudio.BidiRequest) string {
	if accountID := strings.TrimSpace(request.AccountID); accountID != "" {
		return accountID
	}
	if token := strings.TrimSpace(request.SessionToken); token != "" {
		groups, err := service.pool.ClassifyCandidates(ctx, aistudio.AccountSelection{
			ModelID: request.Model, ModelAccessScope: request.ModelAccessScope, Method: "bidiGenerateContent", ResourceID: token,
		}, service.workers.WarmAccountIDs())
		if err != nil {
			return ""
		}
		owners := slices.Concat(groups.WarmReady, groups.WarmAvailable, groups.WarmBusy, groups.StandbyReady, groups.StandbyBusy)
		if len(owners) == 1 {
			return owners[0]
		}
		return ""
	}
	if len(request.AllowedAccountIDs) == 1 {
		return strings.TrimSpace(request.AllowedAccountIDs[0])
	}
	return ""
}

// WorkerGeneration 返回当前 preparer 对应的 Worker 版本号
func (preparer *accountWorkerPreparer) WorkerGeneration() uint64 {
	return preparer.account.generation.Load()
}

func (service *trackedService) bidiCandidates(ctx context.Context, model string, modelAccessScope string) ([]string, error) {
	modelID := strings.TrimPrefix(strings.TrimSpace(model), "models/")
	selection := aistudio.AccountSelection{
		ModelID: modelID, ModelAccessScope: modelAccessScope, Method: "bidiGenerateContent",
		Pool: aistudio.PoolScopeFromContext(ctx),
	}
	groups, err := service.pool.ClassifyCandidates(ctx, selection, service.workers.WarmAccountIDs())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	warmAvailable := append(append([]string(nil), groups.WarmReady...), groups.WarmAvailable...)
	candidates := service.preferBidiAccounts(warmAvailable, modelID, modelAccessScope)
	candidates = append(candidates, service.preferBidiAccounts(groups.StandbyReady, modelID, modelAccessScope)...)
	candidates = append(candidates, service.preferBidiAccounts(groups.WarmBusy, modelID, modelAccessScope)...)
	candidates = append(candidates, service.preferBidiAccounts(groups.StandbyBusy, modelID, modelAccessScope)...)
	if len(candidates) == 0 {
		// 区分原因：全部冷却按 429，账户都不可调度按 503，没有账户支持该模型才按请求本身的原因
		return nil, service.pool.NoCandidateError(selection, groups)
	}
	return candidates, nil
}

// preferBidiAccounts 在实测资格分组内按账户策略排列候选
func (service *trackedService) preferBidiAccounts(accountIDs []string, modelID string, modelAccessScope string) []string {
	scope := strings.TrimSpace(modelAccessScope)
	if scope == "" {
		scope = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	}
	candidates := service.pool.OrderCandidates(accountIDs, scope)
	if strings.TrimSpace(modelAccessScope) == "" {
		return candidates
	}
	states := service.pool.CandidateStatesForScope(candidates, modelID, modelAccessScope)
	verified := make([]string, 0, len(candidates))
	unverified := make([]string, 0, len(candidates))
	for _, accountID := range candidates {
		if states[accountID].ModelAccess == aistudio.ModelAccessVerified {
			verified = append(verified, accountID)
			continue
		}
		unverified = append(unverified, accountID)
	}
	return append(verified, unverified...)
}

func (service *trackedService) bidiRequestContext(ctx context.Context) (context.Context, context.CancelFunc, error) {
	service.lifecycleMu.Lock()
	if service.state.Load() != serviceRunning || service.dataContext == nil {
		service.lifecycleMu.Unlock()
		return nil, nil, &serviceStoppedError{}
	}
	requestCtx, cancel := context.WithCancel(ctx)
	stopData := context.AfterFunc(service.dataContext, cancel)
	context.AfterFunc(requestCtx, func() {
		stopData()
	})
	service.lifecycleMu.Unlock()
	return requestCtx, func() {
		stopData()
		cancel()
	}, nil
}
