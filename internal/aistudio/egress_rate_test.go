package aistudio

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// 出口令牌桶（egress_rate.go）的测试：令牌桶的速率、突发与主用优先在假时钟上直接驱动通道（egressSim）；
// 选号、分类、唤醒、重挂与等待上限经账户池的公开入口

// egressTestSelection 为普通生成选择（非主用模型）；egressPrimarySelection 为主用模型的生成选择
var (
	egressTestSelection    = AccountSelection{ModelID: indexTestModel, Method: "generateContent"}
	egressPrimarySelection = AccountSelection{ModelID: indexTestModel, Method: "generateContent", EgressPriority: true}
)

// newEgressTestPool 返回只在内存中的账户池（单账户并发 concurrency），账户为 ids 对应的 Free 账户
func newEgressTestPool(concurrency int, ids ...string) *AccountPool {
	accounts := make([]*Account, 0, len(ids))
	for _, id := range ids {
		accounts = append(accounts, indexTestAccount(id, BenefitTierFree, indexTestCatalog()))
	}
	return NewAccountPool(accounts, concurrency)
}

// egressLane 返回出口通道（不存在时为 nil）
func egressLane(pool *AccountPool, key string, lane EgressLane) *egressRate {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if record := pool.egresses[key]; record != nil {
		return record.lanes[lane]
	}
	return nil
}

// egressLaneSnapshot 返回快照里出口通道的计数；出口不在快照里时为零值
func egressLaneSnapshot(pool *AccountPool, key string, lane EgressLane) (EgressLaneStatus, bool) {
	for _, status := range pool.EgressSnapshot() {
		if status.Key == key {
			return status.Lanes[lane], true
		}
	}
	return EgressLaneStatus{}, false
}

// laneState 在锁内读出通道的令牌桶参数（计时器回调可能同时在锁内读写通道）
func laneState(pool *AccountPool, rate *egressRate) (tat int64, interval int64, burst int, limit int) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return rate.tat, rate.interval, rate.burst, rate.limit
}

// drainEgress 让通道在接下来 hold 内对两类选择都不准入（tat 推到 now+τ+hold），只供测试
func drainEgress(pool *AccountPool, rate *egressRate, hold time.Duration) {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	now := time.Now().UnixNano()
	rate.tat = now + int64(rate.burst-1)*rate.interval + int64(hold)
	pool.egressArmLocked(rate, now)
}

// egressSim 在假时钟上直接驱动一条通道：准入、预占（消耗令牌）、记发送，都在账户池锁内按给定时刻计算
type egressSim struct {
	pool    *AccountPool
	account *Account
	rate    *egressRate
}

// newEgressSim 返回限速为 limit（每分钟）的直连普通通道，令牌桶在假时刻 start 从空桶开始（与 SetEgressRateLimit 的 0→L 相同）
func newEgressSim(t *testing.T, limit int, start int64) *egressSim {
	t.Helper()
	pool := newEgressTestPool(1, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, limit)
	sim := &egressSim{pool: pool, account: pool.byID["a@example.com"], rate: egressLane(pool, "", EgressLaneNormal)}
	pool.mu.Lock()
	sim.rate.tat = start + int64(sim.rate.burst-1)*sim.rate.interval
	pool.mu.Unlock()
	return sim
}

// admits 判断假时刻 now 这一类选择能否取得令牌
func (sim *egressSim) admits(now int64, primary bool) bool {
	sim.pool.mu.Lock()
	defer sim.pool.mu.Unlock()
	ok, _ := sim.pool.egressAdmitLocked(sim.rate, primary, now)
	return ok
}

// take 在假时刻 now 尝试取得令牌：准入时预占（消耗令牌），send 为真时随即记一次发送；返回是否取得以及租约
func (sim *egressSim) take(now int64, primary bool, send bool) (*AccountLease, bool) {
	sim.pool.mu.Lock()
	defer sim.pool.mu.Unlock()
	if ok, _ := sim.pool.egressAdmitLocked(sim.rate, primary, now); !ok {
		return nil, false
	}
	lease := &AccountLease{pool: sim.pool, account: sim.account, egress: sim.rate}
	sim.pool.reserveEgressLocked(lease, time.Unix(0, now), primary)
	if send {
		sim.pool.recordEgressSentLocked(lease, now)
	}
	return lease, true
}

// maxInWindow 返回升序时刻里任意长度为 window 的半开窗口内的最多个数
func maxInWindow(stamps []int64, window int64) int {
	most := 0
	for start := range stamps {
		end := sort.Search(len(stamps), func(i int) bool { return stamps[i] >= stamps[start]+window })
		most = max(most, end-start)
	}
	return most
}

// TestEgressKeyNormalize 出口键：大小写、socks5h、补 scheme、去密码留用户名、直连与解析失败；标签不含用户名与密码原文、带 #哈希；编号稳定
func TestEgressKeyNormalize(t *testing.T) {
	for _, test := range []struct{ proxy, key string }{
		{"", ""},
		{"\t ", ""},
		{"HTTP://Proxy.Example:8080", "http://proxy.example:8080"},
		{"http://proxy.example:8080/", "http://proxy.example:8080"},
		{"Proxy.Example:3128", "http://proxy.example:3128"},
		{"SOCKS5H://GW.example:1080", "socks5://gw.example:1080"},
		{"socks5://user-1:p%40ss@gw.example:1080", "socks5://user-1@gw.example:1080"},
		{"http://alice:@proxy.example:8080", "http://alice@proxy.example:8080"},
		{"  http://:8080  ", "http://:8080"},
		{"://bad", "://bad"},
	} {
		if got := EgressKey(test.proxy); got != test.key {
			t.Fatalf("EgressKey(%q) = %q，期望 %q", test.proxy, got, test.key)
		}
	}
	// 同一用户名、不同密码与写法是同一出口；不同用户名（会话）是不同出口
	if EgressKey("socks5h://alice:one@GW.example:1080") != EgressKey("socks5://alice:two@gw.example:1080") {
		t.Fatal("同一网关同一用户名应归到同一出口")
	}
	if EgressKey("http://session-a:x@gw.example:1080") == EgressKey("http://session-b:x@gw.example:1080") {
		t.Fatal("同一网关不同用户名应是不同出口")
	}

	for _, proxy := range []string{"socks5://alice:secret@gw.example:1080", "http://user-1:p%40ss@proxy.example:8080"} {
		label := EgressLabel(EgressKey(proxy))
		base, hash, found := strings.Cut(label, "#")
		if !found || len(hash) != 4 || strings.Contains(base, "@") {
			t.Fatalf("带用户名的标签 %q 应为 scheme://host:port#4 位哈希", label)
		}
		for _, secret := range []string{"alice", "secret", "user-1", "p@ss", "p%40ss"} {
			if strings.Contains(label, secret) {
				t.Fatalf("标签 %q 不应包含用户名或密码原文 %q", label, secret)
			}
		}
	}
	if label := EgressLabel(EgressKey("proxy.example:3128")); label != "http://proxy.example:3128" {
		t.Fatalf("无用户名的标签 = %q", label)
	}
	if label := EgressLabel(""); label != "直连" {
		t.Fatalf("直连标签 = %q", label)
	}
	for _, raw := range []string{"://bad", "http://:8080"} {
		label := EgressLabel(EgressKey(raw))
		if !strings.HasPrefix(label, "<无法识别的代理>#") || len(label) != len("<无法识别的代理>#")+4 || strings.Contains(label, "bad") {
			t.Fatalf("解析失败的标签 = %q，不应包含原文", label)
		}
	}

	key := EgressKey("socks5://alice:secret@gw.example:1080")
	if id := EgressID(key); len(id) != 12 || id != EgressID(EgressKey("SOCKS5H://alice:other@GW.EXAMPLE:1080")) ||
		strings.Contains(id, "alice") || id == EgressID("") {
		t.Fatalf("EgressID = %q，应稳定、12 位、不含代理原文", id)
	}
}

// TestEgressBucketRateAndBurst 限速 L、桶容量 B：贪心取令牌 10 分钟，任意 60 秒窗口内准入不超过 L+B，平均速率为 L
func TestEgressBucketRateAndBurst(t *testing.T) {
	const limit = 30
	start := time.Now().UnixNano()
	sim := newEgressSim(t, limit, start)
	_, interval, burst, _ := laneState(sim.pool, sim.rate)
	if burst != 3 || interval != int64(2*time.Second) {
		t.Fatalf("限速 30 的桶容量 %d、间隔 %s，期望 3 与 2s", burst, time.Duration(interval))
	}
	var admitted []int64
	end := start + int64(10*time.Minute)
	for now := start; now < end; now += int64(10 * time.Millisecond) {
		for {
			if _, ok := sim.take(now, true, true); !ok {
				break
			}
			admitted = append(admitted, now)
		}
	}
	if most := maxInWindow(admitted, int64(time.Minute)); most > limit+burst {
		t.Fatalf("任意 60 秒窗口内最多准入 %d 次，超过 L+B=%d", most, limit+burst)
	}
	if perMinute := float64(len(admitted)) / 10; math.Abs(perMinute-limit) > 1 {
		t.Fatalf("平均每分钟准入 %.1f 次，期望约 %d", perMinute, limit)
	}
}

// TestEgressEnterFromEmptyBucketNoStall 窗口里已有 168 次/分钟的发送时打开限速 25：第一个令牌在一个间隔内放行，
// 第一分钟就按限速均匀放行（滑动日志要等窗口里只剩 L−1 条才放行，约停 50 秒）
func TestEgressEnterFromEmptyBucketNoStall(t *testing.T) {
	pool := newEgressTestPool(1, "a@example.com")
	rate := egressLane(pool, "", EgressLaneNormal)
	base := time.Now().UnixNano()
	pool.mu.Lock()
	for i := range 168 {
		at := base - int64(55*time.Second) + int64(i)*int64(55*time.Second)/168
		rate.sent.add(at, true, at-int64(time.Minute))
	}
	pool.mu.Unlock()
	pool.SetEgressRateLimit("", EgressLaneNormal, 25)
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Sent1m != 168 || status.Limit != 25 || status.Burst != 3 {
		t.Fatalf("打开限速后快照 = %+v，期望窗口内 168 次发送、限速 25、桶容量 3", status)
	}
	sim := &egressSim{pool: pool, account: pool.byID["a@example.com"], rate: rate}
	opened := time.Now().UnixNano()
	var first int64
	count := 0
	for now := opened; now < opened+int64(time.Minute); now += int64(10 * time.Millisecond) {
		if _, ok := sim.take(now, true, true); ok {
			if count == 0 {
				first = now
			}
			count++
		}
	}
	_, interval, burst, _ := laneState(pool, rate)
	if wait := time.Duration(first - opened); wait > time.Duration(interval)+10*time.Millisecond {
		t.Fatalf("第一个令牌在打开限速 %s 后，期望不超过一个间隔 %s", wait, time.Duration(interval))
	}
	if count < 24 || count > 25+burst {
		t.Fatalf("打开限速后的第一分钟放行 %d 次，期望约 25 次（没有停顿）", count)
	}
}

// TestEgressRefundOnReleaseWithoutSend 取得生成租约即消耗令牌；没有发出就释放时退还令牌与预占，发出后释放不退还
func TestEgressRefundOnReleaseWithoutSend(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	first := mustTryAcquire(t, pool, egressPrimarySelection)
	if lease, waitable, _ := pool.TryAcquireFor(context.Background(), egressPrimarySelection); lease != nil || !waitable {
		t.Fatalf("令牌用完后第二个租约 = %v（可等待=%v），期望取不到且可等待", lease != nil, waitable)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Pending != 1 || status.NextAt.IsZero() {
		t.Fatalf("预占后快照 = %+v，期望预占 1、下一个令牌时刻非零", status)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Pending != 0 || !status.NextAt.IsZero() {
		t.Fatalf("没有发出就释放后快照 = %+v，期望预占 0、令牌已退还", status)
	}
	second := mustTryAcquire(t, pool, egressPrimarySelection)
	second.RecordGenerationSent()
	if err := second.Release(); err != nil {
		t.Fatal(err)
	}
	status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
	if status.Pending != 0 || status.Sent1m != 1 || status.PrimarySent1m != 1 || status.NextAt.IsZero() {
		t.Fatalf("发出后释放的快照 = %+v，期望预占 0、发送 1（主用 1）、令牌不退还", status)
	}
	if lease, _, _ := pool.TryAcquireFor(context.Background(), egressPrimarySelection); lease != nil {
		t.Fatal("发出的租约释放后不应退还令牌")
	}
}

// TestEgressRefundNotifies 令牌桶已满时等待的选号在没有发出的租约释放、令牌退还后被唤醒并取得租约
func TestEgressRefundNotifies(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	held := mustTryAcquire(t, pool, egressPrimarySelection)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	acquired := make(chan *AccountLease, 1)
	go func() {
		lease, err := pool.AcquireFor(ctx, egressPrimarySelection)
		if err == nil {
			acquired <- lease
		}
	}()
	select {
	case lease := <-acquired:
		_ = lease.Release()
		t.Fatal("令牌用完时不应取得租约")
	case <-time.After(100 * time.Millisecond):
	}
	changed := pool.Changed()
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("退还令牌后应唤醒等待方")
	}
	select {
	case lease := <-acquired:
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("退还令牌后等待的选号应取得租约")
	}
}

// TestEgressPendingDoesNotBlock 6 个预占迟迟不发出、限速 6：预占只在准入时消耗令牌，下一分钟照常按限速放行
func TestEgressPendingDoesNotBlock(t *testing.T) {
	start := time.Now().UnixNano()
	sim := newEgressSim(t, 6, start)
	step := int64(100 * time.Millisecond)
	now, pending := start, 0
	for ; pending < 6 && now < start+int64(2*time.Minute); now += step {
		if _, ok := sim.take(now, true, false); ok {
			pending++
		}
	}
	if status, _ := egressLaneSnapshot(sim.pool, "", EgressLaneNormal); pending != 6 || status.Pending != 6 {
		t.Fatalf("预占 %d 个（通道计数 %d），期望 6", pending, status.Pending)
	}
	next := 0
	for end := now + int64(time.Minute); now < end; now += step {
		if _, ok := sim.take(now, true, false); ok {
			next++
		}
	}
	if next < 5 || next > 8 {
		t.Fatalf("6 个预占没有发出时下一分钟放行 %d 次，期望约 6 次", next)
	}
}

// TestEgressPriorityReserve 主用与非主用同时等待时主用先得令牌（非主用要多等一个令牌）；只有非主用的需求时速率不损失
func TestEgressPriorityReserve(t *testing.T) {
	start := time.Now().UnixNano()
	sim := newEgressSim(t, 30, start)
	_, interval, _, _ := laneState(sim.pool, sim.rate)
	// 空桶：主用的预留令牌立即可用，非主用要等一个间隔
	if !sim.admits(start, true) || sim.admits(start, false) {
		t.Fatal("空桶时应只有主用模型可以取得预留的令牌")
	}
	if !sim.admits(start+interval, false) {
		t.Fatal("一个间隔后非主用模型应取得令牌")
	}
	// 两类都在等：主用先得，非主用在主用之后再等一个间隔
	if _, ok := sim.take(start, true, true); !ok {
		t.Fatal("主用模型应取得预留的令牌")
	}
	// 记一次非主用发送，让非主用占比不低于保底（只看优先级本身）
	sim.pool.mu.Lock()
	sim.pool.recordEgressSentLocked(&AccountLease{pool: sim.pool, account: sim.account, egress: sim.rate}, start)
	sim.pool.mu.Unlock()
	at := start + interval
	if !sim.admits(at, true) || sim.admits(at, false) {
		t.Fatal("令牌到时应先给主用模型")
	}
	if _, ok := sim.take(at, true, true); !ok {
		t.Fatal("主用模型应取得令牌")
	}
	if sim.admits(at+interval, false) || !sim.admits(at+interval, true) {
		t.Fatal("主用一直在等时，下一个令牌仍先给主用模型")
	}

	// 只有非主用的需求：照样按限速取得令牌
	only := newEgressSim(t, 30, start)
	count := 0
	for now := start; now < start+int64(5*time.Minute); now += int64(10 * time.Millisecond) {
		if _, ok := only.take(now, false, true); ok {
			count++
		}
	}
	if perMinute := float64(count) / 5; math.Abs(perMinute-30) > 1 {
		t.Fatalf("只有非主用需求时每分钟取得 %.1f 个令牌，期望约 30", perMinute)
	}
}

// TestEgressNonPrimaryShareFloor 主用需求持续大于限速、每次都先于非主用复查：非主用仍按保底取得约 25% 的令牌，总速率不损失
func TestEgressNonPrimaryShareFloor(t *testing.T) {
	const limit = 24
	start := time.Now().UnixNano()
	sim := newEgressSim(t, limit, start)
	var primary, other []int64
	end := start + int64(10*time.Minute)
	for now := start; now < end; now += int64(10 * time.Millisecond) {
		for {
			if _, ok := sim.take(now, true, true); ok {
				primary = append(primary, now)
				continue
			}
			if _, ok := sim.take(now, false, true); ok {
				other = append(other, now)
				continue
			}
			break
		}
	}
	total := len(primary) + len(other)
	t.Logf("主用 %d、非主用 %d", len(primary), len(other))
	if perMinute := float64(total) / 10; math.Abs(perMinute-limit) > 1 {
		t.Fatalf("两类合计每分钟 %.1f 个令牌，期望约 %d", perMinute, limit)
	}
	if share := float64(len(other)) / float64(total); share < 0.23 || share > 0.4 {
		t.Fatalf("非主用取得 %d/%d（%.2f）个令牌，期望约 25%% 的保底", len(other), total, share)
	}
	// 第一分钟之后的每个整分钟里非主用都有份
	for minute := int64(1); minute < 10; minute++ {
		from, to := start+minute*int64(time.Minute), start+(minute+1)*int64(time.Minute)
		count := sort.Search(len(other), func(i int) bool { return other[i] >= to }) -
			sort.Search(len(other), func(i int) bool { return other[i] >= from })
		if count < limit/5 {
			t.Fatalf("第 %d 分钟非主用只取得 %d 个令牌", minute+1, count)
		}
	}
}

// TestEgressConcurrentNoOversubscribe 50 个 goroutine 并发取生成租约、发送、释放，限速 5、桶容量 2：
// 任意一个窗口内取得的生成租约不超过 L+B=7（统计窗口缩短到 600ms，令牌间隔 120ms）
func TestEgressConcurrentNoOversubscribe(t *testing.T) {
	ids := make([]string, 0, 20)
	for i := range 20 {
		ids = append(ids, fmt.Sprintf("a%02d@example.com", i))
	}
	pool := newEgressTestPool(4, ids...)
	const window = 600 * time.Millisecond
	pool.mu.Lock()
	pool.egressWindow = window
	pool.mu.Unlock()
	pool.SetEgressRateLimit("", EgressLaneNormal, 5)
	if _, _, burst, _ := laneState(pool, egressLane(pool, "", EgressLaneNormal)); burst != 2 {
		t.Fatalf("桶容量 %d，期望 2", burst)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	var mu sync.Mutex
	var stamps []int64
	var wg sync.WaitGroup
	for worker := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			selection := egressTestSelection
			selection.EgressPriority = worker%2 == 0
			for ctx.Err() == nil {
				lease, err := pool.AcquireFor(ctx, selection)
				if err != nil {
					return
				}
				mu.Lock()
				stamps = append(stamps, lease.CheckedAt().UnixNano())
				mu.Unlock()
				lease.RecordGenerationSent()
				if err := lease.Release(); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	slices.Sort(stamps)
	if len(stamps) < 10 {
		t.Fatalf("2.5 秒内只取得 %d 个租约，期望按限速持续放行", len(stamps))
	}
	t.Logf("取得 %d 个租约，任意窗口最多 %d", len(stamps), maxInWindow(stamps, int64(window)))
	if most := maxInWindow(stamps, int64(window)); most > 7 {
		t.Fatalf("任意窗口内最多取得 %d 个生成租约，超过 L+B=7", most)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Pending != 0 {
		t.Fatalf("全部释放后预占 %d，期望 0", status.Pending)
	}
}

// TestEgressFullClassifiedBusy 所在出口通道令牌桶已满的候选按忙碌分组（热账户进 WarmBusy、备用进 StandbyBusy）并计数，
// 索引路径与逐个账户判断的路径结果一致；主用模型还有预留令牌时不受限
func TestEgressFullClassifiedBusy(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com", "b@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	warm := []string{"a@example.com"}
	classify := func(selection AccountSelection) (AccountCandidateGroups, AccountCandidateGroups) {
		pool.mu.Lock()
		defer pool.mu.Unlock()
		now := time.Now()
		indexed, err := pool.classifyCandidatesAtLocked(selection, warm, now)
		if err != nil {
			t.Fatal(err)
		}
		brute, err := pool.classifyCandidatesBruteLocked(selection, warm, now)
		if err != nil {
			t.Fatal(err)
		}
		if !sameGroups(indexed, brute) {
			t.Fatalf("索引分类 %+v 与逐个判断 %+v 不一致", indexed, brute)
		}
		return indexed, brute
	}
	// 空桶：主用有预留令牌，非主用不准入
	if groups, _ := classify(egressPrimarySelection); groups.EgressLimited != 0 || len(groups.WarmReady) != 1 {
		t.Fatalf("主用模型分类 = %+v，期望不受出口限速", groups)
	}
	groups, _ := classify(egressTestSelection)
	if groups.EgressLimited != 2 || groups.WarmEgressLimited != 1 || !slices.Equal(groups.WarmBusy, warm) ||
		!slices.Equal(groups.StandbyBusy, []string{"b@example.com"}) || len(groups.WarmReady)+len(groups.StandbyReady) != 0 {
		t.Fatalf("非主用模型分类 = %+v，期望两个候选都按忙碌分组且计出口已满", groups)
	}
	// 限定账户集合走候选集的另一条路径
	allowed := egressTestSelection
	allowed.AllowedAccountIDs = []string{"b@example.com"}
	if groups, _ := classify(allowed); groups.EgressLimited != 1 || groups.WarmEgressLimited != 0 || len(groups.StandbyBusy) != 1 {
		t.Fatalf("限定账户的分类 = %+v，期望备用账户按忙碌分组", groups)
	}
	// 每分钟限速关闭的通道与非生成选择不受影响
	if groups, _ := classify(AccountSelection{ModelID: indexTestModel, Method: "countTokens"}); groups.EgressLimited != 0 {
		t.Fatalf("非生成选择分类 = %+v，不应受出口限速", groups)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 0)
	if groups, _ := classify(egressTestSelection); groups.EgressLimited != 0 || len(groups.WarmReady) != 1 {
		t.Fatalf("关闭限速后分类 = %+v，期望不受限", groups)
	}
}

// TestEgressWindowReleaseNotifies 令牌桶已满时，下一个令牌到时由窗口释放计时器唤醒等待方，不需要别的事件
func TestEgressWindowReleaseNotifies(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	pool.mu.Lock()
	pool.egressWindow = 600 * time.Millisecond
	pool.mu.Unlock()
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	rate := egressLane(pool, "", EgressLaneNormal)
	drainEgress(pool, rate, 150*time.Millisecond)
	changed := pool.Changed()
	started := time.Now()
	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("下一个令牌到时应唤醒等待方")
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("令牌到时之前（%s）就唤醒了", elapsed)
	}
	mustTryAcquire(t, pool, egressPrimarySelection).Release()
}

// TestSetEgressRateLimitRaiseWakes 调大或关闭限速时立即唤醒等待方并放行；调小不唤醒
func TestSetEgressRateLimitRaiseWakes(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	rate := egressLane(pool, "", EgressLaneNormal)
	drainEgress(pool, rate, time.Minute)
	if lease, _, _ := pool.TryAcquireFor(context.Background(), egressPrimarySelection); lease != nil {
		t.Fatal("令牌桶已满时不应取得租约")
	}
	changed := pool.Changed()
	pool.SetEgressRateLimit("", EgressLaneNormal, 3)
	select {
	case <-changed:
		t.Fatal("调小限速不应唤醒等待方")
	default:
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 6000)
	select {
	case <-changed:
	default:
		t.Fatal("调大限速应唤醒等待方")
	}
	mustTryAcquire(t, pool, egressPrimarySelection).Release()
	drainEgress(pool, rate, time.Minute)
	changed = pool.Changed()
	pool.SetEgressRateLimit("", EgressLaneNormal, 0)
	select {
	case <-changed:
	default:
		t.Fatal("关闭限速应唤醒等待方")
	}
	mustTryAcquire(t, pool, egressPrimarySelection).Release()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if _, armed := pool.egressFull[rate]; len(pool.egressThrottled) != 0 || rate.tat != 0 || armed {
		t.Fatalf("关闭限速后限速通道数 %d、tat %d、仍登记=%v，期望都清除", len(pool.egressThrottled), rate.tat, armed)
	}
}

// TestSetEgressRateLimitRescale 0→L 从空桶开始；L→L′ 剩余欠量按 L/L′ 缩放；同值不变
func TestSetEgressRateLimitRescale(t *testing.T) {
	pool := newEgressTestPool(1, "a@example.com")
	before := time.Now().UnixNano()
	pool.SetEgressRateLimit("", EgressLaneNormal, 30)
	after := time.Now().UnixNano()
	rate := egressLane(pool, "", EgressLaneNormal)
	tat, interval, burst, _ := laneState(pool, rate)
	tau := int64(burst-1) * interval
	if tat < before+tau || tat > after+tau {
		t.Fatalf("0→30 后 tat 偏离 now+τ：%d 不在 [%d, %d]", tat, before+tau, after+tau)
	}
	pool.mu.Lock()
	now := time.Now().UnixNano()
	rate.tat = now + int64(10*time.Second)
	pool.mu.Unlock()
	pool.SetEgressRateLimit("", EgressLaneNormal, 60)
	tat, interval, burst, _ = laneState(pool, rate)
	if deficit := time.Duration(tat - now); deficit < 4900*time.Millisecond || deficit > 5100*time.Millisecond {
		t.Fatalf("30→60 后剩余欠量 %s，期望约 5s", deficit)
	}
	if interval != int64(time.Second) || burst != 6 {
		t.Fatalf("限速 60 的间隔 %s、桶容量 %d，期望 1s 与 6", time.Duration(interval), burst)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 60)
	if again, _, _, _ := laneState(pool, rate); again != tat {
		t.Fatal("同值设置不应改变令牌桶")
	}
	// 没有账户的出口先建立记录，账户挂上后即受限；关闭不存在的出口不建立记录
	pool.SetEgressRateLimit("http://proxy.example:8080", EgressLaneUltra, 0)
	if egressLane(pool, "http://proxy.example:8080", EgressLaneNormal) != nil {
		t.Fatal("关闭不存在的出口不应建立记录")
	}
	pool.SetEgressRateLimit("http://proxy.example:8080", EgressLaneUltra, 12)
	lane := egressLane(pool, "http://proxy.example:8080", EgressLaneUltra)
	if lane == nil {
		t.Fatal("设置没有账户的出口应建立记录")
	}
	if _, _, _, limit := laneState(pool, lane); limit != 12 {
		t.Fatalf("没有账户的出口限速 %d，期望 12", limit)
	}
}

// TestEgressLaneUltraUngated 普通通道限速满桶、Ultra 通道不限速：Ultra 账户照常取得租约，普通账户按出口限速等待
func TestEgressLaneUltraUngated(t *testing.T) {
	pool := NewAccountPool([]*Account{
		indexTestAccount("free@example.com", BenefitTierFree, indexTestCatalog()),
		indexTestAccount("ultra@example.com", BenefitTierUltra, indexTestCatalog()),
	}, 4)
	if key, lane := pool.EgressKeyOf("ultra@example.com"); key != "" || lane != EgressLaneUltra {
		t.Fatalf("Ultra 账户的出口 = (%q, %d)，期望直连的 Ultra 通道", key, lane)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, "", EgressLaneNormal), time.Minute)
	for range 5 {
		lease := mustTryAcquire(t, pool, egressTestSelection)
		if lease.Account().ID != "ultra@example.com" || lease.EgressLane() != EgressLaneUltra {
			t.Fatalf("普通通道满桶时取得了 %s（通道 %d），期望 Ultra 账户", lease.Account().ID, lease.EgressLane())
		}
		if err := lease.Release(); err != nil {
			t.Fatal(err)
		}
	}
	normal := egressTestSelection
	normal.Pool = PoolScopeNormal
	if lease, waitable, _ := pool.TryAcquireFor(context.Background(), normal); lease != nil || !waitable {
		t.Fatalf("普通号池选号 = %v（可等待=%v），期望按出口限速等待", lease != nil, waitable)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneUltra); status.Accounts != 1 || status.Limit != 0 {
		t.Fatalf("Ultra 通道快照 = %+v", status)
	}
}

// TestEgressReattachOnSaveConfig 代理变化时账户换出口通道；在途租约的预占记在原通道，释放时退还原通道
func TestEgressReattachOnSaveConfig(t *testing.T) {
	pool, accountID := testCatalogPool(t)
	const proxy = "http://Proxy.Example:8080"
	key := EgressKey(proxy)
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	lease, err := pool.AcquireFor(context.Background(), AccountSelection{AccountID: accountID, ModelID: testNoEligibleModel, Method: "generateContent"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.EgressKey() != "" {
		t.Fatalf("租约出口 = %q，期望直连", lease.EgressKey())
	}
	config := lease.Account().Config
	config.Proxy = proxy
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if got, _ := pool.EgressKeyOf(accountID); got != key {
		t.Fatalf("改代理后出口 = %q，期望 %q", got, key)
	}
	direct, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
	proxied, _ := egressLaneSnapshot(pool, key, EgressLaneNormal)
	if direct.Accounts != 0 || direct.Pending != 1 || proxied.Accounts != 1 || proxied.Pending != 0 {
		t.Fatalf("改代理后直连 %+v、代理 %+v，期望账户换到代理出口、预占留在直连", direct, proxied)
	}
	rate := egressLane(pool, "", EgressLaneNormal)
	charged, _, _, _ := laneState(pool, rate)
	if lease.EgressKey() != "" {
		t.Fatal("租约的出口在取得时固定，不随重挂改变")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	direct, _ = egressLaneSnapshot(pool, "", EgressLaneNormal)
	if refunded, _, _, _ := laneState(pool, rate); direct.Pending != 0 || refunded >= charged {
		t.Fatalf("释放后直连通道预占 %d、tat %d（释放前 %d），期望退还原通道", direct.Pending, refunded, charged)
	}
	verifyIndex(t, pool, "改代理后")
	// 直连出口已没有账户，但限速仍在（判定器设的），照样列出；代理出口按脱敏标签与编号列出
	snapshot := pool.EgressSnapshot()
	if len(snapshot) != 2 || snapshot[0].Key != "" || snapshot[0].Lanes[0].Limit != 6 ||
		snapshot[1].Label != "http://proxy.example:8080" || snapshot[1].ID != EgressID(key) {
		t.Fatalf("快照 = %+v，期望直连（限速 6、没有账户）与代理出口", snapshot)
	}
}

// TestEgressReattachOnUltraMembership 权益变化与 ULTRA_INCLUDE_PRO 切换后账户换通道，各通道账户数一致
func TestEgressReattachOnUltraMembership(t *testing.T) {
	pool := NewAccountPool([]*Account{
		indexTestAccount("free@example.com", BenefitTierFree, indexTestCatalog()),
		indexTestAccount("pro@example.com", BenefitTierPro, indexTestCatalog()),
	}, 2)
	// 先建索引，覆盖增量更新
	verifyIndex(t, pool, "初始")
	if _, err := pool.ClassifyCandidates(context.Background(), egressTestSelection, nil); err != nil {
		t.Fatal(err)
	}
	counts := func() (int, int) {
		normal, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
		ultra, _ := egressLaneSnapshot(pool, "", EgressLaneUltra)
		return normal.Accounts, ultra.Accounts
	}
	if normal, ultra := counts(); normal != 2 || ultra != 0 {
		t.Fatalf("初始通道账户数 普通=%d Ultra=%d", normal, ultra)
	}
	pool.SetUltraIncludePro(true)
	verifyIndex(t, pool, "并入 Pro")
	if normal, ultra := counts(); normal != 1 || ultra != 1 {
		t.Fatalf("并入 Pro 后通道账户数 普通=%d Ultra=%d", normal, ultra)
	}
	if _, lane := pool.EgressKeyOf("pro@example.com"); lane != EgressLaneUltra {
		t.Fatal("并入 Pro 后 Pro 账户应在 Ultra 通道")
	}
	if _, err := pool.ClassifyCandidates(context.Background(), egressTestSelection, nil); err != nil {
		t.Fatal(err)
	}
	if err := pool.SetCatalog("free@example.com", BenefitTierUltra, indexTestCatalog()); err != nil {
		t.Fatal(err)
	}
	verifyIndex(t, pool, "Free 升为 Ultra")
	if normal, ultra := counts(); normal != 0 || ultra != 2 {
		t.Fatalf("Free 升为 Ultra 后通道账户数 普通=%d Ultra=%d", normal, ultra)
	}
	pool.SetUltraIncludePro(false)
	verifyIndex(t, pool, "关闭并入")
	if normal, ultra := counts(); normal != 1 || ultra != 1 {
		t.Fatalf("关闭并入后通道账户数 普通=%d Ultra=%d", normal, ultra)
	}
}

// TestEgressDetachOnBulkRemove 批量删除（AccountReservation.Remove）与 Remove 都把账户从出口通道摘下，账户数不漂移
func TestEgressDetachOnBulkRemove(t *testing.T) {
	pool := testPoolWithAccount(t, reservationTestAccount)
	store := NewAccountStore(t.TempDir())
	other, publish, err := store.Create(DefaultAccountConfig("bob@example.com"), testStorageState("B"))
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Add(other); err != nil {
		t.Fatal(err)
	}
	if err := publish.Release(); err != nil {
		t.Fatal(err)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Accounts != 2 {
		t.Fatalf("加入后直连账户数 %d，期望 2", status.Accounts)
	}
	reservation, err := pool.ReserveAccount(reservationTestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reservation.Remove(context.Background(), func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Accounts != 1 {
		t.Fatalf("批量删除后直连账户数 %d，期望 1", status.Accounts)
	}
	if _, err := pool.Remove("bob@example.com", func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, listed := egressLaneSnapshot(pool, "", EgressLaneNormal); listed {
		t.Fatal("没有账户、没有限速与发送的出口不应出现在快照里")
	}
	verifyIndex(t, pool, "删除后")
}

// TestEgressPinnedSelectionBypass 指定账户的选号（管理页测试调用）不受出口限速但照常预占；同一账户的不固定选号照拦
func TestEgressPinnedSelectionBypass(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	rate := egressLane(pool, "", EgressLaneNormal)
	drainEgress(pool, rate, time.Minute)
	tat, interval, _, _ := laneState(pool, rate)
	pinned := egressTestSelection
	pinned.AccountID = "a@example.com"
	lease := mustTryAcquire(t, pool, pinned)
	after, _, _, _ := laneState(pool, rate)
	if status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal); status.Pending != 1 || after != tat+interval {
		t.Fatalf("指定账户的租约预占 %d、tat 前移 %s，期望预占 1、消耗一个令牌", status.Pending, time.Duration(after-tat))
	}
	if groups, err := pool.ClassifyCandidates(context.Background(), pinned, nil); err != nil || groups.EgressLimited != 0 {
		t.Fatalf("指定账户的分类 = %+v %v，不应计出口限速", groups, err)
	}
	if other, _, _ := pool.TryAcquireFor(context.Background(), egressTestSelection); other != nil {
		t.Fatal("不固定账户的选号应按出口限速等待")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestEgressFileBoundGated 文件绑定的选号照拦：所属账户所在通道满桶时取不到租约、分类按忙碌并计数
func TestEgressFileBoundGated(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com", "b@example.com")
	if err := pool.BindResource("files/egress-test", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, "", EgressLaneNormal), time.Minute)
	bound := egressTestSelection
	bound.ResourceID = "files/egress-test"
	if lease, waitable, err := pool.TryAcquireFor(context.Background(), bound); lease != nil || !waitable || err != nil {
		t.Fatalf("文件绑定的选号 = %v（可等待=%v）%v，期望按出口限速等待", lease != nil, waitable, err)
	}
	groups, err := pool.ClassifyCandidates(context.Background(), bound, []string{"a@example.com"})
	if err != nil || groups.EgressLimited != 1 || groups.WarmEgressLimited != 1 || len(groups.WarmBusy) != 1 {
		t.Fatalf("文件绑定的分类 = %+v %v，期望所属账户按忙碌分组并计出口已满", groups, err)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 0)
	mustTryAcquire(t, pool, bound).Release()
}

// TestEgressShortCircuitAllFull 1 万个账户都在同一个满桶的出口上：选号不逐个尝试候选，直接返回可等待与下一个令牌的时刻
func TestEgressShortCircuitAllFull(t *testing.T) {
	pool := newEgressFullPool(10000)
	pool.mu.Lock()
	pool.leaseAttempts = 0
	now := time.Now()
	lease, earliest, waitable, err := pool.tryAcquireLocked(egressTestSelection, now)
	attempts := pool.leaseAttempts
	pool.mu.Unlock()
	if lease != nil || !waitable || err != nil || earliest.IsZero() || earliest.Before(now) {
		t.Fatalf("满桶时 tryAcquireLocked = (%v, %s, %v, %v)，期望可等待且最早时刻为下一个令牌", lease != nil, earliest, waitable, err)
	}
	if attempts != 0 {
		t.Fatalf("满桶时逐个尝试了 %d 个候选，期望直接短路", attempts)
	}
	if lease, waitable, _ := pool.TryAcquireFor(context.Background(), egressTestSelection); lease != nil || !waitable {
		t.Fatal("满桶时 TryAcquireFor 应取不到且可等待")
	}
	groups, err := pool.ClassifyCandidates(context.Background(), egressTestSelection, nil)
	if err != nil || groups.EgressLimited != 10000 || len(groups.StandbyBusy) != 10000 {
		t.Fatalf("满桶时分类出口已满 %d、备用忙碌 %d（%v），期望都为 10000", groups.EgressLimited, len(groups.StandbyBusy), err)
	}
	// 另一个出口还有令牌时不短路
	extra := indexTestAccount("zz@example.com", BenefitTierFree, indexTestCatalog())
	extra.Config.Proxy = "http://proxy.example:8080"
	if err := pool.Add(extra); err != nil {
		t.Fatal(err)
	}
	lease = mustTryAcquire(t, pool, egressTestSelection)
	if lease.Account().ID != "zz@example.com" || lease.EgressKey() != "http://proxy.example:8080" {
		t.Fatalf("取得了 %s（出口 %q），期望未受限出口上的账户", lease.Account().ID, lease.EgressKey())
	}
	_ = lease.Release()
}

// TestEgressGateOnlyThrottledLanes 门控只为限速通道算准入（不按出口总数）：很多个账号级代理、只有一个出口限速时，
// 门控里只有那条满桶通道，其余出口照常准入；全部有账户的通道都满桶才短路；关闭限速后门控关闭
func TestEgressGateOnlyThrottledLanes(t *testing.T) {
	accounts := make([]*Account, 0, 200)
	catalog := indexTestCatalog()
	for i := range 200 {
		account := indexTestAccount(fmt.Sprintf("gate-%03d@example.com", i), BenefitTierFree, catalog)
		account.Config.Proxy = fmt.Sprintf("http://proxy-%03d.example:8080", i)
		accounts = append(accounts, account)
	}
	pool := NewAccountPool(accounts, 1)
	limited := "http://proxy-007.example:8080"
	pool.SetEgressRateLimit(limited, EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, limited, EgressLaneNormal), time.Hour)
	pool.mu.Lock()
	if err := pool.verifyEgressLocked(); err != nil {
		pool.mu.Unlock()
		t.Fatal(err)
	}
	gate := pool.egressGateLocked(egressTestSelection, time.Now())
	_, allBlocked := pool.egressAllBlockedLocked(gate)
	blockedRate := pool.egresses[limited].lanes[EgressLaneNormal]
	otherRate := pool.egresses["http://proxy-008.example:8080"].lanes[EgressLaneNormal]
	pool.mu.Unlock()
	if !gate.enabled || len(gate.blocked) != 1 || gate.blockedPopulated != 1 {
		t.Fatalf("门控 = %+v，期望只有一条满桶通道", gate)
	}
	if gate.admits(blockedRate) || !gate.admits(otherRate) || !gate.admits(nil) {
		t.Fatal("满桶通道应不准入，其他出口与没有通道的账户应准入")
	}
	if allBlocked {
		t.Fatal("还有不限速的出口时不应按全部满桶短路")
	}
	lease := mustTryAcquire(t, pool, egressTestSelection)
	if lease.EgressKey() == limited {
		t.Fatalf("取得了满桶出口上的账户 %s", lease.Account().ID)
	}
	_ = lease.Release()

	// 只剩满桶出口上的账户（其余账户删除）：有账户的通道都满桶，短路
	for i := range 200 {
		if i == 7 {
			continue
		}
		if _, err := pool.Remove(fmt.Sprintf("gate-%03d@example.com", i), func(*Account) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	if err := pool.verifyEgressLocked(); err != nil {
		pool.mu.Unlock()
		t.Fatal(err)
	}
	gate = pool.egressGateLocked(egressTestSelection, time.Now())
	next, allBlocked := pool.egressAllBlockedLocked(gate)
	pool.mu.Unlock()
	if !allBlocked || next <= time.Now().UnixNano() {
		t.Fatalf("只剩满桶通道有账户时短路 = %v（下一个令牌 %d），期望短路且时刻在之后", allBlocked, next)
	}
	pool.SetEgressRateLimit(limited, EgressLaneNormal, 0)
	pool.mu.Lock()
	gate = pool.egressGateLocked(egressTestSelection, time.Now())
	pool.mu.Unlock()
	if gate.enabled {
		t.Fatal("没有限速通道时门控应关闭")
	}
}

// newEgressFullPool 返回 n 个账户都在直连出口上、令牌桶已满（一小时内不准入）的账户池
func newEgressFullPool(n int) *AccountPool {
	accounts := make([]*Account, 0, n)
	catalog := indexTestCatalog()
	for i := range n {
		accounts = append(accounts, indexTestAccount(fmt.Sprintf("acct-%05d@example.com", i), BenefitTierFree, catalog))
	}
	pool := NewAccountPool(accounts, 1)
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, "", EgressLaneNormal), time.Hour)
	return pool
}

// BenchmarkTryAcquireEgressFull 单出口满桶、1 万个候选时一次不固定账户的选号（应为短路，不随候选数线性增长）
func BenchmarkTryAcquireEgressFull(b *testing.B) {
	pool := newEgressFullPool(10000)
	ctx := context.Background()
	if _, err := pool.ClassifyCandidates(ctx, egressTestSelection, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if lease, _, _ := pool.TryAcquireFor(ctx, egressTestSelection); lease != nil {
			b.Fatal("满桶时不应取得租约")
		}
	}
}

// TestEgressAcquireForWaitLimit 带 WithEgressWaitLimit 的 AcquireFor 只因出口满桶累计等待超过上限后返回“出口限速”的 CapacityBusyError；
// 没有上限时一直等；因账户忙碌的等待不计入上限
func TestEgressAcquireForWaitLimit(t *testing.T) {
	pool := newEgressTestPool(1, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, "", EgressLaneNormal), 10*time.Second)
	const limit = 200 * time.Millisecond
	started := time.Now()
	lease, err := pool.AcquireFor(WithEgressWaitLimit(context.Background(), limit), egressPrimarySelection)
	elapsed := time.Since(started)
	var busy *CapacityBusyError
	if lease != nil || !errors.As(err, &busy) {
		t.Fatalf("AcquireFor = %v %v，期望出口限速的 CapacityBusyError", lease != nil, err)
	}
	if busy.Reason != EgressBusyReason || busy.Limit != limit || busy.Waited < limit || busy.Attempt != 1 || busy.ModelID != indexTestModel {
		t.Fatalf("错误 = %+v，期望原因出口限速、上限 %s、已等待不少于上限", busy, limit)
	}
	if elapsed < limit || elapsed > 2*time.Second {
		t.Fatalf("返回用时 %s，期望略超过上限 %s", elapsed, limit)
	}
	if retry := time.Until(busy.Until); retry < 4*time.Second || retry > 30*time.Second {
		t.Fatalf("建议重试时间在 %s 后，期望限定在 [5s, 30s]", retry)
	}
	if !strings.HasPrefix(err.Error(), "出口限速：模型 "+indexTestModel+" 所在出口每分钟请求已满") || busy.HTTPStatus() != http.StatusTooManyRequests {
		t.Fatalf("错误文案 %q 或状态码 %d 不符", err.Error(), busy.HTTPStatus())
	}

	// 没有上限时一直等到 ctx 结束
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := pool.AcquireFor(ctx, egressPrimarySelection); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("没有出口等待上限时 AcquireFor = %v，期望等到 ctx 结束", err)
	}

	// 账户忙碌（并发已满）的等待不计入出口等待上限
	pool.SetEgressRateLimit("", EgressLaneNormal, 0)
	held := mustTryAcquire(t, pool, egressPrimarySelection)
	defer held.Release()
	ctx, cancel = context.WithTimeout(WithEgressWaitLimit(context.Background(), 50*time.Millisecond), 300*time.Millisecond)
	defer cancel()
	if _, err := pool.AcquireFor(ctx, egressPrimarySelection); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("账户忙碌时 AcquireFor = %v，期望等到 ctx 结束（不按出口限速返回）", err)
	}
}

// egressCountingTransport 记录调用次数并返回空的成功响应
type egressCountingTransport struct {
	mu    sync.Mutex
	calls []string
}

func (transport *egressCountingTransport) Do(_ context.Context, request RPCRequest) (*RPCResponse, error) {
	transport.mu.Lock()
	transport.calls = append(transport.calls, request.Method)
	transport.mu.Unlock()
	header := http.Header{}
	header.Set("Content-Type", JSONProtobufContentType)
	return &RPCResponse{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("[]"))}, nil
}

// TestEgressOtherSendCounted ctx 带租约的 Client.do 计入通道的非生成 RPC（OtherSent1m），不计入生成发送、不预占、满桶时也不拦
func TestEgressOtherSendCounted(t *testing.T) {
	pool := newEgressTestPool(2, "a@example.com")
	pool.SetEgressRateLimit("", EgressLaneNormal, 6)
	drainEgress(pool, egressLane(pool, "", EgressLaneNormal), time.Minute)
	transport := &egressCountingTransport{}
	client := &Client{transport: transport, catalogs: make(map[string]modelCatalog), tiers: make(map[string]BenefitTier)}
	lease := mustTryAcquire(t, pool, AccountSelection{AccountID: "a@example.com"})
	defer lease.Release()
	ctx := ContextWithAccountLease(context.Background(), lease)
	for _, method := range []string{"CountTokens", "ListModels"} {
		response, err := client.do(ctx, method, "a@example.com", "", []byte("[]"), false)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
	}
	// 不带租约的调用不计
	response, err := client.do(context.Background(), "ListModels", "a@example.com", "", []byte("[]"), false)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(transport.calls) != 3 {
		t.Fatalf("发出 %d 次，期望 3 次（非生成 RPC 不受出口限速）", len(transport.calls))
	}
	status, _ := egressLaneSnapshot(pool, "", EgressLaneNormal)
	if status.OtherSent1m != 2 || status.Sent1m != 0 || status.Pending != 0 {
		t.Fatalf("快照 = %+v，期望非生成 RPC 2 次、生成发送 0、预占 0", status)
	}
}

// TestSchedulingIndexEgressConsistency 增删、改代理、全局代理、号池规则与权益变化后，候选索引里的通道编号与账户字段一致，
// 各通道账户数与实际一致
func TestSchedulingIndexEgressConsistency(t *testing.T) {
	pool, accountID := testCatalogPool(t)
	ctx := context.Background()
	selection := AccountSelection{ModelID: testNoEligibleModel, Method: "generateContent"}
	step := func(name string) {
		t.Helper()
		if _, err := pool.ClassifyCandidates(ctx, selection, nil); err != nil {
			t.Fatal(err)
		}
		verifyIndex(t, pool, name)
	}
	step("初始")
	store := NewAccountStore(t.TempDir())
	var added []string
	for i, proxy := range []string{"", "socks5://GW.example:1080", "socks5://gw.example:1080/", "http://proxy.example:3128"} {
		config := DefaultAccountConfig(fmt.Sprintf("user%d@example.com", i))
		config.Proxy = proxy
		account, publish, err := store.Create(config, testStorageState("S"))
		if err != nil {
			t.Fatal(err)
		}
		if err := pool.Add(account); err != nil {
			t.Fatal(err)
		}
		if err := publish.Release(); err != nil {
			t.Fatal(err)
		}
		if err := pool.SetCatalog(account.ID, []BenefitTier{BenefitTierFree, BenefitTierPro, BenefitTierUltra, BenefitTierFree}[i],
			[]Model{{ID: testNoEligibleModel, Methods: []string{"generateContent"}}}); err != nil {
			t.Fatal(err)
		}
		added = append(added, account.ID)
		step("加入 " + account.ID)
	}
	if key, _ := pool.EgressKeyOf(added[2]); key != "socks5://gw.example:1080" {
		t.Fatalf("同一代理的两种写法应归到一个出口，得到 %q", key)
	}
	if status, _ := egressLaneSnapshot(pool, "socks5://gw.example:1080", EgressLaneNormal); status.Accounts != 1 {
		t.Fatalf("socks5 出口普通通道账户数 %d，期望 1（另一个是 Pro 但未并入）", status.Accounts)
	}
	pool.SetGlobalProxy("http://global.example:8080")
	step("设全局代理")
	if key, _ := pool.EgressKeyOf(accountID); key != "http://global.example:8080" {
		t.Fatalf("没有固定代理的账户出口 = %q，期望全局代理", key)
	}
	if key, _ := pool.EgressKeyOf(added[3]); key != "http://proxy.example:3128" {
		t.Fatalf("固定代理的账户出口 = %q，期望固定代理", key)
	}
	pool.SetUltraIncludePro(true)
	step("并入 Pro")
	lease, err := pool.AcquireAccount(ctx, added[3])
	if err != nil {
		t.Fatal(err)
	}
	config := lease.Account().Config
	config.Proxy = ""
	if err := lease.SaveConfig(config); err != nil {
		t.Fatal(err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	step("清除固定代理")
	if err := pool.SetCatalog(added[2], BenefitTierFree, []Model{{ID: testNoEligibleModel, Methods: []string{"generateContent"}}}); err != nil {
		t.Fatal(err)
	}
	step("Ultra 降为 Free")
	if _, err := pool.Remove(added[0], func(*Account) error { return nil }); err != nil {
		t.Fatal(err)
	}
	step("删除")
	pool.SetGlobalProxy("")
	step("清除全局代理")
}

// TestRelaxRateLimitCooldowns 撤回只缩短第 N≥minLevel 档、到期晚于目标的逐级限流冷却并清档位，原因前缀改写；
// 每日、分钟、403、按账号降级与低档的冷却都不动；扫描只列出该出口上的账户
func TestRelaxRateLimitCooldowns(t *testing.T) {
	now := time.Now()
	ladder := indexTestAccount("ladder@example.com", BenefitTierFree, indexTestCatalog())
	ladder.runtime.Cooldowns = map[string]CooldownState{
		indexTestModel:            {Until: now.Add(30 * time.Minute), Reason: "限流: 连续限流第 3 档，Quota exceeded"},
		"build:" + indexTestModel: {Until: now.Add(5 * time.Minute), Reason: "限流: 连续限流第 2 档，Quota exceeded（Build）"},
		indexTestFlash:            {Until: now.Add(30 * time.Second), Reason: "限流: 连续限流第 1 档，Quota exceeded"},
		indexTestImage:            {Until: now.Add(5 * time.Hour), Reason: "每日限额: 每日请求数已用完"},
		indexTestUltraOnly:        {Until: now.Add(30 * time.Minute), Reason: "限流: 出口限流期间账号持续被拒（连续 6 次），Quota exceeded"},
		"count-tokens:x":          {Until: now.Add(time.Hour), Reason: "无权限: 403"},
	}
	ladder.rateStrikes = map[rateStrikeKey]rateStrike{
		{model: indexTestModel}:                           {level: 3, until: now.Add(30 * time.Minute)},
		{model: indexTestModel, group: ChannelBuild}:      {level: 2, until: now.Add(5 * time.Minute)},
		{model: indexTestFlash}:                           {level: 1, until: now.Add(30 * time.Second)},
		{model: indexTestImage, group: ChannelBuild}:      {level: 1},
		{model: indexTestFlash, group: ChannelBuild}:      {level: 1},
		{model: indexTestModel, group: ChannelPlayground}: {level: 2},
	}
	healthy := indexTestAccount("healthy@example.com", BenefitTierFree, indexTestCatalog())
	proxied := indexTestAccount("proxied@example.com", BenefitTierFree, indexTestCatalog())
	proxied.Config.Proxy = "http://proxy.example:8080"
	proxied.runtime.Cooldowns = map[string]CooldownState{
		indexTestModel: {Until: now.Add(30 * time.Minute), Reason: "限流: 连续限流第 4 档，Quota exceeded"},
	}
	pool := NewAccountPool([]*Account{ladder, healthy, proxied}, 1)
	if got := pool.RateLadderAccounts("", 2); !slices.Equal(got, []string{"ladder@example.com"}) {
		t.Fatalf("直连出口第 2 档以上的账户 = %v", got)
	}
	if got := pool.RateLadderAccounts("http://proxy.example:8080", 2); !slices.Equal(got, []string{"proxied@example.com"}) {
		t.Fatalf("代理出口第 2 档以上的账户 = %v", got)
	}
	if got := pool.RateLadderAccounts("", 4); got != nil {
		t.Fatalf("直连出口第 4 档以上的账户 = %v，期望没有", got)
	}
	if got := pool.RateLadderAccounts("http://missing.example:1", 2); got != nil {
		t.Fatalf("不存在的出口 = %v", got)
	}

	until := now.Add(10 * time.Second)
	relaxed, err := pool.RelaxRateLimitCooldowns("ladder@example.com", until, 2)
	if err != nil || relaxed != 2 {
		t.Fatalf("撤回 = %d %v，期望 2 条", relaxed, err)
	}
	pool.mu.Lock()
	cooldowns := maps.Clone(ladder.runtime.Cooldowns)
	strikes := len(ladder.rateStrikes)
	_, sharedLeft := ladder.rateStrikes[rateStrikeKey{model: indexTestModel}]
	_, buildLeft := ladder.rateStrikes[rateStrikeKey{model: indexTestModel, group: ChannelBuild}]
	_, flashLeft := ladder.rateStrikes[rateStrikeKey{model: indexTestFlash}]
	pool.mu.Unlock()
	for scope, want := range map[string]string{
		indexTestModel:            "限流: 出口限流撤回（原第 3 档），Quota exceeded",
		"build:" + indexTestModel: "限流: 出口限流撤回（原第 2 档），Quota exceeded（Build）",
	} {
		if cooldown := cooldowns[scope]; cooldown.Reason != want || !cooldown.Until.Equal(until.UTC()) {
			t.Fatalf("%s 撤回后 = %+v，期望原因 %q、到期 %s", scope, cooldown, want, until)
		}
	}
	for scope, reason := range map[string]string{
		indexTestFlash:     "限流: 连续限流第 1 档，Quota exceeded",
		indexTestImage:     "每日限额: 每日请求数已用完",
		indexTestUltraOnly: "限流: 出口限流期间账号持续被拒（连续 6 次），Quota exceeded",
		"count-tokens:x":   "无权限: 403",
	} {
		if cooldowns[scope].Reason != reason || !cooldowns[scope].Until.After(now.Add(20*time.Second)) {
			t.Fatalf("%s 不应被撤回：%+v", scope, cooldowns[scope])
		}
	}
	if sharedLeft || buildLeft || !flashLeft || strikes != 3 {
		t.Fatalf("撤回后档位：共用=%v Build=%v 低档=%v 剩 %d 条，期望只清撤回的模型（含 Playground 通道一份）", sharedLeft, buildLeft, flashLeft, strikes)
	}
	if got := pool.RateLadderAccounts("", 2); got != nil {
		t.Fatalf("撤回后直连出口仍列出 %v", got)
	}
	if again, err := pool.RelaxRateLimitCooldowns("ladder@example.com", until, 2); err != nil || again != 0 {
		t.Fatalf("再次撤回 = %d %v，期望 0", again, err)
	}
	if none, err := pool.RelaxRateLimitCooldowns("healthy@example.com", until, 2); err != nil || none != 0 {
		t.Fatalf("没有逐级冷却的账户撤回 = %d %v", none, err)
	}
	if _, err := pool.RelaxRateLimitCooldowns("missing@example.com", until, 2); !errors.Is(err, ErrAccountNotFound) {
		t.Fatalf("不存在的账户 = %v", err)
	}
	verifyIndex(t, pool, "撤回后")
}

// TestCapacityBusyEgressText 原因为“出口限速”时首段为“出口限速：”，其余原因的文案逐字不变
func TestCapacityBusyEgressText(t *testing.T) {
	egress := &CapacityBusyError{
		ModelID: "gemini-pro", Pool: PoolScopeUltra, Reason: EgressBusyReason, Waited: 30 * time.Second, Limit: 30 * time.Second,
		Elapsed: 31500 * time.Millisecond, Attempt: 2,
	}
	if want := "出口限速：模型 gemini-pro 所在出口每分钟请求已满，请稍后重试（Ultra 号池，已排队 30s，上限 30s，总耗时 31.5s，第 2 次尝试）"; egress.Error() != want {
		t.Fatalf("出口限速文案 = %q，期望 %q", egress.Error(), want)
	}
	plain := &CapacityBusyError{ModelID: "gemini-flash", Reason: EgressBusyReason, Waited: 1200 * time.Millisecond, Limit: 30 * time.Second, Attempt: 1}
	if want := "出口限速：模型 gemini-flash 所在出口每分钟请求已满，请稍后重试（已排队 1.2s，上限 30s，第 1 次尝试）"; plain.Error() != want {
		t.Fatalf("普通号池出口限速文案 = %q，期望 %q", plain.Error(), want)
	}
	busy := &CapacityBusyError{
		ModelID: "gemini-flash", Reason: "账号全部忙碌", Waited: 10 * time.Second, Limit: 10 * time.Second,
		Elapsed: 12 * time.Second, ColdStart: 2 * time.Second, Attempt: 1,
	}
	if want := "号池繁忙：模型 gemini-flash 暂时没有空闲的账号或 Worker，请稍后重试（已排队 10s，上限 10s，总耗时 12s（含现场冷启动 2s），第 1 次尝试，最后等待原因：账号全部忙碌）"; busy.Error() != want {
		t.Fatalf("号池繁忙文案 = %q，期望 %q", busy.Error(), want)
	}
}

// TestEgressFilterAccounts 按出口剔除账户：保持原顺序、同时返回被剔除的；不在号池中的账户保留；没有剔除时原样返回
func TestEgressFilterAccounts(t *testing.T) {
	direct := indexTestAccount("direct@example.com", BenefitTierFree, indexTestCatalog())
	proxied := indexTestAccount("proxied@example.com", BenefitTierFree, indexTestCatalog())
	proxied.Config.Proxy = "proxy.example:8080"
	other := indexTestAccount("other@example.com", BenefitTierUltra, indexTestCatalog())
	pool := NewAccountPool([]*Account{direct, proxied, other}, 1)
	ids := []string{"proxied@example.com", "direct@example.com", "missing@example.com", "other@example.com"}
	kept, removed := pool.FilterAccountsByEgress(ids, map[string]struct{}{"": {}})
	if !slices.Equal(kept, []string{"proxied@example.com", "missing@example.com"}) ||
		!slices.Equal(removed, []string{"direct@example.com", "other@example.com"}) {
		t.Fatalf("剔除直连 = 保留 %v、剔除 %v", kept, removed)
	}
	kept, removed = pool.FilterAccountsByEgress(ids, map[string]struct{}{"http://proxy.example:8080": {}})
	if !slices.Equal(kept, []string{"direct@example.com", "missing@example.com", "other@example.com"}) ||
		!slices.Equal(removed, []string{"proxied@example.com"}) {
		t.Fatalf("剔除代理 = 保留 %v、剔除 %v", kept, removed)
	}
	if kept, removed = pool.FilterAccountsByEgress(ids, nil); &kept[0] != &ids[0] || removed != nil {
		t.Fatal("没有受限出口时应原样返回")
	}
	if kept, removed = pool.FilterAccountsByEgress(ids, map[string]struct{}{"socks5://none:1": {}}); &kept[0] != &ids[0] || removed != nil {
		t.Fatal("没有账户被剔除时应原样返回")
	}
}

// TestEgressSnapshotCounts 快照按通道给出账户数、限速、桶容量、30 秒与 60 秒发送、主用发送、非生成 RPC、预占与下一个令牌
func TestEgressSnapshotCounts(t *testing.T) {
	pool := newEgressTestPool(4, "a@example.com")
	rate := egressLane(pool, "", EgressLaneNormal)
	now := time.Now().UnixNano()
	pool.mu.Lock()
	for _, sent := range []struct {
		ago     time.Duration
		primary bool
	}{{70 * time.Second, true}, {50 * time.Second, true}, {40 * time.Second, false}, {20 * time.Second, true}, {time.Second, false}} {
		at := now - int64(sent.ago)
		rate.sent.add(at, sent.primary, at-int64(time.Minute))
	}
	rate.other.add(now-int64(10*time.Second), false, now-int64(time.Minute))
	pool.mu.Unlock()
	status, listed := egressLaneSnapshot(pool, "", EgressLaneNormal)
	if !listed || status.Accounts != 1 || status.Sent1m != 4 || status.Sent30s != 2 || status.PrimarySent1m != 2 ||
		status.OtherSent1m != 1 || status.Limit != 0 || status.Burst != 0 || !status.NextAt.IsZero() {
		t.Fatalf("不限速时快照 = %+v", status)
	}
	snapshot := pool.EgressSnapshot()
	if len(snapshot) != 1 || snapshot[0].Label != "直连" || snapshot[0].ID != EgressID("") || snapshot[0].Lanes[1].Lane != EgressLaneUltra {
		t.Fatalf("快照 = %+v，期望只有直连一个出口", snapshot)
	}
	pool.SetEgressRateLimit("", EgressLaneNormal, 12)
	lease := mustTryAcquire(t, pool, egressPrimarySelection)
	status, _ = egressLaneSnapshot(pool, "", EgressLaneNormal)
	if status.Limit != 12 || status.Burst != 2 || status.Pending != 1 || status.NextAt.IsZero() ||
		status.NextAt.After(time.Now().Add(6*time.Second)) {
		t.Fatalf("限速 12、预占 1 时快照 = %+v", status)
	}
	_ = lease.Release()
}
