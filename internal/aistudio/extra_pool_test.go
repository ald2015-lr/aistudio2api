package aistudio

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// 补充池在账户池内的部分（见 extra_pool.go）：号池取值、规则与版本、成员口径、模型列表、权益与选择覆盖

const (
	extraFlash  = "gemini-extra-flash"
	extraLite   = "gemini-extra-lite"
	extraArgon  = "gemini-4-argon"
	extraCarbon = "gemini-4-carbon"
	extraPaid   = "gemini-3.1-pro-preview"
)

var extraUsable = []string{extraFlash, extraLite}

// extraModel 返回补充池测试目录里的模型（只会对话的生成模型）
func extraModel(id string) Model {
	return Model{ID: id, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}}
}

// extraTestAccount 构造只在内存中的账户：Playground 目录为 flash、lite 与 PAID_ONLY 模型，Build 目录为 argon（Ultra 另有 carbon）
func extraTestAccount(id string, tier BenefitTier) *Account {
	account := indexTestAccount(id, tier, []Model{extraModel(extraFlash), extraModel(extraLite), extraModel(extraPaid)})
	account.buildModels = []Model{extraModel(extraArgon)}
	if tier == BenefitTierUltra {
		account.buildModels = append(account.buildModels, extraModel(extraCarbon))
	}
	return account
}

// extraTestPool 返回两个通道都启用、权益规则为默认（carbon 只给 Ultra，PAID_ONLY 模型只给付费权益，argon 只给付费权益）的账户池
func extraTestPool(accounts ...*Account) *AccountPool {
	pool := NewAccountPool(accounts, 2)
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	pool.SetModelTierRules([]string{extraCarbon}, []string{extraPaid, extraArgon})
	pool.SetPoolRules(false, true)
	return pool
}

// coolKey 给账户的冷却键写入到 until 为止的冷却
func coolKey(t testing.TB, pool *AccountPool, accountID string, key string, until time.Time) {
	t.Helper()
	if err := pool.MarkCooldownIfGeneration(accountID, key, pool.ModelAccessGeneration(accountID), time.Now(), until, "每日限额: 测试"); err != nil {
		t.Fatal(err)
	}
}

func memberIDs(members []ExtraMember) []string {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	return ids
}

func extraOn(includeUltra bool) ExtraPoolRules {
	return ExtraPoolRules{Enabled: true, IncludeUltra: includeUltra, DefaultPaid: true}
}

// exitOf 返回单个账户在门槛 min 下的成员判断
func exitOf(pool *AccountPool, accountID string, now time.Time, min time.Duration) ExtraExit {
	return pool.ExtraMemberStates([]string{accountID}, now, min, extraUsable)[accountID]
}

// TestPoolScopeExtraAppended 补充池取值追加在最后（已有取值 0～4 不变）：名称 extra、中文名补充池，不是分区，不包含任何分区；
// 关闭时权益掩码为 0，设置规范化的零值什么也不做（两个版本都不变）
func TestPoolScopeExtraAppended(t *testing.T) {
	if PoolScopeAll != 0 || PoolScopeNormal != 1 || PoolScopeUltra != 2 || PoolScopePro != 3 || PoolScopeNormalUltra != 4 ||
		PoolScopeExtra != 5 || PoolScopeCount != 6 {
		t.Fatal("号池取值只能追加：已有取值的数值变了，或补充池不在最后")
	}
	if PoolScopeExtra.String() != "extra" || PoolScopeExtra.Label() != "补充池" || PoolScopeExtra.IsPartition() {
		t.Fatalf("补充池的名称 = %q / %q，IsPartition = %t", PoolScopeExtra.String(), PoolScopeExtra.Label(), PoolScopeExtra.IsPartition())
	}
	for partition := range PoolScopeCount + 2 {
		if PoolScopeExtra.Contains(partition) {
			t.Fatalf("补充池不应包含分区 %d", partition)
		}
	}
	pool := extraTestPool(extraTestAccount("free@example.com", BenefitTierFree))
	if pool.rules.tiers[PoolScopeExtra] != 0 {
		t.Fatal("没有开启补充池时权益掩码应为 0")
	}
	eligibility, ruleVersion := pool.eligibilityVersion, pool.PoolRuleVersion()
	for _, rules := range []ExtraPoolRules{{}, {IncludeUltra: true}, {DefaultPaid: true}, {IncludeUltra: true, DefaultPaid: true}} {
		if pool.SetExtraPoolRules(rules) {
			t.Fatalf("关闭时设置 %+v 应规范化为零值、什么也不做", rules)
		}
	}
	if pool.eligibilityVersion != eligibility || pool.PoolRuleVersion() != ruleVersion || pool.ExtraPoolLive() != (ExtraPoolRules{}) {
		t.Fatal("关闭时设置规则不应递增任何版本")
	}
	// 切换号池成员规则时补充池的掩码原样保留（关闭时仍为 0）
	pool.SetPoolRules(true, false)
	if pool.rules.tiers[PoolScopeExtra] != 0 {
		t.Fatal("切换号池成员规则不应让关闭的补充池包含权益")
	}
}

// TestExtraRulesVersioning 开启时只改默认地址借付费模型：只递增 poolRuleVersion，候选索引不作废；改 IncludeUltra 或开关：两个版本都递增、
// 候选索引作废；切换 PRO_POOL 后补充池的掩码保留
func TestExtraRulesVersioning(t *testing.T) {
	pool := extraTestPool(extraTestAccount("free@example.com", BenefitTierFree), extraTestAccount("ultra@example.com", BenefitTierUltra))
	index := func() *schedulingIndex {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.schedulingLocked()
	}
	versions := func() (uint64, uint64) {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		return pool.eligibilityVersion, pool.PoolRuleVersion()
	}
	step := func(name string, rules ExtraPoolRules, wantChanged, wantMask bool) {
		t.Helper()
		before := index()
		eligibility, ruleVersion := versions()
		if changed := pool.SetExtraPoolRules(rules); changed != wantChanged {
			t.Fatalf("%s：changed = %t，期望 %t", name, changed, wantChanged)
		}
		gotEligibility, gotRule := versions()
		switch {
		case !wantChanged && (gotEligibility != eligibility || gotRule != ruleVersion):
			t.Fatalf("%s：没有变化时版本不应变", name)
		case wantChanged && gotRule != ruleVersion+1:
			t.Fatalf("%s：poolRuleVersion %d → %d，期望加一", name, ruleVersion, gotRule)
		case wantMask && (gotEligibility != eligibility+1 || index() == before):
			t.Fatalf("%s：掩码变化应递增 eligibilityVersion 并作废候选索引", name)
		case wantChanged && !wantMask && (gotEligibility != eligibility || index() != before):
			t.Fatalf("%s：只改默认地址借付费模型不应递增 eligibilityVersion、不应作废候选索引", name)
		}
		if pool.ExtraPoolLive() != rules.normalized() {
			t.Fatalf("%s：无锁副本 = %+v", name, pool.ExtraPoolLive())
		}
	}
	step("关闭时切换含 Ultra", ExtraPoolRules{IncludeUltra: true}, false, false)
	step("关闭时切换默认地址借付费模型", ExtraPoolRules{DefaultPaid: true}, false, false)
	step("开启", ExtraPoolRules{Enabled: true, DefaultPaid: true}, true, true)
	step("重复开启", ExtraPoolRules{Enabled: true, DefaultPaid: true}, false, false)
	step("只改默认地址借付费模型", ExtraPoolRules{Enabled: true}, true, false)
	step("改含 Ultra", ExtraPoolRules{Enabled: true, IncludeUltra: true}, true, true)
	ultra := uint8(1) << tierSlot(BenefitTierUltra)
	if mask := pool.rules.tiers[PoolScopeExtra]; mask&ultra == 0 || mask != uint8(1<<tierSlotCount-1) {
		t.Fatalf("含 Ultra 时掩码 = %b，期望全部权益格", mask)
	}
	pool.SetProPool(false)
	if mask := pool.rules.tiers[PoolScopeExtra]; mask != uint8(1<<tierSlotCount-1) {
		t.Fatalf("切换 PRO_POOL 后补充池的掩码应保留，得到 %b", mask)
	}
	step("关闭", ExtraPoolRules{IncludeUltra: true, DefaultPaid: true}, true, true)
	if pool.rules.tiers[PoolScopeExtra] != 0 {
		t.Fatal("关闭后掩码应为 0")
	}
}

// TestExtraMembersBuildOnlyCooling 情形 A：Pro 账户只在 Build 独有的 argon 上冷却（build: 键）、flash 照常可用：管理页显示冷却，
// 是补充池成员；长冷却口径（AccountLongCooling）不认它
func TestExtraMembersBuildOnlyCooling(t *testing.T) {
	pro := "pro@example.com"
	pool := extraTestPool(extraTestAccount(pro, BenefitTierPro), extraTestAccount("free@example.com", BenefitTierFree))
	pool.SetExtraPoolRules(extraOn(false))
	coolKey(t, pool, pro, ChannelCooldownScope(ChannelBuild, extraArgon), time.Now().Add(2*time.Hour))
	if status, _ := pool.StatusOf(pro); status.State != AccountCooldown {
		t.Fatalf("情形 A 的账户状态 = %s，期望 cooldown", status.State)
	}
	if pool.AccountLongCooling(pro, extraArgon, 15*time.Minute) {
		t.Fatal("情形 A 不应被长冷却口径认定")
	}
	now := time.Now()
	members := pool.ExtraMembers(now, 15*time.Minute, extraUsable)
	if !slices.Equal(memberIDs(members), []string{pro}) || members[0].Home != PoolScopePro || members[0].Tier != BenefitTierPro {
		t.Fatalf("成员 = %+v，期望只有情形 A 的 Pro 账户（家分区 Pro）", members)
	}
	if exit := exitOf(pool, pro, now, 15*time.Minute); exit != ExtraStay {
		t.Fatalf("精确判断 = %s，期望仍是成员", exit)
	}
}

// TestExtraMembersThresholds 进入要求冷却剩余不少于门槛（14 分 59 秒不行，15 分钟可以）；已在补充池时只要冷却没到期就一直是成员，
// 到期后为 ExtraRecovered
func TestExtraMembersThresholds(t *testing.T) {
	short, long := "short@example.com", "long@example.com"
	pool := extraTestPool(extraTestAccount(short, BenefitTierFree), extraTestAccount(long, BenefitTierFree))
	pool.SetExtraPoolRules(extraOn(false))
	base := time.Now().Add(time.Minute)
	coolKey(t, pool, short, extraLite, base.Add(15*time.Minute-time.Second))
	coolKey(t, pool, long, extraLite, base.Add(15*time.Minute))
	if got := memberIDs(pool.ExtraMembers(base, 15*time.Minute, extraUsable)); !slices.Equal(got, []string{long}) {
		t.Fatalf("进入口径的成员 = %v，期望只有剩余 15 分钟的账户", got)
	}
	if exitOf(pool, short, base, 15*time.Minute) != ExtraRecovered || exitOf(pool, long, base, 15*time.Minute) != ExtraStay {
		t.Fatal("精确判断的进入门槛与数组扫描不一致")
	}
	for _, at := range []time.Time{base, base.Add(14 * time.Minute), base.Add(15*time.Minute - time.Nanosecond)} {
		if exit := exitOf(pool, long, at, 0); exit != ExtraStay {
			t.Fatalf("留存口径在冷却到期前（%s）= %s，期望仍是成员", at.Sub(base), exit)
		}
	}
	if exit := exitOf(pool, long, base.Add(15*time.Minute), 0); exit != ExtraRecovered {
		t.Fatalf("冷却到期后 = %s，期望 ExtraRecovered", exit)
	}
}

// TestExtraMembersGlobalCooling 生效的全局冷却挡住进入（1 分钟的分钟限额也挡，虽然它不影响“还有模型能用”）；已在补充池时 1 分钟、
// 10 分钟的全局冷却不离开，20 分钟的按全局冷却离开
func TestExtraMembersGlobalCooling(t *testing.T) {
	one, ten, twenty := "one@example.com", "ten@example.com", "twenty@example.com"
	pool := extraTestPool(extraTestAccount(one, BenefitTierFree), extraTestAccount(ten, BenefitTierFree), extraTestAccount(twenty, BenefitTierFree))
	pool.SetExtraPoolRules(extraOn(false))
	now := time.Now()
	for _, accountID := range []string{one, ten, twenty} {
		coolKey(t, pool, accountID, extraLite, now.Add(2*time.Hour))
	}
	if got := memberIDs(pool.ExtraMembers(now, 15*time.Minute, extraUsable)); !slices.Equal(got, []string{one, ten, twenty}) {
		t.Fatalf("没有全局冷却时成员 = %v", got)
	}
	coolKey(t, pool, one, "", now.Add(time.Minute))
	coolKey(t, pool, ten, "", now.Add(10*time.Minute))
	coolKey(t, pool, twenty, "", now.Add(20*time.Minute))
	at := time.Now()
	if got := pool.ExtraMembers(at, 15*time.Minute, extraUsable); len(got) != 0 {
		t.Fatalf("生效的全局冷却应挡住进入，成员 = %v", memberIDs(got))
	}
	for _, accountID := range []string{one, ten} {
		if exit := exitOf(pool, accountID, at, 15*time.Minute); exit != ExtraGlobalCooling {
			t.Fatalf("%s：进入口径的精确判断 = %s，期望全局冷却", accountID, exit)
		}
		if exit := exitOf(pool, accountID, at, 0); exit != ExtraStay {
			t.Fatalf("%s：留存时短的全局冷却 = %s，期望不离开", accountID, exit)
		}
	}
	if exit := exitOf(pool, twenty, at, 0); exit != ExtraGlobalCooling {
		t.Fatalf("留存时 20 分钟的全局冷却 = %s，期望 ExtraGlobalCooling", exit)
	}
}

// TestExtraVerifyCooldownNotMember 只有单个模型需要验证账号的冷却（30 天）时不是成员：状态口径把这个通道当作不可用而不是冷却
func TestExtraVerifyCooldownNotMember(t *testing.T) {
	account := "verify@example.com"
	pool := extraTestPool(extraTestAccount(account, BenefitTierPro))
	pool.SetExtraPoolRules(extraOn(false))
	now := time.Now()
	if _, _, err := pool.MarkModelVerifyCooldownIfGeneration(account, extraLite, pool.ModelAccessGeneration(account), now, now.Add(30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if status, _ := pool.StatusOf(account); status.State != AccountReady {
		t.Fatalf("只有单模型需要验证时状态 = %s，期望就绪", status.State)
	}
	if got := pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable); len(got) != 0 {
		t.Fatalf("单模型需要验证不应算冷却，成员 = %v", memberIDs(got))
	}
	if exit := exitOf(pool, account, time.Now(), 0); exit != ExtraRecovered {
		t.Fatalf("精确判断 = %s，期望不是成员（没有冷却）", exit)
	}
}

// TestExtraMembersNoUsableModel 全部初始化模型都在冷却时不是成员（没有别的模型能用）
func TestExtraMembersNoUsableModel(t *testing.T) {
	account := "all-cooled@example.com"
	pool := extraTestPool(extraTestAccount(account, BenefitTierFree))
	pool.SetExtraPoolRules(extraOn(false))
	until := time.Now().Add(2 * time.Hour)
	coolKey(t, pool, account, extraFlash, until)
	coolKey(t, pool, account, extraLite, until)
	if got := pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable); len(got) != 0 {
		t.Fatalf("没有能用的模型时不应是成员：%v", memberIDs(got))
	}
	if exit := exitOf(pool, account, time.Now(), 0); exit != ExtraNoUsableModel {
		t.Fatalf("精确判断 = %s，期望 ExtraNoUsableModel", exit)
	}
	// 2 分钟内就恢复的冷却按能用处理（短冷却不影响成员身份）
	coolKey(t, pool, account, extraFlash, time.Now().Add(time.Minute))
	pool.mu.Lock()
	pool.byID[account].runtime.Cooldowns[extraFlash] = CooldownState{Until: time.Now().Add(time.Minute), Reason: "分钟限额"}
	pool.accountChangedLocked(pool.byID[account])
	pool.mu.Unlock()
	if exit := exitOf(pool, account, time.Now(), 0); exit != ExtraStay {
		t.Fatalf("1 分钟后恢复的模型应按能用处理，得到 %s", exit)
	}
}

// TestExtraMembersUnschedulable 需要登录、组织停用、停用的账户都不是成员
func TestExtraMembersUnschedulable(t *testing.T) {
	ids := []string{"auth@example.com", "org@example.com", "disabled@example.com"}
	accounts := make([]*Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, extraTestAccount(id, BenefitTierFree))
	}
	pool := extraTestPool(accounts...)
	pool.SetExtraPoolRules(extraOn(false))
	until := time.Now().Add(2 * time.Hour)
	for _, id := range ids {
		coolKey(t, pool, id, extraLite, until)
	}
	sorted := slices.Sorted(slices.Values(ids))
	if got := memberIDs(pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable)); !slices.Equal(got, sorted) {
		t.Fatalf("可调度时成员 = %v，期望全部", got)
	}
	if err := pool.MarkAuthRequired(ids[0], "测试需要登录"); err != nil {
		t.Fatal(err)
	}
	pool.mu.Lock()
	pool.byID[ids[1]].orgDisabled = true
	pool.accountChangedLocked(pool.byID[ids[1]])
	pool.byID[ids[2]].Config.Enabled = false
	pool.accountChangedLocked(pool.byID[ids[2]])
	pool.mu.Unlock()
	if got := pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable); len(got) != 0 {
		t.Fatalf("不可调度的账户不应是成员：%v", memberIDs(got))
	}
	for id, exit := range pool.ExtraMemberStates(ids, time.Now(), 0, extraUsable) {
		if exit != ExtraUnschedulable {
			t.Fatalf("%s：精确判断 = %s，期望 ExtraUnschedulable", id, exit)
		}
	}
}

// TestExtraHealthyStayHome 完全没有冷却的账户永远不是成员；不在账户池的账户为 ExtraMissing
func TestExtraHealthyStayHome(t *testing.T) {
	healthy := "healthy@example.com"
	pool := extraTestPool(extraTestAccount(healthy, BenefitTierPro))
	pool.SetExtraPoolRules(extraOn(true))
	if got := pool.ExtraMembers(time.Now(), 0, extraUsable); len(got) != 0 {
		t.Fatalf("没有冷却的账户不应是成员（门槛为 0 也一样）：%v", memberIDs(got))
	}
	states := pool.ExtraMemberStates([]string{healthy, "missing@example.com"}, time.Now(), 0, extraUsable)
	if states[healthy] != ExtraRecovered || states["missing@example.com"] != ExtraMissing {
		t.Fatalf("精确判断 = %v", states)
	}
}

// TestExtraUltraSwitch EXTRA_POOL_INCLUDE_ULTRA 关闭时 Ultra 账户不是成员（ExtraRuleOff），打开后是；补充池关闭时全部为 ExtraRuleOff
func TestExtraUltraSwitch(t *testing.T) {
	ultra, pro := "ultra@example.com", "pro@example.com"
	pool := extraTestPool(extraTestAccount(ultra, BenefitTierUltra), extraTestAccount(pro, BenefitTierPro))
	for _, id := range []string{ultra, pro} {
		coolKey(t, pool, id, ChannelCooldownScope(ChannelBuild, extraArgon), time.Now().Add(2*time.Hour))
	}
	check := func(name string, want []string, ultraExit ExtraExit) {
		t.Helper()
		if got := memberIDs(pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable)); !slices.Equal(got, want) {
			t.Fatalf("%s：成员 = %v，期望 %v", name, got, want)
		}
		if exit := exitOf(pool, ultra, time.Now(), 0); exit != ultraExit {
			t.Fatalf("%s：Ultra 账户 = %s，期望 %s", name, exit, ultraExit)
		}
	}
	check("关闭", nil, ExtraRuleOff)
	pool.SetExtraPoolRules(extraOn(false))
	check("不含 Ultra", []string{pro}, ExtraRuleOff)
	pool.SetExtraPoolRules(extraOn(true))
	check("含 Ultra", []string{pro, ultra}, ExtraStay)
	pool.SetExtraPoolRules(extraOn(false))
	check("再关闭含 Ultra", []string{pro}, ExtraRuleOff)
}

// TestExtraMembersArrayMatchesExact 随机构造 500 个账户与冷却表：按数组扫描的成员（ExtraMembers）与逐个账户精确判断
// （ExtraMemberStates，进入门槛 15 分钟）的结果相同
func TestExtraMembersArrayMatchesExact(t *testing.T) {
	random := rand.New(rand.NewSource(20261123))
	tiers := []BenefitTier{BenefitTierFree, BenefitTierPlus, BenefitTierPro, BenefitTierUltra}
	accounts := make([]*Account, 0, 500)
	for index := range 500 {
		account := extraTestAccount(fmt.Sprintf("acct-%03d@example.com", index), tiers[random.Intn(len(tiers))])
		switch random.Intn(20) {
		case 0:
			account.Config.Enabled = false
		case 1:
			account.State = AccountAuthRequired
		case 2:
			account.orgDisabled = true
		}
		accounts = append(accounts, account)
	}
	pool := extraTestPool(accounts...)
	pool.SetExtraPoolRules(ExtraPoolRules{Enabled: true, IncludeUltra: random.Intn(2) == 0})
	durations := []time.Duration{30 * time.Second, 5 * time.Minute, 14 * time.Minute, 16 * time.Minute, time.Hour, 6 * time.Hour}
	keys := []string{"", extraFlash, extraLite, extraPaid, ChannelCooldownScope(ChannelBuild, extraArgon), ChannelCooldownScope(ChannelBuild, extraFlash)}
	start := time.Now()
	for _, account := range accounts {
		for range random.Intn(4) {
			coolKey(t, pool, account.ID, keys[random.Intn(len(keys))], start.Add(time.Minute).Add(durations[random.Intn(len(durations))]))
		}
	}
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		ids = append(ids, account.ID)
	}
	for _, at := range []time.Duration{time.Minute, 10 * time.Minute, 50 * time.Minute, 3 * time.Hour} {
		now := start.Add(at)
		got := memberIDs(pool.ExtraMembers(now, 15*time.Minute, extraUsable))
		slices.Sort(got)
		var want []string
		for id, exit := range pool.ExtraMemberStates(ids, now, 15*time.Minute, extraUsable) {
			if exit == ExtraStay {
				want = append(want, id)
			}
		}
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("%s 后：数组扫描 %d 个成员，精确判断 %d 个\n数组 %v\n精确 %v", at, len(got), len(want), got, want)
		}
		if at == 10*time.Minute && len(want) == 0 {
			t.Fatal("随机数据应构造出成员")
		}
	}
}

// BenchmarkExtraMembers30k 3 万个账户、1.2 万个冷却中时一次成员扫描（参考值：本机目标 2ms 以内，不作为门禁）
func BenchmarkExtraMembers30k(b *testing.B) {
	accounts := make([]*Account, 0, 30000)
	for index := range 30000 {
		tier := BenefitTierFree
		if index%5 == 0 {
			tier = BenefitTierPro
		}
		accounts = append(accounts, extraTestAccount(fmt.Sprintf("acct-%05d@example.com", index), tier))
	}
	pool := extraTestPool(accounts...)
	pool.SetExtraPoolRules(extraOn(false))
	until := time.Now().Add(6 * time.Hour)
	for index := 0; index < 30000; index += 5 {
		for _, offset := range []int{0, 1} {
			coolKey(b, pool, accounts[index+offset].ID, extraLite, until)
		}
	}
	now := time.Now()
	if members := pool.ExtraMembers(now, 15*time.Minute, extraUsable); len(members) != 12000 {
		b.Fatalf("成员 %d 个，期望 12000", len(members))
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		pool.ExtraMembers(now, 15*time.Minute, extraUsable)
	}
}

// TestEligibleModelsInExtra 补充池的模型列表按真实权益：只有 Free 时不列 PAID_ONLY 与 argon；有 Pro 时列出 argon；含 Ultra 时列出 carbon；
// 关闭时为空
func TestEligibleModelsInExtra(t *testing.T) {
	catalog := []Model{extraModel(extraFlash), extraModel(extraPaid), extraModel(extraArgon), extraModel(extraCarbon)}
	listed := func(pool *AccountPool) []string {
		var ids []string
		for _, model := range pool.EligibleModelsIn(PoolScopeExtra, catalog) {
			ids = append(ids, model.ID)
		}
		return ids
	}
	free := extraTestPool(extraTestAccount("free@example.com", BenefitTierFree))
	if got := listed(free); len(got) != 0 {
		t.Fatalf("关闭时补充池不应列出模型：%v", got)
	}
	free.SetExtraPoolRules(extraOn(true))
	if got := listed(free); !slices.Equal(got, []string{extraFlash}) {
		t.Fatalf("只有 Free 时 = %v，期望只有 flash", got)
	}
	paid := extraTestPool(extraTestAccount("free@example.com", BenefitTierFree), extraTestAccount("pro@example.com", BenefitTierPro),
		extraTestAccount("ultra@example.com", BenefitTierUltra))
	paid.SetExtraPoolRules(extraOn(false))
	if got := listed(paid); !slices.Equal(got, []string{extraFlash, extraPaid, extraArgon}) {
		t.Fatalf("有 Pro、不含 Ultra 时 = %v", got)
	}
	paid.SetExtraPoolRules(extraOn(true))
	if got := listed(paid); !slices.Equal(got, []string{extraFlash, extraPaid, extraArgon, extraCarbon}) {
		t.Fatalf("含 Ultra 时 = %v", got)
	}
}

// TestFreeNeverInPaidExtraSet Free 成员（目录里列着 argon）不出现在 argon 与 PAID_ONLY 模型的补充池候选集里，Pro 成员出现
func TestFreeNeverInPaidExtraSet(t *testing.T) {
	free, pro := "free@example.com", "pro@example.com"
	pool := extraTestPool(extraTestAccount(free, BenefitTierFree), extraTestAccount(pro, BenefitTierPro))
	pool.SetExtraPoolRules(extraOn(true))
	until := time.Now().Add(2 * time.Hour)
	for _, id := range []string{free, pro} {
		coolKey(t, pool, id, extraLite, until)
	}
	if got := memberIDs(pool.ExtraMembers(time.Now(), 15*time.Minute, extraUsable)); !slices.Equal(got, []string{free, pro}) {
		t.Fatalf("两个都应是成员：%v", got)
	}
	for _, model := range []string{extraArgon, extraPaid} {
		groups, err := pool.ClassifyCandidates(context.Background(), AccountSelection{ModelID: model, Method: "generateContent", Pool: PoolScopeExtra}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(groups.StandbyReady, []string{pro}) {
			t.Fatalf("%s 的补充池候选 = %v，期望只有 Pro", model, groups.StandbyReady)
		}
	}
	groups, err := pool.ClassifyCandidates(context.Background(), AccountSelection{ModelID: extraFlash, Method: "generateContent", Pool: PoolScopeExtra}, nil)
	if err != nil || !slices.Equal(groups.StandbyReady, []string{free, pro}) {
		t.Fatalf("flash 的补充池候选 = %v err=%v", groups.StandbyReady, err)
	}
}

// TestSelectionOverrideAcquireFor 有选择覆盖时不固定账户的选号只在 allowed 里进行（号池改为覆盖的号池），换号上限按 allowed 计；
// 指定账户的选号与没有覆盖的选号与原来相同
func TestSelectionOverrideAcquireFor(t *testing.T) {
	a, b, c := "a@example.com", "b@example.com", "c@example.com"
	pool := extraTestPool(extraTestAccount(a, BenefitTierFree), extraTestAccount(b, BenefitTierFree), extraTestAccount(c, BenefitTierPro))
	pool.SetExtraPoolRules(extraOn(false))
	pool.SetRoutingStrategy("fill-first")
	selection := AccountSelection{ModelID: extraFlash, Method: "generateContent"}
	acquire := func(ctx context.Context, selection AccountSelection) string {
		t.Helper()
		lease, err := pool.AcquireFor(ctx, selection)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		return lease.Account().ID
	}
	normal := ContextWithPoolScope(context.Background(), PoolScopeNormal)
	if got := acquire(normal, selection); got != a {
		t.Fatalf("没有覆盖时 = %s，期望普通号池按顺序的第一个", got)
	}
	override := WithSelectionOverride(normal, PoolScopeExtra, []string{c})
	for range 3 {
		if got := acquire(override, selection); got != c {
			t.Fatalf("有覆盖时 = %s，期望只在 allowed 里选（补充池的 Pro 账户）", got)
		}
	}
	pinned := selection
	pinned.AccountID = b
	if got := acquire(override, pinned); got != b {
		t.Fatalf("指定账户的选号 = %s，不应受覆盖影响", got)
	}
	if limit := accountAttemptLimit(override, pool, false); limit != 1 {
		t.Fatalf("有覆盖时换号上限 = %d，期望 allowed 的个数", limit)
	}
	wide := WithAttemptCeiling(WithSelectionOverride(normal, PoolScopeExtra, []string{a, b, c}), 2)
	if limit := accountAttemptLimit(wide, pool, false); limit != 2 {
		t.Fatalf("换号上限 = %d，期望不超过 WithAttemptCeiling", limit)
	}
	if limit := accountAttemptLimit(normal, pool, false); limit != 2 {
		t.Fatalf("没有覆盖时换号上限 = %d，期望普通号池的账户数", limit)
	}
}

// TestExtraLeaseRevalidated 取得补充池里 Ultra 账户的租约后关闭 EXTRA_POOL_INCLUDE_ULTRA：确认租约时按新掩码放弃（不再属于补充池）
func TestExtraLeaseRevalidated(t *testing.T) {
	ultra := "ultra@example.com"
	pool := extraTestPool(extraTestAccount(ultra, BenefitTierUltra))
	pool.SetExtraPoolRules(extraOn(true))
	selection := AccountSelection{ModelID: extraFlash, Method: "generateContent", Pool: PoolScopeExtra}
	lease, err := pool.AcquireFor(context.Background(), selection)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if eligible, err := pool.refreshAndValidateLease(context.Background(), lease, selection); err != nil || !eligible {
		t.Fatalf("开关打开时租约应有效：%t %v", eligible, err)
	}
	pool.SetExtraPoolRules(extraOn(false))
	if eligible, err := pool.refreshAndValidateLease(context.Background(), lease, selection); err != nil || eligible {
		t.Fatalf("关闭含 Ultra 后应放弃租约：%t %v", eligible, err)
	}
}
