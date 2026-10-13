package app

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
)

// supplyOf 返回状态里某个主用模型的供给，没有时 ok 为假
func supplyOf(entries []api.AdminPrimarySupply, modelID string) (api.AdminPrimarySupply, bool) {
	for _, entry := range entries {
		if entry.Model == modelID {
			return entry, true
		}
	}
	return api.AdminPrimarySupply{}, false
}

// runningAdmin 返回生成服务处于运行状态的管理端
func (env *drainEnv) runningAdmin() *runtimeAdmin {
	env.service.state.Store(serviceRunning)
	return &runtimeAdmin{lifecycle: env.ctx, pool: env.pool, service: env.service, requests: env.requests, workers: env.manager}
}

// TestPrimarySupplyCounts 主用模型供给的口径：warm 只数对主用模型没有冷却、不在排空中的驻留 Worker；standby 只数空闲、
// 对主用模型没有冷却、不在启动中的备用账号，达到上限时标记 capped
func TestPrimarySupplyCounts(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com"}
	env := newDrainEnv(t, accounts, 3, 4)
	env.promote(t, accounts[:3]...)
	// a 能服务主用模型；b 排空中；c 驻留但主用模型已冷却
	if env.manager.startDrain(accounts[1], aistudio.PoolScopeNormal) == nil {
		t.Fatal("b 应进入排空")
	}
	defer env.manager.clearDrain(accounts[1])
	env.coolPro(t, accounts[2])
	// 备用：d 主用模型已冷却，e 正被占用，f 空闲
	env.coolPro(t, accounts[3])
	release := holdLease(t, env.pool, accounts[4])
	entries := env.manager.computePrimarySupply([]string{drainProModel}, primarySupplyStandbyLimit)
	want := api.AdminPrimarySupply{Model: drainProModel, Warm: 1, Standby: 1}
	if len(entries) != 1 || entries[0] != want {
		t.Fatalf("供给 = %+v，应为 %+v", entries, want)
	}
	// 其他模型不受主用模型冷却影响：a、c 能服务，b 排空中不计；e 被占用、f 空闲，d 能服务
	other := env.manager.computePrimarySupply([]string{testRuntimeModel}, primarySupplyStandbyLimit)
	if got := other[0]; got.Warm != 2 || got.Standby != 2 {
		t.Fatalf("其他模型的供给 = %+v，应为 热 2 · 备用 2", got)
	}
	release()
	// e 空闲后备用为 2：上限 1 时只数到 1 并标记 capped，上限 2 时不标记
	if got := env.manager.computePrimarySupply([]string{drainProModel}, 1)[0]; got.Standby != 1 || !got.StandbyCapped {
		t.Fatalf("上限 1 时供给 = %+v，应为备用 1 且 capped", got)
	}
	if got := env.manager.computePrimarySupply([]string{drainProModel}, 2)[0]; got.Standby != 2 || got.StandbyCapped {
		t.Fatalf("上限 2 时供给 = %+v，应为备用 2 且不 capped", got)
	}
	// 正在启动的账号不算空闲备用
	env.manager.openingSet.Store(accounts[5], struct{}{})
	got := env.manager.computePrimarySupply([]string{drainProModel}, primarySupplyStandbyLimit)[0]
	env.manager.openingSet.Delete(accounts[5])
	if got.Standby != 1 {
		t.Fatalf("f 启动中时备用应为 1（只有 e），实际 %+v", got)
	}
	// 号池里没有的模型两项都为 0
	if got := env.manager.computePrimarySupply([]string{"missing-model"}, primarySupplyStandbyLimit); len(got) != 1 || got[0] != (api.AdminPrimarySupply{Model: "missing-model"}) {
		t.Fatalf("号池里没有的模型供给 = %+v", got)
	}
}

// TestPrimarySupplyCachedForStatus 状态接口只读缓存：缓存未过期时状态不随账户变化，过期或主用模型列表变化后重新统计；
// 移出列表的模型立即不显示，没有主用模型、服务没有运行或 ResetAll 之后不显示
func TestPrimarySupplyCachedForStatus(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.promote(t, accounts[0])
	admin := env.runningAdmin()
	status := func() []api.AdminPrimarySupply {
		t.Helper()
		result, err := admin.Status(env.ctx)
		if err != nil {
			t.Fatal(err)
		}
		return result.PrimarySupply
	}
	if got := status(); got != nil {
		t.Fatalf("还没统计时不应显示供给，实际 %+v", got)
	}
	start := time.Now()
	if !env.manager.refreshPrimarySupply(start) {
		t.Fatal("第一次应统计")
	}
	if got, ok := supplyOf(status(), drainProModel); !ok || got.Warm != 1 || got.Standby != 2 {
		t.Fatalf("供给 = %+v，应为 热 1 · 备用 2", got)
	}
	env.coolPro(t, accounts[0])
	if env.manager.refreshPrimarySupply(start.Add(primarySupplyInterval - time.Second)) {
		t.Fatal("缓存未过期时不应重新统计")
	}
	if got, _ := supplyOf(status(), drainProModel); got.Warm != 1 {
		t.Fatalf("状态应读缓存，热 Worker 仍为 1，实际 %+v", got)
	}
	if !env.manager.refreshPrimarySupply(start.Add(primarySupplyInterval)) {
		t.Fatal("缓存过期后应重新统计")
	}
	if got, _ := supplyOf(status(), drainProModel); got.Warm != 0 || got.Standby != 2 {
		t.Fatalf("重新统计后供给 = %+v，应为 热 0 · 备用 2", got)
	}
	// 主用模型列表变化后立即重新统计（不等 30 秒）
	env.service.setPrimaryModels([]string{drainProModel, testRuntimeModel}, 10*time.Second)
	if !env.manager.refreshPrimarySupply(start.Add(primarySupplyInterval + time.Second)) {
		t.Fatal("主用模型列表变化后应立即重新统计")
	}
	entries := status()
	if got, ok := supplyOf(entries, testRuntimeModel); len(entries) != 2 || !ok || got.Warm != 1 {
		t.Fatalf("两个主用模型的供给 = %+v", entries)
	}
	// 移出列表的模型在下次统计前就不再显示
	env.service.setPrimaryModels([]string{testRuntimeModel}, 10*time.Second)
	if entries := status(); len(entries) != 1 || entries[0].Model != testRuntimeModel {
		t.Fatalf("移出列表的模型不应显示，实际 %+v", entries)
	}
	env.service.state.Store(serviceStopped)
	if got := status(); got != nil {
		t.Fatalf("服务没有运行时不应显示供给，实际 %+v", got)
	}
	env.service.state.Store(serviceRunning)
	env.service.setPrimaryModels(nil, 10*time.Second)
	if env.manager.refreshPrimarySupply(start.Add(2*primarySupplyInterval)) || status() != nil {
		t.Fatal("没有主用模型时应清空缓存，不显示供给")
	}
	env.service.setPrimaryModels([]string{drainProModel}, 10*time.Second)
	env.manager.refreshPrimarySupply(start.Add(2 * primarySupplyInterval))
	if err := env.manager.ResetAll(); err != nil {
		t.Fatal(err)
	}
	if env.manager.primarySupply.Load() != nil {
		t.Fatal("ResetAll 后应清空主用模型供给")
	}
}

// TestKeepWarmRefreshesPrimarySupply keepWarm 的周期里统计主用模型供给，状态接口随后能读到
func TestKeepWarmRefreshesPrimarySupply(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com"}
	env := newDrainEnv(t, accounts, 1, 2)
	env.promote(t, accounts[0])
	admin := env.runningAdmin()
	env.service.keepWarmPass(env.ctx, time.Now())
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := supplyOf(status.PrimarySupply, drainProModel); !ok || got.Warm != 1 || got.Standby != 1 {
		t.Fatalf("keepWarm 的周期之后状态里的主用模型供给 = %+v", status.PrimarySupply)
	}
}

// TestStatusShowsDrainingAndRetiring 状态接口按分区显示排空中与待淘汰登记的数量：忙碌的死槽位排空期间排空 1、登记 1，
// 淘汰后都回到 0；JSON 字段为 draining、retiring 与 primary_supply
func TestStatusShowsDrainingAndRetiring(t *testing.T) {
	accounts := []string{"drain-a@example.com", "drain-b@example.com", "drain-c@example.com", "drain-d@example.com",
		"drain-e@example.com", "drain-f@example.com"}
	env := newDrainEnv(t, accounts, 2, 4)
	env.promote(t, accounts[:4]...)
	admin := env.runningAdmin()
	release := env.startBusyDrain(t, accounts[0])
	defer release()
	status, err := admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Draining != 1 || status.Workers.Retiring != 1 || status.Workers.Dead != 1 {
		t.Fatalf("排空期间 workers = 排空 %d · 登记 %d · 冷却占位 %d，应都为 1",
			status.Workers.Draining, status.Workers.Retiring, status.Workers.Dead)
	}
	if status.UltraWorkers.Draining != 0 || status.UltraWorkers.Retiring != 0 {
		t.Fatalf("Ultra 分区不应有排空或登记: %+v", status.UltraWorkers)
	}
	env.manager.refreshPrimarySupply(time.Now())
	status, err = admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"draining":1`, `"retiring":1`, `"primary_supply":[{"model":"` + drainProModel + `"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("状态 JSON 缺少 %s: %s", field, encoded)
		}
	}
	release()
	if !waitUntil(3*time.Second, func() bool { return !slices.Contains(env.manager.WarmAccountIDs(), accounts[0]) }) {
		t.Fatalf("租约释放后排空的 Worker 应被淘汰，驻留 Worker = %v", sortedWarm(env.manager))
	}
	status, err = admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workers.Draining != 0 || status.Workers.Retiring != 0 {
		t.Fatalf("淘汰后排空与登记应为 0: %+v", status.Workers)
	}
}
