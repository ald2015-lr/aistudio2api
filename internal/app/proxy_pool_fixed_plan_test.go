package app

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"
)

// fixedTestPorts 返回 start 起的 count 个端口
func fixedTestPorts(start, count int) []int {
	ports := make([]int, count)
	for index := range ports {
		ports[index] = start + index
	}
	return ports
}

// fixedTestCounts 返回每个端口的账户数（ports 里没有账户的端口为 0）
func fixedTestCounts(assign map[string]int, ports []int) map[int]int {
	counts := make(map[int]int, len(ports))
	for _, port := range ports {
		counts[port] = 0
	}
	for _, port := range assign {
		counts[port]++
	}
	return counts
}

// assertFixedBalanced 断言分配均衡：每端口 base 或 base+1，恰好 extra 个端口为 base+1，且没有端口在 ports 之外
func assertFixedBalanced(t *testing.T, label string, assign map[string]int, ports []int) {
	t.Helper()
	base, extra := fixedShare(len(assign), len(ports))
	counts := fixedTestCounts(assign, ports)
	if len(counts) != len(ports) {
		t.Fatalf("%s: 有账户分到了参与分配之外的端口：%v", label, counts)
	}
	plus := 0
	for port, count := range counts {
		switch count {
		case base:
		case base + 1:
			plus++
		default:
			t.Fatalf("%s: 端口 %d 有 %d 个账户，应为 %d 或 %d", label, port, count, base, base+1)
		}
	}
	if plus != extra {
		t.Fatalf("%s: %d 个端口为 base+1，应为 %d", label, plus, extra)
	}
}

// TestFixedPlanInitialBalance 随机 N∈[0,3000]、P∈[1,600] 与随机亲和：首次迁移总是均衡
func TestFixedPlanInitialBalance(t *testing.T) {
	random := rand.New(rand.NewSource(20261124))
	for round := range 200 {
		accounts := proxyPoolTestIDs(random.Intn(3001))
		ports := fixedTestPorts(10001, 1+random.Intn(600))
		prior := make(map[string]fixedPrior)
		for _, accountID := range accounts {
			if random.Intn(3) > 0 {
				prior[accountID] = fixedPrior{
					Port: 10001 + random.Intn(len(ports)+20), Owner: random.Intn(4) == 0,
					At: time.Unix(int64(random.Intn(1000)), 0),
				}
			}
		}
		assign, kept := fixedPlanInitial(accounts, ports, prior)
		if len(assign) != len(accounts) {
			t.Fatalf("第 %d 组：分配了 %d 个，应为 %d", round, len(assign), len(accounts))
		}
		assertFixedBalanced(t, fmt.Sprintf("第 %d 组", round), assign, ports)
		if want := fixedKeptBound(accounts, ports, prior); kept != want {
			t.Fatalf("第 %d 组：保留 %d 个，上界 %d", round, kept, want)
		}
	}
}

// fixedKeptBound 为首次迁移保留数的上界 Σ min(|组|, base) + min(extra, #{组: |组| > base})
func fixedKeptBound(accounts []string, ports []int, prior map[string]fixedPrior) int {
	base, extra := fixedShare(len(accounts), len(ports))
	groups := make(map[int]int)
	for _, accountID := range accounts {
		if previous, ok := prior[accountID]; ok && slices.Contains(ports, previous.Port) {
			groups[previous.Port]++
		}
	}
	bound, over := 0, 0
	for _, size := range groups {
		bound += min(size, base)
		if size > base {
			over++
		}
	}
	return bound + min(extra, over)
}

// TestFixedPlanInitialUserExample 用户的例子：2000 个账户 500 个端口每端口 4 个；2003 个时 3 个端口 5 个；10 个账户 500 个端口时 10 个端口各 1 个；1 个端口全在上面
func TestFixedPlanInitialUserExample(t *testing.T) {
	for _, test := range []struct {
		accounts, ports, base, plus int
	}{
		{2000, 500, 4, 0}, {2003, 500, 4, 3}, {10, 500, 0, 10}, {7, 1, 7, 0},
	} {
		ports := fixedTestPorts(10001, test.ports)
		assign, _ := fixedPlanInitial(proxyPoolTestIDs(test.accounts), ports, nil)
		assertFixedBalanced(t, fmt.Sprintf("%d/%d", test.accounts, test.ports), assign, ports)
		plus := 0
		for _, count := range fixedTestCounts(assign, ports) {
			if count == test.base+1 {
				plus++
			}
		}
		if plus != test.plus {
			t.Fatalf("%d/%d：%d 个端口多一个，应为 %d", test.accounts, test.ports, plus, test.plus)
		}
	}
}

// TestFixedPlanInitialDeterministic 打乱账户顺序、重建 prior 的 map 20 次结果相同
func TestFixedPlanInitialDeterministic(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	accounts := proxyPoolTestIDs(437)
	ports := fixedTestPorts(20001, 61)
	prior := make(map[string]fixedPrior)
	for _, accountID := range accounts {
		prior[accountID] = fixedPrior{Port: 20001 + random.Intn(70), Owner: random.Intn(5) == 0, At: time.Unix(int64(random.Intn(50)), 0)}
	}
	want, wantKept := fixedPlanInitial(accounts, ports, prior)
	for round := range 20 {
		shuffled := slices.Clone(accounts)
		random.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		rebuilt := make(map[string]fixedPrior, len(prior))
		for _, accountID := range shuffled {
			rebuilt[accountID] = prior[accountID]
		}
		reversed := slices.Clone(ports)
		slices.Reverse(reversed)
		got, kept := fixedPlanInitial(shuffled, reversed, rebuilt)
		if kept != wantKept || len(got) != len(want) {
			t.Fatalf("第 %d 次：保留 %d/%d", round, kept, wantKept)
		}
		for accountID, port := range want {
			if got[accountID] != port {
				t.Fatalf("第 %d 次：%s 分到 %d，第一次是 %d", round, accountID, got[accountID], port)
			}
		}
	}
}

// TestFixedPlanInitialKeepsAffinity 保留数等于上界；同一端口超过份额时归属者优先、再按最近；段外亲和不保留；没有亲和的按 ID 补到最少的端口
func TestFixedPlanInitialKeepsAffinity(t *testing.T) {
	ports := fixedTestPorts(10001, 4)
	at := func(seconds int) time.Time { return time.Unix(int64(seconds), 0) }
	// 8 个账户 4 个端口：base=2。端口 10001 上 4 个亲和（归属者 d、最近的 c、b），10002 上 1 个，段外 1 个
	prior := map[string]fixedPrior{
		"a@example.com": {Port: 10001, At: at(1)},
		"b@example.com": {Port: 10001, At: at(3)},
		"c@example.com": {Port: 10001, At: at(5)},
		"d@example.com": {Port: 10001, Owner: true, At: at(0)},
		"e@example.com": {Port: 10002, At: at(9)},
		"f@example.com": {Port: 30001, Owner: true, At: at(9)},
	}
	accounts := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com",
		"g@example.com", "h@example.com"}
	assign, kept := fixedPlanInitial(accounts, ports, prior)
	assertFixedBalanced(t, "亲和", assign, ports)
	if kept != 3 || kept != fixedKeptBound(accounts, ports, prior) {
		t.Fatalf("保留 %d 个，应为上界 3", kept)
	}
	if assign["d@example.com"] != 10001 || assign["c@example.com"] != 10001 || assign["e@example.com"] != 10002 {
		t.Fatalf("归属者与最近的应留在原端口：%v", assign)
	}
	if assign["a@example.com"] == 10001 || assign["b@example.com"] == 10001 {
		t.Fatalf("超过份额的旧亲和应挪走：%v", assign)
	}
	// 9 个账户：extra=1，多一个的名额给下一个候选（b，按时间）
	accounts = append(accounts, "i@example.com")
	assign, kept = fixedPlanInitial(accounts, ports, prior)
	assertFixedBalanced(t, "多一个", assign, ports)
	if kept != 4 || assign["b@example.com"] != 10001 {
		t.Fatalf("多一个的名额应给 10001 上的下一个候选 b：保留 %d %v", kept, assign)
	}
}

// TestFixedPickLeast 并列取端口号小的；avoid 全部命中时退回全集；空端口集返回 0
func TestFixedPickLeast(t *testing.T) {
	load := map[int]int{10001: 2, 10002: 1, 10003: 1, 10004: 0}
	current := func(port int) int { return load[port] }
	ports := []int{10003, 10002, 10001}
	if got := fixedPickLeast(ports, current, nil); got != 10002 {
		t.Fatalf("并列应取端口号小的：%d", got)
	}
	if got := fixedPickLeast(ports, current, func(port int) bool { return port == 10002 }); got != 10003 {
		t.Fatalf("应避开 10002：%d", got)
	}
	if got := fixedPickLeast(ports, current, func(int) bool { return true }); got != 10002 {
		t.Fatalf("全部避开时退回全集：%d", got)
	}
	if got := fixedPickLeast(nil, current, nil); got != 0 {
		t.Fatalf("空端口集应返回 0：%d", got)
	}
}

// TestFixedPlanEvacuate 只挪 movers；边放边加计数（多个账户不挤到同一个端口）；avoid 生效
func TestFixedPlanEvacuate(t *testing.T) {
	ports := []int{10001, 10002, 10003}
	load := map[int]int{10001: 1, 10002: 1, 10003: 1}
	moves := fixedPlanEvacuate([]string{"c@example.com", "a@example.com", "b@example.com"}, ports, load, nil)
	if len(moves) != 3 || moves[0].Account != "a@example.com" || moves[0].To != 10001 || moves[1].To != 10002 || moves[2].To != 10003 {
		t.Fatalf("应按 ID 升序逐个放到最少的端口：%+v", moves)
	}
	load = map[int]int{10001: 0, 10002: 5, 10003: 5}
	moves = fixedPlanEvacuate([]string{"a@example.com", "b@example.com"}, ports, load, func(port int) bool { return port == 10001 })
	if moves[0].To != 10002 || moves[1].To != 10003 {
		t.Fatalf("应避开暂停的 10001：%+v", moves)
	}
}

// fixedRebalanceBound 为重新平均分配挪动数的下界
func fixedRebalanceBound(assign map[string]int, ports []int) int {
	base, extra := fixedShare(len(assign), len(ports))
	counts := make(map[int]int)
	outside := 0
	for _, port := range assign {
		if slices.Contains(ports, port) {
			counts[port]++
		} else {
			outside++
		}
	}
	over, plus := 0, 0
	for _, port := range ports {
		over += max(0, counts[port]-base)
		if counts[port] >= base+1 {
			plus++
		}
	}
	return over - min(extra, plus) + outside
}

// TestFixedPlanRebalanceMinimal 随机 100 组：挪动数等于下界、执行后均衡、再执行为 0；keep 高的不挪；不在 ports 的全部挪走；[6,4,4,2]→[4,4,4,4]
func TestFixedPlanRebalanceMinimal(t *testing.T) {
	random := rand.New(rand.NewSource(99))
	for round := range 100 {
		ports := fixedTestPorts(10001, 1+random.Intn(40))
		assign := make(map[string]int)
		keep := make(map[string]int)
		at := make(map[string]time.Time)
		for index, accountID := range proxyPoolTestIDs(random.Intn(400)) {
			// 偏向前几个端口，另有少量在段外
			port := 10001 + random.Intn(len(ports)/2+1)
			if random.Intn(10) == 0 {
				port = 30001 + random.Intn(5)
			}
			assign[accountID] = port
			keep[accountID] = random.Intn(3)
			at[accountID] = time.Unix(int64(index), 0)
		}
		moves := fixedPlanRebalance(assign, ports, keep, at)
		if want := fixedRebalanceBound(assign, ports); len(moves) != want {
			t.Fatalf("第 %d 组：挪动 %d 个，下界 %d", round, len(moves), want)
		}
		after := make(map[string]int, len(assign))
		for accountID, port := range assign {
			after[accountID] = port
		}
		for _, move := range moves {
			if move.From != assign[move.Account] || move.From == move.To {
				t.Fatalf("第 %d 组：挪动记录不对 %+v", round, move)
			}
			after[move.Account] = move.To
		}
		assertFixedBalanced(t, fmt.Sprintf("第 %d 组", round), after, ports)
		if again := fixedPlanRebalance(after, ports, keep, at); len(again) != 0 {
			t.Fatalf("第 %d 组：均衡后再执行应不挪：%d", round, len(again))
		}
	}
	// [6,4,4,2] → 挪 2 个，都来自 6 个的端口；keep 高的不挪
	ports := fixedTestPorts(10001, 4)
	assign := make(map[string]int)
	keep := make(map[string]int)
	ids := proxyPoolTestIDs(16)
	for index, accountID := range ids {
		switch {
		case index < 6:
			assign[accountID] = 10001
		case index < 10:
			assign[accountID] = 10002
		case index < 14:
			assign[accountID] = 10003
		default:
			assign[accountID] = 10004
		}
		if index < 4 {
			keep[accountID] = 2
		}
	}
	moves := fixedPlanRebalance(assign, ports, keep, map[string]time.Time{})
	if len(moves) != 2 {
		t.Fatalf("[6,4,4,2] 应挪 2 个：%+v", moves)
	}
	for _, move := range moves {
		if move.From != 10001 || move.To != 10004 || keep[move.Account] != 0 {
			t.Fatalf("应从 10001 挪 keep 低的到 10004：%+v", move)
		}
	}
}
