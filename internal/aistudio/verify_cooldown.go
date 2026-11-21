package aistudio

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// 需要验证账号（403 Your account must be verified … verify-account）分两种（见 app 的 permission_denied.go）：
//   - 整户暂停：账户登记里有两个及以上模型返回过需要验证账号，全局 * 冷却，原因 VerifyRequiredPauseReason，账户不可调度、驻留 Worker 释放；
//   - 单个模型需要验证：只在一个模型上返回过（线上 141 个 Pro 账号里有 66 个只在 /pro 的主用模型上要求验证、其他模型照常），只在这个模型的
//     Playground 键 <模型> 与 Build 键 build:<模型> 上写长冷却，原因 ModelVerifyRequiredReason。账户其他模型照常调度：这个冷却不算死槽位、
//     不算账户冷却状态、不触发淘汰与轮换（见 longCooldownActive、accountModelCoolingLocked、WarmWorkerStates），只让该模型的候选集不选它。
// 两种原因都以“次返回 403 无权限，自动暂停”结尾：认证文件更新、管理页验证通过时与其他 403 冷却一起清除；管理页“清除 403 记录”两种都解除

// ModelVerifyRequiredReason 为单个模型需要验证账号的按模型冷却原因。已持久化的冷却只能按这串逐字识别：不能改动文字；
// 不以“需要验证账号（verify-account）”开头（与整户暂停区分，管理页与诊断脚本按开头分类），不含邮箱、模型名与密钥（作用域键本身就是模型）
const ModelVerifyRequiredReason = "单个模型需要验证账号（verify-account），在官网验证后到管理页点验证或清除 403 记录；该模型已 1 次返回 403 无权限，自动暂停"

// 需要验证账号的冷却种类（VerifyCooldownKind 的返回值，管理接口 /api/cooldowns 的 verify 字段）
const (
	VerifyCooldownAccount = "account"
	VerifyCooldownModel   = "model"
)

// VerifyCooldownKind 返回冷却原因对应的需要验证账号种类：VerifyCooldownAccount（整户暂停）、VerifyCooldownModel（单个模型需要验证），
// 其他冷却为空
func VerifyCooldownKind(reason string) string {
	switch reason {
	case VerifyRequiredPauseReason:
		return VerifyCooldownAccount
	case ModelVerifyRequiredReason:
		return VerifyCooldownModel
	}
	return ""
}

// modelVerifyCooldown 判断按模型的冷却是否为单个模型需要验证账号（不看是否到期）
func modelVerifyCooldown(key string, cooldown CooldownState) bool {
	return key != globalCooldownKey && cooldown.Reason == ModelVerifyRequiredReason
}

// modelVerifyKeys 返回单个模型需要验证账号要写的两个冷却键：Playground 键按账户目录归一（与 MarkCooldownIfGeneration 写入的键一致），
// Build 键为 build:<模型>（与租约在 Build 上时 CooldownScope 写入的阶梯 403 冷却同一构造，候选集按同一键查）
func modelVerifyKeys(account *Account, modelID string) (string, string) {
	modelID = strings.TrimPrefix(strings.TrimSpace(modelID), "models/")
	return canonicalAccountModelID(account, modelID), ChannelCooldownScope(ChannelBuild, modelID)
}

// MarkModelVerifyCooldownIfGeneration 为单个模型需要验证账号写入按模型的长冷却：Playground 键与 Build 键（见 modelVerifyKeys）在一次
// updateRuntime 里写入、一次写盘，原因 ModelVerifyRequiredReason，不论 Build 是否启用（热更新开启 Build 后同样不选它）。
// 目录代际不符时都不写（返回 CooldownSkippedGeneration）；每个键各按检查时间判断，已有不早于 checkedAt 的记录时跳过这个键；
// 已有仍生效且更晚的冷却时保留它、只推进检查时间（不缩短）。返回写入（含保留更晚的）的键数与第一个没有写入的键的原因（都写入时为 CooldownWritten）
func (p *AccountPool) MarkModelVerifyCooldownIfGeneration(
	accountID string, modelID string, generation uint64, checkedAt time.Time, until time.Time,
) (int, CooldownWrite, error) {
	if p == nil {
		return 0, 0, ErrAccountNotFound
	}
	if strings.TrimPrefix(strings.TrimSpace(modelID), "models/") == "" {
		return 0, 0, errors.New("模型 ID 不能为空")
	}
	if !until.After(time.Now()) {
		return 0, 0, errors.New("冷却期限必须在未来")
	}
	checkedAt = checkedAt.UTC()
	if checkedAt.IsZero() {
		return 0, 0, errors.New("冷却检查时间不能为空")
	}
	written, write := 0, CooldownWrite(0)
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		written, write = 0, CooldownWritten
		if generation != account.modelAccessGeneration {
			write = CooldownSkippedGeneration
			return false, nil, nil
		}
		playground, build := modelVerifyKeys(account, modelID)
		now := time.Now()
		changed := false
		for _, key := range []string{playground, build} {
			access := runtimeState.ModelAccess[key]
			if !access.CheckedAt.Before(checkedAt) {
				write = CooldownSkippedStale
				continue
			}
			access.CheckedAt = checkedAt
			runtimeState.ModelAccess[key] = access
			changed = true
			written++
			if current, cooling := runtimeState.Cooldowns[key]; cooling && current.Active(now) && current.Until.After(until) {
				continue
			}
			runtimeState.Cooldowns[key] = CooldownState{Until: until.UTC(), Reason: ModelVerifyRequiredReason}
		}
		return changed, nil, nil
	})
	if err != nil {
		return 0, 0, err
	}
	return written, write, nil
}

// ModelVerifyCooled 返回账户在该模型上单个模型需要验证账号的冷却此刻是否生效（Playground 键或 Build 键任一），以及其中较晚的恢复时间
func (p *AccountPool) ModelVerifyCooled(accountID string, modelID string) (time.Time, bool) {
	if p == nil {
		return time.Time{}, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	account := p.byID[strings.TrimSpace(accountID)]
	if account == nil {
		return time.Time{}, false
	}
	now := time.Now()
	var until time.Time
	playground, build := modelVerifyKeys(account, modelID)
	for _, key := range []string{playground, build} {
		if cooldown, exists := account.runtime.Cooldowns[key]; exists && modelVerifyCooldown(key, cooldown) && cooldown.Active(now) &&
			cooldown.Until.After(until) {
			until = cooldown.Until
		}
	}
	return until, !until.IsZero()
}

// MoveVerifyPauseToModel 把需要验证账号的整户暂停改为只在 modelID 上的单个模型需要验证（启动迁移：登记里只有这一个模型返回过需要验证账号）：
// 一次 updateRuntime 内写 Playground 键与 Build 键的 ModelVerifyRequiredReason 冷却（恢复时间沿用原暂停的，已有更晚的冷却时保留）、
// 删除全局 * 上原因等于 VerifyRequiredPauseReason 的冷却，两个键的检查时间推进到 now（启动时没有进行中的租约；原检查时间晚于 now 时
// 不往回写），先写后删在同一次写盘里。整户暂停此刻没有生效时什么都不做，返回 false
func (p *AccountPool) MoveVerifyPauseToModel(accountID string, modelID string, now time.Time) (bool, error) {
	if p == nil {
		return false, ErrAccountNotFound
	}
	if strings.TrimPrefix(strings.TrimSpace(modelID), "models/") == "" {
		return false, errors.New("模型 ID 不能为空")
	}
	now = now.UTC()
	moved := false
	_, err := p.updateRuntime(accountID, func(account *Account, runtimeState *accountRuntimeState) (bool, func(*Account), error) {
		moved = false
		pause, exists := runtimeState.Cooldowns[globalCooldownKey]
		if !exists || pause.Reason != VerifyRequiredPauseReason || !pause.Active(now) {
			return false, nil, nil
		}
		playground, build := modelVerifyKeys(account, modelID)
		for _, key := range []string{playground, build} {
			if current, cooling := runtimeState.Cooldowns[key]; !cooling || !current.Active(now) || !current.Until.After(pause.Until) {
				runtimeState.Cooldowns[key] = CooldownState{Until: pause.Until, Reason: ModelVerifyRequiredReason}
			}
			if access := runtimeState.ModelAccess[key]; access.CheckedAt.Before(now) {
				access.CheckedAt = now
				runtimeState.ModelAccess[key] = access
			}
		}
		delete(runtimeState.Cooldowns, globalCooldownKey)
		moved = true
		return true, nil, nil
	})
	if err != nil {
		return false, err
	}
	return moved, nil
}

// channelScopeKeys 为 coolingParkedLocked 预先算好的通道与冷却键（ChannelCooldownScope；Build 键要拼接字符串，逐个账户算会每个账户分配一次）
type channelScopeKeys struct {
	channels  []Channel
	keys      []string
	selection AccountSelection
}

// newChannelScopeKeys 按通道顺序算好冷却键；buffer 为调用方栈上的缓冲（通道至多几个）
func newChannelScopeKeys(channels []Channel, selection AccountSelection, scope string, buffer []string) channelScopeKeys {
	keys := buffer[:0]
	for _, channel := range channels {
		keys = append(keys, ChannelCooldownScope(channel, scope))
	}
	return channelScopeKeys{channels: channels, keys: keys, selection: selection}
}

// verifyParkedAccountLocked 一次遍历账户在这些通道中支持选择的通道（channelSupportsLocked）：返回各通道上挡住它的冷却
// （全局 * 与该通道作用域中较晚的一个，与候选集的计算相同）里最早的恢复时间与是否每个都在冷却（同 accountChannelsCooldown），
// 以及是否每个都是需要验证账号的冷却（整户或单个模型）。没有支持的通道或任一通道没有冷却时 cooling 与 parked 都为假；
// 挡住它的有会自行恢复的冷却（每日限额、阶梯 403 等）时 parked 为假。不分配内存；调用方持有 p.mu
func (p *AccountPool) verifyParkedAccountLocked(
	account *Account, scoped channelScopeKeys, now time.Time,
) (until time.Time, cooling bool, parked bool) {
	parked = true
	for at, channel := range scoped.channels {
		if !p.channelSupportsLocked(account, channel, scoped.selection) {
			continue
		}
		cooldown, active := accountCooldown(account, scoped.keys[at], now)
		if !active {
			return time.Time{}, false, false
		}
		parked = parked && VerifyCooldownKind(cooldown.Reason) != ""
		if until.IsZero() || cooldown.Until.Before(until) {
			until = cooldown.Until
		}
	}
	if until.IsZero() {
		return time.Time{}, false, false
	}
	return until, true, parked
}

// coolingParkedLocked 在候选全部冷却、最早恢复为 until 时核对这些冷却中的候选：全部只被需要验证账号的冷却挡住时返回 true
// （它们只能靠人工验证恢复，30 天只是兜底上限）；否则返回会自行恢复的候选里最早的恢复时间（没有时为 until）。
// 与分类同一口径：固定账户或资源时只看那个账户；否则按候选集，排除 ExcludedAccountIDs，限定账户集合时只看集合内的。
// 不限账户时先看冷却堆顶（恢复最早的候选）：挡住它的不是需要验证账号的冷却时，它就是会自行恢复的候选里最早的，直接返回，
// 不再逐个核对——号池在主用模型上用完每日额度这类常见的全部冷却每个 429 都会走到这里，原先每次在 p.mu 内扫一遍全部冷却中的候选，
// 3820 个账户约 2.3 ms，是选号本身的 10 倍；只有堆顶就是需要验证账号的冷却（候选几乎都在等人工验证）时才逐个核对。调用方持有 p.mu
func (p *AccountPool) coolingParkedLocked(selection AccountSelection, until time.Time, now time.Time) (bool, time.Time) {
	selection.ModelID = strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/")
	parked, selfRecovering := 0, time.Time{}
	var keyBuffer [4]string
	check := func(account *Account, scoped channelScopeKeys) {
		recovered, cooling, verify := p.verifyParkedAccountLocked(account, scoped, now)
		switch {
		case !cooling:
		case verify:
			parked++
		case selfRecovering.IsZero() || recovered.Before(selfRecovering):
			selfRecovering = recovered
		}
	}
	result := func() (bool, time.Time) {
		if parked > 0 && selfRecovering.IsZero() {
			return true, until
		}
		if selfRecovering.IsZero() {
			return false, until
		}
		return false, selfRecovering
	}
	if strings.TrimSpace(selection.AccountID) != "" || selection.ResourceID != "" {
		indices, err := p.selectionIndicesLocked(selection)
		if err != nil {
			return false, until
		}
		scoped := newChannelScopeKeys(p.selectionChannelsLocked(selection), selection, selectionAccessScope(selection), keyBuffer[:])
		for _, index := range indices {
			if account := p.accounts[index]; account != nil {
				check(account, scoped)
			}
		}
		return result()
	}
	set := p.candidateSetLocked(selection, now)
	nowNanos := now.UnixNano()
	set.expireLocked(p, nowNanos)
	exclusion := p.exclusionLocked(selection)
	scoped := newChannelScopeKeys(set.channels, set.selection, set.scope, keyBuffer[:])
	cooled := func(position int) *Account {
		if position >= len(p.accounts) || set.until[position] <= nowNanos || exclusion.has(position) {
			return nil
		}
		return p.accounts[position]
	}
	if selection.AllowedAccountIDs != nil {
		// 限定账户集合：集合通常很小，与 classifyAllowedLocked 一样逐个看集合内的账户
		for _, accountID := range selection.AllowedAccountIDs {
			account := p.byID[strings.TrimSpace(accountID)]
			if account == nil || !p.sched.positionValid(p, account) || cooled(account.position) != account {
				continue
			}
			check(account, scoped)
		}
		return result()
	}
	if entry, found := set.earliestCoolingEntryLocked(exclusion); found {
		if account := cooled(entry.position); account != nil {
			if recovered, cooling, verify := p.verifyParkedAccountLocked(account, scoped, now); cooling && !verify {
				return false, recovered
			}
		}
	} else {
		return false, until
	}
	for position := range set.until {
		if account := cooled(position); account != nil {
			check(account, scoped)
		}
	}
	return result()
}

// AllCoolingErrorFor 返回候选全部冷却、最早恢复为 until 时的错误：冷却中的候选全部只被需要验证账号的冷却挡住时 Parked 为真
// （对外 503、不带重试秒数，见 AllCoolingError.HTTPStatus）；否则 Until 为会自行恢复的候选里最早的恢复时间
// （需要验证账号的冷却不计入 Retry-After）
func (p *AccountPool) AllCoolingErrorFor(selection AccountSelection, until time.Time) *AllCoolingError {
	if p == nil {
		return &AllCoolingError{ModelID: strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/"), Until: until}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.allCoolingErrorLocked(selection, until, time.Now())
}

// allCoolingErrorLocked 与 AllCoolingErrorFor 相同；调用方持有 p.mu
func (p *AccountPool) allCoolingErrorLocked(selection AccountSelection, until time.Time, now time.Time) *AllCoolingError {
	parked, earliest := p.coolingParkedLocked(selection, until, now)
	return &AllCoolingError{
		ModelID: strings.TrimPrefix(strings.TrimSpace(selection.ModelID), "models/"),
		Until:   earliest, Parked: parked, Pool: selection.Pool,
	}
}

// verifyParkedText 为 Parked 的 AllCoolingError 写进内部错误文本的说明
func verifyParkedText(modelID string) string {
	return fmt.Sprintf("模型 %s 的候选账户都在等人工验证账号（需要验证账号的暂停），不会自行恢复", modelID)
}
