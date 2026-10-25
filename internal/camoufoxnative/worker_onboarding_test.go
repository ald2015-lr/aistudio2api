package camoufoxnative

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOnboardingURL 为假首次使用页的 location.href
const fakeOnboardingURL = "https://aistudio.google.com/onboarding"

// fakeOnboardedStorage 为假页面完成首次使用之后导出的 localStorage
const fakeOnboardedStorage = `{"origin":"https://aistudio.google.com","localStorage":[{"name":"onboarded","value":"1"}]}`

// onboardingSite 为带首次使用页的假 AI Studio：第 n 次导航后是否落到首次使用页由 land 决定；停在首次使用页时一步式表达式
// 先勾一个必选项、再点继续（点了即离开首次使用页；survey 为真时点了继续先到用途调查一步：继续禁用、报告可以跳过，带 Skip 时点跳过
// 离开），其余表达式按一切正常的官网页面回答。override 返回 true 时替换该表达式的回答（在持有 mu 时调用，可以改 onboarding）。
// 字段在假浏览器的协程里读写，测试读取前加锁
type onboardingSite struct {
	prompt      string
	land        func(navigation int) bool
	override    func(site *onboardingSite, expression string) (fakeEvaluate, bool)
	variants    map[string]onboardingStepOptions
	survey      bool
	mu          sync.Mutex
	onboarding  bool
	checked     bool
	surveying   bool
	navigations int
	detections  int
	stepCalls   int
}

func newOnboardingSite(prompt string, land func(navigation int) bool) *onboardingSite {
	return &onboardingSite{prompt: prompt, land: land, variants: onboardingStepVariants()}
}

func (site *onboardingSite) evaluate(expression string) fakeEvaluate {
	site.mu.Lock()
	defer site.mu.Unlock()
	if site.override != nil {
		if answer, ok := site.override(site, expression); ok {
			return answer
		}
	}
	if options, ok := site.variants[expression]; ok {
		site.stepCalls++
		switch {
		case !site.onboarding:
			return onboardingReply("left", nil)
		case options.Observe:
			return onboardingReply("observe", nil)
		case !site.checked:
			site.checked = true
			return onboardingReply("checked", map[string]any{"label": "* 私は 18 歳以上です"})
		case site.surveying && options.Skip:
			site.onboarding = false
			return onboardingReply("skipped", map[string]any{"button": "スキップ"})
		case site.surveying:
			return onboardingReply("waiting", map[string]any{"reason": "继续按钮未启用", "button": "続行", "skip": "スキップ"})
		case site.survey:
			site.surveying = true
			return onboardingReply("clicked", map[string]any{"button": "続行"})
		}
		site.onboarding = false
		return onboardingReply("clicked", map[string]any{"button": "続行"})
	}
	switch {
	case expression == "location.href" && site.onboarding:
		return fakeEvaluate{value: fakeOnboardingURL}
	case expression == onboardingPageExpression:
		site.detections++
		return fakeEvaluate{value: site.onboarding}
	case strings.Contains(expression, "return window.__aistudioBootstrapRequestBody"):
		return fakeEvaluate{value: `["models/gemini-flash-latest"]`}
	case expression == localStorageExportExpression:
		return fakeEvaluate{value: fakeOnboardedStorage}
	}
	return healthyBootstrapPage(expression, site.prompt)
}

// command 记录导航并决定落到哪个页面；安装拦截时发出一条被拦截的 GenerateContent 请求事件（带官网公共头）
func (site *onboardingSite) command(method string, _ map[string]any) []map[string]any {
	site.mu.Lock()
	defer site.mu.Unlock()
	switch method {
	case "browsingContext.navigate":
		site.navigations++
		site.onboarding = site.land(site.navigations)
		site.checked = false
		site.surveying = false
	case "network.addIntercept":
		header := func(name, value string) map[string]any {
			return map[string]any{"name": name, "value": map[string]any{"type": "string", "value": value}}
		}
		return []map[string]any{{
			"type":   "event",
			"method": "network.beforeRequestSent",
			"params": map[string]any{
				"isBlocked": true,
				"request": map[string]any{
					"request": "request-1",
					"url":     "https://alkalimakersuite-pa.clients6.google.com" + generateContentPath,
					"method":  "POST",
					"headers": []any{header("user-agent", "Mozilla/5.0"), header("x-goog-api-key", "key"),
						header("x-goog-authuser", "0"), header("x-user-agent", "grpc-web-javascript/0.1")},
				},
			},
		}}
	}
	return nil
}

// counts 返回导航、识别与一步式表达式的次数
func (site *onboardingSite) counts() (navigations, detections, stepCalls int) {
	site.mu.Lock()
	defer site.mu.Unlock()
	return site.navigations, site.detections, site.stepCalls
}

// onboardingBootstrap 为一次在假站点上的 bootstrap
type onboardingBootstrap struct {
	err     error
	page    *fakeBiDiPage
	worker  *Worker
	results []OnboardingResult
	stages  []StartupStage
}

func runOnboardingBootstrap(t *testing.T, site *onboardingSite) onboardingBootstrap {
	t.Helper()
	page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{command: site.command})
	run := onboardingBootstrap{page: page}
	// State.PID 取自浏览器进程：给一个只有 Pid 的假进程
	run.worker = &Worker{client: page.client, process: &browserProcess{command: &exec.Cmd{Process: &os.Process{Pid: 4242}}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run.err = run.worker.bootstrap(ctx, Options{
		Model:               "gemini-flash-latest",
		BootstrapPrompt:     site.prompt,
		OnboardingCompleted: func(result OnboardingResult) { run.results = append(run.results, result) },
		StartupProgress:     func(stage StartupStage) { run.stages = append(run.stages, stage) },
	}, storageState{Cookies: []storageCookie{{Name: "SID", Value: "x", Domain: ".google.com", Path: "/", Secure: true}}})
	return run
}

// countMethod 返回假浏览器收到 method 命令的次数
func countMethod(page *fakeBiDiPage, method string) int {
	count := 0
	for _, item := range page.commandMethods() {
		if item == method {
			count++
		}
	}
	return count
}

// TestBootstrapOnboarding bootstrap 遇到首次使用页：一开始就在、或填写提示词时被路由过去，完成首次使用后重新导航、重做到填好提示词，
// 启动成功；完成后又回到首次使用页超过上限时以固定前缀失败；订阅与拦截之后才到首次使用页时就地完成并照常失败，不重复订阅与拦截；
// 不在首次使用页时除多一次识别用的 evaluate 外，命令与原来完全一样
func TestBootstrapOnboarding(t *testing.T) {
	fastOnboarding(t)
	const prompt = "AIStudio2API bootstrap test"
	t.Run("一开始就在首次使用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(navigation int) bool { return navigation == 1 })
		run := runOnboardingBootstrap(t, site)
		if run.err != nil {
			t.Fatalf("完成首次使用后应启动成功: %v", run.err)
		}
		navigations, _, _ := site.counts()
		if navigations != 2 || len(run.results) != 1 || run.results[0].Checked != 1 || run.results[0].Button != "続行" {
			t.Fatalf("应完成一次首次使用并重新导航: navigations=%d results=%+v", navigations, run.results)
		}
		if string(run.results[0].LocalStorage) != fakeOnboardedStorage {
			t.Fatalf("完成后应导出页面的 localStorage: %s", run.results[0].LocalStorage)
		}
		if countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("订阅与拦截只应各一次: %v", run.page.commandMethods("script.evaluate"))
		}
		if run.worker.state.PageURL != fakePageURL || run.worker.state.SnapshotKey != "snapshotKey" {
			t.Fatalf("状态应取自重新导航后的对话页: %+v", run.worker.state)
		}
		loading := 0
		for _, stage := range run.stages {
			if stage == StartupLoadingAIStudio {
				loading++
			}
		}
		if loading != 2 || run.page.snapshots.Load() != 0 {
			t.Fatalf("重新导航时应再报告一次载入 AI Studio，不应采集快照: stages=%v snapshots=%d", run.stages, run.page.snapshots.Load())
		}
	})
	t.Run("从条款到用途调查再到离开", func(t *testing.T) {
		// 多步向导：条款一步勾选、点继续，用途调查一步继续禁用，等页面稳定后点跳过，离开后重新导航、启动成功
		site := newOnboardingSite(prompt, func(navigation int) bool { return navigation == 1 })
		site.survey = true
		run := runOnboardingBootstrap(t, site)
		if run.err != nil {
			t.Fatalf("走完多步向导后应启动成功: %v", run.err)
		}
		navigations, _, _ := site.counts()
		if navigations != 2 || len(run.results) != 1 {
			t.Fatalf("应完成一次首次使用并重新导航: navigations=%d results=%+v", navigations, run.results)
		}
		result := run.results[0]
		if want := []string{"勾选:* 私は 18 歳以上です", "点击:続行", "跳过:スキップ"}; !slices.Equal(result.Steps, want) ||
			result.Checked != 1 || result.Skipped != 1 || result.Button != "続行" || string(result.LocalStorage) != fakeOnboardedStorage {
			t.Fatalf("完成结果应按顺序记下勾选、点击继续与跳过: %+v", result)
		}
		if countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("订阅与拦截只应各一次: %v", run.page.commandMethods("script.evaluate"))
		}
	})
	t.Run("填写提示词时被路由过去", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression != fillPromptExpression(prompt) || site.navigations != 1 {
				return fakeEvaluate{}, false
			}
			// 输入框出现、判定就绪之后 SPA 路由到首次使用页
			site.onboarding = true
			return fakeEvaluate{exception: "提示词输入框不存在"}, true
		}
		run := runOnboardingBootstrap(t, site)
		navigations, _, _ := site.counts()
		if run.err != nil || navigations != 2 || len(run.results) != 1 {
			t.Fatalf("应完成首次使用后重做到填好提示词并启动成功: navigations=%d results=%+v err=%v", navigations, run.results, run.err)
		}
		if countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("重做的一段在订阅之前，订阅与拦截只应各一次: %v", run.page.commandMethods("script.evaluate"))
		}
	})
	t.Run("关闭启动弹层时被路由过去", func(t *testing.T) {
		// 读地址、关弹层、定位 snapshot 函数、安装请求捕获这几步失败时同样识别首次使用页（不只填写提示词那一步）：完成后从重新导航开始重做
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression != dismissOverlaysExpression || site.navigations != 1 {
				return fakeEvaluate{}, false
			}
			site.onboarding = true
			return fakeEvaluate{exception: "启动弹层按钮 匹配多个可见目标"}, true
		}
		run := runOnboardingBootstrap(t, site)
		navigations, _, _ := site.counts()
		if run.err != nil || navigations != 2 || len(run.results) != 1 || run.page.snapshots.Load() != 0 {
			t.Fatalf("应完成首次使用后重做并启动成功: navigations=%d results=%+v snapshots=%d err=%v", navigations, run.results, run.page.snapshots.Load(), run.err)
		}
		if countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("重做的一段在订阅之前，订阅与拦截只应各一次: %v", run.page.commandMethods("script.evaluate"))
		}
	})
	t.Run("超过上限", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return true })
		run := runOnboardingBootstrap(t, site)
		want := OnboardingFailedPrefix + ": 已自动完成 2 次仍回到首次使用页 | 页面={"
		if run.err == nil || !strings.HasPrefix(run.err.Error(), want) || !strings.Contains(run.err.Error(), OnboardingStructureMarker+`{"scope":"ms-onboarding"`) {
			t.Fatalf("超过上限应以固定前缀失败并附快照与结构摘要: %v", run.err)
		}
		navigations, _, _ := site.counts()
		if navigations != 3 || len(run.results) != 2 || countMethod(run.page, "session.subscribe") != 0 {
			t.Fatalf("应完成 2 次、导航 3 次、不订阅: navigations=%d results=%d methods=%v", navigations, len(run.results), run.page.commandMethods("script.evaluate"))
		}
		if reason, ok := PageStateFailure(run.err); ok {
			t.Fatalf("首次使用页未能完成不应算页面状态类: %s", reason)
		}
	})
	t.Run("订阅之后才到首次使用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression != submitPromptExpression {
				return fakeEvaluate{}, false
			}
			site.onboarding = true
			return fakeEvaluate{exception: "官网 Run 按钮不存在"}, true
		}
		run := runOnboardingBootstrap(t, site)
		if run.err == nil || !strings.HasPrefix(run.err.Error(), "提交官网提示词: 页面表达式异常: ") ||
			!strings.Contains(run.err.Error(), "；已完成首次使用页 | 页面={") {
			t.Fatalf("应就地完成并照常失败，原错误前缀不变: %v", run.err)
		}
		checkFakeSnapshot(t, run.err.Error())
		navigations, _, _ := site.counts()
		if navigations != 1 || len(run.results) != 1 || run.page.snapshots.Load() != 1 {
			t.Fatalf("就地完成、不重新导航，快照只采一次: navigations=%d results=%d snapshots=%d", navigations, len(run.results), run.page.snapshots.Load())
		}
		if string(run.results[0].LocalStorage) != fakeOnboardedStorage {
			t.Fatalf("就地完成后应导出页面的 localStorage（启动随后失败、浏览器关闭，只有这一次机会写回）: %s", run.results[0].LocalStorage)
		}
		if countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("不应重复订阅与拦截: %v", run.page.commandMethods("script.evaluate"))
		}
		var interrupted *onboardingInterrupted
		if !errors.As(run.err, &interrupted) {
			t.Fatal("错误链应可识别为首次使用页上的失败")
		}
		if reason, ok := PageStateFailure(run.err); ok {
			t.Fatalf("失败时停在首次使用页不应算页面状态类: %s", reason)
		}
	})
	t.Run("订阅之后识别出首次使用页但随即自行离开", func(t *testing.T) {
		// 识别之后 SPA 自己路由回了对话页：一步式表达式第一次就返回 left，什么都没点，不补“已完成首次使用页”、不报告
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression == submitPromptExpression {
				site.onboarding = true
				return fakeEvaluate{exception: "官网 Run 按钮不存在"}, true
			}
			if _, ok := site.variants[expression]; ok && site.onboarding {
				site.onboarding = false
				return onboardingReply("left", nil), true
			}
			return fakeEvaluate{}, false
		}
		run := runOnboardingBootstrap(t, site)
		message := ""
		if run.err != nil {
			message = run.err.Error()
		}
		if !strings.HasPrefix(message, "提交官网提示词: 页面表达式异常: ") || !strings.Contains(message, "；首次使用页已自行离开 | 页面={") ||
			strings.Contains(message, "已完成首次使用页") || strings.Contains(message, OnboardingStructureMarker) {
			t.Fatalf("什么都没点时应补“首次使用页已自行离开”而不是“已完成”: %s", message)
		}
		if len(run.results) != 0 || countMethod(run.page, "session.subscribe") != 1 || countMethod(run.page, "network.addIntercept") != 1 {
			t.Fatalf("什么都没点时不应报告，不重复订阅与拦截: results=%+v methods=%v", run.results, run.page.commandMethods("script.evaluate"))
		}
		if reason, ok := PageStateFailure(run.err); ok {
			t.Fatalf("失败时停在首次使用页不应算页面状态类: %s", reason)
		}
	})
	t.Run("订阅之后才到首次使用页且未能完成", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression == submitPromptExpression {
				site.onboarding = true
				return fakeEvaluate{exception: "官网 Run 按钮不存在"}, true
			}
			if _, ok := site.variants[expression]; ok && site.onboarding {
				return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"}), true
			}
			return fakeEvaluate{}, false
		}
		run := runOnboardingBootstrap(t, site)
		message := ""
		if run.err != nil {
			message = run.err.Error()
		}
		if !strings.HasPrefix(message, "提交官网提示词: 页面表达式异常: ") ||
			!strings.Contains(message, "；首次使用页未能完成: 找不到可点的继续按钮 | 已做=- | 页面={") ||
			!strings.HasSuffix(message, "}]}") || !strings.Contains(message, OnboardingStructureMarker) {
			t.Fatalf("就地完成失败时原错误前缀不变，补原因并依次接进度记录、快照与结构摘要: %s", message)
		}
		if len(run.results) != 0 {
			t.Fatalf("没能完成时不应报告: %+v", run.results)
		}
	})
	// 浏览器卡住、识别表达式不回答：识别与快照共用 pageSnapshotTimeout，失败与原来只采快照一样很快返回，不等到 ctx（20 秒）到期
	bounded := func(t *testing.T, failing string, answer fakeEvaluate, prefix string) {
		t.Helper()
		site := newOnboardingSite(prompt, func(int) bool { return false })
		failed := false
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			switch {
			case expression == failing:
				failed = true
				return answer, true
			case expression == onboardingPageExpression && failed:
				return fakeEvaluate{noReply: true}, true
			}
			return fakeEvaluate{}, false
		}
		started := time.Now()
		run := runOnboardingBootstrap(t, site)
		elapsed := time.Since(started)
		if run.err == nil || !strings.HasPrefix(run.err.Error(), prefix) || !strings.Contains(run.err.Error(), PageSnapshotMarker+"快照失败: ") {
			t.Fatalf("原错误前缀应不变，快照随识别一起超时: %v", run.err)
		}
		if elapsed > pageSnapshotTimeout+2*time.Second {
			t.Fatalf("识别不回答时应在约 %s 内返回，用了 %s", pageSnapshotTimeout, elapsed)
		}
		var interrupted *onboardingInterrupted
		if errors.As(run.err, &interrupted) || len(run.results) != 0 {
			t.Fatalf("没识别出首次使用页时不应就地完成: %v", run.err)
		}
	}
	t.Run("识别不回答（填写提示词失败）", func(t *testing.T) {
		bounded(t, fillPromptExpression(prompt), fakeEvaluate{exception: "提示词输入框不存在"}, `填写 bootstrap 提示词失败 value="" err=页面表达式异常: `)
	})
	t.Run("识别不回答（订阅之后）", func(t *testing.T) {
		bounded(t, submitPromptExpression, fakeEvaluate{exception: "官网 Run 按钮不存在"}, "提交官网提示词: 页面表达式异常: ")
	})
	t.Run("不在首次使用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		run := runOnboardingBootstrap(t, site)
		if run.err != nil {
			t.Fatalf("正常页面应启动成功: %v", run.err)
		}
		want := []string{"session.new", "browsingContext.getTree", "script.addPreloadScript", "storage.setCookie", "browsingContext.navigate",
			"session.subscribe", "network.addIntercept", "network.failRequest", "network.removeIntercept", "session.unsubscribe"}
		if got := run.page.commandMethods("script.evaluate"); !slices.Equal(got, want) {
			t.Fatalf("evaluate 以外的命令应与原来完全一样\n得到 %v\n期望 %v", got, want)
		}
		navigations, detections, stepCalls := site.counts()
		if navigations != 1 || detections != 1 || stepCalls != 0 || len(run.results) != 0 || run.page.snapshots.Load() != 0 {
			t.Fatalf("只应多一次识别: navigations=%d detections=%d steps=%d results=%d", navigations, detections, stepCalls, len(run.results))
		}
		if run.worker.state.Headers.Get("x-goog-api-key") != "key" || run.worker.state.PID != 4242 {
			t.Fatalf("启动结果不对: %+v", run.worker.state)
		}
	})
}

// loginSite 为登录与验证用的假 AI Studio：停在首次使用页时没有输入框，导航后按 land 决定落到哪里
func loginSite(land func(navigation int) bool) *onboardingSite {
	site := newOnboardingSite("", land)
	site.onboarding = true
	site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
		switch expression {
		case "location.href":
			if site.onboarding {
				return fakeEvaluate{value: fakeOnboardingURL}, true
			}
			return fakeEvaluate{value: aiStudioOrigin + "/prompts/new_chat"}, true
		case promptReadyExpression:
			return fakeEvaluate{value: !site.onboarding}, true
		}
		return fakeEvaluate{}, false
	}
	return site
}

// TestWaitVerificationOnboarding 新账户自动验证停在首次使用页：自动完成后重新载入对话页，继续等到输入框（已登录）；
// 完成后又回到首次使用页超过上限时以固定前缀失败（不是登录失效）
func TestWaitVerificationOnboarding(t *testing.T) {
	fastOnboarding(t)
	t.Run("完成后继续等输入框", func(t *testing.T) {
		site := loginSite(func(int) bool { return false })
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{command: site.command})
		var results []OnboardingResult
		session := &loginSession{client: page.client, contextID: "ctx-1", onboardingCompleted: func(result OnboardingResult) {
			results = append(results, result)
		}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pageURL, authenticated, reason, err := session.waitVerification(ctx)
		if len(results) != 1 || results[0].Checked != 1 || results[0].Button != "続行" {
			t.Fatalf("完成后应报告一次（供验证日志）: %+v", results)
		}
		navigations, _, stepCalls := site.counts()
		if err != nil || !authenticated || reason != "" || pageURL != aiStudioOrigin+"/prompts/new_chat" {
			t.Fatalf("完成首次使用后应验证通过: %q %t %q %v", pageURL, authenticated, reason, err)
		}
		if navigations != 1 || stepCalls < 2 {
			t.Fatalf("应自动完成并重新载入一次: navigations=%d steps=%d", navigations, stepCalls)
		}
	})
	t.Run("超过上限", func(t *testing.T) {
		site := loginSite(func(int) bool { return true })
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{command: site.command})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, authenticated, _, err := session.waitVerification(ctx)
		if err == nil || authenticated || !strings.HasPrefix(err.Error(), OnboardingFailedPrefix+": 已自动完成 2 次仍回到首次使用页") {
			t.Fatalf("超过上限应以固定前缀失败: %t %v", authenticated, err)
		}
		if navigations, _, _ := site.counts(); navigations != 2 {
			t.Fatalf("完成 2 次各重新载入一次，得到 %d 次", navigations)
		}
	})
}

// TestWaitLoginOnboarding 交互式登录停在首次使用页：自动完成后重新载入；自动完成失败时不再点击，交给用户手动完成后照常返回
func TestWaitLoginOnboarding(t *testing.T) {
	fastOnboarding(t)
	t.Run("自动完成", func(t *testing.T) {
		site := loginSite(func(int) bool { return false })
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{command: site.command})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pageURL, err := session.waitLogin(ctx, map[string]storageOrigin{})
		if navigations, _, _ := site.counts(); err != nil || pageURL != aiStudioOrigin+"/prompts/new_chat" || navigations != 1 {
			t.Fatalf("自动完成后应登录成功: %q %v navigations=%d", pageURL, err, navigations)
		}
	})
	t.Run("自动完成失败后手动完成", func(t *testing.T) {
		site := loginSite(func(int) bool { return false })
		// polls 为读地址的次数，stepsAfterFailure 为自动完成失败后第一次读地址时已执行的步数（都在持有 site.mu 时读写）
		polls, stepsAfterFailure := 0, 0
		base := site.override
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if _, ok := site.variants[expression]; ok && site.onboarding {
				site.stepCalls++
				return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"}), true
			}
			if expression == "location.href" {
				polls++
				switch polls {
				case 2:
					// 第 1 次读地址后自动完成，stuck 持续 onboardingTiming.stuck 后失败；之后不应再执行一步式表达式
					stepsAfterFailure = site.stepCalls
				case 3:
					// 用户已在浏览器里手动完成首次使用页
					site.onboarding = false
				}
			}
			return base(site, expression)
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeSnapshot}, fakeBiDiHooks{command: site.command})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pageURL, err := session.waitLogin(ctx, map[string]storageOrigin{})
		if err != nil || pageURL != aiStudioOrigin+"/prompts/new_chat" {
			t.Fatalf("手动完成后应登录成功: %q %v", pageURL, err)
		}
		site.mu.Lock()
		defer site.mu.Unlock()
		if site.navigations != 0 || stepsAfterFailure == 0 || site.stepCalls != stepsAfterFailure {
			t.Fatalf("自动完成失败后不应再点击、不应重新导航: navigations=%d 失败时 %d 步，结束时 %d 步", site.navigations, stepsAfterFailure, site.stepCalls)
		}
	})
}
