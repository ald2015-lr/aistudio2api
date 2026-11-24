package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Mag1cFall/AIStudio2API/internal/aistudio"
	"github.com/Mag1cFall/AIStudio2API/internal/api"
	"github.com/Mag1cFall/AIStudio2API/internal/config"
)

// 补充池（EXTRA_POOL）默认关闭时与原行为逐字节一致的金样：一个固定的场景（三个分区的账户、预热、三个入口的请求含一次 429 换号、
// 空闲回收、保存一次无关的设置）跑完后，把租约的账户序列、规范化后的运行日志、状态接口 JSON、三个入口的模型列表、IP 池容量与
// 号池规则版本写成 JSON，与 testdata/extra_off_golden.json 逐字节比较。金样在加入补充池之前的代码上用 -update 生成，
// 之后每一次改动都必须让它原样通过

var updateGolden = flag.Bool("update", false, "重新生成 testdata 下的金样文件")

const (
	goldenNormalA = "golden-normal-a@example.com"
	goldenNormalB = "golden-normal-b@example.com"
	goldenNormalC = "golden-normal-c@example.com"
	goldenProA    = "golden-pro-a@example.com"
	goldenProB    = "golden-pro-b@example.com"
	goldenUltraA  = "golden-ultra-a@example.com"
	// goldenPaidModel 只在 Build 通道、只有 Pro 与 Ultra 账户的 Build 目录里有
	goldenPaidModel = "gemini-4-argon"
	goldenFile      = "extra_off_golden.json"
)

// goldenUpstream 为金样场景的上游：按顺序记下每次生成调用的入口、账户、模型与通道；quota 中的账户第一次调用返回分钟限额 429
type goldenUpstream struct {
	mu    sync.Mutex
	calls []string
	quota map[string]bool
	route string
}

func (upstream *goldenUpstream) Models(context.Context) ([]aistudio.Model, error) { return nil, nil }

func (upstream *goldenUpstream) CountTokens(context.Context, aistudio.TokenCountRequest) (aistudio.TokenCount, error) {
	return aistudio.TokenCount{}, nil
}

func (upstream *goldenUpstream) Generate(ctx context.Context, request aistudio.GenerateRequest) (<-chan aistudio.Event, error) {
	channel := ""
	if lease, ok := aistudio.AccountLeaseFromContext(ctx); ok {
		lease.RecordGenerationSent()
		channel = string(lease.Channel())
	}
	upstream.mu.Lock()
	upstream.calls = append(upstream.calls, fmt.Sprintf("%s %s %s/%s", upstream.route, request.AccountID, strings.TrimPrefix(request.Model, "models/"), channel))
	limited := upstream.quota[request.AccountID]
	delete(upstream.quota, request.AccountID)
	upstream.mu.Unlock()
	if limited {
		return nil, minuteQuotaError()
	}
	events := make(chan aistudio.Event, 2)
	events <- aistudio.Event{Kind: aistudio.EventText, Text: "hello"}
	events <- aistudio.Event{Kind: aistudio.EventFinish, FinishReason: "STOP"}
	close(events)
	return events, nil
}

func (upstream *goldenUpstream) setRoute(route string) {
	upstream.mu.Lock()
	upstream.route = route
	upstream.mu.Unlock()
}

func (upstream *goldenUpstream) called() []string {
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	return slices.Clone(upstream.calls)
}

// goldenCatalog 为金样场景的目录服务：缓存里总是两个模型（保存设置后的模型同步按它重建模型快照）
type goldenCatalog struct{}

func (goldenCatalog) RefreshAccountModels(context.Context, string) ([]aistudio.Model, error) {
	return goldenCatalog{}.CachedModels(), nil
}

func (goldenCatalog) CachedModels() []aistudio.Model {
	return []aistudio.Model{goldenModel(testRuntimeModel), goldenModel(goldenPaidModel)}
}

// goldenEnv 为金样场景的运行时
type goldenEnv struct {
	ctx      context.Context
	cfg      config.Config
	pool     *aistudio.AccountPool
	workers  *accountWorkerManager
	requests *requestRegistry
	service  *trackedService
	admin    *runtimeAdmin
	manager  *runtimeManager
	upstream *goldenUpstream
	// logged 为已经收进某个阶段的运行日志条数
	logged int
}

// goldenOptions 为场景的可变部分：caseACooldown 为 Pro 账户 goldenProA 只在 Build 通道的付费模型上的冷却时长（情形 A），
// configure 在写 .env 之前调整配置，prepare 在运行时建好、场景开始之前调整运行时（补充池开启的子用例用它们）
type goldenOptions struct {
	caseACooldown time.Duration
	configure     func(t *testing.T, env *goldenEnv)
	prepare       func(t *testing.T, env *goldenEnv)
}

// goldenModel 返回金样目录中的模型
func goldenModel(id string) aistudio.Model {
	return aistudio.Model{ID: id, Name: id, Methods: []string{"generateContent", "countTokens"}, Capabilities: map[string]bool{"chat_model": true}}
}

func newGoldenEnv(t *testing.T, options goldenOptions) *goldenEnv {
	t.Helper()
	clearAppConfigEnv(t)
	clearPrimaryConfigEnv(t)
	for _, key := range []string{
		"ULTRA_PRIMARY_MODELS", "ULTRA_PRIMARY_QUEUE_WAIT", "PRO_POOL", "PRO_WARM_WORKER_LIMIT", "PRO_MAX_ACTIVE_WORKERS", "WARM_STARTUP_CONCURRENCY",
	} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
	cfg := config.Default()
	cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.WarmStartupConcurrency = 2, 3, 1
	cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers = 1, 1
	cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers = 1, 2
	cfg.UltraPrimaryModels = nil
	env := &goldenEnv{cfg: cfg, upstream: &goldenUpstream{quota: map[string]bool{goldenNormalA: true}}}
	if options.configure != nil {
		options.configure(t, env)
	}
	path := filepath.Join(t.TempDir(), ".env")
	if err := env.cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg = env.cfg
	workers, pool, requests := proPartitionManager(t,
		[]string{goldenNormalA, goldenNormalB, goldenNormalC}, []string{goldenUltraA}, []string{goldenProA, goldenProB},
		cfg.WarmWorkerLimit, cfg.MaxActiveWorkers, cfg.ProWarmWorkerLimit, cfg.ProMaxActiveWorkers)
	workers.setUltraCapacity(cfg.UltraWarmWorkerLimit, cfg.UltraMaxActiveWorkers)
	workers.warmConcurrency.Store(int64(cfg.WarmStartupConcurrency))
	workers.startupSlots.resized()
	pool.SetUpstreamChannels([]aistudio.Channel{aistudio.ChannelPlayground, aistudio.ChannelBuild})
	for _, accountID := range []string{goldenProA, goldenProB, goldenUltraA} {
		if err := pool.SetBuildCatalog(accountID, []aistudio.Model{goldenModel(goldenPaidModel)}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	service := retireTestService(ctx, workers, pool, requests)
	service.service = env.upstream
	service.catalog = goldenCatalog{}
	service.timeout.Store(int64(time.Minute))
	service.setPrimaryModelSettings(cfg.PrimaryModels, cfg.PrimaryQueueWait, cfg.UltraPrimaryModels, cfg.UltraPrimaryQueueWait)
	service.replaceModelSnapshot(goldenCatalog{}.CachedModels())
	admin := &runtimeAdmin{lifecycle: ctx, pool: pool, service: service, requests: requests, workers: workers, configPath: path, config: cfg}
	manager := &runtimeManager{
		lifecycle: ctx, configPath: path, activeManagement: cfg, requests: requests, apiKey: newAPIKeyHolder(cfg.ProxyAPIKey),
		intent: &serviceIntent{}, current: &runtimeGeneration{service: service, admin: admin, config: cfg},
	}
	manager.ultraExclusive.Store(cfg.UltraExclusive)
	manager.proPool.Store(cfg.ProPool)
	// 情形 A：Pro 账户只在 Build 独有的付费模型上冷却（另一个模型照常能用）；Free 账户在唯一的模型上冷却
	coolModel(t, pool, goldenProA, aistudio.ChannelCooldownScope(aistudio.ChannelBuild, goldenPaidModel), options.caseACooldown, "每日限额")
	coolModelFor(t, pool, goldenNormalC, testRuntimeModel, 2*time.Hour)
	env.ctx, env.pool, env.workers, env.requests, env.service, env.admin, env.manager = ctx, pool, workers, requests, service, admin, manager
	if options.prepare != nil {
		options.prepare(t, env)
	}
	return env
}

// goldenPhase 为一个阶段新写的运行日志（规范化后）
type goldenPhase struct {
	Name string   `json:"name"`
	Logs []string `json:"logs"`
}

// goldenRecord 为金样文件的内容
type goldenRecord struct {
	Calls           []string            `json:"calls"`
	Results         []string            `json:"results"`
	Phases          []goldenPhase       `json:"phases"`
	Status          map[string]any      `json:"status"`
	Models          map[string][]string `json:"models"`
	ProxyCapacity   int                 `json:"proxy_capacity"`
	LiveCapacity    int                 `json:"live_capacity"`
	PoolRuleVersion uint64              `json:"pool_rule_version"`
}

var (
	goldenClock    = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?( [A-Z]{2,5})?`)
	goldenTimeOnly = regexp.MustCompile(`\b\d{2}:\d{2}:\d{2}\b`)
	goldenPID      = regexp.MustCompile(`PID=\d+`)
	goldenDuration = regexp.MustCompile(`\b\d+(\.\d+)?(ns|µs|us|ms|s|m|h)(\d+(\.\d+)?(ns|µs|us|ms|s|m|h))*\b`)
)

// normalizeLogs 去掉日志里随运行变化的部分：时刻、PID 与耗时
func normalizeLogs(messages []string) []string {
	result := make([]string, 0, len(messages))
	for _, message := range messages {
		message = goldenClock.ReplaceAllString(message, "<时刻>")
		message = goldenTimeOnly.ReplaceAllString(message, "<时刻>")
		message = goldenPID.ReplaceAllString(message, "PID=<pid>")
		message = goldenDuration.ReplaceAllString(message, "<时长>")
		result = append(result, message)
	}
	return result
}

// phase 收集上一个阶段之后新写的运行日志（来源、级别与内容，规范化后）
func (env *goldenEnv) phase(name string) goldenPhase {
	env.requests.mu.Lock()
	entries := slices.Clone(env.requests.logs[env.logged:])
	env.logged = len(env.requests.logs)
	env.requests.mu.Unlock()
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		line := entry.Level + " " + entry.Source + " " + entry.Message
		if entry.Event != "" {
			line += " [" + entry.Event + "]"
		}
		if entry.Request != nil {
			data, _ := json.Marshal(entry.Request)
			line += " " + string(data)
		}
		lines = append(lines, line)
	}
	return goldenPhase{Name: name, Logs: normalizeLogs(lines)}
}

// settle 等后台的启动、预热与回收都结束
func (env *goldenEnv) settle(t *testing.T) {
	t.Helper()
	env.workers.waitPrewarm()
	if !waitUntil(5*time.Second, func() bool { return len(env.workers.OpeningAccountIDs()) == 0 }) {
		t.Fatalf("还有正在启动的 Worker: %v", env.workers.OpeningAccountIDs())
	}
	env.pool.WaitBackground()
}

// generate 在入口 scope 上请求一次模型，返回规范化后的结果
func (env *goldenEnv) generate(t *testing.T, route string, scope aistudio.PoolScope, index int, model string) string {
	t.Helper()
	env.upstream.setRoute(route)
	request := aistudio.GenerateRequest{
		ID: fmt.Sprintf("golden-%s-%d", route, index), Model: model,
		Contents: []aistudio.Content{{Role: aistudio.RoleUser, Parts: []aistudio.Part{{Text: "hi"}}}},
	}
	parent := aistudio.ContextWithPoolScope(env.ctx, scope)
	requestCtx, requestCancel := context.WithTimeout(parent, 20*time.Second)
	defer requestCancel()
	events := make(chan aistudio.Event, 8)
	go env.service.generateWithRetry(parent, requestCtx, requestCancel, time.Now(), request, "", events, newGenerationDiagnostics(request, "未发送"))
	text, failure := "", ""
	var failed error
	for event := range events {
		switch event.Kind {
		case aistudio.EventText:
			text += event.Text
		case aistudio.EventError:
			failed, failure = event.Err, event.Err.Error()
		}
	}
	env.settle(t)
	// 请求结果日志：按 API 层的访问日志投影（号池标记、账户、状态与错误）
	calls := env.upstream.called()
	account := ""
	if len(calls) > 0 {
		account = strings.Fields(calls[len(calls)-1])[1]
	}
	status, path := http.StatusOK, "/v1/chat/completions"
	if route != "default" {
		path = "/" + route + path
	}
	switch {
	case errors.Is(failed, aistudio.ErrModelNotFound):
		status = http.StatusNotFound
	case failed != nil:
		status = http.StatusTooManyRequests
	}
	env.admin.RecordAccessLog(api.AccessLog{
		RequestID: request.ID, Model: model, Method: http.MethodPost, Path: path, Status: status, Pool: requestPoolLabel(parent),
		Account: account, Authorized: true, Error: normalizeLogs([]string{failure})[0], Generation: true,
	})
	result := fmt.Sprintf("%s #%d %s: 正文=%q", route, index, model, text)
	if failure != "" {
		result += " 错误=" + normalizeLogs([]string{failure})[0]
	}
	return result
}

// statusJSON 返回状态接口的 JSON（去掉服务端时刻、版本与启停时间等随运行变化的字段）
func (env *goldenEnv) statusJSON(t *testing.T) map[string]any {
	t.Helper()
	status, err := env.admin.Status(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"time", "version", "transition_since"} {
		delete(value, key)
	}
	return value
}

// models 返回入口的模型列表：模型 ID 与可用通道
func (env *goldenEnv) models(t *testing.T, scope aistudio.PoolScope) []string {
	t.Helper()
	models, err := env.service.Models(aistudio.ContextWithPoolScope(env.ctx, scope))
	if err != nil {
		t.Fatal(err)
	}
	result := make([]string, 0, len(models))
	for _, model := range models {
		channels := make([]string, 0, len(model.Channels))
		for _, channel := range model.Channels {
			channels = append(channels, string(channel))
		}
		result = append(result, model.ID+" "+strings.Join(channels, ","))
	}
	return result
}

// runGoldenScenario 跑一遍金样场景，返回记录
func runGoldenScenario(t *testing.T, options goldenOptions) goldenRecord {
	t.Helper()
	env := newGoldenEnv(t, options)
	var record goldenRecord

	// 1. 预热：各分区按常驻数补齐（启动预热并发为 1，按顺序启动）
	if err := <-env.workers.StartPrewarm(env.ctx); err != nil {
		t.Fatalf("预热失败: %v", err)
	}
	env.settle(t)
	record.Phases = append(record.Phases, env.phase("预热"))

	// 2. 一个 keepWarm 周期（登记、清扫、平均调用、对照权益规则、IP 池容量与补齐预热）
	env.service.keepWarmPass(env.ctx, time.Now())
	env.settle(t)
	record.Phases = append(record.Phases, env.phase("keepWarm"))

	// 3. 默认地址、/pro、/ultra 各 6 个请求：默认地址第一个请求的账户返回分钟限额 429、换号；默认地址请求付费模型按改用入口提示结束
	routes := []struct {
		name   string
		scope  aistudio.PoolScope
		models []string
	}{
		{"default", aistudio.PoolScopeNormal, []string{testRuntimeModel, testRuntimeModel, testRuntimeModel, testRuntimeModel, testRuntimeModel, goldenPaidModel}},
		{"pro", aistudio.PoolScopePro, []string{testRuntimeModel, goldenPaidModel, testRuntimeModel, goldenPaidModel, testRuntimeModel, testRuntimeModel}},
		{"ultra", aistudio.PoolScopeUltra, []string{testRuntimeModel, goldenPaidModel, testRuntimeModel, goldenPaidModel, testRuntimeModel, testRuntimeModel}},
	}
	for _, route := range routes {
		for index, model := range route.models {
			record.Results = append(record.Results, env.generate(t, route.name, route.scope, index+1, model))
		}
		record.Phases = append(record.Phases, env.phase("请求 "+route.name))
	}
	record.Calls = env.upstream.called()

	// 4. 空闲回收一轮：把时刻注入到一小时之后，超出常驻数的空闲 Worker 都满足回收条件
	before := time.Now().Add(time.Hour)
	for env.workers.reapIdleWorker(before) {
	}
	env.settle(t)
	record.Phases = append(record.Phases, env.phase("空闲回收"))

	// 5. 保存一次无关的设置（请求超时），按管理页的方式立即热更新
	value := runtimeConfigDTO(env.cfg)
	value.RequestTimeout = (4 * time.Minute).String()
	if _, err := env.manager.UpdateRuntimeConfig(env.ctx, value); err != nil {
		t.Fatal(err)
	}
	env.settle(t)
	record.Phases = append(record.Phases, env.phase("保存设置"))

	record.Status = env.statusJSON(t)
	record.Models = map[string][]string{
		"default": env.models(t, aistudio.PoolScopeNormal),
		"pro":     env.models(t, aistudio.PoolScopePro),
		"ultra":   env.models(t, aistudio.PoolScopeUltra),
	}
	saved, err := config.Load(env.manager.configPath)
	if err != nil {
		t.Fatal(err)
	}
	proPartition := env.pool.PartitionMembers().Count(aistudio.PoolScopePro) > 0
	record.ProxyCapacity = proxyPoolCapacity(saved, proPartition)
	record.LiveCapacity = env.workers.liveProxyPoolCapacity(proPartition)
	record.PoolRuleVersion = env.pool.PoolRuleVersion()
	return record
}

func encodeGolden(t *testing.T, record goldenRecord) []byte {
	t.Helper()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// goldenDiff 返回两份 JSON 第一处不同的行，便于定位
func goldenDiff(want []byte, got []byte) string {
	wantLines, gotLines := strings.Split(string(want), "\n"), strings.Split(string(got), "\n")
	for index := 0; index < max(len(wantLines), len(gotLines)); index++ {
		var wantLine, gotLine string
		if index < len(wantLines) {
			wantLine = wantLines[index]
		}
		if index < len(gotLines) {
			gotLine = gotLines[index]
		}
		if wantLine != gotLine {
			return fmt.Sprintf("第 %d 行\n期望: %s\n实际: %s", index+1, wantLine, gotLine)
		}
	}
	return ""
}

// TestExtraOffGolden 补充池关闭（默认）时，场景的全部可观察输出与加入补充池之前的代码逐字节相同
func TestExtraOffGolden(t *testing.T) {
	path := filepath.Join("testdata", goldenFile)
	t.Run("关闭", func(t *testing.T) {
		got := encodeGolden(t, runGoldenScenario(t, goldenOptions{caseACooldown: 2 * time.Hour}))
		if *updateGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			return
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取金样失败（先用 -update 生成）: %v", err)
		}
		if string(want) != string(got) {
			t.Fatalf("与金样不同：%s", goldenDiff(want, got))
		}
	})
	// 补充池开启、但没有成员（情形 A 的冷却不到进入门槛，其余账户没有别的模型能用）：上游收到的账户序列与 Worker 启停日志必须与关闭时相同；
	// 号池规则版本、状态接口的补充池字段与模型列表允许不同。默认地址请求付费模型（golden-default-6）是唯一允许不同的请求：开启且
	// EXTRA_POOL_DEFAULT_PAID（默认）时它列在默认地址的 /v1/models 里、只能由补充池服务，没有成员时按 503（模型在列）结束，不按 404 与改用入口
	// 的提示；它在两边都没有发到上游，日志比较时去掉它的行
	t.Run("开启但没有成员", func(t *testing.T) {
		if *updateGolden {
			t.Skip("生成金样时只跑关闭的子用例")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取金样失败（先用 -update 生成）: %v", err)
		}
		var want goldenRecord
		if err := json.Unmarshal(data, &want); err != nil {
			t.Fatal(err)
		}
		got := runGoldenScenario(t, goldenOptions{
			caseACooldown: 10 * time.Minute,
			configure:     func(_ *testing.T, env *goldenEnv) { env.cfg.ExtraPool = true },
			prepare: func(t *testing.T, env *goldenEnv) {
				env.workers.setExtraSettings(extraSettingsOf(env.cfg))
				if !env.pool.SetExtraPoolRules(extraRulesOf(env.cfg)) {
					t.Fatal("开启补充池应改变账户池的补充池规则")
				}
			},
		})
		if !slices.Equal(got.Calls, want.Calls) {
			t.Fatalf("上游收到的账户序列与关闭时不同：\n开启 %v\n关闭 %v", got.Calls, want.Calls)
		}
		const onlyExtra = 5 // 默认地址的第 6 个请求：付费模型
		for index := range got.Results {
			if index == onlyExtra {
				if !strings.Contains(got.Results[index], "在号池的模型列表里") || !strings.Contains(got.Results[index], "补充池此刻没有可借出的 Worker") {
					t.Fatalf("默认地址的付费模型只能由补充池服务、没有成员时应按 503（模型在列）结束：%s", got.Results[index])
				}
				continue
			}
			if got.Results[index] != want.Results[index] {
				t.Fatalf("第 %d 个请求的结果与关闭时不同：\n开启 %s\n关闭 %s", index+1, got.Results[index], want.Results[index])
			}
		}
		dropOnlyExtra(&got)
		dropOnlyExtra(&want)
		if gotLogs, wantLogs := goldenWorkerLogs(got), goldenWorkerLogs(want); !slices.Equal(gotLogs, wantLogs) {
			t.Fatalf("Worker 启停日志与关闭时不同：\n开启 %q\n关闭 %q", gotLogs, wantLogs)
		}
		if got.PoolRuleVersion != want.PoolRuleVersion+1 {
			t.Fatalf("开启补充池应只多递增一次号池规则版本：%d，关闭时 %d", got.PoolRuleVersion, want.PoolRuleVersion)
		}
		// 没有成员时补充池的对账、预热、回收与接手都不写日志（预热目标为 0，没有成员变化）：各阶段的运行日志里没有补充池的行
		for _, phase := range got.Phases {
			for _, line := range phase.Logs {
				if strings.Contains(line, "补充池") {
					t.Fatalf("没有成员时不应写补充池的日志：%s: %s", phase.Name, line)
				}
			}
		}
	})
}

// dropOnlyExtra 去掉默认地址付费模型那个请求（只能由补充池服务，见“开启但没有成员”）的日志行
func dropOnlyExtra(record *goldenRecord) {
	for index := range record.Phases {
		record.Phases[index].Logs = slices.DeleteFunc(record.Phases[index].Logs, func(line string) bool {
			return strings.Contains(line, `"id":"golden-default-6"`)
		})
	}
}

// goldenWorkerLogs 返回各阶段日志中与 Worker 启停有关的行（按阶段顺序）
func goldenWorkerLogs(record goldenRecord) []string {
	var lines []string
	for _, phase := range record.Phases {
		for _, line := range phase.Logs {
			if strings.Contains(line, "Worker") {
				lines = append(lines, phase.Name+": "+line)
			}
		}
	}
	return lines
}
