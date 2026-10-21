package aistudio

import (
	"maps"
	"testing"
	"time"
)

// runtimeSnapshot 返回账户内存运行态中冷却与模型资格的副本
func runtimeSnapshot(t *testing.T, pool *AccountPool, id string) (map[string]CooldownState, map[string]ModelAccess) {
	t.Helper()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	account := pool.byID[id]
	if account == nil {
		t.Fatalf("账户 %s 不存在", id)
	}
	return maps.Clone(account.runtime.Cooldowns), maps.Clone(account.runtime.ModelAccess)
}

// markTestCooldown 以 checkedAt 写入一条冷却，必须真正写入
func markTestCooldown(t *testing.T, pool *AccountPool, id string, scope string, checkedAt time.Time, until time.Time) {
	t.Helper()
	write, err := pool.TryMarkCooldownIfGeneration(id, scope, pool.ModelAccessGeneration(id), checkedAt, until, MinuteQuotaKind+": 测试")
	if err != nil || write != CooldownWritten {
		t.Fatalf("写入冷却 %q: write=%s err=%v", scope, write, err)
	}
}

// TestSuccessKeepsShortGlobalCooldown 账号全局的短冷却（分钟限额）不被同账号取得租约更晚的请求的成功清除，只推进 CheckedAt；
// 之后取得租约早于这次成功的 429 不再写回，更晚的照常写入
func TestSuccessKeepsShortGlobalCooldown(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	generation := pool.ModelAccessGeneration(id)
	t1 := time.Now().Add(-time.Minute)
	markTestCooldown(t, pool, id, "", t1, time.Now().Add(50*time.Second))

	t2 := t1.Add(2 * time.Second)
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(id, "flash", generation, t2); err != nil {
		t.Fatal(err)
	}
	if !pool.AccountCoolingDown(id, "") {
		t.Fatal("短的全局冷却被成功提前清除")
	}
	cooldowns, access := runtimeSnapshot(t, pool, id)
	if _, kept := cooldowns[globalCooldownKey]; !kept || !access[globalCooldownKey].CheckedAt.Equal(t2.UTC()) {
		t.Fatalf("全局冷却=%v CheckedAt=%s，期望保留冷却并推进到 %s", cooldowns, access[globalCooldownKey].CheckedAt, t2.UTC())
	}
	if access["flash"].State != ModelAccessVerified {
		t.Fatalf("模型资格=%+v，期望已验证", access["flash"])
	}
	// 再次以同一时刻成功：快路径判定没有变化
	if changed, err := pool.MarkModelAccessVerifiedIfGeneration(id, "flash", generation, t2); err != nil || changed {
		t.Fatalf("重复成功 changed=%t err=%v", changed, err)
	}

	write, err := pool.TryMarkCooldownIfGeneration(id, "", generation, t1.Add(time.Second), time.Now().Add(40*time.Second), "旧租约")
	if err != nil || write != CooldownSkippedStale {
		t.Fatalf("取得租约早于成功的 429：write=%s err=%v，期望 %s", write, err, CooldownSkippedStale)
	}
	write, err = pool.TryMarkCooldownIfGeneration(id, "", generation, t2.Add(time.Second), time.Now().Add(40*time.Second), "新租约")
	if err != nil || write != CooldownWritten {
		t.Fatalf("取得租约晚于成功的 429：write=%s err=%v，期望 %s", write, err, CooldownWritten)
	}
}

// TestSuccessClearsLongGlobalCooldown 剩余不少于 ShortCooldownMax 的全局冷却照旧被成功清除（每日限额被成功纠正）
func TestSuccessClearsLongGlobalCooldown(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	t1 := time.Now().Add(-time.Minute)
	markTestCooldown(t, pool, id, "", t1, time.Now().Add(2*time.Hour))
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(id, "flash", pool.ModelAccessGeneration(id), t1.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if pool.AccountCoolingDown(id, "") {
		t.Fatal("长的全局冷却应被成功清除")
	}
}

// TestClearCooldownKeepsShortGlobal ClearCooldownIfGeneration 对短的全局冷却只推进 CheckedAt，模型自己的冷却照常清除；
// 长的全局冷却两种调用都清除；已过期的条目被清除
func TestClearCooldownKeepsShortGlobal(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	generation := pool.ModelAccessGeneration(id)
	t1 := time.Now().Add(-time.Minute)
	markTestCooldown(t, pool, id, "", t1, time.Now().Add(50*time.Second))
	markTestCooldown(t, pool, id, "flash", t1, time.Now().Add(2*time.Hour))

	t2 := t1.Add(time.Second)
	if err := pool.ClearCooldownIfGeneration(id, "", generation, t2); err != nil {
		t.Fatal(err)
	}
	cooldowns, access := runtimeSnapshot(t, pool, id)
	if _, kept := cooldowns[globalCooldownKey]; !kept || !access[globalCooldownKey].CheckedAt.Equal(t2.UTC()) {
		t.Fatalf("Clear(\"\") 后全局冷却=%v CheckedAt=%s，期望保留并推进到 %s", cooldowns, access[globalCooldownKey].CheckedAt, t2.UTC())
	}
	t3 := t2.Add(time.Second)
	if err := pool.ClearCooldownIfGeneration(id, "flash", generation, t3); err != nil {
		t.Fatal(err)
	}
	cooldowns, access = runtimeSnapshot(t, pool, id)
	if _, cooling := cooldowns["flash"]; cooling {
		t.Fatalf("模型冷却应被清除：%v", cooldowns)
	}
	if _, kept := cooldowns[globalCooldownKey]; !kept || !access[globalCooldownKey].CheckedAt.Equal(t3.UTC()) ||
		!access["flash"].CheckedAt.Equal(t3.UTC()) {
		t.Fatalf("Clear(flash) 后冷却=%v 资格=%v，期望全局保留、两个 CheckedAt 推进到 %s", cooldowns, access, t3.UTC())
	}
	// 同一时刻再清一次：没有可改的内容
	if err := pool.ClearCooldownIfGeneration(id, "flash", generation, t3); err != nil {
		t.Fatal(err)
	}
	if !pool.AccountCoolingDown(id, "") {
		t.Fatal("短的全局冷却被清除")
	}

	for _, scope := range []string{"", "flash"} {
		long := testPoolWithAccount(t, id)
		markTestCooldown(t, long, id, "", t1, time.Now().Add(2*time.Hour))
		if err := long.ClearCooldownIfGeneration(id, scope, long.ModelAccessGeneration(id), t2); err != nil {
			t.Fatal(err)
		}
		if long.AccountCoolingDown(id, "") {
			t.Fatalf("Clear(%q) 应清除长的全局冷却", scope)
		}
	}

	expired := testPoolWithAccount(t, id)
	markTestCooldown(t, expired, id, "", t1, time.Now().Add(30*time.Millisecond))
	time.Sleep(60 * time.Millisecond)
	if err := expired.ClearCooldownIfGeneration(id, "", expired.ModelAccessGeneration(id), t2); err != nil {
		t.Fatal(err)
	}
	if cooldowns, _ := runtimeSnapshot(t, expired, id); len(cooldowns) != 0 {
		t.Fatalf("已过期的冷却条目应被清除：%v", cooldowns)
	}
}

// TestShortCooldownBoundary 剩余不到 ShortCooldownMax 的全局冷却保留，超过的被清除（成功写回与清除冷却两条路径）
func TestShortCooldownBoundary(t *testing.T) {
	const id = "alice@example.com"
	for _, test := range []struct {
		remaining time.Duration
		kept      bool
	}{
		{ShortCooldownMax - time.Second, true},
		{ShortCooldownMax + time.Second, false},
	} {
		for _, path := range []string{"成功写回", "清除冷却"} {
			pool := testPoolWithAccount(t, id)
			generation := pool.ModelAccessGeneration(id)
			t1 := time.Now().Add(-time.Minute)
			markTestCooldown(t, pool, id, "", t1, time.Now().Add(test.remaining))
			var err error
			if path == "成功写回" {
				_, err = pool.MarkModelAccessVerifiedIfGeneration(id, "flash", generation, t1.Add(time.Second))
			} else {
				err = pool.ClearCooldownIfGeneration(id, "flash", generation, t1.Add(time.Second))
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := pool.AccountCoolingDown(id, ""); got != test.kept {
				t.Fatalf("%s 剩余 %s：仍冷却=%t，期望 %t", path, test.remaining, got, test.kept)
			}
		}
	}
	minute := MinuteQuotaKind + ": x"
	if !shortCooldownActive(CooldownState{Until: time.Now().Add(time.Minute), Reason: minute}, time.Now()) ||
		!shortCooldownActive(CooldownState{Until: time.Now().Add(time.Minute), Reason: RateLimitKind + ": x"}, time.Now()) ||
		shortCooldownActive(CooldownState{Until: time.Now().Add(time.Minute), Reason: "上传转录文件: x"}, time.Now()) ||
		shortCooldownActive(CooldownState{Until: time.Now().Add(-time.Second), Reason: minute}, time.Now()) ||
		shortCooldownActive(CooldownState{}, time.Now()) {
		t.Fatal("shortCooldownActive 判断错误")
	}
}

// TestSuccessClearsShortNonQuotaGlobalCooldown 账号全局 * 上剩余不到 ShortCooldownMax、但不是分钟限额或限流的冷却（转录上传失败等
// 暂时故障写的 30 秒冷却）照旧被取得租约更晚的成功清除：成功写回、清除冷却与清除模型冷却三条路径都清除；分钟限额与限流的短冷却仍保留
func TestSuccessClearsShortNonQuotaGlobalCooldown(t *testing.T) {
	const id = "alice@example.com"
	for _, test := range []struct {
		reason string
		kept   bool
	}{
		{"上传转录文件: read tcp 127.0.0.1:1->127.0.0.1:2: connection reset by peer", false},
		{"AI Studio 上传返回 HTTP 503", false},
		{MinuteQuotaKind + ": 测试", true},
		{RateLimitKind + ": 测试", true},
	} {
		for _, path := range []string{"成功写回", "清除冷却", "清除模型冷却"} {
			pool := testPoolWithAccount(t, id)
			generation := pool.ModelAccessGeneration(id)
			t0 := time.Now().Add(-2 * time.Second)
			write, err := pool.TryMarkCooldownIfGeneration(id, "", generation, t0, time.Now().Add(30*time.Second), test.reason)
			if err != nil || write != CooldownWritten {
				t.Fatalf("写入冷却 %q: write=%s err=%v", test.reason, write, err)
			}
			g0 := t0.Add(time.Second)
			switch path {
			case "成功写回":
				_, err = pool.MarkModelAccessVerifiedIfGeneration(id, "flash", generation, g0)
			case "清除冷却":
				err = pool.ClearCooldownIfGeneration(id, "", generation, g0)
			default:
				err = pool.ClearCooldownIfGeneration(id, "flash", generation, g0)
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := pool.AccountCoolingDown(id, ""); got != test.kept {
				t.Fatalf("%s 原因 %q：仍冷却=%t，期望 %t", path, test.reason, got, test.kept)
			}
			_, access := runtimeSnapshot(t, pool, id)
			if !access[globalCooldownKey].CheckedAt.Equal(g0.UTC()) {
				t.Fatalf("%s 原因 %q：全局 CheckedAt=%s，期望推进到 %s", path, test.reason, access[globalCooldownKey].CheckedAt, g0.UTC())
			}
		}
	}
}

// TestTryMarkCooldownResult TryMarkCooldownIfGeneration 如实返回写入、记录过旧跳过与目录代际变化跳过；恢复时间已过时返回错误
func TestTryMarkCooldownResult(t *testing.T) {
	const id = "alice@example.com"
	pool := testPoolWithAccount(t, id)
	generation := pool.ModelAccessGeneration(id)
	t1 := time.Now().Add(-time.Minute)
	until := time.Now().Add(time.Minute)
	if write, err := pool.TryMarkCooldownIfGeneration(id, "", generation, t1, until, "r"); err != nil || write != CooldownWritten {
		t.Fatalf("首次写入 write=%s err=%v", write, err)
	}
	if write, err := pool.TryMarkCooldownIfGeneration(id, "", generation, t1, until, "r"); err != nil || write != CooldownSkippedStale {
		t.Fatalf("同一时刻再写 write=%s err=%v，期望 %s", write, err, CooldownSkippedStale)
	}
	if err := pool.ResetModelAccess(id); err != nil {
		t.Fatal(err)
	}
	if write, err := pool.TryMarkCooldownIfGeneration(id, "flash", generation, t1.Add(time.Second), until, "r"); err != nil || write != CooldownSkippedGeneration {
		t.Fatalf("代际变化后 write=%s err=%v，期望 %s", write, err, CooldownSkippedGeneration)
	}
	if write, err := pool.TryMarkCooldownIfGeneration(id, "", pool.ModelAccessGeneration(id), t1.Add(2*time.Second), time.Now().Add(-time.Second), "r"); err == nil || write != 0 {
		t.Fatalf("恢复时间已过 write=%s err=%v，期望报错", write, err)
	}
	// 原接口仍只返回错误
	if err := pool.MarkCooldownIfGeneration(id, "", pool.ModelAccessGeneration(id), t1.Add(3*time.Second), until, "r"); err != nil {
		t.Fatal(err)
	}
	for write, want := range map[CooldownWrite]string{
		CooldownWritten:           "已写入",
		CooldownSkippedGeneration: "模型目录已更新，旧租约的结果不再写回",
		CooldownSkippedStale:      "同账号取得租约更晚的请求已写回结果",
	} {
		if write.String() != want {
			t.Fatalf("%d.String()=%q，期望 %q", write, write.String(), want)
		}
	}
}

// TestWarmWorkerStatesShortCooling 淘汰选号不把剩余不到 ShortCooldownMax 的冷却当作冷却中；Dead 仍按 minRemaining 判断
func TestWarmWorkerStatesShortCooling(t *testing.T) {
	const id = "alice@example.com"
	for _, test := range []struct {
		name      string
		scope     string
		modelID   string
		remaining time.Duration
		cooling   bool
		dead      bool
		checkDead bool
	}{
		{name: "全局 30 秒", remaining: 30 * time.Second, cooling: false, dead: false, checkDead: true},
		{name: "全局 3 分钟", remaining: 3 * time.Minute, cooling: true, dead: false, checkDead: true},
		{name: "全局 2 小时", remaining: 2 * time.Hour, cooling: true, dead: true, checkDead: true},
		{name: "模型 30 秒", scope: "flash", modelID: "flash", remaining: 30 * time.Second, cooling: false},
		{name: "全局 2 分钟加 1 秒", remaining: ShortCooldownMax + time.Second, cooling: true},
		{name: "全局 2 分钟减 1 秒", remaining: ShortCooldownMax - time.Second, cooling: false},
	} {
		pool := testPoolWithAccount(t, id)
		markTestCooldown(t, pool, id, test.scope, time.Now().Add(-time.Minute), time.Now().Add(test.remaining))
		state, ok := pool.WarmWorkerStates([]string{id}, test.modelID, 15*time.Minute)[id]
		if !ok {
			t.Fatalf("%s：没有返回状态", test.name)
		}
		if state.Cooling != test.cooling || test.checkDead && state.Dead != test.dead {
			t.Fatalf("%s：Cooling=%t Dead=%t，期望 Cooling=%t Dead=%t", test.name, state.Cooling, state.Dead, test.cooling, test.dead)
		}
	}
}
