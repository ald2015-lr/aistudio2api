package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// 管理页启用、停用账户立即生效（上游 a4092ef 的 setAccountEnabled，按本分叉重写）：原先保存账户一律先取独占租约，停用一个正在跑长流的账户
// 要等到请求结束（最长 REQUEST_TIMEOUT + 30 秒），等满返回 account_busy。现在只切换启用状态（代理、语言、时区都不变）时：
//   - 不取租约，写回 account.json 并在账户池生效（aistudio.AccountPool.SetEnabled）：停用后不再发出新的生成租约，进行中的请求照常结束；
//   - 同步写手动停用记录（停用记为手动停用，启用清除记录）、IP 池重算出口，与原先保存账户的联动相同；
//   - 等进行中的请求结束、关闭 Worker、重建固定出口（停用，阶段 draining，取得独占租约、开始关闭 Worker 后为 closing），或合并模型目录、
//     没有目录时同步一次（启用，阶段 syncing），都转入后台（accountTransitions），账户行显示阶段与进行中的请求数；
//     模型目录的合并刷新与推送合并几百毫秒后做一次（syncModelCacheSoon）。
//
// Ultra 号池的成员只看权益，不随启停变化；停用后驻留 Worker 由收尾关闭（keepWarm 的不可调度登记也会释放它），分区名额随之空出。
// 同一账户的启停按账户串行：先取消并等待上一次收尾结束（正在关闭 Worker 时等关完），再写入新的状态，启用不会和尚未关完的旧 Worker 撞车。
// 再次提交账户当前的状态（原样保存、批量停用里包含正在收尾的账户、重复提交）不取消同方向进行中的收尾，阶段照常显示；
// 写入失败（另一个进程持有账户文件锁、写 account.json 失败）时被取消的收尾按原阶段重新开始（逐个与批量启用相同）。
// 改代理等其他字段、删除账户（单个与批量）、就地重新导入先取消收尾，操作失败（账户正忙、删除失败）或就地重新导入完成后按取消前的阶段
// 重新开始收尾（resumeTransition），收尾不会因为其他操作被吞掉；批量启用同样先取消停用收尾再写入（见 enableAccountConfig）。
// 生成服务实例关闭时取消全部收尾并等它们退出（closeRuntime）

// modelSyncCoalesce 为启停后合并刷新模型目录与推送快照的等待：批量启停时几百个账户只合并刷新几次，不为每个账户构建一次全部账户的状态
const modelSyncCoalesce = 300 * time.Millisecond

// accountTransitions 为账户启停的后台收尾与按账户串行的操作锁；零值可用
type accountTransitions struct {
	mu      sync.Mutex
	running map[string]*accountTransition
	locks   map[string]*accountOpLock
	closed  bool
}

// accountTransition 为一个账户进行中的收尾，stage 为它的阶段（draining 或 syncing）
type accountTransition struct {
	stage  aistudio.AccountTransition
	cancel context.CancelFunc
	done   chan struct{}
}

// accountOpLock 为一个账户的操作锁，refs 为持有与等待的数量（为 0 时从表里删除）
type accountOpLock struct {
	mu   sync.Mutex
	refs int
}

// lock 取得账户的操作锁并返回解锁函数：启停（setAccountEnabled）、保存账户时停用记录的复核与写入、批量与自动启用、
// 改为跟随默认代理、删除账户按账户串行。不取租约的启停与持有租约读写账户配置的路径因此不会交错：自动启用在锁内复核停用记录，
// 与手动停用之间没有空档；读出配置改一个字段再写回的路径不会盖掉刚切换的启用状态
func (transitions *accountTransitions) lock(accountID string) func() {
	transitions.mu.Lock()
	if transitions.locks == nil {
		transitions.locks = make(map[string]*accountOpLock)
	}
	entry := transitions.locks[accountID]
	if entry == nil {
		entry = &accountOpLock{}
		transitions.locks[accountID] = entry
	}
	entry.refs++
	transitions.mu.Unlock()
	entry.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			entry.mu.Unlock()
			transitions.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(transitions.locks, accountID)
			}
			transitions.mu.Unlock()
		})
	}
}

// cancel 取消账户进行中的收尾并等它退出；没有收尾时立即返回。收尾里的等待都随 ctx 结束，关闭 Worker 时等它关完
func (transitions *accountTransitions) cancel(accountID string) {
	transitions.mu.Lock()
	transition := transitions.running[accountID]
	transitions.mu.Unlock()
	if transition != nil {
		transition.cancel()
		<-transition.done
	}
}

// active 返回账户是否有进行中的收尾
func (transitions *accountTransitions) active(accountID string) bool {
	return transitions.stage(accountID) != ""
}

// stage 返回账户进行中的收尾的阶段，没有收尾时为空
func (transitions *accountTransitions) stage(accountID string) aistudio.AccountTransition {
	transitions.mu.Lock()
	defer transitions.mu.Unlock()
	if transition := transitions.running[accountID]; transition != nil {
		return transition.stage
	}
	return ""
}

// start 在后台运行账户的收尾（阶段为 stage）：parent 结束（生成服务实例的生命周期）或 cancel 时 ctx 结束。已经关闭时不启动、返回 false。
// 调用方应持有账户操作锁并已取消上一次收尾；仍有遗留时先取消并等它退出
func (transitions *accountTransitions) start(
	parent context.Context, accountID string, stage aistudio.AccountTransition, run func(context.Context),
) bool {
	ctx, cancel := context.WithCancel(parent)
	transition := &accountTransition{stage: stage, cancel: cancel, done: make(chan struct{})}
	transitions.mu.Lock()
	if transitions.closed {
		transitions.mu.Unlock()
		cancel()
		return false
	}
	if transitions.running == nil {
		transitions.running = make(map[string]*accountTransition)
	}
	previous := transitions.running[accountID]
	transitions.running[accountID] = transition
	transitions.mu.Unlock()
	if previous != nil {
		previous.cancel()
		<-previous.done
	}
	go func() {
		defer close(transition.done)
		defer cancel()
		defer func() {
			transitions.mu.Lock()
			if transitions.running[accountID] == transition {
				delete(transitions.running, accountID)
			}
			transitions.mu.Unlock()
		}()
		run(ctx)
	}()
	return true
}

// close 取消全部收尾并等它们退出，之后不再启动新的（生成服务实例关闭时，在关闭 Worker 之前调用）
func (transitions *accountTransitions) close() {
	transitions.mu.Lock()
	transitions.closed = true
	running := make([]*accountTransition, 0, len(transitions.running))
	for _, transition := range transitions.running {
		running = append(running, transition)
	}
	transitions.mu.Unlock()
	for _, transition := range running {
		transition.cancel()
	}
	for _, transition := range running {
		<-transition.done
	}
}

// enableOnlyChange 判断保存账户是否只涉及启用状态：代理、语言、时区都与账户现有配置相同（启用状态可以不变，例如页面上的状态已过时、
// 再次停用已停用的账户、原样保存）。这些字段不变时固定出口与 Worker 配置都不用重建，原先原样保存也要等独占租约、关闭 Worker
func enableOnlyChange(current aistudio.AccountStatus, next aistudio.AccountConfig) bool {
	return current.Proxy == next.Proxy && current.Locale == next.Locale && current.Timezone == next.Timezone
}

// setAccountEnabled 只切换启用状态时立即生效并返回（见文件开头）：写回 account.json 与账户池、写停用记录、IP 池重算出口，
// 启用状态有变化时在后台收尾。账户已是目标状态、同方向的收尾还在进行时（原样保存、批量停用里包含正在收尾的账户、重复提交）
// 保留这次收尾，不取消，阶段照常显示。账户在另一个进程里有租约时返回 account_busy，不存在时返回 account_not_found
func (admin *runtimeAdmin) setAccountEnabled(ctx context.Context, accountID string, enabled bool) (api.AdminAccount, error) {
	unlock := admin.transitions.lock(accountID)
	defer unlock()
	// cancelled 为这次取消的收尾的阶段：写入失败时按它重新开始
	var cancelled aistudio.AccountTransition
	if !admin.sameDirectionTransition(accountID, enabled) {
		cancelled = admin.transitions.stage(accountID)
		admin.transitions.cancel(accountID)
	}
	var account *aistudio.Account
	var changed bool
	if err := admin.service.changeModels(func() (err error) {
		account, changed, err = admin.pool.SetEnabled(accountID, enabled)
		return err
	}); err != nil {
		// 写入失败（另一个进程持有账户文件锁、写 account.json 失败）时启用状态没变：被取消的收尾按原阶段重新开始，停用的账户照常
		// 等请求结束、关闭 Worker、重建固定出口。原先直接返回，账户停在没有收尾的停用状态，Worker 只能等淘汰循环关闭
		admin.resumeTransitionLocked(accountID, cancelled)
		return api.AdminAccount{}, accountOperationError(err)
	}
	label := account.Config.Label
	if enabled {
		// 管理员启用时清除包括手动停用在内的记录
		accountDisableNotes.clear(account.ID)
	} else {
		// 与原先保存账户相同：已经停用的账户再次停用同样记为手动停用，已有手动停用记录的更新为这次的时间
		reason := "通过管理页或管理接口停用"
		if caller := api.AdminCaller(ctx); caller != "" {
			reason += "（来源 " + caller + "）"
		}
		accountDisableNotes.set(account.ID, reason, false)
		if changed {
			admin.requests.log(account.ID, "WARN", "账户已停用 | "+reason)
		} else {
			admin.requests.log(account.ID, "INFO", "停用账户记为手动停用，重启与认证文件更新后保持停用 | "+reason)
		}
	}
	// 与原先保存账户相同：IP 池重算账户的外部占用、固定与默认出口
	admin.ipPool().refresh(admin.pool, account.ID)
	if changed {
		admin.startTransition(account.ID, label, transitionStage(enabled))
		admin.syncModelCacheSoon()
	}
	admin.requests.log("auth", "INFO", "账户配置已更新 | 账户="+label)
	return admin.account(account.ID)
}

// transitionStage 返回启用状态 enabled 对应的收尾阶段：启用为 syncing，停用为 draining
func transitionStage(enabled bool) aistudio.AccountTransition {
	if enabled {
		return aistudio.AccountSyncing
	}
	return aistudio.AccountDraining
}

// sameDirectionTransition 判断账户已是启用状态 enabled、且正在进行同方向的收尾（停用对 draining、启用对 syncing）：
// 再次提交同一状态时保留这次收尾。调用方持有账户操作锁（启用状态只在锁内改变）
func (admin *runtimeAdmin) sameDirectionTransition(accountID string, enabled bool) bool {
	status, ok := admin.pool.StatusOf(accountID)
	return ok && status.Enabled == enabled && admin.transitions.stage(accountID) == transitionStage(enabled)
}

// startTransition 在后台开始账户的收尾：阶段 draining 为 finishDisable，syncing 为 finishEnable。账户行随即显示阶段；
// 生成服务实例正在关闭时不再收尾（Worker 随实例关闭），阶段清除。调用方持有账户操作锁并已取消上一次收尾
func (admin *runtimeAdmin) startTransition(accountID string, label string, stage aistudio.AccountTransition) {
	finish := admin.finishEnable
	if stage == aistudio.AccountDraining {
		finish = admin.finishDisable
	}
	admin.pool.SetTransition(accountID, stage)
	if !admin.transitions.start(admin.lifecycle, accountID, stage, func(ctx context.Context) {
		finish(ctx, accountID, label)
	}) {
		admin.pool.SetTransition(accountID, "")
	}
}

// cancelTransition 取消账户进行中的收尾并返回取消前的阶段（没有收尾时为空），改代理、删除、就地重新导入在取租约或删除之前调用，
// 之后按需要用 resumeTransition 恢复。只在取消时短暂持有账户操作锁：之后等独占租约时不持有它（持有租约的路径先取租约再取锁）
func (admin *runtimeAdmin) cancelTransition(accountID string) aistudio.AccountTransition {
	unlock := admin.transitions.lock(accountID)
	defer unlock()
	stage := admin.transitions.stage(accountID)
	admin.transitions.cancel(accountID)
	return stage
}

// resumeTransitionLocked 为被其他操作取消的收尾重新开始（stage 为取消前的阶段）：停用收尾只在账户仍停用、启用收尾只在账户仍启用时
// 重新开始，账户已不存在、状态已经变了、已有新的收尾或 stage 为空时什么也不做。调用方持有账户操作锁
func (admin *runtimeAdmin) resumeTransitionLocked(accountID string, stage aistudio.AccountTransition) {
	if stage == "" || admin.transitions.active(accountID) {
		return
	}
	status, ok := admin.pool.StatusOf(accountID)
	if !ok || transitionStage(status.Enabled) != stage {
		return
	}
	admin.startTransition(accountID, status.Label, stage)
}

// resumeTransition 取得账户操作锁后调用 resumeTransitionLocked
func (admin *runtimeAdmin) resumeTransition(accountID string, stage aistudio.AccountTransition) {
	if stage == "" {
		return
	}
	unlock := admin.transitions.lock(accountID)
	defer unlock()
	admin.resumeTransitionLocked(accountID, stage)
}

// finishDisable 为停用的收尾（阶段 draining）：登记独占等待（账户已停用，不再发出新的生成租约），等进行中的请求都结束、取得独占租约后
// 关闭 Worker 并重建固定出口（关闭旧出口的空闲连接），与原先保存账户在租约内做的相同。ctx 结束（再次启停、删除、实例关闭）时放弃等待。
// 取得独占租约时账户可能正由预热这类不持有租约的启动打开 Worker（期间持有 startupMu，最长约 INIT_TIMEOUT）：先可取消地等启动结束
// （waitOpening），再开始关闭、阶段改为 closing。原先直接 Reset，在 startupMu 上等到启动结束、不能取消，紧接着的再启用、删除、
// 就地重新导入都排在后面，刚启动好的浏览器还会被关掉；等启动期间被取消时不关闭 Worker（再启用的账户留着它，删除与实例关闭自己关闭）
func (admin *runtimeAdmin) finishDisable(ctx context.Context, accountID string, label string) {
	defer admin.endTransition(accountID)
	reservation, err := admin.pool.ReserveAccount(accountID)
	if err != nil {
		return
	}
	lease, err := reservation.Wait(ctx)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, aistudio.ErrAccountNotFound) {
			admin.requests.log(label, "WARN", "账户停用收尾失败 | 错误="+strings.TrimSpace(err.Error()))
		}
		return
	}
	account := lease.Account()
	var finishErr error
	if !account.Config.Enabled {
		if err := admin.workers.waitOpening(ctx, account.ID); err != nil {
			if releaseErr := lease.Release(); releaseErr != nil {
				admin.requests.log(label, "WARN", "账户停用收尾失败 | 错误="+strings.TrimSpace(releaseErr.Error()))
			}
			return
		}
		// 账户行随即显示正在关闭（推送与其他账户变化合并，见 publishModelAccess）；收尾结束时 endTransition 清除
		admin.pool.SetTransition(accountID, aistudio.AccountClosing)
		if admin.service != nil {
			admin.service.publishModelAccess()
		}
		finishErr = admin.workers.Reset(account.ID)
		if update, err := admin.headers.prepareUpdate(account, account.Config); err != nil {
			finishErr = errors.Join(finishErr, err)
		} else {
			update.Commit()
		}
	}
	if err := errors.Join(finishErr, lease.Release()); err != nil {
		admin.requests.log(label, "WARN", "账户停用收尾失败 | 错误="+strings.TrimSpace(err.Error()))
	}
}

// finishEnable 为启用的收尾（阶段 syncing）：停用账户不参与启动时的模型目录同步，启用后没有目录就不能预热、接不到请求，
// 没有目录时在这里同步一次（与批量启用共用并发名额，见 backfillCatalogs）；有目录时只等模型目录合并刷新
func (admin *runtimeAdmin) finishEnable(ctx context.Context, accountID string, _ string) {
	defer admin.endTransition(accountID)
	if status, ok := admin.pool.StatusOf(accountID); ok && status.Enabled && len(status.Models) == 0 {
		admin.backfillCatalogs(ctx, []string{accountID})
	}
}

// endTransition 清除账户的收尾阶段并合并推送账户列表
func (admin *runtimeAdmin) endTransition(accountID string) {
	admin.pool.SetTransition(accountID, "")
	if admin.service != nil && admin.lifecycle.Err() == nil {
		admin.service.publishModelAccess()
	}
}

// syncModelCacheSoon 合并几百毫秒内的启停，之后刷新一次模型目录并推送快照（见 syncModelCache）：批量启停时不为每个账户
// 构建一次全部账户的状态
func (admin *runtimeAdmin) syncModelCacheSoon() {
	if !admin.modelSyncPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(modelSyncCoalesce, func() {
		admin.modelSyncPending.Store(false)
		if admin.lifecycle.Err() != nil {
			return
		}
		admin.syncModelCache()
	})
}
