package camoufoxnative

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// 拒绝页的假地址（照上游 a68acad 的识别，查询参数里带回跳地址）
const (
	fakeServiceRestrictedURL = "https://accounts.google.com/info/servicerestricted?continue=https://aistudio.google.com/prompts/new_chat&authuser=0"
	fakeAgeRequirementURL    = "https://aistudio.google.com/docs/troubleshoot-ai-studio?hl=en#age-requirements"
	fakeWorkspaceURL         = "https://aistudio.google.com/docs/workspace?authuser=0"
	fakeRegionURL            = "https://aistudio.google.com/docs/available-regions?hl=en"
)

// deniedSnapshot 返回停在 href 的假页面对快照表达式的回答（正文带一个假邮箱，确认快照照常打码）
func deniedSnapshot(href string) fakeEvaluate {
	return fakeEvaluate{value: `{"href":"` + href + `","title":"AI Studio","ready":"complete",` +
		`"box":[0,0],"input":[0,0],"run":[0,0,0],"waa":false,"hook":false,"body":"Signed in as lisa@example.com"}`}
}

// TestDeniedPageURL 拒绝页按地址识别：servicerestricted（accounts.google.com 下，不是登录页）、年龄要求、Workspace 说明页
// 为拒绝账号的种类，地区说明页单独识别（不是账号的问题）；其他 AI Studio 页面、登录页与回跳参数里的地址都不算。
// 写进错误的地址只留 host+path，AI Studio 说明页另带片段；就绪条件包含这些页面
func TestDeniedPageURL(t *testing.T) {
	cases := []struct {
		url    string
		kind   Restriction
		region bool
		path   string
	}{
		{url: fakeServiceNotAllowedURL, kind: RestrictionOrgDisabled, path: "access.workspace.google.com/ServiceNotAllowed"},
		{url: fakeServiceRestrictedURL, kind: RestrictionService, path: "accounts.google.com/info/servicerestricted"},
		{url: "https://ACCOUNTS.google.com/info/servicerestricted/", kind: RestrictionService},
		{url: fakeAgeRequirementURL, kind: RestrictionAge, path: "aistudio.google.com/docs/troubleshoot-ai-studio#age-requirements"},
		{url: fakeWorkspaceURL, kind: RestrictionWorkspace, path: "aistudio.google.com/docs/workspace"},
		{url: fakeRegionURL, region: true, path: "aistudio.google.com/docs/available-regions"},
		{url: "https://accounts.google.com/v3/signin/identifier?continue=https://aistudio.google.com/docs/workspace"},
		{url: "https://accounts.google.com/info/unknownerror"},
		{url: "https://aistudio.google.com/prompts/new_chat?continue=/docs/available-regions"},
		{url: "https://aistudio.google.com/docs/api-key"},
		{url: "https://ai.google.dev/docs/workspace"},
		{url: ""},
		{url: "about:neterror"},
	}
	for _, item := range cases {
		if got := pageRestriction(item.url); got != item.kind {
			t.Errorf("%s 的种类 = %q，期望 %q", item.url, got, item.kind)
		}
		if got := isRegionNotSupportedURL(item.url); got != item.region {
			t.Errorf("%s 是否地区说明页 = %t，期望 %t", item.url, got, item.region)
		}
		denied := item.kind != "" || item.region
		if got := isDeniedPageURL(item.url); got != denied {
			t.Errorf("%s 是否拒绝页 = %t，期望 %t", item.url, got, denied)
		}
		err := deniedPageError(item.url, "")
		if !denied {
			if err != nil {
				t.Errorf("%s 不应给出拒绝页错误: %v", item.url, err)
			}
			continue
		}
		if item.path != "" && !strings.HasSuffix(err.Error(), " url="+item.path) {
			t.Errorf("%s 的错误 = %q，地址应为 %s", item.url, err, item.path)
		}
		if strings.Contains(err.Error(), "continue=") || strings.Contains(err.Error(), "authuser") || strings.Contains(err.Error(), "hl=") {
			t.Errorf("错误里不应带查询参数: %v", err)
		}
		if _, ok := PageStateFailure(err); ok || !deniedPageFailure(err) {
			t.Errorf("拒绝页不应算页面状态类（不清缓存）: %v", err)
		}
		// 被页面状态类的文案包着（例如订阅之后的失败）同样不算
		if _, ok := PageStateFailure(fmt.Errorf("官网 WAA service 未暴露: %w", err)); ok {
			t.Errorf("包着拒绝页的页面状态类错误不应清缓存: %v", err)
		}
		if item.region {
			if !errors.Is(err, ErrRegionNotSupported) || errors.Is(err, ErrServiceNotAllowed) || !strings.HasPrefix(err.Error(), RegionNotSupportedPrefix) {
				t.Errorf("地区说明页应只满足 ErrRegionNotSupported: %v", err)
			}
			continue
		}
		var blocked *ServiceNotAllowedError
		if !errors.Is(err, ErrServiceNotAllowed) || errors.Is(err, ErrRegionNotSupported) || !errors.As(err, &blocked) ||
			blocked.Restriction() != item.kind || !strings.HasPrefix(err.Error(), item.kind.Reason()+" url=") {
			t.Errorf("%s 应为种类 %s 的拒绝账号错误: %v", item.url, item.kind, err)
		}
	}
	for _, fragment := range []string{"'" + serviceNotAllowedHost + "'", "'" + aiStudioHost + "'", "available-regions", "docs\\/workspace", "troubleshoot-ai-studio"} {
		if !strings.Contains(workerPageReadyExpression, fragment) {
			t.Fatalf("就绪条件应包含 %s: %s", fragment, workerPageReadyExpression)
		}
	}
	// 旧版本停在这些页时报“输入框未就绪 url=https://aistudio.google.com/docs/…”：同样不算页面状态类
	for _, address := range []string{fakeAgeRequirementURL, fakeWorkspaceURL, fakeRegionURL} {
		legacy := errors.New(pageInputNotReadyPrefix + " url=" + address + ": context deadline exceeded")
		if _, ok := PageStateFailure(legacy); ok {
			t.Fatalf("停在 %s 的旧文案不应算页面状态类", address)
		}
	}
	if _, ok := PageStateFailure(errors.New(pageInputNotReadyPrefix + " url=https://aistudio.google.com/prompts/new_chat: context deadline exceeded")); !ok {
		t.Fatal("停在对话页的输入框未就绪仍算页面状态类")
	}
	// 保存的种类名还原：空串与不认识的按组织停用
	for value, want := range map[string]Restriction{"": RestrictionOrgDisabled, "bogus": RestrictionOrgDisabled, "age_requirement": RestrictionAge, " service_restricted ": RestrictionService} {
		if got := ParseRestriction(value); got != want {
			t.Fatalf("ParseRestriction(%q) = %q，期望 %q", value, got, want)
		}
	}
	if (&ServiceNotAllowedError{URL: "x"}).Error() != ServiceNotAllowedPrefix+" url=x" {
		t.Fatal("没有种类的错误按组织停用写")
	}
}

// TestBootstrapRestrictedPages bootstrap 导航后落到服务受限、年龄要求、Workspace 说明页：就绪条件立刻成立，按种类以拒绝账号的错误
// 立即失败（servicerestricted 不再当成登录失效），不订阅网络事件；落到地区说明页时以不支持出口地区失败，不算拒绝账号
func TestBootstrapRestrictedPages(t *testing.T) {
	fastOnboarding(t)
	const prompt = "AIStudio2API bootstrap test"
	storage := storageState{Cookies: []storageCookie{{Name: "SID", Value: "x", Domain: ".google.com", Path: "/", Secure: true}}}
	beforeSubscribe := []string{"session.new", "browsingContext.getTree", "script.addPreloadScript", "storage.setCookie", "browsingContext.navigate"}
	cases := []struct {
		name string
		url  string
		kind Restriction
	}{
		{name: "服务受限", url: fakeServiceRestrictedURL, kind: RestrictionService},
		{name: "年龄要求", url: fakeAgeRequirementURL, kind: RestrictionAge},
		{name: "Workspace 未开放", url: fakeWorkspaceURL, kind: RestrictionWorkspace},
		{name: "不支持出口地区", url: fakeRegionURL},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
				switch expression {
				case workerPageReadyExpression:
					return fakeEvaluate{value: true}
				case "location.href":
					return fakeEvaluate{value: item.url}
				case onboardingPageExpression:
					return fakeEvaluate{value: false}
				}
				t.Errorf("拒绝页上不应执行其他页面脚本: %.80s", expression)
				return fakeEvaluate{value: true}
			}, deniedSnapshot(item.url))
			worker := &Worker{client: page.client}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("应立即失败，用了 %s", elapsed)
			}
			if err == nil || strings.Contains(err.Error(), "隔离登录态失效") {
				t.Fatalf("应以拒绝页失败、不是登录失效: %v", err)
			}
			if strings.Contains(err.Error(), "lisa@") || !strings.Contains(err.Error(), PageSnapshotMarker) {
				t.Fatalf("应附打码的快照: %v", err)
			}
			var blocked *ServiceNotAllowedError
			if item.kind == "" {
				if !errors.Is(err, ErrRegionNotSupported) || errors.Is(err, ErrServiceNotAllowed) {
					t.Fatalf("地区说明页应以不支持出口地区失败、不算拒绝账号: %v", err)
				}
			} else if !errors.As(err, &blocked) || blocked.Restriction() != item.kind || !strings.HasPrefix(err.Error(), item.kind.Reason()+" url=") {
				t.Fatalf("应为种类 %s 的拒绝账号错误: %v", item.kind, err)
			}
			if got := page.commandMethods("script.evaluate"); strings.Join(got, ",") != strings.Join(beforeSubscribe, ",") {
				t.Fatalf("导航之后不应再有其他命令\n得到 %v\n期望 %v", got, beforeSubscribe)
			}
		})
	}
	t.Run("填写提示词时被跳到年龄要求页", func(t *testing.T) {
		site := newOnboardingSite(prompt, func(int) bool { return false })
		blocked := false
		site.override = func(site *onboardingSite, expression string) (fakeEvaluate, bool) {
			switch {
			case expression == fillPromptExpression(prompt):
				blocked = true
				return fakeEvaluate{exception: "提示词输入框不存在"}, true
			case expression == "location.href" && blocked:
				return fakeEvaluate{value: fakeAgeRequirementURL}, true
			}
			return fakeEvaluate{}, false
		}
		page := newFakeBiDiPageWithHooks(t, site.evaluate, deniedSnapshot(fakeAgeRequirementURL), fakeBiDiHooks{command: site.command})
		worker := &Worker{client: page.client}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := worker.bootstrap(ctx, Options{Model: "gemini-flash-latest", BootstrapPrompt: prompt}, storage)
		var denied *ServiceNotAllowedError
		if !errors.As(err, &denied) || denied.Restriction() != RestrictionAge {
			t.Fatalf("快照显示停在年龄要求页时应按年龄要求失败: %v", err)
		}
	})
}

// TestWaitVerificationRestrictedPages 验证时页面被跳到服务受限页：立即返回未通过、OrgDisabled 为真、原因与种类为服务受限，
// 不当成登录失效（地址在 accounts.google.com 下）；被跳到地区说明页时返回不支持出口地区的错误（不是未通过、不标记账号）。
// 交互式登录遇到 Workspace 说明页时立即以拒绝账号结束
func TestWaitVerificationRestrictedPages(t *testing.T) {
	fastOnboarding(t)
	verify := func(t *testing.T, address string) (LoginVerification, error) {
		t.Helper()
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			if expression == "location.href" {
				return fakeEvaluate{value: address}
			}
			t.Errorf("拒绝页上不应执行其他页面脚本: %.80s", expression)
			return fakeEvaluate{value: false}
		}, deniedSnapshot(address))
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		started := time.Now()
		verification, err := session.waitVerification(ctx)
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("应立即返回，用了 %s", elapsed)
		}
		return verification, err
	}
	t.Run("服务受限", func(t *testing.T) {
		verification, err := verify(t, fakeServiceRestrictedURL)
		if err != nil || verification.Authenticated || !verification.OrgDisabled || verification.Restriction != RestrictionService ||
			verification.Reason != ServiceRestrictedPrefix || verification.PageURL != fakeServiceRestrictedURL {
			t.Fatalf("应返回未通过与服务受限的原因（不是登录失效）: %+v %v", verification, err)
		}
	})
	t.Run("组织停用仍带种类", func(t *testing.T) {
		verification, err := verify(t, fakeServiceNotAllowedURL)
		if err != nil || !verification.OrgDisabled || verification.Restriction != RestrictionOrgDisabled || verification.Reason != ServiceNotAllowedPrefix {
			t.Fatalf("组织停用 = %+v %v", verification, err)
		}
	})
	t.Run("不支持出口地区", func(t *testing.T) {
		verification, err := verify(t, fakeRegionURL)
		if !errors.Is(err, ErrRegionNotSupported) || verification.OrgDisabled || verification.Reason != "" {
			t.Fatalf("应返回不支持出口地区的错误，不是未通过: %+v %v", verification, err)
		}
	})
	t.Run("交互式登录遇到 Workspace 说明页", func(t *testing.T) {
		polls := 0
		page := newFakeBiDiPageWithSnapshot(t, func(expression string) fakeEvaluate {
			if expression == "location.href" {
				polls++
				if polls == 1 {
					return fakeEvaluate{value: "https://accounts.google.com/v3/signin/identifier"}
				}
				return fakeEvaluate{value: fakeWorkspaceURL}
			}
			return fakeEvaluate{value: false}
		}, deniedSnapshot(fakeWorkspaceURL))
		session := &loginSession{client: page.client, contextID: "ctx-1"}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pageURL, err := session.waitLogin(ctx, map[string]storageOrigin{})
		var blocked *ServiceNotAllowedError
		if pageURL != "" || !errors.As(err, &blocked) || blocked.Restriction() != RestrictionWorkspace {
			t.Fatalf("应以 Workspace 未开放结束: %q %v", pageURL, err)
		}
	})
}

// TestStudioKeyDeniedReason 获取 Key 时页面被跳到拒绝页：原因按种类写（servicerestricted 不再报登录已失效），地区说明页写出口地区，
// 其他页面（登录页、API Keys 页）为空
func TestStudioKeyDeniedReason(t *testing.T) {
	cases := map[string]string{
		fakeServiceNotAllowedURL: StudioKeyReasonOrgDisabled,
		fakeServiceRestrictedURL: ServiceRestrictedPrefix,
		fakeAgeRequirementURL:    AgeRequirementPrefix,
		fakeWorkspaceURL:         WorkspaceDisabledPrefix,
		fakeRegionURL:            RegionNotSupportedPrefix,
		"https://accounts.google.com/v3/signin/identifier": "",
		"https://aistudio.google.com/apikey":               "",
	}
	for address, want := range cases {
		if got := studioKeyDeniedReason(address); got != want {
			t.Errorf("studioKeyDeniedReason(%s) = %q，期望 %q", address, got, want)
		}
	}
}
