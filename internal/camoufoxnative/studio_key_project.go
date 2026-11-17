package camoufoxnative

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
)

// 补 Key 的项目信息（见 studioKeyRun.finish）：选定的 Key 在页面自己的响应里没有项目编号时，按下面的顺序找，找到编号就停：
//  1. 与这次所有响应里的项目对照（correlateStudioProject：Key 记录里的项目 ID、显示名，或响应里一共只有一个项目）；
//  2. 读 Key 列表里这一行（只读表达式 studioKeyRowExpression）：主区域里显示着这条 Key 打码末四位的那一行（表格行、列表项），
//     按表头带“项目”字样的列（没有表头时看整行）找项目 ID、projects/<数字>，或整段对上响应里某个项目的显示名；单独的 6 位以上数字
//     只在这一格只有它、或它是已知项目的编号时才算编号（显示名里带着的数字不算，见 studioKeyRowProject）；
//     对不上已知项目时把项目列的文字当显示名记下；
//  3. 还没有编号：用真实指针点这一行自己的 Key 名称（或打码的 Key 本身）打开“API key details”对话框（每次运行最多一次；
//     页面上有别的对话框或下拉列表挡着时不点；删除、复制、重新生成、编辑、限制、更多操作这类控件一律不点），按标签读出
//     Project name、Project number、Project ID（英文、简体中文、繁体中文、日文、韩文；先按结构：dt/dd、表格的表头格与下一格、
//     label 与它的控件、aria-labelledby，再看紧挨着的下一个元素或“标签：值”写在一起的文字），再用对话框自己的关闭类按钮关掉。
//     点开之后新到的响应只按强依据（Key 的记录、项目 ID、显示名）对照，不认“唯一的项目”；详情里按标签写着的编号与响应对上的
//     不一致时按详情。
//
// 任何一步对上的项目与已读到的项目 ID、显示名矛盾时都不用（不把一个项目的显示名和另一个项目的编号、ID 拼在一起）。
// 有编号、没有显示名时（如 Key 的资源名里只有编号）只读地补显示名：响应里这个项目的显示名，或 Key 列表这一行的项目列（completeDisplay）。
// 都没找到时照样返回 Key（不算失败），步骤里记一笔。页面文字在页面里先打码（Key 换成 <key>、邮箱留前两位）再交给 Go。

// studioKeyDetailsSettle 为详情对话框出现后等项目编号显示出来的时间（测试里不改；对话框里没有编号时最多多等这么久）
var studioKeyDetailsSettle = 3 * time.Second

// studioKeyRun 为一次获取里读响应、核对 Key、补项目信息要用到的状态
type studioKeyRun struct {
	session   *loginSession
	collector string
	listener  *studioKeyListener
	flow      *studioKeyFlow
	options   StudioKeyOptions
	// keys 为页面接口 JSON 类正文里的候选 Key（含拒绝集合里的；选用前都要核对，见 chooseStudioKeyRow）
	keys []StudioKeyCandidate
	// denied 为这次运行的拒绝集合（页面自己的公共 Key 等，见 studio_key_confirm.go），deniedOrder 为进集合的顺序（诊断用）
	denied      map[string]studioKeyDenySource
	deniedOrder []string
	// exclude、excludeLast4、excludeFingerprints 为 StudioKeyOptions 里排除的 Key、它们的末四位与只剩指纹的（见 loadExclusions）
	exclude             map[string]bool
	excludeLast4        map[string]bool
	excludeFingerprints map[string]bool
	// before 为开始创建之前的状态（之后只认新出现的 Key），没有创建时为空
	before *studioKeyBefore
	// list 为最近一次读到的 Key 列表（listed 表示读到过）
	list   studioKeyListView
	listed bool
	// diag 为带像 Key 字符串的响应（诊断用）；responses 为读过正文的响应条数，diagDropped 为超出上限没列出的条数
	diag        []studioKeyDiag
	responses   int
	diagDropped int
	// notes 为补项目信息的步骤记录（打码、截短，接在 StudioKeyResult.Steps 后面）
	notes []string
	// detailsOpened 表示这次已经点过一次 Key 的名称（打开详情最多一次）；details 为那次读到的内容
	detailsOpened bool
	details       *studioKeyDetailsOutcome
	// createdClosed 表示这次已经关过一次创建结果对话框；dialogMismatch 表示已记过创建结果对话框里的 Key 与新行对不上；
	// shownKeys 为创建结果对话框里显示过的新的完整 Key（机密：只在内存里，最多 4 条；对话框关掉之后还要拿它与新出现的行核对），
	// shownNoted 表示已记过对话框里的新 Key 还没有印证
	createdClosed  bool
	dialogMismatch bool
	shownKeys      []string
	shownNoted     bool
	// chosen 为最后选用的 Key（机密：只在内存里，诊断里标出它的末四位）
	chosen string
}

// drain 读出新到的响应正文：页面文档、脚本、样式里像 Key 的字符串进拒绝集合；页面接口的 JSON 类正文里整段是 Key 的字符串记作候选，
// 项目也只从这类正文里取（见 studioKeyBodyKind）
func (run *studioKeyRun) drain(ctx context.Context) {
	responses, denied := run.listener.take()
	for _, item := range denied {
		run.deny(item.key, item.source)
	}
	for _, response := range responses {
		body, ok := run.session.client.studioKeyResponseBody(ctx, run.collector, response.id)
		if !ok {
			continue
		}
		kind := studioKeyBodyKind(response, body)
		var found []string
		if studioKeyScanPattern.Match(body) {
			found = scanStudioKeyStrings(string(body))
		}
		var candidates []StudioKeyCandidate
		switch kind {
		case studioKeyBodyPage:
			for _, key := range found {
				run.deny(key, studioKeyDenySource{where: "页面文档或脚本（" + response.host + "/" + response.name + "）", strong: true})
			}
		case studioKeyBodyJSON:
			candidates = ExtractStudioKeys(body)
			run.keys = mergeStudioKeys(run.keys, candidates)
			run.flow.noteProjects(ExtractStudioProjects(body))
		}
		run.noteResponse(response, kind, found, candidates)
	}
}

func (run *studioKeyRun) note(text string) {
	text = studioKeyText(text, 80)
	if text != "" && len(run.notes) < 8 {
		run.notes = append(run.notes, text)
	}
}

// candidate 返回响应里这条 Key 合并后的记录
func (run *studioKeyRun) candidate(key string) StudioKeyCandidate {
	for _, candidate := range run.keys {
		if candidate.Key == key {
			return candidate
		}
	}
	return StudioKeyCandidate{Key: key}
}

// finish 整理结果；没有项目编号时按顺序补（见文件开头），有编号、没有显示名时只读地补显示名（completeDisplay）；找不到不算失败。
// 创建过或打开过详情时步骤里附上诊断（只有末四位）
func (run *studioKeyRun) finish(ctx context.Context, chosen StudioKeyCandidate) StudioKeyResult {
	run.chosen = chosen.Key
	result := run.flow.result(chosen, len(run.list.Rows))
	switch {
	case result.ProjectNumber == "":
		run.completeProject(ctx, &result)
	case result.ProjectDisplay == "":
		run.completeDisplay(ctx, &result)
	}
	result.Steps = append(result.Steps, run.notes...)
	result.ClientFingerprints = run.clientFingerprints()
	if result.Created || run.detailsOpened {
		if diag := run.diagnostics(); diag != "" {
			result.Steps = append(result.Steps, "诊断："+diag)
		}
	}
	return result
}

// completeDisplay 有项目编号、没有显示名时（如 Key 的资源名里只有编号，主循环读到它就结束）只读地补显示名，不点任何东西：
// 先看新到的响应里这个项目的显示名；再读 Key 列表里这一行的项目列（写着别的编号、别的项目 ID，或整段对上别的已知项目时不用）；
// 都没有时再等一会儿后到的响应（最多 studioKeyTiming.projectName）
func (run *studioKeyRun) completeDisplay(ctx context.Context, result *StudioKeyResult) {
	if run.knownDisplay(ctx, result) {
		return
	}
	if last4 := StudioKeyLast4(result.Key); studioKeyLast4Pattern.MatchString(last4) {
		if row, ok := run.readRow(ctx, last4); ok {
			project, _ := studioKeyRowProject(row, run.flow.projects)
			switch {
			case project.Number != "" && project.Number != result.ProjectNumber,
				project.ID != "" && result.ProjectID != "" && project.ID != result.ProjectID:
				run.note("Key 列表里这一行的项目与响应里的不一致，没有用")
			case project.Display != "":
				result.ProjectDisplay = project.Display
				if result.ProjectID == "" {
					result.ProjectID = project.ID
				}
				run.note("项目显示名：从 Key 列表里这一行读到")
				return
			}
		}
	}
	for deadline := time.Now().Add(studioKeyTiming.projectName); ctx.Err() == nil && time.Now().Before(deadline); {
		if waitContext(ctx, studioKeyTiming.poll) != nil {
			break
		}
		if run.knownDisplay(ctx, result) {
			return
		}
	}
	run.note("项目显示名：没有找到")
}

// knownDisplay 读新到的响应，响应里这条 Key 或这个编号的项目有显示名时补上
func (run *studioKeyRun) knownDisplay(ctx context.Context, result *StudioKeyResult) bool {
	run.drain(ctx)
	if candidate := run.candidate(result.Key); candidate.ProjectNumber == result.ProjectNumber && candidate.ProjectDisplay != "" {
		applyStudioProject(result, StudioProject{Number: candidate.ProjectNumber, ID: candidate.ProjectID, Display: candidate.ProjectDisplay})
	}
	if project, ok := run.flow.project(result.ProjectNumber); ok {
		applyStudioProject(result, project)
	}
	return result.ProjectDisplay != ""
}

// completeProject 依次与响应对照、读 Key 列表里这一行、打开这条 Key 的详情对话框，补上项目编号（以及显示名与项目 ID）。
// 为读完整 Key 已经打开过详情时先用那次读到的（详情按标签写着这条 Key 的项目，最可靠），不再点第二次
func (run *studioKeyRun) completeProject(ctx context.Context, result *StudioKeyResult) {
	if run.details != nil {
		run.applyDetails(result, *run.details)
		if result.ProjectNumber != "" {
			return
		}
	}
	if how, ok := run.correlate(result, false); ok {
		run.note("项目信息：按" + how + "对上响应里的项目")
		return
	}
	last4 := StudioKeyLast4(result.Key)
	if !studioKeyLast4Pattern.MatchString(last4) {
		run.note("项目信息：没有找到")
		return
	}
	if row, ok := run.readRow(ctx, last4); ok {
		project, how := studioKeyRowProject(row, run.flow.projects)
		applyStudioProject(result, project)
		if result.ProjectNumber != "" {
			run.note("项目信息：从 Key 列表里这一行读到（" + how + "）")
			return
		}
		if how, ok := run.correlate(result, false); ok {
			run.note("项目信息：按 Key 列表里这一行的" + how + "对上响应里的项目")
			return
		}
	}
	if run.details == nil {
		run.openDetails(ctx, result, last4)
	}
	if result.ProjectNumber == "" {
		run.note("项目信息：没有找到项目编号")
	}
}

// correlate 用响应里的项目补上编号（见 match）；strongOnly 时不认“唯一的项目”这条弱依据
func (run *studioKeyRun) correlate(result *StudioKeyResult, strongOnly bool) (string, bool) {
	if result.ProjectNumber != "" {
		return "", true
	}
	project, how, ok := run.match(*result, strongOnly)
	if !ok {
		return "", false
	}
	applyStudioProject(result, project)
	return how, true
}

// match 用响应里的项目对照这条 Key（不改 result），返回对上的项目与依据：这条 Key 后来的响应里带着编号（Key 的记录），或按
// correlateStudioProject 对上（strongOnly 时不认“唯一的项目”）。对上的项目与 result 里已有的项目 ID、显示名（读自 Key 的记录、
// Key 列表这一行或详情）矛盾时不认：不把一个项目的显示名和另一个项目的编号、ID 拼在一起
func (run *studioKeyRun) match(result StudioKeyResult, strongOnly bool) (StudioProject, string, bool) {
	candidate := run.candidate(result.Key)
	var (
		project StudioProject
		how     string
	)
	if candidate.ProjectNumber != "" {
		project = StudioProject{Name: "projects/" + candidate.ProjectNumber, Number: candidate.ProjectNumber, ID: candidate.ProjectID,
			Display: candidate.ProjectDisplay}
		if known, ok := run.flow.project(project.Number); ok {
			project = mergeStudioProjects([]StudioProject{project}, []StudioProject{known})[0]
		}
		how = studioKeyMatchRecord
	} else {
		if candidate.ProjectID == "" {
			candidate.ProjectID = result.ProjectID
		}
		if result.ProjectDisplay != "" {
			// 页面上读到的显示名也拿来对照（复制一份，不改响应里那条记录）
			candidate.projectHints = append(slices.Clone(candidate.projectHints), result.ProjectDisplay)
		}
		var ok bool
		project, how, ok = correlateStudioProject(candidate, run.flow.projects)
		if !ok || strongOnly && how == studioKeyMatchSingle {
			return StudioProject{}, "", false
		}
	}
	if !studioProjectsAgree(project, StudioProject{ID: result.ProjectID, Display: result.ProjectDisplay}) {
		return StudioProject{}, "", false
	}
	return project, how, true
}

// studioProjectsAgree 判断两处读到的项目不矛盾：编号、项目 ID、显示名（不区分大小写）两边都有时须相同
func studioProjectsAgree(one, other StudioProject) bool {
	return (one.Number == "" || other.Number == "" || one.Number == other.Number) &&
		(one.ID == "" || other.ID == "" || one.ID == other.ID) &&
		(one.Display == "" || other.Display == "" || strings.EqualFold(one.Display, other.Display))
}

// adopt 把结果的项目定为 project（须有编号）：显示名与项目 ID 先用 project 的，再用响应里同一编号的项目的；结果里原有的显示名、ID
// （读自 Key 的记录或 Key 列表这一行）只在这两处都没有、且不是响应里别的项目的时才留下
func (run *studioKeyRun) adopt(result *StudioKeyResult, project StudioProject) {
	known, _ := run.flow.project(project.Number)
	display, id := project.Display, project.ID
	if display == "" {
		display = known.Display
	}
	if id == "" {
		id = known.ID
	}
	other := func(match func(StudioProject) bool) bool {
		return slices.ContainsFunc(run.flow.projects, func(item StudioProject) bool { return item.Number != project.Number && match(item) })
	}
	if display == "" && result.ProjectDisplay != "" &&
		!other(func(item StudioProject) bool { return strings.EqualFold(item.Display, result.ProjectDisplay) }) {
		display = result.ProjectDisplay
	}
	if id == "" && result.ProjectID != "" && !other(func(item StudioProject) bool { return item.ID == result.ProjectID }) {
		id = result.ProjectID
	}
	result.ProjectName, result.ProjectNumber, result.ProjectDisplay, result.ProjectID = "projects/"+project.Number, project.Number, display, id
}

// studioKeyRowCell 为 Key 列表里这一行的一格：Text 为打码、截短的文字，Header 为这一列的表头（没有表头时为空），Key 表示这格里显示着这条 Key
type studioKeyRowCell struct {
	Text   string `json:"t"`
	Header string `json:"h"`
	Key    bool   `json:"k"`
}

// studioKeyRowView 为 studioKeyRowExpression 的结果：Count 为对得上末四位的行数（不止一行时 Cells 为空）
type studioKeyRowView struct {
	Count int                `json:"count"`
	Cells []studioKeyRowCell `json:"cells"`
}

// readRow 读 Key 列表里这一行（列表可能还在渲染：没找到时隔一轮再读，最多三次）
func (run *studioKeyRun) readRow(ctx context.Context, last4 string) (studioKeyRowView, bool) {
	var view studioKeyRowView
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			if waitContext(ctx, studioKeyTiming.poll) != nil {
				break
			}
		}
		encoded, err := run.session.client.evaluateString(ctx, run.session.contextID, studioKeyRowExpression(last4))
		if err != nil || encoded == "" {
			continue
		}
		view = studioKeyRowView{}
		if json.Unmarshal([]byte(encoded), &view) != nil {
			continue
		}
		if view.Count > 0 {
			break
		}
	}
	switch {
	case view.Count == 0:
		run.note("Key 列表里没有找到这条 Key（…" + last4 + "）")
		return view, false
	case view.Count > 1 || len(view.Cells) == 0:
		run.note("Key 列表里有多行对得上末四位，不猜")
		return view, false
	}
	return view, true
}

var (
	studioProjectIDScan   = regexp.MustCompile(`\bgen-lang-client-[0-9]{6,}\b`)
	studioKeyLast4Pattern = regexp.MustCompile(`^[0-9A-Za-z_-]{4}$`)
	// studioCloudProjectID 为 Google Cloud 项目 ID 的格式（小写字母开头，6 到 30 位小写字母、数字与连字符）
	studioCloudProjectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
)

// studioKeyRowProject 从 Key 列表里这一行读项目：项目列（表头带“项目”字样；没有表头时整行）里的项目 ID、projects/<数字>，
// 再与响应里的项目对照（编号、ID，或整格文字去掉 ID 与 projects/<数字> 后逐字对上显示名，只对上一个时才算）。
// 项目列里单独的 6 位以上数字只在两种情况下算编号：这一格（去掉 ID 与 projects/<数字> 后）只有这个数字，或它就是响应里某个项目的编号；
// 显示名里带着的数字（如“Team Project 202611”）不当编号（编号一旦记错就不会再打开详情去读，补项目信息也不会再排它）。
// 对不上时项目列的文字（去掉 ID 与 projects/<数字> 后像显示名）记作显示名。返回读到的项目（可能只有显示名或 ID）与依据
func studioKeyRowProject(row studioKeyRowView, projects []StudioProject) (StudioProject, string) {
	var cells []studioKeyRowCell
	for _, cell := range row.Cells {
		if cell.Header != "" && studioKeyLabels.projectWord.MatchString(cell.Header) {
			cells = append(cells, cell)
		}
	}
	headed := len(cells) > 0
	if !headed {
		cells = row.Cells
	}
	var found StudioProject
	var texts []string
	for _, cell := range cells {
		text := cell.Text
		if found.ID == "" {
			found.ID = studioProjectIDScan.FindString(text)
		}
		if match := studioProjectTextScan.FindStringSubmatch(text); match != nil && found.Number == "" {
			found.Number = match[1]
		}
		rest := studioProjectTextScan.ReplaceAllString(studioProjectIDScan.ReplaceAllString(text, " "), " ")
		rest = strings.Trim(strings.Join(strings.Fields(rest), " "), " ·-–—|¦()（）,，")
		if rest != "" && !cell.Key {
			texts = append(texts, rest)
		}
	}
	if found.Number != "" {
		found.Name = "projects/" + found.Number
		if project, ok := uniqueStudioProject(projects, func(project StudioProject) bool { return project.Number == found.Number }); ok {
			return mergeStudioProjects([]StudioProject{found}, []StudioProject{project})[0], "项目编号"
		}
		return found, "项目编号"
	}
	if found.ID != "" {
		if project, ok := uniqueStudioProject(projects, func(project StudioProject) bool { return project.ID == found.ID }); ok {
			return project, studioKeyMatchID
		}
	}
	if project, ok := uniqueStudioProject(projects, func(project StudioProject) bool {
		for _, text := range texts {
			if project.Display != "" && strings.EqualFold(text, project.Display) {
				return true
			}
		}
		return false
	}); ok && (found.ID == "" || project.ID == "" || project.ID == found.ID) {
		return project, studioKeyMatchDisplay
	}
	if !headed {
		return found, "项目列"
	}
	if number, display := studioKeyRowBareNumber(texts, projects); number != "" {
		project, known := uniqueStudioProject(projects, func(project StudioProject) bool { return project.Number == number })
		switch {
		case !known:
			found.Number, found.Name, found.Display = number, "projects/"+number, display
			return found, "项目编号"
		case found.ID == "" || project.ID == "" || project.ID == found.ID:
			if project.Display == "" {
				project.Display = display
			}
			if project.ID == "" {
				project.ID = found.ID
			}
			project.Name = "projects/" + project.Number
			return project, "项目编号"
		}
		// 这一行的项目 ID 与这个编号的项目矛盾：不用这个编号
	}
	for _, text := range texts {
		if looksLikeDisplayName(text) {
			found.Display = text
			break
		}
	}
	return found, "项目列"
}

// studioKeyRowBareNumber 在项目列的文字里找能当项目编号的单独数字（6 位以上，前后是空白）：一格里只有这个数字，或它是 projects 里
// 某个项目的编号；几格里对上的不是同一个编号时不猜。返回编号与像显示名的文字（对上编号的那一格去掉这个数字后的，或其他格的）
func studioKeyRowBareNumber(texts []string, projects []StudioProject) (string, string) {
	number, display := "", ""
	for _, text := range texts {
		fields := strings.Fields(text)
		hit, rest := "", fields
		for index, field := range fields {
			if studioProjectDigitsOnly.MatchString(field) &&
				(len(fields) == 1 || slices.ContainsFunc(projects, func(project StudioProject) bool { return project.Number == field })) {
				hit, rest = field, append(slices.Clone(fields[:index]), fields[index+1:]...)
				break
			}
		}
		if hit != "" {
			if number != "" && number != hit {
				return "", ""
			}
			number = hit
		}
		if candidate := strings.Trim(strings.Join(rest, " "), " ·-–—|¦()（）,，"); display == "" && looksLikeDisplayName(candidate) {
			display = candidate
		}
	}
	return number, display
}

// studioKeyField 为详情对话框里按标签读到的一项：Kind 为 name、number、id 或 project（只写着“项目”的标签），Value 已打码、截短
type studioKeyField struct {
	Kind  string `json:"k"`
	Value string `json:"v"`
}

// studioKeyDetailsView 为 studioKeyDetailsExpression 的结果：Dialog 表示读的是最上层对话框，Page 表示点了之后页面换到了别的站内页面
// （读主区域），两者都假时没有可读的地方
type studioKeyDetailsView struct {
	Dialog bool             `json:"dialog"`
	Page   bool             `json:"page"`
	Title  string           `json:"title"`
	Fields []studioKeyField `json:"fields"`
}

// studioKeyDetailsProject 把详情里的各项整理成项目：编号去掉空白与 projects/ 前缀后须是 6 位以上数字，项目 ID 须是 Google Cloud
// 项目 ID 的格式，项目名须像显示名；只写着“项目”的一项按内容归到编号、ID 或显示名
func studioKeyDetailsProject(view studioKeyDetailsView) StudioProject {
	var project StudioProject
	setNumber := func(value string) {
		value = strings.TrimPrefix(strings.Join(strings.Fields(value), ""), "projects/")
		if project.Number == "" && studioProjectDigitsOnly.MatchString(value) {
			project.Number = value
		}
	}
	for _, field := range view.Fields {
		value := strings.TrimSpace(field.Value)
		switch field.Kind {
		case "number":
			setNumber(value)
		case "id":
			if project.ID == "" && studioCloudProjectID.MatchString(value) {
				project.ID = value
			}
		case "name":
			if project.Display == "" && looksLikeDisplayName(value) {
				project.Display = value
			}
		case "project":
			switch {
			case studioProjectTextScan.MatchString(value):
				setNumber(studioProjectTextScan.FindStringSubmatch(value)[1])
			case studioProjectIDScan.MatchString(value):
				if project.ID == "" {
					project.ID = studioProjectIDScan.FindString(value)
				}
				if rest := strings.Trim(strings.Join(strings.Fields(studioProjectIDScan.ReplaceAllString(value, " ")), " "), " ·-()（）"); project.Display == "" && looksLikeDisplayName(rest) {
					project.Display = rest
				}
			case project.Display == "" && looksLikeDisplayName(value):
				project.Display = value
			}
		}
	}
	if project.Number != "" {
		project.Name = "projects/" + project.Number
	}
	return project
}

var studioProjectDigitsOnly = regexp.MustCompile(`^[0-9]{6,}$`)

// studioKeyDetailsOutcome 为打开一次 Key 详情读到的内容：key 为末四位对上、不在拒绝集合里的完整 Key（机密，只在内存里；keyFrom 为
// 它的来处），view 为按标签读到的项目各项
type studioKeyDetailsOutcome struct {
	clicked bool
	shown   bool
	key     string
	keyFrom string
	view    studioKeyDetailsView
}

// openDetails 打开这条 Key 的详情对话框读项目（每次运行最多点一次），读完用对话框自己的关闭类按钮关掉
func (run *studioKeyRun) openDetails(ctx context.Context, result *StudioKeyResult, last4 string) {
	if run.details == nil {
		if run.detailsOpened {
			return
		}
		outcome := run.openRowDetails(ctx, last4, result.Key)
		run.details = &outcome
	}
	run.applyDetails(result, *run.details)
}

// openRowDetails 用真实指针点这一行自己的 Key 名称打开详情（每次运行最多一次；页面上有对话框或下拉列表挡着时不点；删除、复制、
// 重新生成、编辑、限制、更多操作这类控件与行里的复制图标一律不点），轮询读：known 为空时读完整 Key（点开之后新到的响应里末四位相同、
// 不在拒绝集合里的那条，或详情对话框里末四位相同的完整 Key），以及按标签写着的项目各项；读完用对话框自己的关闭类按钮关掉
func (run *studioKeyRun) openRowDetails(ctx context.Context, last4 string, known string) studioKeyDetailsOutcome {
	var outcome studioKeyDetailsOutcome
	if run.detailsOpened {
		return outcome
	}
	session := run.session
	page, inspected := session.studioKeyInspect(ctx)
	switch {
	case !inspected:
		run.note("读不到页面状态，没有打开 Key 详情")
		return outcome
	case page.Dialog != nil || len(page.Options) > 0:
		run.note("页面上有对话框挡着，没有打开 Key 详情")
		return outcome
	}
	run.detailsOpened = true
	before := page.Path
	if before == "" {
		before = StudioKeyPath
	}
	attempted, err := session.pointerClick(ctx, session.contextID, studioKeyDetailsTargetExpression(last4))
	if !attempted {
		if err == nil || errors.Is(err, errElementMissing) {
			run.note("Key 列表里这一行没有可点的 Key 名称，没有打开详情")
		} else {
			run.note("读取 Key 名称的位置失败，没有打开详情")
		}
		return outcome
	}
	outcome.clicked = true
	run.note("打开 Key 详情")
	var (
		view     studioKeyDetailsView
		shownAt  time.Time
		deadline = time.Now().Add(studioKeyTiming.noDialog)
		replied  bool
	)
	for ctx.Err() == nil {
		run.drain(ctx)
		key := known
		if key == "" {
			if outcome.key == "" {
				if found, ok := run.rowKeyFromResponses(last4); ok {
					outcome.key, outcome.keyFrom = found, "点开详情后的响应"
				} else if found, err := session.client.evaluateString(ctx, session.contextID, studioKeyDetailsKeyExpression(last4, before)); err == nil &&
					IsStudioKey(found) && StudioKeyLast4(found) == last4 && !run.isDenied(found) {
					outcome.key, outcome.keyFrom = found, "详情对话框"
				}
			}
			key = outcome.key
		}
		if key != "" && !replied {
			// 点开之后新到的响应里对上的项目：只认强依据（Key 的记录、项目 ID、显示名）；只用来决定何时不必再等，采用与否见 applyDetails
			_, _, replied = run.match(StudioKeyResult{Key: key}, true)
		}
		if encoded, err := session.client.evaluateString(ctx, session.contextID, studioKeyDetailsExpression(before)); err == nil && encoded != "" {
			var current studioKeyDetailsView
			if json.Unmarshal([]byte(encoded), &current) == nil {
				view = current
			}
		}
		now := time.Now()
		if (view.Dialog || view.Page) && shownAt.IsZero() {
			shownAt = now
		}
		if key != "" && (studioKeyDetailsProject(view).Number != "" || replied && !shownAt.IsZero()) {
			break
		}
		if !shownAt.IsZero() && now.Sub(shownAt) >= studioKeyDetailsSettle || shownAt.IsZero() && now.After(deadline) {
			break
		}
		if waitContext(ctx, studioKeyTiming.poll) != nil {
			break
		}
	}
	outcome.view, outcome.shown = view, !shownAt.IsZero()
	if !outcome.shown {
		run.note("点了 Key 名称，没有出现详情")
	}
	if !view.Dialog {
		return outcome
	}
	closed, err := session.pointerClick(ctx, session.contextID, studioKeyDetailsCloseExpression)
	switch {
	case closed && err == nil:
		run.note("关闭 Key 详情")
	case closed:
		run.note("关闭 Key 详情时出错")
	default:
		run.note("Key 详情里没有找到关闭按钮")
	}
	return outcome
}

// applyDetails 用详情里读到的项目补上结果的项目信息：与点开之后的响应对照（只认强依据），按标签写着的编号优先
func (run *studioKeyRun) applyDetails(result *StudioKeyResult, outcome studioKeyDetailsOutcome) {
	if !outcome.shown {
		return
	}
	reply, replyHow, replied := run.match(*result, true)
	details := studioKeyDetailsProject(outcome.view)
	found := details.Number != "" || details.Display != "" || details.ID != ""
	if !found && !replied {
		run.note("Key 详情里没有认出项目名与项目编号")
	}
	switch {
	case details.Number != "":
		// 详情是这条 Key 自己的对话框、按标签写着项目编号，比响应里的对照可靠：不一致时按详情，响应里那个项目的显示名与 ID 都不用
		if replied && reply.Number != details.Number {
			run.note("详情里的项目编号与响应里对上的不一致，按详情")
		} else if replied {
			details = mergeStudioProjects([]StudioProject{details}, []StudioProject{reply})[0]
		}
		run.adopt(result, details)
		run.note("项目信息：从 Key 详情读到")
	case replied && studioProjectsAgree(details, reply):
		// 详情里没有编号：按响应里对上的项目，详情里按标签写着的项目名与 ID 优先
		if details.Display != "" {
			reply.Display = details.Display
		}
		if details.ID != "" {
			reply.ID = details.ID
		}
		run.adopt(result, reply)
		run.note("项目信息：打开详情后按" + replyHow + "对上页面响应里的项目")
	default:
		if replied {
			run.note("详情里的项目与响应里对上的不一致，没有记项目编号")
		}
		// 详情里按标签写着的项目名与项目 ID 比 Key 列表那一行的文字可靠：有就用它，再只按强依据与响应对照
		if details.Display != "" {
			result.ProjectDisplay = details.Display
		}
		if details.ID != "" {
			result.ProjectID = details.ID
		}
		if found && !replied {
			if how, ok := run.correlate(result, true); ok {
				run.note("项目信息：按 Key 详情里的" + how + "对上响应里的项目")
			}
		}
	}
}

// 详情对话框里各项的标签（英文、简体中文、繁体中文、日文、韩文；写成 Go 与 JS 都能编译的子集，两边都不区分大小写，前后按整段匹配；
// 只用非捕获分组：“标签：值”写在一起时值是最后一个分组）
const (
	studioKeyFieldNumber  = `project\s*(?:number|no\.?|#)|项目\s*编号|项目\s*号码|项目号|專案\s*編號|專案\s*號碼|項目\s*編號|プロジェクト\s*番号|프로젝트\s*번호`
	studioKeyFieldID      = `project\s*id|项目\s*id|專案\s*id|項目\s*id|プロジェクト\s*id|프로젝트\s*id`
	studioKeyFieldName    = `project\s*name|项目\s*名称|项目名|專案\s*名稱|項目\s*名稱|プロジェクト\s*名|프로젝트\s*이름`
	studioKeyFieldProject = `project|项目|專案|項目|プロジェクト|프로젝트`
	// Key 列表里这一行不点的控件（删除、复制、重新生成、编辑、限制、停用、更多操作等），英文按整词
	studioKeyPatternRowAction = `\b(delete|remove|revoke|destroy|copy|regenerate|rotate|renew|edit|rename|restrict|restriction|restrictions|disable|more|menu|options|actions)\b|删除|移除|撤销|复制|重新生成|轮换|编辑|重命名|限制|停用|更多|操作|刪除|撤銷|複製|重新產生|編輯|重新命名|削除|コピー|再生成|編集|名前を変更|制限|無効|その他|삭제|복사|재생성|수정|편집|이름\s*변경|제한|사용\s*중지|더보기|작업`
	// 这一行不点的图标（ligature 名）
	studioKeyPatternRowIcon = `^(delete|delete_forever|delete_outline|content_copy|copy_all|edit|edit_note|more_vert|more_horiz|autorenew|refresh|sync|block|lock|key_off|settings|tune|link_off|close)$`
	// 详情对话框自己的关闭类按钮（整段文字）
	studioKeyPatternDetailsClose = `^\s*(close|done|ok|okay|got\s+it|dismiss|cancel|关闭|完成|确定|知道了|取消|關閉|確定|閉じる|完了|キャンセル|닫기|완료|확인|취소)\s*$`
	// 关闭类按钮的 aria-label（含有即可）
	studioKeyPatternDetailsCloseLabel = `\b(close|dismiss|done)\b|关闭|關閉|閉じる|닫기|完成|完了|완료`
)

// studioKeyFieldLabels 为 Go 侧按同样规则匹配标签用的正则（测试与核对用）
var studioKeyFieldLabels = map[string]*regexp.Regexp{
	"number":  studioKeyRegexp(`^\s*(?:` + studioKeyFieldNumber + `)\s*[:：]?\s*$`),
	"id":      studioKeyRegexp(`^\s*(?:` + studioKeyFieldID + `)\s*[:：]?\s*$`),
	"name":    studioKeyRegexp(`^\s*(?:` + studioKeyFieldName + `)\s*[:：]?\s*$`),
	"project": studioKeyRegexp(`^\s*(?:` + studioKeyFieldProject + `)\s*[:：]?\s*$`),
}

// studioKeyFieldKind 返回标签文字是哪一项（按编号、ID、项目名、只写着“项目”的顺序），认不出时为空
func studioKeyFieldKind(text string) string {
	for _, kind := range []string{"number", "id", "name", "project"} {
		if studioKeyFieldLabels[kind].MatchString(text) {
			return kind
		}
	}
	return ""
}

// studioKeyRowHelpers 为找 Key 列表里这一行的函数（依赖 pageDOMHelpers、studioKeyDOMHelpers；外层要有参数 last4）。
// 打码的 Key 写作“…TST1”“...TST1”“****TST1”（或完整的 Key），只在页面里比较，原文不交给 Go
var studioKeyRowHelpers = studioKeyListHelpers + `
  // studioKeyRow 找主区域（不在对话框与弹层里）显示着这条 Key 的那一行（见 rowOf）。对得上的行不止一个时 row 为 null（不猜）；
  // leaves 为显示着这条 Key 的最内层元素
  const studioKeyRow = () => {
    const root = keyArea();
    if (!root) return {count: 0, row: null, leaf: null, leaves: []};
    const items = keyItems(root);
    const leaves = items.filter(item => item.keys.some(key => key.l === last4)).map(item => item.leaf);
    const rows = [];
    for (const leaf of leaves) {
      const row = rowOf(leaf, root, items);
      if (!rows.includes(row)) rows.push(row);
    }
    return {count: rows.length, row: rows.length === 1 ? rows[0] : null, leaf: rows.length === 1 ? leaves[0] : null, leaves};
  };
  // showsKey 判断 element 里有显示着这条 Key 的元素
  const showsKey = (element, leaves) => leaves.some(leaf => within(leaf, element));
  // rowCells 为这一行的各格（表格的格；不是表格时为有文字的最内层元素），表头数与格数一致时带上表头
  const rowCells = (row, leaves) => {
    let cells = [...row.querySelectorAll(cellSelector)].filter(visible).filter(cell => {
      const outer = cell.parentElement && cell.parentElement.closest(cellSelector);
      return !(outer && within(outer, row));
    });
    let headers = [];
    if (cells.length) {
      const table = row.closest('table, [role=table], [role=grid], mat-table, .mat-mdc-table');
      if (table) headers = [...table.querySelectorAll('th, [role=columnheader], mat-header-cell, .mat-mdc-header-cell')]
        .filter(visible).filter(header => !within(header, row));
      if (headers.length !== cells.length) headers = [];
    } else {
      cells = [...row.querySelectorAll('*')].filter(visible).filter(element => textOf(element) && ![...element.children].some(child => textOf(child)));
      if (!cells.length) cells = [row];
    }
    return cells.slice(0, 12).map((cell, index) => ({t: short(plainText(cell), 120), h: headers[index] ? short(plainText(headers[index]), 40) : '',
      k: showsKey(cell, leaves)}));
  };
`

// studioKeyRowExpression 只读：返回 Key 列表里显示着末四位为 last4 的那一行（见 studioKeyRowView）
func studioKeyRowExpression(last4 string) string {
	return `((last4) => {` + pageDOMHelpers + studioKeyDOMHelpers + studioKeyRowHelpers + `
  const found = studioKeyRow();
  return JSON.stringify({count: found.count, cells: found.row ? rowCells(found.row, found.leaves) : []});
})(` + jsString(last4) + `)`
}

// studioKeyDetailsTargetExpression 返回这一行自己的 Key 名称（打开详情用），没有时为 null：行里可见、可用、不是站外链接、有文字、
// 不是删除复制编辑之类操作的按钮或链接（嵌在这类控件里的也不算），先取文字里带着这条打码 Key 的，再取与 Key 同一格里的
func studioKeyDetailsTargetExpression(last4 string) string {
	return `((last4) => {` + pageDOMHelpers + studioKeyDOMHelpers + studioKeyRowHelpers + `
  const found = studioKeyRow();
  if (!found.row) return null;
  const rowAction = pattern(` + jsString(studioKeyPatternRowAction) + `);
  const rowIcon = pattern(` + jsString(studioKeyPatternRowIcon) + `);
  const described = element => [nameOf(element), element.getAttribute('mattooltip') || '', element.getAttribute('data-tooltip') || ''].join(' ');
  const controls = [...found.row.querySelectorAll(controlSelector)].filter(visible).filter(buttonEnabled)
    .filter(element => !offSite(element) && plainText(element) !== '')
    .filter(element => !rowAction.test(described(element)) && !iconText(element).split(' ').some(name => rowIcon.test(name)))
    .filter(element => {
      const outer = element.parentElement && element.parentElement.closest(controlSelector);
      return !(outer && within(outer, found.row));
    });
  let keyCell = found.leaf.closest(cellSelector);
  if (!keyCell && found.leaf.parentElement !== found.row) keyCell = found.leaf.parentElement;
  return controls.find(element => showsKey(element, found.leaves)) ||
    controls.find(element => keyCell && within(element, keyCell)) || null;
})(` + jsString(last4) + `)`
}

// studioKeyDetailsExpression 只读：按标签读最上层对话框里的项目名、项目编号、项目 ID（没有对话框、点了之后页面路径不再是 before 时
// 读主区域，表头里的与只写着“项目”的标签不算）。标签元素自己的文字整段是标签；值取 dt 后的 dd、表头格或标签格后的下一格、label 指向的
// 控件、mat-form-field 里的控件、aria-labelledby 指向标签的元素、紧挨着的下一个元素，或父元素去掉标签后的文字；“标签：值”写在同一段
// 文字里的也认
func studioKeyDetailsExpression(before string) string {
	return `((before) => {` + pageDOMHelpers + studioKeyDOMHelpers + `
  const dialog = currentDialog();
  const moved = !dialog && location.pathname !== before;
  const scope = dialog || (moved ? (document.querySelector('main, [role=main]') || document.body) : null);
  const view = {dialog: Boolean(dialog), page: Boolean(scope) && moved, title: '', fields: []};
  if (!scope) return JSON.stringify(view);
  if (dialog) view.title = short([...dialog.querySelectorAll('h1, h2, h3, [mat-dialog-title], .mat-mdc-dialog-title, [role=heading]')]
    .filter(visible).map(textOf).find(Boolean) || '', 80);
  const sources = [['number', ` + jsString(studioKeyFieldNumber) + `], ['id', ` + jsString(studioKeyFieldID) + `],
    ['name', ` + jsString(studioKeyFieldName) + `], ['project', ` + jsString(studioKeyFieldProject) + `]];
  const exact = sources.map(([kind, source]) => [kind, new RegExp('^\\s*(?:' + source + ')\\s*[:：]?\\s*$', 'i')]);
  const inline = sources.map(([kind, source]) => [kind, new RegExp('^\\s*(?:' + source + ')\\s*[:：]\\s*(\\S.*)$', 'i')]);
  const kindOf = text => (exact.find(([, regex]) => regex.test(text)) || [''])[0];
  const siblings = element => element.parentElement ? [...element.parentElement.children] : [];
  const nextOf = element => {
    const list = siblings(element);
    return list[list.indexOf(element) + 1] || null;
  };
  const valueText = element => element.matches('input, textarea, select') ? clean(inputValue(element)) : plainText(element);
  const valueOf = label => {
    if (label.matches('dt, th, td, [role=rowheader], [role=term]')) {
      const next = nextOf(label);
      return next ? valueText(next) : '';
    }
    if (label.localName === 'label' && label.getAttribute('for')) {
      const target = document.getElementById(label.getAttribute('for'));
      if (target) return valueText(target);
    }
    const field = label.closest('mat-form-field, .mat-mdc-form-field');
    if (field && label.matches('mat-label, label')) {
      const control = field.querySelector('input, textarea, select');
      if (control) return valueText(control);
    }
    if (label.id) {
      const target = [...scope.querySelectorAll('[aria-labelledby]')]
        .find(element => clean(element.getAttribute('aria-labelledby')).split(' ').includes(label.id));
      if (target) return valueText(target);
    }
    const next = nextOf(label);
    if (next) {
      const text = valueText(next);
      if (text && !kindOf(text)) return text;
    }
    const parent = label.parentElement;
    if (parent && parent !== scope && siblings(label).length <= 3) {
      const rest = clean(plainText(parent).replace(plainText(label), '')).replace(/^[:：]\s*/, '');
      if (rest && rest.length <= 120 && !kindOf(rest)) return rest;
    }
    return '';
  };
  const seen = new Set();
  for (const element of scope.querySelectorAll('*')) {
    if (view.fields.length >= 12) break;
    if (!visible(element) || (!dialog && element.closest('th, thead, [role=columnheader]'))) continue;
    const text = plainText(element);
    if (!text || text.length > 160) continue;
    let kind = kindOf(text);
    let value = '';
    if (kind) {
      if ([...element.children].some(child => kindOf(plainText(child)) === kind)) continue;
      value = valueOf(element);
    } else {
      const match = inline.map(([name, regex]) => [name, regex.exec(text)]).find(([, result]) => result);
      if (!match || [...element.children].some(child => inline.some(([, regex]) => regex.test(plainText(child))))) continue;
      kind = match[0];
      value = match[1][match[1].length - 1];
    }
    if (!value || seen.has(kind) || (!dialog && kind === 'project')) continue;
    seen.add(kind);
    view.fields.push({k: kind, v: short(value, 120)});
  }
  return JSON.stringify(view);
})(` + jsString(before) + `)`
}

// studioKeyDetailsCloseExpression 返回最上层对话框自己的关闭类按钮（整段文字是 Close、Done、关闭、完成之类；或只有 close 图标；
// 或 aria-label 像关闭），链接与删除复制之类的操作按钮除外；没有时为 null
var studioKeyDetailsCloseExpression = `(() => {` + pageDOMHelpers + studioKeyDOMHelpers + `
  const dialog = currentDialog();
  if (!dialog) return null;
  const closeText = pattern(` + jsString(studioKeyPatternDetailsClose) + `);
  const closeLabel = pattern(` + jsString(studioKeyPatternDetailsCloseLabel) + `);
  const rowAction = pattern(` + jsString(studioKeyPatternRowAction) + `);
  const controls = dialogControls(dialog).filter(buttonEnabled)
    .filter(element => !(element.localName === 'a' && element.getAttribute('href')) && !rowAction.test(nameOf(element)));
  return controls.find(element => closeText.test(plainText(element))) ||
    controls.find(element => !plainText(element) && /^(close|clear)$/i.test(iconText(element))) ||
    controls.find(element => closeLabel.test(clean(element.getAttribute('aria-label') || ''))) ||
    controls.find(element => element.hasAttribute('mat-dialog-close') && !plainText(element)) || null;
})()`
