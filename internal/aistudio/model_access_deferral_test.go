package aistudio

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// deferralPool 返回有运行态文件的单账户号池（账户目录在 TempDir 下）：Playground 与 Build 目录都支持 strikeModel
func deferralPool(t *testing.T) (*AccountPool, string) {
	t.Helper()
	store := NewAccountStore(t.TempDir())
	pool := NewAccountPool(nil, 2)
	t.Cleanup(pool.WaitBackground)
	account, publish, err := store.Create(DefaultAccountConfig(strikeAccount), testStorageState("OLD"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(account); err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	model := Model{ID: strikeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
	if err := pool.SetCatalog(strikeAccount, BenefitTierFree, []Model{model}); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetBuildCatalog(strikeAccount, []Model{model}); err != nil {
		t.Fatal(err)
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	return pool, account.RuntimePath
}

// diskAccess 返回运行态文件里作用域的模型资格与冷却
func diskAccess(t *testing.T, path string, scope string) (ModelAccess, CooldownState, bool) {
	t.Helper()
	state, err := readRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	cooldown, cooling := state.Cooldowns[scope]
	return state.ModelAccess[scope], cooldown, cooling
}

func pendingAccessCount(pool *AccountPool) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return len(pool.byID[strikeAccount].pendingAccess)
}

// verifyAt 以 checkedAt 记一次生成成功（MarkGenerationVerifiedAsync 的同步部分）
func verifyAt(t *testing.T, pool *AccountPool, scope string, checkedAt time.Time) (bool, int) {
	t.Helper()
	changed, cleared, err := pool.markModelAccessVerifiedClearing(strikeAccount, scope, pool.ModelAccessGeneration(strikeAccount), checkedAt)
	if err != nil {
		t.Fatal(err)
	}
	return changed, cleared
}

// TestDeferredModelAccessFewerWrites 已验证的作用域连续成功只推进 CheckedAt：推迟时一次也不写运行态文件（原先每次都写），
// 关闭推迟（生成服务停止、进程关闭）时写出最晚的时间，重新读取运行态文件（重启）时不丢；状态变化的第一次成功照旧立即写
func TestDeferredModelAccessFewerWrites(t *testing.T) {
	const successes = 50
	count := func(deferral bool) (int, *AccountPool, string, time.Time) {
		pool, path := deferralPool(t)
		base := time.Now().Add(-time.Hour)
		if changed, _ := verifyAt(t, pool, strikeModel, base); !changed {
			t.Fatal("第一次成功应把资格写成已验证")
		}
		if access, _, _ := diskAccess(t, path, strikeModel); access.State != ModelAccessVerified {
			t.Fatalf("状态变化应立即写盘: %+v", access)
		}
		_ = pool.SetModelAccessDeferral(deferral)
		previous, _ := os.ReadFile(path)
		writes := 0
		last := base
		for index := 1; index <= successes; index++ {
			last = base.Add(time.Duration(index) * time.Millisecond)
			if changed, cleared := verifyAt(t, pool, strikeModel, last); changed || cleared != 0 {
				t.Fatalf("只推进 CheckedAt 的成功 changed=%t cleared=%d", changed, cleared)
			}
			current, _ := os.ReadFile(path)
			if !bytes.Equal(current, previous) {
				writes++
				previous = current
			}
		}
		return writes, pool, path, last
	}
	direct, _, _, _ := count(false)
	if direct != successes {
		t.Fatalf("不推迟时每次成功都写盘：%d 次，期望 %d", direct, successes)
	}
	writes, pool, path, last := count(true)
	if writes != 0 || pendingAccessCount(pool) != 2 {
		t.Fatalf("推迟时写盘 %d 次、推迟 %d 个作用域，期望 0 次、2 个（模型与全局 *）", writes, pendingAccessCount(pool))
	}
	// 同一时刻再成功一次：快路径按推迟的时间判断没有新东西
	if changed, _ := verifyAt(t, pool, strikeModel, last); changed {
		t.Fatal("重复成功不应有变化")
	}
	// 关闭推迟（停止或关闭）：全部写出
	if err := pool.SetModelAccessDeferral(false); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{strikeModel, globalCooldownKey} {
		if access, _, _ := diskAccess(t, path, scope); !access.CheckedAt.Equal(last.UTC()) {
			t.Fatalf("%s：写出后磁盘 CheckedAt=%s，期望 %s", scope, access.CheckedAt, last.UTC())
		}
	}
	if pendingAccessCount(pool) != 0 {
		t.Fatal("写出后不应再有推迟的时间")
	}
	// 重启：新的账户池从同一目录载入，CheckedAt 是最后一次成功的
	restarted := NewAccountPool(nil, 2)
	t.Cleanup(restarted.WaitBackground)
	accounts, err := NewAccountStore(filepath.Dir(filepath.Dir(path))).Load()
	if err != nil || len(accounts) != 1 {
		t.Fatalf("重新载入: %d %v", len(accounts), err)
	}
	if err := restarted.Add(accounts[0]); err != nil {
		t.Fatal(err)
	}
	_, access := runtimeSnapshot(t, restarted, strikeAccount)
	if access[strikeModel].State != ModelAccessVerified || !access[strikeModel].CheckedAt.Equal(last.UTC()) || !access[globalCooldownKey].CheckedAt.Equal(last.UTC()) {
		t.Fatalf("重启后资格 = %+v，期望已验证且 CheckedAt=%s", access, last.UTC())
	}
	// 关闭推迟之后照旧每次写
	next := last.Add(time.Millisecond)
	verifyAt(t, pool, strikeModel, next)
	if access, _, _ := diskAccess(t, path, strikeModel); !access.CheckedAt.Equal(next.UTC()) || pendingAccessCount(pool) != 0 {
		t.Fatalf("关闭推迟后应立即写: %+v", access)
	}
}

// TestDeferredModelAccessMergedBeforeWrites 推迟的成功时间在下一次运行态修改之前并入：取得租约早于这次成功的 429 照旧判为过时（与立即写盘相同），
// 那次运行态读写顺带把推迟的时间写出；更晚的 429 照常写入
func TestDeferredModelAccessMergedBeforeWrites(t *testing.T) {
	pool, path := deferralPool(t)
	generation := pool.ModelAccessGeneration(strikeAccount)
	base := time.Now().Add(-time.Hour)
	verifyAt(t, pool, strikeModel, base)
	_ = pool.SetModelAccessDeferral(true)
	successAt := base.Add(10 * time.Second)
	verifyAt(t, pool, strikeModel, successAt)
	if access, _, _ := diskAccess(t, path, strikeModel); !access.CheckedAt.Equal(base.UTC()) || pendingAccessCount(pool) == 0 {
		t.Fatalf("应推迟写盘: %+v", access)
	}
	write, err := pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, successAt.Add(-time.Second), time.Now().Add(time.Hour), "旧租约")
	if err != nil || write != CooldownSkippedStale {
		t.Fatalf("取得租约早于推迟的成功的 429：write=%s err=%v，期望 %s", write, err, CooldownSkippedStale)
	}
	if access, _, cooling := diskAccess(t, path, strikeModel); cooling || !access.CheckedAt.Equal(successAt.UTC()) || access.State != ModelAccessVerified ||
		pendingAccessCount(pool) != 0 {
		t.Fatalf("推迟的时间应并进这次运行态写入: %+v cooling=%t pending=%d", access, cooling, pendingAccessCount(pool))
	}
	// 全局 * 上的 429 同样按推迟的时间判断
	verifyAt(t, pool, strikeModel, successAt.Add(time.Second))
	write, err = pool.TryMarkCooldownIfGeneration(strikeAccount, "", generation, successAt.Add(500*time.Millisecond), time.Now().Add(time.Hour), "旧租约")
	if err != nil || write != CooldownSkippedStale {
		t.Fatalf("全局 429：write=%s err=%v，期望 %s", write, err, CooldownSkippedStale)
	}
	write, err = pool.TryMarkCooldownIfGeneration(strikeAccount, strikeModel, generation, successAt.Add(2*time.Second), time.Now().Add(time.Hour), "新租约")
	if err != nil || write != CooldownWritten {
		t.Fatalf("取得租约晚于成功的 429：write=%s err=%v", write, err)
	}
	// 有冷却时的成功照旧立即写（清除冷却），不推迟
	clearAt := successAt.Add(3 * time.Second)
	verifyAt(t, pool, strikeModel, clearAt)
	if access, _, cooling := diskAccess(t, path, strikeModel); cooling || !access.CheckedAt.Equal(clearAt.UTC()) || pendingAccessCount(pool) != 0 {
		t.Fatalf("清除冷却应立即写盘: %+v cooling=%t", access, cooling)
	}
}

// TestDeferredModelAccessRateStrike 有可清零的连续限流档位时成功不推迟：立即写盘并清零档位（clearableRateStrikeLocked 照旧）
func TestDeferredModelAccessRateStrike(t *testing.T) {
	pool, path := deferralPool(t)
	verifyAt(t, pool, strikeModel, time.Now().Add(-time.Hour))
	_ = pool.SetModelAccessDeferral(true)
	first, _, _ := markStrike(t, pool, strikeMark(pool, ChannelPlayground, time.Now().Add(-time.Second), true, true))
	successAt := first.Until.Add(time.Millisecond)
	if _, cleared := verifyAt(t, pool, strikeModel, successAt); cleared != 1 {
		t.Fatalf("成功应清零第 1 档，cleared=%d", cleared)
	}
	if _, exists := strikeRecord(t, pool, ""); exists {
		t.Fatal("档位应已清零")
	}
	if access, _, cooling := diskAccess(t, path, strikeModel); cooling || !access.CheckedAt.Equal(successAt.UTC()) || pendingAccessCount(pool) != 0 {
		t.Fatalf("清档位的成功应立即写盘: %+v cooling=%t pending=%d", access, cooling, pendingAccessCount(pool))
	}

	// 另一通道的成功清共用的档位：这个作用域没有冷却、只差 CheckedAt，但有可清零的档位，同样不推迟
	fast, fastPath := deferralPool(t)
	future := time.Now().Add(time.Hour)
	verifyAt(t, fast, strikeBuildKey, future)
	_ = fast.SetModelAccessDeferral(true)
	markStrike(t, fast, strikeMark(fast, ChannelPlayground, time.Now(), true, true))
	fastAt := future.Add(time.Millisecond)
	if _, cleared := verifyAt(t, fast, strikeBuildKey, fastAt); cleared != 1 || pendingAccessCount(fast) != 0 {
		t.Fatalf("另一通道的成功应立即清零档位：cleared=%d pending=%d", cleared, pendingAccessCount(fast))
	}
	if access, _, _ := diskAccess(t, fastPath, strikeBuildKey); !access.CheckedAt.Equal(fastAt.UTC()) {
		t.Fatalf("应立即写盘: %+v", access)
	}
}

// TestDeferredModelAccessGlobalCooldown 账号全局 * 上短的分钟限额冷却：成功只推进 CheckedAt，可以推迟，冷却保留到自行到期；
// 写出时同样保留。长的全局冷却由成功清除，不推迟
func TestDeferredModelAccessGlobalCooldown(t *testing.T) {
	pool, path := deferralPool(t)
	base := time.Now().Add(-time.Minute)
	verifyAt(t, pool, strikeModel, base)
	_ = pool.SetModelAccessDeferral(true)
	markTestCooldown(t, pool, strikeAccount, "", base.Add(time.Second), time.Now().Add(50*time.Second))
	successAt := base.Add(2 * time.Second)
	verifyAt(t, pool, strikeModel, successAt)
	if pendingAccessCount(pool) == 0 || !pool.AccountCoolingDown(strikeAccount, "") {
		t.Fatal("短的全局冷却时应推迟、冷却保留")
	}
	if err := pool.FlushDeferredModelAccess(); err != nil {
		t.Fatal(err)
	}
	access, _, cooling := diskAccess(t, path, globalCooldownKey)
	if !cooling || !access.CheckedAt.Equal(successAt.UTC()) || !pool.AccountCoolingDown(strikeAccount, "") {
		t.Fatalf("写出后全局 CheckedAt=%s cooling=%t，期望推进到 %s 且保留冷却", access.CheckedAt, cooling, successAt.UTC())
	}

	long, longPath := deferralPool(t)
	verifyAt(t, long, strikeModel, base)
	_ = long.SetModelAccessDeferral(true)
	markTestCooldown(t, long, strikeAccount, "", base.Add(time.Second), time.Now().Add(2*time.Hour))
	verifyAt(t, long, strikeModel, base.Add(2*time.Second))
	if _, _, cooling := diskAccess(t, longPath, globalCooldownKey); cooling || long.AccountCoolingDown(strikeAccount, "") || pendingAccessCount(long) != 0 {
		t.Fatal("长的全局冷却应由成功立即清除")
	}
}

// TestDeferredModelAccessGenerationReset 重置模型资格（目录代际变化）后丢弃推迟的时间，不写回旧代际的资格
func TestDeferredModelAccessGenerationReset(t *testing.T) {
	pool, path := deferralPool(t)
	base := time.Now().Add(-time.Hour)
	verifyAt(t, pool, strikeModel, base)
	_ = pool.SetModelAccessDeferral(true)
	verifyAt(t, pool, strikeModel, base.Add(time.Second))
	if err := pool.ResetModelAccess(strikeAccount); err != nil {
		t.Fatal(err)
	}
	if err := pool.FlushDeferredModelAccess(); err != nil {
		t.Fatal(err)
	}
	if access, _, _ := diskAccess(t, path, strikeModel); access != (ModelAccess{}) || pendingAccessCount(pool) != 0 {
		t.Fatalf("重置后不应写回旧代际的资格: %+v pending=%d", access, pendingAccessCount(pool))
	}
}

// TestDeferredModelAccessTimerFlush 推迟的时间最多一个写盘间隔后由计时器写出
func TestDeferredModelAccessTimerFlush(t *testing.T) {
	pool, path := deferralPool(t)
	pool.accessFlushInterval = 50 * time.Millisecond
	base := time.Now().Add(-time.Hour)
	verifyAt(t, pool, strikeModel, base)
	_ = pool.SetModelAccessDeferral(true)
	successAt := base.Add(time.Second)
	verifyAt(t, pool, strikeModel, successAt)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if access, _, _ := diskAccess(t, path, strikeModel); access.CheckedAt.Equal(successAt.UTC()) && pendingAccessCount(pool) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("计时器没有写出推迟的时间")
		}
		time.Sleep(10 * time.Millisecond)
	}
	pool.mu.Lock()
	scheduled := pool.accessFlushScheduled
	pool.mu.Unlock()
	if scheduled {
		t.Fatal("写出后不应还挂着计时")
	}
}

// rewriteRuntime 模拟共用账户目录的另一个进程改写运行态文件
func rewriteRuntime(t *testing.T, path string, change func(*accountRuntimeState)) {
	t.Helper()
	state, err := readRuntime(path)
	if err != nil {
		t.Fatal(err)
	}
	change(&state)
	if err := writeRuntime(path, state); err != nil {
		t.Fatal(err)
	}
}

// TestDeferredModelAccessOtherProcess 共用账户目录的另一个进程在推迟期间写了运行态：并入按成功时的规则——
// 它写下的、租约更早的冷却与非已验证状态被这次成功覆盖（与成功当时立即写盘、它的写入随后判为过时的结果相同）；
// 它改了目录（目录代际加一、清掉了资格）时丢弃旧代际的推迟时间，不把已被清掉的资格写回
func TestDeferredModelAccessOtherProcess(t *testing.T) {
	pool, path := deferralPool(t)
	base := time.Now().Add(-time.Hour)
	verifyAt(t, pool, strikeModel, base)
	_ = pool.SetModelAccessDeferral(true)
	successAt := base.Add(10 * time.Second)
	verifyAt(t, pool, strikeModel, successAt)
	rewriteRuntime(t, path, func(state *accountRuntimeState) {
		state.ModelAccess[strikeModel] = ModelAccess{State: ModelAccessState("denied"), CheckedAt: base.Add(time.Second).UTC()}
		state.Cooldowns[strikeModel] = CooldownState{Until: time.Now().Add(time.Hour).UTC(), Reason: "另一个进程"}
	})
	if err := pool.FlushDeferredModelAccess(); err != nil {
		t.Fatal(err)
	}
	if access, _, cooling := diskAccess(t, path, strikeModel); cooling || access.State != ModelAccessVerified || !access.CheckedAt.Equal(successAt.UTC()) {
		t.Fatalf("租约更早的外部写入应被这次成功覆盖: %+v cooling=%t", access, cooling)
	}

	verifyAt(t, pool, strikeModel, successAt.Add(time.Second))
	if pendingAccessCount(pool) == 0 {
		t.Fatal("应推迟")
	}
	rewriteRuntime(t, path, func(state *accountRuntimeState) {
		state.CatalogFingerprint = "other-process-catalog"
		clear(state.ModelAccess)
	})
	if err := pool.FlushDeferredModelAccess(); err != nil {
		t.Fatal(err)
	}
	if access, _, _ := diskAccess(t, path, strikeModel); access != (ModelAccess{}) || pendingAccessCount(pool) != 0 {
		t.Fatalf("另一个进程改了目录后不应写回旧代际的资格: %+v pending=%d", access, pendingAccessCount(pool))
	}
}
