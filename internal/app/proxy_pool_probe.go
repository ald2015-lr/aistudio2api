package app

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 抽测 IP 池与测试账户实际出口（POST /api/proxy/test 的 pool 与 account_id）：与普通的“测试代理”一样经两条路径访问出口 IP 服务，
// 不访问 Google；调用方持有 proxyTestMu

// proxyPoolTarget 为抽测的一个出口：slot 为端口号或临时会话号，url 为上游（可能带账号密码，只交给网络层）
type proxyPoolTarget struct {
	slot string
	url  string
}

// testProxyPool 抽测 IP 池的出口（输入框里当前的设置，不必先保存）：静态在端口段里均匀抽 samples 个端口（含首尾），
// 动态用 samples 个临时会话号（长度为程序可能生成的最大值，能提前发现代理商的长度限制）；每个出口经两条路径各访问一次出口 IP 服务，
// 最多 proxyPoolSampleConcurrency 个出口同时进行，总期限 proxyProbeTimeout
func (manager *runtimeManager) testProxyPool(ctx context.Context, input api.ProxyPoolConfig, samples int) (api.ProxyTestResult, error) {
	cfg, err := proxyPoolFromInput(input)
	if err != nil {
		// 校验的错误是固定文案，不含地址内容
		return api.ProxyTestResult{}, invalidConfigError(err)
	}
	if !cfg.Enabled() {
		return api.ProxyTestResult{}, invalidConfigError(fmt.Errorf("IP 池未开启：先选择静态 IP（static）或动态 IP（dynamic）再抽测"))
	}
	if samples <= 0 {
		samples = proxyPoolSampleDefault
	}
	samples = min(samples, proxyPoolSampleMax)
	targets := manager.proxyPoolTargets(cfg, samples)
	if len(targets) == 0 {
		return api.ProxyTestResult{}, invalidConfigError(fmt.Errorf("PROXY_POOL_URL 无法生成出口地址"))
	}

	probeCtx, cancel := context.WithTimeout(ctx, proxyProbeTimeout)
	defer cancel()
	results := make([]api.ProxyTestSample, len(targets))
	relayed := make([]bool, len(targets))
	limiter := make(chan struct{}, proxyPoolSampleConcurrency)
	var wait sync.WaitGroup
	for index, target := range targets {
		wait.Add(1)
		go func() {
			defer wait.Done()
			select {
			case limiter <- struct{}{}:
			case <-probeCtx.Done():
				results[index] = proxyPoolFailedSample(target.slot, "检测超时")
				return
			}
			defer func() { <-limiter }()
			browser, direct, viaRelay, err := manager.probeBothPaths(probeCtx, target.url)
			if err != nil {
				results[index] = proxyPoolFailedSample(target.slot, err.Error())
				return
			}
			relayed[index] = viaRelay
			results[index] = api.ProxyTestSample{
				Slot: target.slot, Browser: browser, Direct: direct, SameIP: browser.IP != "" && browser.IP == direct.IP,
			}
		}()
	}
	wait.Wait()

	first := results[0]
	authenticated := config.ProxyHasCredentials(targets[0].url)
	result := api.ProxyTestResult{
		Proxy: cfg.Endpoint(), Authenticated: authenticated, Relayed: relayed[0],
		Browser: first.Browser, Direct: first.Direct, SameIP: first.SameIP, Samples: results,
	}
	exits := make(map[string]struct{}, len(results))
	duplicate, partial, rotating := false, false, false
	for _, sample := range results {
		if sample.Browser.IP == "" || sample.Direct.IP == "" {
			partial = true
		}
		if sample.Browser.IP != "" && sample.Direct.IP != "" && sample.Browser.IP != sample.Direct.IP {
			rotating = true
		}
		// 出口按浏览器路径（Worker 实际走的）计，浏览器路径失败时取后台请求路径
		exit := sample.Browser.IP
		if exit == "" {
			exit = sample.Direct.IP
		}
		if exit == "" {
			continue
		}
		if _, seen := exits[exit]; seen {
			duplicate = true
		}
		exits[exit] = struct{}{}
	}
	distinct := len(exits)
	result.DistinctIPs = &distinct
	if rotating {
		result.Warnings = append(result.Warnings, proxyWarningRotatingIP)
	}
	if !authenticated && proxyScheme(targets[0].url) == "https" {
		result.Warnings = append(result.Warnings, proxyWarningHTTPSPlainBrowser)
	}
	if duplicate {
		result.Warnings = append(result.Warnings, proxyWarningDuplicateIP)
	}
	if partial {
		result.Warnings = append(result.Warnings, proxyWarningPoolPartial)
	}
	manager.logProxyPoolTest(cfg, results)
	return result, nil
}

// proxyPoolTargets 返回抽测的出口：静态在端口段里均匀取 samples 个端口（含首尾），动态取 samples 个临时会话号
func (manager *runtimeManager) proxyPoolTargets(cfg config.ProxyPool, samples int) []proxyPoolTarget {
	var targets []proxyPoolTarget
	switch cfg.Mode {
	case config.ProxyPoolStatic:
		for _, port := range proxyPoolSamplePorts(cfg, samples) {
			if url := cfg.StaticURL(port); url != "" {
				targets = append(targets, proxyPoolTarget{slot: strconv.Itoa(port), url: url})
			}
		}
	case config.ProxyPoolDynamic:
		for range samples {
			session := manager.proxyPool.probeSession()
			if url := cfg.SessionURL(session); url != "" {
				targets = append(targets, proxyPoolTarget{slot: session, url: url})
			}
		}
	}
	return targets
}

// proxyPoolSamplePorts 在端口段里均匀取 samples 个端口（含首尾，端口数少于 samples 时取全部）
func proxyPoolSamplePorts(cfg config.ProxyPool, samples int) []int {
	start, end := cfg.Ports()
	size := end - start + 1
	count := min(samples, size)
	if count <= 1 {
		return []int{start}
	}
	ports := make([]int, 0, count)
	for index := range count {
		ports = append(ports, start+index*(size-1)/(count-1))
	}
	return ports
}

// proxyPoolFailedSample 为没能开始测试的出口（中转申请失败、期限已到）：两条路径都记同一说明（不含地址）
func proxyPoolFailedSample(slot string, message string) api.ProxyTestSample {
	probe := api.ProxyTestProbe{Error: limitProbeError(message)}
	return api.ProxyTestSample{Slot: slot, Browser: probe, Direct: probe}
}

// logProxyPoolTest 每个出口记一行：两条路径都成功记 INFO，否则记 WARN；只写端口号或会话号，不写地址
func (manager *runtimeManager) logProxyPoolTest(cfg config.ProxyPool, samples []api.ProxyTestSample) {
	if manager.requests == nil {
		return
	}
	mode, field := "静态", "端口"
	if cfg.Mode == config.ProxyPoolDynamic {
		mode, field = "动态", "会话"
	}
	for _, sample := range samples {
		level := "INFO"
		if sample.Browser.IP == "" || sample.Direct.IP == "" {
			level = "WARN"
		}
		manager.requests.log("service", level, fmt.Sprintf("代理测试 | IP 池=%s | %s=%s | 浏览器路径=%s | 后台请求路径=%s",
			mode, field, sample.Slot, proxyProbeText(sample.Browser), proxyProbeText(sample.Direct)))
	}
}

// testPoolLoginExit 测试 IP 池给出的一个出口（新账户对话框代理留空时为此刻会分给登录的出口，账户编辑对话框清空代理时为清空后的
// 默认出口）：结果的 Slot 为端口号或会话号，日志与抽测相同、只写端口号或会话号
func (manager *runtimeManager) testPoolLoginExit(ctx context.Context, preview proxyPoolPreview) (api.ProxyTestResult, error) {
	result, err := manager.testNormalizedProxy(ctx, preview.url)
	if err != nil {
		return api.ProxyTestResult{}, err
	}
	result.Slot = preview.slot
	manager.logProxyPoolTest(preview.cfg, []api.ProxyTestSample{{
		Slot: preview.slot, Browser: result.Browser, Direct: result.Direct, SameIP: result.SameIP,
	}})
	return result, nil
}

// testAccountProxy 测试账户编辑对话框里代理留空时的出口：账户没有自己的代理时为它实际使用的出口（EffectiveProxy：IP 池绑定 > 默认代理），
// 结果的 Slot 为它当前的端口号或会话号；账户有自己的代理（对话框里清空了）时为清空后 IP 池会给它的出口，见 testClearedAccountProxy
func (manager *runtimeManager) testAccountProxy(ctx context.Context, accountID string) (api.ProxyTestResult, error) {
	generation := manager.generation()
	admin, ok := generation.admin.(*runtimeAdmin)
	if !ok || admin.pool == nil {
		return api.ProxyTestResult{}, fmt.Errorf("测试账户出口尚未就绪")
	}
	account, err := admin.pool.Account(accountID)
	if err != nil {
		return api.ProxyTestResult{}, accountOperationError(err)
	}
	if strings.TrimSpace(account.Config.Proxy) != "" {
		return manager.testClearedAccountProxy(ctx, account.ID, generation.config.Proxy)
	}
	value := account.EffectiveProxy(generation.config.Proxy)
	if value == "" {
		return api.ProxyTestResult{}, &adminOperationError{
			status: http.StatusBadRequest, code: "proxy_required", message: "这个账户没有可测试的出口：账户代理、IP 池与默认代理都为空",
		}
	}
	normalized, err := config.NormalizeProxy(value)
	if err != nil {
		return api.ProxyTestResult{}, invalidConfigError(err)
	}
	slot := ""
	if strings.TrimSpace(account.Config.Proxy) == "" {
		if binding, ok := account.CurrentProxyBinding(); ok {
			slot = binding.Slot
		}
	}
	result, err := manager.testNormalizedProxy(ctx, normalized)
	if err != nil {
		return api.ProxyTestResult{}, err
	}
	result.Slot = slot
	if manager.requests != nil {
		level := "INFO"
		if result.Browser.IP == "" || result.Direct.IP == "" {
			level = "WARN"
		}
		exit := ""
		if label := admin.ipPool().slotLabel(account.ID); label != "" {
			exit = " | " + label
		}
		manager.requests.log("service", level, fmt.Sprintf("代理测试 | 账户=%s | 代理=%s%s | 浏览器路径=%s | 后台请求路径=%s",
			account.ID, result.Proxy, exit, proxyProbeText(result.Browser), proxyProbeText(result.Direct)))
	}
	return result, nil
}

// testClearedAccountProxy 测试账户清空自己的代理后的出口（账户编辑对话框里清空代理再点“测试代理”）：IP 池开启时为它的默认出口
// （静态为归自己的端口或探测序列上第一个可用的端口，之后启动的 Worker 可能另分端口；动态为它的会话号），结果的 Slot 为端口号或会话号，
// 日志与抽测相同、只写端口号或会话号；IP 池关闭或没有可用端口时为默认代理
func (manager *runtimeManager) testClearedAccountProxy(ctx context.Context, accountID, global string) (api.ProxyTestResult, error) {
	if preview, ok := manager.proxyPool.accountPreview(accountID); ok {
		return manager.testPoolLoginExit(ctx, preview)
	}
	if strings.TrimSpace(global) == "" {
		return api.ProxyTestResult{}, &adminOperationError{
			status: http.StatusBadRequest, code: "proxy_required", message: "清空账户代理后这个账户没有可测试的出口：IP 池没有可用的出口，默认代理也为空",
		}
	}
	normalized, err := config.NormalizeProxy(global)
	if err != nil {
		return api.ProxyTestResult{}, invalidConfigError(err)
	}
	result, err := manager.testNormalizedProxy(ctx, normalized)
	if err != nil {
		return api.ProxyTestResult{}, err
	}
	manager.logProxyTest(result)
	return result, nil
}
