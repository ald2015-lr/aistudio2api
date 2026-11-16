package camoufoxnative

import (
	"strings"
	"testing"
	"time"
)

// TestStudioKeyLabels 多语言文字分类：新建项目、导入、没有项目、创建 Key 的确认（带“项目”字样的不算）、取消、验证账号、条款与邮件订阅
func TestStudioKeyLabels(t *testing.T) {
	for _, text := range []string{"+ Create project", "Create a new project", "New project", "创建项目", "新建项目", "建立專案", "新增專案",
		"新しいプロジェクト", "プロジェクトを作成", "새 프로젝트", "프로젝트 만들기"} {
		if !isCreateProjectLabel(text, "") {
			t.Errorf("%q 应认作新建项目", text)
		}
	}
	if !isCreateProjectLabel("Project", "add") || isCreateProjectLabel("Import project", "add") {
		t.Error("加号图标加“项目”字样应认作新建项目，导入除外")
	}
	for _, text := range []string{"Default Gemini Project", "gen-lang-client-0123456789", "Import project", "Create key", "Create API key"} {
		if isCreateProjectLabel(text, "") {
			t.Errorf("%q 不应认作新建项目", text)
		}
	}
	for _, text := range []string{"Create key", "Create API key", "Generate key", "创建密钥", "创建 API 密钥", "建立 API 金鑰", "キーを作成", "API キーを作成", "키 만들기"} {
		if !isKeyConfirmLabel(text) {
			t.Errorf("%q 应认作创建 Key 的确认", text)
		}
	}
	for _, text := range []string{"Create project", "Create API key in new project", "Create API key in existing project", "Cancel"} {
		if isKeyConfirmLabel(text) {
			t.Errorf("%q 不应认作创建 Key 的确认", text)
		}
	}
	options := map[string]string{
		"Default Gemini Project":      studioKeyOptionExisting,
		"gen-lang-client-0123456789":  studioKeyOptionExisting,
		"+ Create project":            studioKeyOptionCreate,
		"新建项目":                        studioKeyOptionCreate,
		"Import project":              studioKeyOptionImport,
		"导入项目":                        studioKeyOptionImport,
		"プロジェクトをインポート":                studioKeyOptionImport,
		"No Cloud Projects Available": studioKeyOptionNone,
		"没有可用的 Cloud 项目":              studioKeyOptionNone,
		"Choose an imported project":  studioKeyOptionOther,
	}
	for text, want := range options {
		if got := classifyStudioKeyOption(studioKeyControl{Text: text, Enabled: true}); got != want {
			t.Errorf("选项 %q 分类为 %s，期望 %s", text, got, want)
		}
	}
	if got := classifyStudioKeyOption(studioKeyControl{Text: "Some project", Enabled: false}); got != studioKeyOptionOther {
		t.Errorf("不可用的选项不算已有项目: %s", got)
	}
	if got := classifyStudioKeyOption(studioKeyControl{Text: "Learn more", Enabled: true, Button: true}); got != studioKeyOptionOther {
		t.Errorf("下拉面板里的普通按钮不算已有项目: %s", got)
	}
	for _, text := range []string{"Create", "Create project", "创建", "建立", "作成", "만들기", "OK"} {
		if !studioKeyLabels.projectConfirm.MatchString(text) && !isCreateProjectLabel(text, "") {
			t.Errorf("%q 应认作新建项目的确认", text)
		}
	}
	for _, text := range []string{"Cancel", "取消", "キャンセル", "취소", "Not now", "Close", "Go back", "Maybe later"} {
		if !studioKeyLabels.cancel.MatchString(text) {
			t.Errorf("%q 应认作取消", text)
		}
	}
	// 英文按整词：“Send feedback”“Disclose”“Playback”不是关闭类按钮
	for _, text := range []string{"Send feedback", "Disclose", "Playback", "Closed beta"} {
		if studioKeyLabels.cancel.MatchString(text) {
			t.Errorf("%q 不应认作取消", text)
		}
	}
	// 名字里碰巧带“no project”“没有…项目”的已有项目不算“没有可用项目”的提示；不可用的选项里带提示文字的算
	for _, text := range []string{"Nano project", "Casino Project", "Piano project", "我的没有名字的项目"} {
		if got := classifyStudioKeyOption(studioKeyControl{Text: text, Enabled: true}); got != studioKeyOptionExisting {
			t.Errorf("选项 %q 分类为 %s，应为已有项目", text, got)
		}
		if !selectHasProject(&studioKeySelect{Value: text}) {
			t.Errorf("下拉框值 %q 应算选着项目", text)
		}
	}
	if got := classifyStudioKeyOption(studioKeyControl{Text: "You have no Cloud projects yet", Enabled: false}); got != studioKeyOptionNone {
		t.Errorf("不可用、带提示文字的选项应为没有项目: %s", got)
	}
	for _, text := range []string{"Your account must be verified before it can continue using the Gemini API.", "Verify your account",
		"请验证您的账号", "アカウントを確認してください", "계정 인증"} {
		if !studioKeyLabels.verify.MatchString(text) {
			t.Errorf("%q 应认作验证账号提示", text)
		}
	}
	if !studioKeyLabels.terms.MatchString("I agree to the Google Cloud Terms of Service *") || studioKeyLabels.optional.MatchString("I agree to the Google Cloud Terms of Service *") {
		t.Error("服务条款应认作条款、不是订阅")
	}
	if !studioKeyLabels.optional.MatchString("Email me with product updates and offers") {
		t.Error("邮件订阅应认作可选项")
	}
}

// TestSelectHasProject 下拉框的值是占位、新建、导入或“没有项目”时算没选
func TestSelectHasProject(t *testing.T) {
	cases := map[string]bool{"Default Gemini Project": true, "Choose an imported project": false, "+ Create project": false,
		"Import project": false, "No Cloud Projects Available": false, "": false}
	for value, want := range cases {
		if got := selectHasProject(&studioKeySelect{Value: value}); got != want {
			t.Errorf("下拉框值 %q：selectHasProject=%v，期望 %v", value, got, want)
		}
	}
	if selectHasProject(&studioKeySelect{Value: "Gemini Project", Empty: true}) || selectHasProject(nil) {
		t.Error("结构上为空的下拉框算没选")
	}
}

// studioKeyFlowDriver 按虚拟时钟驱动 studioKeyFlow：每一步先 observe 再 decide，点击与导航按已发出处理
type studioKeyFlowDriver struct {
	t    *testing.T
	flow *studioKeyFlow
	now  time.Time
}

func newStudioKeyFlowDriver(t *testing.T, before studioKeyPage) *studioKeyFlowDriver {
	driver := &studioKeyFlowDriver{t: t, flow: newStudioKeyFlow(), now: time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)}
	driver.flow.opened(&before, driver.now)
	return driver
}

// step 前进 advance 后读一次页面、决定下一步
func (driver *studioKeyFlowDriver) step(page studioKeyPage, advance time.Duration) studioKeyStep {
	driver.now = driver.now.Add(advance)
	driver.flow.observe(page, driver.now)
	step := driver.flow.decide(page, driver.now)
	if step.Kind == studioKeyStepClick || step.Kind == studioKeyStepNavigate {
		driver.flow.apply(step, driver.now)
	}
	return step
}

// expectClick 断言下一步是点击 scope 里文字为 text 的控件
func (driver *studioKeyFlowDriver) expectClick(page studioKeyPage, advance time.Duration, scope string, text string) studioKeyStep {
	driver.t.Helper()
	step := driver.step(page, advance)
	if step.Kind != studioKeyStepClick || step.Target.Scope != scope || text != "" && step.Target.Text != text {
		driver.t.Fatalf("期望点击 %s %q，得到 %+v（步骤 %v）", scope, text, step, driver.flow.steps)
	}
	return step
}

func (driver *studioKeyFlowDriver) expectWait(page studioKeyPage, advance time.Duration) {
	driver.t.Helper()
	if step := driver.step(page, advance); step.Kind != studioKeyStepWait {
		driver.t.Fatalf("期望等待，得到 %+v（步骤 %v）", step, driver.flow.steps)
	}
}

func (driver *studioKeyFlowDriver) expectFail(page studioKeyPage, advance time.Duration, reason string) studioKeyStep {
	driver.t.Helper()
	step := driver.step(page, advance)
	if step.Kind != studioKeyStepFail || !strings.HasPrefix(step.Reason, reason) {
		driver.t.Fatalf("期望失败 %q，得到 %+v（步骤 %v）", reason, step, driver.flow.steps)
	}
	return step
}

func testApikeyPage(dialog *studioKeyDialog, options ...studioKeyControl) studioKeyPage {
	return studioKeyPage{Host: "aistudio.google.com", Path: StudioKeyPath, Dialog: dialog, Options: options, Create: "enabled"}
}

func testKeyDialog(selection *studioKeySelect, confirmEnabled bool) *studioKeyDialog {
	return &studioKeyDialog{
		Title: "Create API key", Text: "Create API key Name your key Choose an imported project Cancel Create key",
		Buttons: []studioKeyControl{{Index: 0, Text: "Cancel", Enabled: true}, {Index: 1, Text: "Create key", Enabled: confirmEnabled}},
		Select:  selection, Inputs: []studioKeyInput{{Index: 0, Label: "Name your key"}},
	}
}

func testEmptySelect() *studioKeySelect {
	return &studioKeySelect{Empty: true, Placeholder: "Choose an imported project"}
}

func testProjectDialog(nameEmpty bool, termsChecked bool, confirmEnabled bool) *studioKeyDialog {
	return &studioKeyDialog{
		Title: "Create a new project", Text: "Create a new project Project name Cancel Create project",
		Buttons: []studioKeyControl{{Index: 0, Text: "Cancel", Enabled: true}, {Index: 1, Text: "Create project", Enabled: confirmEnabled}},
		Inputs:  []studioKeyInput{{Index: 0, Label: "Project name", Empty: nameEmpty, Required: true}},
		Boxes: []studioKeyBox{
			{Index: 0, Label: "I agree to the Google Cloud Terms of Service *", Checked: termsChecked, Required: true},
			{Index: 1, Label: "Email me with product updates and offers"},
		},
	}
}

var testNoProjectOptions = []studioKeyControl{
	{Index: 0, Text: "Import project", Enabled: true, Icon: "download"},
	{Index: 1, Text: "No Cloud Projects Available", Enabled: false},
	{Index: 2, Text: "Create project", Enabled: true, Icon: "add"},
}

// TestStudioKeyFlowCreatesProjectThenKey 没有项目（需要验证账号的账户常见）：展开下拉框只有导入、“没有项目”与“Create project”，
// 点一次新建项目，勾必需的服务条款（邮件订阅不勾），确认一次；新项目出现在响应里、下拉框选上它之后点一次“Create key”。
// 之后页面再要求新建项目也不再点
func TestStudioKeyFlowCreatesProjectThenKey(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	expanded := testEmptySelect()
	expanded.Expanded = true
	// 列表刚展开：先等它稳定（异步加载的项目可能稍后才出现），不急着新建
	driver.expectWait(testApikeyPage(testKeyDialog(expanded, false), testNoProjectOptions...), time.Second)
	step := driver.expectClick(testApikeyPage(testKeyDialog(expanded, false), testNoProjectOptions...), studioKeyTiming.listSettle, "option", "Create project")
	if step.Target.Index != 2 || step.Mark != studioKeyMarkProjectEntry {
		t.Fatalf("应点第 3 个选项并记为新建项目入口: %+v", step)
	}
	step = driver.expectClick(testApikeyPage(testProjectDialog(false, false, false)), time.Second, "box", "I agree to the Google Cloud Terms of Service *")
	if step.Target.Index != 0 {
		t.Fatalf("应勾服务条款: %+v", step)
	}
	driver.expectClick(testApikeyPage(testProjectDialog(false, true, true)), time.Second, "dialog", "Create project")
	busy := testProjectDialog(false, true, false)
	busy.Busy = true
	driver.expectWait(testApikeyPage(busy), time.Second)
	// 建好之前：对话框关了、下拉框仍为空，等一会儿（不再点新建）
	driver.expectWait(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second)
	driver.flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"}})
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Gemini Project"}, true)), time.Second, "dialog", "Create key")
	driver.expectWait(testApikeyPage(&studioKeyDialog{Title: "API key created", Text: "API key created <key> Copy Done"}), time.Second)
	if !driver.flow.projectCreated() || len(driver.flow.newProjects) != 1 || driver.flow.newProjects[0].Number != "123456789012" {
		t.Fatalf("应记下新建的项目: %+v", driver.flow.newProjects)
	}
	steps := strings.Join(driver.flow.steps, "→")
	for _, want := range []string{"打开创建对话框", "展开项目下拉框", "下拉框里没有已有项目", "点击新建项目“Create project”", "勾选条款“I agree", "确认新建项目“Create project”",
		"新项目已出现（projects/123456789012）", "确认创建 Key“Create key”"} {
		if !strings.Contains(steps, want) {
			t.Fatalf("步骤缺少 %q: %s", want, steps)
		}
	}
	if strings.Contains(steps, "Email me") {
		t.Fatalf("邮件订阅不应勾选: %s", steps)
	}
	// 一次运行只建一个项目、一个 Key：页面再给出新建项目的入口也不点
	again := testKeyDialog(nil, false)
	again.Buttons = []studioKeyControl{{Index: 0, Text: "Cancel", Enabled: true}, {Index: 1, Text: "Create project", Enabled: true}}
	flow := driver.flow
	flow.keyConfirmAt = time.Time{}
	if step := driver.step(testApikeyPage(again), time.Second); step.Kind == studioKeyStepClick && step.Mark == studioKeyMarkProjectEntry {
		t.Fatalf("同一次运行不应再新建项目: %+v", step)
	}
	if step := driver.step(testApikeyPage(testProjectDialog(false, true, true)), time.Second); step.Kind == studioKeyStepClick && step.Mark == studioKeyMarkProjectConfirm {
		t.Fatalf("同一次运行不应再确认新建项目: %+v", step)
	}
}

// TestStudioKeyFlowUsesExistingProject 下拉框里有已有项目时选它（优先 Default Gemini Project），不新建项目；之后再出现新建项目也不点
func TestStudioKeyFlowUsesExistingProject(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	options := []studioKeyControl{
		{Index: 0, Text: "Other project", Enabled: true},
		{Index: 1, Text: "Default Gemini Project gen-lang-client-0123456789", Enabled: true},
		{Index: 2, Text: "Create project", Enabled: true, Icon: "add"},
	}
	step := driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false), options...), time.Second, "option", "")
	if step.Target.Index != 1 || step.Mark != studioKeyMarkPick {
		t.Fatalf("应选 Default Gemini Project: %+v", step)
	}
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Default Gemini Project"}, true)), time.Second, "dialog", "Create key")
	if driver.flow.projectStarted() || driver.flow.projectCreated() {
		t.Fatal("有已有项目时不应新建项目")
	}
	// 有已有项目时，即使下拉框里只剩新建项目的选项也不点
	fresh := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	fresh.flow.existingOffered = true
	if step := fresh.step(testApikeyPage(testKeyDialog(testEmptySelect(), false), testNoProjectOptions...), time.Second); step.Kind == studioKeyStepClick {
		t.Fatalf("出现过已有项目后不应点新建项目: %+v", step)
	}
}

// TestStudioKeyFlowPreselectedProject 对话框里已经选着项目、确认按钮可用：直接点“Create key”（原有账户的常见情形）
func TestStudioKeyFlowPreselectedProject(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Default Gemini Project"}, true)), time.Second, "dialog", "Create key")
	// 没有下拉框的旧版对话框
	plain := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	plain.expectClick(testApikeyPage(testKeyDialog(nil, true)), time.Second, "dialog", "Create key")
}

// TestStudioKeyFlowProjectsPageFallback 下拉框里只有导入与“没有项目”、对话框里也没有新建项目的入口：去一次 Projects 页，点它自己的
// 新建项目按钮、确认，新项目出现后回到 API Keys 页重新打开创建对话框，选上新项目再创建 Key
func TestStudioKeyFlowProjectsPageFallback(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	driver.expectWait(testApikeyPage(testKeyDialog(testEmptySelect(), false), testNoProjectOptions[:2]...), time.Second)
	step := driver.step(testApikeyPage(testKeyDialog(testEmptySelect(), false), testNoProjectOptions[:2]...), studioKeyTiming.listSettle)
	if step.Kind != studioKeyStepNavigate || step.Path != studioKeyProjectsPath {
		t.Fatalf("应打开 Projects 页: %+v", step)
	}
	projects := studioKeyPage{Host: "aistudio.google.com", Path: "/projects", Project: "Create a new project"}
	driver.expectWait(studioKeyPage{Host: "aistudio.google.com", Path: "/projects"}, time.Second)
	// 先等 Projects 页加载项目列表，再点新建
	driver.expectWait(projects, time.Second)
	driver.expectClick(projects, 3*studioKeyTiming.listSettle, "page-project", "Create a new project")
	driver.expectClick(studioKeyPage{Host: "aistudio.google.com", Path: "/projects", Dialog: testProjectDialog(false, true, true)}, time.Second, "dialog", "Create project")
	driver.expectWait(studioKeyPage{Host: "aistudio.google.com", Path: "/projects"}, time.Second)
	driver.flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012", Display: "Gemini Project"}})
	step = driver.step(studioKeyPage{Host: "aistudio.google.com", Path: "/projects"}, 3*time.Second)
	if step.Kind != studioKeyStepNavigate || step.Path != StudioKeyPath {
		t.Fatalf("新项目出现后应回到 API Keys 页: %+v", step)
	}
	// 回来后先等 settle（Key 列表的响应可能带着新项目自带的 Key），再点“Create API key”
	driver.expectWait(testApikeyPage(nil), 500*time.Millisecond)
	driver.expectWait(testApikeyPage(nil), studioKeyTiming.settle/2)
	driver.expectClick(testApikeyPage(nil), studioKeyTiming.settle/2, "page-create", "")
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	step = driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false),
		studioKeyControl{Index: 0, Text: "Import project", Enabled: true}, studioKeyControl{Index: 1, Text: "Gemini Project", Enabled: true}), 3*time.Second, "option", "Gemini Project")
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Gemini Project"}, true)), time.Second, "dialog", "Create key")
	if driver.flow.projectsPageAt.IsZero() || driver.flow.projectEntryAt.IsZero() || driver.flow.projectConfirmAt.IsZero() {
		t.Fatalf("应各做一次: %+v", driver.flow.steps)
	}
	// Projects 页只去一次
	again := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	again.flow.projectsPageAt = again.now
	again.flow.selectOpens = 1
	if step := again.step(testApikeyPage(testKeyDialog(testEmptySelect(), false), testNoProjectOptions[:2]...), time.Second); step.Kind == studioKeyStepNavigate {
		t.Fatalf("不应再去 Projects 页: %+v", step)
	}
}

// TestStudioKeyFlowProjectNameEmpty 新建项目对话框的项目名为空：不确认、不输入文字，等一会儿后以具体原因失败
func TestStudioKeyFlowProjectNameEmpty(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.flow.projectEntryAt = driver.now
	dialog := testProjectDialog(true, true, true)
	driver.expectWait(testApikeyPage(dialog), time.Second)
	step := driver.expectFail(testApikeyPage(dialog), 5*time.Second, StudioKeyReasonProjectName)
	if !driver.flow.projectConfirmAt.IsZero() {
		t.Fatalf("项目名为空时不应点确认: %+v", step)
	}
}

// TestStudioKeyFlowProjectTimeout 确认新建项目后一直没有新项目：到 projectWait 以具体原因失败
func TestStudioKeyFlowProjectTimeout(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.flow.projectEntryAt = driver.now
	driver.expectClick(testApikeyPage(testProjectDialog(false, true, true)), time.Second, "dialog", "Create project")
	busy := testProjectDialog(false, true, false)
	busy.Busy = true
	driver.expectWait(testApikeyPage(busy), 30*time.Second)
	driver.expectFail(testApikeyPage(busy), studioKeyTiming.projectWait, StudioKeyReasonProjectTimeout)
}

// TestStudioKeyFlowVerifyDialog 验证账号的对话框：只点一次它自己的关闭类按钮（不点验证），仍在时失败
func TestStudioKeyFlowVerifyDialog(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	verify := &studioKeyDialog{Title: "Verify your account", Text: "Verify your account to continue using the Gemini API Verify Not now",
		Buttons: []studioKeyControl{{Index: 0, Text: "Verify", Enabled: true}, {Index: 1, Text: "Not now", Enabled: true}}}
	if kind := classifyStudioKeyDialog(verify); kind != studioKeyDialogVerify {
		t.Fatalf("应认作验证账号对话框: %s", kind)
	}
	step := driver.expectClick(testApikeyPage(verify), time.Second, "dialog", "Not now")
	if step.Target.Index != 1 {
		t.Fatalf("不应点验证按钮: %+v", step)
	}
	driver.expectWait(testApikeyPage(verify), time.Second)
	driver.expectFail(testApikeyPage(verify), studioKeyTiming.noDialog, StudioKeyReasonVerifyModal)
}

// TestStudioKeyFlowAlerts 点创建之前就有的验证账号横幅不算出错；之后新出现的出错提示立即失败（原因带打码后的提示原文）
func TestStudioKeyFlowAlerts(t *testing.T) {
	banner := "Your account must be verified before it can continue using the Gemini API. Verify"
	before := testApikeyPage(nil)
	before.Alerts = []string{banner}
	driver := newStudioKeyFlowDriver(t, before)
	page := testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Default Gemini Project"}, true))
	page.Alerts = []string{banner}
	driver.expectClick(page, time.Second, "dialog", "Create key")
	page.Alerts = []string{banner, "Failed to generate API key: permission denied " + fakeNewKey}
	step := driver.expectFail(page, time.Second, StudioKeyReasonKeyFailed)
	if strings.Contains(step.Reason, fakeNewKey) || !strings.Contains(step.Reason, "permission denied") {
		t.Fatalf("原因应带打码后的提示: %s", step.Reason)
	}
	// 页面没打开创建对话框、却新出现了验证账号的提示：按需要先验证账号失败
	refused := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	page = testApikeyPage(nil)
	page.Alerts = []string{"You must verify your account to create API keys"}
	refused.expectFail(page, time.Second, StudioKeyReasonVerifyBlocked)
}

// TestStudioKeyFlowDetail 失败原因后面接的步骤与对话框摘要：打码（Key、邮箱）、不用页面快照的分隔（免得被 StripPageSnapshots 去掉）
func TestStudioKeyFlowDetail(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	dialog := testKeyDialog(testEmptySelect(), false)
	dialog.Error = "owner lisa@example.com " + fakeNewKey
	driver.step(testApikeyPage(dialog, testNoProjectOptions...), time.Second)
	detail := driver.flow.detail()
	for _, forbidden := range []string{fakeNewKey, "TESTKEY", "lisa@", PageSnapshotMarker} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("摘要不应包含 %q: %s", forbidden, detail)
		}
	}
	for _, want := range []string{" | 步骤=打开创建对话框", " | 对话框=", "标题=Create API key", "下拉框=未选(Choose an imported project)", "选项=Import project/No Cloud Projects Available(不可用)"} {
		if !strings.Contains(detail, want) {
			t.Fatalf("摘要缺少 %q: %s", want, detail)
		}
	}
	keyErr := &StudioKeyError{Reason: StudioKeyReasonSelectStuck, Detail: detail}
	if StripPageSnapshots(keyErr.Error()) != keyErr.Error() {
		t.Fatalf("摘要不应被 StripPageSnapshots 去掉: %s", keyErr.Error())
	}
}

// TestStudioKeyFlowResult 返回值：Key 的记录里没有项目、这次新建了一个项目时用新项目补上；只在创建过时带步骤
func TestStudioKeyFlowResult(t *testing.T) {
	flow := newStudioKeyFlow()
	flow.opened(nil, time.Now())
	flow.projectEntryAt = time.Now()
	flow.projectConfirmAt = time.Now()
	flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012", Display: "Gemini Project"}})
	result := flow.result(StudioKeyCandidate{Key: fakeNewKey}, 1)
	if !result.Created || !result.ProjectCreated || result.ProjectName != "projects/123456789012" || result.ProjectNumber != "123456789012" ||
		result.ProjectDisplay != "Gemini Project" || len(result.Steps) == 0 {
		t.Fatalf("result = %+v", redactResult(result))
	}
	// 开始新建项目之前就看到的项目不算新项目
	known := newStudioKeyFlow()
	known.noteProjects([]StudioProject{{Name: "projects/210987654321", Number: "210987654321"}})
	known.projectEntryAt = time.Now()
	known.noteProjects([]StudioProject{{Name: "projects/210987654321", Number: "210987654321"}})
	if len(known.newProjects) != 0 {
		t.Fatalf("已有项目不应算新项目: %+v", known.newProjects)
	}
	read := newStudioKeyFlow().result(StudioKeyCandidate{Key: fakeNewKey, ProjectName: "projects/1", ProjectNumber: "1"}, 1)
	if read.Created || read.ProjectCreated || len(read.Steps) != 0 {
		t.Fatalf("只读取时不应带创建标记与步骤: %+v", redactResult(read))
	}
}

func redactResult(result StudioKeyResult) StudioKeyResult {
	result.Key = RedactStudioKeys(result.Key)
	return result
}

// TestStudioKeyFlowClosesListBeforeConfirm 已经选着项目、下拉列表却开着（遮罩挡住对话框）：先点下拉框所在位置把列表收起来，
// 不在列表开着时点确认（那样只会点在遮罩上，却被记成已创建）
func TestStudioKeyFlowClosesListBeforeConfirm(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	selected := &studioKeySelect{Value: "Default Gemini Project", Expanded: true}
	other := []studioKeyControl{{Index: 0, Text: "Learn more", Enabled: true, Button: true}}
	step := driver.expectClick(testApikeyPage(testKeyDialog(selected, true), other...), 3*time.Second, "select", "")
	if step.Mark != studioKeyMarkSelect || !driver.flow.keyConfirmAt.IsZero() {
		t.Fatalf("应先收起列表、不点确认: %+v", step)
	}
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Default Gemini Project"}, true)), time.Second, "dialog", "Create key")
}

// TestStudioKeyFlowKnownProjectNoCreate 页面自己的响应里出现过项目（账户已有项目）、下拉框里却只有导入与“Create project”：不新建，
// 列表停住后收起再展开等它出现，仍没有就以“已有项目但没列出”失败；没有下拉框、只有新建入口的对话框与 Projects 页同样不新建
func TestStudioKeyFlowKnownProjectNoCreate(t *testing.T) {
	known := []StudioProject{{Name: "projects/210987654321", Number: "210987654321", Display: "Default Gemini Project"}}
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.flow.noteProjects(known)
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	expanded := testEmptySelect()
	expanded.Expanded = true
	open := testApikeyPage(testKeyDialog(expanded, false), testNoProjectOptions...)
	closed := testApikeyPage(testKeyDialog(testEmptySelect(), false))
	var failed studioKeyStep
	collapses := 0
	for round := 0; round < 200 && failed.Kind != studioKeyStepFail; round++ {
		page := open
		if !driver.flow.selectAt.IsZero() && driver.flow.selectOpens%2 == 0 {
			page = closed
		}
		step := driver.step(page, 500*time.Millisecond)
		if step.Kind == studioKeyStepClick && step.Mark != studioKeyMarkSelect {
			t.Fatalf("已有项目时不应点下拉框以外的东西: %+v（步骤 %v）", step, driver.flow.steps)
		}
		if step.Kind == studioKeyStepNavigate {
			t.Fatalf("已有项目时不应去 Projects 页: %+v", step)
		}
		if step.Kind == studioKeyStepClick && step.Note == "收起项目下拉框等已有项目" {
			collapses++
		}
		failed = step
	}
	if failed.Kind != studioKeyStepFail || failed.Reason != StudioKeyReasonProjectUnlisted || collapses == 0 || driver.flow.projectStarted() {
		t.Fatalf("应收起再展开核对、最后以已有项目但没列出失败: %+v（收起 %d 次，步骤 %v）", failed, collapses, driver.flow.steps)
	}
	// 列表在等待期间列出了已有项目：选它
	later := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	later.flow.noteProjects(known)
	later.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	later.expectWait(open, time.Second)
	listed := append([]studioKeyControl{{Index: 0, Text: "Default Gemini Project", Enabled: true}}, testNoProjectOptions...)
	later.expectClick(testApikeyPage(testKeyDialog(expanded, false), listed...), time.Second, "option", "Default Gemini Project")
	// 没有下拉框、只有新建项目入口的对话框
	plain := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	plain.flow.noteProjects(known)
	entryOnly := &studioKeyDialog{Title: "Create API key", Buttons: []studioKeyControl{{Index: 0, Text: "Cancel", Enabled: true},
		{Index: 1, Text: "Create project", Enabled: true, Icon: "add"}}}
	plain.expectWait(testApikeyPage(entryOnly), studioKeyTiming.listSettle)
	plain.expectFail(testApikeyPage(entryOnly), studioKeyTiming.stuck, StudioKeyReasonProjectUnlisted)
	// Projects 页上列出了已有项目（响应里出现）：不点新建
	page := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	page.flow.projectsPageAt = page.now
	page.flow.noteProjects(known)
	page.expectFail(studioKeyPage{Host: "aistudio.google.com", Path: "/projects", Project: "Create a new project"}, studioKeyTiming.settle,
		StudioKeyReasonProjectUnlisted)
	// 不是这次点出来的新建项目对话框：账户已有项目时不确认
	unasked := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	unasked.flow.noteProjects(known)
	unasked.expectWait(testApikeyPage(testProjectDialog(false, true, true)), time.Second)
	unasked.expectFail(testApikeyPage(testProjectDialog(false, true, true)), studioKeyTiming.stuck, StudioKeyReasonUnknownDialog)
	if unasked.flow.projectStarted() {
		t.Fatal("账户已有项目时不应确认新建项目")
	}
}

// TestStudioKeyFlowWaitsForProjectList 项目列表异步加载：先只有导入与“Create project”，不到 listSettle 就列出了已有项目时选它、
// 不新建；列表一直在转圈时不新建，等到 stuck 以“列表没加载完”失败；只有占位类选项时不据此断定没有项目
func TestStudioKeyFlowWaitsForProjectList(t *testing.T) {
	expanded := testEmptySelect()
	expanded.Expanded = true
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	staticOnly := testApikeyPage(testKeyDialog(expanded, false), testNoProjectOptions[0], testNoProjectOptions[2])
	driver.expectWait(staticOnly, 500*time.Millisecond)
	driver.expectWait(staticOnly, studioKeyTiming.listSettle/2)
	loaded := testApikeyPage(testKeyDialog(expanded, false), studioKeyControl{Index: 0, Text: "Gemini Project", Enabled: true},
		testNoProjectOptions[0], testNoProjectOptions[2])
	step := driver.expectClick(loaded, studioKeyTiming.listSettle/2, "option", "Gemini Project")
	if step.Mark != studioKeyMarkPick || driver.flow.projectStarted() {
		t.Fatalf("列表加载出已有项目后应选它、不新建: %+v", step)
	}
	// 列表一直在转圈
	busy := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	busy.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	spinning := testApikeyPage(testKeyDialog(expanded, false), testNoProjectOptions...)
	spinning.ListBusy = true
	busy.expectWait(spinning, time.Second)
	busy.expectWait(spinning, 2*studioKeyTiming.listSettle)
	busy.expectFail(spinning, studioKeyTiming.stuck, StudioKeyReasonListLoading)
	if busy.flow.projectStarted() {
		t.Fatal("列表在转圈时不应新建项目")
	}
}

// TestStudioKeyFlowNativeSelect 原生 select 的选项看得到、点不了：有已有项目时不新建；只有占位与“没有可用项目”时才用对话框里的新建入口
func TestStudioKeyFlowNativeSelect(t *testing.T) {
	entryDialog := func(options ...studioKeyControl) *studioKeyDialog {
		dialog := testKeyDialog(&studioKeySelect{Empty: true, Placeholder: "Choose an imported project", Options: options}, false)
		dialog.Buttons = append(dialog.Buttons, studioKeyControl{Index: 2, Text: "Create project", Enabled: true, Icon: "add"})
		return dialog
	}
	withProject := entryDialog(studioKeyControl{Index: 0, Text: "Choose an imported project", Enabled: false},
		studioKeyControl{Index: 1, Text: "Default Gemini Project", Enabled: true})
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(withProject), time.Second, "select", "")
	for round := 0; round < 100; round++ {
		step := driver.step(testApikeyPage(withProject), time.Second)
		if step.Kind == studioKeyStepClick && step.Mark == studioKeyMarkProjectEntry {
			t.Fatalf("原生 select 里有已有项目时不应新建: %+v", step)
		}
		if step.Kind == studioKeyStepFail {
			break
		}
	}
	if !driver.flow.existingOffered {
		t.Fatal("原生 select 里的已有项目应记为出现过")
	}
	none := entryDialog(studioKeyControl{Index: 0, Text: "No Cloud Projects Available", Enabled: false})
	empty := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	empty.expectClick(testApikeyPage(none), time.Second, "select", "")
	var created studioKeyStep
	for round := 0; round < 100 && created.Kind != studioKeyStepClick; round++ {
		created = empty.step(testApikeyPage(none), time.Second)
		if created.Kind == studioKeyStepClick && created.Mark == studioKeyMarkSelect {
			created = studioKeyStep{}
		}
	}
	if created.Mark != studioKeyMarkProjectEntry || created.Target.Text != "Create project" {
		t.Fatalf("原生 select 只有“没有可用项目”时应用对话框里的新建入口: %+v（步骤 %v）", created, empty.flow.steps)
	}
	if selectShowsNoProject(testKeyDialog(testEmptySelect(), false)) {
		t.Fatal("看不到选项、也没有提示时不能断定没有项目")
	}
}

// TestStudioKeyFlowReopensAfterProjectConfirm 下拉框路径里确认新建项目后，页面关掉了创建 Key 的对话框、响应里又读不到新项目：
// 过 noDialog 重新打开创建对话框，在下拉框里看到新项目后选它创建 Key（不白等到 projectWait）；一直看不到时以超时失败，原因说明项目可能已建好
func TestStudioKeyFlowReopensAfterProjectConfirm(t *testing.T) {
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.flow.projectEntryAt = driver.now
	driver.expectClick(testApikeyPage(testProjectDialog(false, true, true)), time.Second, "dialog", "Create project")
	driver.expectWait(testApikeyPage(nil), time.Second)
	driver.expectWait(testApikeyPage(nil), studioKeyTiming.noDialog/2)
	step := driver.expectClick(testApikeyPage(nil), studioKeyTiming.noDialog, "page-create", "")
	if step.Mark != studioKeyMarkOpen || driver.flow.opens != 2 {
		t.Fatalf("应重新打开创建对话框: %+v", step)
	}
	driver.expectWait(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second)
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), studioKeyTiming.retry*3, "select", "")
	expanded := testEmptySelect()
	expanded.Expanded = true
	options := append([]studioKeyControl{{Index: 0, Text: "Gemini Project", Enabled: true}}, testNoProjectOptions...)
	driver.expectClick(testApikeyPage(testKeyDialog(expanded, false), options...), time.Second, "option", "Gemini Project")
	if driver.flow.projectReadyAt.IsZero() || len(driver.flow.newProjects) != 0 {
		t.Fatalf("应从下拉框判断新项目就绪: %+v", driver.flow.steps)
	}
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "Gemini Project"}, true)), time.Second, "dialog", "Create key")
	// 一直看不到新项目：到 projectWait 以超时失败
	never := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	never.flow.projectEntryAt = never.now
	never.expectClick(testApikeyPage(testProjectDialog(false, true, true)), time.Second, "dialog", "Create project")
	var last studioKeyStep
	reopened := 0
	for round := 0; round < 400 && last.Kind != studioKeyStepFail; round++ {
		last = never.step(testApikeyPage(nil), time.Second)
		if last.Kind == studioKeyStepClick && last.Mark == studioKeyMarkOpen {
			reopened++
		}
	}
	if last.Reason != StudioKeyReasonProjectTimeout || reopened == 0 || never.flow.opens > 3 {
		t.Fatalf("应重新打开过创建对话框（最多三次）、最后以超时失败: %+v（重开 %d 次）", last, reopened)
	}
}

// TestStudioKeyFlowEmptyState API Keys 页上没有可用的“Create API key”、只有新建项目的按钮：页面响应里没出现过项目时去一次 Projects 页；
// 出现过项目、已经去过 Projects 页、有对话框挡着或没有新建按钮时不去
func TestStudioKeyFlowEmptyState(t *testing.T) {
	empty := studioKeyPage{Host: "aistudio.google.com", Path: StudioKeyPath, Project: "Create project", Alerts: []string{"banner"}}
	flow := newStudioKeyFlow()
	step, ok := flow.emptyStateStep(empty)
	if !ok || step.Kind != studioKeyStepNavigate || step.Path != studioKeyProjectsPath || step.Mark != studioKeyMarkProjectsPage || !flow.baseAlerts["banner"] {
		t.Fatalf("应去 Projects 页并记下已有提示: %+v", step)
	}
	flow.apply(step, time.Now())
	if !flow.running() {
		t.Fatal("去了 Projects 页后创建流程应算已开始")
	}
	if _, ok := flow.emptyStateStep(empty); ok {
		t.Fatal("Projects 页只去一次")
	}
	known := newStudioKeyFlow()
	known.noteProjects([]StudioProject{{Name: "projects/210987654321", Number: "210987654321"}})
	if _, ok := known.emptyStateStep(empty); ok {
		t.Fatal("页面响应里有项目时不应去新建")
	}
	if _, ok := newStudioKeyFlow().emptyStateStep(testApikeyPage(nil)); ok {
		t.Fatal("没有新建项目的按钮时不应去")
	}
}

// countClicks 用同一个页面驱动 rounds 轮（每轮 advance），返回点击 scope 的次数与最后一步（失败时停下）
func (driver *studioKeyFlowDriver) countClicks(page studioKeyPage, advance time.Duration, rounds int, scope string) (int, studioKeyStep) {
	clicks := 0
	var last studioKeyStep
	for round := 0; round < rounds && last.Kind != studioKeyStepFail; round++ {
		last = driver.step(page, advance)
		if last.Kind == studioKeyStepClick && last.Target.Scope == scope {
			clicks++
		}
	}
	return clicks, last
}

// TestStudioKeyFlowReopenNeedsDialog 重新打开创建对话框之后页面一直没有对话框（页面可能直接创建了 Key）：不再点“Create API key”，
// 按第一次点击的规则只等 createWait 再失败；从 Projects 页回来时先等 settle 再点，同样只点一次
func TestStudioKeyFlowReopenNeedsDialog(t *testing.T) {
	// 下拉框路径：新项目已出现，页面关掉了创建对话框 → 重新打开一次，之后一直没有对话框
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.flow.projectEntryAt = driver.now
	driver.expectClick(testApikeyPage(testProjectDialog(false, true, true)), time.Second, "dialog", "Create project")
	driver.flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012", Display: "Gemini Project"}})
	clicks, last := driver.countClicks(testApikeyPage(nil), 500*time.Millisecond, 400, "page-create")
	if clicks != 1 || last.Kind != studioKeyStepFail || last.Reason != StudioKeyReasonDialogClosed || driver.flow.opens != 2 {
		t.Fatalf("应只重新打开一次、等 createWait 后失败: 点了 %d 次，最后 %+v（步骤 %v）", clicks, last, driver.flow.steps)
	}
	if waited := driver.now.Sub(driver.flow.openedAt); waited <= studioKeyTiming.createWait {
		t.Fatalf("重新打开后应等满 createWait 才失败: %s", waited)
	}
	// 从 Projects 页回来（下拉框路径去过 Projects 页）：settle 之前不点，之后只点一次
	returned := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	returned.flow.dialogSinceOpen = true
	returned.flow.dialogSeen = true
	returned.flow.projectsPageAt = returned.now
	returned.flow.projectEntryAt = returned.now
	returned.flow.projectConfirmAt = returned.now
	returned.flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012"}})
	returned.flow.apply(studioKeyStep{Kind: studioKeyStepNavigate, Path: StudioKeyPath, Mark: studioKeyMarkReturn}, returned.now)
	start := returned.now
	clicks, last = returned.countClicks(testApikeyPage(nil), 500*time.Millisecond, 400, "page-create")
	if clicks != 1 || last.Kind != studioKeyStepFail || last.Reason != StudioKeyReasonDialogClosed {
		t.Fatalf("从 Projects 页回来后应只点一次: 点了 %d 次，最后 %+v（步骤 %v）", clicks, last, returned.flow.steps)
	}
	if opened := returned.flow.openedAt.Sub(start); opened < studioKeyTiming.settle {
		t.Fatalf("回来后应先等 settle 再点: %s", opened)
	}
	// 空状态路径（没点过“Create API key”就去了 Projects 页）：回来后第一次点，之后没有对话框按“已点击创建但没读到新 Key”失败
	empty := &studioKeyFlowDriver{t: t, flow: newStudioKeyFlow(), now: time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)}
	step, ok := empty.flow.emptyStateStep(studioKeyPage{Host: "aistudio.google.com", Path: StudioKeyPath, Project: "Create project"})
	if !ok {
		t.Fatal("应去 Projects 页")
	}
	empty.flow.apply(step, empty.now)
	empty.expectClick(studioKeyPage{Host: "aistudio.google.com", Path: "/projects", Project: "Create project"}, 3*studioKeyTiming.listSettle, "page-project", "Create project")
	empty.expectClick(studioKeyPage{Host: "aistudio.google.com", Path: "/projects", Dialog: testProjectDialog(false, true, true)}, time.Second, "dialog", "Create project")
	empty.flow.noteProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012"}})
	if step := empty.step(studioKeyPage{Host: "aistudio.google.com", Path: "/projects"}, 3*time.Second); step.Kind != studioKeyStepNavigate || step.Mark != studioKeyMarkReturn {
		t.Fatalf("新项目出现后应回到 API Keys 页: %+v", step)
	}
	clicks, last = empty.countClicks(testApikeyPage(nil), 500*time.Millisecond, 400, "page-create")
	if clicks != 1 || last.Reason != StudioKeyReasonCreateFailed {
		t.Fatalf("空状态回来后应只点一次: 点了 %d 次，最后 %+v（步骤 %v）", clicks, last, empty.flow.steps)
	}
}

// TestStudioKeyFlowExternalLinks 站外链接（如“在 Google Cloud 控制台新建项目”）不当作新建项目的入口、确认或关闭类按钮，下拉选项里的站外链接也不选
func TestStudioKeyFlowExternalLinks(t *testing.T) {
	cloud := studioKeyControl{Index: 2, Text: "Create a project in Google Cloud console", Enabled: true, External: true}
	if _, ok := projectEntryControl(&studioKeyDialog{Buttons: []studioKeyControl{cloud}}); ok {
		t.Fatal("站外链接不应认作新建项目的入口")
	}
	if got := classifyStudioKeyOption(studioKeyControl{Text: "Create project", Enabled: true, Icon: "add", External: true}); got != studioKeyOptionOther {
		t.Fatalf("站外链接的选项不应点: %s", got)
	}
	// 下拉框里只有导入与“没有项目”，对话框里只有站外的新建链接：去 Projects 页，不点链接
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	dialog := testKeyDialog(testEmptySelect(), false)
	dialog.Buttons = append(dialog.Buttons, cloud)
	driver.expectClick(testApikeyPage(dialog), time.Second, "select", "")
	driver.expectWait(testApikeyPage(dialog, testNoProjectOptions[:2]...), time.Second)
	step := driver.step(testApikeyPage(dialog, testNoProjectOptions[:2]...), studioKeyTiming.listSettle)
	if step.Kind != studioKeyStepNavigate || step.Path != studioKeyProjectsPath || !driver.flow.projectEntryAt.IsZero() {
		t.Fatalf("应去 Projects 页、不点站外链接: %+v", step)
	}
	// 新建项目对话框里唯一的“Create”是站外链接：不确认
	project := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	project.flow.projectEntryAt = project.now
	external := testProjectDialog(false, true, true)
	external.Buttons = []studioKeyControl{{Index: 0, Text: "Cancel", Enabled: true}, {Index: 1, Text: "Create", Enabled: true, External: true}}
	for round := 0; round < 100; round++ {
		if step := project.step(testApikeyPage(external), time.Second); step.Kind == studioKeyStepClick {
			t.Fatalf("不应点站外链接: %+v", step)
		} else if step.Kind == studioKeyStepFail {
			break
		}
	}
	// 验证账号弹窗里只有站外的关闭类链接：不点，到时失败
	verify := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	modal := &studioKeyDialog{Title: "Verify your account", Text: "Verify your account to continue Verify Not now",
		Buttons: []studioKeyControl{{Index: 0, Text: "Verify", Enabled: true}, {Index: 1, Text: "Not now", Enabled: true, External: true}}}
	verify.expectWait(testApikeyPage(modal), time.Second)
	verify.expectFail(testApikeyPage(modal), studioKeyTiming.noDialog, StudioKeyReasonVerifyModal)
}

// TestStudioKeyFlowProjectNamedNew 已有项目的名字里含“New Project”：按已有项目选它，不当作新建项目的入口；列表里另有带加号标记的
// “Create project”时，名字整段就是“New Project”的也按已有项目算，选上后认得出下拉框里选着它
func TestStudioKeyFlowProjectNamedNew(t *testing.T) {
	for _, text := range []string{"My New Project", "New Project Alpha", "我的新項目", "Add project data"} {
		if isCreateProjectOption(text, "") {
			t.Errorf("%q 不应认作新建项目的选项", text)
		}
		if !selectHasProject(&studioKeySelect{Value: text}) {
			t.Errorf("下拉框值 %q 应算选着项目", text)
		}
	}
	for _, text := range []string{"Create project", "+ Create project", "Create project Create a new project", "新しいプロジェクトを作成", "새 프로젝트 만들기", "新建项目"} {
		if !isCreateProjectOption(text, "") {
			t.Errorf("%q 应认作新建项目的选项", text)
		}
	}
	if !isCreateProjectOption("Project", "add") || isCreateProjectOption("Import project", "add") {
		t.Error("加号图标加“项目”字样应认作新建项目，导入除外")
	}
	expanded := testEmptySelect()
	expanded.Expanded = true
	driver := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	driver.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	options := []studioKeyControl{{Index: 0, Text: "Import project", Enabled: true, Icon: "download"}, {Index: 1, Text: "My New Project", Enabled: true},
		{Index: 2, Text: "Create project", Enabled: true}}
	driver.expectClick(testApikeyPage(testKeyDialog(expanded, false), options...), time.Second, "option", "My New Project")
	driver.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "My New Project"}, true)), time.Second, "dialog", "Create key")
	if driver.flow.projectStarted() {
		t.Fatal("不应新建项目")
	}
	// 名字整段就是“New Project”，列表里另有带加号图标的“Create project”
	exact := newStudioKeyFlowDriver(t, testApikeyPage(nil))
	exact.expectClick(testApikeyPage(testKeyDialog(testEmptySelect(), false)), time.Second, "select", "")
	options = []studioKeyControl{{Index: 0, Text: "Import project", Enabled: true, Icon: "download"}, {Index: 1, Text: "New Project gen-lang-client-0123456789", Enabled: true},
		{Index: 2, Text: "New Project", Enabled: true}, {Index: 3, Text: "Create project", Enabled: true, Icon: "add"}}
	step := exact.expectClick(testApikeyPage(testKeyDialog(expanded, false), options...), time.Second, "option", "")
	if step.Mark != studioKeyMarkPick || step.Target.Index == 3 {
		t.Fatalf("应选已有项目、不点新建: %+v", step)
	}
	exact.expectClick(testApikeyPage(testKeyDialog(&studioKeySelect{Value: "New Project"}, true)), time.Second, "dialog", "Create key")
	if exact.flow.projectStarted() || !exact.flow.existingOffered {
		t.Fatal("应记为有已有项目、不新建")
	}
	// 没有带标记的新建选项时，整段文字像新建项目的选项才算新建（无法区分时宁可不选它当项目）
	if kinds := classifyStudioKeyOptions([]studioKeyControl{{Text: "New Project", Enabled: true}, {Text: "Import project", Enabled: true}}); kinds[0] != studioKeyOptionCreate {
		t.Fatalf("没有带标记的新建选项时“New Project”应算新建: %v", kinds)
	}
}

// TestStudioKeyTimeout 整次获取的时限不短于 StudioKeyMinTimeout（REQUEST_TIMEOUT 调小时，没有项目的账户也来得及建项目再建 Key）；
// 最长的路径（空状态去 Projects 页建项目、回来等 Key 列表、创建 Key）各段时限加起来不超过它
func TestStudioKeyTimeout(t *testing.T) {
	if studioKeyTimeout(time.Minute) != StudioKeyMinTimeout || studioKeyTimeout(10*time.Minute) != 10*time.Minute {
		t.Fatalf("时限 = %s / %s", studioKeyTimeout(time.Minute), studioKeyTimeout(10*time.Minute))
	}
	timing := studioKeyTiming
	worst := 2*timing.settle + 3*timing.listSettle + timing.projectWait + timing.settle + timing.stuck + timing.createWait
	if worst >= StudioKeyMinTimeout {
		t.Fatalf("最长路径 %s 不应超过下限 %s", worst, StudioKeyMinTimeout)
	}
}
