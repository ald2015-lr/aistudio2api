package camoufoxnative

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeServiceNotAllowedURL 为组织停用页的 location.href（照线上：查询参数里有应用编号与回跳地址）
const fakeServiceNotAllowedURL = "https://access.workspace.google.com/ServiceNotAllowed?application=151518919664&source=scrip" +
	"&continue=https://aistudio.google.com/prompts/new_chat?model%3Dgemini-flash-latest&pli=1"

// fakeServiceNotAllowedSnapshot 为假组织停用页对快照表达式的回答（照线上快照）
const fakeServiceNotAllowedSnapshot = `{"href":"` + fakeServiceNotAllowedURL + `","title":"Service not allowed","ready":"complete",` +
	`"box":[0,0],"input":[0,0],"run":[0,0,0],"waa":false,"hook":false,` +
	`"body":"Admin We are sorry, but you do not have access to AI Studio. Contact lisa@example.com"}`

// serviceNotAllowedPrefixWant 为组织停用错误的固定开头（地址只留 host+path）
const serviceNotAllowedPrefixWant = ServiceNotAllowedPrefix + " url=access.workspace.google.com/ServiceNotAllowed"

// checkServiceNotAllowed 确认错误为组织停用：可用 errors.Is / errors.As 识别，文案以固定前缀开头、地址不带查询参数，
// 末尾带打码的页面快照，不算页面状态类（不清缓存）
func checkServiceNotAllowed(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("应以组织停用失败")
	}
	var blocked *ServiceNotAllowedError
	if !errors.Is(err, ErrServiceNotAllowed) || !errors.As(err, &blocked) || blocked.URL != "access.workspace.google.com/ServiceNotAllowed" {
		t.Fatalf("错误应可用 errors.Is/As 识别为组织停用: %v", err)
	}
	message := err.Error()
	if !strings.HasPrefix(message, serviceNotAllowedPrefixWant+PageSnapshotMarker+"{") {
		t.Fatalf("文案应以固定前缀开头并附快照\n得到 %s\n期望前缀 %s", message, serviceNotAllowedPrefixWant)
	}
	if strings.Contains(message, "application=") || strings.Contains(message, "lisa@") {
		t.Fatalf("文案里不应有查询参数与邮箱: %s", message)
	}
	if decoded := snapshotJSON(t, message); decoded["url"] != "access.workspace.google.com/ServiceNotAllowed" || decoded["title"] != "Service not allowed" {
		t.Fatalf("快照应是组织停用页: %v", decoded)
	}
	if reason, ok := PageStateFailure(err); ok {
		t.Fatalf("组织停用不应算页面状态类（不清缓存）: %s", reason)
	}
}

// TestBootstrapServiceNotAllowed bootstrap 导航后落到组织停用页（access.workspace.google.com）：就绪条件立刻成立，
// 以固定前缀失败、不等 120 秒、不订阅网络事件；等就绪超时时页面恰好落到这一页、填写提示词或订阅之后才被跳过去、
// 完成首次使用页后重新导航落到这一页，同样按组织停用失败
func TestBootstrapServiceNotAllowed(t *testing.T) {
	fastOnboarding(t)
	const prompt = "AIStudio2API bootstrap test"
	storage := storageState{Cookies: []storageCookie{{Name: "SID", Value: "x", Domain: ".google.com", Path: "/", Secure: true}}}
	beforeSubscribe := []string{"session.new", "browsingContext.getTree", "script.addPreloadScript", "storage.setCookie", "browsingContext.navigate"}
	t.Run("导航后落到组织停用页", func(t *testing.T) {
		readyChecks := 0
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			switch expression {
			case workerPageReadyExpression:
				readyChecks++
				return fakeEvaluate{value: true}
			case "location.href":
				return fakeEvaluate{value: fakeServiceNotAllowedURL}
			case onboardingPageExpression:
				return fakeEvaluate{value: false}
			}
			t.Errorf("组织停用页上不应执行其他页面脚本: %.80s", expression)
			return fakeEvaluate{value: true}
		}, fakeEvaluate{value: fakeServiceNotAllowedSnapshot})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("应立即失败，用了 %s", elapsed)
		}
		checkServiceNotAllowed(t, err)
		if got := page.commandMethods("script.evaluate"); !slices.Equal(got, beforeSubscribe) {
			t.Fatalf("导航之后不应再有其他命令\n得到 %v\n期望 %v", got, beforeSubscribe)
		}
		if readyChecks != 1 || page.snapshots.Load() != 1 {
			t.Fatalf("就绪条件应只检查一次、快照只采一次: ready=%d snapshots=%d", readyChecks, page.snapshots.Load())
		}
	})
	t.Run("等就绪超时时落到组织停用页", func(t *testing.T) {
		// 初始化期限先到（跳转很慢）：失败处读到的地址在组织停用页，按组织停用失败，不再报“输入框未就绪”
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			switch expression {
			case workerPageReadyExpression:
				return fakeEvaluate{value: false}
			case "location.href":
				return fakeEvaluate{value: fakeServiceNotAllowedURL}
			case onboardingPageExpression:
				return fakeEvaluate{value: false}
			}
			return fakeEvaluate{value: true}
		}, fakeEvaluate{value: fakeServiceNotAllowedSnapshot})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
		defer cancel()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		checkServiceNotAllowed(t, err)
		if strings.Contains(err.Error(), pageInputNotReadyPrefix) {
			t.Fatalf("不应再报输入框未就绪: %v", err)
		}
	})
	t.Run("填写提示词时被跳到组织停用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		blocked := false
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			switch {
			case expression == fillPromptExpression(prompt):
				blocked = true
				return fakeEvaluate{exception: "提示词输入框不存在"}, true
			case expression == "location.href" && blocked:
				return fakeEvaluate{value: fakeServiceNotAllowedURL}, true
			}
			return fakeEvaluate{}, false
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeServiceNotAllowedSnapshot}, fakeBiDiHooks{command: site.command})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		checkServiceNotAllowed(t, err)
		if navigations, _, steps := site.counts(); navigations != 1 || steps != 0 || page.snapshots.Load() != 1 {
			t.Fatalf("不应重新导航或处理首次使用页，快照只采一次: navigations=%d steps=%d snapshots=%d", navigations, steps, page.snapshots.Load())
		}
		if countMethod(page, "session.subscribe") != 0 {
			t.Fatalf("不应订阅网络事件: %v", page.commandMethods("script.evaluate"))
		}
	})
	t.Run("订阅之后被跳到组织停用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression == submitPromptExpression {
				return fakeEvaluate{exception: "官网 Run 按钮不存在"}, true
			}
			return fakeEvaluate{}, false
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeServiceNotAllowedSnapshot}, fakeBiDiHooks{command: site.command})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		checkServiceNotAllowed(t, err)
		if countMethod(page, "session.subscribe") != 1 || countMethod(page, "network.addIntercept") != 1 || page.snapshots.Load() != 1 {
			t.Fatalf("订阅与拦截各一次、快照只采一次: methods=%v snapshots=%d", page.commandMethods("script.evaluate"), page.snapshots.Load())
		}
	})
	t.Run("完成首次使用后重新导航落到组织停用页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(navigation int) bool { return navigation == 1 })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if expression == "location.href" && site.navigations >= 2 {
				return fakeEvaluate{value: fakeServiceNotAllowedURL}, true
			}
			return fakeEvaluate{}, false
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeServiceNotAllowedSnapshot}, fakeBiDiHooks{command: site.command})
		var results []OnboardingResult
		worker := &Worker{client: page.client, process: &browserProcess{command: &exec.Cmd{Process: &os.Process{Pid: 4242}}}}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		err := worker.bootstrap(ctx, Options{
			Model: "gemini-flash-latest", BootstrapPrompt: prompt,
			OnboardingCompleted: func(result OnboardingResult) { results = append(results, result) },
		}, storage)
		checkServiceNotAllowed(t, err)
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("重新导航后应立即失败，用了 %s", elapsed)
		}
		if navigations, _, _ := site.counts(); navigations != 2 || len(results) != 1 || countMethod(page, "session.subscribe") != 0 {
			t.Fatalf("应完成一次首次使用、重新导航一次后失败，不订阅: navigations=%d results=%d methods=%v",
				navigations, len(results), page.commandMethods("script.evaluate"))
		}
	})
	t.Run("首次使用页没能完成时页面已在组织停用页", func(t *testing.T) {
		// 一步式表达式一直找不到按钮：没能完成时的快照显示页面已被跳到组织停用页，按组织停用失败，不报首次使用页未能完成
		site := newOnboardingSite(prompt, func(int) bool { return true })
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if _, ok := site.variants[expression]; ok && site.onboarding {
				site.stepCalls++
				return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"}), true
			}
			return fakeEvaluate{}, false
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeServiceNotAllowedSnapshot}, fakeBiDiHooks{command: site.command})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		checkServiceNotAllowed(t, err)
		if strings.Contains(err.Error(), OnboardingFailedPrefix) || onboardingRelated(err) {
			t.Fatalf("不应报首次使用页未能完成: %v", err)
		}
	})
}

// TestWaitVerificationServiceNotAllowed 验证时页面被跳到组织停用页：立即返回未通过、OrgDisabled 为真、原因为固定文案，
// 不等到验证超时；完成首次使用页途中被跳过去也一样。交互式登录遇到时立即以组织停用结束
func TestWaitVerificationServiceNotAllowed(t *testing.T) {
	fastOnboarding(t)
	t.Run("验证时直接落到组织停用页", func(t *testing.T) {
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			if expression == "location.href" {
				return fakeEvaluate{value: fakeServiceNotAllowedURL}
			}
			t.Errorf("组织停用页上不应执行其他页面脚本: %.80s", expression)
			return fakeEvaluate{value: false}
		}, fakeEvaluate{value: fakeServiceNotAllowedSnapshot})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		verification, err := session.waitVerification(ctx)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("应立即返回，用了 %s", elapsed)
		}
		if err != nil || verification.Authenticated || !verification.OrgDisabled || verification.Reason != ServiceNotAllowedPrefix ||
			verification.PageURL != fakeServiceNotAllowedURL {
			t.Fatalf("应返回未通过与组织停用的原因: %+v %v", verification, err)
		}
	})
	t.Run("完成首次使用页途中被跳到组织停用页", func(t *testing.T) {
		site := loginSite(func(int) bool { return true })
		base := site.override
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			if _, ok := site.variants[expression]; ok && site.onboarding {
				site.stepCalls++
				return onboardingReply("stuck", map[string]any{"reason": "找不到可点的继续按钮"}), true
			}
			return base(site, expression)
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, fakeEvaluate{value: fakeServiceNotAllowedSnapshot}, fakeBiDiHooks{command: site.command})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		verification, err := session.waitVerification(ctx)
		if err != nil || verification.Authenticated || !verification.OrgDisabled || verification.Reason != ServiceNotAllowedPrefix {
			t.Fatalf("应返回未通过与组织停用的原因（不是首次使用页未能完成）: %+v %v", verification, err)
		}
		if navigations, _, _ := site.counts(); navigations != 0 {
			t.Fatalf("不应重新载入对话页: navigations=%d", navigations)
		}
	})
	t.Run("交互式登录遇到组织停用页", func(t *testing.T) {
		polls := 0
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			if expression == "location.href" {
				polls++
				if polls == 1 {
					// 用户还在登录页
					return fakeEvaluate{value: "https://accounts.google.com/v3/signin/identifier"}
				}
				return fakeEvaluate{value: fakeServiceNotAllowedURL}
			}
			return fakeEvaluate{value: false}
		}, fakeEvaluate{value: fakeServiceNotAllowedSnapshot})
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		pageURL, err := session.waitLogin(ctx, map[string]storageOrigin{})
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("应立即结束，用了 %s", elapsed)
		}
		if pageURL != "" {
			t.Fatalf("不应返回登录成功的页面: %q", pageURL)
		}
		checkServiceNotAllowed(t, err)
	})
}

// TestServiceNotAllowedURL 组织停用页按域名识别（不区分大小写），地址只留 host+path；旧文案“输入框未就绪 url=https://access…”同样不算页面状态类
func TestServiceNotAllowedURL(t *testing.T) {
	for _, value := range []string{fakeServiceNotAllowedURL, "https://ACCESS.workspace.google.com/AccessDenied"} {
		if !isServiceNotAllowedURL(value) {
			t.Fatalf("%s 应识别为组织停用页", value)
		}
	}
	for _, value := range []string{"https://aistudio.google.com/prompts/new_chat?continue=https://access.workspace.google.com/", "https://accounts.google.com/", "", "about:neterror"} {
		if isServiceNotAllowedURL(value) {
			t.Fatalf("%s 不应识别为组织停用页", value)
		}
	}
	if got := serviceNotAllowedPath(fakeServiceNotAllowedURL); got != "access.workspace.google.com/ServiceNotAllowed" {
		t.Fatalf("地址应只留 host+path: %s", got)
	}
	if !strings.Contains(workerPageReadyExpression, "location.hostname === 'access.workspace.google.com'") {
		t.Fatal("就绪条件应包含组织停用页")
	}
	legacy := errors.New("AI Studio 输入框未就绪 url=" + fakeServiceNotAllowedURL + ": context deadline exceeded")
	if _, ok := PageStateFailure(legacy); ok {
		t.Fatal("停在组织停用页的旧文案不应算页面状态类")
	}
}
