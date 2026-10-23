package app

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
	"github.com/Mag1cFall/AIStudio2API/internal/proxyrelay"
)

const (
	// proxyPoolCheckDuplicateLimit、proxyPoolCheckFailureLimit 为检查结果里列出的重复组与失败端口上限
	proxyPoolCheckDuplicateLimit = 20
	proxyPoolCheckFailureLimit   = 50
)

// startCheck 异步检查全部端口（静态）：逐个端口经后台请求路径访问出口 IP 服务（并发 10、总期限 5 分钟，不经本机中转、
// 不持有 proxyTestMu），进度与结果写进状态接口的 check。cfg 必须已通过校验且是 static；不是时、或已有检查在进行时返回 false。
// ctx 应是不随管理请求结束的上下文（例如生命周期上下文）；probe 为空时用 probeProxyExit
func (pool *proxyPool) startCheck(ctx context.Context, cfg config.ProxyPool, probe func(context.Context, string) api.ProxyTestProbe) bool {
	if pool == nil {
		return false
	}
	normalized, err := config.NormalizeProxyPool(cfg)
	if err != nil || normalized.Mode != config.ProxyPoolStatic {
		return false
	}
	urls := normalized.StaticURLs()
	if len(urls) == 0 {
		return false
	}
	if probe == nil {
		probe = probeProxyExit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	start, _ := normalized.Ports()
	pool.mu.Lock()
	if pool.checkRunning {
		pool.mu.Unlock()
		return false
	}
	pool.checkRunning = true
	pool.check = &api.AdminProxyPoolCheck{Running: true, StartedAt: pool.now().UTC(), Total: len(urls)}
	check := pool.check
	pool.mu.Unlock()
	go pool.runCheck(ctx, check, normalized.Endpoint(), start, urls, probe)
	return true
}

// runCheck 执行一次检查：结果按端口记下出口 IP，出口 IP 相同的端口归为重复组；错误按该端口的上游脱敏
func (pool *proxyPool) runCheck(ctx context.Context, check *api.AdminProxyPoolCheck, endpoint string, start int, urls []string,
	probe func(context.Context, string) api.ProxyTestProbe) {
	ctx, cancel := context.WithTimeout(ctx, proxyPoolCheckBudget)
	defer cancel()
	results := make([]api.ProxyTestProbe, len(urls))
	indexes := make(chan int)
	var wait sync.WaitGroup
	for worker := 0; worker < min(proxyPoolCheckConcurrency, len(urls)); worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range indexes {
				result := probe(ctx, urls[index])
				result.Error = limitProbeError(proxyrelay.Scrub(result.Error, urls[index]))
				if result.IP == "" && result.Error == "" {
					result.Error = "没有拿到出口 IP"
				}
				results[index] = result
				pool.mu.Lock()
				check.Done++
				if result.IP != "" {
					check.OK++
				}
				pool.mu.Unlock()
			}
		}()
	}
	for index := range urls {
		indexes <- index
	}
	close(indexes)
	wait.Wait()

	byIP := make(map[string][]int)
	var failed []api.AdminProxyPoolFailure
	for index, result := range results {
		port := start + index
		if result.IP == "" {
			failed = append(failed, api.AdminProxyPoolFailure{Port: port, Error: result.Error})
			continue
		}
		byIP[result.IP] = append(byIP[result.IP], port)
	}
	var duplicates []api.AdminProxyPoolDuplicate
	for ip, ports := range byIP {
		if len(ports) >= 2 {
			duplicates = append(duplicates, api.AdminProxyPoolDuplicate{IP: ip, Ports: ports})
		}
	}
	sort.Slice(duplicates, func(i, j int) bool { return duplicates[i].Ports[0] < duplicates[j].Ports[0] })
	duplicateGroups := len(duplicates)
	if len(duplicates) > proxyPoolCheckDuplicateLimit {
		duplicates = duplicates[:proxyPoolCheckDuplicateLimit]
	}
	failedCount := len(failed)
	if len(failed) > proxyPoolCheckFailureLimit {
		failed = failed[:proxyPoolCheckFailureLimit]
	}

	pool.mu.Lock()
	finished := pool.now().UTC()
	check.Running = false
	check.FinishedAt = finished
	check.DistinctIPs = len(byIP)
	check.Duplicates = duplicates
	check.Failed = failed
	elapsed := finished.Sub(check.StartedAt).Round(time.Second)
	total, ok := check.Total, check.OK
	pool.checkRunning = false
	pool.mu.Unlock()

	level := "INFO"
	if duplicateGroups > 0 || failedCount > 0 {
		level = "WARN"
	}
	pool.emit([]proxyPoolLog{{source: "service", level: level, message: fmt.Sprintf(
		"IP 池检查完成 | 出口=%s | 端口=%d | 成功=%d | 不同 IP=%d | 重复=%d 组 | 失败=%d | 耗时=%s",
		endpoint, total, ok, len(byIP), duplicateGroups, failedCount, elapsed)}})
}
