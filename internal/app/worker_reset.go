package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
)

// 失败 Worker 的后台关闭。
//
// 请求遇到 Worker 失败（浏览器退出、控制连接断开）或协议 Code 5 时要关闭这个 Worker 版本，关闭前要等同账户其他进行中的请求结束：
// 它们经同一个浏览器流式返回，每个最长可达 REQUEST_TIMEOUT。原先失败的请求同步等这次关闭，自己干等几分钟，等到超时还直接失败、不换号。
// 现在关闭交给后台（scheduleWorkerReset）：
//   - 同一账户的同一 Worker 版本只关闭一次，并发失败的请求与失败 Worker 巡检共用这一次；
//   - 请求释放租约与登记独占等待在账户池的同一次加锁内完成（aistudio.AccountLease.ReleaseToExclusiveReservation），登记保持到关闭结束，
//     期间账户不发出新租约：排队的请求不会落到坏掉的 Worker 上，也不会因 Worker 已失败而原地替换浏览器、切断同账户还在输出的流；
//   - 后台最多等排空超时（REQUEST_TIMEOUT + 30 秒），请求取消后照常继续；ResetAll 与 Close 取消它们并等 goroutine 退出。
//
// 请求只等一小会儿（见 workerResetWaitFor）：不固定账号、还能换号的请求等不到就换号（原账号仍算已尝试），换号找不到可用账号时
// 回到原账号一直等关闭结束后重放；固定账号与文件绑定的请求只能用这个账号，照旧等关闭结束后在原账号重放。
// 换号的取舍：解释器更新后整个号池的驻留 Worker 都可能返回 Code 5，换号会落到另一个同样过时的 Worker 上，原账号重放才是新实例；
// 每次换号仍计入连续失败上限，没有其他进行中请求的账号通常在等待时间内关闭完成、直接在新实例重放，不受影响

const (
	// defaultWorkerResetWait 为不固定账号、还能换号的请求等待同账号关闭失败 Worker 的默认上限。没有其他进行中的请求时关闭只要
	// 浏览器退出的时间，通常在这之内完成、随后在原账号重放；有其他请求还在输出时它们往往还要几十秒到几分钟，换号更快
	defaultWorkerResetWait = 2 * time.Second
	// primaryWorkerResetWait 为主用模型的请求等待关闭的上限：主用模型带排队预算，最多等这么久且不超过剩余预算的四分之一，
	// 预算留给换号后的排队，不在等待关闭上用完（用完后只接受立即可用的账号，否则按 429 结束）
	primaryWorkerResetWait = time.Second
	// workerResetWaitUnbounded 表示一直等到关闭结束（请求取消时返回）
	workerResetWaitUnbounded time.Duration = -1
)

// workerResetSource 为后台关闭的发起方，决定结束时写哪条日志
type workerResetSource int

const (
	// workerResetRequest：请求遇到 Worker 失败或协议 Code 5
	workerResetRequest workerResetSource = iota
	// workerResetSweep：失败 Worker 巡检（见 sweepFailedWorkers）
	workerResetSweep
)

// workerResetKey 标识一次后台关闭：账户与它的 Worker 版本
type workerResetKey struct {
	accountID  string
	generation uint64
}

// workerReset 为一次后台关闭失败 Worker 的结果：done 关闭前写好其余字段，读取方等 done 关闭后再读
type workerReset struct {
	done chan struct{}
	// reset 为关闭了这个 Worker 版本；为 false 且 expired、err 都为空时这个版本已经关闭或替换过，不必再关
	reset bool
	// expired 为后台没等到独占租约就放弃（超过上限，或被 ResetAll、Close 取消），失败的 Worker 没有关闭
	expired bool
	err     error
}

// finishedWorkerReset 返回已经结束的后台关闭结果（不需要等待或不能登记时使用）
func finishedWorkerReset(reset bool, expired bool, err error) *workerReset {
	pending := &workerReset{done: make(chan struct{}), reset: reset, expired: expired, err: err}
	close(pending.done)
	return pending
}

// replayable 判断关闭结束后能否在原账号重放：关闭成功，或这个版本已经不在了（调用方等 done 关闭后再调用）
func (pending *workerReset) replayable() bool {
	return !pending.expired && pending.err == nil
}

// wait 等后台关闭结束，最多等 limit（0 为不等待，workerResetWaitUnbounded 为一直等）；返回是否已经结束，请求取消时返回它的错误。
// 先不阻塞地检查一次：limit 为 0 时已经结束的关闭照样算结束
func (pending *workerReset) wait(ctx context.Context, limit time.Duration) (bool, error) {
	select {
	case <-pending.done:
		return true, nil
	default:
	}
	if limit == 0 {
		return false, nil
	}
	var timeout <-chan time.Time
	if limit > 0 {
		timer := time.NewTimer(limit)
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case <-pending.done:
		return true, nil
	case <-timeout:
		return false, nil
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

// deferralNote 为请求不等关闭结束就换号时写进请求进度的说明（调用方接着写“，换号重试”）
func (pending *workerReset) deferralNote(wait time.Duration) string {
	select {
	case <-pending.done:
		if pending.err != nil {
			return "WAA Worker 恢复时关闭失败的 Worker 出错（" + strings.TrimSpace(pending.err.Error()) + "）"
		}
		return "WAA Worker 恢复的后台关闭已放弃"
	default:
		return fmt.Sprintf("WAA Worker 恢复等待 %s 未完成（同账号仍有进行中的请求），后台继续关闭失败的 Worker", wait.Round(time.Millisecond))
	}
}

// scheduleWorkerReset 在后台关闭账户的这个 Worker 版本并返回这次关闭；同一版本已在关闭时沿用那一次，不再登记第二次。
// lease 为请求仍持有的失败租约（没有时为 nil），返回前一定已经释放：要登记时原子地转成独占等待交给后台，其余情况照常释放
// （已有同版本的关闭时那次已经登记，账户照样不发出新租约）；releaseErr 为释放租约的错误。
// 这个版本已经关闭或替换过时直接返回结束的结果，不再等独占租约；ResetAll、Close 期间不登记，返回放弃的结果（它们随后直接关闭全部 Worker）
func (manager *accountWorkerManager) scheduleWorkerReset(
	accountID string, generation uint64, lease *aistudio.AccountLease, source workerResetSource,
) (pending *workerReset, releaseErr error) {
	manager.mu.RLock()
	account := manager.accounts[accountID]
	manager.mu.RUnlock()
	if account == nil {
		return finishedWorkerReset(false, false, fmt.Errorf("WAA worker 账户不存在: %s", accountID)), lease.Release()
	}
	if account.generation.Load() != generation {
		return finishedWorkerReset(false, false, nil), lease.Release()
	}
	key := workerResetKey{accountID: accountID, generation: generation}
	manager.resetMu.Lock()
	if existing := manager.resets[key]; existing != nil {
		manager.resetMu.Unlock()
		return existing, lease.Release()
	}
	if manager.resetClosed || manager.resetHolds > 0 {
		manager.resetMu.Unlock()
		return finishedWorkerReset(false, true, nil), lease.Release()
	}
	var reservation *aistudio.AccountReservation
	var reserveErr error
	if lease != nil {
		reservation, releaseErr = lease.ReleaseToExclusiveReservation()
	} else {
		reservation, reserveErr = manager.pool.ReserveAccount(accountID)
	}
	if reservation == nil {
		manager.resetMu.Unlock()
		return finishedWorkerReset(false, false, errors.Join(reserveErr, releaseErr)), releaseErr
	}
	// 在 resetMu 内取后台 context：ResetAll 先暂停登记再取消后台 context，登记成功的关闭用的一定是随后会被取消的那一个
	base := manager.backgroundContext()
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, manager.workerResetLimit())
	pending = &workerReset{done: make(chan struct{})}
	if manager.resets == nil {
		manager.resets = make(map[workerResetKey]*workerReset)
	}
	manager.resets[key] = pending
	manager.resetWG.Add(1)
	manager.resetMu.Unlock()
	go manager.runWorkerReset(ctx, cancel, key, reservation, pending, source)
	return pending, releaseErr
}

// runWorkerReset 为 scheduleWorkerReset 的后台部分：等到独占租约后关闭失败的 Worker，写好结果与日志后从登记表移除
func (manager *accountWorkerManager) runWorkerReset(
	ctx context.Context,
	cancel context.CancelFunc,
	key workerResetKey,
	reservation *aistudio.AccountReservation,
	pending *workerReset,
	source workerResetSource,
) {
	defer manager.resetWG.Done()
	defer cancel()
	startedAt := time.Now()
	reset, err := manager.ResetIfGeneration(ctx, reservation, key.accountID, key.generation)
	expired := err != nil && ctx.Err() != nil &&
		(errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled))
	switch {
	case expired:
		err = nil
		// 被 ResetAll、Close 取消时不写日志：它们随后直接关闭全部 Worker
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			manager.logWorkerResetExpired(key.accountID, time.Since(startedAt))
		}
	case err != nil && source == workerResetSweep:
		manager.requests.log(key.accountID, "WARN", "关闭失败的常驻 Worker 出错 | "+strings.TrimSpace(err.Error()))
	case err != nil:
		manager.requests.log(key.accountID, "WARN", "WAA Worker 后台恢复出错 | 错误="+strings.TrimSpace(err.Error()))
	case reset && source == workerResetSweep:
		manager.requests.log(key.accountID, "WARN", "常驻 Worker 已失败，后台关闭以便重新预热")
	}
	pending.reset, pending.expired, pending.err = reset, expired, err
	manager.resetMu.Lock()
	delete(manager.resets, key)
	manager.resetMu.Unlock()
	close(pending.done)
}

// logWorkerResetExpired 记录后台关闭等不到独占租约而放弃。之后账号重新接受请求：浏览器已失败的 Worker 由失败 Worker 巡检
// 下一轮重新登记关闭；协议 Code 5 的 Worker 状态仍正常，巡检不处理，再有请求在它上面失败时重新登记
func (manager *accountWorkerManager) logWorkerResetExpired(accountID string, waited time.Duration) {
	next := "Worker 状态仍正常，再有请求失败时重新登记关闭"
	if manager.WorkerFailed(accountID) {
		next = "失败 Worker 巡检会重新登记关闭"
	}
	manager.requests.log(accountID, "WARN", fmt.Sprintf(
		"WAA Worker 后台恢复放弃 | 等待=%s | 同账号仍有进行中的请求，账号恢复接受请求，%s",
		waited.Round(time.Second), next,
	))
}

// workerResetLimit 返回后台关闭等待独占租约的上限：默认为排空超时（REQUEST_TIMEOUT + 30 秒，进行中的请求都受 REQUEST_TIMEOUT 限制），
// resetLimit 大于 0 时用它（测试调低）
func (manager *accountWorkerManager) workerResetLimit() time.Duration {
	if limit := time.Duration(manager.resetLimit.Load()); limit > 0 {
		return limit
	}
	return manager.drainTimeout()
}

// pauseWorkerResets 暂停登记新的后台关闭（ResetAll 使用，可嵌套），返回恢复登记的函数。
// 暂停期间登记的请求得到放弃的结果；resetWG 只在没有暂停时 Add，暂停后 Wait 不会与新的 Add 并发
func (manager *accountWorkerManager) pauseWorkerResets() func() {
	manager.resetMu.Lock()
	manager.resetHolds++
	manager.resetMu.Unlock()
	return func() {
		manager.resetMu.Lock()
		manager.resetHolds--
		manager.resetMu.Unlock()
	}
}

// closeWorkerResets 不再登记新的后台关闭，并等进行中的后台 goroutine 退出（Close 使用；调用方已取消 lifecycle，它们随之放弃）
func (manager *accountWorkerManager) closeWorkerResets() {
	manager.resetMu.Lock()
	manager.resetClosed = true
	manager.resetMu.Unlock()
	manager.resetWG.Wait()
}

// workerResetWaitFor 返回不固定账号、还能换号的请求等待同账号关闭失败 Worker 的上限：默认 defaultWorkerResetWait（resetWait 可改，
// 测试使用，0 为不等待）；主用模型的请求带排队预算，最多等 primaryWorkerResetWait 且不超过剩余预算的四分之一，预算用完时不等待
func (service *trackedService) workerResetWaitFor(budget *leaseWaitBudget) time.Duration {
	wait := defaultWorkerResetWait
	if configured := service.resetWait.Load(); configured != nil {
		wait = max(*configured, 0)
	}
	if budget != nil {
		wait = max(min(wait, primaryWorkerResetWait, budget.remaining()/4), 0)
	}
	return wait
}

// switchUnavailable 判断换号是否没有别的账号可用：没有账号符合条件、候选全部冷却，或不等待的选号没有立即可用的账号
func switchUnavailable(err error) bool {
	var cooling *aistudio.AllCoolingError
	var noImmediate *noImmediateLeaseError
	return errors.Is(err, aistudio.ErrNoEligibleAccount) || errors.As(err, &cooling) || errors.As(err, &noImmediate)
}
