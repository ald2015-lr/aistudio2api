package app

import (
	"slices"
	"sort"
	"time"
)

// IP 池固定分配（PROXY_POOL_FIXED）的分配算法：纯函数，不碰 IP 池状态，结果只取决于输入（内部排序，不依赖 map 迭代顺序、时间与随机数）。
// N 个账户、P 个端口时 base = N/P、extra = N%P，均衡 = 每个端口 base 或 base+1 个、恰好 extra 个端口为 base+1

// fixedPrior 为首次迁移时账户已有的端口：Port 为亲和端口（在参与分配的端口里），Owner 为这个端口的归属就是它（最后一个独占它的账户，
// 运行中独占 Worker 的账户都是），At 为亲和记录的时间
type fixedPrior struct {
	Port  int
	Owner bool
	At    time.Time
}

// fixedMove 为一次挪动：From 为原端口（调用方填），To 为新端口
type fixedMove struct {
	Account  string
	From, To int
}

// fixedShare 返回 N 个账户平均分到 P 个端口时每个端口的基数与多一个的端口数
func fixedShare(accounts, ports int) (base, extra int) {
	if ports <= 0 {
		return 0, 0
	}
	return accounts / ports, accounts % ports
}

// sortedUniqueStrings 返回去空、去重、升序的副本
func sortedUniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return slices.Compact(result)
}

// sortedUniqueInts 返回去重、升序的副本
func sortedUniqueInts(values []int) []int {
	result := slices.Clone(values)
	sort.Ints(result)
	return slices.Compact(result)
}

// fixedPlanInitial 首次迁移：尽量保留账户已有的亲和端口（每个端口最多保留份额：base 个，再按归属、时间给 extra 个端口各多留一个），
// 其余账户按 ID 升序逐个放到已分配数最少的端口（并列取端口号小的）。accounts 顺序无关；ports 为参与分配的端口；prior 只计端口在 ports 里的。
// 返回每个账户的端口与保留原端口的个数（等于上界 Σ min(|组|, base) + min(extra, #{组: |组| > base})）
func fixedPlanInitial(accounts []string, ports []int, prior map[string]fixedPrior) (map[string]int, int) {
	accounts = sortedUniqueStrings(accounts)
	ports = sortedUniqueInts(ports)
	assign := make(map[string]int, len(accounts))
	if len(ports) == 0 || len(accounts) == 0 {
		return assign, 0
	}
	base, extra := fixedShare(len(accounts), len(ports))
	inPorts := make(map[int]bool, len(ports))
	for _, port := range ports {
		inPorts[port] = true
	}
	groups := make(map[int][]string)
	for _, accountID := range accounts {
		if previous, ok := prior[accountID]; ok && inPorts[previous.Port] {
			groups[previous.Port] = append(groups[previous.Port], accountID)
		}
	}
	// 组内顺序：归属优先，再按亲和时间新到旧，再按 ID
	before := func(left, right string) bool {
		a, b := prior[left], prior[right]
		if a.Owner != b.Owner {
			return a.Owner
		}
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return left < right
	}
	groupPorts := make([]int, 0, len(groups))
	for port, members := range groups {
		sort.Slice(members, func(i, j int) bool { return before(members[i], members[j]) })
		groupPorts = append(groupPorts, port)
	}
	sort.Ints(groupPorts)
	count := make(map[int]int, len(ports))
	kept := 0
	for _, port := range groupPorts {
		members := groups[port]
		for index := 0; index < len(members) && index < base; index++ {
			assign[members[index]] = port
			count[port]++
			kept++
		}
	}
	// 多一个的名额：保留之后仍有剩余的组，按下一个候选的归属、时间与端口号取前 extra 个
	var candidates []int
	for _, port := range groupPorts {
		if len(groups[port]) > base {
			candidates = append(candidates, port)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := prior[groups[candidates[i]][base]], prior[groups[candidates[j]][base]]
		if left.Owner != right.Owner {
			return left.Owner
		}
		if !left.At.Equal(right.At) {
			return left.At.After(right.At)
		}
		return candidates[i] < candidates[j]
	})
	for index := 0; index < len(candidates) && index < extra; index++ {
		port := candidates[index]
		assign[groups[port][base]] = port
		count[port]++
		kept++
	}
	load := func(port int) int { return count[port] }
	for _, accountID := range accounts {
		if _, ok := assign[accountID]; ok {
			continue
		}
		port := fixedPickLeast(ports, load, nil)
		assign[accountID] = port
		count[port]++
	}
	return assign, kept
}

// fixedPickLeast 返回 load 最小的端口（并列取端口号小的）：avoid 非 nil 且有不被 avoid 的端口时只在它们里选；ports 为空时返回 0
func fixedPickLeast(ports []int, load func(int) int, avoid func(int) bool) int {
	usable := func(port int) bool { return true }
	if avoid != nil && slices.ContainsFunc(ports, func(port int) bool { return !avoid(port) }) {
		usable = func(port int) bool { return !avoid(port) }
	}
	best, bestLoad := 0, 0
	for _, port := range ports {
		if !usable(port) {
			continue
		}
		current := load(port)
		if best == 0 || current < bestLoad || current == bestLoad && port < best {
			best, bestLoad = port, current
		}
	}
	return best
}

// fixedPlanEvacuate 把 movers 按 ID 升序逐个放到 ports 里 load 最小的端口（边放边加计数，多个账户不挤到同一个端口；avoid 见 fixedPickLeast）。
// load 会被改写（调用方传副本）；返回的 From 由调用方填
func fixedPlanEvacuate(movers []string, ports []int, load map[int]int, avoid func(int) bool) []fixedMove {
	movers = sortedUniqueStrings(movers)
	ports = sortedUniqueInts(ports)
	if len(ports) == 0 {
		return nil
	}
	moves := make([]fixedMove, 0, len(movers))
	current := func(port int) int { return load[port] }
	for _, accountID := range movers {
		port := fixedPickLeast(ports, current, avoid)
		load[port]++
		moves = append(moves, fixedMove{Account: accountID, To: port})
	}
	return moves
}

// fixedPlanRebalance 最少挪动的重新平均分配：assign 为账户 → 当前端口，ports 为参与分配的端口。端口不在 ports 里的账户全部挪；
// 账户数 ≥ base+1 的端口按（账户数降序、端口号升序）取前 extra 个目标为 base+1，其余目标为 base；超过目标的端口按
// （keep 升序、at 降序、ID 降序）挑出多出的账户（keep 高的有浏览器在固定端口上服务，尽量不挪）；这些账户按 ID 升序逐个放到当前最少的端口。
// 挪动数等于下界 Σ max(0, c−base) − min(extra, #{c ≥ base+1}) + |不在 ports 的|，执行后均衡，再执行一次为 0
func fixedPlanRebalance(assign map[string]int, ports []int, keep map[string]int, at map[string]time.Time) []fixedMove {
	ports = sortedUniqueInts(ports)
	if len(ports) == 0 || len(assign) == 0 {
		return nil
	}
	inPorts := make(map[int]bool, len(ports))
	for _, port := range ports {
		inPorts[port] = true
	}
	ids := make([]string, 0, len(assign))
	for accountID := range assign {
		ids = append(ids, accountID)
	}
	sort.Strings(ids)
	base, extra := fixedShare(len(ids), len(ports))
	count := make(map[int]int, len(ports))
	members := make(map[int][]string)
	var movers []string
	for _, accountID := range ids {
		port := assign[accountID]
		if !inPorts[port] {
			movers = append(movers, accountID)
			continue
		}
		count[port]++
		members[port] = append(members[port], accountID)
	}
	plus := make([]int, 0, len(ports))
	for _, port := range ports {
		if count[port] >= base+1 {
			plus = append(plus, port)
		}
	}
	sort.SliceStable(plus, func(i, j int) bool {
		if count[plus[i]] != count[plus[j]] {
			return count[plus[i]] > count[plus[j]]
		}
		return plus[i] < plus[j]
	})
	target := make(map[int]int, len(ports))
	for _, port := range ports {
		target[port] = base
	}
	for index := 0; index < len(plus) && index < extra; index++ {
		target[plus[index]] = base + 1
	}
	for _, port := range ports {
		over := count[port] - target[port]
		if over <= 0 {
			continue
		}
		candidates := slices.Clone(members[port])
		sort.Slice(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			if keep[left] != keep[right] {
				return keep[left] < keep[right]
			}
			if !at[left].Equal(at[right]) {
				return at[left].After(at[right])
			}
			return left > right
		})
		movers = append(movers, candidates[:over]...)
		count[port] -= over
	}
	moves := fixedPlanEvacuate(movers, ports, count, nil)
	result := moves[:0]
	for _, move := range moves {
		move.From = assign[move.Account]
		if move.From != move.To {
			result = append(result, move)
		}
	}
	return result
}
