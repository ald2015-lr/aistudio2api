package aistudio

import (
	"testing"
	"time"
)

// warmCoolingModel 返回测试用的可生成模型
func warmCoolingModel(id string) Model {
	return Model{ID: id, Methods: []string{"generateContent"}, Capabilities: map[string]bool{"chat_model": true}}
}

// TestWarmWorkerStatesChannelCooling 淘汰选号的“冷却中”按该模型生成请求所有支持的启用通道判断：Build 键 build:<模型> 的冷却算；
// 只有一个通道冷却、另一支持的通道可用时不算；账户 Build 目录不支持该模型时只看 Playground；全局冷却不少于 2 分钟总算；
// modelID 为空时只看全局。Dead 仍按死槽位口径（所有启用通道都冷却，不看通道是否支持）
func TestWarmWorkerStatesChannelCooling(t *testing.T) {
	both := []Channel{ChannelPlayground, ChannelBuild}
	build := ChannelCooldownScope(ChannelBuild, strikeModel)
	for _, test := range []struct {
		name     string
		channels []Channel
		// noBuild 为账户 Build 目录不支持该模型
		noBuild bool
		cool    map[string]time.Duration
		modelID string
		cooling bool
		dead    bool
	}{
		{name: "只启用 Build、Build 键冷却 3 分钟", channels: []Channel{ChannelBuild}, cool: map[string]time.Duration{build: 3 * time.Minute}, modelID: strikeModel, cooling: true},
		{name: "两个通道都冷却 3 分钟", channels: both, cool: map[string]time.Duration{strikeModel: 3 * time.Minute, build: 3 * time.Minute}, modelID: strikeModel, cooling: true},
		{name: "两个通道都冷却 2 小时", channels: both, cool: map[string]time.Duration{strikeModel: 2 * time.Hour, build: 2 * time.Hour}, modelID: strikeModel, cooling: true, dead: true},
		{name: "两个通道启用只冷却 Build", channels: both, cool: map[string]time.Duration{build: 2 * time.Hour}, modelID: strikeModel},
		{name: "两个通道启用只冷却 Playground", channels: both, cool: map[string]time.Duration{strikeModel: 2 * time.Hour}, modelID: strikeModel},
		{name: "两个通道都冷却但最早恢复不到 2 分钟", channels: both, cool: map[string]time.Duration{strikeModel: 3 * time.Minute, build: time.Minute}, modelID: strikeModel},
		{name: "Build 目录不支持该模型、Playground 冷却 3 分钟", channels: both, noBuild: true, cool: map[string]time.Duration{strikeModel: 3 * time.Minute}, modelID: strikeModel, cooling: true},
		{name: "Build 目录不支持该模型、Playground 冷却 2 小时（Build 键没冷却，不算死槽位）", channels: both, noBuild: true, cool: map[string]time.Duration{strikeModel: 2 * time.Hour}, modelID: strikeModel, cooling: true},
		{name: "全局 3 分钟", channels: both, cool: map[string]time.Duration{"": 3 * time.Minute}, modelID: strikeModel, cooling: true},
		{name: "全局 3 分钟、模型不在账户目录", channels: both, cool: map[string]time.Duration{"": 3 * time.Minute}, modelID: "other-model", cooling: true},
		{name: "全局 1 分钟", channels: both, cool: map[string]time.Duration{"": time.Minute}, modelID: strikeModel},
		{name: "账户不支持的模型键冷却不算", channels: both, cool: map[string]time.Duration{"other-model": 2 * time.Hour}, modelID: "other-model"},
		{name: "modelID 为空时模型键冷却不算", channels: both, cool: map[string]time.Duration{strikeModel: 2 * time.Hour, build: 2 * time.Hour}},
		{name: "modelID 为空时全局 3 分钟", channels: both, cool: map[string]time.Duration{"": 3 * time.Minute}, cooling: true},
	} {
		model := warmCoolingModel(strikeModel)
		pool := NewAccountPool([]*Account{indexTestAccount(strikeAccount, BenefitTierFree, []Model{model})}, 2)
		if !test.noBuild {
			if err := pool.SetBuildCatalog(strikeAccount, []Model{model}); err != nil {
				t.Fatal(err)
			}
		}
		pool.SetUpstreamChannels(test.channels)
		for scope, remaining := range test.cool {
			markTestCooldown(t, pool, strikeAccount, scope, time.Now().Add(-time.Minute), time.Now().Add(remaining))
		}
		state, ok := pool.WarmWorkerStates([]string{strikeAccount}, test.modelID, 15*time.Minute)[strikeAccount]
		if !ok {
			t.Fatalf("%s：没有返回状态", test.name)
		}
		if state.Cooling != test.cooling || state.Dead != test.dead {
			t.Fatalf("%s：Cooling=%t Dead=%t，期望 Cooling=%t Dead=%t", test.name, state.Cooling, state.Dead, test.cooling, test.dead)
		}
	}
}

// TestWarmWorkerStatesNonGenerateModel 账户目录里的模型不支持 generateContent（只有 predictLongRunning 这类方法，视频、Live 请求
// 按模型取 Worker）：没有生成通道可判断，按 Playground 的模型键判断冷却中，两个通道启用与只启用 Playground 时都算；短冷却仍不算
func TestWarmWorkerStatesNonGenerateModel(t *testing.T) {
	const videoModel = "video-model"
	for _, test := range []struct {
		name     string
		channels []Channel
		cool     time.Duration
		cooling  bool
		dead     bool
	}{
		{name: "两个通道启用、模型键冷却 2 小时", channels: []Channel{ChannelPlayground, ChannelBuild}, cool: 2 * time.Hour, cooling: true},
		{name: "只启用 Playground、模型键冷却 2 小时", channels: []Channel{ChannelPlayground}, cool: 2 * time.Hour, cooling: true, dead: true},
		{name: "两个通道启用、模型键冷却 1 分钟", channels: []Channel{ChannelPlayground, ChannelBuild}, cool: time.Minute},
	} {
		model := Model{ID: videoModel, Methods: []string{"predictLongRunning"}, Capabilities: map[string]bool{"video_generation": true}}
		pool := NewAccountPool([]*Account{indexTestAccount(strikeAccount, BenefitTierFree, []Model{warmCoolingModel(strikeModel), model})}, 2)
		if err := pool.SetBuildCatalog(strikeAccount, []Model{warmCoolingModel(strikeModel)}); err != nil {
			t.Fatal(err)
		}
		pool.SetUpstreamChannels(test.channels)
		markTestCooldown(t, pool, strikeAccount, videoModel, time.Now().Add(-time.Minute), time.Now().Add(test.cool))
		state, ok := pool.WarmWorkerStates([]string{strikeAccount}, videoModel, 15*time.Minute)[strikeAccount]
		if !ok {
			t.Fatalf("%s：没有返回状态", test.name)
		}
		if state.Cooling != test.cooling || state.Dead != test.dead {
			t.Fatalf("%s：Cooling=%t Dead=%t，期望 Cooling=%t Dead=%t", test.name, state.Cooling, state.Dead, test.cooling, test.dead)
		}
		// 生成模型的冷却口径不受影响：视频模型键的冷却不让文本模型算冷却中
		if state := pool.WarmWorkerStates([]string{strikeAccount}, strikeModel, 15*time.Minute)[strikeAccount]; state.Cooling {
			t.Fatalf("%s：视频模型的冷却不应让 %s 算冷却中", test.name, strikeModel)
		}
	}
}

// TestCoolingCandidateCount 冷却中的候选按调度分类口径计数：是该模型的候选、所有支持的启用通道都在冷却（含短冷却）才计；
// 不是候选（目录不支持、不在号池）的账户、只冷却一个通道的账户与不在列表里的账户不计，冷却到期后不再计
func TestCoolingCandidateCount(t *testing.T) {
	model := warmCoolingModel(strikeModel)
	other := warmCoolingModel("other-model")
	ids := []string{"cool-a@example.com", "cool-b@example.com", "cool-c@example.com", "cool-d@example.com", "cool-e@example.com", "cool-f@example.com"}
	accounts := []*Account{
		indexTestAccount(ids[0], BenefitTierFree, []Model{model}),
		indexTestAccount(ids[1], BenefitTierFree, []Model{model}),
		indexTestAccount(ids[2], BenefitTierUltra, []Model{model}),
		indexTestAccount(ids[3], BenefitTierFree, []Model{other}),
		indexTestAccount(ids[4], BenefitTierFree, []Model{model}),
		indexTestAccount(ids[5], BenefitTierFree, []Model{model}),
	}
	pool := NewAccountPool(accounts, 2)
	for _, id := range ids {
		if id == ids[3] {
			continue
		}
		if err := pool.SetBuildCatalog(id, []Model{model}); err != nil {
			t.Fatal(err)
		}
	}
	pool.SetUpstreamChannels([]Channel{ChannelPlayground, ChannelBuild})
	build := ChannelCooldownScope(ChannelBuild, strikeModel)
	checkedAt := time.Now().Add(-time.Minute)
	// a：两个通道都在短冷却（30 秒）；c：Ultra 账户两个通道都冷却 1 小时；d：两个目录都没有该模型（不是候选），全局冷却；
	// e：只冷却 Playground；f：冷却但不在列表里
	markTestCooldown(t, pool, ids[3], "", checkedAt, time.Now().Add(time.Hour))
	for _, scope := range []string{strikeModel, build} {
		markTestCooldown(t, pool, ids[0], scope, checkedAt, time.Now().Add(30*time.Second))
		markTestCooldown(t, pool, ids[2], scope, checkedAt, time.Now().Add(time.Hour))
		markTestCooldown(t, pool, ids[5], scope, checkedAt, time.Now().Add(time.Hour))
	}
	markTestCooldown(t, pool, ids[4], strikeModel, checkedAt, time.Now().Add(time.Hour))
	listed := ids[:5]
	selection := AccountSelection{ModelID: "models/" + strikeModel, Method: "generateContent"}
	for scope, want := range map[PoolScope]int{PoolScopeNormal: 1, PoolScopeUltra: 1, PoolScopeAll: 2} {
		selection.Pool = scope
		if got := pool.CoolingCandidateCount(selection, listed); got != want {
			t.Fatalf("号池 %q 的冷却候选 = %d，期望 %d", scope.String(), got, want)
		}
	}
	// 全局冷却的候选同样计入
	markTestCooldown(t, pool, ids[1], "", checkedAt, time.Now().Add(time.Hour))
	selection.Pool = PoolScopeNormal
	if got := pool.CoolingCandidateCount(selection, listed); got != 2 {
		t.Fatalf("b 全局冷却后普通号池的冷却候选 = %d，期望 2", got)
	}
	if got := pool.CoolingCandidateCount(selection, nil); got != 0 {
		t.Fatalf("空列表的冷却候选 = %d", got)
	}
	var missing *AccountPool
	if got := missing.CoolingCandidateCount(selection, listed); got != 0 {
		t.Fatalf("nil 账户池的冷却候选 = %d", got)
	}
	// 冷却到期后不再计入
	short := time.Now().Add(30 * time.Millisecond)
	markTestCooldown(t, pool, ids[1], "", checkedAt.Add(time.Second), short)
	for _, scope := range []string{strikeModel, build} {
		markTestCooldown(t, pool, ids[0], scope, checkedAt.Add(time.Second), short)
	}
	time.Sleep(time.Until(short) + 20*time.Millisecond)
	if got := pool.CoolingCandidateCount(selection, listed); got != 0 {
		t.Fatalf("冷却到期后普通号池的冷却候选 = %d，期望 0", got)
	}
}
