package aistudio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

const (
	verifyTestA     = "verify-a@example.com"
	verifyTestB     = "verify-b@example.com"
	verifyTestModel = "verify-model"
	verifyTestOther = "verify-other"
)

// verifyTestPool 返回两个账户、两个模型、两个通道都启用的账户池（Build 目录同样有这两个模型）
func verifyTestPool(t *testing.T, accountIDs ...string) *AccountPool {
	t.Helper()
	models := []Model{warmCoolingModel(verifyTestModel), warmCoolingModel(verifyTestOther)}
	accounts := make([]*Account, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		accounts = append(accounts, indexTestAccount(accountID, BenefitTierFree, models))
	}
	pool := NewAccountPool(accounts, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, accountID := range accountIDs {
		if err := pool.SetBuildCatalog(accountID, models); err != nil {
			t.Fatal(err)
		}
	}
	return pool
}

// markModelVerify 给账户在模型上写 30 天的单个模型需要验证冷却（两个键）
func markModelVerify(t *testing.T, pool *AccountPool, accountID string, modelID string) time.Time {
	t.Helper()
	until := time.Now().Add(30 * 24 * time.Hour)
	written, write, err := pool.MarkModelVerifyCooldownIfGeneration(accountID, modelID, pool.ModelAccessGeneration(accountID), time.Now(), until)
	if err != nil || written != 2 || write != CooldownWritten {
		t.Fatalf("写入单个模型需要验证冷却: written=%d write=%s err=%v", written, write, err)
	}
	return until
}

// TestModelVerifyReasonKind 两种需要验证账号的原因按逐字识别种类，单个模型的不以整户暂停的开头开头，都以“次返回 403 无权限，自动暂停”结尾
func TestModelVerifyReasonKind(t *testing.T) {
	if VerifyCooldownKind(VerifyRequiredPauseReason) != VerifyCooldownAccount || VerifyCooldownKind(ModelVerifyRequiredReason) != VerifyCooldownModel ||
		VerifyCooldownKind("无权限（模型）: x") != "" || VerifyCooldownKind("") != "" {
		t.Fatal("种类识别不对")
	}
	accountPrefix, _, _ := strings.Cut(VerifyRequiredPauseReason, "，")
	if strings.HasPrefix(ModelVerifyRequiredReason, accountPrefix) || !strings.HasSuffix(ModelVerifyRequiredReason, "次返回 403 无权限，自动暂停") {
		t.Fatalf("单个模型的原因应与整户暂停的开头不同、以 403 自动暂停的后缀结尾: %q", ModelVerifyRequiredReason)
	}
	if strings.Contains(ModelVerifyRequiredReason, "@") || strings.Contains(ModelVerifyRequiredReason, verifyTestModel) {
		t.Fatal("原因不应含邮箱或模型名")
	}
}

// TestMarkModelVerifyCooldownIfGeneration 一次写两个键（Playground 键按账户目录归一，Build 键 build:<模型>）；目录代际不符时都不写；
// 某个键已有不早于检查时间的记录时只写另一个；已有更晚的冷却时保留它
func TestMarkModelVerifyCooldownIfGeneration(t *testing.T) {
	pool := verifyTestPool(t, verifyTestA)
	now := time.Now()
	until := now.Add(30 * 24 * time.Hour)
	if written, write, err := pool.MarkModelVerifyCooldownIfGeneration(verifyTestA, verifyTestModel, pool.ModelAccessGeneration(verifyTestA)+1, now, until); err != nil ||
		written != 0 || write != CooldownSkippedGeneration {
		t.Fatalf("代际不符应都不写: written=%d write=%s err=%v", written, write, err)
	}
	// Build 键已有更晚的检查时间（取得租约更晚的请求已写回）
	if _, err := pool.MarkModelAccessVerifiedIfGeneration(verifyTestA, ChannelCooldownScope(ChannelBuild, verifyTestModel), pool.ModelAccessGeneration(verifyTestA), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	written, write, err := pool.MarkModelVerifyCooldownIfGeneration(verifyTestA, "models/"+verifyTestModel, pool.ModelAccessGeneration(verifyTestA), now, until)
	if err != nil || written != 1 || write != CooldownSkippedStale {
		t.Fatalf("应只写 Playground 键: written=%d write=%s err=%v", written, write, err)
	}
	status, _ := pool.StatusOf(verifyTestA)
	if cooldown := status.Cooldowns[verifyTestModel]; cooldown.Reason != ModelVerifyRequiredReason || !cooldown.Until.Equal(until.UTC()) {
		t.Fatalf("Playground 键 = %+v", cooldown)
	}
	if _, ok := status.Cooldowns[ChannelCooldownScope(ChannelBuild, verifyTestModel)]; ok {
		t.Fatal("陈旧的 Build 键不应写入")
	}
	// 已有更晚的冷却：保留
	later := now.Add(40 * 24 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(verifyTestA, verifyTestOther, pool.ModelAccessGeneration(verifyTestA), now.Add(-time.Minute), later, "其他长冷却"); err != nil {
		t.Fatal(err)
	}
	if written, _, err := pool.MarkModelVerifyCooldownIfGeneration(verifyTestA, verifyTestOther, pool.ModelAccessGeneration(verifyTestA), now, until); err != nil || written != 2 {
		t.Fatalf("应推进两个键: written=%d err=%v", written, err)
	}
	status, _ = pool.StatusOf(verifyTestA)
	if cooldown := status.Cooldowns[verifyTestOther]; !cooldown.Until.Equal(later.UTC()) || cooldown.Reason != "其他长冷却" {
		t.Fatalf("更晚的冷却应保留: %+v", cooldown)
	}
}

// TestModelVerifyNotDeadOrCooling 单个模型需要验证的冷却不算死槽位、不算淘汰选号的冷却中、不算账户冷却状态与热门冷却模型；
// 该模型的候选集照常不选它、按冷却记最早恢复，其他模型的候选照常；对照：同样两个键换成每日限额时都算
func TestModelVerifyNotDeadOrCooling(t *testing.T) {
	for _, test := range []struct {
		name   string
		verify bool
	}{{name: "单个模型需要验证", verify: true}, {name: "对照：每日限额"}} {
		pool := verifyTestPool(t, verifyTestA, verifyTestB)
		if test.verify {
			markModelVerify(t, pool, verifyTestA, verifyTestModel)
		} else {
			until := time.Now().Add(30 * 24 * time.Hour)
			for _, key := range []string{verifyTestModel, ChannelCooldownScope(ChannelBuild, verifyTestModel)} {
				if err := pool.MarkCooldownIfGeneration(verifyTestA, key, pool.ModelAccessGeneration(verifyTestA), time.Now(), until, "每日限额: 测试"); err != nil {
					t.Fatal(err)
				}
			}
		}
		counted := !test.verify
		if got := pool.AccountLongCooling(verifyTestA, verifyTestModel, 15*time.Minute); got != counted {
			t.Fatalf("%s：AccountLongCooling = %t", test.name, got)
		}
		if _, got := pool.LongCoolingSet([]string{verifyTestA}, []string{verifyTestModel}, 15*time.Minute, false)[verifyTestA]; got != counted {
			t.Fatalf("%s：LongCoolingSet = %t", test.name, got)
		}
		if got := pool.LongCooldownModels([]string{verifyTestA}, 15*time.Minute)[verifyTestModel] > 0; got != counted {
			t.Fatalf("%s：LongCooldownModels 计入 = %t", test.name, got)
		}
		state := pool.WarmWorkerStates([]string{verifyTestA}, verifyTestModel, 15*time.Minute)[verifyTestA]
		if state.Dead != counted || state.Cooling != counted || state.Unschedulable != "" {
			t.Fatalf("%s：WarmWorkerStates = %+v", test.name, state)
		}
		status, _ := pool.StatusOf(verifyTestA)
		if wantState := map[bool]AccountState{true: AccountReady, false: AccountCooldown}[test.verify]; status.State != wantState {
			t.Fatalf("%s：账户状态 = %s，期望 %s", test.name, status.State, wantState)
		}
		if counts := pool.StateCounts(); (counts.Cooldown > 0) != counted {
			t.Fatalf("%s：状态计数 = %+v", test.name, counts)
		}
		groups, err := pool.ClassifyCandidates(context.Background(), AccountSelection{ModelID: verifyTestModel, Method: "generateContent"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, accountID := range append(groups.StandbyReady, groups.StandbyBusy...) {
			if accountID == verifyTestA {
				t.Fatalf("%s：该模型的候选不应包含 A: %+v", test.name, groups)
			}
		}
		if groups.EarliestCooldown.IsZero() {
			t.Fatalf("%s：A 应按冷却记最早恢复", test.name)
		}
		other, err := pool.ClassifyCandidates(context.Background(), AccountSelection{ModelID: verifyTestOther, Method: "generateContent"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(other.StandbyReady) != 2 {
			t.Fatalf("%s：其他模型的候选应照常: %+v", test.name, other)
		}
	}
}

// TestVerifyCooldownNotReplacedByShorterCooldown 需要验证账号的冷却（单个模型或整户）不被更短的其他冷却替换（阶梯 403、多模型连续 403
// 的 30 分钟自动暂停）：保留原因与期限，返回 CooldownKeptLater；更长的其他冷却与需要验证写需要验证照常写入
func TestVerifyCooldownNotReplacedByShorterCooldown(t *testing.T) {
	pool := verifyTestPool(t, verifyTestA)
	until := markModelVerify(t, pool, verifyTestA, verifyTestModel)
	write, err := pool.TryMarkCooldownIfGeneration(verifyTestA, verifyTestModel, pool.ModelAccessGeneration(verifyTestA), time.Now(), time.Now().Add(30*time.Minute), "无权限（模型）: 403")
	if err != nil || write != CooldownKeptLater {
		t.Fatalf("更短的阶梯冷却不应替换: write=%s err=%v", write, err)
	}
	status, _ := pool.StatusOf(verifyTestA)
	if cooldown := status.Cooldowns[verifyTestModel]; cooldown.Reason != ModelVerifyRequiredReason || !cooldown.Until.Equal(until.UTC()) {
		t.Fatalf("单个模型需要验证的冷却应保留: %+v", cooldown)
	}
	// 整户暂停不被 30 分钟的自动暂停缩短
	pause := time.Now().Add(30 * 24 * time.Hour)
	if err := pool.MarkCooldownIfGeneration(verifyTestA, "", pool.ModelAccessGeneration(verifyTestA), time.Now(), pause, VerifyRequiredPauseReason); err != nil {
		t.Fatal(err)
	}
	if write, err := pool.TryMarkCooldownIfGeneration(verifyTestA, "", pool.ModelAccessGeneration(verifyTestA), time.Now(), time.Now().Add(30*time.Minute), "10 分钟内 2 个模型连续 3 次返回 403 无权限，自动暂停"); err != nil ||
		write != CooldownKeptLater {
		t.Fatalf("整户暂停不应被缩短: write=%s err=%v", write, err)
	}
	if !pool.VerifyRequiredPaused(verifyTestA) {
		t.Fatal("整户暂停应保留")
	}
	// 更长的其他冷却照常写入
	longer := time.Now().Add(60 * 24 * time.Hour)
	if write, err := pool.TryMarkCooldownIfGeneration(verifyTestA, verifyTestModel, pool.ModelAccessGeneration(verifyTestA), time.Now(), longer, "其他长冷却"); err != nil || write != CooldownWritten {
		t.Fatalf("更长的冷却应写入: write=%s err=%v", write, err)
	}
}

// TestAllCoolingParked 候选全部只被需要验证账号的冷却挡住时 Parked（503、不带重试时间）；有会自行恢复的候选时不 Parked，
// Until 取其中最早的；排除列表、限定账户集合、固定账户与 AcquireFor、NoCandidateError 两个构造点同一口径
func TestAllCoolingParked(t *testing.T) {
	selection := AccountSelection{ModelID: verifyTestModel, Method: "generateContent"}
	t.Run("全部单个模型需要验证", func(t *testing.T) {
		pool := verifyTestPool(t, verifyTestA, verifyTestB)
		markModelVerify(t, pool, verifyTestA, verifyTestModel)
		markModelVerify(t, pool, verifyTestB, verifyTestModel)
		cooling := pool.AllCoolingErrorFor(selection, time.Now().Add(30*24*time.Hour))
		if !cooling.Parked || !cooling.RetryAt().IsZero() || cooling.HTTPStatus() != http.StatusServiceUnavailable || cooling.ErrorCode() != "model_unavailable" {
			t.Fatalf("应为 Parked: %+v", cooling)
		}
		groups, err := pool.ClassifyCandidates(context.Background(), selection, nil)
		if err != nil {
			t.Fatal(err)
		}
		var noCandidate *AllCoolingError
		if err := pool.NoCandidateError(selection, groups); !errors.As(err, &noCandidate) || !noCandidate.Parked {
			t.Fatalf("NoCandidateError 应为 Parked: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = pool.AcquireFor(ctx, selection)
		var acquired *AllCoolingError
		if !errors.As(err, &acquired) || !acquired.Parked {
			t.Fatalf("AcquireFor 应立即返回 Parked 的全部冷却: %v", err)
		}
		if _, err := pool.AcquireFor(ctx, AccountSelection{AccountID: verifyTestA, ModelID: verifyTestModel, Method: "generateContent"}); !errors.As(err, &acquired) || !acquired.Parked {
			t.Fatalf("固定账户同样 Parked: %v", err)
		}
		// 其他模型照常
		lease, err := pool.AcquireFor(ctx, AccountSelection{ModelID: verifyTestOther, Method: "generateContent"})
		if err != nil {
			t.Fatalf("其他模型应能取得租约: %v", err)
		}
		_ = lease.Release()
	})
	t.Run("整户暂停与单个模型混合", func(t *testing.T) {
		pool := verifyTestPool(t, verifyTestA, verifyTestB)
		markModelVerify(t, pool, verifyTestA, verifyTestModel)
		if err := pool.MarkCooldownIfGeneration(verifyTestB, "", pool.ModelAccessGeneration(verifyTestB), time.Now(), time.Now().Add(30*24*time.Hour), VerifyRequiredPauseReason); err != nil {
			t.Fatal(err)
		}
		if cooling := pool.AllCoolingErrorFor(selection, time.Now().Add(30*24*time.Hour)); !cooling.Parked {
			t.Fatalf("整户暂停同样算: %+v", cooling)
		}
	})
	t.Run("有会自行恢复的候选", func(t *testing.T) {
		pool := verifyTestPool(t, verifyTestA, verifyTestB)
		markModelVerify(t, pool, verifyTestA, verifyTestModel)
		daily := time.Now().Add(time.Hour)
		for _, key := range []string{verifyTestModel, ChannelCooldownScope(ChannelBuild, verifyTestModel)} {
			if err := pool.MarkCooldownIfGeneration(verifyTestB, key, pool.ModelAccessGeneration(verifyTestB), time.Now(), daily, "每日限额: 测试"); err != nil {
				t.Fatal(err)
			}
		}
		cooling := pool.AllCoolingErrorFor(selection, time.Now().Add(30*24*time.Hour))
		if cooling.Parked || !cooling.Until.Equal(daily.UTC()) || cooling.HTTPStatus() != http.StatusTooManyRequests {
			t.Fatalf("有每日限额的候选时不 Parked、Until 取每日限额: %+v", cooling)
		}
		// 排除每日限额的候选：只剩需要验证的
		excluded := selection
		excluded.ExcludedAccountIDs = []string{verifyTestB}
		if cooling := pool.AllCoolingErrorFor(excluded, daily); !cooling.Parked {
			t.Fatalf("排除 B 后应 Parked: %+v", cooling)
		}
		allowed := selection
		allowed.AllowedAccountIDs = []string{verifyTestA}
		if cooling := pool.AllCoolingErrorFor(allowed, daily); !cooling.Parked {
			t.Fatalf("限定 A 时应 Parked: %+v", cooling)
		}
		allowed.AllowedAccountIDs = []string{verifyTestB}
		if cooling := pool.AllCoolingErrorFor(allowed, daily); cooling.Parked {
			t.Fatalf("限定 B 时不应 Parked: %+v", cooling)
		}
	})
	t.Run("发出前被需要验证账号的冷却拦下", func(t *testing.T) {
		blocked := &AccountCoolingDownError{AccountID: verifyTestA, ModelID: verifyTestModel, Until: time.Now().Add(time.Hour), Reason: ModelVerifyRequiredReason}
		if !blocked.VerifyParked() || !blocked.RetryAt().IsZero() || blocked.HTTPStatus() != http.StatusServiceUnavailable || blocked.ErrorCode() != "model_unavailable" {
			t.Fatalf("需要验证账号的冷却应按 503、不带重试时间: %+v", blocked)
		}
		quota := &AccountCoolingDownError{Until: time.Now().Add(time.Hour), Reason: "每日限额: x"}
		if quota.VerifyParked() || quota.HTTPStatus() != http.StatusTooManyRequests || quota.RetryAt().IsZero() {
			t.Fatal("每日限额照旧 429")
		}
	})
}

// parkedScaleTestPool 返回 n 个账户都在 verifyTestModel 两个通道上冷却的账户池：每 verifyEvery 个（从第 0 个起）为单个模型需要验证（30 天），
// 其余为每日限额（1–50 小时，第 1 个最早）；verifyEvery 为 1 时全部需要验证。冷却直接写进运行态后再建池（几千个账户逐个写太慢）
func parkedScaleTestPool(t testing.TB, n int, verifyEvery int) (*AccountPool, AccountSelection) {
	t.Helper()
	models := []Model{warmCoolingModel(verifyTestModel), warmCoolingModel(verifyTestOther)}
	accounts := make([]*Account, 0, n)
	now := time.Now()
	for position := 0; position < n; position++ {
		account := indexTestAccount(fmt.Sprintf("scale-%05d@example.com", position), BenefitTierFree, models)
		until, reason := now.Add(time.Duration(position%50+1)*time.Hour), "每日限额: 测试"
		if position%verifyEvery == 0 {
			until, reason = now.Add(30*24*time.Hour+time.Duration(position)*time.Second), ModelVerifyRequiredReason
		}
		for _, key := range []string{verifyTestModel, ChannelCooldownScope(ChannelBuild, verifyTestModel)} {
			account.runtime.Cooldowns[key] = CooldownState{Until: until, Reason: reason}
		}
		accounts = append(accounts, account)
	}
	pool := NewAccountPool(accounts, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	for _, account := range accounts {
		if err := pool.SetBuildCatalog(account.ID, models); err != nil {
			t.Fatal(err)
		}
	}
	return pool, AccountSelection{ModelID: verifyTestModel, Method: "generateContent"}
}

// TestAllCoolingErrorForShortCircuit 候选全部冷却时，最早恢复的候选被会自行恢复的冷却挡住（号池用完每日额度这类常见情况）就直接返回，
// 不在 p.mu 内逐个核对全部冷却中的候选：每次调用的耗时不随账户数增长（原先 3820 个账户约 2.3 ms，是选号的 10 倍）；
// 只有最早恢复的就是需要验证账号的冷却时才逐个核对，逐个核对不按账户分配内存。结果与逐个核对相同
func TestAllCoolingErrorForShortCircuit(t *testing.T) {
	t.Run("结果", func(t *testing.T) {
		pool, selection := parkedScaleTestPool(t, 300, 3)
		groups, err := pool.ClassifyCandidates(context.Background(), selection, nil)
		if err != nil || groups.EarliestCooldown.IsZero() {
			t.Fatalf("应全部冷却: %+v %v", groups, err)
		}
		if groups.EarliestCooldown.After(time.Now().Add(time.Hour)) {
			t.Fatalf("最早恢复应为 1 小时的每日限额: %v", groups.EarliestCooldown)
		}
		cooling := pool.AllCoolingErrorFor(selection, groups.EarliestCooldown)
		if cooling.Parked || !cooling.Until.Equal(groups.EarliestCooldown) {
			t.Fatalf("最早恢复的是每日限额：不 Parked、Until 为最早恢复: %+v earliest=%v", cooling, groups.EarliestCooldown)
		}
		parked, parkedSelection := parkedScaleTestPool(t, 300, 1)
		groups, err = parked.ClassifyCandidates(context.Background(), parkedSelection, nil)
		if err != nil {
			t.Fatal(err)
		}
		if cooling := parked.AllCoolingErrorFor(parkedSelection, groups.EarliestCooldown); !cooling.Parked {
			t.Fatalf("全部需要验证时 Parked: %+v", cooling)
		}
		// 排除最早恢复（1 小时）的每日限额候选后，下一个仍是每日限额（2 小时）
		excluded := selection
		excluded.ExcludedAccountIDs = []string{"scale-00050@example.com", "scale-00100@example.com", "scale-00200@example.com", "scale-00250@example.com"}
		groups, err = pool.ClassifyCandidates(context.Background(), excluded, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !groups.EarliestCooldown.After(time.Now().Add(time.Hour)) {
			t.Fatalf("排除后最早恢复应为 2 小时的每日限额: %v", groups.EarliestCooldown)
		}
		if cooling := pool.AllCoolingErrorFor(excluded, groups.EarliestCooldown); cooling.Parked || !cooling.Until.Equal(groups.EarliestCooldown) {
			t.Fatalf("排除后取下一个每日限额: %+v earliest=%v", cooling, groups.EarliestCooldown)
		}
	})
	t.Run("耗时不随账户数增长", func(t *testing.T) {
		small, smallSelection := parkedScaleTestPool(t, 40, 3)
		large, largeSelection := parkedScaleTestPool(t, 4000, 3)
		measure := func(pool *AccountPool, selection AccountSelection) time.Duration {
			groups, err := pool.ClassifyCandidates(context.Background(), selection, nil)
			if err != nil {
				t.Fatal(err)
			}
			const calls = 200
			started := time.Now()
			for range calls {
				if cooling := pool.AllCoolingErrorFor(selection, groups.EarliestCooldown); cooling.Parked {
					t.Fatal("不应 Parked")
				}
			}
			return time.Since(started) / calls
		}
		// 交替测量、各取最小值，抵消并行测试与 GC 的干扰；短路时两者相当，逐个核对时大池是小池的约 100 倍
		smallBest, largeBest := time.Duration(1<<62), time.Duration(1<<62)
		for range 7 {
			smallBest = min(smallBest, measure(small, smallSelection))
			largeBest = min(largeBest, measure(large, largeSelection))
		}
		if largeBest > 10*smallBest+20*time.Microsecond {
			t.Fatalf("4000 个账户每次 %v，40 个账户每次 %v：最早恢复的是每日限额时不应逐个核对", largeBest, smallBest)
		}
	})
	t.Run("逐个核对不按账户分配", func(t *testing.T) {
		pool, selection := parkedScaleTestPool(t, 500, 1)
		groups, err := pool.ClassifyCandidates(context.Background(), selection, nil)
		if err != nil {
			t.Fatal(err)
		}
		allocations := testing.AllocsPerRun(20, func() {
			if cooling := pool.AllCoolingErrorFor(selection, groups.EarliestCooldown); !cooling.Parked {
				t.Fatal("应 Parked")
			}
		})
		if allocations > 8 {
			t.Fatalf("500 个需要验证的候选逐个核对分配了 %.0f 次，不应按账户分配", allocations)
		}
	})
}
