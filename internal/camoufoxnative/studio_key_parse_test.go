package camoufoxnative

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// 测试用的假 Key（不是真实 Key）：fakeClientKey 扮演页面自己的公共 Key
var (
	fakeNewKey     = "AQ.TESTKEY" + strings.Repeat("x", 24)
	fakeOtherKey   = "AQ.TESTKEY" + strings.Repeat("y", 20) + "TST1"
	fakeClassicKey = "AIza" + "TESTKEY" + strings.Repeat("0", 28)
	fakeClientKey  = "AIza" + "TESTCLIENT" + strings.Repeat("c", 25)
)

// TestExtractStudioKeysNestedArrays 带 XSSI 前缀的 protobuf-JSON 嵌套数组：Key 与同一条记录里的 projects/<数字> 配对，
// 显示名取同一记录里的普通文字；另一条记录的项目不会串到这条 Key 上
func TestExtractStudioKeysNestedArrays(t *testing.T) {
	body := `)]}'
[[["projects/111/locations/global/keys/abc",null,"` + fakeNewKey + `",["projects/111","Default Gemini Project"],[1730000000,0]],
["projects/222/locations/global/keys/def",null,"` + fakeClassicKey + `",["projects/222","Other project"]]],null]`
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 2 {
		t.Fatalf("keys = %d，期望 2", len(keys))
	}
	if keys[0].Key != fakeNewKey || keys[0].ProjectName != "projects/111" || keys[0].ProjectNumber != "111" {
		t.Fatalf("第一条 = %+v", redactCandidate(keys[0]))
	}
	if keys[0].ProjectDisplay != "Default Gemini Project" {
		t.Fatalf("显示名 = %q", keys[0].ProjectDisplay)
	}
	if keys[1].Key != fakeClassicKey || keys[1].ProjectNumber != "222" {
		t.Fatalf("第二条 = %+v", redactCandidate(keys[1]))
	}
}

// TestExtractStudioKeysBatchExecute batchexecute 分块格式：每块前有长度行，Key 在再包一层的 JSON 字符串里
func TestExtractStudioKeysBatchExecute(t *testing.T) {
	inner, _ := json.Marshal([]any{[]any{[]any{"projects/333", fakeNewKey, "My Project"}}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "ListKeys", string(inner), nil, nil, "generic"}})
	body := ")]}'\n\n" + "123\n" + string(outer) + "\n25\n[[\"e\",4,null,null,1]]\n"
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 1 || keys[0].Key != fakeNewKey || keys[0].ProjectName != "projects/333" {
		t.Fatalf("keys = %+v", redactCandidates(keys))
	}
}

// TestExtractStudioKeysNoTextScan 不再按文本扫描：不是 JSON 的正文（HTML、脚本、纯文字）、JSON 里嵌在别的文字（地址）里的 Key 都不是候选；
// 只有整段是 Key 的 JSON 字符串才是
func TestExtractStudioKeysNoTextScan(t *testing.T) {
	for _, body := range []string{
		`garbage projects/9 ........ key=` + fakeClassicKey + ` projects/444 tail`,
		`<html><script>window.WIZ_global_data={"WIu0Nc":"` + fakeClientKey + `"}</script></html>`,
		`var k="` + fakeClientKey + `";function g(){return "AQ.getInstanceForTestingOnlyFooBarBaz"}`,
		`{"url":"https://generativelanguage.googleapis.com/v1beta/models?key=` + fakeClientKey + `"}`,
		"x" + fakeClassicKey + "TOOLONG",
	} {
		if keys := ExtractStudioKeys([]byte(body)); len(keys) != 0 {
			t.Fatalf("%s: keys = %+v", RedactStudioKeys(body), redactCandidates(keys))
		}
	}
	if keys := ExtractStudioKeys([]byte(`{"key":" ` + fakeClassicKey + ` "}`)); len(keys) != 1 || keys[0].Key != fakeClassicKey {
		t.Fatalf("整段是 Key 的字符串应是候选: %+v", redactCandidates(keys))
	}
}

// TestStudioKeyBodyKind 正文的种类：文档、脚本、样式（按 MIME、导航、请求目标）进拒绝集合；JSON、json+protobuf、batchexecute 的 text/plain
// 找候选；MIME 为空时看开头
func TestStudioKeyBodyKind(t *testing.T) {
	cases := []struct {
		response studioKeyResponse
		body     string
		want     string
	}{
		{studioKeyResponse{mime: "text/html"}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "application/json", document: true}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "", destination: "script"}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "text/javascript"}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "application/x-javascript"}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "text/css"}, `[]`, studioKeyBodyPage},
		{studioKeyResponse{mime: "application/json+protobuf"}, `[]`, studioKeyBodyJSON},
		{studioKeyResponse{mime: "application/json"}, `{}`, studioKeyBodyJSON},
		{studioKeyResponse{mime: "text/plain"}, ")]}'\n12\n[]", studioKeyBodyJSON},
		{studioKeyResponse{}, "<!doctype html>", studioKeyBodyPage},
		{studioKeyResponse{}, ")]}'\n[]", studioKeyBodyJSON},
		{studioKeyResponse{}, "123\n[[1]]", studioKeyBodyJSON},
		{studioKeyResponse{}, "var a = 1", studioKeyBodyOther},
		{studioKeyResponse{mime: "image/png"}, "", studioKeyBodyOther},
	}
	for _, item := range cases {
		if got := studioKeyBodyKind(item.response, []byte(item.body)); got != item.want {
			t.Fatalf("%+v %q = %s，期望 %s", item.response, item.body, got, item.want)
		}
	}
}

// TestExtractStudioKeysNone 没有 Key 的响应返回空
func TestExtractStudioKeysNone(t *testing.T) {
	if keys := ExtractStudioKeys([]byte(`)]}'` + "\n" + `[[["projects/1","name"]]]`)); len(keys) != 0 {
		t.Fatalf("keys = %d", len(keys))
	}
}

// TestRedactStudioKeys 错误与日志文本里的 Key 只留末四位
func TestRedactStudioKeys(t *testing.T) {
	text := RedactStudioKeys("失败: " + fakeNewKey + " / " + fakeClassicKey)
	if strings.Contains(text, fakeNewKey) || strings.Contains(text, fakeClassicKey) || strings.Contains(text, "TESTKEY") {
		t.Fatalf("没有打码: %s", text)
	}
	if !strings.Contains(text, "<key:…xxxx>") {
		t.Fatalf("缺少末四位: %s", text)
	}
	keyErr := &StudioKeyError{Reason: "r", Cause: errorString("页面 " + fakeNewKey)}
	if strings.Contains(keyErr.Error(), fakeNewKey) {
		t.Fatalf("StudioKeyError 泄露 Key: %s", keyErr.Error())
	}
}

// TestChooseStudioKeyRow 候选只有与 Key 列表里的行对上才算：末四位相同、不在拒绝集合里；行上显示完整 Key 时用它（不在拒绝集合里时）；
// 已保存的那条所在的行优先；只读时只认已保存的那条（完整相等），列表里没有它那一行时 hasRow 为假
func TestChooseStudioKeyRow(t *testing.T) {
	denied := func(key string) bool { return key == fakeClientKey }
	none := func(string) bool { return false }
	rows := []studioKeyListRow{{Last4: StudioKeyLast4(fakeNewKey)}, {Last4: StudioKeyLast4(fakeOtherKey)}}
	candidates := []StudioKeyCandidate{{Key: fakeClientKey}, {Key: fakeOtherKey, ProjectNumber: "210987654321"}, {Key: fakeNewKey}}
	// 公共 Key 排在第一、却对不上任何一行：选第一条能确认的行
	if choice := chooseStudioKeyRow(rows, candidates, denied, "", false); !choice.confirmed || choice.candidate.Key != fakeNewKey {
		t.Fatalf("应确认第一行: %+v", redactChoice(choice))
	}
	// 已保存的那条所在的行优先
	if choice := chooseStudioKeyRow(rows, candidates, denied, fakeOtherKey, false); !choice.confirmed || choice.candidate.Key != fakeOtherKey ||
		choice.candidate.ProjectNumber != "210987654321" {
		t.Fatalf("应选已保存的那条: %+v", redactChoice(choice))
	}
	// 只有公共 Key、列表里的行对不上：不确认，目标是第一行（之后打开详情）
	if choice := chooseStudioKeyRow(rows, candidates[:1], denied, "", false); choice.confirmed || !choice.hasRow || choice.row.Last4 != rows[0].Last4 {
		t.Fatalf("不应确认: %+v", redactChoice(choice))
	}
	// 公共 Key 的末四位恰好是某一行的：在拒绝集合里，照样不认
	clientRow := []studioKeyListRow{{Last4: StudioKeyLast4(fakeClientKey)}}
	if choice := chooseStudioKeyRow(clientRow, candidates[:1], denied, "", false); choice.confirmed {
		t.Fatalf("拒绝集合里的 Key 不应确认: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(clientRow, candidates[:1], none, "", false); !choice.confirmed {
		t.Fatalf("不在拒绝集合里时末四位对上即确认: %+v", redactChoice(choice))
	}
	// 行上显示完整 Key：单独不算（可能是截断的前缀），要有响应里完整相等的那条，或就是已保存的那条；没有时目标是这一行（之后打开详情）；
	// 响应里末四位相同的另一条不认；显示的是拒绝集合里的 Key 时不用
	full := []studioKeyListRow{{Last4: StudioKeyLast4(fakeClassicKey), Full: fakeClassicKey}}
	if choice := chooseStudioKeyRow(full, nil, denied, "", false); choice.confirmed || !choice.hasRow || choice.row.Full != fakeClassicKey {
		t.Fatalf("行上显示的完整 Key 没有印证时不应确认: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(full, []StudioKeyCandidate{{Key: fakeClassicKey, ProjectNumber: "1"}}, denied, "", false); !choice.confirmed ||
		choice.candidate.Key != fakeClassicKey || choice.candidate.ProjectNumber != "1" {
		t.Fatalf("响应里有同一条时应确认: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(full, nil, denied, fakeClassicKey, false); !choice.confirmed || choice.candidate.Key != fakeClassicKey {
		t.Fatalf("行上显示的就是已保存的那条时应确认: %+v", redactChoice(choice))
	}
	twinClassic := "AIza" + "TESTKEY" + strings.Repeat("1", 24) + "0000"
	if choice := chooseStudioKeyRow(full, []StudioKeyCandidate{{Key: twinClassic}}, denied, "", false); choice.confirmed {
		t.Fatalf("响应里末四位相同、与行上显示的不同的不应确认: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow([]studioKeyListRow{{Last4: "cccc", Full: fakeClientKey}}, nil, denied, "", false); choice.confirmed {
		t.Fatalf("行上显示的是公共 Key: %+v", redactChoice(choice))
	}
	// 同一末四位有两条候选、都不是已保存的：不猜
	twin := "AQ.TESTKEY" + strings.Repeat("z", 20) + "xxxx"
	if choice := chooseStudioKeyRow(rows[:1], []StudioKeyCandidate{{Key: fakeNewKey}, {Key: twin}}, none, "", false); choice.confirmed {
		t.Fatalf("同一末四位有两条候选时不应猜: %+v", redactChoice(choice))
	}
	// 只读：只认已保存的那条
	if choice := chooseStudioKeyRow(rows, candidates, denied, fakeOtherKey, true); !choice.confirmed || choice.candidate.Key != fakeOtherKey {
		t.Fatalf("只读应选已保存的那条: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(rows, candidates[:1], denied, fakeOtherKey, true); choice.confirmed || !choice.hasRow || choice.row.Last4 != "TST1" {
		t.Fatalf("只读、响应里没有已保存的那条：目标仍是它那一行: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(rows[:1], candidates, denied, fakeOtherKey, true); choice.confirmed || choice.hasRow {
		t.Fatalf("只读、列表里没有已保存的那条: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(rows, candidates, denied, "", true); choice.confirmed || choice.hasRow {
		t.Fatalf("只读、已保存的 Key 不可用（在拒绝集合里）时不应选任何一行: %+v", redactChoice(choice))
	}
	if choice := chooseStudioKeyRow(rows, []StudioKeyCandidate{{Key: twin}}, none, fakeNewKey, true); choice.confirmed {
		t.Fatalf("只读时末四位相同、完整 Key 不同的不算: %+v", redactChoice(choice))
	}
}

// TestNewStudioKeyNeedsCorroboration 创建之后，创建结果对话框里显示的完整 Key 单独不算：要有列表里新出现的一行末四位对上，或（还没有新行时）
// 响应里有同一条才认；对话框关掉之后记下的 Key 仍可与之后出现的新行核对；新行对不上时不用（返回新行，之后打开它的详情）；
// 拒绝集合里的、创建前就有的不算
func TestNewStudioKeyNeedsCorroboration(t *testing.T) {
	newRun := func() *studioKeyRun {
		run := &studioKeyRun{flow: newStudioKeyFlow()}
		run.markBefore(studioKeyListView{Rows: []studioKeyListRow{{Last4: "0000"}}})
		return run
	}
	run := newRun()
	if _, pending, awaiting, ok := run.newStudioKey(studioKeyListView{}, []string{fakeNewKey}); ok || pending != nil || !awaiting {
		t.Fatalf("只有对话框里的 Key、没有新行与响应时应等印证: ok=%v awaiting=%v", ok, awaiting)
	}
	if !strings.Contains(strings.Join(run.notes, "→"), "还没有新出现的行或响应印证") {
		t.Fatalf("应记一笔: %v", run.notes)
	}
	// 对话框关掉之后（读不到对话框里的 Key）出现新的一行，末四位与记下的对上
	if candidate, _, _, ok := run.newStudioKey(studioKeyListView{Rows: []studioKeyListRow{{Last4: "0000"}, {Last4: "xxxx"}}}, nil); !ok || candidate.Key != fakeNewKey {
		t.Fatalf("应与新出现的一行核对后认下: ok=%v", ok)
	}
	run = newRun()
	run.keys = []StudioKeyCandidate{{Key: fakeNewKey, ProjectNumber: "123456789012"}}
	if candidate, _, _, ok := run.newStudioKey(studioKeyListView{}, []string{fakeNewKey}); !ok || candidate.ProjectNumber != "123456789012" {
		t.Fatalf("响应里有同一条时应认下（连同响应里的项目）: ok=%v", ok)
	}
	// 新出现的行对不上对话框里的 Key：不用，返回这一行（之后打开它的详情）
	run = newRun()
	if _, pending, _, ok := run.newStudioKey(studioKeyListView{Rows: []studioKeyListRow{{Last4: "TST1"}}}, []string{fakeNewKey}); ok || pending == nil ||
		pending.Last4 != "TST1" || !strings.Contains(strings.Join(run.notes, "→"), "对不上") {
		t.Fatalf("新行对不上时不应认下: ok=%v notes=%v", ok, run.notes)
	}
	// 拒绝集合里的、创建前就有的不算
	run = newRun()
	run.deny(fakeNewKey, studioKeyDenySource{where: "测试", strong: true})
	run.keys = []StudioKeyCandidate{{Key: fakeNewKey}}
	if _, _, awaiting, ok := run.newStudioKey(studioKeyListView{Rows: []studioKeyListRow{{Last4: "xxxx"}}}, []string{fakeNewKey}); ok || awaiting {
		t.Fatal("拒绝集合里的 Key 不应认下")
	}
	run = &studioKeyRun{flow: newStudioKeyFlow(), keys: []StudioKeyCandidate{{Key: fakeNewKey}}}
	run.markBefore(studioKeyListView{})
	if _, _, awaiting, ok := run.newStudioKey(studioKeyListView{}, []string{fakeNewKey}); ok || awaiting {
		t.Fatal("创建前就在响应里的 Key 不应认下")
	}
}

func redactChoice(choice studioKeyChoice) studioKeyChoice {
	choice.candidate = redactCandidate(choice.candidate)
	choice.row.Full = RedactStudioKeys(choice.row.Full)
	return choice
}

// TestExtractStudioKeysOwnRecord Key 自己那条记录里的资源名 projects/<数字>/…/keys/… 算它的项目，并优先于多条 Key 共用的容器里的
// projects/<数字>；记录里的 gen-lang-client-<数字> 记作项目 ID；别的 Key 的记录里的 ID 不串过来
func TestExtractStudioKeysOwnRecord(t *testing.T) {
	body := `)]}'
[[["projects/123456789012/locations/global/keys/k1","Gemini API Key","` + fakeNewKey + `"],
["k2","` + fakeOtherKey + `","gen-lang-client-0987654321"]],"projects/210987654321"]`
	keys := ExtractStudioKeys([]byte(body))
	if len(keys) != 2 {
		t.Fatalf("keys = %+v", redactCandidates(keys))
	}
	if keys[0].ProjectNumber != "123456789012" || keys[0].ProjectName != "projects/123456789012" || keys[0].ProjectID != "" {
		t.Fatalf("第一条应按自己的资源名: %+v", redactCandidate(keys[0]))
	}
	// 第二条自己的记录里没有编号：共用容器里的 projects/<数字> 照旧算它的（与改动前一致），项目 ID 取自己记录里的
	if keys[1].ProjectID != "gen-lang-client-0987654321" || keys[1].ProjectNumber != "210987654321" {
		t.Fatalf("第二条: %+v", redactCandidate(keys[1]))
	}
	if !slices.Contains(keys[0].projectHints, "Gemini API Key") || slices.Contains(keys[1].projectHints, "Gemini API Key") {
		t.Fatalf("显示名线索只取自己的记录: %v / %v", keys[0].projectHints, keys[1].projectHints)
	}
}

// TestCorrelateStudioProject Key 的记录里没有项目编号时与响应里的项目对照：按项目 ID、按显示名（逐字）、响应里只有一个项目；
// 多个项目又没有依据、显示名对上多个项目、项目 ID 与唯一的项目矛盾时都不猜
func TestCorrelateStudioProject(t *testing.T) {
	defaultProject := StudioProject{Name: "projects/210987654321", Number: "210987654321", ID: "gen-lang-client-0987654321", Display: "Default Gemini Project"}
	other := StudioProject{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"}
	twin := StudioProject{Name: "projects/345678901234", Number: "345678901234", Display: "Default Gemini Project"}
	cases := []struct {
		name      string
		candidate StudioKeyCandidate
		projects  []StudioProject
		want      string
		how       string
	}{
		{"id", StudioKeyCandidate{ProjectID: "gen-lang-client-0123456789"}, []StudioProject{defaultProject, other}, "123456789012", studioKeyMatchID},
		{"display", StudioKeyCandidate{projectHints: []string{"Gemini API Key", "default gemini project"}}, []StudioProject{defaultProject, other}, "210987654321", studioKeyMatchDisplay},
		{"single", StudioKeyCandidate{}, []StudioProject{other, {Name: other.Name, Number: other.Number}}, "123456789012", studioKeyMatchSingle},
		{"ambiguous", StudioKeyCandidate{projectHints: []string{"Gemini API Key"}}, []StudioProject{defaultProject, other}, "", ""},
		{"twins", StudioKeyCandidate{projectHints: []string{"Default Gemini Project"}}, []StudioProject{defaultProject, twin}, "", ""},
		{"contradict", StudioKeyCandidate{ProjectID: "gen-lang-client-5555555555"}, []StudioProject{defaultProject}, "", ""},
		{"none", StudioKeyCandidate{}, nil, "", ""},
	}
	for _, item := range cases {
		project, how, ok := correlateStudioProject(item.candidate, item.projects)
		if ok != (item.want != "") || project.Number != item.want || how != item.how {
			t.Fatalf("%s: project=%+v how=%q ok=%v，期望 %q %q", item.name, project, how, ok, item.want, item.how)
		}
	}
}

// TestStudioKeyRowProject 从 Key 列表里这一行读项目：项目列整段对上已知项目的显示名（只对上一个）、项目列里的项目 ID 与编号；
// 对不上时项目列的文字记作显示名；没有表头时不把普通文字当显示名，只对照已知项目
func TestStudioKeyRowProject(t *testing.T) {
	projects := []StudioProject{
		{Name: "projects/210987654321", Number: "210987654321", ID: "gen-lang-client-0987654321", Display: "Default Gemini Project"},
		{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"},
	}
	row := func(project string, headed bool) studioKeyRowView {
		header := func(text string) string {
			if headed {
				return text
			}
			return ""
		}
		return studioKeyRowView{Count: 1, Cells: []studioKeyRowCell{
			{Text: "Gemini API Key …TST1", Header: header("Key"), Key: true}, {Text: project, Header: header("Project")},
			{Text: "Nov 1, 2026", Header: header("Created on")},
		}}
	}
	if project, how := studioKeyRowProject(row("Default Gemini Project", true), projects); project.Number != "210987654321" || how != studioKeyMatchDisplay {
		t.Fatalf("显示名: %+v %s", project, how)
	}
	if project, _ := studioKeyRowProject(row("gen-lang-client-0123456789", true), projects); project.Number != "123456789012" {
		t.Fatalf("项目 ID: %+v", project)
	}
	if project, how := studioKeyRowProject(row("345678901234", true), projects); project.Number != "345678901234" || project.Name != "projects/345678901234" ||
		project.Display != "" || how != "项目编号" {
		t.Fatalf("项目列里只有编号: %+v %s", project, how)
	}
	if project, _ := studioKeyRowProject(row("My Project · 345678901234", true), projects); project.Number != "" {
		t.Fatalf("显示名后面带着的数字不是已知项目的编号时不当编号: %+v", project)
	}
	known := append(slices.Clone(projects), StudioProject{Name: "projects/345678901234", Number: "345678901234"})
	if project, _ := studioKeyRowProject(row("My Project · 345678901234", true), known); project.Number != "345678901234" || project.Display != "My Project" {
		t.Fatalf("数字是已知项目的编号: %+v", project)
	}
	// 显示名里带着 6 位以上的数字：按显示名逐字对上，不把其中的数字当编号；对不上已知项目时只记显示名、不记编号（留给详情）
	teams := []StudioProject{{Name: "projects/123456789012", Number: "123456789012", Display: "Team Project 202611"}, projects[0]}
	if project, how := studioKeyRowProject(row("Team Project 202611", true), teams); project.Number != "123456789012" || how != studioKeyMatchDisplay {
		t.Fatalf("显示名里带数字: %+v %s", project, how)
	}
	if project, _ := studioKeyRowProject(row("Load test 2026111601", true), projects); project.Number != "" || project.Display != "Load test 2026111601" {
		t.Fatalf("显示名里带数字、对不上已知项目: %+v", project)
	}
	// 项目列有两格：显示名一格、编号一格
	split := studioKeyRowView{Count: 1, Cells: []studioKeyRowCell{{Text: "Gemini API Key …TST1", Header: "Key", Key: true},
		{Text: "Load test 2026111601", Header: "Project"}, {Text: "456789012345", Header: "Project number"}}}
	if project, _ := studioKeyRowProject(split, projects); project.Number != "456789012345" || project.Display != "Load test 2026111601" {
		t.Fatalf("项目列分成显示名与编号两格: %+v", project)
	}
	// 项目列里的项目 ID 与数字对应的已知项目矛盾：不用这个数字
	if project, _ := studioKeyRowProject(row("gen-lang-client-0555555555 · 123456789012 extra", true), projects); project.Number != "" ||
		project.ID != "gen-lang-client-0555555555" {
		t.Fatalf("项目 ID 与编号矛盾时不用编号: %+v", project)
	}
	if project, _ := studioKeyRowProject(row("My Other Project", true), projects); project.Number != "" || project.Display != "My Other Project" {
		t.Fatalf("对不上已知项目时记显示名: %+v", project)
	}
	if project, _ := studioKeyRowProject(row("My Other Project", false), projects); project.Number != "" || project.Display != "" {
		t.Fatalf("没有表头时不猜显示名: %+v", project)
	}
	if project, _ := studioKeyRowProject(row("Gemini Project", false), projects); project.Number != "123456789012" {
		t.Fatalf("没有表头时整格对上已知显示名: %+v", project)
	}
}

// TestStudioKeyDetailsProject 详情里的各项：编号去掉空白与 projects/ 前缀，不是数字的不算；项目 ID 要像 Google Cloud 项目 ID；
// 只写着“项目”的一项按内容归类
func TestStudioKeyDetailsProject(t *testing.T) {
	project := studioKeyDetailsProject(studioKeyDetailsView{Dialog: true, Fields: []studioKeyField{
		{Kind: "name", Value: "Default Gemini Project"}, {Kind: "number", Value: "2109 8765 4321"}, {Kind: "id", Value: "gen-lang-client-0987654321"},
	}})
	if project.Number != "210987654321" || project.Name != "projects/210987654321" || project.Display != "Default Gemini Project" ||
		project.ID != "gen-lang-client-0987654321" {
		t.Fatalf("project = %+v", project)
	}
	if bad := studioKeyDetailsProject(studioKeyDetailsView{Fields: []studioKeyField{{Kind: "number", Value: "Nov 1"}, {Kind: "id", Value: "<key>"}}}); bad != (StudioProject{}) {
		t.Fatalf("不像编号与项目 ID 的不算: %+v", bad)
	}
	generic := studioKeyDetailsProject(studioKeyDetailsView{Fields: []studioKeyField{{Kind: "project", Value: "Default Gemini Project (gen-lang-client-0987654321)"}}})
	if generic.ID != "gen-lang-client-0987654321" || generic.Display != "Default Gemini Project" || generic.Number != "" {
		t.Fatalf("只写着“项目”的一项: %+v", generic)
	}
}

// TestStudioKeyFieldLabels 详情对话框的标签：英文、简体中文、繁体中文、日文、韩文，带冒号也认；Key 名称、创建时间之类不算
func TestStudioKeyFieldLabels(t *testing.T) {
	for text, want := range map[string]string{
		"Project name": "name", "Project number": "number", "Project ID": "id", "Project number:": "number", "Project": "project",
		"项目名称": "name", "项目编号": "number", "项目 ID": "id", "项目编号：": "number",
		"專案名稱": "name", "專案編號": "number", "專案 ID": "id",
		"プロジェクト名": "name", "プロジェクト番号": "number", "プロジェクト ID": "id",
		"프로젝트 이름": "name", "프로젝트 번호": "number", "프로젝트 ID": "id",
		"Name": "", "API key": "", "Created": "", "Project number 123": "", "Default Gemini Project": "",
	} {
		if got := studioKeyFieldKind(text); got != want {
			t.Fatalf("%q = %q，期望 %q", text, got, want)
		}
	}
}

type errorString string

func (err errorString) Error() string { return string(err) }

func redactCandidate(candidate StudioKeyCandidate) StudioKeyCandidate {
	candidate.Key = RedactStudioKeys(candidate.Key)
	return candidate
}

func redactCandidates(keys []StudioKeyCandidate) []StudioKeyCandidate {
	out := make([]StudioKeyCandidate, len(keys))
	for index, key := range keys {
		out[index] = redactCandidate(key)
	}
	return out
}

// TestExtractStudioProjectsOperation 建项目接口的长操作结果（Cloud Resource Manager 形态，合成数据）：编号、项目 ID 与显示名，
// 状态之类的大写枚举不当显示名
func TestExtractStudioProjectsOperation(t *testing.T) {
	body := `{"name":"operations/cp.7340000000000000000","done":true,"response":{"@type":"type.googleapis.com/google.cloudresourcemanager.v3.Project",` +
		`"name":"projects/123456789012","parent":"organizations/0","projectId":"gen-lang-client-0123456789","state":"ACTIVE",` +
		`"displayName":"Gemini Project","createTime":"2026-11-16T08:00:00.000Z"}}`
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 1 {
		t.Fatalf("projects = %+v", projects)
	}
	want := StudioProject{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"}
	if projects[0] != want {
		t.Fatalf("project = %+v，期望 %+v", projects[0], want)
	}
}

// TestExtractStudioProjectsNestedArrays 页面自己的项目列表（protobuf-JSON 嵌套数组、带 XSSI 前缀，合成数据）：每条记录取编号、
// 同一层的 gen-lang-client-<数字> 与显示名；大写的状态枚举不当显示名
func TestExtractStudioProjectsNestedArrays(t *testing.T) {
	body := ")]}'\n" + `[[["projects/123456789012","gen-lang-client-0123456789","ACTIVE","Gemini Project",null,[1763280000,0]],` +
		`["projects/210987654321","gen-lang-client-0987654321","Default Gemini Project"]]]`
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 2 || projects[0].Number != "123456789012" || projects[0].ID != "gen-lang-client-0123456789" ||
		projects[0].Display != "Gemini Project" || projects[1].Display != "Default Gemini Project" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestExtractStudioProjectsBatchExecute batchexecute 分块、项目在再包一层的 JSON 字符串里；Key 的资源名（projects/<数字>/…/keys/…）
// 也算出它所在的项目，与另一处的显示名合并
func TestExtractStudioProjectsBatchExecute(t *testing.T) {
	inner, _ := json.Marshal([]any{[]any{"projects/123456789012/locations/global/keys/abc", nil, []any{"projects/123456789012", "Gemini Project"}}})
	outer, _ := json.Marshal([]any{[]any{"wrb.fr", "CreateProject", string(inner), nil, nil, "generic"}})
	body := ")]}'\n\n" + "123\n" + string(outer) + "\n25\n[[\"e\",4,null,null,1]]\n"
	projects := ExtractStudioProjects([]byte(body))
	if len(projects) != 1 || projects[0].Name != "projects/123456789012" || projects[0].Display != "Gemini Project" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestExtractStudioProjectsIgnoresNoise 没有项目、编号太短（页面脚本里偶然出现的短串）、前面紧接着字母的（subprojects/…）都不算；
// 不是 JSON 时按文本扫描
func TestExtractStudioProjectsIgnoresNoise(t *testing.T) {
	for _, body := range []string{`{"a":"b"}`, `["projects/12","x"]`, `var a="subprojects/1234567890";`} {
		if projects := ExtractStudioProjects([]byte(body)); len(projects) != 0 {
			t.Fatalf("%s: projects = %+v", body, projects)
		}
	}
	projects := ExtractStudioProjects([]byte(`<html>see projects/123456789012 and projects/123456789012/x</html>`))
	if len(projects) != 1 || projects[0].Number != "123456789012" {
		t.Fatalf("projects = %+v", projects)
	}
}

// TestMergeStudioProjects 同一编号只留一条，后来的结果补上 ID 与显示名
func TestMergeStudioProjects(t *testing.T) {
	merged := mergeStudioProjects([]StudioProject{{Name: "projects/123456789012", Number: "123456789012"}},
		[]StudioProject{{Name: "projects/123456789012", Number: "123456789012", ID: "gen-lang-client-0123456789", Display: "Gemini Project"},
			{Name: "projects/210987654321", Number: "210987654321"}})
	if len(merged) != 2 || merged[0].ID != "gen-lang-client-0123456789" || merged[0].Display != "Gemini Project" {
		t.Fatalf("merged = %+v", merged)
	}
}

// TestExtractStudioProjectsKeyRecordName Key 列表的记录（资源名 projects/<数字>/…/keys/…、对象的 parent 字段、同一层或下一层含有 Key
// 的容器）只给出项目编号（与 ID），不把 Key 自己的名字（“Gemini API Key”）当项目显示名；与项目列表合并时不论谁先到，显示名都是项目列表里的
func TestExtractStudioProjectsKeyRecordName(t *testing.T) {
	list := ")]}'\n" + `[[["projects/123456789012","gen-lang-client-0123456789","Default Gemini Project"]]]`
	for name, keys := range map[string]string{
		"resource":   ")]}'\n" + `[[["projects/123456789012/locations/global/keys/k1","Gemini API Key","` + fakeNewKey + `"]]]`,
		"object":     `{"keys":[{"name":"projects/123456789012/locations/global/keys/k1","displayName":"Gemini API Key","keyString":"` + fakeNewKey + `"}]}`,
		"parent":     `{"keys":[{"parent":"projects/123456789012","displayName":"Gemini API Key","uid":"k1"}]}`,
		"with-key":   `[[["projects/123456789012","Gemini API Key","` + fakeNewKey + `"]]]`,
		"nested-key": `[[["projects/123456789012","Gemini API Key",["` + fakeNewKey + `"]]]]`,
	} {
		alone := ExtractStudioProjects([]byte(keys))
		if len(alone) != 1 || alone[0].Number != "123456789012" || alone[0].Display != "" {
			t.Fatalf("%s: projects = %+v", name, alone)
		}
		keysFirst := mergeStudioProjects(ExtractStudioProjects([]byte(keys)), ExtractStudioProjects([]byte(list)))
		listFirst := mergeStudioProjects(ExtractStudioProjects([]byte(list)), ExtractStudioProjects([]byte(keys)))
		for _, merged := range [][]StudioProject{keysFirst, listFirst} {
			if len(merged) != 1 || merged[0].Display != "Default Gemini Project" || merged[0].ID != "gen-lang-client-0123456789" {
				t.Fatalf("%s: merged = %+v", name, merged)
			}
		}
	}
	// Key 自己那条记录里与 projects/<数字> 同一层的文字也不当这条 Key 的项目显示名（记录里另有项目自己的子记录时取那里的，见
	// TestExtractStudioKeysNestedArrays）
	for _, body := range []string{`[[["projects/123456789012","Gemini API Key","` + fakeNewKey + `"]]]`,
		`{"keys":[{"parent":"projects/123456789012","displayName":"Gemini API Key","keyString":"` + fakeNewKey + `"}]}`} {
		keys := ExtractStudioKeys([]byte(body))
		if len(keys) != 1 || keys[0].ProjectNumber != "123456789012" || keys[0].ProjectDisplay != "" {
			t.Fatalf("%s: keys = %+v", body, redactCandidates(keys))
		}
	}
}

// TestStudioKeyRunMatch 对照响应里的项目：strongOnly（打开详情之后）不认“唯一的项目”；对上的项目与已读到的显示名、项目 ID 矛盾时
// 不认（Key 后来的记录里带着编号时也一样）；项目 ID、Key 的记录是强依据
func TestStudioKeyRunMatch(t *testing.T) {
	unrelated := StudioProject{Name: "projects/999999999999", Number: "999999999999", ID: "gen-lang-client-0999999999", Display: "Unrelated Project"}
	run := &studioKeyRun{flow: newStudioKeyFlow(), keys: []StudioKeyCandidate{{Key: fakeNewKey}}}
	run.flow.noteProjects([]StudioProject{unrelated})
	if _, how, ok := run.match(StudioKeyResult{Key: fakeNewKey}, false); !ok || how != studioKeyMatchSingle {
		t.Fatalf("打开详情之前应认唯一的项目: %v %q", ok, how)
	}
	if _, _, ok := run.match(StudioKeyResult{Key: fakeNewKey}, true); ok {
		t.Fatal("strongOnly 时不应认唯一的项目")
	}
	if _, _, ok := run.match(StudioKeyResult{Key: fakeNewKey, ProjectDisplay: "My Other Project"}, false); ok {
		t.Fatal("显示名矛盾时不应认")
	}
	if _, _, ok := run.match(StudioKeyResult{Key: fakeNewKey, ProjectID: "gen-lang-client-0123456789"}, false); ok {
		t.Fatal("项目 ID 矛盾时不应认")
	}
	if project, how, ok := run.match(StudioKeyResult{Key: fakeNewKey, ProjectID: unrelated.ID}, true); !ok || how != studioKeyMatchID || project.Number != unrelated.Number {
		t.Fatalf("项目 ID 对上是强依据: %v %q %+v", ok, how, project)
	}
	run.keys[0].ProjectName, run.keys[0].ProjectNumber = unrelated.Name, unrelated.Number
	if project, how, ok := run.match(StudioKeyResult{Key: fakeNewKey}, true); !ok || how != studioKeyMatchRecord || project != unrelated {
		t.Fatalf("Key 的记录里带着编号: %v %q %+v", ok, how, project)
	}
	if _, _, ok := run.match(StudioKeyResult{Key: fakeNewKey, ProjectDisplay: "My Other Project"}, true); ok {
		t.Fatal("Key 的记录对上的项目与显示名矛盾时也不应认")
	}
}

// TestStudioKeyRunAdopt 定下项目时显示名与项目 ID 先取这个项目自己的；结果里原有的（读自 Key 列表这一行）是响应里别的项目的就去掉，
// 不是时才留下
func TestStudioKeyRunAdopt(t *testing.T) {
	unrelated := StudioProject{Name: "projects/999999999999", Number: "999999999999", ID: "gen-lang-client-0999999999", Display: "Unrelated Project"}
	run := &studioKeyRun{flow: newStudioKeyFlow()}
	run.flow.noteProjects([]StudioProject{unrelated, {Name: "projects/345678901234", Number: "345678901234"}})
	result := StudioKeyResult{ProjectDisplay: "unrelated project", ProjectID: unrelated.ID}
	run.adopt(&result, StudioProject{Number: "345678901234"})
	if result.ProjectNumber != "345678901234" || result.ProjectName != "projects/345678901234" || result.ProjectDisplay != "" || result.ProjectID != "" {
		t.Fatalf("不应留下别的项目的显示名与 ID: %+v", result)
	}
	result = StudioKeyResult{ProjectDisplay: "My Other Project", ProjectID: "gen-lang-client-0345678901"}
	run.adopt(&result, StudioProject{Number: "345678901234"})
	if result.ProjectDisplay != "My Other Project" || result.ProjectID != "gen-lang-client-0345678901" {
		t.Fatalf("不矛盾时应留下: %+v", result)
	}
	result = StudioKeyResult{ProjectDisplay: "My Other Project"}
	run.adopt(&result, unrelated)
	if result.ProjectDisplay != "Unrelated Project" || result.ProjectID != unrelated.ID {
		t.Fatalf("项目自己的显示名优先: %+v", result)
	}
}
