package aistudio

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// IP 池绑定与出口通道（合并 429 治理）：绑定变化在同一次加锁内重挂出口通道并唤醒等待方；会话号写在密码里时出口键带 #会话号；
// 空闲出口记录删除、编号复用

// TestProxyBindingsReattachEgress SetProxyBindings 与 UpdateProxyBindings 写入新绑定时按新的出口键重挂通道、唤醒等待方；
// 内容不变的推送不重挂、不唤醒；清除绑定回到全局代理（为空时直连）
func TestProxyBindingsReattachEgress(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com", "b@example.com")
	const portA, portB = "http://user:S3cr3t-P%40ss@pool.example:10001", "http://user:S3cr3t-P%40ss@pool.example:10002"
	changed := pool.Changed()
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "a@example.com", Binding: &ProxyBinding{URL: portA, Slot: "10001", Active: true}}})
	select {
	case <-changed:
	default:
		t.Fatal("换了通道应唤醒等待方")
	}
	if key, _ := pool.EgressKeyOf("a@example.com"); key != EgressKey(portA) || key == "" {
		t.Fatalf("绑定后 a 的出口 = %q，期望 %q", key, EgressKey(portA))
	}
	if key, _ := pool.EgressKeyOf("b@example.com"); key != "" {
		t.Fatalf("b 没有绑定，出口应为直连：%q", key)
	}
	verifyIndex(t, pool, "推送绑定后")
	// 内容相同的推送：不写入、不重挂、不唤醒
	changed = pool.Changed()
	if written := pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "a@example.com", Binding: &ProxyBinding{URL: portA, Slot: "10001", Active: true}}}); written != 0 {
		t.Fatalf("相同绑定写入 %d 个", written)
	}
	select {
	case <-changed:
		t.Fatal("没有换通道时不应唤醒")
	default:
	}
	// 整池重推：b 挂到 10002，a 清除绑定回到直连
	pool.UpdateProxyBindings(func(accountID, _ string) *ProxyBinding {
		if accountID == "b@example.com" {
			return &ProxyBinding{URL: portB, Slot: "10002"}
		}
		return nil
	})
	select {
	case <-changed:
	default:
		t.Fatal("整池重推换了通道应唤醒等待方")
	}
	if key, _ := pool.EgressKeyOf("a@example.com"); key != "" {
		t.Fatalf("清除绑定后 a 应回到直连：%q", key)
	}
	if key, _ := pool.EgressKeyOf("b@example.com"); key != EgressKey(portB) {
		t.Fatalf("重推后 b 的出口 = %q", key)
	}
	verifyIndex(t, pool, "整池重推后")
	// 账户有自己的代理时绑定（含 EgressTag）不参与出口键
	pool.mu.Lock()
	pool.byID["b@example.com"].Config.Proxy = "http://own.example:3128"
	pool.attachEgressLocked(pool.byID["b@example.com"])
	pool.mu.Unlock()
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "b@example.com", Binding: &ProxyBinding{URL: portB, EgressTag: "s1"}}})
	if key, _ := pool.EgressKeyOf("b@example.com"); key != EgressKey("http://own.example:3128") {
		t.Fatalf("有自己代理的账户出口 = %q", key)
	}
	verifyIndex(t, pool, "账户固定代理")
}

// TestEgressTagSeparatesPasswordSessions 会话号写在密码里时 EgressKey 去掉密码后各会话是同一个键：绑定带 EgressTag 时出口键追加 #会话号，
// 每个会话一个出口、标签各不相同且不含会话号与密码；没有 EgressTag 时合成一个出口（对照）
func TestEgressTagSeparatesPasswordSessions(t *testing.T) {
	ids := []string{"a@example.com", "b@example.com", "c@example.com"}
	pool := newEgressTestPool(2, ids...)
	updates := make([]ProxyBindingUpdate, 0, len(ids))
	for index, id := range ids {
		session := fmt.Sprintf("sess%02d", index)
		url := "http://customer-a1:pw-" + session + "@gate.example.com:58688"
		updates = append(updates, ProxyBindingUpdate{AccountID: id, Binding: &ProxyBinding{URL: url, Slot: session, EgressTag: session}})
	}
	pool.SetProxyBindings(updates)
	keys, labels := map[string]bool{}, map[string]bool{}
	for _, status := range pool.EgressSnapshot() {
		if status.Lanes[EgressLaneNormal].Accounts == 0 {
			continue
		}
		keys[status.Key] = true
		labels[status.Label] = true
		if status.Label == "" || containsAny(status.Label, "sess", "pw-", "customer") {
			t.Fatalf("标签 %q 不应含会话号、密码或用户名", status.Label)
		}
	}
	if len(keys) != len(ids) || len(labels) != len(ids) {
		t.Fatalf("带 EgressTag 时出口 %d 个、标签 %d 个，期望各 %d 个", len(keys), len(labels), len(ids))
	}
	for index, id := range ids {
		if key, _ := pool.EgressKeyOf(id); key != EgressKey("http://customer-a1@gate.example.com:58688")+fmt.Sprintf("#sess%02d", index) {
			t.Fatalf("%s 的出口键 = %q", id, key)
		}
	}
	verifyIndex(t, pool, "带 EgressTag")
	// 对照：去掉 EgressTag 后全部会话合成一个出口
	for index := range updates {
		binding := *updates[index].Binding
		binding.EgressTag = ""
		updates[index].Binding = &binding
	}
	pool.SetProxyBindings(updates)
	shared, _ := pool.EgressKeyOf(ids[0])
	for _, id := range ids[1:] {
		if key, _ := pool.EgressKeyOf(id); key != shared {
			t.Fatalf("没有 EgressTag 时 %s 的出口 %q 应与 %q 相同", id, key, shared)
		}
	}
	verifyIndex(t, pool, "去掉 EgressTag")
}

// TestEgressPruneIdleRecords 没有账户、没有预占、窗口内没有发送且不限速的出口记录在快照时删除，编号对给新出口复用；
// 有发送记录或限速的出口保留到窗口过去
func TestEgressPruneIdleRecords(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com")
	pool.mu.Lock()
	pool.egressWindow = 50 * time.Millisecond
	pool.mu.Unlock()
	const port1, port2, port3 = "http://pool.example:10001", "http://pool.example:10002", "http://pool.example:10003"
	push := func(url string) {
		pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "a@example.com", Binding: &ProxyBinding{URL: url}}})
	}
	push(port1)
	rate := egressLane(pool, EgressKey(port1), EgressLaneNormal)
	pool.mu.Lock()
	now := time.Now().UnixNano()
	rate.sent.add(now, true, now-int64(pool.egressWindow))
	pool.mu.Unlock()
	push(port2)
	// 直连与 10001 都没有账户了：直连马上删除，10001 窗口内还有发送，保留
	if _, listed := egressLaneSnapshot(pool, EgressKey(port1), EgressLaneNormal); !listed {
		t.Fatal("窗口内还有发送的出口应保留")
	}
	if egressLane(pool, "", EgressLaneNormal) != nil {
		t.Fatal("没有账户与活动的直连出口记录应删除")
	}
	verifyIndex(t, pool, "删除直连")
	time.Sleep(80 * time.Millisecond)
	pool.EgressSnapshot()
	if egressLane(pool, EgressKey(port1), EgressLaneNormal) != nil {
		t.Fatal("窗口过去后空闲的出口记录应删除")
	}
	pool.SetEgressRateLimit(EgressKey(port1), EgressLaneNormal, 6)
	pool.EgressSnapshot()
	if egressLane(pool, EgressKey(port1), EgressLaneNormal) == nil {
		t.Fatal("限速中的出口即使没有账户也应保留")
	}
	pool.SetEgressRateLimit(EgressKey(port1), EgressLaneNormal, 0)
	pool.EgressSnapshot()
	push(port3)
	pool.EgressSnapshot()
	pool.mu.Lock()
	lanes, records, free := len(pool.egressLanes), len(pool.egresses), len(pool.egressFree)
	pool.mu.Unlock()
	// 出现过直连、10001、10002、10003 四个出口，同时最多两个：编号对被复用，通道数组不随出口增减一直增长
	if records != 1 || lanes > 6 || free*2+records*2 != lanes {
		t.Fatalf("出口记录 %d 个、通道 %d 条、空闲编号对 %d 个，期望复用编号", records, lanes, free)
	}
	if got := egressLane(pool, EgressKey(port3), EgressLaneNormal); got == nil || got.id%2 != 0 || got.id >= uint32(lanes) {
		t.Fatalf("10003 的普通通道编号应为复用的编号对起点：%+v", got)
	}
	verifyIndex(t, pool, "复用编号后")
	if err := pool.VerifyEgressForTest(); err != nil {
		t.Fatal(err)
	}
}

// TestEgressLabelSessionTag 出口键带 #会话号时标签按用户名与会话号一起哈希：同一用户名不同会话的标签不同，不带会话号时与原来相同
func TestEgressLabelSessionTag(t *testing.T) {
	base := EgressKey("http://customer:pw@gate.example.com:58688")
	if EgressLabel(base) != "http://gate.example.com:58688#"+egressHash("customer", 4) {
		t.Fatalf("不带会话号的标签变了：%q", EgressLabel(base))
	}
	first, second := EgressLabel(base+"#s1"), EgressLabel(base+"#s2")
	if first == second || first == EgressLabel(base) || containsAny(first, "s1", "customer", "pw") {
		t.Fatalf("带会话号的标签 %q / %q", first, second)
	}
}

func containsAny(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

// sessionEgressTestPool 建一个每个账户各绑一个会话（会话号在密码里，出口键带 #会话号）的账户池：动态 IP 下没有 Worker 的账户
// 默认出口就是自己的会话，出口数与账户数相同
func sessionEgressTestPool(count int) (*AccountPool, []string) {
	ids := make([]string, count)
	for index := range ids {
		ids[index] = fmt.Sprintf("acct-%05d@example.com", index)
	}
	pool := newEgressTestPool(1, ids...)
	updates := make([]ProxyBindingUpdate, 0, count)
	for index, id := range ids {
		session := fmt.Sprintf("s%05d", index)
		updates = append(updates, ProxyBindingUpdate{AccountID: id, Binding: &ProxyBinding{
			URL: "http://customer-a1:pw-" + session + "@gate.example.com:58688", Slot: session, EgressTag: session,
		}})
	}
	pool.SetProxyBindings(updates)
	return pool, ids
}

// TestEgressSnapshotForKeys 按键取快照（复查 2-1）：只构造请求的出口，没有记录的键不返回；总数与全量快照的条数相同（换绑定后、
// 只设了限速的空出口、窗口内还有发送的空出口都一致）；待删记录只含没有账户的出口。出口上万时按一个键取快照的分配不随出口数增长
func TestEgressSnapshotForKeys(t *testing.T) {
	pool, ids := sessionEgressTestPool(64)
	pool.mu.Lock()
	pool.egressWindow = 50 * time.Millisecond
	pool.mu.Unlock()
	first, _ := pool.EgressKeyOf(ids[0])
	check := func(stage string) {
		t.Helper()
		all := pool.EgressSnapshot()
		statuses, total := pool.EgressSnapshotFor([]string{first, "http://missing.example:1"})
		if total != len(all) {
			t.Fatalf("%s：总数 %d，全量快照 %d 条", stage, total, len(all))
		}
		for _, status := range all {
			if status.Key == first && (len(statuses) != 1 || statuses[0] != status) {
				t.Fatalf("%s：按键取到 %+v，全量里是 %+v", stage, statuses, status)
			}
		}
		if err := pool.VerifyEgressForTest(); err != nil {
			t.Fatalf("%s：%v", stage, err)
		}
	}
	check("每个账户一个出口")
	if statuses, total := pool.EgressSnapshotFor(nil); len(statuses) != 0 || total != len(ids) {
		t.Fatalf("不给键时只返回总数：%d 条，总数 %d", len(statuses), total)
	}
	// 账户 0 换到新会话：旧会话窗口内还有发送，保留到窗口过去
	pool.mu.Lock()
	rate := pool.egresses[first].lanes[EgressLaneNormal]
	now := time.Now().UnixNano()
	rate.sent.add(now, true, now-int64(pool.egressWindow))
	pool.mu.Unlock()
	pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: ids[0], Binding: &ProxyBinding{
		URL: "http://customer-a1:pw-next@gate.example.com:58688", Slot: "next", EgressTag: "next",
	}}})
	check("换会话、旧会话窗口内还有发送")
	pool.SetEgressRateLimit("http://idle.example:3128", EgressLaneNormal, 6)
	check("只设了限速的空出口")
	time.Sleep(80 * time.Millisecond)
	check("窗口过去")
	pool.mu.Lock()
	_, kept := pool.egresses[first]
	idle := len(pool.egressIdle)
	pool.mu.Unlock()
	if kept || idle != 1 {
		t.Fatalf("旧会话应删除（%v），待删记录只剩限速中的空出口（%d）", kept, idle)
	}

	large, largeIDs := sessionEgressTestPool(10000)
	key, _ := large.EgressKeyOf(largeIDs[5000])
	large.EgressSnapshotFor([]string{key})
	if allocs := testing.AllocsPerRun(20, func() {
		if statuses, total := large.EgressSnapshotFor([]string{key}); len(statuses) != 1 || total != len(largeIDs) {
			t.Fatalf("按键取快照 %d 条，总数 %d", len(statuses), total)
		}
	}); allocs > 4 {
		t.Fatalf("1 万个出口时按一个键取快照分配 %.0f 次，应与出口数无关", allocs)
	}
}

// BenchmarkEgressSnapshot1e4 1 万个出口（动态 IP 每个账户一个会话）时全量快照与按键快照的耗时对比（复查 2-1）
func BenchmarkEgressSnapshot1e4(b *testing.B) {
	pool, ids := sessionEgressTestPool(10000)
	keys := make([]string, 0, 200)
	for _, id := range ids[:200] {
		key, _ := pool.EgressKeyOf(id)
		keys = append(keys, key)
	}
	b.Run("全量", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			pool.EgressSnapshot()
		}
	})
	b.Run("按200个键", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			pool.EgressSnapshotFor(keys)
		}
	})
}

// TestLeaseRebindEgress 冷启动的租约跟到实际出口（合并复查第 2 轮 1-1）：租约取得时账户挂在默认出口 D 上，Worker 启动后绑定改到端口 P。
// 重挂之前 EgressKey 仍是 D；RebindEgress 把租约与没发出的预占搬到 P（D 退还预占与令牌并唤醒等待方，P 限速时消耗一个令牌），
// 之后的发送、释放都记在 P 上；通道没变时不动
func TestLeaseRebindEgress(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com")
	const portD, portP = "http://user:S3cr3t-P%40ss@pool.example:10001", "http://user:S3cr3t-P%40ss@pool.example:10002"
	keyD, keyP := EgressKey(portD), EgressKey(portP)
	bind := func(url, slot string, active bool) {
		pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "a@example.com", Binding: &ProxyBinding{URL: url, Slot: slot, Active: active}}})
	}
	bind(portD, "10001", false)
	pool.SetEgressRateLimit(keyD, EgressLaneNormal, 6)
	pool.SetEgressRateLimit(keyP, EgressLaneNormal, 6)
	lease := mustTryAcquire(t, pool, egressPrimarySelection)
	if lease.EgressKey() != keyD || lease.EgressLane() != EgressLaneNormal {
		t.Fatalf("租约取得时的出口 = %q，期望默认出口 %q", lease.EgressKey(), keyD)
	}
	if lease.RebindEgress() {
		t.Fatal("账户的通道没变时不应重挂")
	}
	if status, _ := egressLaneSnapshot(pool, keyD, EgressLaneNormal); status.Pending != 1 || status.NextAt.IsZero() {
		t.Fatalf("D 预占后快照 = %+v，期望预占 1、令牌已消耗", status)
	}
	bind(portP, "10002", true)
	if lease.EgressKey() != keyD {
		t.Fatalf("重挂之前租约的出口应仍是 D：%q", lease.EgressKey())
	}
	changed := pool.Changed()
	if !lease.RebindEgress() {
		t.Fatal("Worker 换了端口后应重挂到 P")
	}
	select {
	case <-changed:
	default:
		t.Fatal("D 退还令牌后应唤醒等待方")
	}
	if lease.EgressKey() != keyP {
		t.Fatalf("重挂后租约的出口 = %q，期望 %q", lease.EgressKey(), keyP)
	}
	if status, _ := egressLaneSnapshot(pool, keyD, EgressLaneNormal); status.Pending != 0 || !status.NextAt.IsZero() {
		t.Fatalf("D 重挂后快照 = %+v，期望预占 0、令牌已退还", status)
	}
	if status, _ := egressLaneSnapshot(pool, keyP, EgressLaneNormal); status.Pending != 1 || status.NextAt.IsZero() || status.Accounts != 1 {
		t.Fatalf("P 重挂后快照 = %+v，期望 1 个账户、预占 1、令牌已消耗", status)
	}
	lease.RecordGenerationSent()
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	statusD, _ := egressLaneSnapshot(pool, keyD, EgressLaneNormal)
	statusP, _ := egressLaneSnapshot(pool, keyP, EgressLaneNormal)
	if statusD.Sent1m != 0 || statusD.Pending != 0 || statusP.Sent1m != 1 || statusP.Pending != 0 || statusP.NextAt.IsZero() {
		t.Fatalf("发出并释放后 D = %+v、P = %+v，期望发送与预占都记在 P、P 的令牌不退还", statusD, statusP)
	}
	if err := pool.VerifyEgressForTest(); err != nil {
		t.Fatal(err)
	}
	verifyIndex(t, pool, "重挂后")
}

// TestLeaseRebindEgressAtSend 没有显式重挂的路径（转写、Live、Veo 经 Worker() 冷启动，尝试内重开 Worker）：生成发送前自动跟到账户当前的
// 通道，发送记在实际出口上；非生成 RPC 记在账户当前的通道上（Go 侧按当前绑定出网），不重挂租约；没发出就释放时退还当前通道的预占
func TestLeaseRebindEgressAtSend(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com")
	const portD, portP = "http://pool.example:10001", "http://pool.example:10002"
	keyD, keyP := EgressKey(portD), EgressKey(portP)
	bind := func(url string) {
		pool.SetProxyBindings([]ProxyBindingUpdate{{AccountID: "a@example.com", Binding: &ProxyBinding{URL: url, Active: true}}})
	}
	bind(portD)
	lease := mustTryAcquire(t, pool, egressTestSelection)
	bind(portP)
	transport := &egressCountingTransport{}
	client := &Client{transport: transport, catalogs: make(map[string]modelCatalog), tiers: make(map[string]BenefitTier)}
	response, err := client.do(ContextWithAccountLease(context.Background(), lease), "ListModels", "a@example.com", "", []byte("[]"), false)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if lease.EgressKey() != keyD {
		t.Fatalf("非生成 RPC 不应重挂租约：%q", lease.EgressKey())
	}
	if status, _ := egressLaneSnapshot(pool, keyP, EgressLaneNormal); status.OtherSent1m != 1 || status.Pending != 0 {
		t.Fatalf("P 的快照 = %+v，期望非生成 RPC 记在账户当前的通道上", status)
	}
	if err := lease.beginGenerationSend(indexTestModel); err != nil {
		t.Fatal(err)
	}
	if lease.EgressKey() != keyP {
		t.Fatalf("生成发送前应跟到账户当前的出口：%q", lease.EgressKey())
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	statusD, _ := egressLaneSnapshot(pool, keyD, EgressLaneNormal)
	statusP, _ := egressLaneSnapshot(pool, keyP, EgressLaneNormal)
	if statusD.Sent1m != 0 || statusD.Pending != 0 || statusD.OtherSent1m != 0 || statusP.Sent1m != 1 || statusP.Pending != 0 {
		t.Fatalf("D = %+v、P = %+v，期望发送记在 P、两边都没有剩下的预占", statusD, statusP)
	}
	// 没发出就释放：预占已随重挂搬到 P，释放时从 P 退还
	held := mustTryAcquire(t, pool, egressTestSelection)
	bind(portD)
	if !held.RebindEgress() {
		t.Fatal("换回 D 后应重挂")
	}
	if status, _ := egressLaneSnapshot(pool, keyD, EgressLaneNormal); status.Pending != 1 {
		t.Fatalf("重挂后 D 的预占 = %d，期望 1", status.Pending)
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{keyD, keyP} {
		if status, _ := egressLaneSnapshot(pool, key, EgressLaneNormal); status.Pending != 0 {
			t.Fatalf("释放后 %s 仍有预占 %d", EgressLabel(key), status.Pending)
		}
	}
	if err := pool.VerifyEgressForTest(); err != nil {
		t.Fatal(err)
	}
}
