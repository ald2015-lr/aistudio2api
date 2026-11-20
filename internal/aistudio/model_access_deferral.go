package aistudio

import (
	"time"
)

// 推迟写盘的模型资格成功时间
//
// 生成成功后 MarkGenerationVerifiedAsync 把这次租约的时间写进运行态（runtime-state.json）的模型资格与账号全局 * 的 CheckedAt：
// 之后到达的、租约更早的失败（429 冷却、403 等）按它判为过时，不再写冷却。原先每次成功都要加跨进程锁、读一遍、写一遍运行态文件，
// 同一账号连续成功时几乎每次都只是把 CheckedAt 往后推。现在只推进 CheckedAt 的成功不立即写盘：
//   - 只在这些情况推迟：开启了推迟（SetModelAccessDeferral，生成服务运行期间）、账户有运行态文件、目录代际相同、这个作用域已是
//     “已验证”且没有冷却、账号全局 * 上没有要清除的冷却（短的分钟限额或限流冷却本来就只推进 CheckedAt、等它自行到期，见
//     shortCooldownActive），也没有可清零的连续限流档位（clearableRateStrikeLocked：要清档位的成功照旧立即写盘、写盘后清档位）。
//     其余情况（状态变化、清除冷却、清档位）照旧立即写
//   - 推迟的时间记在账户上（pendingAccess，受账户池 mu 保护），同一作用域只留最晚的一个
//   - 合并进下一次运行态写入：updateRuntimeContext 读完运行态文件、执行任何修改之前，先按成功时的规则把推迟的时间并进去（作用域：
//     CheckedAt 不晚于它时置为已验证并清除该作用域的冷却；全局 *：推进 CheckedAt，非短的冷却清除），再执行这次修改。所以之后的
//     失败写入（冷却、档位、资格）看到的与成功当时立即写盘完全相同，租约更早的失败照旧判为过时；写盘成功后删除已写出的推迟时间
//   - 最多 deferredModelAccessFlushInterval 后由计时器写出；生成服务停止与进程关闭时全部写出并改回立即写（新一代生成服务从磁盘载入，
//     不能留着没写盘的时间）。并入在同步磁盘值之后：别的进程改了目录（同步时目录代际加一）时丢弃旧代际的推迟时间，不把已被清掉的
//     资格写回；本进程的目录变化与重置模型资格都经运行态更新，推迟的时间先并入、再按原规则清除
//
// 只影响写盘次数，不改变调度：推迟的只有已验证作用域的 CheckedAt，状态、冷却与档位都照旧。进程被强杀时每个账号最多丢失
// deferredModelAccessFlushInterval 内的 CheckedAt 推进，重启后少数租约更早的失败可能不再被判为过时（与成功之前到达时相同）

// deferredModelAccessFlushInterval 为推迟的模型资格成功时间最长多久写盘
const deferredModelAccessFlushInterval = 30 * time.Second

// SetModelAccessDeferral 开关推迟写盘的模型资格成功时间（生成服务启动时打开，停止时关闭）。关闭时先把全部推迟的时间写盘，
// 之后每次成功都立即写；返回写盘遇到的第一个错误
func (p *AccountPool) SetModelAccessDeferral(enabled bool) error {
	if p == nil {
		return nil
	}
	p.accessDeferral.Store(enabled)
	if enabled {
		return nil
	}
	return p.FlushDeferredModelAccess()
}

// FlushDeferredModelAccess 把全部账户推迟的模型资格成功时间写盘，返回遇到的第一个错误
func (p *AccountPool) FlushDeferredModelAccess() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	var pending []string
	for _, account := range p.accounts {
		if len(account.pendingAccess) > 0 {
			pending = append(pending, account.ID)
		}
	}
	p.mu.Unlock()
	var first error
	for _, accountID := range pending {
		// 不修改任何内容的运行态更新：读文件后先并入推迟的时间，有变化时写盘（见 updateRuntimeContext）
		if _, err := p.updateRuntime(accountID, func(*Account, *accountRuntimeState) (bool, func(*Account), error) {
			return false, nil, nil
		}); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// deferModelAccessLocked 记下推迟写盘的成功时间：作用域 scope 与账号全局 * 都取较晚的一个。还没有安排写盘时安排一次。调用方持有 p.mu
func (p *AccountPool) deferModelAccessLocked(account *Account, scope string, checkedAt time.Time) {
	if account.pendingAccess == nil || account.pendingAccessGeneration != account.modelAccessGeneration {
		account.pendingAccess = make(map[string]time.Time, 2)
		account.pendingAccessGeneration = account.modelAccessGeneration
	}
	for _, key := range []string{scope, globalCooldownKey} {
		if account.pendingAccess[key].Before(checkedAt) {
			account.pendingAccess[key] = checkedAt
		}
	}
	if !p.accessFlushScheduled {
		p.accessFlushScheduled = true
		interval := p.accessFlushInterval
		if interval <= 0 {
			interval = deferredModelAccessFlushInterval
		}
		time.AfterFunc(interval, func() {
			// 写盘登记为后台写入（WaitBackground 等它结束）；计时期间不登记，免得等待者白等一个间隔
			finish := p.background.begin()
			defer finish()
			p.mu.Lock()
			p.accessFlushScheduled = false
			p.mu.Unlock()
			_ = p.FlushDeferredModelAccess()
		})
	}
}

// mergePendingAccessLocked 按成功时的规则（见 markModelAccessVerifiedClearing）把推迟的时间并进刚从文件读出并同步过的运行态 state，
// 返回并入的时间（写盘成功后据此删除）与 state 是否有变化。目录代际已变时丢弃推迟的时间。调用方持有 p.mu
func mergePendingAccessLocked(account *Account, state *accountRuntimeState, now time.Time) (map[string]time.Time, bool) {
	if len(account.pendingAccess) == 0 {
		return nil, false
	}
	if account.pendingAccessGeneration != account.modelAccessGeneration {
		account.pendingAccess = nil
		return nil, false
	}
	if state.ModelAccess == nil {
		state.ModelAccess = make(map[string]ModelAccess)
	}
	if state.Cooldowns == nil {
		state.Cooldowns = make(map[string]CooldownState)
	}
	merged := make(map[string]time.Time, len(account.pendingAccess))
	changed := false
	for key, checkedAt := range account.pendingAccess {
		merged[key] = checkedAt
		access := state.ModelAccess[key]
		if access.CheckedAt.After(checkedAt) {
			continue
		}
		if key == globalCooldownKey {
			if cooldown, exists := state.Cooldowns[key]; exists && !shortCooldownActive(cooldown, now) {
				delete(state.Cooldowns, key)
				changed = true
			}
			if access.CheckedAt.Before(checkedAt) {
				access.CheckedAt = checkedAt
				state.ModelAccess[key] = access
				changed = true
			}
			continue
		}
		if _, exists := state.Cooldowns[key]; exists {
			delete(state.Cooldowns, key)
			changed = true
		}
		if access.State != ModelAccessVerified || access.CheckedAt.Before(checkedAt) {
			state.ModelAccess[key] = ModelAccess{State: ModelAccessVerified, CheckedAt: checkedAt}
			changed = true
		}
	}
	return merged, changed
}

// forgetMergedAccessLocked 删除已写盘的推迟时间：期间又推迟了更晚的时间的作用域保留。调用方持有 p.mu
func forgetMergedAccessLocked(account *Account, merged map[string]time.Time) {
	for key, checkedAt := range merged {
		if !account.pendingAccess[key].After(checkedAt) {
			delete(account.pendingAccess, key)
		}
	}
}
