package app

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// seedBalancedUsage 把今日发送数并回账户池（只能调大），并作废管理器的 1 秒缓存副本，让下一次排序读到新计数
func seedBalancedUsage(t *testing.T, manager *accountWorkerManager, counts map[string]uint32) {
	t.Helper()
	if !manager.pool.SeedQuotaDayUsage(aistudio.QuotaDayKey(time.Now()), counts) {
		t.Fatal("并回今日发送数失败（测试恰好跨过太平洋时间零点？）")
	}
	state := &manager.balancedState
	state.usageMu.Lock()
	state.usage, state.usageAt = aistudio.QuotaUsageSnapshot{}, time.Time{}
	state.usageMu.Unlock()
}

// TestLeastUsedFirstMatchesFullSort 部分选择与“全量稳定排序取前 limit 个、其余保持原顺序”的参考实现在随机输入上一致，不重复、不丢
func TestLeastUsedFirstMatchesFullSort(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("随机种子 %d", seed)
	random := rand.New(rand.NewSource(seed))
	for round := 0; round < 200; round++ {
		size, limit := random.Intn(300), 1+random.Intn(70)
		accountIDs := make([]string, size)
		counts := make(map[string]uint32, size)
		for index := range accountIDs {
			accountIDs[index] = fmt.Sprintf("least-%04d@example.com", index)
			if random.Intn(3) != 0 {
				counts[accountIDs[index]] = uint32(random.Intn(8))
			}
		}
		random.Shuffle(len(accountIDs), func(i, j int) { accountIDs[i], accountIDs[j] = accountIDs[j], accountIDs[i] })
		sorted := sortByUsage(accountIDs, counts)
		want := slices.Clone(sorted[:min(limit, len(sorted))])
		for _, accountID := range accountIDs {
			if !slices.Contains(want[:min(limit, len(sorted))], accountID) {
				want = append(want, accountID)
			}
		}
		if got := leastUsedFirst(accountIDs, counts, limit); !slices.Equal(got, want) {
			t.Fatalf("第 %d 轮（%d 个，前 %d）：\n得到 %v\n期望 %v", round, size, limit, got, want)
		}
	}
}

// TestBalancedColdCandidatesOrder 预热与替补的冷启动候选（rankColdCandidates）：平均调用开启时最终顺序为能服务的主用模型数↓、热门模型数↓、
// 今日发送↑、近期成功优先、原顺序，2 小时内启动失败过的号整组排在最后；关闭时与原来一致（近期成功整组在前，今日发送与失败记录不参与）。
// 替补（pickReplacement）按同一顺序挑号
func TestBalancedColdCandidatesOrder(t *testing.T) {
	const (
		heavy     = "cold-heavy@example.com"
		light     = "cold-light@example.com"
		tied      = "cold-tied@example.com"
		forbidden = "cold-forbidden@example.com"
		mid       = "cold-mid@example.com"
	)
	accounts := []string{heavy, light, tied, forbidden, mid}
	env := newDrainEnv(t, accounts, 1, 5)
	memory := openBadAccountMemory("", env.requests)
	env.manager.badAccounts.Store(memory)
	memory.observe(heavy, "", egressSuccess)
	memory.observe(tied, "", egressSuccess)
	forbidPrimary(t, env, forbidden)
	seedBalancedUsage(t, env.manager, map[string]uint32{heavy: 90, light: 5, tied: 5, mid: 40})
	models := []string{testRuntimeModel}
	for _, test := range []struct {
		name        string
		balanced    bool
		want        []string
		replacement string
	}{
		// 关闭：近期成功的 heavy、tied 整组在前，其余保持原顺序，对主用模型冷却的排最后
		{name: "关闭", want: []string{heavy, tied, light, mid, forbidden}, replacement: heavy},
		// 开启：能服务主用模型的按今日发送从少到多；tied 与 light 同为 5 次，近期成功的 tied 在前；
		// forbidden 今天没发过，仍因服务不了主用模型排最后
		{name: "开启", balanced: true, want: []string{tied, light, mid, heavy, forbidden}, replacement: tied},
	} {
		env.manager.setBalanced(test.balanced)
		if got := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts); !slices.Equal(got, test.want) {
			t.Fatalf("%s：冷启动候选排序 = %v，期望 %v", test.name, got, test.want)
		}
		if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != test.replacement {
			t.Fatalf("%s：替补应选 %s，实际 %q", test.name, test.replacement, got)
		}
	}
	if !slices.Equal(accounts, []string{heavy, light, tied, forbidden, mid}) {
		t.Fatalf("排序不应改动调用方的切片: %v", accounts)
	}
	// 平均调用开启时，2 小时内启动失败过、之后没有成功的号整组排在最后（组内顺序不变）：预热失败记录的 5 分钟过后它不再被排除，
	// 替补照样不会先挑它；关闭时与原来一致
	setFailedAt := func(at time.Time, accountIDs ...string) {
		env.manager.warmFailMu.Lock()
		defer env.manager.warmFailMu.Unlock()
		for _, accountID := range accountIDs {
			env.manager.warmFailures[accountID] = at
		}
	}
	env.manager.noteWarmFailure(tied)
	env.manager.noteWarmFailure(forbidden)
	setFailedAt(time.Now().Add(-10*time.Minute), tied, forbidden)
	env.manager.setBalanced(true)
	if got, want := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts), []string{light, mid, heavy, tied, forbidden}; !slices.Equal(got, want) {
		t.Fatalf("启动失败过的号应排在最后：%v，期望 %v", got, want)
	}
	if got := env.manager.pickReplacement(env.ctx, aistudio.PoolScopeNormal, models, env.manager.WarmAccountIDs()); got != light {
		t.Fatalf("替补应跳过启动失败过的 tied、选 light，实际 %q", got)
	}
	env.manager.setBalanced(false)
	if got, want := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts), []string{heavy, tied, light, mid, forbidden}; !slices.Equal(got, want) {
		t.Fatalf("关闭时顺序应与原来一致：%v，期望 %v", got, want)
	}
	// 超过 2 小时：不再排后，记录随之清掉
	setFailedAt(time.Now().Add(-balancedColdFailHold-time.Minute), tied, forbidden)
	env.manager.setBalanced(true)
	if got, want := env.manager.rankColdCandidates(aistudio.PoolScopeNormal, accounts), []string{tied, light, mid, heavy, forbidden}; !slices.Equal(got, want) {
		t.Fatalf("失败超过 2 小时后应回到原来的位置：%v，期望 %v", got, want)
	}
	env.manager.warmFailMu.Lock()
	left := len(env.manager.warmFailures)
	env.manager.warmFailMu.Unlock()
	if left != 0 {
		t.Fatalf("超过保留时长的失败记录应清掉，还剩 %d 条", left)
	}
}

// TestBalancedStandbyPreferredLeastUsed 请求路径挑备用账号（standbyCapacity.preferred）：平均调用开启时 1300 个候选里排在末尾、
// 今日发送最少的 64 个被放到前段最前（按发送数从少到多），原列表的前 64 个也留在前段（同档按今日发送从少到多），其余保持原顺序，
// 不重复、不丢；关闭时前段仍是原列表的前 64 个
func TestBalancedStandbyPreferredLeastUsed(t *testing.T) {
	env := newDrainEnv(t, []string{"prefer-warm@example.com"}, 1, 2)
	const total = 1300
	list := make([]string, total)
	counts := make(map[string]uint32, total)
	for index := range list {
		list[index] = fmt.Sprintf("standby-%04d@example.com", index)
		if index < total-standbyPreferLimit {
			counts[list[index]] = uint32(100 + index%7)
		} else {
			// 末尾 64 个：越靠后发得越少，最后一个今天没发过
			counts[list[index]] = uint32(total - 1 - index)
		}
	}
	seedBalancedUsage(t, env.manager, counts)
	tail := slices.Clone(list[total-standbyPreferLimit:])
	slices.Reverse(tail)
	front := sortByUsage(list[:standbyPreferLimit], counts)

	env.manager.setBalanced(true)
	segments := env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel).preferred(list)
	if len(segments[0]) != 2*standbyPreferLimit || !slices.Equal(segments[0][:standbyPreferLimit], tail) {
		t.Fatalf("前段应先是今日发送最少的 64 个（从少到多）: %v", segments[0])
	}
	if !slices.Equal(segments[0][standbyPreferLimit:], front) {
		t.Fatalf("前段随后应是原列表的前 64 个（按今日发送从少到多）: %v", segments[0][standbyPreferLimit:])
	}
	if !slices.Equal(segments[1], list[standbyPreferLimit:total-standbyPreferLimit]) {
		t.Fatal("后段应为其余候选、保持原顺序")
	}
	all := append(slices.Clone(segments[0]), segments[1]...)
	slices.Sort(all)
	sorted := slices.Clone(list)
	slices.Sort(sorted)
	if !slices.Equal(all, sorted) {
		t.Fatal("两段合起来应与原候选一一对应，不重复、不丢")
	}

	env.manager.setBalanced(false)
	segments = env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel).preferred(list)
	if !slices.Equal(segments[0], list[:standbyPreferLimit]) || !slices.Equal(segments[1], list[standbyPreferLimit:]) {
		t.Fatal("关闭时前段应为原列表的前 64 个、后段为其余")
	}
}

// TestBalancedStandbyPreferredKeepsPrimaryCapable 平均调用开启时请求路径扩容仍优先启动能服务主用模型的号：能服务主用模型的 100 个号
// 接过主用请求、今日各 20 多次，服务不了的 100 个号今日 0 次。原先前段只看今日发送最少的 64 个，全是服务不了主用模型的号，
// 为其他模型扩容时启动的 Worker 接不了主用请求；现在原列表的前段也参与排序，先挑能服务主用模型的号里今日发送最少的
func TestBalancedStandbyPreferredKeepsPrimaryCapable(t *testing.T) {
	var capable, plain []string
	for index := 0; index < 100; index++ {
		capable = append(capable, fmt.Sprintf("cap-%03d@example.com", index))
		plain = append(plain, fmt.Sprintf("plain-%03d@example.com", index))
	}
	resident := "aaa-resident@example.com"
	env := newDrainEnv(t, append(append([]string{resident}, capable...), plain...), 1, 4)
	other := []aistudio.Model{{ID: testRuntimeModel, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}}
	for _, accountID := range plain {
		if err := env.pool.SetCatalog(accountID, aistudio.BenefitTierFree, other); err != nil {
			t.Fatal(err)
		}
	}
	counts := map[string]uint32{}
	for index, accountID := range capable {
		counts[accountID] = uint32(20 + (index+3)%5)
	}
	seedBalancedUsage(t, env.manager, counts)
	list := append(slices.Clone(capable), plain...)
	pick := func() (string, int) {
		capacity := env.manager.standbyCapacity(env.manager.WarmAccountIDs(), testRuntimeModel)
		head := capacity.preferred(list)[0]
		capableInHead := 0
		for _, accountID := range head {
			if strings.HasPrefix(accountID, "cap-") {
				capableInHead++
			}
		}
		return capacity.expandable(list), capableInHead
	}
	if got, _ := pick(); got != capable[0] {
		t.Fatalf("关闭时扩容应选能服务主用模型的 %s，实际 %q", capable[0], got)
	}
	env.manager.setBalanced(true)
	// 前 64 个能服务主用模型的号里今日发送最少的是 cap-002（20 次）
	got, capableInHead := pick()
	if got != capable[2] || capableInHead != standbyPreferLimit {
		t.Fatalf("开启时扩容应选能服务主用模型、今日发送最少的 %s（前段里能服务的 %d 个），实际 %q", capable[2], capableInHead, got)
	}
}

// TestBalancedIdleWarmFirst 驻留 Worker 之间空闲的优先：3 个热 Worker 依次各取一个租约后释放后两个，A 上还有 1 个请求、B、C 空闲、
// 上次选中 C。轮询只按 ID 轮转，下一次仍选 A（叠到已有请求的号上）；平均调用先在空闲的热 Worker 里选，选 B。固定账户的选号不受影响
func TestBalancedIdleWarmFirst(t *testing.T) {
	const (
		accountA = "idle-a@example.com"
		accountB = "idle-b@example.com"
		accountC = "idle-c@example.com"
	)
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	for _, test := range []struct {
		name     string
		balanced bool
		want     string
	}{
		{name: "轮询", want: accountA},
		{name: "平均调用", balanced: true, want: accountB},
	} {
		env := newDrainEnv(t, []string{accountA, accountB, accountC}, 3, 3)
		env.promote(t, accountA, accountB, accountC)
		env.manager.setBalanced(test.balanced)
		var held []*aistudio.AccountLease
		for range 3 {
			lease, err := env.service.acquireWarmLease(env.ctx, selection)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, lease)
		}
		if got := []string{held[0].Account().ID, held[1].Account().ID, held[2].Account().ID}; !slices.Equal(got, []string{accountA, accountB, accountC}) {
			t.Fatalf("%s：前三个租约应依次落在 A、B、C: %v", test.name, got)
		}
		_ = held[1].Release()
		_ = held[2].Release()
		lease, err := env.service.acquireWarmLease(env.ctx, selection)
		if err != nil {
			t.Fatal(err)
		}
		if got := lease.Account().ID; got != test.want {
			t.Fatalf("%s：A 上还有请求、B、C 空闲时应选 %s，实际 %s", test.name, test.want, got)
		}
		_ = lease.Release()
		// 固定账户：照样落在指定的 A，不因 A 有请求而换成空闲的号
		fixed := selection
		fixed.AccountID = accountA
		pinned, err := env.service.acquireWarmLease(env.ctx, fixed)
		if err != nil {
			t.Fatal(err)
		}
		if got := pinned.Account().ID; got != accountA {
			t.Fatalf("%s：固定账户的选号应落在 A，实际 %s", test.name, got)
		}
		_ = pinned.Release()
		_ = held[0].Release()
	}
}

// TestBalancedIdleDeadSlotFirst 死槽位那一层同样先空闲、后全部：A、B 对主用模型长时间冷却（对其他模型是死槽位），C 正常；
// A 上有 1 个请求、B 空闲、上次选中 B。其他模型的请求先用死槽位：轮询在 A、B 里按 ID 轮转选 A，平均调用先选空闲的 B
func TestBalancedIdleDeadSlotFirst(t *testing.T) {
	const (
		accountA = "dead-a@example.com"
		accountB = "dead-b@example.com"
		accountC = "dead-c@example.com"
	)
	selection := aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"}
	for _, test := range []struct {
		name     string
		balanced bool
		want     string
	}{
		{name: "轮询", want: accountA},
		{name: "平均调用", balanced: true, want: accountB},
	} {
		env := newDrainEnv(t, []string{accountA, accountB, accountC}, 3, 3)
		env.promote(t, accountA, accountB, accountC)
		env.coolPro(t, accountA)
		env.coolPro(t, accountB)
		env.manager.setBalanced(test.balanced)
		busy := selection
		busy.AccountID = accountA
		holdA, _, err := env.pool.TryAcquireFor(env.ctx, busy)
		if err != nil || holdA == nil {
			t.Fatalf("取 A 的租约: %v", err)
		}
		last := selection
		last.AccountID = accountB
		leaseB, _, err := env.pool.TryAcquireFor(env.ctx, last)
		if err != nil || leaseB == nil {
			t.Fatalf("取 B 的租约: %v", err)
		}
		_ = leaseB.Release()
		lease, err := env.service.acquireWarmLease(env.ctx, selection)
		if err != nil {
			t.Fatal(err)
		}
		if got := lease.Account().ID; got != test.want {
			t.Fatalf("%s：应选死槽位里的 %s，实际 %s", test.name, test.want, got)
		}
		_ = lease.Release()
		_ = holdA.Release()
	}
}

// TestBalancedIdleVictimTieBreak 淘汰选号同档时：关闭平均调用先关最久未用的 A；开启后先关今日发送多的 B（即使它更近用过），
// 发送数相同时仍比最久未用
func TestBalancedIdleVictimTieBreak(t *testing.T) {
	const (
		accountA = "victim-a@example.com"
		accountB = "victim-b@example.com"
		accountC = "victim-c@example.com"
	)
	env := newDrainEnv(t, []string{accountA, accountB, accountC}, 1, 3)
	env.promote(t, accountA, accountB, accountC)
	for _, accountID := range []string{accountA, accountB, accountC} {
		holdLease(t, env.pool, accountID)()
		time.Sleep(5 * time.Millisecond)
	}
	normal := aistudio.PoolScopeNormal
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountA {
		t.Fatalf("关闭平均调用时应先关最久未用的 A，实际 %q", got)
	}
	env.manager.setBalanced(true)
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountA {
		t.Fatalf("今日发送数都相同时应先关最久未用的 A，实际 %q", got)
	}
	seedBalancedUsage(t, env.manager, map[string]uint32{accountA: 5, accountB: 30, accountC: 30})
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountB {
		t.Fatalf("开启平均调用时应先关今日发送多的 B（与 C 同数，B 更久未用），实际 %q", got)
	}
	env.manager.setBalanced(false)
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountA {
		t.Fatalf("关闭后应回到先关最久未用的 A，实际 %q", got)
	}
}

// TestIdleVictimSkipsDrained 淘汰选号不选软排空中的号（它的槽位已算作让给接替的号）；平均调用开启时普通排空中的号也不选，
// 关闭时普通排空照旧可选（与原来一致）
func TestIdleVictimSkipsDrained(t *testing.T) {
	const (
		accountA = "victim-a@example.com"
		accountB = "victim-b@example.com"
		accountC = "victim-c@example.com"
	)
	// 峰值 8：drainLimit 为 2，能软排空一个
	env := newDrainEnv(t, []string{accountA, accountB, accountC}, 1, 8)
	env.promote(t, accountA, accountB, accountC)
	for _, accountID := range []string{accountA, accountB, accountC} {
		holdLease(t, env.pool, accountID)()
		time.Sleep(5 * time.Millisecond)
	}
	normal := aistudio.PoolScopeNormal
	env.manager.setBalanced(true)
	// A 用得最多、也最久未用：没有排空时先关它
	seedBalancedUsage(t, env.manager, map[string]uint32{accountA: 90, accountB: 30, accountC: 30})
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountA {
		t.Fatalf("没有排空时应先关今日发送多的 A，实际 %q", got)
	}
	drain := env.manager.startDrain(accountA, normal, true)
	if drain == nil {
		t.Fatal("应能软排空")
	}
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountB {
		t.Fatalf("软排空中的 A 不应被选中，应选 B，实际 %q", got)
	}
	if got := env.manager.pickReplaceVictim("", drainProModel, victimReplaceable, normal); got == accountA {
		t.Fatal("按需替换不应选软排空中的 A")
	}
	if !env.manager.hardenDrain(accountA, drain) {
		t.Fatal("应能改为普通排空")
	}
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountB {
		t.Fatalf("平均调用开启时普通排空中的 A 同样不选，应选 B，实际 %q", got)
	}
	env.manager.setBalanced(false)
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountA {
		t.Fatalf("关闭平均调用时普通排空照旧可选（最久未用的 A），实际 %q", got)
	}
	// 关闭后残留的软排空（热关闭到下一轮淘汰循环之间）仍不选
	env.manager.clearDrain(accountA)
	if env.manager.startDrain(accountA, normal, true) == nil {
		t.Fatal("应能软排空")
	}
	if got := env.manager.pickIdleVictim("", testRuntimeModel, victimAny, normal); got != accountB {
		t.Fatalf("软排空中的 A 在关闭平均调用后也不应被选中，实际 %q", got)
	}
}

// TestBalancedPassOverview 平均调用的一轮（balancedPass）：未开启时什么也不做；开启后统计两个分区可调度账号（不可调度的不算）的今日发送数分布，
// 写概况日志，30 秒内不重复统计；关闭后状态页概况清空
func TestBalancedPassOverview(t *testing.T) {
	const (
		first   = "overview-a@example.com"
		second  = "overview-b@example.com"
		third   = "overview-c@example.com"
		blocked = "overview-d@example.com"
	)
	env := newDrainEnv(t, []string{first, second, third, blocked}, 1, 4)
	if err := env.pool.MarkAuthRequired(blocked, "HTTP 401"); err != nil {
		t.Fatal(err)
	}
	seedBalancedUsage(t, env.manager, map[string]uint32{first: 10, second: 2, blocked: 99})
	now := time.Now()
	env.service.balancedPass(env.ctx, now)
	if env.manager.balancedStatus(true) != nil || hasLogContaining(env.requests, "平均调用概况") {
		t.Fatal("未开启时不应统计概况或写日志")
	}
	env.manager.setBalanced(true)
	env.service.balancedPass(env.ctx, now)
	status := env.manager.balancedStatus(true)
	if status == nil {
		t.Fatal("开启后应有概况")
	}
	want := api.AdminBalancedPartition{Accounts: 3, Used: 2, Min: 0, Median: 2, P90: 10, Max: 10}
	if status.Normal != want || status.Ultra != (api.AdminBalancedPartition{}) || status.Gap != balancedRotateGap || status.QuotaDay == "" {
		t.Fatalf("概况 = %+v，期望普通分区 %+v（需要登录的账号不计）", status, want)
	}
	if env.manager.balancedStatus(false) != nil {
		t.Fatal("服务没有运行时不应显示概况")
	}
	if !hasLogContaining(env.requests, "平均调用概况", "额度日="+status.QuotaDay, "普通 可调度=3 今日用过=2 最少/中位/p90/最多=0/2/10/10", "Ultra 可调度=0") {
		t.Fatalf("应写概况日志，日志 = %v", serviceLogMessages(env.requests))
	}
	for _, message := range serviceLogMessages(env.requests) {
		if strings.Contains(message, "平均调用概况") && strings.Contains(message, "@example.com") {
			t.Fatalf("概况日志不应列出账户: %s", message)
		}
	}
	// 30 秒内不重复统计
	seedBalancedUsage(t, env.manager, map[string]uint32{third: 7})
	env.service.balancedPass(env.ctx, now.Add(10*time.Second))
	if got := env.manager.balancedStatus(true); got != status {
		t.Fatal("30 秒内不应重新统计")
	}
	env.service.balancedPass(env.ctx, now.Add(balancedCheckInterval))
	if got := env.manager.balancedStatus(true); got == status || got.Normal.Used != 3 {
		t.Fatalf("30 秒后应重新统计: %+v", got)
	}
	env.manager.setBalanced(false)
	if env.manager.balancedStatus(true) != nil {
		t.Fatal("关闭后状态页概况应清空")
	}
}

// TestBalancedStatusReadsCacheOnly 状态接口只读缓存，不对账户池加锁：管理器没有账户池时照样返回缓存的概况（碰到账户池会 panic）；
// 状态 JSON 只在开启且服务运行时带 balanced
func TestBalancedStatusReadsCacheOnly(t *testing.T) {
	manager := &accountWorkerManager{}
	cached := &api.AdminBalanced{QuotaDay: "2026-11-18", Gap: balancedRotateGap, Normal: api.AdminBalancedPartition{Accounts: 1300, Used: 812}}
	manager.setBalanced(true)
	manager.balancedState.status.Store(cached)
	if got := manager.balancedStatus(true); got != cached {
		t.Fatalf("应原样返回缓存的概况: %+v", got)
	}

	env := newEgressEnv(t, []string{"status-a@example.com"}, nil, 1, 2, 0)
	admin := &runtimeAdmin{pool: env.pool, service: env.service, requests: env.service.requests, workers: env.manager}
	env.service.state.Store(serviceRunning)
	encode := func() string {
		t.Helper()
		status, err := admin.Status(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(status)
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	if strings.Contains(encode(), `"balanced"`) {
		t.Fatal("未开启时状态不应带 balanced")
	}
	env.manager.setBalanced(true)
	env.service.balancedPass(context.Background(), time.Now())
	encoded := encode()
	for _, field := range []string{`"balanced":{`, `"quota_day":`, `"gap":50`, `"normal":{"accounts":1`, `"ultra":{"accounts":0`, `"rotations_1h":0`} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("状态 JSON 缺少 %s: %s", field, encoded)
		}
	}
	env.service.state.Store(serviceStopped)
	if strings.Contains(encode(), `"balanced"`) {
		t.Fatal("服务没有运行时状态不应带 balanced")
	}
}

// TestBalancedRoutingHotReload 管理页保存 ROUTING_STRATEGY=balanced：.env 里是 balanced，平均调用开关立即打开（不重启生成服务），
// 账户池按 round-robin 选号，热更新汇总日志写“策略=balanced（平均调用）”；改回 round-robin 立即关闭
func TestBalancedRoutingHotReload(t *testing.T) {
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	clearAccountRPMEnv(t)
	t.Setenv("ROUTING_STRATEGY", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	path := filepath.Join(t.TempDir(), ".env")
	cfg := config.Default()
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	requests := newRequestRegistry(ctx)
	pool, accounts := testAccountPool(t, "hot-a@example.com", "hot-b@example.com")
	configurePool(pool, cfg)
	workers := newAccountWorkerManager(pool, accounts, requests, "", "", time.Minute, cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency, false)
	t.Cleanup(func() { _ = workers.Close() })
	service := &trackedService{
		lifecycle: ctx, catalog: emptyCatalog{}, pool: pool, requests: requests, workers: workers,
		forbidden: newForbiddenTracker(), quota: newQuotaSharing("", requests),
	}
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	// picks 取放两次不限账户的生成租约：round-robin 两次落在不同账户，fill-first 都落在第一个
	picks := func() []string {
		t.Helper()
		var got []string
		for range 2 {
			lease, _, err := pool.TryAcquireFor(ctx, aistudio.AccountSelection{ModelID: testRuntimeModel, Method: "generateContent"})
			if err != nil || lease == nil {
				t.Fatalf("取租约: %v", err)
			}
			got = append(got, lease.Account().ID)
			_ = lease.Release()
		}
		return got
	}
	for _, test := range []struct {
		strategy string
		balanced bool
		summary  string
	}{
		{strategy: "balanced", balanced: true, summary: "策略=balanced（平均调用）"},
		{strategy: "round-robin", summary: "策略=round-robin |"},
	} {
		value := runtimeConfigDTO(cfg)
		value.RoutingStrategy = test.strategy
		updated, err := manager.UpdateRuntimeConfig(ctx, value)
		if err != nil {
			t.Fatal(err)
		}
		if updated.ServiceRestartRequired {
			t.Fatalf("策略改为 %s 后不应要求重启生成服务", test.strategy)
		}
		saved, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if saved.RoutingStrategy != test.strategy || manager.current.config.RoutingStrategy != test.strategy {
			t.Fatalf(".env 与生效配置的策略 = %q、%q，期望 %q", saved.RoutingStrategy, manager.current.config.RoutingStrategy, test.strategy)
		}
		if workers.balancedOn() != test.balanced {
			t.Fatalf("策略为 %s 时平均调用开关 = %t", test.strategy, workers.balancedOn())
		}
		if got := picks(); got[0] == got[1] {
			t.Fatalf("策略为 %s 时账户池应按 round-robin 轮流选号，两次都选了 %s", test.strategy, got[0])
		}
		if !hasLogContaining(requests, "配置已热更新", test.summary) {
			t.Fatalf("热更新日志应写明 %q，日志 = %v", test.summary, serviceLogMessages(requests))
		}
	}
	if got := routingStrategySummary("fill-first"); got != "fill-first" {
		t.Fatalf("其他策略应原样输出: %q", got)
	}
}
