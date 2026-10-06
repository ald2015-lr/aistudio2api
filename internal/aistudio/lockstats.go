package aistudio

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// timedMutex 为账户池锁：用法与 sync.Mutex 相同，另外记录每次等锁与持锁的时间。
// 调度选号、目录索引重建、Cookie 回写等都在这把锁里进行，锁竞争会直接表现为"排队分配账号"变慢；
// 统计结果由管理接口 /api/debug/perf 输出，诊断脚本据此判断慢在锁上还是慢在浏览器
type timedMutex struct {
	mu       sync.Mutex
	lockedAt time.Duration
}

// slowLockHold 持锁超过该时长时记录调用位置，用来找出耗时的临界区
const slowLockHold = 20 * time.Millisecond

var lockClockBase = time.Now()

// lockClock 返回单调时钟读数，不受系统校时影响
func lockClock() time.Duration {
	return time.Since(lockClockBase)
}

type lockCounters struct {
	acquisitions atomic.Int64
	waitNanos    atomic.Int64
	maxWaitNanos atomic.Int64
	holdNanos    atomic.Int64
	maxHoldNanos atomic.Int64
	slowHolds    atomic.Int64
	slowMu       sync.Mutex
	slowSites    map[string]*slowLockSite
}

// slowLockSite 为某个调用位置的慢持锁统计
type slowLockSite struct {
	Site    string  `json:"site"`
	Count   int64   `json:"count"`
	TotalMS float64 `json:"total_ms"`
	MaxMS   float64 `json:"max_ms"`
}

var poolLockCounters = &lockCounters{slowSites: make(map[string]*slowLockSite)}

// Lock 加锁并记录等锁时间
func (m *timedMutex) Lock() {
	start := lockClock()
	m.mu.Lock()
	now := lockClock()
	m.lockedAt = now
	wait := int64(now - start)
	poolLockCounters.acquisitions.Add(1)
	poolLockCounters.waitNanos.Add(wait)
	storeMaxInt64(&poolLockCounters.maxWaitNanos, wait)
}

// Unlock 解锁并记录持锁时间
func (m *timedMutex) Unlock() {
	held := lockClock() - m.lockedAt
	m.mu.Unlock()
	poolLockCounters.holdNanos.Add(int64(held))
	storeMaxInt64(&poolLockCounters.maxHoldNanos, int64(held))
	if held >= slowLockHold {
		poolLockCounters.recordSlow(held)
	}
}

// recordSlow 记录一次慢持锁及其调用位置（调用 Unlock 的函数）
func (counters *lockCounters) recordSlow(held time.Duration) {
	counters.slowHolds.Add(1)
	site := "未知位置"
	if pc, _, _, ok := runtime.Caller(2); ok {
		if function := runtime.FuncForPC(pc); function != nil {
			site = function.Name()
			if slash := strings.LastIndex(site, "/"); slash >= 0 {
				site = site[slash+1:]
			}
		}
	}
	millis := lockMillis(int64(held))
	counters.slowMu.Lock()
	defer counters.slowMu.Unlock()
	entry := counters.slowSites[site]
	if entry == nil {
		if len(counters.slowSites) >= 64 {
			return
		}
		entry = &slowLockSite{Site: site}
		counters.slowSites[site] = entry
	}
	entry.Count++
	entry.TotalMS += millis
	entry.MaxMS = max(entry.MaxMS, millis)
}

// PoolLockStats 返回账户池锁自进程启动以来的累计统计
func PoolLockStats() map[string]any {
	counters := poolLockCounters
	counters.slowMu.Lock()
	sites := make([]slowLockSite, 0, len(counters.slowSites))
	for _, entry := range counters.slowSites {
		sites = append(sites, *entry)
	}
	counters.slowMu.Unlock()
	sort.Slice(sites, func(left, right int) bool { return sites[left].TotalMS > sites[right].TotalMS })
	if len(sites) > 10 {
		sites = sites[:10]
	}
	return map[string]any{
		"uptime_s":               lockClock().Seconds(),
		"acquisitions":           counters.acquisitions.Load(),
		"wait_ms_total":          lockMillis(counters.waitNanos.Load()),
		"wait_ms_max":            lockMillis(counters.maxWaitNanos.Load()),
		"hold_ms_total":          lockMillis(counters.holdNanos.Load()),
		"hold_ms_max":            lockMillis(counters.maxHoldNanos.Load()),
		"slow_holds":             counters.slowHolds.Load(),
		"slow_hold_threshold_ms": lockMillis(int64(slowLockHold)),
		"slow_sites":             sites,
	}
}

func lockMillis(nanos int64) float64 {
	return float64(nanos) / float64(time.Millisecond)
}

func storeMaxInt64(target *atomic.Int64, value int64) {
	for {
		current := target.Load()
		if value <= current || target.CompareAndSwap(current, value) {
			return
		}
	}
}
